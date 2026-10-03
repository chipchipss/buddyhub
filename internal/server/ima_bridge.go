// Package server: ima_bridge.go — 腾讯 ima.copilot 直连路由。
//
// 模型名带 "ima:" 前缀时走本路径。与其它通道的关键差异（见 extprovider/ima
// 的模块头）：**会话制 + 自定义 SSE**——要先 init_session，问答走
// /cgi-bin/assistant/qa，事件名全大写，业务错误藏在 200 的流里。
//
// 出站/入站的映射：
//   - 客户端的多轮 messages → ima 只收单条 question：取最后一条 user 消息
//     （与 traework 的 lastUserText 同策略；会话连续性由 ima 的 session 维持）
//   - 上游 SSE → 标准 OpenAI chunk（TEXT_DELTA 的增量 → delta.content）
//   - 会话满（INNER_EXCEPTION）→ 重建会话重试一次（参考实现同款）
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/ima"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// imaChatStream 转发到 ima。返回 false = 无可用账号。
func (h *Handler) imaBridgeChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PIMA && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	// 模型名 → (type, id)；未知名字回落默认（上游自己也有路由层）
	modelName := strings.TrimPrefix(bareModel, imaModelPrefix)
	m := ima.Lookup(modelName)
	if m == nil {
		d := ima.Default()
		m = &d
	}

	question := lastUserText(body)
	if question == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages 里没有 user 轮次")
		return true
	}
	// 会话键：客户端的 conversation_id（粘性）或请求级随机
	convID := conversationIDOf(body)
	wantStream := h.clientWantsStream(body)

	lastErr := ""
	for _, a := range accounts {
		var cred ima.Credential
		if json.Unmarshal(a.Cred, &cred) != nil || cred.Cookie == "" {
			lastErr = "凭据解析失败或缺少 cookie"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		// 凭据临期先续期（cookie 里的 IMA-TOKEN 约 2 小时到期，refresh_token
		// 通常随 cookie 一起带）。续期成功后回写账号表，下轮直接命中。
		if cred.NeedsRefresh() {
			if fresh, rerr := h.refreshIMACred(r.Context(), a.ID, &cred); rerr == nil {
				cred = *fresh
			} else {
				log.Printf("ima-bridge: %s 续期失败: %v", a.ID, rerr)
			}
		}

		resp, err := ima.AskStream(r.Context(), convID, a.ID, question, *m, &cred)
		if err != nil {
			lastErr = err.Error()
			log.Printf("ima-bridge: %s 问答失败: %v", a.ID, err)
			if isIMAAuthError(err) {
				// 认证失败（code=41/600001/5/51）：换新 token 并丢弃会话再试一次；
				// 续期也救不回来才换下一个账号。
				fresh, rerr := h.refreshIMACred(r.Context(), a.ID, &cred)
				if rerr == nil {
					cred = *fresh
					ima.DropSession(convID, a.ID)
					resp, err = retryWithNewSession(r.Context(), convID, a.ID, question, *m, &cred)
					if err == nil {
						lastErr = ""
					} else {
						lastErr = fmt.Sprintf("%s（续期后仍失败：%v）", lastErr, err)
					}
				} else {
					lastErr = fmt.Sprintf("%s（续期也失败：%v）", lastErr, rerr)
				}
				if err != nil {
					h.noteChat(a.Provider, a.ID, errOf(lastErr))
					continue
				}
			} else {
				// 会话类错误：重建会话再试一次（同账号）
				resp, err = retryWithNewSession(r.Context(), convID, a.ID, question, *m, &cred)
				if err != nil {
					h.noteChat(a.Provider, a.ID, errOf(lastErr))
					continue
				}
			}
		}

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			lastErr = "上游 HTTP " + itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			log.Printf("ima-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		log.Printf("ima-bridge: acct=%s model=%s 建流成功", a.ID, m.Name)
		if wantStream {
			// 流式：流内 INNER_EXCEPTION 已成流，无法换会话重试——包成 error 帧透出
			h.translateIMA(w, resp, bareModel, true)
		} else {
			// 非流式：还能救——聚合时遇到会话满就重建会话重问一次
			doc, aerr := h.collectIMAWithRetry(r.Context(), convID, a.ID, question, *m, &cred, resp)
			if aerr != nil {
				lastErr = aerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
			writeJSON(w, http.StatusOK, doc)
		}
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastIMAErr = lastErr
	return false
}

// refreshIMACred 换新 IMA-TOKEN + 回写账号表。
//
// **单飞是必需的**：refresh_token 是一次性轮换语义（换成功后旧 token 即作废），
// 并发的多个请求各自续期一次会让后到的拿着作废的 token → 全线 600001。
// 同账号的续期合并成一次，其余请求复用结果（与 cline/autoclaw 的单飞同款理由）。
func (h *Handler) refreshIMACred(ctx context.Context, accountID string, cred *ima.Credential) (*ima.Credential, error) {
	fresh, err := h.imaFlights.Do(accountID, func() (*ima.Credential, error) {
		return cred.Refresh(ctx)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PIMA, accountID, raw)
		}
	}
	return fresh, err
}

// retryWithNewSession 丢弃缓存的会话重建一次再问。
func retryWithNewSession(ctx context.Context, convID, accountID, question string, m ima.Model, cred *ima.Credential) (*http.Response, error) {
	ima.DropSession(convID, accountID)
	sessionID, err := ima.InitSession(ctx, question, cred)
	if err != nil {
		return nil, err
	}
	ima.PutSession(convID, accountID, sessionID)
	return ima.QAStream(ctx, sessionID, question, m, cred)
}

// isIMAAuthError 上游明确说凭据失效（该换账号，而不是重建会话）。
func isIMAAuthError(err error) bool {
	var ie *ima.IMAError
	if ok := asIMAError(err, &ie); ok {
		return ie.Auth
	}
	return false
}

// asIMAError errors.As 的本地包装（避免在每个调用点 import errors）。
func asIMAError(err error, target **ima.IMAError) bool {
	for err != nil {
		if e, ok := err.(*ima.IMAError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// collectIMAWithRetry 非流式聚合：读到流内会话错误（INNER_EXCEPTION 等）时
// 重建会话重问一次。参考实现 collectResponseText + ensureSession(force) 的同款语义。
func (h *Handler) collectIMAWithRetry(ctx context.Context, convID, accountID, question string,
	m ima.Model, cred *ima.Credential, resp *http.Response) (map[string]any, error) {
	defer resp.Body.Close()
	text, hadErr, errMsg := drainIMA(resp)
	if !hadErr {
		return imaCompletion(text, m.Name), nil
	}
	// 流内错误 → 重建会话重问一次
	ima.DropSession(convID, accountID)
	sessionID, err := ima.InitSession(ctx, question, cred)
	if err != nil {
		return nil, fmt.Errorf("ima 流内错误（%s）且重建会话失败: %w", errMsg[:min(80, len(errMsg))], err)
	}
	ima.PutSession(convID, accountID, sessionID)
	resp2, err := ima.QAStream(ctx, sessionID, question, m, cred)
	if err != nil {
		return nil, fmt.Errorf("ima 流内错误（%s）且重试失败: %w", errMsg[:min(80, len(errMsg))], err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ima 重试 HTTP %d", resp2.StatusCode)
	}
	text2, hadErr2, errMsg2 := drainIMA(resp2)
	if hadErr2 && text2 == "" {
		return nil, fmt.Errorf("ima 重试仍流内错误: %s", errMsg2[:min(120, len(errMsg2))])
	}
	return imaCompletion(text2, m.Name), nil
}

// drainIMA 读完一条 ima SSE 流：拼接文本、报告流内错误。
func drainIMA(resp *http.Response) (text string, hadErr bool, errMsg string) {
	_ = ima.ScanSSE(resp.Body, func(ev ima.QAEvent) bool {
		if ev.IsError() {
			hadErr, errMsg = true, ev.Data
			return false
		}
		text += ev.Text()
		return !ev.IsControl()
	})
	return text, hadErr, errMsg
}

// imaCompletion 聚合结果 → 标准 chat.completion。
func imaCompletion(text, model string) map[string]any {
	return map[string]any{
		"id": newChunkID(), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	}
}

// translateIMA 把 ima 的事件流转成客户端要的形态。
func (h *Handler) translateIMA(w http.ResponseWriter, resp *http.Response, model string, wantStream bool) {
	defer resp.Body.Close()

	// 聚合：非流式客户端拿一条完整 chat.completion
	if !wantStream {
		var sb strings.Builder
		var hadError bool
		errMsg := ""
		_ = ima.ScanSSE(resp.Body, func(ev ima.QAEvent) bool {
			if ev.IsError() {
				hadError = true
				errMsg = ev.Data
				return false
			}
			sb.WriteString(ev.Text())
			return !ev.IsControl()
		})
		if hadError && sb.Len() == 0 {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "ima 流内错误："+errMsg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": newChunkID(), "object": "chat.completion",
			"created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": sb.String()},
				"finish_reason": "stop",
			}},
		})
		return
	}

	// 流式：TEXT_DELTA → delta.content；结束补 [DONE]
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
	send := func(content any, finish *string) {
		choice := map[string]any{"index": 0, "delta": map[string]any{}}
		if s, isStr := content.(string); isStr && s != "" {
			choice["delta"] = map[string]any{"content": s}
		}
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

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event, data strings.Builder
	flushEv := func() (ima.QAEvent, bool) {
		if event.String() == "" && data.Len() == 0 {
			return ima.QAEvent{}, false
		}
		ev := ima.QAEvent{Event: event.String(), Data: data.String()}
		event.Reset()
		data.Reset()
		return ev, true
	}
	stop := false
	for sc.Scan() && !stop {
		line := sc.Text()
		switch {
		case line == "":
			if ev, ok := flushEv(); ok {
				if ev.IsError() {
					// 流内错误：包成 error 帧给客户端（已成流的响应无法换状态码）
					raw, _ := json.Marshal(map[string]any{
						"error": map[string]any{"message": "ima 流内错误：" + ev.Data, "type": "upstream_error"},
					})
					_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
					flusher.Flush()
					stop = true
					break
				}
				if txt := ev.Text(); txt != "" {
					send(txt, nil)
				}
				if ev.IsControl() {
					stop = true
				}
			}
		case strings.HasPrefix(line, "event:"):
			event.WriteString(strings.TrimSpace(line[len("event:"):]))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimPrefix(line[len("data:"):], " "))
		case line == "data":
			data.WriteString("\n")
		}
	}
	var done = "stop"
	send(map[string]any{}, &done)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// conversationIDOf 取客户端的 conversation_id（会话粘性）；没有就请求级随机。
func conversationIDOf(body []byte) string {
	var peek struct {
		User       string `json:"user"`
		ConvID     string `json:"conversation_id"`
		ConvID2    string `json:"conversationId"`
		SessionKey string `json:"session_key"`
	}
	_ = json.Unmarshal(body, &peek)
	if peek.ConvID != "" {
		return peek.ConvID
	}
	if peek.ConvID2 != "" {
		return peek.ConvID2
	}
	if peek.SessionKey != "" {
		return peek.SessionKey
	}
	return newChunkID() // 请求级：每次新会话
}

/* ── 模型目录（静态清单） ─────────────────────────────────────── */

var imaModelsOnce sync.Once
var imaModelsList []ima.Model

// imaCatalog 静态模型清单（official_N 是官方写死的 id，漂移概率低——
// 与 qoder 的静态目录同一取舍）。
func imaCatalog() []ima.Model {
	imaModelsOnce.Do(func() { imaModelsList = ima.Models() })
	return imaModelsList
}

// imaModelPrefix ima 路由前缀。
const imaModelPrefix = "ima:"

// isIMAModel 判断裸模型名是否请求 ima 通道。
func isIMAModel(bare string) bool { return strings.HasPrefix(bare, imaModelPrefix) }
