// Package server: copilot_bridge.go — GitHub Copilot 直连路由。
//
// 模型名带 "copilot:" 前缀（如 copilot:gpt-4o、copilot:claude-sonnet-4）时走本路径：
// 从外部账号表（extstore）取启用的 Copilot 账号 → Copilot token 到期前自动续期
// （401/403 再强制续期重试一次）→ 转发到 api.githubcopilot.com。
//
// 与其它通道的关键差异：**上游就是 OpenAI 协议**，网关不做任何翻译——
// 请求体原样转发（模型名剥掉前缀由调用方完成），响应体原样透传。
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

	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// CopilotChatStream 直接向客户端透传 GitHub Copilot 的 OpenAI 兼容响应。
// 返回 false = 未找到可用账号（调用方报错，不回落腾讯池）。
func (h *Handler) copilotChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	// 每次进入先清：那是**上一次请求**的事实。上一位用户撞到的失败原因不该
	// 挂在这一次「账号不存在」的提示里——本次一条上游都没碰过时必须无原因。
	h.lastCopilotErr = ""
	if h.cfg.ExtAccounts == nil {
		return false
	}
	mgr := h.extManager
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCopilot && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred copilot.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// Copilot token 约 25 分钟过期：到期前自动用 GitHub token 续期；
		// 续期失败（GitHub token 被撤销 / 无订阅）换下一个账号。
		if cred.NeedsRefresh() {
			fresh, err := copilot.Refresh(r.Context(), &cred)
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("copilot-bridge: %s 续期失败: %v", a.ID, err)
				continue
			}
			if raw, merr := json.Marshal(fresh); merr == nil && mgr != nil {
				mgr.ReplaceCred(a.Provider, a.ID, raw)
			}
			cred = *fresh
		}

		resp, err := copilot.Chat(r.Context(), &cred, body)
		if err != nil {
			lastErr = err.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("copilot-bridge: %s chat 失败: %v", a.ID, err)
			continue
		}

		// 401/403：token 可能被上游提前失效，强制续期重试一次。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			fresh, rerr := copilot.Refresh(r.Context(), &cred)
			if rerr != nil {
				lastErr = rerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("copilot-bridge: %s 强制续期失败: %v", a.ID, rerr)
				continue
			}
			if raw, merr := json.Marshal(fresh); merr == nil && mgr != nil {
				mgr.ReplaceCred(a.Provider, a.ID, raw)
			}
			cred = *fresh
			resp, err = copilot.Chat(r.Context(), &cred, body)
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
		}

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			// 模型级的话（目录外的模型名）与账号无关：不换号、不给健康账号
			// 记失败，直接把「哪个模型能用」讲清楚。
			if copilot.ModelUnsupported(resp.StatusCode, raw) {
				reason := copilotModelMissingReason(bareModel, len(h.copilotCatalog()))
				h.lastCopilotErr = reason
				log.Printf("copilot-bridge: model=%s 上游不支持（HTTP %d），不换号: %s", bareModel, resp.StatusCode, shorten(string(raw), 200))
				writeOpenAIError(w, http.StatusNotFound, "model_not_found", reason)
				return true
			}
			lastErr = "上游 HTTP " + strconv.Itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("copilot-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue // 换下一个 Copilot 账号
		}

		log.Printf("copilot-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.passThroughCopilot(w, resp, h.clientWantsStream(body))
		return true
	}
	h.lastCopilotErr = lastErr
	return false // 全部 Copilot 账号不可用
}

// passThroughCopilot 原样透传上游响应（上游即 OpenAI 格式，无需翻译）。
//
// 上游按请求体的 stream 字段决定返回 SSE 还是 JSON；客户端要流式而上游给了
// JSON 时，包成单帧 SSE 再补 [DONE]，与其它通道的语义保持一致。
func (h *Handler) passThroughCopilot(w http.ResponseWriter, resp *http.Response, wantStream bool) {
	defer resp.Body.Close()

	// 流式：逐帧透传（帧格式本就是标准 chat.completion.chunk）。
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		h.proxyCopilotSSE(w, resp.Body)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	// 客户端要流式、上游给了 JSON：包成单帧 SSE。
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

// proxyCopilotSSE 逐帧透传 Copilot SSE（复用 Qoder 的纯透传实现）。
func (h *Handler) proxyCopilotSSE(w http.ResponseWriter, rc io.ReadCloser) {
	h.proxyQoderSSE(w, rc)
}

// copilotModelMissingReason 目录外模型名的用户可读原因。
//
// 关键是把「模型名的事」和「账号的事」分开：这句如果说成「没有可用账号」，
// 用户就会去重登一个本来就登录成功的账号——实测就是这么绕了一圈的。
func copilotModelMissingReason(model string, catalogCount int) string {
	if catalogCount > 0 {
		return "模型 " + model + " 不在该 GitHub Copilot 账号的可用目录内" +
			"（上游 400 model_not_supported，与账号是否登录无关，换号也没用）；" +
			"实时目录共 " + strconv.Itoa(catalogCount) + " 个模型，见 GET /v1/models 的 copilot: 前缀"
	}
	return "模型 " + model + " 不在该 GitHub Copilot 账号的可用目录内" +
		"（上游 400 model_not_supported，与账号是否登录无关，换号也没用）；" +
		"可用名单见 GET /v1/models 的 copilot: 前缀"
}

/* ── 模型目录（实时拉取 + 10 分钟缓存） ───────────────────────────── */

var (
	copilotCatMu   sync.Mutex
	copilotCatAt   time.Time
	copilotCatMods []copilot.Model
)

// copilotCatalog 返回 Copilot 账号可用的模型（上游按订阅等级过滤）。
// 首次调用实时拉取并缓存 10 分钟；拉取失败时回退到上一次成功结果。
func (h *Handler) copilotCatalog() []copilot.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	copilotCatMu.Lock()
	if time.Since(copilotCatAt) < 10*time.Minute && copilotCatMods != nil {
		mods := copilotCatMods
		copilotCatMu.Unlock()
		return mods
	}
	copilotCatMu.Unlock()

	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCopilot && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var mods []copilot.Model
	for _, a := range accounts {
		var cred copilot.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			continue
		}
		if cred.NeedsRefresh() {
			fresh, err := copilot.Refresh(ctx, &cred)
			if err != nil {
				continue
			}
			if raw, merr := json.Marshal(fresh); merr == nil && h.extManager != nil {
				h.extManager.ReplaceCred(a.Provider, a.ID, raw)
			}
			cred = *fresh
		}
		got, err := copilot.ListModels(ctx, &cred)
		if err != nil {
			log.Printf("copilot-bridge: %s 拉取模型列表失败: %v", a.ID, err)
			continue
		}
		mods = got
		break
	}
	if len(mods) == 0 {
		copilotCatMu.Lock()
		prev := copilotCatMods
		copilotCatMu.Unlock()
		return prev
	}
	copilotCatMu.Lock()
	copilotCatMods, copilotCatAt = mods, time.Now()
	copilotCatMu.Unlock()
	return mods
}

// copilotModelPrefix Copilot 路由前缀。
const copilotModelPrefix = "copilot:"

// isCopilotModel 判断裸模型名是否请求 Copilot 通道。
func isCopilotModel(bare string) bool { return strings.HasPrefix(bare, copilotModelPrefix) }

// shorten 截断日志/错误文案里的上游响应（保留头部，UTF-8 安全按字节回退）。
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // 不切断多字节字符
		n--
	}
	return s[:n] + "…"
}
