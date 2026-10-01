// Package server: traework_bridge.go — TraeWork（字节）直连路由。
//
// 模型名带 "traework:" 前缀时走本路径。上游是**会话式**协议：一轮对话要
// 「建会话 → 开事件流 → 发消息」，事件流是独立长连接，且事件体没有稳定形状
// （递归收集，见 traework 包的注释）。
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/traework"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// traeworkChatStream 转发到 TraeWork。返回 false = 无可用账号。
func (h *Handler) traeworkChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	prompt := lastUserText(body)
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages 里没有 user 轮次")
		return true
	}

	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PTraeWork && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred traework.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.AccessToken == "" {
			lastErr = "账号缺少 access_token"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		if h.clientWantsStream(body) {
			h.streamTraeWork(w, r, &cred, prompt, bareModel)
			h.noteChat(a.Provider, a.ID, nil)
			return true
		}

		turn, err := traework.RunTurn(r.Context(), &cred, prompt, nil)
		if err != nil {
			lastErr = err.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("traework-bridge: %s 对话失败: %v", a.ID, err)
			continue
		}
		writeJSON(w, http.StatusOK, buildTraeWorkCompletion(turn, bareModel))
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastTraeWorkErr = lastErr
	return false
}

// streamTraeWork 把会话事件流边收边转成 OpenAI SSE。
func (h *Handler) streamTraeWork(w http.ResponseWriter, r *http.Request, cred *traework.Credential, prompt, model string) {
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

	id := newChunkID()
	created := time.Now().Unix()
	send := func(delta map[string]any, finish *string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			choice["finish_reason"] = *finish
		} else {
			choice["finish_reason"] = nil
		}
		raw, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": model, "choices": []any{choice},
		})
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
		flusher.Flush()
	}

	_, err := traework.RunTurn(r.Context(), cred, prompt, func(text string) {
		send(map[string]any{"content": text}, nil)
	})
	if err != nil {
		raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
		return
	}
	stop := "stop"
	send(map[string]any{}, &stop)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// buildTraeWorkCompletion 非流式聚合结果。
func buildTraeWorkCompletion(turn *traework.Turn, model string) map[string]any {
	msg := map[string]any{"role": "assistant", "content": turn.Text}
	if turn.Reasoning != "" {
		msg["reasoning_content"] = turn.Reasoning
	}
	out := map[string]any{
		"id": newChunkID(), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
	}
	if len(turn.Usage) > 0 {
		var u any
		if json.Unmarshal(turn.Usage, &u) == nil {
			out["usage"] = u
		}
	}
	return out
}

// lastUserText 取最后一条 user 消息的纯文本（上游只接受单轮 prompt）。
func lastUserText(body []byte) string {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	for i := len(doc.Messages) - 1; i >= 0; i-- {
		m := doc.Messages[i]
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			var b strings.Builder
			for _, blk := range blocks {
				if blk.Type == "text" {
					b.WriteString(blk.Text)
				}
			}
			if strings.TrimSpace(b.String()) != "" {
				return b.String()
			}
		}
	}
	return ""
}

/* ── 模型目录（10 分钟缓存） ─────────────────────────────────── */

var (
	traeworkCatMu   sync.Mutex
	traeworkCatAt   time.Time
	traeworkCatMods []traework.Model
)

func (h *Handler) traeworkCatalog() []traework.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	traeworkCatMu.Lock()
	if time.Since(traeworkCatAt) < 10*time.Minute && traeworkCatMods != nil {
		mods := traeworkCatMods
		traeworkCatMu.Unlock()
		return mods
	}
	traeworkCatMu.Unlock()

	var cred *traework.Credential
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PTraeWork || a.Disabled {
			continue
		}
		var c traework.Credential
		if json.Unmarshal(a.Cred, &c) == nil && c.AccessToken != "" {
			cred = &c
			break
		}
	}
	if cred == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mods, _ := traework.ListModels(ctx, cred) // 内部已回落到静态名单
	if len(mods) == 0 {
		return nil
	}
	traeworkCatMu.Lock()
	traeworkCatMods, traeworkCatAt = mods, time.Now()
	traeworkCatMu.Unlock()
	return mods
}

// traeworkModelPrefix TraeWork 路由前缀。
const traeworkModelPrefix = "traework:"

// isTraeWorkModel 判断裸模型名是否请求 TraeWork 通道。
func isTraeWorkModel(bare string) bool { return strings.HasPrefix(bare, traeworkModelPrefix) }
