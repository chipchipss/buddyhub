package accio

// stream.go ADK 帧 → OpenAI chunk 的转换，以及请求入口。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// Chat 发起对话（ADK 信封）。
//
// requestID 必须与 BuildUpstreamBody 写进 body 的那个**是同一个**——
// 它同时出现在 body 与 URL 的 `sg_k` 签名里，对不上上游直接拒。
func Chat(ctx context.Context, cred *Credential, body []byte, requestID string) (*http.Response, error) {
	url := GenerateContentURL(GatewayBase+ADKLLMPath, requestID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", AcceptLang)
	req.Header.Set("x-app-key", AppKey)
	req.Header.Set("x-app-version", AppVersion)
	req.Header.Set("x-client-id", ClientID)
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	return httpClient.Do(req)
}

// Translator ADK 帧 → OpenAI chunk。
type Translator struct {
	ID      string
	Created int64
	Model   string
	// toolIndex 上游的 function_call 不带 index，按出现顺序编号
	toolIndex int
	sawDone   bool
}

// NewTranslator 建转换器。
func NewTranslator(id string, created int64, model string) *Translator {
	return &Translator{ID: id, Created: created, Model: model}
}

// Translate 处理一帧，返回要发出去的 SSE 文本（可能为空）。
func (t *Translator) Translate(f *Frame) string {
	if f == nil {
		return ""
	}
	if f.ErrorCode != "" || f.ErrorMsg != "" {
		t.sawDone = true
		return SseFrame(map[string]any{
			"error": map[string]any{"message": ErrorMessage(f.ErrorCode, f.ErrorMsg), "type": f.ErrorCode},
		}) + SseDone()
	}

	var out strings.Builder
	delta := map[string]any{}
	var reasoning strings.Builder
	var calls []any
	for _, p := range f.Parts {
		switch p.Kind {
		case "text":
			delta["content"] = appendStr(delta["content"], p.Text)
		case "thought":
			reasoning.WriteString(p.Text)
		case "function_call":
			args := p.ArgsJSON
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"index": t.toolIndex, "id": p.CallID, "type": "function",
				"function": map[string]any{"name": p.CallName, "arguments": args},
			})
			t.toolIndex++
		}
	}
	if reasoning.Len() > 0 {
		delta["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		delta["tool_calls"] = calls
	}
	if len(delta) > 0 {
		out.WriteString(SseFrame(t.chunk(delta, nil)))
	}

	if f.TurnComplete || f.FinishReason != "" {
		t.sawDone = true
		finish := FinishReason(f.FinishReason)
		if len(calls) > 0 && f.FinishReason == "" {
			finish = "tool_calls"
		}
		chunk := t.chunk(map[string]any{}, &finish)
		if len(f.Usage) > 0 {
			var u any
			if json.Unmarshal(f.Usage, &u) == nil {
				chunk["usage"] = u
			}
		}
		out.WriteString(SseFrame(chunk))
		out.WriteString(SseDone())
	}
	return out.String()
}

// Finish 流结束时收尾（上游没发 turnComplete 也要补 `[DONE]`）。
func (t *Translator) Finish() string {
	if t.sawDone {
		return ""
	}
	t.sawDone = true
	stop := "stop"
	return SseFrame(t.chunk(map[string]any{}, &stop)) + SseDone()
}

func (t *Translator) chunk(delta map[string]any, finish *string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	} else {
		choice["finish_reason"] = nil
	}
	return map[string]any{
		"id": t.ID, "object": "chat.completion.chunk", "created": t.Created,
		"model": t.Model, "choices": []any{choice},
	}
}

func appendStr(prev any, add string) string {
	s, _ := prev.(string)
	return s + add
}

// ScanSSE 逐条扫描 `data:` 行，交给回调。
func ScanSSE(r io.Reader, fn func(data string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		fn(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return sc.Err()
}

// Aggregate 把 ADK 流聚合成一个非流式 chat.completion。
func Aggregate(r io.Reader, id string, created int64, model string) ([]byte, error) {
	var content, reasoning strings.Builder
	var calls []any
	var usage json.RawMessage
	finish := "stop"
	var upstreamErr string

	_ = ScanSSE(r, func(data string) {
		f := ParseFrame(data)
		if f == nil {
			return
		}
		// 排障：把上游每一帧原文记到调试日志（ACCIO_DEBUG_SSE=1 时）。
		// 上游 200 但正文为空的唯一解释就是帧形状与 ParseFrame 期望不符，
		// 没原始帧就永远只能猜。
		if os.Getenv("ACCIO_DEBUG_SSE") != "" {
			log.Printf("accio-debug: SSE data: %s", data)
		}
		if f.ErrorCode != "" || f.ErrorMsg != "" {
			upstreamErr = ErrorMessage(f.ErrorCode, f.ErrorMsg)
			return
		}
		for _, p := range f.Parts {
			switch p.Kind {
			case "text":
				content.WriteString(p.Text)
			case "thought":
				reasoning.WriteString(p.Text)
			case "function_call":
				args := p.ArgsJSON
				if args == "" {
					args = "{}"
				}
				calls = append(calls, map[string]any{
					"id": p.CallID, "type": "function",
					"function": map[string]any{"name": p.CallName, "arguments": args},
				})
			}
		}
		if len(f.Usage) > 0 {
			usage = f.Usage
		}
		if f.FinishReason != "" {
			finish = FinishReason(f.FinishReason)
		}
		if len(calls) > 0 && f.FinishReason == "" {
			finish = "tool_calls"
		}
	})
	if upstreamErr != "" {
		return nil, fmt.Errorf("%s", upstreamErr)
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
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

// NewRequestID 生成本次请求 id（同时用于 sg_k 签名）。
func NewRequestID() string {
	return randomHex(16)
}
