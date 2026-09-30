package trae

// payload.go Trae SOLO 出站请求体：**白名单重建**。
//
// 为什么不是"改几个字段"：上游 `llm_utils_chat` 的契约极小，而且**多带字段
// 不是被忽略、是被拒**——带 agent_type / device_id / ide_version 会得到流内
// `4023 model is unknown`，带 thinking / stream_options / response_format 一族
// 会以别的方式炸流。所以这里反过来做：**只把清单里的键搬过去**，其余一概丢弃。
//
// 四处「照 OpenAI 形状直发必定 4001」的 SOLO 专属变形（每处都有测试钉着）：
//
//	① tools[].function.parameters 必须是 **JSON 字符串**（OpenAI 是对象）
//	② tool_choice 必须是**裸字符串**（对象形态要按 type 折算）
//	③ assistant 的 tool_calls[].function 要改名 **function_call**
//	④ developer 角色上游不认（实测静默空流），降成 system

import (
	"encoding/json"
	"strings"
)

// DefaultConfigName 模型名解析不出来时的兜底，而不是发一个空 config_name。
const DefaultConfigName = "glm-5.2"

// defaultMaxTokens 上游会把输出截在 128k；客户端没给就补一个大上限，
// 让长回复不被腰斩（显式给的值原样透传）。
const defaultMaxTokens = 1000000

// soloFunction llm_utils_chat **只认这一个** function 值。
// （参考实现早先给 cn/intl 发 inline_chat，结果是那两类凭据的每个模型都流内 4001。）
const soloFunction = "solo_work_lite"

// samplingKeys 采样类白名单（只有数值才搬）。
var samplingKeys = []string{
	"temperature", "top_p", "max_tokens", "presence_penalty",
	"frequency_penalty", "seed", "n",
}

// PrepareBody 入站 OpenAI 请求体 → SOLO 出站体。
//
// resolvedModel 非空时覆盖请求体里的 model（网关按前缀解析后的裸名）。
func PrepareBody(source []byte, resolvedModel string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(source, &obj); err != nil {
		return nil, err
	}

	normalizeMessages(obj)
	dropOrphanToolResults(obj)
	normalizeToolChoice(obj)
	normalizeTools(obj)

	model := strings.TrimSpace(resolvedModel)
	if model == "" {
		if s, ok := obj["model"].(string); ok {
			model = s
		}
	}
	model = sanitizeModelName(model)
	if model == "" {
		model = DefaultConfigName
	}

	out := map[string]any{
		"function":    soloFunction,
		"stream":      true, // 上游只支持流式；非流式由网关本地聚合
		"config_name": model,
		"model":       model,
	}
	if v, ok := obj["messages"]; ok {
		out["messages"] = v
	}
	if v, ok := obj["tools"]; ok {
		out["tools"] = v
	}
	if v, ok := obj["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	for _, k := range samplingKeys {
		if v, ok := obj[k]; ok {
			if _, isNum := v.(float64); isNum {
				out[k] = v
			}
		}
	}
	if _, ok := out["max_tokens"]; !ok {
		out["max_tokens"] = defaultMaxTokens
	}
	// reasoning_effort：只搬有意义的档位（auto/none/off 等同不传）
	if s, ok := obj["reasoning_effort"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "auto", "none", "off":
		default:
			out["reasoning_effort"] = s
		}
	}
	// stop 只认字符串或数组
	switch obj["stop"].(type) {
	case string, []any:
		out["stop"] = obj["stop"]
	}
	return json.Marshal(out)
}

// sanitizeModelName 去掉网关侧的展示后缀（-solo / -intl），上游不认。
func sanitizeModelName(model string) string {
	name := strings.TrimSpace(model)
	name = strings.TrimSuffix(name, "-solo")
	name = strings.TrimSuffix(name, "-intl")
	return strings.TrimSpace(name)
}

// normalizeMessages 处理三处消息级变形：
//   - developer → system（上游不认 developer，实测静默空流）
//   - assistant 的 tool_calls[].function → function_call，并剔除没有名字的调用
//   - content 字符串 → [{"type":"text","text":…}] 块数组
//
// 被剔空的 assistant 占位消息置为 null（上游对空 assistant 可能是空流而不是报错，
// 空流在网关这边会被判成"成功但没话"，比报错难查得多）。
func normalizeMessages(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, slot := range msgs {
		entry, ok := slot.(map[string]any)
		if !ok {
			continue
		}
		contentPresent := false
		var contentValue any
		if v, ok := entry["content"]; ok {
			contentPresent = true
			contentValue = v
		}
		role, _ := entry["role"].(string)
		if role == "developer" {
			entry["role"] = "system"
			role = "system"
		}

		markNull := false
		if role == "assistant" {
			if calls, ok := entry["tool_calls"].([]any); ok {
				kept := make([]any, 0, len(calls))
				for _, c := range calls {
					call, ok := c.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := call["function"]; ok {
						call["function_call"] = fn
						delete(call, "function")
					}
					// 没有名字的调用上游不认，丢掉
					named := false
					if fc, ok := call["function_call"].(map[string]any); ok {
						if n, ok := fc["name"].(string); ok && strings.TrimSpace(n) != "" {
							named = true
						}
					}
					if named {
						kept = append(kept, call)
					}
				}
				if len(kept) == 0 {
					delete(entry, "tool_calls")
					if !contentPresent || contentValue == nil {
						markNull = true
					}
				} else {
					entry["tool_calls"] = kept
				}
			}
		}
		if text, ok := contentValue.(string); ok {
			entry["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
		if markNull {
			msgs[i] = nil
		}
	}
}

// dropOrphanToolResults 丢掉悬空的 tool 结果（客户端修剪历史后留下的引用）。
//
// 上游对孤儿 tool 结果**可能是空流而不是报错**，所以宁可在这里清掉。
func dropOrphanToolResults(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	known := map[string]bool{}
	out := make([]any, 0, len(msgs))
	for _, slot := range msgs {
		entry, ok := slot.(map[string]any)
		if !ok {
			out = append(out, slot)
			continue
		}
		role, _ := entry["role"].(string)
		if role == "assistant" {
			if calls, ok := entry["tool_calls"].([]any); ok {
				for _, c := range calls {
					if call, ok := c.(map[string]any); ok {
						if id, ok := call["id"].(string); ok && id != "" {
							known[id] = true
						}
					}
				}
			}
			out = append(out, entry)
			continue
		}
		if role == "tool" {
			id, _ := entry["tool_call_id"].(string)
			if id == "" || !known[id] {
				continue // 悬空引用，丢掉
			}
		}
		out = append(out, entry)
	}
	obj["messages"] = out
}

// normalizeToolChoice 把 OpenAI 的对象形态折算成上游要的**裸字符串**。
//
//	{"type":"auto"}                → "auto"
//	{"type":"required"}            → "required"
//	{"type":"function","function":{"name":"x"}} → "x"
//	{"type":"none"} / 其它         → 删掉 tool_choice 且**同时删掉 tools**
func normalizeToolChoice(obj map[string]any) {
	choice, ok := obj["tool_choice"]
	if !ok {
		return
	}
	suppressTools := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	switch v := choice.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppressTools()
		}
	case map[string]any:
		kind, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "none":
			delete(obj, "tool_choice")
			suppressTools()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(kind))
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				obj["tool_choice"] = "auto"
			} else {
				obj["tool_choice"] = name
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools ①：`tools[].function.parameters` 必须是 **JSON 字符串**
// （OpenAI 发的是对象；直发上游必 4001）。
func normalizeTools(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			switch params.(type) {
			case string:
				// 已经是字符串，不动
			default:
				if raw, err := json.Marshal(params); err == nil {
					fn["parameters"] = string(raw)
				}
			}
		}
	}
}
