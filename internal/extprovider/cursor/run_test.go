package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 假流验证级（Chief 口径）：不连真实 Cursor，用本地 httptest 同时扮演
// RunSSE（下行 Connect 帧）与 BidiAppend（上行逐条消息），把 run.go 的
// H1 全路径端到端钉死。真连补验步骤见 docs/KNOWN-LIMITATIONS.md。

// fakeAgent 收下行帧脚本 + 记录上行 append 的裸消息体。
type fakeAgent struct {
	mu       sync.Mutex
	appends  [][]byte // 每次 BidiAppend 解出的 hex 消息体（#0 起）
	runSSE   [][]byte // 要按序写下去的帧负载（已含 flags，原样写出）
	status   int      // RunSSE 的 HTTP 状态（0 = 200）
	appendTo time.Duration
}

// frame 拼一条非压缩下行帧。end=true 置 end flag。
func frame(payload []byte, end bool) []byte {
	f := byte(0)
	if end {
		f = 2
	}
	return EncodeFrame(payload, f)
}

func (f *fakeAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathBidiAppend:
			body := make([]byte, 0, 4096)
			buf := make([]byte, 4096)
			for {
				n, err := r.Body.Read(buf)
				body = append(body, buf[:n]...)
				if err != nil {
					break
				}
			}
			// BidiAppend 请求体：{1: hex(msg), 2: {1: reqID}, 3: seq}
			fs := Fields(body)
			if hx := Str(fs, 1); hx != "" {
				if raw, err := hex.DecodeString(hx); err == nil {
					f.mu.Lock()
					f.appends = append(f.appends, raw)
					f.mu.Unlock()
				}
			}
			w.Header().Set("content-type", "application/proto")
			w.WriteHeader(http.StatusOK)
		case pathRunSSE:
			if f.status != 0 && (f.status < 200 || f.status >= 300) {
				w.WriteHeader(f.status)
				return
			}
			fl, _ := w.(http.Flusher)
			for _, fr := range f.runSSE {
				_, _ = w.Write(fr)
				if fl != nil {
					fl.Flush()
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// drain 读流到终局或超时，返回事件序列。
func drain(t *testing.T, s *ChatStream, timeout time.Duration) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-s.Events:
			if !ok {
				return out
			}
			out = append(out, e)
			if e.Kind == EventDone || e.Kind == EventError {
				return out
			}
		case <-deadline:
			t.Fatalf("stream timed out after %v, got %d events", timeout, len(out))
		}
	}
}

func newTestClient(f *fakeAgent) (*Client, *httptest.Server) {
	srv := httptest.NewServer(f.handler())
	c := New()
	c.APIBase = srv.URL
	return c, srv
}

func testConv(msgs ...string) *Conversation {
	raw := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		b, _ := json.Marshal(map[string]any{"role": "user", "content": m})
		raw = append(raw, b)
	}
	return &Conversation{Msgs: raw, Last: msgs[0], Estimate: 42}
}

func req(c string) ChatRequest {
	return ChatRequest{Token: "tok", Model: "gpt-5", Conv: testConv(c)}
}

func appends(f *fakeAgent) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.appends))
	copy(out, f.appends)
	return out
}

// ── 用例 ─────────────────────────────────────────────────────────────

// 正文 + 回合结束用量 + end 帧：text 事件在前，done 带正确 usage。
func TestChatRunTextThenUsage(t *testing.T) {
	f := &fakeAgent{runSSE: [][]byte{
		frame(PB().Sub(1, PB().Sub(1, PB().Str(1, "你好"))).Raw(), false),
		frame(PB().Sub(1, PB().Sub(14, PB().Varint(1, 100).Varint(2, 30).Varint(3, 20).Varint(4, 5))).Raw(), false),
		frame([]byte(`{}`), true),
	}}
	c, srv := newTestClient(f)
	defer srv.Close()

	s, err := c.ChatRun(context.Background(), req("hi"))
	if err != nil {
		t.Fatalf("ChatRun: %v", err)
	}
	evs := drain(t, s, 3*time.Second)
	if len(evs) != 2 || evs[0].Kind != EventText || evs[0].Text != "你好" {
		t.Fatalf("events = %+v", evs)
	}
	if evs[1].Kind != EventDone || evs[1].Usage == nil {
		t.Fatalf("no done event: %+v", evs)
	}
	u := evs[1].Usage
	// input=100, cacheRead=20, cacheWrite=5 → 净 input 75；prompt 回加缓存项 = 100。
	if u.PromptTokens != 100 || u.CompletionTokens != 30 || u.TotalTokens != 130 ||
		u.CachedTokens != 20 || u.CacheWriteTokens != 5 {
		t.Fatalf("usage = %+v", u)
	}
	if evs[1].Stop != "stop" {
		t.Fatalf("stop = %q", evs[1].Stop)
	}
	// #0 上行必须是 run_request（裸消息非带帧头）。
	if ap := appends(f); len(ap) < 1 || len(ap[0]) == 0 {
		t.Fatalf("no run_request appended: %v", ap)
	}
}

// 无正文无工具，只有 end 帧：按 fork 语义回空回复错误（可重试，非完成回合）。
func TestChatRunEmptyReply(t *testing.T) {
	f := &fakeAgent{runSSE: [][]byte{frame([]byte(`{}`), true)}}
	c, srv := newTestClient(f)
	defer srv.Close()
	s, _ := c.ChatRun(context.Background(), req("hi"))
	evs := drain(t, s, 3*time.Second)
	if len(evs) != 1 || evs[0].Kind != EventError || !strings.Contains(evs[0].Err.Error(), "空回复") {
		t.Fatalf("events = %+v", evs)
	}
}

// end 帧带 Connect 错误（配额耗尽）：分类成 429 + rate_limit_exceeded。
func TestChatRunEndFrameQuotaError(t *testing.T) {
	payload := `{"error":{"code":"resource_exhausted","message":"usage limit hit"}}`
	f := &fakeAgent{runSSE: [][]byte{frame([]byte(payload), true)}}
	c, srv := newTestClient(f)
	defer srv.Close()
	s, _ := c.ChatRun(context.Background(), req("hi"))
	evs := drain(t, s, 3*time.Second)
	if len(evs) != 1 || evs[0].Kind != EventError {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Status != http.StatusTooManyRequests || evs[0].Code != "rate_limit_exceeded" {
		t.Fatalf("status/code = %d/%q", evs[0].Status, evs[0].Code)
	}
}

// 建流非 2xx：不读流，直接吐 EventError（带建议状态）。
func TestChatRunBuildFailure(t *testing.T) {
	f := &fakeAgent{status: http.StatusUnauthorized}
	c, srv := newTestClient(f)
	defer srv.Close()
	s, err := c.ChatRun(context.Background(), req("hi"))
	if err != nil {
		t.Fatalf("ChatRun: %v", err)
	}
	evs := drain(t, s, 3*time.Second)
	if len(evs) == 0 || evs[0].Kind != EventError || evs[0].Status != http.StatusUnauthorized {
		t.Fatalf("events = %+v", evs)
	}
}

// 服务端索要 blob：field 4 下行 → 上行必须有应答消息（kv_response）。
func TestChatRunKVBlobRequest(t *testing.T) {
	conv := testConv("hi")
	// 取第一条消息的 sha256（ChatRun 内部 BuildRun 的 blobs 表以此键）。
	want := sha256.Sum256(conv.Msgs[0])
	kvMsg := PB().Sub(4, PB().Varint(1, 7).Sub(2, PB().BytesField(1, want[:]))).Raw()
	f := &fakeAgent{runSSE: [][]byte{
		frame(kvMsg, false),
		frame([]byte(`{}`), true),
	}}
	c, srv := newTestClient(f)
	defer srv.Close()
	s, _ := c.ChatRun(context.Background(), req("hi"))
	drain(t, s, 3*time.Second)
	// append #0 run_request + 至少一条 kv 应答。
	ap := appends(f)
	if len(ap) < 2 {
		t.Fatalf("expected kv response append, got %d appends", len(ap))
	}
	if fs := Fields(ap[1]); fs == nil || Find(fs, 3) == nil {
		t.Fatalf("second append is not a kv_response (field 3): %+v", fs)
	}
}

// 工具调用：field 27 先报数量，field 2 exec 交付一次调用 → EventTool 后 Done(tool_calls)。
func TestChatRunToolCallAndListed(t *testing.T) {
	// mcp_args 的 kv 就是 CallDynamicTool 的入参包：{namespace,toolName,arguments}；
	// run.go 从 args["arguments"] 解出真参数 JSON（buildConversation 的包装对应物）。
	argVal, err := Value(map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	a := PB().Str(5, "shell").Str(3, "call_1").
		Sub(2, PB().Str(1, "arguments").Sub(2, argVal))
	execMsg := PB().Sub(2, PB().Varint(1, 9).Sub(11, a)).Raw()
	listedMsg := PB().Sub(1, PB().Sub(27, PB().Varint(1, 1))).Raw()
	f := &fakeAgent{runSSE: [][]byte{
		frame(listedMsg, false),
		frame(execMsg, false),
	}}
	c, srv := newTestClient(f)
	defer srv.Close()
	s, _ := c.ChatRun(context.Background(), req("hi"))
	evs := drain(t, s, 3*time.Second)
	var tool *Event
	for i := range evs {
		if evs[i].Kind == EventTool {
			tool = &evs[i]
		}
	}
	if tool == nil {
		t.Fatalf("no tool event: %+v", evs)
	}
	if tool.Tool.Name != "shell" || tool.Tool.ID != "call_1" {
		t.Fatalf("tool = %+v", tool.Tool)
	}
	if !strings.Contains(tool.Tool.Args, `"x":1`) {
		t.Fatalf("tool args = %q", tool.Tool.Args)
	}
	last := evs[len(evs)-1]
	if last.Kind != EventDone || last.Stop != "tool_calls" {
		t.Fatalf("final = %+v", last)
	}
}

// Close 幂等且释放 goroutine：不阻塞。
func TestChatRunCloseIdempotent(t *testing.T) {
	f := &fakeAgent{runSSE: [][]byte{frame(PB().Sub(1, PB().Sub(1, PB().Str(1, "x"))).Raw(), false)}}
	c, srv := newTestClient(f)
	defer srv.Close()
	s, _ := c.ChatRun(context.Background(), req("hi"))
	s.Close()
	s.Close() // 幂等，panic 即失败
}

// append 失败终局 Run（fork end() 语义 L1764-1770）：服务端 500 的
// BidiAppend 必须把真实病因（HTTP 500）作为 EventError 放出来，而不是
// 吞掉后卡在"流在终局前结束"。
func TestChatRunAppendFailureEndsRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathBidiAppend:
			w.WriteHeader(http.StatusInternalServerError)
		case pathRunSSE:
			// 立即给响应头（让建流成功），然后挂着不回 end 帧：
			// 上行失败后必须由 fail() 取消 runCtx 来终局。
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New()
	c.APIBase = srv.URL

	s, err := c.ChatRun(context.Background(), req("hi"))
	if err != nil {
		t.Fatalf("ChatRun: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-s.Events:
			if !ok {
				t.Fatal("stream closed without an error event")
			}
			if e.Kind == EventError {
				if !strings.Contains(e.Err.Error(), "BidiAppend HTTP 500") {
					t.Fatalf("error should name the append failure: %v", e.Err)
				}
				return
			}
		case <-deadline:
			t.Fatal("append failure did not end the Run within 5s")
		}
	}
}

// 上行严格串行（coder 双验缺口回归钉死）：每条 BidiAppend 都必须是
// 开始→完成→下一条，序号 0,1,2… 连续无重叠（fork L1729 chain 语义——
// 上一条完成才发下一条，心跳/应答/首帧同队）。
//
// 关键：心跳 goroutine（pumpRun 之外）与解码应答（pumpRun 之内）必须
// **真的并发**——心跳周期经 HeartbeatEvery 钩子压到毫秒级，RunSSE 保持
// 打开直到攒够多条上行（含数个心跳）才发 end 帧。修复前的"锁外 HTTP"
// 写法在此构造下必然出现 inFlight>1（重叠）或序号乱序。
func TestAppendsStrictlySerialized(t *testing.T) {
	var (
		mu        sync.Mutex
		inFlight  int
		overlaped bool
		seqs      []int
	)
	appendCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(seqs)
	}

	conv := testConv("hi")
	want := sha256.Sum256(conv.Msgs[0])
	kvReq := PB().Sub(4, PB().Varint(1, 7).Sub(2, PB().BytesField(1, want[:]))).Raw()
	// 再要一次工具清单（36 类应答，同 goroutine 但多一条上行）。
	stateReq := PB().Sub(2, PB().Varint(1, 12).Sub(36, PB().Sub(1, PB().Str(1, "q")))).Raw()

	const wantAppends = 6 // 首帧 + kv 应答 + 清单应答 + ≥3 个心跳
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathRunSSE {
			fl, _ := w.(http.Flusher)
			_, _ = w.Write(frame(kvReq, false))
			_, _ = w.Write(frame(stateReq, false))
			if fl != nil {
				fl.Flush()
			}
			// 攒够上行（心跳持续在发）再收流。
			for i := 0; i < 200 && appendCount() < wantAppends; i++ {
				time.Sleep(5 * time.Millisecond)
			}
			_, _ = w.Write(frame([]byte(`{}`), true))
			if fl != nil {
				fl.Flush()
			}
			return
		}
		// BidiAppend：记录并发窗口与序号
		mu.Lock()
		inFlight++
		if inFlight > 1 {
			overlaped = true
		}
		mu.Unlock()
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		fs := Fields(body)
		mu.Lock()
		seqs = append(seqs, int(Num(fs, 3)))
		mu.Unlock()
		time.Sleep(2 * time.Millisecond) // 放大并发窗口：乱序在此现形
		w.WriteHeader(http.StatusOK)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}))
	defer srv.Close()
	c := New()
	c.APIBase = srv.URL
	beat := 2 * time.Millisecond // 跨 goroutine 并发源（心跳）压到毫秒级
	c.HeartbeatEvery = &beat

	s, err := c.ChatRun(context.Background(), ChatRequest{Token: "tok", Model: "gpt-5", Conv: conv})
	if err != nil {
		t.Fatalf("ChatRun: %v", err)
	}
	drain(t, s, 10*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(seqs) < wantAppends {
		t.Fatalf("only %d appends — 心跳没参与，用例没压到跨 goroutine 并发", len(seqs))
	}
	if overlaped {
		t.Fatal("BidiAppend overlapped: 上行必须串行（fork chain 语义）")
	}
	for i, n := range seqs {
		if n != i {
			t.Fatalf("seq out of order at %d: got %d", i, n)
		}
	}
}
