package accio

// protocol.go OpenAI Chat Completions ↔ 阿里 ADK（Gemini 风格）信封。
//
// 下游（OpenAI，网关内部统一形态）：
//
//	{ "model": "…", "messages": [{role, content, tool_calls, tool_call_id}],
//	  "tools": [{"type":"function","function":{name, description, parameters}}],
//	  "temperature": 0.7, "max_tokens": 1024, "stream": true }
//
// 上游（`POST {llmBase}/generateContent`，**protobuf-JSON 的 snake_case** 形态）：
//
//	{ "model": "gemini-3-flash-preview", "tenant": "accio-agent",
//	  "iai_tag": "phoenix-desktop", "request_id": "…", "token": "<accessToken>",
//	  "contents": [ {"role":"user","parts":[{"text":"hi"}]},
//	                {"role":"model","parts":[{"function_call":{"id","name","args_json"}}]},
//	                {"role":"tool","parts":[{"function_response":{"id","name","response_json"}}]} ],
//	  "system_instruction": "字符串，不是对象",
//	  "tools": [{"name":"f","description":"…","parameters_json":"{…}"}],
//	  "temperature": 0.7, "max_output_tokens": 1024,
//	  "tool_config": "{\"functionCallingConfig\":{…}}",   ← 也是字符串
//	  "include_thoughts": true, "reasoning_effort": "high",
//	  "properties": {"normalized_response":"true","reasoning_effort":"high"} }
//
// 三处「照 OpenAI 直发必失败」的形态差异：
//
//	① `tools[].parameters_json` 与 `tool_config` 都是 **JSON 字符串**（不是对象）
//	② assistant 的 tool_calls 要拆成 `parts[].function_call`，`args_json` 是字符串
//	③ tool 结果要包成 `parts[].function_response`，`response_json` 是字符串

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultModel 模型名解析不出来时的兜底。
const DefaultModel = "gemini-3-flash-preview"

// GenerateContentURL 拼上游对话地址。
//
// `sg_k` 是 **request_id 的 MD5**（上游的请求签名参数，不是可选的）。
func GenerateContentURL(llmBase, requestID string) string {
	sum := md5.Sum([]byte(requestID))
	return llmBase + "/generateContent?sg_k=" + hex.EncodeToString(sum[:])
}

// BuildUpstreamBody 把 OpenAI 请求体翻成 ADK 信封。
//
// resolvedModel 非空时覆盖请求体里的 model。
func BuildUpstreamBody(source []byte, resolvedModel, token, requestID, deviceID string) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(source, &in); err != nil {
		return nil, err
	}

	model := strings.TrimSpace(resolvedModel)
	if model == "" {
		model, _ = in["model"].(string)
	}
	model = sanitizeModel(model)
	if model == "" {
		model = DefaultModel
	}

	contents, systemInstruction := buildContents(in["messages"])

	out := map[string]any{
		"model":      model,
		"tenant":     Tenant,
		"iai_tag":    IaiTag,
		"empid":      "",
		"request_id": requestID,
		"token":      token,
		"contents":   contents,
		// 上游要字符串形式的 tool_config
		"tool_config": `{"functionCallingConfig":{"streamFunctionCallArguments":true}}`,
		"properties": map[string]any{
			"normalized_response": "true",
		},
	}
	if systemInstruction != "" {
		out["system_instruction"] = systemInstruction
	}
	if tools := buildTools(in["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if v, ok := in["temperature"].(float64); ok {
		out["temperature"] = v
	}
	if v, ok := in["max_tokens"].(float64); ok {
		out["max_output_tokens"] = v
	} else if v, ok := in["max_completion_tokens"].(float64); ok {
		out["max_output_tokens"] = v
	}
	if v, ok := in["top_p"].(float64); ok {
		out["top_p"] = v
	}
	if effort, ok := in["reasoning_effort"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(effort)) {
		case "", "auto", "none", "off":
		default:
			out["reasoning_effort"] = effort
			out["include_thoughts"] = true
			out["properties"] = map[string]any{
				"normalized_response": "true",
				"reasoning_effort":    effort,
			}
		}
	}
	if deviceID != "" {
		out["device_id"] = deviceID
	}
	return json.Marshal(out)
}

// sanitizeModel 去掉网关侧展示后缀。
func sanitizeModel(m string) string {
	m = strings.TrimSpace(m)
	for _, suf := range []string{"-global", "-cn"} {
		m = strings.TrimSuffix(m, suf)
	}
	return strings.TrimSpace(m)
}

// buildContents OpenAI messages → ADK contents + system_instruction。
func buildContents(raw any) ([]any, string) {
	msgs, ok := raw.([]any)
	if !ok {
		return []any{}, ""
	}
	var contents []any
	var systemParts []string
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			// ADK 的 system_instruction 是**一个字符串**，多条 system 拼起来
			if s := contentText(msg["content"]); s != "" {
				systemParts = append(systemParts, s)
			}
			continue
		case "assistant":
			parts := contentToParts(msg["content"])
			if calls := assistantToolCallParts(msg["tool_calls"]); len(calls) > 0 {
				parts = append(parts, calls...)
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		case "tool":
			contents = append(contents, map[string]any{
				"role":  "tool",
				"parts": []any{toolResultPart(msg)},
			})
		default: // user 及其它
			parts := contentToParts(msg["content"])
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "user", "parts": parts})
		}
	}
	return contents, strings.Join(systemParts, "\n\n")
}

// contentText 取纯文本（字符串或块数组）。
func contentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			} else if s, ok := item.(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// contentToParts 内容 → ADK parts（文本 + 图片）。
func contentToParts(v any) []any {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []any{map[string]any{"text": t}}
	case []any:
		var parts []any
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok && s != "" {
					parts = append(parts, map[string]any{"text": s})
				}
				continue
			}
			switch m["type"] {
			case "text":
				if s, ok := m["text"].(string); ok && s != "" {
					parts = append(parts, map[string]any{"text": s})
				}
			case "image_url":
				if p := imagePart(m["image_url"]); p != nil {
					parts = append(parts, p)
				}
			}
		}
		return parts
	}
	return nil
}

// imagePart OpenAI image_url → ADK 内联图片。
func imagePart(v any) map[string]any {
	url := ""
	switch t := v.(type) {
	case string:
		url = t
	case map[string]any:
		url, _ = t["url"].(string)
	}
	if url == "" {
		return nil
	}
	// data URI：拆出 mime 与 base64
	if strings.HasPrefix(url, "data:") {
		if i := strings.Index(url, ";base64,"); i > 0 {
			mime := strings.TrimPrefix(url[:i], "data:")
			return map[string]any{
				"inline_data": map[string]any{"mime_type": mime, "data": url[i+len(";base64,"):]},
			}
		}
	}
	return map[string]any{
		"file_data": map[string]any{"mime_type": "", "file_uri": url},
	}
}

// assistantToolCallParts assistant 的 tool_calls → ADK function_call parts。
//
// `args_json` 是**字符串**（不是对象）——上游 protobuf-JSON 的形态。
func assistantToolCallParts(v any) []any {
	calls, ok := v.([]any)
	if !ok {
		return nil
	}
	var parts []any
	for _, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		args := "{}"
		switch a := fn["arguments"].(type) {
		case string:
			if strings.TrimSpace(a) != "" {
				args = a
			}
		case map[string]any:
			if raw, err := json.Marshal(a); err == nil {
				args = string(raw)
			}
		}
		id, _ := call["id"].(string)
		parts = append(parts, map[string]any{
			"function_call": map[string]any{"id": id, "name": name, "args_json": args},
		})
	}
	return parts
}

// toolResultPart tool 结果 → ADK function_response part。
//
// `response_json` 是字符串，形如 `{"content":"…","is_error":false}`。
func toolResultPart(msg map[string]any) map[string]any {
	id, _ := msg["tool_call_id"].(string)
	if id == "" {
		id, _ = msg["tool_call_id_alt"].(string)
	}
	name, _ := msg["name"].(string)
	text := contentText(msg["content"])
	payload, _ := json.Marshal(map[string]any{"content": text, "is_error": false})
	return map[string]any{
		"function_response": map[string]any{
			"id": id, "name": name, "response_json": string(payload),
		},
	}
}

// buildTools OpenAI tools → ADK tools。
//
// `parameters_json` 是**字符串**（不是对象）。
func buildTools(v any) []any {
	tools, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			// 可能已经是 ADK 形态
			fn = tool
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		params := "{}"
		switch p := fn["parameters"].(type) {
		case string:
			if strings.TrimSpace(p) != "" {
				params = p
			}
		default:
			if p != nil {
				if raw, err := json.Marshal(p); err == nil {
					params = string(raw)
				}
			}
		}
		desc, _ := fn["description"].(string)
		out = append(out, map[string]any{
			"name": name, "description": desc, "parameters_json": params,
		})
	}
	return out
}

/* ── 响应侧：ADK 帧 → OpenAI chunk ──────────────────────────── */

// Part 一帧里的一个内容片段。
type Part struct {
	Kind     string // text / thought / function_call / function_response
	Text     string
	CallID   string
	CallName string
	ArgsJSON string
	RespJSON string
}

// Frame 一条已解析的上游帧。
type Frame struct {
	Parts        []Part
	TurnComplete bool
	FinishReason string
	ErrorCode    string
	ErrorMsg     string
	Usage        json.RawMessage
}

// ParseFrame 解析一条 SSE data（空/`[DONE]` 返回 nil）。
func ParseFrame(data string) *Frame {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || strings.EqualFold(trimmed, "[DONE]") {
		return nil
	}
	var v map[string]any
	if json.Unmarshal([]byte(trimmed), &v) != nil {
		return nil
	}
	f := &Frame{
		TurnComplete: boolField(v, "turnComplete", "turn_complete"),
		FinishReason: textField(v, "finishReason", "finish_reason"),
		ErrorCode:    textField(v, "errorCode", "error_code"),
		ErrorMsg:     textField(v, "errorMessage", "error_message"),
	}
	if u := fieldOf(v, "usageMetadata", "usage_metadata"); u != nil {
		if raw, err := json.Marshal(u); err == nil {
			f.Usage = raw
		}
	}
	if content, ok := v["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					f.Parts = append(f.Parts, parsePart(pm))
				}
			}
		}
	}
	return f
}

func parsePart(p map[string]any) Part {
	if fc, ok := p["function_call"].(map[string]any); ok {
		return Part{
			Kind:     "function_call",
			CallID:   textField(fc, "id", "id"),
			CallName: textField(fc, "name", "name"),
			ArgsJSON: textField(fc, "argsJson", "args_json"),
		}
	}
	if fr, ok := p["function_response"].(map[string]any); ok {
		return Part{
			Kind:     "function_response",
			CallID:   textField(fr, "id", "id"),
			CallName: textField(fr, "name", "name"),
			RespJSON: textField(fr, "responseJson", "response_json"),
		}
	}
	if t, ok := p["text"].(string); ok {
		// 思维链片段带 thought 标记
		if boolField(p, "thought", "thought") {
			return Part{Kind: "thought", Text: t}
		}
		return Part{Kind: "text", Text: t}
	}
	return Part{Kind: "other"}
}

func fieldOf(m map[string]any, camel, snake string) any {
	if v, ok := m[camel]; ok {
		return v
	}
	return m[snake]
}

func textField(m map[string]any, camel, snake string) string {
	if v := fieldOf(m, camel, snake); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, camel, snake string) bool {
	if v := fieldOf(m, camel, snake); v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// FinishReason 上游 finishReason → OpenAI finish_reason。
func FinishReason(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "STOP", "FINISH_REASON_STOP", "":
		return "stop"
	case "MAX_TOKENS", "FINISH_REASON_MAX_TOKENS", "LENGTH":
		return "length"
	case "TOOL_CALLS", "FUNCTION_CALL", "FINISH_REASON_TOOL_CALLS":
		return "tool_calls"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

// SseFrame / SseDone 出站帧。
func SseFrame(v any) string {
	raw, _ := json.Marshal(v)
	return "data: " + string(raw) + "\n\n"
}

// SseDone 出站结束帧。
func SseDone() string { return "data: [DONE]\n\n" }

// ErrorMessage 从上游错误帧里取可读文案。
func ErrorMessage(code, msg string) string {
	if msg != "" {
		return msg
	}
	if code != "" {
		return fmt.Sprintf("上游返回错误（%s）", code)
	}
	return "上游返回错误"
}
