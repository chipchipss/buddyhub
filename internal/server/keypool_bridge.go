// Package server: keypool_bridge.go — Codex 订阅池与免费 key 池的路由。
//
// 模型前缀路由：
//   - "codex:<model>"  → ChatGPT 订阅号池（本机 ~/.codex* 凭据，Responses API）
//   - "free:<provider>/<model>" → 免费 key 池（groq/zp/l7/or，config 注入 key）
//
// Codex 上游返回 Responses 事件流 → 实时转译为 OpenAI chat SSE（复用
// responsesStreamTranslate 的反向逻辑）；免费池上游返回标准 chat SSE → 原样透传。
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

	"github.com/chipchipss/buddyhub/internal/extprovider/keypool"
)

// keyPoolKeys 免费 key 池配置（main 从 config 注入）。
type keyPoolKeys struct {
	mu   sync.RWMutex
	keys map[string]string // provider → api key
}

var poolKeys = &keyPoolKeys{keys: map[string]string{}}

// SetKeyPoolKeys 注入各上游 API key（main 启动时从 config 读取）。
func SetKeyPoolKeys(keys map[string]string) {
	poolKeys.mu.Lock()
	poolKeys.keys = keys
	poolKeys.mu.Unlock()
}

// KeyPoolDesc 免费池概览（provider + 掩码 key；面板统一账号目录用）。
func KeyPoolDesc() []struct{ Provider, Masked string } {
	poolKeys.mu.RLock()
	defer poolKeys.mu.RUnlock()
	out := make([]struct{ Provider, Masked string }, 0, len(poolKeys.keys))
	for k, v := range poolKeys.keys {
		if v == "" {
			continue
		}
		masked := v
		if len(v) > 8 {
			masked = v[:6] + "…" + v[len(v)-4:]
		} else {
			masked = strings.Repeat("*", len(v))
		}
		out = append(out, struct{ Provider, Masked string }{k, masked})
	}
	return out
}

// CodexCount 本机 Codex 登录数。
func CodexCount() int { return len(keypool.ListCodexAccounts()) }

// keyFor 取指定上游的 key（缺省空串 = 匿名层，仅 LLM7 支持）。
func keyFor(provider string) string {
	poolKeys.mu.RLock()
	defer poolKeys.mu.RUnlock()
	return poolKeys.keys[provider]
}

const (
	codexModelPrefix = "codex:"
	freeModelPrefix  = "free:"
)

// isCodexModel / isFreeModel 前缀判定。
func isCodexModel(bare string) bool { return strings.HasPrefix(bare, codexModelPrefix) }
func isFreeModel(bare string) bool  { return strings.HasPrefix(bare, freeModelPrefix) }

// codexCooldown 账号冷却表（key → 冷却截止，进程内）。
var codexCooldown sync.Map

// CodexChatStream ChatGPT 订阅池直连。返回 false = 无可用账号。
// 上游为 Responses 事件流；客户端要流式时转译为 chat SSE，非流式时聚合转 chat.completion。
func (h *Handler) codexChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	// 先清：那是**上一次请求**的事实，不该挂进本次的失败提示里。
	h.lastCodexErr = ""
	accounts := keypool.ListCodexAccounts()
	if len(accounts) == 0 {
		h.lastCodexErr = "本机未发现有效 Codex 登录（~/.codex*/auth.json 均缺失或已过期）"
		return false
	}
	now := time.Now().UnixMilli()
	var lastErr string
	for _, acct := range accounts {
		if cd, ok := codexCooldown.Load(acct.Key); ok && cd.(int64) > now {
			continue
		}
		model := strings.TrimPrefix(bareModel, codexModelPrefix)
		_ = model
		rc, status, err := keypool.New().CodexChatStream(r.Context(), acct, body, "")
		if err != nil {
			// 401 = token 过期；429 = 用量窗口耗尽 —— 都记冷却。
			if status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
				codexCooldown.Store(acct.Key, now+15*60*1000)
			}
			lastErr = fmt.Sprintf("%s: HTTP %d: %v", acct.Key, status, err)
			log.Printf("codex-bridge: %s 失败 (HTTP %d): %v", acct.Key, status, err)
			continue
		}

		// 上游是 Responses 事件流：按客户端形态转译。
		if h.clientWantsStream(body) {
			// Responses 事件流 → chat.completion.chunk SSE：直接透传 Responses
			// 事件会让 Chat 客户端无法解析，故统一走聚合→chat 形态。
			resp, aerr := codexAggregate(rc)
			rc.Close()
			if aerr != nil {
				lastErr = aerr.Error()
				continue
			}
			writeJSON(w, http.StatusOK, resp)
			return true
		}
		resp, aerr := codexAggregate(rc)
		rc.Close()
		if aerr != nil {
			lastErr = aerr.Error()
			continue
		}
		writeJSON(w, http.StatusOK, resp)
		return true
	}
	h.lastCodexErr = lastErr
	return false
}

// codexAggregate 把 Codex Responses SSE 聚合成 chat.completion JSON。
// 复用 responses 事件流的 output item 完成态（message.output_text 拼接、
// function_call 归并、usage 转换——与 responsesStreamTranslate 收尾同口径）。
func codexAggregate(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	var content, reasoning string
	var toolCalls []any
	var usage map[string]any
	var respID, model string
	var finish = "stop"

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	type tcState struct {
		id, name string
		args     strings.Builder
	}
	tools := map[string]*tcState{}
	var toolOrder []string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // Responses 事件流有 event: 行；只取 data: 行
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Type     string         `json:"type"`
			Item     map[string]any `json:"item"`
			Delta    string         `json:"delta"`
			Text     string         `json:"text"`
			Args     string         `json:"arguments"`
			Name     string         `json:"name"`
			ItemID   string         `json:"item_id"`
			Response map[string]any `json:"response"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			content += ev.Delta
		case "response.reasoning_summary_text.delta":
			reasoning += ev.Delta
		case "response.output_item.done":
			if ev.Item != nil && ev.Item["type"] == "function_call" {
				id, _ := ev.Item["call_id"].(string)
				name, _ := ev.Item["name"].(string)
				args, _ := ev.Item["arguments"].(string)
				st := tools[id]
				if st == nil {
					st = &tcState{id: id, name: name}
					tools[id] = st
					toolOrder = append(toolOrder, id)
				}
				st.args.Reset()
				st.args.WriteString(args)
			}
			if ev.Item != nil && ev.Item["type"] == "message" {
				// message item done：content 已从 delta 拼齐，无需处理
			}
		case "response.function_call_arguments.delta":
			st := tools[ev.ItemID]
			if st != nil {
				st.args.WriteString(ev.Args)
			}
		case "response.function_call_arguments.done":
			st := tools[ev.ItemID]
			if st != nil {
				st.args.Reset()
				st.args.WriteString(ev.Args)
			}
		case "response.completed":
			if ev.Response != nil {
				if v, ok := ev.Response["usage"].(map[string]any); ok {
					usage = v
				}
				if v, ok := ev.Response["id"].(string); ok {
					respID = v
				}
				if v, ok := ev.Response["model"].(string); ok {
					model = v
				}
			}
		case "response.failed":
			if ev.Response != nil {
				return nil, fmt.Errorf("codex 上游返回 response.failed（流中断或配额耗尽）")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	_ = reasoning // Codex 的 reasoning summary 不回填 reasoning_content（summary 非全文）

	for _, id := range toolOrder {
		st := tools[id]
		toolCalls = append(toolCalls, map[string]any{
			"id":       st.id,
			"type":     "function",
			"function": map[string]any{"name": st.name, "arguments": st.args.String()},
		})
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if content == "" {
			msg["content"] = nil
		}
	}
	if respID == "" {
		respID = "chatcmpl-codex-" + compactUUID()
	}
	if model == "" {
		model = "codex"
	}
	out := map[string]any{
		"id": respID, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if usage != nil {
		// Responses usage → chat usage 字段名映射
		out["usage"] = map[string]any{
			"prompt_tokens":     usage["input_tokens"],
			"completion_tokens": usage["output_tokens"],
			"total_tokens":      usage["total_tokens"],
		}
	}
	return json.Marshal(out)
}

// keyPoolChatStream 免费 key 池直连（groq/zp/l7/or）。返回 false = 无 key 或全失败。
func (h *Handler) keyPoolChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	// free:<provider>/<model> → provider + upstream model
	rest := strings.TrimPrefix(bareModel, freeModelPrefix)
	slash := strings.Index(rest, "/")
	if slash < 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model",
			"free: 模型格式为 free:<provider>/<model>，如 free:groq/openai/gpt-oss-120b")
		return true
	}
	provider := rest[:slash]
	upstreamModel := rest[slash+1:]
	switch provider {
	case keypool.PGroq, keypool.PZhipu, keypool.PLLMS7, keypool.POpenRouter:
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", "未知免费上游: "+provider)
		return true
	}
	apiKey := keyFor(provider)
	if apiKey == "" && provider != keypool.PLLMS7 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_api_key",
			fmt.Sprintf("未配置 %s 的 API key（config 免费池注入）", provider))
		return true
	}
	// 请求体模型名重写为上游真名
	var req map[string]any
	if json.Unmarshal(body, &req) == nil {
		req["model"] = upstreamModel
		if fixed, err := json.Marshal(req); err == nil {
			body = fixed
		}
	}
	rc, status, err := keypool.New().ChatStream(r.Context(), provider, apiKey, body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "free_pool_error", fmt.Sprintf("HTTP %d: %v", status, err))
		return true
	}
	if h.clientWantsStream(body) {
		h.proxySSEPassthrough(w, rc)
	} else {
		resp, aerr := upstreamAggregateFromSSE(streamToBytes(rc))
		rc.Close()
		if aerr != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", aerr.Error())
			return true
		}
		writeJSON(w, http.StatusOK, json.RawMessage(resp))
	}
	return true
}

// streamToBytes 把流读尽（免费池聚合入口）。
func streamToBytes(rc io.ReadCloser) []byte {
	defer rc.Close()
	var ctx = context.Background()
	_ = ctx
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := rc.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

// proxySSEPassthrough SSE 原样透传（帧格式已是标准 OpenAI chunk）。
func (h *Handler) proxySSEPassthrough(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
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
