// Part of the Cursor model catalog (ported from magpie index.mjs L522-827, MIT).
//
// List fetching and caching: AvailableModels + GetUsableModels concurrently,
// cached per token digest for ModelsKeep (fork usable/lists, L759-783).
package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── 清单获取 ──────────────────────────────────────────────────────────

// Catalog 一个 token 的模型目录（fork usable + lists Map）。
type Catalog struct {
	token   string
	models  []Model
	fetched time.Time
	// pickerRaw 原始 JSON（named 的 legacySlugs/idAliases 要用）。
	pickerRaw []pickerModelRaw
}

// CatalogCache 目录缓存（fork usable 的 lists Map：按 token 摘要存，
// ModelsKeep 分钟内不重复拉）。零值可用。
type CatalogCache struct {
	mu    sync.Mutex
	byTok map[string]*Catalog
}

// Get 取（或拉）一个 token 的目录（fork usable L765-783）。
// 未命中才拉上游；拉失败时**保留上一次结果**（fork：拿不到就留着旧的，
// 不清空——清空会让调用方误判"这个账号没有任何模型可用"）。
func (cc *CatalogCache) Get(ctx context.Context, client *Client, token string) (*Catalog, error) {
	cc.mu.Lock()
	if cc.byTok == nil {
		cc.byTok = map[string]*Catalog{}
	}
	key := catalogKey(token)
	if had := cc.byTok[key]; had != nil && time.Since(had.fetched) < ModelsKeep {
		cc.mu.Unlock()
		return had, nil
	}
	cc.mu.Unlock()

	cat, err := FetchCatalog(ctx, client, token)
	if err != nil {
		cc.mu.Lock()
		had := cc.byTok[key]
		cc.mu.Unlock()
		if had != nil {
			return had, nil // 保留旧目录（fork 同款）
		}
		return nil, err
	}
	cc.mu.Lock()
	cc.byTok[key] = cat
	cc.mu.Unlock()
	return cat, nil
}

// Invalidate 作废一个 token 的缓存（重新登录后调用，让下次重拉）。
func (cc *CatalogCache) Invalidate(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.byTok, catalogKey(token))
}

// catalogKey token 的 sha256 摘要作缓存键（明文 token 不进 map 键，
// 与 fork 同样处理：它也是 hash 后才存）。
func catalogKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// FetchCatalog 拉取并构造目录（fork usable L765-783）。
// 上游给不出目录时返回上一次的（有缓存）；什么都没有才报错。
func FetchCatalog(ctx context.Context, client *Client, token string) (*Catalog, error) {
	// picker 与 usable 并发拉（fork Promise.all）。
	type pickerRes struct {
		models []pickerModelRaw
		err    error
	}
	type usableRes struct {
		ids map[string]bool
		err error
	}
	pc := make(chan pickerRes, 1)
	uc := make(chan usableRes, 1)

	go func() {
		raw, err := unaryJSON(ctx, client, pathAvailableModels, token,
			map[string]any{"useModelParameters": true, "doNotUseMarkdown": true}, pickerTimeout)
		var out pickerRes
		if err == nil {
			var p modelsPayload
			if json.Unmarshal(raw, &p) == nil {
				out.models = p.Models
			}
		} else {
			out.err = err
		}
		pc <- out
	}()
	go func() {
		raw, err := unaryJSON(ctx, client, pathUsableModels, token, nil, usableTimeout)
		var out usableRes
		if err == nil {
			var p usablePayload
			if json.Unmarshal(raw, &p) == nil {
				ids := map[string]bool{}
				for _, m := range p.Models {
					if m.ModelID != "" {
						ids[m.ModelID] = true
					}
					if m.DisplayModelID != "" {
						ids[m.DisplayModelID] = true
					}
				}
				if len(ids) > 0 {
					out.ids = ids
				}
			}
		} else {
			out.err = err
		}
		uc <- out
	}()

	p := <-pc
	u := <-uc
	// usable 拿不到不算失败（fork：usable 拿不到就保留整个 picker）。
	if p.err != nil {
		return nil, fmt.Errorf("cursor: 模型目录拉取失败: %w", p.err)
	}
	models := modelsOf(p.models, u.ids)
	if len(models) == 0 {
		return nil, errors.New("cursor: 上游列不出任何模型")
	}
	cat := &Catalog{token: token, models: models, pickerRaw: p.models, fetched: time.Now()}
	cat.attachAliases()
	return cat, nil
}

// attachAliases 把每个模型的别名挂到条目上（named 第三分支）。
func (c *Catalog) attachAliases() {
	byName := map[string]int{}
	for i, m := range c.pickerRaw {
		byName[m.Name] = i
	}
	for i := range c.models {
		idx, ok := byName[c.models[i].Run]
		if !ok {
			continue
		}
		c.models[i].legacy = c.pickerRaw[idx].LegacySlugs
		c.models[i].aliases = c.pickerRaw[idx].IDAliases
	}
}

// Models 目录条目（调用方只读）。
func (c *Catalog) Models() []Model {
	if c == nil {
		return nil
	}
	return c.models
}

// FetchedAt 拉取时间（缓存判断用）。
func (c *Catalog) FetchedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.fetched
}

// unaryJSON 一次 Connect unary 调用，返回响应体（fork unary L187-202 同款）。
func unaryJSON(ctx context.Context, c *Client, path, token string, body any, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/"+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-cursor-client-type", "cli")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return raw, nil
}
