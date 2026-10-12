package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 阶段 2：proxyQoderResilientSSE 场景测试 ───────────────────
 * 审阅意见②：held 帧释放拆四个显式用例；铁律③反例在此钉死。
 * 直测 proxyQoderResilientSSE（不经 HTTP 层），上游流用 io.ReadCloser 注入。
 */

// newQoderPumpHarness 构造一个最小 Handler（无 extManager——续跑路径会因
// 无账号返回 false，正好测"续跑失败"分支；主路径不依赖账号）。
func newQoderPumpHarness(t *testing.T) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: upstream.New(),
	})
}

// runPump 跑一遍 resilient SSE 泵，返回写给客户端的完整输出。
func runPump(t *testing.T, h *Handler, upstreamSSE string, readErr error, origBody string) string {
	t.Helper()
	rc := &fakeReadCloser{data: upstreamSSE, readErr: readErr}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(origBody))
	h.proxyQoderResilientSSE(w, req, rc, qoder.ResilienceConfig{AutoContinueMax: 1}, "acct-test", []byte(origBody), "")
	return w.Body.String()
}

type fakeReadCloser struct {
	data    string
	pos     int
	readErr error
}

func (f *fakeReadCloser) Read(p []byte) (int, error) {
	if f.pos < len(f.data) {
		n := copy(p, f.data[f.pos:])
		f.pos += n
		return n, nil
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	return 0, io.EOF
}

func (f *fakeReadCloser) Close() error { return nil }

const chunkContent = `{"id":"c1","choices":[{"delta":{"content":"%s"}}]}`

func chunkLine(s string) string { return "data: " + fmt.Sprintf(chunkContent, s) + "\n\n" }

func finishLine(reason string) string {
	return "data: " + fmt.Sprintf(`{"id":"c1","choices":[{"delta":{},"finish_reason":"%s"}]}`, reason) + "\n\n"
}

// ── 场景 1（held 释放·无续跑顺序）：finish(stop) + EOF 无 [DONE]
// → 终帧释放 + [DONE]，顺序：终帧在 [DONE] 之前。 ──

func TestQoderPumpFinishWithoutDoneReleasesHeld(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := chunkLine("答案正文") + finishLine("stop")
	out := runPump(t, h, sse, nil, `{"messages":[]}`)

	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("held finish frame not released:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", out)
	}
	fi := strings.Index(out, `"finish_reason":"stop"`)
	di := strings.Index(out, "data: [DONE]")
	if fi > di {
		t.Fatalf("finish frame must precede [DONE]:\n%s", out)
	}
}

// ── 场景 2（held 释放·有续跑哨兵复用）：续跑关闭（无账号）时
// → 续跑失败错误帧 + [DONE]，held 帧不透传（已丢弃）。 ──

func TestQoderPumpAutoContinueFailedDropsHeld(t *testing.T) {
	h := newQoderPumpHarness(t)
	// 带 tools + 悬挂意图 + stop——触发续跑，但 harness 无账号 → 失败。
	sse := chunkLine("让我先检查配置文件。") + finishLine("stop")
	out := runPump(t, h, sse, nil, `{"tools":[{"type":"function"}],"messages":[]}`)

	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("held frame must be dropped before failed continuation:\n%s", out)
	}
	if !strings.Contains(out, "auto-continue request failed") {
		t.Fatalf("continuation failure not surfaced:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", out)
	}
}

// ── 场景 3（held 释放·信封错误置空顺序）：finish 后来中途信封错误
// → 错误帧透出，held 帧不得出现。 ──

func TestQoderPumpMidEnvelopeErrorClearsHeld(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := chunkLine("部分内容") + finishLine("stop") +
		"data: " + `{"statusCodeValue":500,"body":"mid-stream boom"}` + "\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)

	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("held frame must be cleared on envelope error:\n%s", out)
	}
	if !strings.Contains(out, "mid-stream boom") {
		t.Fatalf("inline error missing:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] missing after inline error:\n%s", out)
	}
}

// ── 场景 4（held 释放·abort 不释放）：客户端断开（写失败）
// → 静默，无错误帧、无 [DONE]、held 帧不泄。 ──

func TestQoderPumpClientAbortSilent(t *testing.T) {
	h := newQoderPumpHarness(t)
	// 一个写一次就失败的 ResponseWriter 模拟客户端断开。
	broken := &brokenWriter{header: http.Header{}}
	rc := &fakeReadCloser{data: chunkLine("第一段") + chunkLine("第二段") + finishLine("stop")}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	h.proxyQoderResilientSSE(broken, req, rc, qoder.ResilienceConfig{}, "acct", []byte(`{"messages":[]}`), "")

	out := broken.buf.String()
	if strings.Contains(out, "error") || strings.Contains(out, "[DONE]") || strings.Contains(out, "finish_reason") {
		t.Fatalf("abort path must stay silent, got:\n%s", out)
	}
}

type brokenWriter struct {
	header http.Header
	buf    strings.Builder
	writes int
}

func (b *brokenWriter) Header() http.Header { return b.header }
func (b *brokenWriter) Write(p []byte) (int, error) {
	b.writes++
	if b.writes > 1 { // 第一次写（头部后的第一帧）成功，之后断开
		return 0, fmt.Errorf("client gone")
	}
	return b.buf.Write(p)
}
func (b *brokenWriter) WriteHeader(int) {}
func (b *brokenWriter) Flush()          {}

// ── 铁律③反例：见过 finish_reason 后 transport error
// → 必须发 stream_disconnected，不得释放 held 帧、不得补 [DONE] 当正常结束。 ──

func TestQoderPumpTransportErrorAfterFinishIsFailure(t *testing.T) {
	h := newQoderPumpHarness(t)
	// 正常 EOF 前 read 返回错误（不是 EOF）。
	sse := chunkLine("截断的答案") + finishLine("stop")
	out := runPump(t, h, sse, fmt.Errorf("connection reset by peer"), `{"messages":[]}`)

	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("held frame must NOT be released on transport error:\n%s", out)
	}
	if !strings.Contains(out, "stream_disconnected") {
		t.Fatalf("failure not reported as stream_disconnected:\n%s", out)
	}
	if strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") &&
		!strings.Contains(out, "error") {
		t.Fatalf("transport error must not masquerade as clean done:\n%s", out)
	}
}

// ── 偏差1 修复验证：429/限流内联错误帧带 retry-after 提示 ──

func TestQoderPumpRateLimitFrameCarriesRetryHint(t *testing.T) {
	h := newQoderPumpHarness(t)
	// 中途信封错误且分类为限流（code 带 rate_limit_exceeded 语义由上游给）。
	sse := "data: " + `{"statusCodeValue":429,"body":"too many requests, slow down"}` + "\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)
	if !strings.Contains(out, "stream_disconnected") && !strings.Contains(out, "error") {
		t.Fatalf("429 envelope must produce inline error:\n%s", out)
	}
	// 直接验证帧构造原语（桥接路径的 code 由分类决定，这里锁 retry-after 语义）。
	frame := qoder.BuildMidStreamErrorFrame("rate_limit_exceeded", "too many requests")
	if !strings.Contains(frame, "retry after 30s") {
		t.Fatalf("rate_limit frame missing retry hint: %s", frame)
	}
}

// ── 偏差2 修复验证：tool_calls 终帧直透（不暂存、立即到客户端）──

func TestQoderPumpToolCallsTerminalPassthrough(t *testing.T) {
	h := newQoderPumpHarness(t)
	// tool_calls 终帧后跟 [DONE]：终帧必须在 [DONE] 之前原样到达（不暂存）。
	sse := chunkLine("调用工具中") + finishLine("tool_calls") + "data: [DONE]\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)

	fi := strings.Index(out, `"finish_reason":"tool_calls"`)
	di := strings.Index(out, "data: [DONE]")
	if fi < 0 || di < 0 || fi > di {
		t.Fatalf("tool_calls frame must pass through before [DONE]:\n%s", out)
	}
	// 且不得触发续跑（fork：tool_calls 是真实回合结束）
	if strings.Contains(out, "auto-continue") {
		t.Fatalf("tool_calls turn must not continue:\n%s", out)
	}
	if strings.Contains(out, "error") {
		t.Fatalf("tool_calls + done must be clean:\n%s", out)
	}
}

// ── premature_done：无 finish_reason 即 [DONE]。 ──

func TestQoderPumpPrematureDone(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := chunkLine("没说完就断") + "data: [DONE]\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)

	if !strings.Contains(out, "stream_disconnected") {
		t.Fatalf("premature [DONE] must fail loudly:\n%s", out)
	}
}

// ── clean_done：finish + [DONE] 正常路径。 ──

func TestQoderPumpCleanDone(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := chunkLine("完整回答") + finishLine("stop") + "data: [DONE]\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)

	if strings.Contains(out, "error") {
		t.Fatalf("clean done must have no error frame:\n%s", out)
	}
	// finish 帧释放恰一次（held 释放 + 不重复透传）
	if strings.Count(out, `"finish_reason":"stop"`) != 1 {
		t.Fatalf("finish frame count = %d, want 1:\n%s", strings.Count(out, `"finish_reason":"stop"`), out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", out)
	}
}

// ── premature_eof：EOF 且无 finish 无 [DONE]。 ──

func TestQoderPumpPrematureEOF(t *testing.T) {
	h := newQoderPumpHarness(t)
	out := runPump(t, h, chunkLine("只有正文"), nil, `{"messages":[]}`)
	if !strings.Contains(out, "stream_disconnected") {
		t.Fatalf("premature eof must fail loudly:\n%s", out)
	}
}

// ── 排队帧防御：流内 isQueued 帧不透传、发注释。 ──

func TestQoderPumpQueueSignalHeartbeat(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := "data: " + `{"isQueued":true}` + "\n\n" + chunkLine("真实正文") + "data: [DONE]\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)
	if strings.Contains(out, `"isQueued":true`) {
		t.Fatalf("queue frame leaked:\n%s", out)
	}
	if !strings.Contains(out, ": queued") {
		t.Fatalf("heartbeat comment missing:\n%s", out)
	}
	if !strings.Contains(out, "真实正文") {
		t.Fatalf("content after queue frame lost:\n%s", out)
	}
}

// ── AutoContinueMax=0 关闭续跑（env 止血路径）。 ──

func TestQoderPumpAutoContinueDisabled(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := chunkLine("让我先检查配置文件。") + finishLine("stop")
	rc := &fakeReadCloser{data: sse}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"tools":[{}]}`))
	h.proxyQoderResilientSSE(w, req, rc, qoder.ResilienceConfig{AutoContinueMax: 0}, "acct", []byte(`{"tools":[{}]}`), "")
	out := w.Body.String()
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("AutoContinueMax=0 must release held frame directly:\n%s", out)
	}
	if strings.Contains(out, "auto-continue") {
		t.Fatalf("continuation must not be attempted:\n%s", out)
	}
}

// ── 上下文溢出映射 400。 ──

func TestQoderPumpContextOverflowMapped(t *testing.T) {
	h := newQoderPumpHarness(t)
	sse := "data: " + `{"statusCodeValue":413,"body":"This model's maximum context length is 65536 tokens"}` + "\n\n"
	out := runPump(t, h, sse, nil, `{"messages":[]}`)
	var probe struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &probe); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if probe.Error.Code != "context_length_exceeded" {
		t.Fatalf("overflow code = %q:\n%s", probe.Error.Code, out)
	}
}

func firstJSONLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "data: [DONE]") {
			payload := strings.TrimPrefix(line, "data: ")
			if strings.Contains(payload, `"error"`) {
				return payload
			}
		}
	}
	return s
}

// 防编译器报 time 未用（测试文件头部 import 保留扩展位）。
var _ = time.Now
var _ = context.Background
