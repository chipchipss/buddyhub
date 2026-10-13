// Package server: zcode_sse.go — ZCode 流内额度耗尽扫描（Chief 裁定①）。
//
// fork spentUp 本来就在 200 body 上判（index.mjs L1560-69）——Anthropic
// SSE 流里，额度耗尽以 error 事件（body code 1113/1005）形态出现在流中。
// 这里给 zcode 桥的 SSE 泵加一个帧级钩子：事件 JSON 命中 zcodeSpentUp 时
// 触发 onSpent（入账 + noteChat 冷却），**不消费流、不中断转发**——耗尽
// 事件本身仍按原样走翻译层（Anthropic error event → OpenAI error chunk，
// 客户端能看到真实错误），账本记录是旁路观测。
//
// 非流式路径的 200+spent 已在 zcode_bridge.go 主桥处理（读 body 可行）。
package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// proxyZcodeSSEWithSpentCheck 流式透传 + 帧级耗尽扫描。
// 与 proxyZaiSSE 唯一差别：每个 Anthropic 事件先过 zcodeSpentUp（只在首次
// 命中时触发 onSpent——一次流多帧重复报错不该重复入账），然后照常交给
// 翻译泵。错误事件在 Anthropic 流中形状为 {type:"error", error:{code,...}}。
func (h *Handler) proxyZcodeSSEWithSpentCheck(w http.ResponseWriter, rc io.ReadCloser, model string, a *extstore.ExtAccount) {
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
	spentSeen := false
	upstream.ReadAnthropicSSE(rc, func(event []byte) {
		if !spentSeen && zcodeSpentUp(string(event), http.StatusOK) {
			spentSeen = true
			h.zcodeRecordSpent(a)
			// noteChat 冷却（与 HTTP 429 路径同款——extstore 轮转把该号退后）。
			h.noteChat(a.Provider, a.ID, errOf("额度/配额耗尽（流内 200-body 1113/1005）"))
		}
		chunks, err := conv.Feed(event)
		if err != nil {
			return
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + string(c) + "\n\n"))
			flusher.Flush()
		}
	})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// zaiAggregateSSEWithSpentCheck 非流式客户端收 SSE 的聚合变体：
// 聚合前逐帧扫耗尽，命中时调 onSpent（一次流只触发一次），帧照常进聚合。
func zaiAggregateSSEWithSpentCheck(rc io.ReadCloser, model string, onSpent func()) map[string]any {
	seen := false
	wrapped := &spentCheckReader{inner: rc, onSpent: func(event []byte) {
		if !seen && zcodeSpentUp(string(event), http.StatusOK) {
			seen = true
			onSpent()
		}
	}}
	return zaiAggregateSSE(wrapped, model)
}

// spentCheckReader 在 SSE data 行级别钩 zcodeSpentUp（不改变字节流）。
type spentCheckReader struct {
	inner    io.ReadCloser
	onSpent  func([]byte)
	buf      []byte
	seenData bool
}

func (s *spentCheckReader) Read(p []byte) (int, error) {
	n, err := s.inner.Read(p)
	if n > 0 {
		s.buf = append(s.buf, p[:n]...)
		for {
			idx := -1
			for i, b := range s.buf {
				if b == '\n' {
					idx = i
					break
				}
			}
			if idx < 0 {
				break
			}
			line := string(s.buf[:idx])
			s.buf = s.buf[idx+1:]
			if payload, ok := cutSSEData(line); ok && !s.seenData {
				s.seenData = true
				if s.onSpent != nil {
					s.onSpent([]byte(payload))
				}
			}
		}
	}
	return n, err
}

func (s *spentCheckReader) Close() error { return s.inner.Close() }

// cutSSEData 从一行 SSE 里剥 "data: " 前缀。
func cutSSEData(line string) (string, bool) {
	for _, prefix := range []string{"data: ", "data:"} {
		if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
			return strings.TrimSpace(line[len(prefix):]), true
		}
	}
	return "", false
}

// 引用占位（qoder 包在此文件用于 AccountKey——保留 import 语义一致）。
var _ = qoder.AccountKey
