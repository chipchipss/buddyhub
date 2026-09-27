// Package server: responses.go — OpenAI Responses API 端点。
//
// 移植自 workbuddy-gateway responses.go 的协议转换层（responsesToChatRequest /
// chatCompletionToResponses / SSE 转译），对接 panel 既有出站管线：
//   - 入站 Responses 请求 → 转 Chat Completions 请求体 → 复用 h.chatCompletions
//     的轮转/粘性/成本学习全部逻辑（通过内部 HTTP 转发，而非复制轮转代码）；
//   - 非流式：panel 聚合返回 chat.completion → chatCompletionToResponses 转 Responses；
//   - 流式：panel 输出 chat SSE → 实时转译为 Responses 语义事件流。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mergedToolCall 累积合并上游按 index 分片下发的工具调用增量。
type mergedToolCall struct {
	ID   string
	Type string
	Name string
	Args strings.Builder
}

// applyToolCallDelta 将上游 tool_calls 增量按 index 归并进 map；order 记录首次出现的
// index 顺序，保证最终输出顺序稳定。
func applyToolCallDelta(toolCalls map[int]*mergedToolCall, order *[]int, tcs []any) {
	for _, tcAny := range tcs {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		st, exists := toolCalls[idx]
		if !exists {
			st = &mergedToolCall{Type: "function"}
			toolCalls[idx] = st
			*order = append(*order, idx)
		}
		if id, ok := tc["id"].(string); ok && id != "" {
			st.ID = id
		}
		if t, ok := tc["type"].(string); ok && t != "" {
			st.Type = t
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				st.Name = n
			}
			if a, ok := fn["arguments"].(string); ok {
				st.Args.WriteString(a)
			}
		}
	}
}

// responsesToChatRequest 将 Responses 请求体转换为 Chat Completions 请求体。
func responsesToChatRequest(respReq map[string]any, modelName string) (map[string]any, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if instructions, ok := respReq["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := respReq["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		// 一次模型回复拆成 reasoning、message、function_call 独立项；Chat 要求同一次
		// 回复是一条 assistant 消息。连续助手项在 user/tool 结果处结束，新 reasoning
		// 项也代表下一次助手回复。
		var pendingReasoning string
		var pendingAssistant map[string]any
		flushAssistant := func() {
			if pendingAssistant != nil {
				messages = append(messages, pendingAssistant)
				pendingAssistant = nil
			}
			pendingReasoning = ""
		}
		for _, itemAny := range input {
			switch item := itemAny.(type) {
			case string:
				flushAssistant()
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				typ, _ := item["type"].(string)
				if typ == "reasoning" {
					if pendingAssistant != nil {
						flushAssistant()
					}
					if text := reasoningReplayText(item); text != "" {
						if pendingReasoning != "" {
							pendingReasoning += "\n\n"
						}
						pendingReasoning += text
					}
					continue
				}
				if typ == "web_search_call" {
					continue
				}
				for _, msgAny := range convertResponsesInputItem(item) {
					msg, ok := msgAny.(map[string]any)
					if !ok {
						continue
					}
					if role, _ := msg["role"].(string); role == "assistant" {
						if pendingAssistant == nil {
							pendingAssistant = msg
							if pendingReasoning != "" {
								if _, explicit := pendingAssistant["reasoning_content"]; !explicit {
									pendingAssistant["reasoning_content"] = pendingReasoning
								}
							}
						} else {
							mergeResponsesAssistantItem(pendingAssistant, msg)
						}
						continue
					}
					flushAssistant()
					messages = append(messages, msg)
				}
			}
		}
		flushAssistant()
	}

	if len(messages) == 0 {
		return nil, fmt.Errorf("input 为空：Responses 请求必须提供 input 或 instructions")
	}
	chat["messages"] = messages

	if v, ok := respReq["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := respReq["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if v, ok := respReq["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if tools := convertResponsesTools(respReq["tools"]); len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc := convertResponsesToolChoice(respReq["tool_choice"]); tc != nil {
		chat["tool_choice"] = tc
	}
	if reasoning, ok := respReq["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" && effort != "none" {
			chat["reasoning_effort"] = effort
		}
	}
	return chat, nil
}

// reasoningReplayText 取出 reasoning 项里的推理正文：content 优先，summary 兜底；
// encrypted_content 不是明文，不当 reasoning_content。
func reasoningReplayText(item map[string]any) string {
	if text := joinReasoningParts(item["content"]); text != "" {
		return text
	}
	return joinReasoningParts(item["summary"])
}

func joinReasoningParts(raw any) string {
	parts, ok := raw.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, partAny := range parts {
		part, ok := partAny.(map[string]any)
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// mergeResponsesAssistantItem 将一个 Responses 回复的正文与工具调用合成同一条 Chat assistant。
func mergeResponsesAssistantItem(dst, item map[string]any) {
	if assistantMessageHasPayload(item) {
		if !assistantMessageHasPayload(dst) {
			dst["content"] = item["content"]
		} else {
			dst["content"] = append(responsesChatContentParts(dst["content"]), responsesChatContentParts(item["content"])...)
		}
	}
	if calls, ok := item["tool_calls"].([]any); ok && len(calls) > 0 {
		if existing, ok := dst["tool_calls"].([]any); ok {
			dst["tool_calls"] = append(existing, calls...)
		} else {
			dst["tool_calls"] = calls
		}
	}
}

func responsesChatContentParts(content any) []any {
	switch value := content.(type) {
	case []any:
		return value
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": value}}
	}
	return nil
}

// convertResponsesInputItem 将单个 Responses input item 转换为 0..1 条 chat 消息。
func convertResponsesInputItem(item map[string]any) []any {
	switch typ, _ := item["type"].(string); typ {
	case "function_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmptyStr(callID, "call_"+compactUUID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      stringifyToolOutput(item["output"]),
		}}
	case "reasoning", "web_search_call":
		return nil
	}

	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	message := map[string]any{"role": role, "content": convertResponsesContent(item["content"])}
	if role == "assistant" {
		if reasoning, ok := item["reasoning_content"].(string); ok {
			message["reasoning_content"] = reasoning
		}
	}
	return []any{message}
}

// convertResponsesContent 将 Responses content（string 或 parts 数组）转换为 chat content。
func convertResponsesContent(content any) any {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				if s, ok := pAny.(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "input_text", "output_text", "text":
				if t, ok := p["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t, ok := p["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				url, _ := p["image_url"].(string)
				if url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return fmt.Sprintf("%v", c)
	}
}

// stringifyToolOutput 将 function_call_output 的 output 统一转为字符串。
func stringifyToolOutput(output any) string {
	switch o := output.(type) {
	case nil:
		return ""
	case string:
		return o
	default:
		if b, err := json.Marshal(o); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", o)
	}
}

// convertResponsesTools 将 Responses 扁平 function 工具转换为 chat 嵌套 function 工具。
func convertResponsesTools(toolsAny any) []any {
	arr, ok := toolsAny.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, tAny := range arr {
		t, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := t["type"].(string)
		if typ != "" && typ != "function" {
			continue
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		params := t["parameters"]
		if name == "" {
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
				desc, _ = fn["description"].(string)
				params = fn["parameters"]
			}
		}
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if params != nil {
			fn["parameters"] = params
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// convertResponsesToolChoice 将 Responses tool_choice 转换为 chat tool_choice。
func convertResponsesToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v
		}
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "function" {
			name, _ := v["name"].(string)
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	return nil
}

// chatCompletionToResponses 将 chat.completion JSON 转换为 Responses 响应对象。
func chatCompletionToResponses(chatJSON []byte, modelName string) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(chatJSON, &chat); err != nil {
		return nil, err
	}

	var content, reasoning string
	var toolCalls []any
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				content, _ = msg["content"].(string)
				reasoning, _ = msg["reasoning_content"].(string)
				toolCalls, _ = msg["tool_calls"].([]any)
			}
		}
	}

	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":      "rs_" + compactUUID(),
			"type":    "reasoning",
			"status":  "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if content != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + compactUUID(),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": content, "annotations": []any{}},
			},
		})
	}
	for _, tcAny := range toolCalls {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		callID, _ := tc["id"].(string)
		name, args := "", ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		output = append(output, map[string]any{
			"id":        "fc_" + compactUUID(),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   ifEmptyStr(callID, "call_"+compactUUID()),
			"name":      name,
			"arguments": args,
		})
	}

	now := time.Now().Unix()
	created := now
	if v, ok := chat["created"].(float64); ok && v > 0 && int64(v) <= now {
		created = int64(v)
	}
	respID, _ := chat["id"].(string)
	if respID == "" {
		respID = "resp_" + compactUUID()
	} else {
		respID = "resp_" + strings.TrimPrefix(respID, "chatcmpl-")
	}

	result := buildResponsesEnvelope(respID, modelName, created)
	result["status"] = "completed"
	result["output"] = output
	result["completed_at"] = now
	if usage, ok := chat["usage"].(map[string]any); ok {
		result["usage"] = toResponsesUsage(usage)
	}
	return json.Marshal(result)
}

// buildResponsesEnvelope 构造 Responses 响应骨架。
func buildResponsesEnvelope(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"completed_at":         nil,
		"status":               "in_progress",
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                true,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"metadata":             map[string]any{},
	}
}

// toResponsesUsage 将 chat usage 转换为 Responses usage 结构。
func toResponsesUsage(u map[string]any) map[string]any {
	in := numOr0(u["prompt_tokens"])
	out := numOr0(u["completion_tokens"])
	total := numOr0(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	cached, reasoningTokens := 0.0, 0.0
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = numOr0(d["cached_tokens"])
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens = numOr0(d["reasoning_tokens"])
	}
	return map[string]any{
		"input_tokens":          int64(in),
		"input_tokens_details":  map[string]any{"cached_tokens": int64(cached)},
		"output_tokens":         int64(out),
		"output_tokens_details": map[string]any{"reasoning_tokens": int64(reasoningTokens)},
		"total_tokens":          int64(total),
	}
}

func numOr0(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func ifEmptyStr(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

// noteFinishReason 提取 chunk 里的 finish_reason（保留首个非空值）。
func noteFinishReason(current string, chunk map[string]any) string {
	if chunk == nil {
		return current
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := choice["finish_reason"].(string); ok && v != "" {
			return v
		}
	}
	return current
}

// stripDataPrefix 剥掉 SSE 行的 "data:" 前缀（可重复）。
func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

// responsesStreamTranslate 把 panel 已输出的 chat SSE 实时转译为 Responses 事件流。
// 上游错误/中断路径由调用方处理；本函数只处理 200 SSE 的语义转译。
func responsesStreamTranslate(w http.ResponseWriter, src io.Reader, modelName string) (usage map[string]any, finishReason string, sawDone bool, scanErr error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		scanErr = fmt.Errorf("streaming unsupported")
		return
	}

	seq := 0
	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		data["sequence_number"] = seq
		seq++
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
		flusher.Flush()
	}

	created := time.Now().Unix()
	respID := "resp_" + compactUUID()
	envelope := buildResponsesEnvelope(respID, modelName, created)
	emit("response.created", map[string]any{"response": envelope})
	emit("response.in_progress", map[string]any{"response": envelope})

	nextIndex := 0
	idxToItem := map[int]map[string]any{}

	reasoningOpen, reasoningIndex := false, -1
	reasoningItemID := ""
	var reasoningSB strings.Builder

	msgOpen, msgIndex := false, -1
	msgItemID := ""
	var msgSB strings.Builder

	toolCalls := map[int]*mergedToolCall{}
	var toolOrder []int
	tcItemID := map[int]string{}
	tcIndex := map[int]int{}

	openReasoning := func() {
		if reasoningOpen {
			return
		}
		reasoningOpen = true
		reasoningIndex = nextIndex
		nextIndex++
		reasoningItemID = "rs_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": reasoningIndex,
			"item":         map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		})
		emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	openMessage := func() {
		if msgOpen {
			return
		}
		msgOpen = true
		msgIndex = nextIndex
		nextIndex++
		msgItemID = "msg_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": msgIndex,
			"item":         map[string]any{"id": msgItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		})
		emit("response.content_part.added", map[string]any{
			"item_id": msgItemID, "output_index": msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "annotations": []any{}, "text": ""},
		})
	}

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		finishReason = noteFinishReason(finishReason, chunk)
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				openReasoning()
				reasoningSB.WriteString(rc)
				emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "delta": rc,
				})
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				openMessage()
				msgSB.WriteString(ct)
				emit("response.output_text.delta", map[string]any{
					"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "delta": ct,
				})
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				type argDelta struct {
					idx int
					arg string
				}
				var argDeltas []argDelta
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := tc["index"].(float64); ok {
						idx = int(v)
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if a, ok := fn["arguments"].(string); ok && a != "" {
							argDeltas = append(argDeltas, argDelta{idx: idx, arg: a})
						}
					}
				}
				before := len(toolOrder)
				applyToolCallDelta(toolCalls, &toolOrder, tcs)
				for i := before; i < len(toolOrder); i++ {
					idx := toolOrder[i]
					st := toolCalls[idx]
					if st.ID == "" {
						st.ID = "call_" + compactUUID()
					}
					tcItemID[idx] = "fc_" + compactUUID()
					tcIndex[idx] = nextIndex
					nextIndex++
					emit("response.output_item.added", map[string]any{
						"output_index": tcIndex[idx],
						"item": map[string]any{
							"id": tcItemID[idx], "type": "function_call", "status": "in_progress",
							"call_id": st.ID, "name": st.Name, "arguments": "",
						},
					})
				}
				for _, ad := range argDeltas {
					emit("response.function_call_arguments.delta", map[string]any{
						"item_id": tcItemID[ad.idx], "output_index": tcIndex[ad.idx], "delta": ad.arg,
					})
				}
			}
		}
	}
	scanErr = scanner.Err()
	if scanErr != nil {
		failed := buildResponsesEnvelope(respID, modelName, created)
		failed["status"] = "failed"
		failed["error"] = map[string]any{"code": "stream_interrupted", "message": "上游流式响应中断，本次回复不完整"}
		emit("response.failed", map[string]any{"response": failed})
		return
	}
	if finishReason == "" {
		// 干净 EOF 但无 finish_reason：拒绝伪造 response.completed。
		failed := buildResponsesEnvelope(respID, modelName, created)
		failed["status"] = "failed"
		failed["error"] = map[string]any{
			"code":    "stream_closed_without_finish",
			"message": "上游流正常结束，但没有 finish_reason，不能当成完整回复",
		}
		emit("response.failed", map[string]any{"response": failed})
		return
	}

	if !reasoningOpen && !msgOpen && len(toolOrder) == 0 {
		openMessage()
	}

	if reasoningOpen {
		text := reasoningSB.String()
		emit("response.reasoning_summary_text.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "text": text})
		emit("response.reasoning_summary_part.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
		item := map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
		emit("response.output_item.done", map[string]any{"output_index": reasoningIndex, "item": item})
		idxToItem[reasoningIndex] = item
	}
	if msgOpen {
		text := msgSB.String()
		emit("response.output_text.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "text": text})
		emit("response.content_part.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "annotations": []any{}, "text": text}})
		item := map[string]any{"id": msgItemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "text": text}}}
		emit("response.output_item.done", map[string]any{"output_index": msgIndex, "item": item})
		idxToItem[msgIndex] = item
	}
	for _, idx := range toolOrder {
		st := toolCalls[idx]
		args := st.Args.String()
		emit("response.function_call_arguments.done", map[string]any{"item_id": tcItemID[idx], "output_index": tcIndex[idx], "name": st.Name, "arguments": args})
		item := map[string]any{"id": tcItemID[idx], "type": "function_call", "status": "completed", "call_id": st.ID, "name": st.Name, "arguments": args}
		emit("response.output_item.done", map[string]any{"output_index": tcIndex[idx], "item": item})
		idxToItem[tcIndex[idx]] = item
	}

	output := make([]any, 0, len(idxToItem))
	for i := 0; i < nextIndex; i++ {
		if item, ok := idxToItem[i]; ok {
			output = append(output, item)
		}
	}

	final := buildResponsesEnvelope(respID, modelName, created)
	final["status"] = "completed"
	final["output"] = output
	final["completed_at"] = time.Now().Unix()
	if usage != nil {
		final["usage"] = toResponsesUsage(usage)
	}
	emit("response.completed", map[string]any{"response": final})

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	return
}

// responsesInternalError 按 Responses 形状返回错误。
func responsesInternalError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// compactUUID 去连字符的随机 UUID。
func compactUUID() string {
	var buf [16]byte
	if _, err := crandRead(buf[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hexEncode(buf)
}

// responsesRoundTrip 把 Responses 请求转成 chat 后**进程内**调用 chatCompletions，
// 完整复用轮转/粘性/成本学习/错误分类管线；然后把 chat 响应转回 Responses 语义。
func (h *Handler) responsesRoundTrip(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		responsesInternalError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	var respReq map[string]any
	if err := json.Unmarshal(bodyBytes, &respReq); err != nil {
		responsesInternalError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}
	modelName, _ := respReq["model"].(string)
	if modelName == "" {
		responsesInternalError(w, http.StatusBadRequest, "missing_model", "Responses 请求必须提供 model")
		return
	}
	isStream, _ := respReq["stream"].(bool)

	chatReq, err := responsesToChatRequest(respReq, modelName)
	if err != nil {
		responsesInternalError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq["stream"] = true // panel 上游强制流式（非流式由网关聚合）
	chatBytes, err := json.Marshal(chatReq)
	if err != nil {
		responsesInternalError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}

	// 进程内回环调用 chatCompletions：借 recorder 捕获响应（状态码 + body 流）。
	rec := newCaptureRecorder(w)
	innerReq := r.Clone(r.Context())
	innerReq.Method = http.MethodPost
	innerReq.Body = io.NopCloser(bytes.NewReader(chatBytes))
	innerReq.ContentLength = int64(len(chatBytes))
	innerReq.Header = r.Header.Clone()
	innerReq.Header.Set("Content-Type", "application/json")
	// Responses 的 stream=true/false 语义在 chat 层恒为 true（chatCompletions 从
	// body 读 stream），由本函数负责最终形态转换。
	h.chatCompletions(rec, innerReq)

	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status >= 400 {
		// chat 层错误原样转 Responses 错误信封（保留上游原文）。
		responsesInternalError(w, rec.status, "upstream_error", rec.buf.String())
		return
	}

	if isStream {
		// chat SSE → Responses 事件流（边收边转）。
		usageMap, finishReason, _, scanErr := responsesStreamTranslate(w, bytes.NewReader(rec.bodyBytes()), modelName)
		_ = usageMap
		if scanErr == nil && finishReason == "" {
			// 已在 translate 内发 response.failed；无需重复。
			return
		}
		return
	}

	// 非流式：聚合的 chat.completion → Responses JSON。
	// chatCompletions 恒以流式打上游；此处把其 SSE 输出聚合为 chat.completion。
	agg, aerr := upstreamAggregateFromSSE(rec.bodyBytes())
	if aerr != nil {
		responsesInternalError(w, http.StatusBadGateway, "aggregate_error", aerr.Error())
		return
	}
	out, terr := chatCompletionToResponses(agg, modelName)
	if terr != nil {
		responsesInternalError(w, http.StatusInternalServerError, "translate_error", "转换 Responses 响应失败: "+terr.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// assistantMessageHasPayload 判断消息 content 是否有实际负载（本地副本，
// upstream 包的版本未导出）。
func assistantMessageHasPayload(message map[string]any) bool {
	content, exists := message["content"]
	if !exists || content == nil {
		return false
	}
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	default:
		return true
	}
}
