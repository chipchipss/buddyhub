package trae

// stream.go 上游 SOLO SSE → OpenAI chunk 的转换。
//
// 这条流上有三件事必须先想清楚再写代码：
//
//  1. **上游不发 `[DONE]`**：结束帧是 `event: done`。`[DONE]` 由这里补；
//     上游中途断掉（没有 done）也要补，否则客户端永远等在最后一片上。
//  2. **业务错误藏在 200 的流里**（`event: error`）：它不是 chunk，
//     而是一条 `event: error` + 结束。
//  3. **`token_usage` 是"欠着"的**：读到它并不立刻发帧，而是挂在下一帧
//     （正常就是那条 finish 帧）上一起走。所以"usage 之后流就断了"这种上游
//     行为会让 usage 静默消失——照抄行为，不偷偷改。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SoloEvent 一条已解析的上游事件。
type SoloEvent struct {
	Kind     string // output / token_usage / done / error / other
	Response string
	Reason   string
	Tools    json.RawMessage
	Usage    json.RawMessage
	Finish   string
	Code     string
	Message  string
}

// ParseSoloEvent 解析一条 `event:` + `data:`。
//
// data 为空时只有 `done` 有意义（上游会发一条不带 data 的 done）。
func ParseSoloEvent(event, data string) *SoloEvent {
	if strings.TrimSpace(data) == "" {
		if event == "done" {
			return &SoloEvent{Kind: "done"}
		}
		return &SoloEvent{Kind: "other"}
	}
	var doc map[string]any
	if json.Unmarshal([]byte(data), &doc) != nil {
		return &SoloEvent{Kind: "other"}
	}
	switch event {
	case "output":
		ev := &SoloEvent{Kind: "output"}
		ev.Response, _ = doc["response"].(string)
		ev.Reason, _ = doc["reasoning_content"].(string)
		if tc, ok := doc["tool_calls"]; ok && tc != nil {
			if raw, err := json.Marshal(tc); err == nil {
				ev.Tools = raw
			}
		}
		return ev
	case "token_usage":
		raw, _ := json.Marshal(doc)
		return &SoloEvent{Kind: "token_usage", Usage: raw}
	case "done":
		ev := &SoloEvent{Kind: "done"}
		ev.Finish, _ = doc["finish_reason"].(string)
		return ev
	case "error":
		ev := &SoloEvent{Kind: "error"}
		ev.Code, ev.Message = errorEventFields(doc)
		return ev
	default:
		return &SoloEvent{Kind: "other"}
	}
}

// errorEventFields 从 error 帧里取 code / message（字段名有多种写法）。
func errorEventFields(doc map[string]any) (string, string) {
	pickStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := doc[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	msg := pickStr("message", "msg", "error_message")
	if msg == "" {
		if e, ok := doc["error"].(map[string]any); ok {
			msg = pickStr2(e, "message", "msg")
		} else if e, ok := doc["error"].(string); ok {
			msg = e
		}
	}
	code := pickStr("code", "error_code")
	if code == "" {
		if e, ok := doc["error"].(map[string]any); ok {
			code = pickStr2(e, "code", "error_code")
		}
	}
	if code == "" {
		code = "upstream_error"
	}
	if msg == "" {
		msg = "上游返回错误"
	}
	return code, msg
}

func pickStr2(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// SoloTranslator 把上游事件流转成 OpenAI SSE。
type SoloTranslator struct {
	ID      string
	Created int64
	Model   string
	// pendingUsage token_usage 是"欠着"的，挂到下一帧一起走
	pendingUsage json.RawMessage
	sawDone      bool
}

// NewSoloTranslator 建一个转换器（id/created 每次请求不同）。
func NewSoloTranslator(id string, created int64, model string) *SoloTranslator {
	return &SoloTranslator{ID: id, Created: created, Model: model}
}

// Translate 处理一条上游事件，返回要发给客户端的 SSE 帧（可能为空）。
func (t *SoloTranslator) Translate(ev *SoloEvent) [][]byte {
	switch ev.Kind {
	case "output":
		delta := map[string]any{}
		if ev.Response != "" {
			delta["content"] = ev.Response
		}
		if ev.Reason != "" {
			delta["reasoning_content"] = ev.Reason
		}
		if len(ev.Tools) > 0 {
			delta["tool_calls"] = normalizeStreamToolCalls(ev.Tools)
		}
		if len(delta) == 0 {
			return nil
		}
		return [][]byte{t.chunk(delta, nil)}

	case "token_usage":
		t.pendingUsage = ev.Usage
		return nil

	case "done":
		t.sawDone = true
		return [][]byte{t.chunk(map[string]any{}, strPtr(ev.Finish)), doneFrame()}

	case "error":
		t.sawDone = true
		return [][]byte{t.errorFrame(ev.Code, ev.Message), doneFrame()}
	}
	return nil
}

// Finish 流结束时收尾：上游没发 done 也要补 `[DONE]`，
// 否则客户端永远等在最后一片上。
func (t *SoloTranslator) Finish() [][]byte {
	if t.sawDone {
		return nil
	}
	t.sawDone = true
	return [][]byte{t.chunk(map[string]any{}, strPtr("stop")), doneFrame()}
}

func (t *SoloTranslator) chunk(delta map[string]any, finish *string) []byte {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	} else {
		choice["finish_reason"] = nil
	}
	payload := map[string]any{
		"id": t.ID, "object": "chat.completion.chunk", "created": t.Created,
		"model": t.Model, "choices": []any{choice},
	}
	// usage 欠在 finish 帧上一起走（上游就是这行为）
	if finish != nil && len(t.pendingUsage) > 0 {
		var usage any
		if json.Unmarshal(t.pendingUsage, &usage) == nil {
			payload["usage"] = usage
		}
		t.pendingUsage = nil
	}
	raw, _ := json.Marshal(payload)
	return append([]byte("data: "), append(raw, '\n', '\n')...)
}

func (t *SoloTranslator) errorFrame(code, message string) []byte {
	payload := map[string]any{"error": map[string]any{"message": message, "type": code}}
	raw, _ := json.Marshal(payload)
	return append([]byte("data: "), append(raw, '\n', '\n')...)
}

func doneFrame() []byte { return []byte("data: [DONE]\n\n") }

func strPtr(s string) *string {
	if s == "" {
		s = "stop"
	}
	return &s
}

// normalizeStreamToolCalls 上游流式 tool_calls 形态归一到 OpenAI 的
// `[{index, id, type, function:{name, arguments}}]`。
func normalizeStreamToolCalls(raw json.RawMessage) any {
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		items = []map[string]any{one}
	}
	out := make([]any, 0, len(items))
	for i, it := range items {
		// 上游可能已经带了 function_call 形态
		fn, _ := it["function"].(map[string]any)
		if fn == nil {
			fn, _ = it["function_call"].(map[string]any)
		}
		if fn == nil {
			fn = map[string]any{}
		}
		idx := i
		if v, ok := it["index"].(float64); ok {
			idx = int(v)
		}
		entry := map[string]any{"index": idx, "type": "function", "function": fn}
		if id, ok := it["id"].(string); ok && id != "" {
			entry["id"] = id
		}
		out = append(out, entry)
	}
	return out
}

// ScanSoloSSE 逐帧扫描上游 SSE，把 (event, data) 交给回调。
//
// 返回 io.EOF 表示流正常结束（不代表有 done 帧——调用方要靠 Finish() 兜底）。
func ScanSoloSSE(r io.Reader, fn func(event, data string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event, data string
	flush := func() {
		if event != "" || data != "" {
			fn(event, data)
		}
		event, data = "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			if data != "" {
				data += "\n"
			}
			data += chunk
		}
	}
	flush()
	return sc.Err()
}

// AggregateSolo 把上游事件流聚合成一个非流式 chat.completion。
func AggregateSolo(r io.Reader, id string, created int64, model string) ([]byte, error) {
	var content, reasoning strings.Builder
	var toolCalls []any
	var usage json.RawMessage
	var finish string
	var upstreamErr *SoloEvent

	_ = ScanSoloSSE(r, func(event, data string) {
		ev := ParseSoloEvent(event, data)
		switch ev.Kind {
		case "output":
			content.WriteString(ev.Response)
			reasoning.WriteString(ev.Reason)
			if len(ev.Tools) > 0 {
				if calls, ok := normalizeStreamToolCalls(ev.Tools).([]any); ok {
					toolCalls = append(toolCalls, calls...)
				}
			}
		case "token_usage":
			usage = ev.Usage
		case "done":
			finish = ev.Finish
		case "error":
			upstreamErr = ev
		}
	})
	if upstreamErr != nil {
		return nil, fmt.Errorf("%s", upstreamErr.Message)
	}
	if finish == "" {
		finish = "stop"
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	payload := map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if len(usage) > 0 {
		var u any
		if json.Unmarshal(usage, &u) == nil {
			payload["usage"] = u
		}
	}
	return json.Marshal(payload)
}
