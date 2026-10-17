package codearts

// chat_timeout_test.go — P1-5 的回归守卫：CodeArts 对话流不能被 30s 总超时硬切。
//
// 缺陷（审计 P1-5）：包级 httpClient 带 `Timeout: requestTimeout`（30s），而
// http.Client.Timeout 覆盖**整个请求生命周期，含读响应体**。Chat()
// （chat.go:284）返回的是 SSE 流，慢模型/长上下文/思维链模型的回答超过 30s
// 就会被 transport 在 body 读到一半时强行断开——客户端看到「答到一半没了」，
// 甚至可能连 data: [DONE] 都没有。
//
// 同仓库其余对话通道都刻意避开：upstream/client.go:679 `ChatHTTP: Timeout: 0`、
// zai/http.go:39 `Timeout: 0`、其余 extprovider 全是 `Timeout: 0` +
// ResponseHeaderTimeout。只有 CodeArts 用 30s 总超时跑对话。
//
// **本条测试在交付时是 FAIL 的**——它就是 P1-5 的守卫，不是摆设。修复
// （对话链路换独立 Timeout:0 + ResponseHeaderTimeout client）落地后转绿。
import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newSlowFirstFrameUpstream 首帧延迟 delay 之后才开始吐正文的 SSE 上游。
//
// 为什么用 httptest + 真实网络栈而不是自定义 RoundTripper：Client.Timeout
// 的定时器只在**真实 http.Transport** 上生效（探针实证：把 35s 睡眠的
// io.Pipe 塞进自定义 RoundTripper 时，请求直接跑完、超时完全不触发——
// 那条路根本测不到缺陷）。
type slowFirstFrameUpstream struct {
	*httptest.Server
	hits int
}

func newSlowFirstFrameUpstream(t *testing.T, delay time.Duration, frames []string) *slowFirstFrameUpstream {
	t.Helper()
	u := &slowFirstFrameUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(delay)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.Close)
	return u
}

// redirectTransport 把指向 SnapEngineURL 的请求改打到本地 test server。
// Chat() 的目标写死在 SnapEngineURL（const，不改生产代码），只能在这一层
// 转发；签名与请求形状原样保留，改写的只有 scheme/host。
type redirectTransport struct {
	base http.RoundTripper
	to   *url.URL
}

func (rt *redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	u.Scheme = rt.to.Scheme
	u.Host = rt.to.Host
	r2 := r.Clone(r.Context())
	r2.URL = &u
	r2.Host = ""
	return rt.base.RoundTrip(r2)
}

// TestChatStreamSurvivesSlowFirstFrame 上游首帧 35s 后才到（> 30s 总超时），
// 对话流必须不断线：所有 SSE 帧可完整读完，且能读到 [DONE]。
//
// 失败形态（P1-5 未修时）：Chat() 返回
// `Post ".../api/v2/chat/completions": context deadline exceeded
// (Client.Timeout exceeded while awaiting headers)`。
//
// 为什么用 35s 真实等待而不是 Short 跳过：这条测试守的是**真实时长超时**，
// 把 sleep 缩短成 0 会让 Client.Timeout（30s）永远不会触发，测试恒绿、
// 完全失去守卫能力——一个不会红的守卫等于没有。35s 只比 30s 多 5s 余量，
// 测试内不并发，只拖自己这 35s。
func TestChatStreamSurvivesSlowFirstFrame(t *testing.T) {
	const firstFrameDelay = 35 * time.Second
	if testing.Short() {
		t.Skip("真实等待 35s 超时验证；-short 模式下跳过（缩短 sleep 会让 30s 总超时永不触发，守卫失效）")
	}

	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"upstream","choices":[{"delta":{"role":"assistant","content":"hello "}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"upstream","choices":[{"delta":{"content":"world"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"upstream","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}

	up := newSlowFirstFrameUpstream(t, firstFrameDelay, frames)
	to, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("解析 test server URL: %v", err)
	}

	// 保留包级 client 的 30s 总超时（缺陷本体），只把出站流量改道到本地。
	// 这正是 P1-5 的形态：client 带 Timeout=30s，上游首帧 35s 后才到。
	SetHTTPClient(&http.Client{
		Timeout:   requestTimeout,
		Transport: &redirectTransport{base: http.DefaultTransport, to: to},
	})
	// 恢复干净默认 client，避免污染同包后续用例（它们复用包级 httpClient）。
	defer SetHTTPClient(&http.Client{Timeout: requestTimeout, Transport: http.DefaultTransport})

	cli := New()
	start := time.Now()
	resp, cerr := cli.Chat(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		"glm-5.2", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), true, ChatOptions{})
	if cerr != nil {
		// 客户端超时错误的两种文本形态都要认出来，报成同一个可读原因。
		var uerr *url.Error
		if errors.As(cerr, &uerr) && uerr.Timeout() {
			t.Fatalf("对话被 30s 总超时切断（P1-5 未修）：%v", cerr)
		}
		t.Fatalf("对话失败：%v", cerr)
	}
	defer resp.Body.Close()

	// 逐行读完：这正是 Client.Timeout 会杀掉的阶段。
	var got []string
	sawDone := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		if payload != "" {
			got = append(got, payload)
		}
	}
	if serr := sc.Err(); serr != nil {
		t.Fatalf("读流中断（P1-5：流中段被 transport 切断）：%v", serr)
	}
	if !sawDone {
		t.Errorf("没读到 data: [DONE]，流被提前切断")
	}
	if len(got) != len(frames) {
		t.Errorf("只读到 %d/%d 帧：%v", len(got), len(frames), got)
	}
	for i, want := range frames {
		if i >= len(got) {
			break
		}
		if got[i] != want {
			t.Errorf("帧 %d 不符:\n got: %s\nwant: %s", i, got[i], want)
		}
	}
	// 首帧确实等到了 35s（证明测试真的跨越了 30s 线，而不是侥幸走快路径）。
	if elapsed := time.Since(start); elapsed < requestTimeout {
		t.Errorf("请求 %v 就结束了，未跨越 30s 总超时线，本测试无效", elapsed)
	}
}
