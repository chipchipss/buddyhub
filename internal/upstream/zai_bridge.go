// Package upstream: zai_bridge.go — Z.AI（智谱 GLM）API Key 通道客户端。
//
// 协议事实（来源：zcode2api app/constants.py + 智谱官方文档 docs.z.ai/devpack）：
//   - GLM Coding Plan 同时支持三种协议；此处走 **Anthropic Messages 兼容端点**
//     https://api.z.ai/api/anthropic/v1/messages（zcode2api 的 zai_fallback/bigmodel
//     同形路径），鉴权为普通 API Key（x-api-key 头），无 session、无验证码、无 OAuth。
//   - 智谱开放平台 bigmodel 同形：https://open.bigmodel.cn/api/anthropic/v1/messages。
//   - 模型名大小写敏感（GLM-5.3-Flash / GLM-5.3）；小写别名由网关映射。
//   - anthropic-version 头：2023-06-01。
//
// 网关侧职责：OpenAI chat 请求 → Anthropic messages 体（翻译），Anthropic 响应
// （JSON 或 SSE）→ OpenAI 形态回传。流式为有状态转换（message_start /
// content_block_delta / message_delta → chat.completion.chunk）。
package upstream

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

// Z.Ai 上游端点。
const (
	ZaiBase        = "https://api.z.ai"
	ZaiMessagesURL = ZaiBase + "/api/anthropic/v1/messages"
	// BigModelBase 智谱开放平台（同形 anthropic 端点；kind="bigmodel"）。
	BigModelBase        = "https://open.bigmodel.cn"
	BigModelMessagesURL = BigModelBase + "/api/anthropic/v1/messages"
)

// AnthropicVersion Anthropic API 版本头（上游实测要求）。
const AnthropicVersion = "2023-06-01"

// ZaiModelMap 客户端小写别名 → 官方大小写敏感模型名。
var ZaiModelMap = map[string]string{
	"glm-5.3-flash": "GLM-5.3-Flash",
	"glm-5.3":       "GLM-5.3",
	"glm-5.2":       "GLM-5.2",
	"glm-5-turbo":   "GLM-5-Turbo",
	"glm-turbo":     "GLM-5-Turbo",
	"glm-5.1":       "GLM-5.1",
	"glm-4.7":       "GLM-4.7",
}

// ZaiMaxTokensLimit 上游 max_tokens 合法上限（超限 400 code 1210）。
const ZaiMaxTokensLimit = 131072

// ZaiTranslateIn OpenAI chat 请求体 → Anthropic messages 请求体。
// 翻译规则照抄 zcode2api openai_compat.openai_to_anthropic（system 提取、
// tool_calls ↔ tool_use/tool_result、stop→stop_sequences、tool_choice 映射）。
func ZaiTranslateIn(openaiBody []byte) ([]byte, string, error) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			// tool_calls 仅 assistant 消息携带
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			// tool_call_id 仅 tool 消息携带
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		MaxTokens   int             `json:"max_tokens"`
		Temperature float64         `json:"temperature"`
		TopP        float64         `json:"top_p"`
		Stop        json.RawMessage `json:"stop"`
		Stream      bool            `json:"stream"`
		Tools       []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(openaiBody, &req); err != nil {
		return nil, "", fmt.Errorf("解析 OpenAI 请求: %w", err)
	}
	model := req.Model
	// 小写别名映射（大小写敏感上游）
	if mapped, ok := ZaiModelMap[strings.ToLower(model)]; ok {
		model = mapped
	}

	var systemParts []string
	type contentBlock map[string]any
	type anthMsg struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	var out []anthMsg

	textOf := func(raw json.RawMessage) string {
		if len(raw) == 0 {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		// content parts: [{"type":"text","text":"..."}]
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					sb.WriteString(p.Text)
				}
			}
			return sb.String()
		}
		return ""
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if t := textOf(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
		case "tool":
			out = append(out, anthMsg{Role: "user", Content: []contentBlock{
				{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": textOf(m.Content)},
			}})
		case "assistant":
			var blocks []contentBlock
			if t := textOf(m.Content); t != "" {
				blocks = append(blocks, contentBlock{"type": "text", "text": t})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]any
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				}
				if args == nil {
					args = map[string]any{}
				}
				blocks = append(blocks, contentBlock{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": args,
				})
			}
			if len(blocks) == 0 {
				blocks = []contentBlock{{"type": "text", "text": ""}}
			}
			out = append(out, anthMsg{Role: "assistant", Content: blocks})
		default: // user 与未知角色一律按 user
			out = append(out, anthMsg{Role: "user", Content: m.Content})
		}
	}

	body := map[string]any{
		"model":      model,
		"messages":   out,
		"max_tokens": clampInt(req.MaxTokens, 1, ZaiMaxTokensLimit),
	}
	if len(systemParts) > 0 {
		body["system"] = strings.Join(systemParts, "\n\n")
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if len(req.Stop) > 0 {
		var one string
		if json.Unmarshal(req.Stop, &one) == nil && one != "" {
			body["stop_sequences"] = []string{one}
		} else {
			var many []string
			if json.Unmarshal(req.Stop, &many) == nil && len(many) > 0 {
				body["stop_sequences"] = many
			}
		}
	}
	if req.Stream {
		body["stream"] = true
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			if t.Function.Name == "" {
				continue
			}
			schema := t.Function.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": schema,
			})
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}
	// tool_choice: "none" 去掉 tools；"required" → {"type":"any"}；{"type":"function"} → named
	if len(req.ToolChoice) > 0 {
		var choice string
		if json.Unmarshal(req.ToolChoice, &choice) == nil {
			switch choice {
			case "none":
				delete(body, "tools")
			case "required":
				body["tool_choice"] = map[string]any{"type": "any"}
			}
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(req.ToolChoice, &obj) == nil && obj.Type == "function" && obj.Function.Name != "" {
				body["tool_choice"] = map[string]any{"type": "tool", "name": obj.Function.Name}
			}
		}
	}
	buf, err := json.Marshal(body)
	return buf, model, err
}

// ZaiPostMessages 发起 messages 请求。返回原始体 + Content-Type + 状态码。
// kind: "zai"（api.z.ai）| "bigmodel"（open.bigmodel.cn）。
func ZaiPostMessages(kind, apiKey string, body []byte) (io.ReadCloser, int, string, error) {
	url := ZaiMessagesURL
	if kind == "bigmodel" {
		url = BigModelMessagesURL
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", AnthropicVersion)
	req.Header.Set("User-Agent", "BuddyHub/1.0")
	resp, err := zaiHTTP().Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, resp.StatusCode, ct, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return resp.Body, 200, ct, nil
}

// ZaiTranslateOutJSON Anthropic 非流式响应 → OpenAI chat.completion。
func ZaiTranslateOutJSON(anthropicBody []byte, fallbackModel string) ([]byte, error) {
	var data struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(anthropicBody, &data); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 响应: %w", err)
	}
	var text strings.Builder
	var toolCalls []map[string]any
	for _, b := range data.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": string(b.Input),
				},
			})
		}
	}
	finish := map[string]string{
		"max_tokens": "length", "tool_use": "tool_calls", "end_turn": "stop",
		"stop_sequence": "stop",
	}[data.StopReason]
	if finish == "" {
		finish = "stop"
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	model := data.Model
	if model == "" {
		model = fallbackModel
	}
	in, out := data.Usage.InputTokens, data.Usage.OutputTokens
	return json.Marshal(map[string]any{
		"id": str_or(data.ID, "chatcmpl-"+randHex24()), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		},
	})
}

// str_or 空串回退。
func str_or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ZaiSSEConverter Anthropic SSE → OpenAI chunk 流的有状态转换器。
// 事件序列：message_start（usage.prompt）→ content_block_start（tool_use 首片）
// → content_block_delta（text_delta / input_json_delta）* →
// content_block_stop → message_delta（stop_reason + usage.output_tokens）
// → message_stop。
type ZaiSSEConverter struct {
	Model    string
	chunkID  string
	finish   string
	inTok    int
	outTok   int
	toolIdx  int
	toolOpen bool
	sawRole  bool
}

// NewZaiSSEConverter 构造。
func NewZaiSSEConverter(model string) *ZaiSSEConverter {
	return &ZaiSSEConverter{Model: model}
}

// Feed 处理一条 Anthropic SSE 事件 JSON，产出 0..n 条 OpenAI chunk JSON 行
// （不含 "data: " 前缀；调用方负责 SSE 封帧与 [DONE]）。
func (c *ZaiSSEConverter) Feed(event []byte) ([][]byte, error) {
	var evt struct {
		Type    string `json:"type"`
		Message struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Index    int `json:"index"`
		ContentB struct {
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
	if err := json.Unmarshal(event, &evt); err != nil {
		return nil, nil // 非 JSON 行静默跳过（SSE 心跳等）
	}
	var out [][]byte
	chunk := func(delta map[string]any, finish any) []byte {
		if !c.sawRole {
			delta["role"] = "assistant"
			c.sawRole = true
		}
		b, _ := json.Marshal(map[string]any{
			"id": str_or(c.chunkID, "chatcmpl-"+randHex24()), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": c.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return b
	}
	switch evt.Type {
	case "message_start":
		if evt.Message.ID != "" {
			c.chunkID = evt.Message.ID
		}
		c.inTok = evt.Message.Usage.InputTokens
	case "content_block_start":
		if evt.ContentB.Type == "tool_use" {
			c.toolOpen = true
			out = append(out, chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": c.toolIdx, "id": evt.ContentB.ID, "type": "function",
				"function": map[string]any{"name": evt.ContentB.Name, "arguments": ""},
			}}}, nil))
		}
	case "content_block_delta":
		switch evt.Delta.Type {
		case "text_delta":
			if evt.Delta.Text != "" {
				out = append(out, chunk(map[string]any{"content": evt.Delta.Text}, nil))
			}
		case "input_json_delta":
			if evt.Delta.PartialJSON != "" {
				out = append(out, chunk(map[string]any{"tool_calls": []map[string]any{{
					"index": c.toolIdx, "type": "function",
					"function": map[string]any{"arguments": evt.Delta.PartialJSON},
				}}}, nil))
			}
		}
	case "content_block_stop":
		if c.toolOpen {
			c.toolOpen = false
			c.toolIdx++
		}
	case "message_delta":
		if evt.Delta.StopReason != "" {
			c.finish = map[string]string{
				"max_tokens": "length", "tool_use": "tool_calls", "end_turn": "stop",
			}[evt.Delta.StopReason]
			if c.finish == "" {
				c.finish = "stop"
			}
		}
		if evt.Usage.OutputTokens > 0 {
			c.outTok = evt.Usage.OutputTokens
		}
		// usage 收尾帧（finish_reason 落定 + usage 值）
		fin := any(c.finish)
		if fin == nil {
			fin = "stop"
		}
		out = append(out, mustJSON(map[string]any{
			"id": str_or(c.chunkID, "chatcmpl-x"), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": c.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": fin}},
			"usage": map[string]any{
				"prompt_tokens": c.inTok, "completion_tokens": c.outTok, "total_tokens": c.inTok + c.outTok,
			},
		}))
	}
	return out, nil
}

// ReadAnthropicSSE 逐事件读取 Anthropic SSE 流（data: 行 → 事件 JSON）。
func ReadAnthropicSSE(r io.Reader, handle func(event []byte)) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return
			}
			handle([]byte(payload))
		}
		if err != nil {
			return
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func randHex24() string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 24)
	for i := range b {
		b[i] = hexd[time.Now().UnixNano()%16]
	}
	return string(b)
}

// zaiHTTP 独立客户端（上游无需 TLS 指纹伪装，标准客户端即可）。
func zaiHTTP() *http.Client { return &http.Client{Timeout: 600 * time.Second} }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
