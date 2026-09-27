// Package server: responses_helpers.go — Responses 端点的支撑工具。
package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// crandRead crypto/rand.Read 的包内别名（便于测试注入）。
func crandRead(b []byte) (int, error) { return rand.Read(b) }

// hexEncode 16 字节转 32 位 hex 字符串。
func hexEncode(b [16]byte) string { return hex.EncodeToString(b[:]) }

// captureRecorder 捕获内部 HTTP 调用响应的 ResponseWriter（进程内回环用）。
type captureRecorder struct {
	header     http.Header
	buf        bytes.Buffer
	status     int
	underlying http.ResponseWriter // 非空时透传 Write/Flush（流式场景）
}

// bodyBytes 捕获的响应字节。
func (c *captureRecorder) bodyBytes() []byte { return c.buf.Bytes() }

func newCaptureRecorder(underlying http.ResponseWriter) *captureRecorder {
	return &captureRecorder{header: http.Header{}, underlying: underlying}
}

func (c *captureRecorder) Header() http.Header { return c.header }

func (c *captureRecorder) Write(b []byte) (int, error) {
	c.buf.Write(b)
	if c.underlying != nil {
		_, _ = c.underlying.Write(b)
	}
	return len(b), nil
}

func (c *captureRecorder) WriteHeader(statusCode int) {
	if c.status == 0 {
		c.status = statusCode
	}
	if c.underlying != nil {
		c.underlying.WriteHeader(statusCode)
	}
}

// Flush 实现 http.Flusher（SSE 转译路径需要；透传到底层 writer）。
func (c *captureRecorder) Flush() {
	if f, ok := c.underlying.(http.Flusher); ok {
		f.Flush()
	}
}

// upstreamAggregateFromSSE 把 chat SSE 帧序列聚合为 chat.completion JSON。
// 复用 panel 上游 sse.Aggregate 的信封解析口径（data: 前缀剥离 + [DONE] 终止）。
// panel 的 Aggregate 吃 io.Reader 逐帧解析；此处把已缓存的 SSE 文本喂给它。
func upstreamAggregateFromSSE(sse []byte) ([]byte, error) {
	return aggregateSSEBytes(sse)
}

// aggregateSSEBytes 独立聚合实现（与 upstream.Aggregate 同口径但吃 []byte）：
// 提取 choices[0].delta 拼接 content/reasoning_content/tool_calls，末帧 usage 透传。
func aggregateSSEBytes(sse []byte) ([]byte, error) {
	type toolState struct {
		ID   string
		Name string
		Args strings.Builder
	}
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	tools := map[int]*toolState{}
	var order []int

	scanner := bufio.NewScanner(strings.NewReader(string(sse)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		for strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if jsonUnmarshalInto(line, &chunk) != nil {
			continue
		}
		if m, ok := chunk["model"].(string); ok && m != "" {
			respModel = m
		}
		if id, ok := chunk["id"].(string); ok && id != "" {
			respID = id
		}
		if v, ok := chunk["created"].(float64); ok && v > 0 {
			created = int64(v)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" && finish == "" {
				finish = fr
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if r, ok := delta["role"].(string); ok && r != "" && role == "" {
				role = r
			}
			if rc, ok := delta["reasoning_content"].(string); ok {
				reasoning += rc
			}
			if ct, ok := delta["content"].(string); ok {
				content += ct
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := tc["index"].(float64); ok {
						idx = int(v)
					}
					st, exists := tools[idx]
					if !exists {
						st = &toolState{}
						tools[idx] = st
						order = append(order, idx)
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						st.ID = id
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
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if finish == "" {
		return nil, errStreamClosedWithoutFinish
	}

	var toolCalls []any
	for _, idx := range order {
		st := tools[idx]
		toolCalls = append(toolCalls, map[string]any{
			"id":   st.ID,
			"type": "function",
			"function": map[string]any{
				"name":      st.Name,
				"arguments": st.Args.String(),
			},
		})
	}
	if role == "" {
		role = "assistant"
	}
	msg := map[string]any{"role": role, "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if respID == "" {
		respID = "chatcmpl-" + compactUUID()
	}
	if respModel == "" {
		respModel = "unknown"
	}
	if created == 0 {
		created = timeNowUnix()
	}
	out := map[string]any{
		"id":      respID,
		"object":  "chat.completion",
		"created": created,
		"model":   respModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return jsonMarshal(out)
}
