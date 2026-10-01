// Package server: raccoon_bridge.go — 小浣熊（商汤）直连路由。
//
// 模型名带 "raccoon:" 前缀时走本路径。与 Trae/QClaw 不同，**上游本身就是
// OpenAI 兼容**（body 原样透传、SSE 是标准 chat.completion.chunk），所以本桥
// 既不需要白名单重建，也不需要事件流翻译——唯一要处理的是**响应帧里的 model
// 被上游换成了它的内部名**，得回写成客户端请求的那个。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// raccoonChatStream 转发到小浣熊。返回 false = 无可用账号。
func (h *Handler) raccoonChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PRaccoon && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	// body **原样透传**：上游就是 OpenAI 形状，既不改 system 消息（那是腾讯
	// 上游的硬要求），也不做白名单重建。
	outBody := body

	cli := raccoon.New()
	lastErr := ""
	for _, a := range accounts {
		var cred raccoon.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			continue
		}
		if cred.IsExpired() {
			fresh, rerr := h.refreshRaccoonCred(r.Context(), a.ID, &cred)
			if rerr != nil {
				lastErr = rerr.Error()
				log.Printf("raccoon-bridge: %s 续期失败: %v", a.ID, rerr)
				continue
			}
			cred = *fresh
		}

		resp, cerr := cli.Chat(r.Context(), &cred, outBody)
		if cerr != nil {
			// 401/403：登录态过期 → 续期后同账号重试一次
			var ue *raccoon.UpstreamError
			if errors.As(cerr, &ue) && (ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden) {
				if fresh, rerr := h.refreshRaccoonCred(r.Context(), a.ID, &cred); rerr == nil {
					cred = *fresh
					resp, cerr = cli.Chat(r.Context(), &cred, outBody)
				}
			}
		}
		if cerr != nil {
			lastErr = cerr.Error()
			log.Printf("raccoon-bridge: %s chat 失败: %v", a.ID, cerr)
			continue
		}

		log.Printf("raccoon-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.forwardRaccoon(w, resp, body, bareModel)
		return true
	}
	h.lastRaccoonErr = lastErr
	return false
}

// refreshRaccoonCred 单飞续期 + 回写账号表。
//
// 单飞是必须的：续期会**轮换 refresh_token**，并发打两次会让第二次拿旧 token
// 去换，直接失败。
func (h *Handler) refreshRaccoonCred(ctx context.Context, accountID string, cred *raccoon.Credential) (*raccoon.Credential, error) {
	fresh, err := h.raccoonFlights.Do(accountID, func() (*raccoon.Credential, error) {
		return raccoon.New().Refresh(ctx, cred)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PRaccoon, accountID, raw)
		}
	}
	return fresh, err
}

// forwardRaccoon 把上游响应转给客户端：流式逐帧（回写 model），非流式本地聚合。
func (h *Handler) forwardRaccoon(w http.ResponseWriter, resp *http.Response, reqBody []byte, model string) {
	defer resp.Body.Close()

	if !h.clientWantsStream(reqBody) {
		// 上游两种形态都要接：`stream:false` 时它**直接回纯 JSON**，
		// 而 Aggregate 只认 SSE——拿 JSON 去 Aggregate 会得到
		// 「upstream stream contained no valid data events」。先探首字节。
		br := bufio.NewReader(resp.Body)
		if first, err := br.Peek(1); err == nil && len(first) > 0 && first[0] == '{' {
			raw, _ := io.ReadAll(br)
			out := rewriteJSONObject(string(raw), "model", model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, out)
			return
		}
		doc, err := upstream.Aggregate(br)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		// 聚合出来的 doc 同样带上游的内部模型名，跟流式一样要回写
		if doc != nil {
			if _, ok := doc["model"].(string); ok {
				doc["model"] = model
			}
		}
		writeJSON(w, http.StatusOK, doc)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// SSE 是行导向的：逐行转发，只在 `data:` 行上回写 model。
	//
	// 用 `sc.Text()` 而不是 `sc.Bytes()`——后者返回的是**复用的内部缓冲**，
	// 下一次 Scan 就会覆盖它，拿它直接 Write 会读到被改写过的数据。
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[len("data:"):])
			// RewriteModel 自己先探有没有 model 字段，没有就原样返回
			// （`data: [DONE]`、注释行因此零开销）
			if out := string(raccoon.RewriteModel([]byte(payload), model)); out != payload {
				line = "data: " + out
			}
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return // 客户端断开
		}
		flusher.Flush()
	}
}

/* ── 模型目录（实时拉取 + 10 分钟缓存 + 静态兜底） ─────────────── */

var (
	raccoonCatMu   sync.Mutex
	raccoonCatAt   time.Time
	raccoonCatMods []raccoon.Model
)

// raccoonCatalog 目录：远程 → 旧缓存 → 静态兜底，三级兜底。
//
// 分三级是因为**离线时 /v1/models 必须仍有内容**：那也是客户端选模型的唯一
// 依据，空列表会让整条通道看起来「不存在」。
func (h *Handler) raccoonCatalog() []raccoon.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	raccoonCatMu.Lock()
	if time.Since(raccoonCatAt) < raccoon.CatalogTTL && raccoonCatMods != nil {
		mods := raccoonCatMods
		raccoonCatMu.Unlock()
		return mods
	}
	raccoonCatMu.Unlock()

	var cred *raccoon.Credential
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PRaccoon || a.Disabled {
			continue
		}
		var c raccoon.Credential
		if json.Unmarshal(a.Cred, &c) == nil && c.AccessToken != "" {
			cred = &c
			break
		}
	}
	if cred == nil {
		return raccoon.FallbackModels()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mods, err := raccoon.New().ListModels(ctx, cred)
	if err != nil {
		// 失败保留旧缓存；**从未成功过**就用静态兜底——不能让目录变空
		log.Printf("raccoon-bridge: 拉取模型目录失败，改用兜底/旧缓存: %v", err)
		raccoonCatMu.Lock()
		prev := raccoonCatMods
		raccoonCatMu.Unlock()
		if prev != nil {
			return prev
		}
		return raccoon.FallbackModels()
	}
	if len(mods) == 0 {
		return raccoon.FallbackModels()
	}
	raccoonCatMu.Lock()
	raccoonCatMods, raccoonCatAt = mods, time.Now()
	raccoonCatMu.Unlock()
	return mods
}

// raccoonModelPrefix 小浣熊路由前缀。
const raccoonModelPrefix = "raccoon:"

// isRaccoonModel 判断裸模型名是否请求小浣熊通道。
func isRaccoonModel(bare string) bool { return strings.HasPrefix(bare, raccoonModelPrefix) }

var _ = fmt.Sprintf // keep fmt import if unused paths change
