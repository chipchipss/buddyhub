package codearts

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

/* ── 对话长流不被总超时腰斩（P1-5 守卫）────────────────────────── */

// slowChatTransport 让对话首帧延迟 delay 才产出，模拟 CodeArts 上的慢模型 /
// 长上下文 / 思维链模型。短请求（签到/余额）不受影响，立即返回。
//
// **必须响应取消**（knownRoundTripperImpl 之外的自定义 RoundTripper 要走
// r.Context().Done()）：http.Client.Timeout 到期时是 transport 负责把请求掐掉
// 的。若 mock 只管 time.Sleep 睡完就返回，永远收不到取消信号，P1-5 是否复现
// 一概测不出来——守卫恒绿，等于没有守卫。
type slowChatTransport struct {
	delay   time.Duration
	chatHit chan struct{}
}

func (t *slowChatTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	isChat := strings.HasSuffix(r.URL.Path, ChatPath)
	hitOnce := func() {
		if t.chatHit != nil {
			select {
			case t.chatHit <- struct{}{}:
			default:
			}
		}
	}
	if !isChat {
		hitOnce()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	}

	// 真实等待首帧；期间请求被取消（超时）就立刻把错误抛回去，
	// 与 http.Transport 在超时/取消时的行为一致。
	timer := time.NewTimer(t.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
		hitOnce()
		return nil, r.Context().Err()
	}
	hitOnce()

	// 首帧 + [DONE]：一场正常结束的 SSE 短流。
	frame := `data: {"id":"c1","object":"chat.completion.chunk","model":"codearts:glm-5.3",` +
		`"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(frame)),
	}, nil
}

// 首帧 35s 后到达的对话：建流必须成功，且整个流能读完。
//
// 这条守的是 P1-5：`http.Client.Timeout` 覆盖整个请求生命周期（含读 body），
// 对话曾复用短请求的 30s client，于是任何超过 30s 的回答在第 30 秒被硬切——
// 用户看到的是半句话突然断掉。现在对话走独立的 Timeout:0 client。
func TestChatLongStreamIsNotCutByTotalTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("本用例需真实等待 35s（>30s 硬切断阈值）；-short 下跳过，CI 跑全量时生效")
	}
	const firstFrameDelay = 35 * time.Second // 必须 > 修复前的 requestTimeout(30s)

	tr := &slowChatTransport{delay: firstFrameDelay, chatHit: make(chan struct{}, 1)}
	SetHTTPClient(&http.Client{Timeout: requestTimeout, Transport: tr})
	t.Cleanup(func() { SetHTTPClient(&http.Client{Timeout: requestTimeout}) })

	cli := New()
	if cli.ChatHTTP == nil {
		t.Fatal("New() 应带 ChatHTTP")
	}

	ctx, cancel := context.WithTimeout(context.Background(), firstFrameDelay+30*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := cli.Chat(ctx, &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		"glm-5.3", []byte(`{"messages":[]}`), true, ChatOptions{})
	if err != nil {
		t.Fatalf("对话建流被切断（P1-5 复发）: %v（已过去 %v）", err, time.Since(start).Round(time.Millisecond))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body) // 读完整个流，不能半途 EOF/报错
	if err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	if !strings.Contains(string(raw), `"content":"hi"`) {
		t.Fatalf("流内容不完整: %q", string(raw))
	}
	if !strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("流缺少终止帧 [DONE]: %q", string(raw))
	}
	// 证明它真的等到了首帧，而不是走了某条"立即返回"的捷径。
	if elapsed := time.Since(start); elapsed < firstFrameDelay {
		t.Fatalf("只过了 %v 就返回了，没走到慢首帧路径", elapsed.Round(time.Millisecond))
	}
}

// 短请求（签到/余额类）仍必须被 30s 硬切——拆双客户端不能把短请求的保护也拆掉。
// 这是上面那条的对偶：对话无限等，短请求照样有界。
//
// 本条断言**出厂配置**（defaultChatClient），不是包级全局：那个会被
// SetHTTPClient 注入顶掉，断言它等于让结果取决于别的用例先跑过什么。
func TestShortRequestStillBoundedByTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("本用例需真实等待 >30s；-short 下跳过")
	}

	// 短请求：30s 总超时。写死期望值，别读可能被注入顶掉的 httpClient 全局。
	if got := (&http.Client{Timeout: requestTimeout}).Timeout; got != 30*time.Second {
		t.Fatalf("短请求总超时基准 = %v, want 30s", got)
	}
	if requestTimeout != 30*time.Second {
		t.Fatalf("requestTimeout = %v, want 30s（拆双客户端时不许顺手改掉短请求的 30s）", requestTimeout)
	}

	// 对话：无总超时（否则长流被腰斩），首字节一个有界上限（否则挂死）。
	def := defaultChatClient()
	if def.Timeout != 0 {
		t.Fatalf("对话总超时 = %v, want 0（否则长流会被腰斩）", def.Timeout)
	}
	tr, ok := def.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("对话 transport 类型 = %T, want *http.Transport", def.Transport)
	}
	if tr.ResponseHeaderTimeout != chatRequestTimeout {
		t.Fatalf("对话首字节超时 = %v, want %v", tr.ResponseHeaderTimeout, chatRequestTimeout)
	}
}

// SetHTTPClient 的搬迁语义：测试注入一处 mock，对话链路必须也走它。
// 否则注入只覆盖签到，对话仍打真网——CI 会真连华为云。
func TestSetHTTPClientAlsoCoversChat(t *testing.T) {
	hit := make(chan struct{}, 4)
	tr := &slowChatTransport{delay: 0, chatHit: hit}
	SetHTTPClient(&http.Client{Timeout: requestTimeout, Transport: tr})
	t.Cleanup(func() { SetHTTPClient(&http.Client{Timeout: requestTimeout}) })

	cli := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := cli.Chat(ctx, &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		"glm-5.3", []byte(`{"messages":[]}`), true, ChatOptions{})
	if err != nil {
		t.Fatalf("对话没走到注入的 transport: %v", err)
	}
	defer resp.Body.Close()
	select {
	case <-hit:
	default:
		t.Fatal("对话没有经过注入的 transport（会打到真网）")
	}
}

// SetChatHTTPClient 的两条硬契约（我验收 P1-5 时补的兜底，opencode 原实现里
// 没有）——注入路径不能把「对话永不被总时长腰斩」这个修复悄悄破坏掉：
//
//  1. c.Timeout 一律归 0。搬过来的话，任何带 Timeout 的注入都会把 P1-5 修掉
//     的长流腰斩又放回来，而且调用方看不出区别。
//  2. 首字节必须有界（ResponseHeaderTimeout）。Timeout:0 意味着永不总切断，
//     首字节就必须由它单独兜底，否则上游只连上不吐头时对话无限挂等——
//     既不报错也不结束，goroutine 和连接一起泄漏。
//
// 原实现的坑（探针实证）：SetChatHTTPClient(&http.Client{Timeout: 0}) 会留下
// transport = nil（即 http.DefaultTransport，ResponseHeaderTimeout = 0），
// 且 SetChatHTTPClient 的「还原」路径同样退化。
func TestSetChatHTTPClientNeverUnbounded(t *testing.T) {
	cases := []struct {
		name string
		in   *http.Client
	}{
		{"裸 client（无 transport）", &http.Client{Timeout: 0}},
		{"显式 Timeout 必须被清零", &http.Client{Timeout: 30 * time.Second, Transport: http.DefaultTransport}},
		{"自带空 transport", &http.Client{Transport: &http.Transport{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SetChatHTTPClient(tc.in)
			t.Cleanup(func() { SetChatHTTPClient(nil) })

			cli := New()
			if cli.ChatHTTP.Timeout != 0 {
				t.Errorf("对话总超时 = %v, want 0（P1-5 会被这条注入复发）", cli.ChatHTTP.Timeout)
			}
			tr, ok := cli.ChatHTTP.Transport.(*http.Transport)
			if !ok || tr == nil {
				t.Fatalf("transport = %T, want *http.Transport", cli.ChatHTTP.Transport)
			}
			if tr.ResponseHeaderTimeout != chatRequestTimeout {
				t.Errorf("首字节上限 = %v, want %v（0 = 对话流会无限挂等）", tr.ResponseHeaderTimeout, chatRequestTimeout)
			}
		})
	}

	// 注入 http.DefaultTransport 时**不能就地改**：那是进程级共享的，在它身上设
	// ResponseHeaderTimeout 会影响所有用它的请求（别的 provider、别的包）。
	t.Run("不污染全局 DefaultTransport", func(t *testing.T) {
		SetChatHTTPClient(&http.Client{Transport: http.DefaultTransport})
		t.Cleanup(func() { SetChatHTTPClient(nil) })
		if d, ok := http.DefaultTransport.(*http.Transport); ok && d.ResponseHeaderTimeout != 0 {
			t.Errorf("全局 DefaultTransport 被改：ResponseHeaderTimeout = %v（应保持 0，由 Clone 隔离）", d.ResponseHeaderTimeout)
		}
	})

	// 宿主显式配的首字节上限不许被我们的兜底覆盖掉。
	t.Run("显式首字节上限被保留", func(t *testing.T) {
		SetChatHTTPClient(&http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 7 * time.Second}})
		t.Cleanup(func() { SetChatHTTPClient(nil) })
		cli := New()
		if got := cli.ChatHTTP.Transport.(*http.Transport).ResponseHeaderTimeout; got != 7*time.Second {
			t.Errorf("首字节上限 = %v, want 7s", got)
		}
	})

	// nil = 还原出厂默认（与 SetHTTPClient 的 nil 语义对齐）。
	t.Run("nil 还原出厂默认", func(t *testing.T) {
		SetChatHTTPClient(&http.Client{Transport: &http.Transport{}})
		SetChatHTTPClient(nil)
		cli := New()
		if cli.ChatHTTP.Timeout != 0 {
			t.Errorf("还原后总超时 = %v, want 0", cli.ChatHTTP.Timeout)
		}
		if got := cli.ChatHTTP.Transport.(*http.Transport).ResponseHeaderTimeout; got != chatRequestTimeout {
			t.Errorf("还原后首字节上限 = %v, want %v", got, chatRequestTimeout)
		}
	})
}

// 手填 Client（绕过 New）时，对话链路**不得**回落到短请求 httpClient。
//
// chatHTTP() 曾回落 httpClient（Timeout: 30s），于是 `&Client{HTTP: x}` 这种
// 外部构造会让 P1-5 原样复发，且调用方看不出区别——只表现为长流偶发断在
// 30s。现在回落包级 chatHTTP（Timeout: 0）。
//
// 全仓当前无这种调用方（都走 New()），但 Client 字段是导出的 API：这是给
// 未来的外部宿主/后来者留的守卫，成本只有一次构造。
func TestBareClientChatFallsBackToChatClient(t *testing.T) {
	t.Cleanup(func() {
		SetHTTPClient(&http.Client{Timeout: requestTimeout})
		SetChatHTTPClient(nil)
	})
	// 让两条链路可区分：短请求 client 带 30s，对话恒 0。
	SetHTTPClient(&http.Client{Timeout: requestTimeout})
	SetChatHTTPClient(nil)

	bare := &Client{HTTP: httpClient} // 没填 ChatHTTP，模拟外部手填
	got := bare.chatHTTP()
	if got == httpClient {
		t.Fatalf("对话回落到短请求 httpClient（30s 总超时）——P1-5 在这条路径复发")
	}
	if got == nil {
		t.Fatal("chatHTTP() = nil")
	}
	if got.Timeout != 0 {
		t.Errorf("回落到的 client Timeout = %v, want 0", got.Timeout)
	}

	// 填了 ChatHTTP 就优先用它（不被回落干扰）。
	own := &http.Client{Timeout: 0}
	bare2 := &Client{HTTP: httpClient, ChatHTTP: own}
	if got2 := bare2.chatHTTP(); got2 != own {
		t.Errorf("ChatHTTP 非空时应优先用它，got = %p want %p", got2, own)
	}
}

// SetHTTPClient 注入缺 transport（或 transport 缺首字节上限）的 client 时，
// 对话链路同样要有界——与 SetChatHTTPClient 的兜底契约对称（omp 终审备注 A）。
// 原实现只搬 c.Transport：注入 {Timeout:30s}（Transport nil）会让对话落到
// DefaultTransport（首字节无上限），两条注入路径一条有界一条无界。
func TestSetHTTPClientChatPathAlsoBounded(t *testing.T) {
	t.Cleanup(func() { SetHTTPClient(&http.Client{Timeout: requestTimeout}) })

	// Transport 为 nil 的注入：对话链路必须拿到默认对话 transport（首字节 120s）。
	SetHTTPClient(&http.Client{Timeout: requestTimeout})
	cli := New()
	if cli.ChatHTTP.Timeout != 0 {
		t.Errorf("对话总超时 = %v, want 0", cli.ChatHTTP.Timeout)
	}
	tr, ok := cli.ChatHTTP.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("transport = %T, want *http.Transport", cli.ChatHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout != chatRequestTimeout {
		t.Errorf("首字节上限 = %v, want %v（0 = 对话流无限挂等）", tr.ResponseHeaderTimeout, chatRequestTimeout)
	}

	// 显式 transport 缺上限：Clone 后补，且注入方手里的 transport 不被就地改。
	mine := &http.Transport{MaxIdleConns: 7}
	SetHTTPClient(&http.Client{Timeout: requestTimeout, Transport: mine})
	cli = New()
	tr = cli.ChatHTTP.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != chatRequestTimeout {
		t.Errorf("首字节上限 = %v, want %v", tr.ResponseHeaderTimeout, chatRequestTimeout)
	}
	if mine.ResponseHeaderTimeout != 0 {
		t.Errorf("注入方的 transport 被就地改了（MaxIdleConns=7 那份），应 Clone 隔离")
	}
	// 短请求链路仍是注入的原 client。
	if New().HTTP.Timeout != requestTimeout {
		t.Errorf("短请求总超时 = %v, want %v", New().HTTP.Timeout, requestTimeout)
	}
}
