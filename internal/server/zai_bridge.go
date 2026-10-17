// Package server: zai_bridge.go — Z.AI / ZCode 通道（Plan JWT + API Key 回退）。
//
// 模型名带 "zai:" 前缀时走本路径：
//
//	OpenAI chat 请求 → Anthropic messages 体（upstream.ZaiTranslateIn）
//	  → zai.Client.Do（池化选号 → 取验证码 → 构造身份头 → 分类失败 → 更新账号状态）
//	  → 响应反向翻译（JSON 直转；SSE 经 ZaiSSEConverter 有状态转换）
//
// 账号来源与运行态由 internal/zai 管理（data/zai-accounts.json），
// 不再直接从 config 的 key 数组轮询——那套已被账号池取代（首次启动会自动导入）。
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

// ZaiChatStream Z.AI 通道：翻译 → 池化转发 → 反译。
//
// 返回的 outcome：served=已写响应；matched 且未 served = 什么都没写，
// 调用方按 code/detail/lastErr 报错，**不回落腾讯池**。
// lastErr 是**本请求**的失败原因（请求级局部量，不跨请求共享）。
//
// ── 为什么这条必须自己带 lastErr 回来 ─────────────────────────────
// 它的未 served 路径有两条（未配置账号 / 无可用账号），dispatch 侧要拿
// 本次的失败原因拼 503 文案（有具体原因时说「通道暂不可用：<原因>」，
// 没有时才说「配置 schedule.zai 后重试」）。迁移前那两处读的是 Handler
// 上的共享字段，正是本次竞争修复的对象；现在原因随 outcome 回来，
// 每个并发请求读到的都是自己的那一条。
func (h *Handler) ZaiChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bridgeOutcome {
	const noZaiKeyMsg = "没有可用的 Z.AI / 智谱 通道（配置 schedule.zai 或面板添加账号后重试）"
	// zaiFail 未 served 的 outcome 组装：文案口径保持迁移前 dispatch 的特例——
	// 有具体原因时说「Z.AI 通道暂不可用：<原因>」，没有任何凭据线索时才说
	// 「没有可用的 Z.AI / 智谱 通道」。**不**走 bridgeMiss：那条统一口径
	// （「池内账号均已尝试并失败」）对 zai 是错的话术——Z.AI 凭据来自 config
	// 的 schedule.zai 而非账号池，「均已尝试并失败」描述的事在这里不会发生。
	zaiFail := func(reason string) bridgeOutcome {
		detail := noZaiKeyMsg
		if reason != "" {
			detail = "Z.AI 通道暂不可用：" + reason
		}
		return bridgeOutcome{matched: true, code: "no_zai_key", detail: detail, lastErr: reason}
	}
	if h.cfg.Zai == nil {
		return zaiFail("未配置 Z.AI 账号（面板「自动化 → Z.AI」添加，或 config 的 schedule.zai.zai_keys）")
	}

	// bareModel 仍带路由前缀「zai:」（它是网关侧协议，上游只认裸模型名）。其余桥接
	// 都在翻译前剥前缀并重写 body，zai 此前漏了这步——导致把字面量 "zai:glm-4-flash"
	// 发给上游，被判 1211 Unknown Model。剥前缀后小写别名映射才有机会命中。
	if m := strings.TrimPrefix(bareModel, zaiModelPrefix); m != bareModel {
		body = rewriteModel(body, m)
	}

	anthBody, model, err := upstream.ZaiTranslateIn(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return bridgeServed()
	}

	res, err := h.cfg.Zai.Do(r.Context(), anthBody)
	if err != nil {
		// 无可用账号：把本次的原因带回调用方（见函数头注释）。
		log.Printf("zai-bridge: 无可用账号：%v", err)
		return zaiFail(err.Error())
	}
	rc := res.Resp.Body
	ctype := res.Resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ctype, "text/event-stream")

	channel := "apikey"
	if res.UsedPlan {
		channel = "plan"
	}
	log.Printf("zai-bridge: 账号=%s 通道=%s model=%s ctype=%s", res.Account.Name, channel, model, ctype)

	if h.clientWantsStream(body) {
		if isSSE {
			h.proxyZaiSSE(w, rc, model)
		} else {
			// 上游 JSON：包成单帧 SSE（客户端要流式而上游非流式）
			h.proxyZaiJSONAsSSE(w, rc, model)
		}
		return bridgeServed()
	}

	if isSSE {
		writeJSON(w, http.StatusOK, zaiAggregateSSE(rc, model))
		return bridgeServed()
	}
	defer rc.Close()
	raw, rerr := io.ReadAll(rc)
	if rerr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", rerr.Error())
		return bridgeServed()
	}
	out, terr := upstream.ZaiTranslateOutJSON(raw, model)
	if terr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", terr.Error())
		return bridgeServed()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return bridgeServed()
}

// zaiStatusJSON 供面板/状态接口读取的 Z.AI 通道概览。
//
// 注意：这里**不再**回 `last_error`（上次 zai: 对话的失败原因）。那个值原先
// 挂在 Handler 字段上跨请求共享，是 R-1 数据竞争修复删掉的那批字段之一；
// 请求级的原因现在只随当次请求的响应透出（见 ZaiChatStream 的 outcome）。
// 账号级失败原因另有真相源：zai pool 的账号快照（internal/zai 的 LastErr）。
func (h *Handler) zaiStatusJSON() map[string]any {
	if h.cfg.Zai == nil {
		return map[string]any{"configured": false}
	}
	out := map[string]any{
		"configured": true,
		"accounts":   h.cfg.Zai.Snapshots(),
		"pool":       h.cfg.Zai.Stats(),
	}
	if h.cfg.ZaiCaptcha != nil {
		out["captcha"] = map[string]any{
			"enabled":    h.cfg.ZaiCaptcha.Enabled(),
			"pool_size":  h.cfg.ZaiCaptcha.PoolSize(),
			"last_error": h.cfg.ZaiCaptcha.LastError(),
		}
	}
	return out
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
