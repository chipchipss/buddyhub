// Package server: cline_bridge.go — Cline（cline.bot）直连路由。
//
// 模型名带 "cline:" 前缀时走本路径。前缀后面直接跟**带计费池前缀的模型名**，
// 例如 `cline:cline-free/deepseek-v4.1-flash`、`cline:cline-pass/glm-5.3`：
//
//	cline:  → 网关路由（选中本通道）
//	cline-free/ / cline-pass/ / cline-cloud/ → **上游计费通道选择器**
//
// 所以出站时只剥掉 `cline:`，池前缀原样带给上游——它正是用来选池的。
// 与其它通道的关键差异：**上游就是 OpenAI 协议**，网关不做翻译，原样透传。
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// clineChatStream 转发到 Cline。返回 false = 无可用账号。
func (h *Handler) clineChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	// 先清：那是**上一次请求**的事实，不该挂进本次的失败提示里。
	h.lastClineErr = ""
	if h.cfg.ExtAccounts == nil {
		return false
	}
	// free 池的 deepseek 系模型对过小的 max_tokens 会内部 500（"empty response
	// content"）——客户端传 max_tokens:8 这类探针值时必挂。出站前把 free 池请求
	// 的 max_tokens 抬到 128 安全下限（客户端未传时保留，仅补下限；传了且更小则提上去）。
	if clineFreePool(bareModel) {
		if norm := floorMaxTokens(body, 128); norm != nil {
			body = norm
		}
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCline && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred cline.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.NeedsRefresh() {
			// refresh_token 是一次性轮换语义：并发续期会互相作废，
			// 故同一账号同一时刻只允许一次续期在飞。
			fresh, err := h.refreshClineCred(r.Context(), a.ID, &cred)
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("cline-bridge: %s 续期失败: %v", a.ID, err)
				continue
			}
			cred = *fresh
		}

		resp, err := cline.Chat(r.Context(), &cred, body)
		if err != nil {
			lastErr = err.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("cline-bridge: %s chat 失败: %v", a.ID, err)
			continue
		}

		// 401 = 令牌被提前失效，强制续期重试一次
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			fresh, rerr := h.refreshClineCred(r.Context(), a.ID, &cred)
			if rerr != nil {
				lastErr = rerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
			cred = *fresh
			resp, err = cline.Chat(r.Context(), &cred, body)
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
		}

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			// 403 是「这个账号对这个模型没有权限」的确定性拒绝（没订阅 / 非产品面），
			// 换号重试同样会 403——但这里仍换下一个账号试一次：
			// 池里可能同时有免费池与订阅池账号，换号是有意义的。
			lastErr = "上游 HTTP " + strconv.Itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			// free 池的部分模型（deepseek-v4.1-flash 实测）对很小的 max_tokens
			// 会内部失败并回 500 "empty response content"——上游模型层的怪癖，
			// 不是账号问题。给用户一条当场能做的提示（调大 max_tokens 或去掉它）。
			if resp.StatusCode == http.StatusInternalServerError &&
				strings.Contains(string(raw), "empty response content") {
				lastErr += "（free 池该模型对过小的 max_tokens 会内部失败：调大 max_tokens（如 300+）或不传该字段）"
			}
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("cline-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue
		}

		log.Printf("cline-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.passThroughCline(w, resp, h.clientWantsStream(body))
		return true
	}
	h.lastClineErr = lastErr
	return false
}

// refreshClineCred 单飞续期 + 回写账号表。
//
// 为什么必须单飞：Cline 的 refresh_token 是**一次性轮换**语义，并发的多个请求
// 同时发现临期时若各自去打一次续期，后到的那次会拿着已作废的 token → 401 →
// 用户被踢下线。同账号的续期合并成一次，其余请求复用结果。
func (h *Handler) refreshClineCred(ctx context.Context, accountID string, cred *cline.Credential) (*cline.Credential, error) {
	fresh, err := h.clineFlights.Do(accountID, func() (*cline.Credential, error) {
		return cline.Refresh(ctx, cred)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PCline, accountID, raw)
		}
	}
	return fresh, err
}

// passThroughCline 原样透传上游响应（上游即 OpenAI 格式，无需翻译）。
func (h *Handler) passThroughCline(w http.ResponseWriter, resp *http.Response, wantStream bool) {
	defer resp.Body.Close()

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		h.proxyQoderSSE(w, resp.Body)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	// 非流式时上游包了一层信封 {"data":{...},"success":true}，取出内层。
	if inner := unwrapClineEnvelope(raw); inner != nil {
		raw = inner
	}
	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
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
	_, _ = w.Write(append(append([]byte("data: "), raw...), []byte("\n\ndata: [DONE]\n\n")...))
	flusher.Flush()
}

// unwrapClineEnvelope 非流式响应带 {"data":…,"success":true} 信封时取出内层；
// 不是信封则返回 nil（保持原样）。
func unwrapClineEnvelope(raw []byte) []byte {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Data) == 0 {
		return nil
	}
	// 只在看起来是 chat.completion 时才解包，避免误伤本就没有信封的响应
	if !strings.Contains(string(env.Data), `"choices"`) {
		return nil
	}
	return env.Data
}

/* ── 模型目录（实时拉取 + 10 分钟缓存） ───────────────────────── */

var (
	clineCatMu   sync.Mutex
	clineCatAt   time.Time
	clineCatMods []cline.Model
)

// clineCatalog 返回 Cline 可用模型（**免鉴权**公开目录，10 分钟缓存）。
//
// 前缀按池展开成 `cline:<池前缀><模型名>`，让同一模型的不同计费池各占一个 id
// ——池是计费通道，混在一起用户无从选择。
func (h *Handler) clineCatalog() []cline.Model {
	if h.cfg.ExtAccounts == nil || !h.hasClineAccount() {
		return nil
	}
	clineCatMu.Lock()
	if time.Since(clineCatAt) < 10*time.Minute && clineCatMods != nil {
		mods := clineCatMods
		clineCatMu.Unlock()
		return mods
	}
	clineCatMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cat, err := cline.ListModels(ctx)
	if err != nil {
		log.Printf("cline-bridge: 拉取模型目录失败: %v", err)
		clineCatMu.Lock()
		prev := clineCatMods
		clineCatMu.Unlock()
		return prev
	}
	mods := cat.All()
	clineCatMu.Lock()
	clineCatMods, clineCatAt = mods, time.Now()
	clineCatMu.Unlock()
	return mods
}

func (h *Handler) hasClineAccount() bool {
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCline && !a.Disabled {
			return true
		}
	}
	return false
}

// clineModelPrefix Cline 路由前缀。
const clineModelPrefix = "cline:"

// isClineModel 判断裸模型名是否请求 Cline 通道。
func isClineModel(bare string) bool { return strings.HasPrefix(bare, clineModelPrefix) }

// clineFreePool 该裸模型名是否请求 Cline **免费池**（入参是剥掉 `cline:` 后的名字，
// 形如 `cline-free/deepseek-v4.1-flash`——池前缀保留，见 dispatch.go 的 cline 块）。
func clineFreePool(bare string) bool { return strings.HasPrefix(bare, cline.FreePrefix) }

// floorMaxTokens 把请求体的 max_tokens / max_completion_tokens 抬到 min 下限：
// 客户端没传则补上；传了且小于 min 则提到 min；大于 min 原样。返回 nil = 未改动。
// 只对合法 JSON 对象生效，解析失败原样返回（绝不破坏请求）。
func floorMaxTokens(body []byte, min int) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil
	}
	changed := false
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := obj[k]
		if !ok {
			continue
		}
		f, ok := v.(float64)
		if !ok || f > float64(min) {
			continue
		}
		obj[k] = float64(min)
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}
