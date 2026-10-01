// Package server: qoder_bridge.go — Qoder 模型直连路由。
//
// 模型名带 "qoder:" 前缀（如 qoder:lite、qoder:qfmodel）时走本路径：
// 从外部账号表（extstore）取一个启用的 qoder 账号 → 自动续期（NeedsRefresh
// 或 401 触发）→ ChatStream（api2-v2 纯 Bearer 新版协议）→ SSE 透传/聚合。
// 与腾讯池完全独立：Qoder 凭据失败只在本路径内轮换下一个 Qoder 账号。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// QoderChatStream 直接向客户端透传 Qoder 的 OpenAI 兼容 SSE。
// 返回 false = 未找到可用账号（调用方报错，不回落腾讯池）。
func (h *Handler) qoderChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	mgr := h.extManager
	cli := qoder.New()
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PQoder && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred qoder.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// 到期前自动续期；续期失败继续尝试下一个账号。
		if cred.NeedsRefresh() {
			if fresh, err := cli.Refresh(r.Context(), &cred); err == nil {
				if raw, merr := json.Marshal(fresh); merr == nil {
					mgr.ReplaceCred(a.Provider, a.ID, raw)
					cred = *fresh
				}
			} else {
				log.Printf("qoder-bridge: %s 续期失败: %v", a.ID, err)
				// 此前这里**只 continue 不设 lastErr**：整轮失败时
				// h.lastQoderErr 是空串，503 文案里看不到真实原因。
				lastErr = "续期失败: " + err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
		}

		outBody, err := qoder.BuildChatBody(body, qoder.ChatMeta{})
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return true
		}
		rc, status, err := cli.ChatStream(r.Context(), &cred, outBody, qoder.ChatMeta{})
		if err != nil {
			// 401/403 触发强制刷新重试一次（对齐 Qoder CLI forceRefreshToken 语义）。
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				if fresh, rerr := cli.Refresh(r.Context(), &cred); rerr == nil {
					if raw, merr := json.Marshal(fresh); merr == nil {
						mgr.ReplaceCred(a.Provider, a.ID, raw)
						cred = *fresh
					}
					rc, status, err = cli.ChatStream(r.Context(), &cred, outBody, qoder.ChatMeta{})
				}
			}
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("qoder-bridge: %s chat 失败 (HTTP %d): %v", a.ID, status, err)
				continue // 换下一个 Qoder 账号
			}
		}

		// 成功建流：按客户端是否要流式决定透传或聚合。
		log.Printf("qoder-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		if h.clientWantsStream(body) {
			h.proxyQoderSSE(w, rc)
		} else {
			h.proxyQoderAggregate(w, rc)
		}
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastQoderErr = lastErr
	return false // 全部 Qoder 账号不可用
}

// clientWantsStream 从请求体读 stream 字段。
func (h *Handler) clientWantsStream(body []byte) bool {
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Stream
}

// proxyQoderSSE 逐帧透传 Qoder SSE（帧格式本就是标准 OpenAI chat.completion.chunk）。
func (h *Handler) proxyQoderSSE(w http.ResponseWriter, rc io.ReadCloser) {
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
				return // 客户端断开
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// proxyQoderAggregate 聚合 Qoder SSE 为标准 chat.completion 非流式响应。
func (h *Handler) proxyQoderAggregate(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ExtManager Qoder 桥接需要的最小外部账号能力（main 注入实现）。
type ExtManager interface {
	ReplaceCred(provider, id string, cred json.RawMessage)
	// NoteChatResult 上报一次对话结果，驱动外部账号的冷却/退避
	//（extstore.SelectChatOrder 据此把失败的号退到队尾）。
	NoteChatResult(provider, id string, err error)
}

// ExtSetManager 注入外部账号管理器（main 装配时调用；nil = Qoder 桥接禁用）。
func (h *Handler) ExtSetManager(m ExtManager) { h.extManager = m }

// noteChat 上报一次对话结果。
//
// 统一从这里走：桥接里散落 `if h.extManager != nil` 会漏掉（漏一次 = 那个号
// 永远不进冷却，每次都白白撞），而裸用/测试场景没有管理器时静默忽略即可。
func (h *Handler) noteChat(provider, id string, err error) {
	if h.extManager == nil {
		return
	}
	h.extManager.NoteChatResult(provider, id, err)
}

// errOf 把失败原因串转成 error。
//
// 桥接内部一直用字符串（最终要拼进 `h.lastXxxErr` 文案给用户看），而上报接口
// 要 error——两者的形状差只在这一个转换点收口，不用改每个桥接的变量类型。
func errOf(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// qoderModelPrefix Qoder 路由前缀。
const qoderModelPrefix = "qoder:"

// isQoderModel 判断裸模型名是否请求 Qoder 通道。
func isQoderModel(bare string) bool { return strings.HasPrefix(bare, qoderModelPrefix) }
