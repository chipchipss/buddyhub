// Package server: loomy_bridge.go — Loomy 模型直连路由。
//
// 模型名带 "loomy:" 前缀（如 loomy:spark-x、loomy:deepseek-v4-flash-0731）时走
// 本路径：Loomy 模型网关（loomyad.xunfei.cn/api/v1）本就 OpenAI 兼容，登录
// session 即 Bearer（useSessionAuth）。凭据来源两路：
//  1. 外部账号池 extstore 的 loomy-cli 账号（密码/短信登录落库，多号轮换）
//  2. data/loomy-session.json（FindLoomySession 第 0 优先级，客户端导入或登录双写）
//
// 与腾讯池完全独立：Loomy 凭据失败只在本路径内轮换下一个 Loomy 账号，不回落。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/chipchipss/buddyhub/internal/upstream"
)

// loomyModelPrefix Loomy 路由前缀。
const loomyModelPrefix = "loomy:"

// isLoomyModel 判断裸模型名是否请求 Loomy 通道。
func isLoomyModel(bare string) bool { return strings.HasPrefix(bare, loomyModelPrefix) }

// loomyCred 外部池 loomy-cli 凭据形状（saveLoomyLogin 落库结构）。
type loomyCred struct {
	Session string `json:"session"`
	UserID  string `json:"userid"`
	Phone   string `json:"phone"`
}

// loomyAccount 一个可用凭据（session + 展示标识）。
type loomyAccount struct {
	Session string
	Label   string
}

// loomyAccounts 收集全部可用 Loomy 凭据：外部池优先，session 文件兜底去重。
func loomyAccounts(h *Handler) []loomyAccount {
	seen := map[string]bool{}
	var out []loomyAccount
	if h.cfg.ExtAccounts != nil {
		for _, a := range h.cfg.ExtAccounts() {
			if a.Provider != "loomy-cli" || a.Disabled {
				continue
			}
			var c loomyCred
			if json.Unmarshal(a.Cred, &c) != nil || c.Session == "" {
				continue
			}
			if !seen[c.Session] {
				seen[c.Session] = true
				out = append(out, loomyAccount{Session: c.Session, Label: a.ID})
			}
		}
	}
	if s, err := upstream.FindLoomySession(); err == nil && s != nil && s.Session != "" && !seen[s.Session] {
		out = append(out, loomyAccount{Session: s.Session, Label: "session-file"})
	}
	return out
}

// LoomyChatStream Loomy 模型网关直连：多号轮换 + 401/403 换号。
// 返回 false = 无可用账号（调用方报错，不回落腾讯池）。
func (h *Handler) LoomyChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	accounts := loomyAccounts(h)
	if len(accounts) == 0 {
		return false
	}
	m := strings.TrimPrefix(bareModel, loomyModelPrefix)
	outBody := rewriteModel(body, m)

	lastErr := ""
	for _, acc := range accounts {
		rc, status, ctype, err := upstream.LoomyChatStream(acc.Session, string(outBody))
		if err != nil {
			// 401/403 = session 失效。密码不落盘（loomy-cli 凭据只存 session），
			// 无法静默重登——明确提示用户重新登录，换下一个账号继续试。
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				lastErr = "session 失效，请在面板重新登录该 Loomy 账号"
				log.Printf("loomy-bridge: %s session 失效 (HTTP %d)", acc.Label, status)
				continue
			}
			lastErr = err.Error()
			log.Printf("loomy-bridge: %s chat 失败 (HTTP %d): %v", acc.Label, status, err)
			if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
				// 请求本身的问题，换号无意义
				writeOpenAIError(w, status, "upstream_error", lastErr)
				return true
			}
			continue
		}
		log.Printf("loomy-bridge: acct=%s model=%s 建流成功 ctype=%s", acc.Label, m, ctype)
		isSSE := strings.Contains(ctype, "text/event-stream")
		if h.clientWantsStream(body) {
			// 客户端要流式：上游 SSE 直接透传；上游 JSON（非流式响应）包一层
			// 单帧 SSE 再收尾，保证客户端始终拿到合法 event-stream。
			if isSSE {
				h.proxyLoomySSE(w, rc)
			} else {
				h.proxyJSONAsSSE(w, rc)
			}
		} else {
			// 客户端要整体响应：SSE 聚合为 chat.completion；JSON 原样透传。
			if isSSE {
				h.proxyLoomyAggregate(w, rc)
			} else {
				defer rc.Close()
				raw, rerr := io.ReadAll(rc)
				if rerr != nil {
					writeOpenAIError(w, http.StatusBadGateway, "upstream_read", rerr.Error())
					return true
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(raw)
			}
		}
		return true
	}
	h.lastLoomyErr = lastErr
	return false
}

// proxyLoomySSE 逐帧透传（上游 SSE 帧即标准 OpenAI chat.completion.chunk，
// 尾帧带 usage.points_consumed 原样透出）。
func (h *Handler) proxyLoomySSE(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// proxyJSONAsSSE 把上游 JSON 响应包装为单帧 SSE + [DONE]（客户端要流式而上游
// 返回了整体 JSON 时的合规化封装）。
func (h *Handler) proxyJSONAsSSE(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// proxyLoomyAggregate 聚合 SSE 为标准非流式响应。
func (h *Handler) proxyLoomyAggregate(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// loomyCatalog 透出上游模型目录（有可用凭据时调用一次并缓存 10 分钟）。
func (h *Handler) loomyCatalog() []map[string]any {
	accounts := loomyAccounts(h)
	if len(accounts) == 0 {
		return nil
	}
	mods, err := upstream.LoomyModels(accounts[0].Session)
	if err != nil {
		log.Printf("loomy-bridge: 模型目录拉取失败: %v", err)
		return nil
	}
	return mods
}
