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

// peekedBody 把嗅探用的 bufio.Reader 挂回响应体：Peek 已经把字节读进缓冲区，
// 不回填就会丢头（风控页前 512 字节正是判据）。
type peekedBody struct {
	*bufio.Reader
	src io.ReadCloser
}

func (p peekedBody) Close() error { return p.src.Close() }

// InterceptReason 判定「HTTP 200 却根本不是模型流」的上游干扰；正常流返回 ""。
//
// 实测上游会把阿里风控校验页（`rgv587_flag:sm` + `punish … action=deny`）以
// text/html、HTTP 200 回给 /api/adk/llm/generateContent。SSE 扫描看不到 `data:`
// 行就静默收尾，客户端拿到「200 + 空正文」，日志里一片清白——账号被风控拦死
// 却查不出原因。这里只嗅不消费（字节会原样回填），让桥接层能在写响应头之前
// 就把真实原因报出来。
func InterceptReason(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	br := bufio.NewReaderSize(resp.Body, 1024)
	resp.Body = peekedBody{Reader: br, src: resp.Body}
	peek, err := br.Peek(512)
	if len(peek) == 0 {
		if err != nil && err != io.EOF {
			return "读取上游响应失败：" + err.Error()
		}
		return "上游 200 但响应体为空，没有收到任何 ADK 帧"
	}
	head := strings.ToLower(strings.TrimSpace(string(peek)))
	// 真流的开头一定是 `data:`；HTML/JSON 信封不是。
	if strings.HasPrefix(head, "data:") {
		return ""
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		return ""
	}
	if strings.HasPrefix(head, "<") || strings.Contains(ct, "html") {
		if strings.Contains(head, "rgv587_flag") || strings.Contains(head, "punish") {
			return "上游风控拦截（阿里安全校验页，非额度/凭据问题）：需在官方客户端人工完成验证后重试"
		}
		return "上游返回 HTML 而非模型流（首行证据：" + truncate(string(peek), 160) + "）"
	}
	return ""
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
	return ScanSSEAll(r, fn, nil)
}

// ScanSSEAll 与 ScanSSE 同，但把**非 data 行**也交给 onOther（可为 nil）——
// 上游塞回 HTML 风控页 / 错误信封时，那些行就是唯一的现场证据。
func ScanSSEAll(r io.Reader, fn func(data string), onOther func(line string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			if onOther != nil {
				onOther(line)
			}
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
	var sawData bool
	var otherEvidence string

	_ = ScanSSEAll(r, func(data string) {
		sawData = true
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
	}, func(line string) {
		if otherEvidence == "" {
			if s := strings.TrimSpace(line); s != "" {
				otherEvidence = truncate(s, 160)
			}
		}
	})
	if upstreamErr != "" {
		return nil, fmt.Errorf("%s", upstreamErr)
	}
	// 一帧都没有 = 上游压根没在说模型的话（风控页、错误信封、空响应）。以前这会被
	// 聚合成「200 + 空正文」的合法 completion，把拦截伪装成模型无话可说；现在报错误差。
	if !sawData {
		if otherEvidence == "" {
			return nil, fmt.Errorf("上游 200 但响应体为空，没有收到任何 ADK 帧")
		}
		return nil, fmt.Errorf("上游 200 但不是模型流（首行证据：%s）", otherEvidence)
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
