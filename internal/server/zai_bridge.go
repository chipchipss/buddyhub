// Package server: zai_bridge.go — Z.AI / 智谱 GLM API Key 直连路由。
//
// 模型名带 "zai:" 前缀（如 zai:GLM-5.3-Flash，大小写不敏感别名自动映射）时走
// 本路径：OpenAI chat 请求 → Anthropic messages 体（upstream.ZaiTranslateIn）
// → api.z.ai / open.bigmodel.cn（x-api-key 鉴权，免验证码）→ 响应反向翻译
// （JSON 直转；SSE 经 ZaiSSEConverter 有状态转换）。凭据取 config 的
// schedule.zai 段（zai_keys / bigmodel_keys 轮换），皆空时 503 提示。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/upstream"
)

// zaiKeys 取两池 key（config schedule.zai 透传到 Config 值）。
func (h *Handler) zaiKeys() (zai, bigmodel []string) {
	return h.cfg.ZaiKeys, h.cfg.BigModelKeys
}

// ZaiChatStream Z.AI 通道：翻译→转发→反译。返回 false = 无可用 key。
func (h *Handler) ZaiChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	zaiKeys, bmKeys := h.zaiKeys()
	type pool struct {
		kind string
		keys []string
	}
	pools := []pool{{"zai", zaiKeys}, {"bigmodel", bmKeys}}

	anthBody, model, err := upstream.ZaiTranslateIn(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return true
	}

	lastErr := "没有可用的 Z.AI / 智谱 API Key（配置 schedule.zai 后重试）"
	for _, p := range pools {
		for _, key := range p.keys {
			rc, status, ctype, err := upstream.ZaiPostMessages(p.kind, key, anthBody)
			if err != nil {
				lastErr = fmt.Sprintf("%s: %v", p.kind, err)
				// 401/403/402 = 该 key 失效/欠费，换下一把；429 也换（免费并发低）
				if status == http.StatusUnauthorized || status == http.StatusForbidden ||
					status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
					log.Printf("zai-bridge: %s key 失效/限流 (HTTP %d)", p.kind, status)
					continue
				}
				log.Printf("zai-bridge: %s 请求失败 (HTTP %d): %v", p.kind, status, err)
				continue
			}
			isSSE := strings.Contains(ctype, "text/event-stream")
			if h.clientWantsStream(body) {
				if isSSE {
					h.proxyZaiSSE(w, rc, model)
				} else {
					// 上游 JSON：包成单帧 SSE（客户端要流式而上游非流式）
					h.proxyZaiJSONAsSSE(w, rc, model)
				}
			} else {
				if isSSE {
					writeJSON(w, http.StatusOK, zaiAggregateSSE(rc, model))
				} else {
					defer rc.Close()
					raw, rerr := io.ReadAll(rc)
					if rerr != nil {
						writeOpenAIError(w, http.StatusBadGateway, "upstream_read", rerr.Error())
						return true
					}
					out, terr := upstream.ZaiTranslateOutJSON(raw, model)
					if terr != nil {
						writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", terr.Error())
						return true
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(out)
				}
			}
			log.Printf("zai-bridge: kind=%s model=%s 建流成功 ctype=%s", p.kind, model, ctype)
			return true
		}
	}
	h.lastZaiErr = lastErr
	return false
}

// proxyZaiSSE 读 Anthropic SSE 事件流 → 转换为 OpenAI chunk 流写出。
func (h *Handler) proxyZaiSSE(w http.ResponseWriter, rc io.ReadCloser, model string) {
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
	conv := upstream.NewZaiSSEConverter(model)
	upstream.ReadAnthropicSSE(rc, func(event []byte) {
		chunks, err := conv.Feed(event)
		if err != nil {
			return
		}
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// proxyZaiJSONAsSSE 上游 JSON → 单帧 SSE（含 usage 字段还原）+ [DONE]。
func (h *Handler) proxyZaiJSONAsSSE(w http.ResponseWriter, rc io.ReadCloser, model string) {
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	out, terr := upstream.ZaiTranslateOutJSON(raw, model)
	if terr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", terr.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "no flusher")
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", out)
	flusher.Flush()
}

// zaiAggregateSSE 逐帧扫描 Anthropic SSE 拼接 text_delta 聚合为 OpenAI chat.completion。
// tool_calls 聚合：tool_use 首片(name/id) + input_json_delta 拼接,按 index 分槽。
func zaiAggregateSSE(rc io.ReadCloser, model string) map[string]any {
	defer rc.Close()
	var text strings.Builder
	in, out := 0, 0
	stop := "stop"
	type toolAcc struct{ id, name, args strings.Builder }
	tools := map[int]*toolAcc{}
	br := bufio.NewReaderSize(rc, 64*1024)
	type evt struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				break
			}
			var e evt
			if json.Unmarshal([]byte(payload), &e) == nil {
				switch e.Type {
				case "message_start":
					in = e.Message.Usage.InputTokens
				case "content_block_start":
					if e.ContentBlock.Type == "tool_use" {
						tools[e.Index] = &toolAcc{}
					}
				case "content_block_delta":
					switch e.Delta.Type {
					case "text_delta":
						text.WriteString(e.Delta.Text)
					case "input_json_delta":
						if t, ok := tools[e.Index]; ok {
							t.args.WriteString(e.Delta.PartialJSON)
						}
					}
				case "message_delta":
					if e.Delta.StopReason != "" {
						stop = map[string]string{"max_tokens": "length", "tool_use": "tool_calls", "end_turn": "stop"}[e.Delta.StopReason]
						if stop == "" {
							stop = "stop"
						}
					}
					if e.Usage.OutputTokens > 0 {
						out = e.Usage.OutputTokens
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(tools) > 0 {
		var tcs []map[string]any
		for idx, t := range tools {
			tcs = append(tcs, map[string]any{
				"index": idx, "id": t.id.String(), "type": "function",
				"function": map[string]any{"name": t.name.String(), "arguments": t.args.String()},
			})
		}
		msg["tool_calls"] = tcs
	}
	return map[string]any{
		"id": "chatcmpl-zai", "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": stop}},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
	}
}
