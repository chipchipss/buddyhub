package copilot

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveDeviceFlow 对真实 GitHub 跑一次设备码申请（只申请、不轮询、不授权）。
//
// 默认跳过——CI 与离线环境不该依赖外网。需要时显式开启：
//
//	COPILOT_LIVE=1 HTTPS_PROXY=http://127.0.0.1:2080 go test ./internal/extprovider/copilot/ -run Live -v
//
// 注意：github.com（网页域）在国内网络常需代理，而 api.github.com 往往可直连；
// 设备流两个端点都在 github.com 上，因此这条通道通常必须配 HTTPS_PROXY。
func TestLiveDeviceFlow(t *testing.T) {
	if os.Getenv("COPILOT_LIVE") == "" {
		t.Skip("未设置 COPILOT_LIVE=1，跳过真实 GitHub 冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	f, err := StartDeviceFlow(ctx)
	if err != nil {
		t.Fatalf("真实设备码申请失败（检查网络/代理 HTTPS_PROXY）: %v", err)
	}
	if f.UserCode == "" || f.DeviceCode == "" {
		t.Fatalf("回执不完整: %+v", f)
	}
	if !strings.Contains(f.VerificationURI, "github.com") {
		t.Fatalf("verification_uri 异常: %s", f.VerificationURI)
	}
	t.Logf("设备码 %s → %s（%ds 内有效，轮询间隔 %ds）",
		f.UserCode, f.VerificationURI, f.ExpiresIn, f.Interval)

	// 未授权时轮询必须返回 pending（而不是报错或 Done）——验证状态机语义。
	// 首次轮询实测会命中 incorrect_device_code（设备码尚未生效），Poll 内部容忍之。
	for i := 1; i <= 3; i++ {
		res, err := f.Poll(ctx)
		if err != nil {
			t.Fatalf("第 %d 次轮询失败: %v", i, err)
		}
		t.Logf("轮询 %d: Done=%v Error=%q", i, res.Done, res.Error)
		if res.Done {
			t.Fatalf("未授权却 Done 了: %+v", res)
		}
		if res.Error == "" {
			return // 落到 pending，符合预期
		}
		time.Sleep(time.Duration(f.Interval) * time.Second)
	}
	t.Fatal("连续 3 次轮询都未落到 pending")
}

// SetProxy 只改本通道的传输层，且必须拒绝坏地址（否则会静默直连，
// 用户以为配了代理却还是连不上 github.com）。
func TestSetProxy(t *testing.T) {
	prev := httpClient
	t.Cleanup(func() { httpClient = prev })

	for _, bad := range []string{"://nope", "127.0.0.1:2080"} {
		// 缺 scheme 的裸 host:port 也要拦住——url.Parse 会把它当 path，
		// 代理实际不生效，用户会以为配了。
		if err := SetProxy(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if err := SetProxy("http://127.0.0.1:2080"); err != nil {
		t.Fatalf("合法代理被拒: %v", err)
	}
	// 配了代理时外层是「代理优先 + 直连兜底」的包装，真正的代理在 primary 上。
	ft, ok := httpClient.Transport.(*fallbackTransport)
	if !ok {
		t.Fatal("配了代理就该包一层直连兜底")
	}
	tr := ft.primary
	if tr.Proxy == nil {
		t.Fatal("代理未生效")
	}
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "github.com"}})
	if err != nil || u == nil || u.Host != "127.0.0.1:2080" {
		t.Fatalf("代理地址不对: %v %v", u, err)
	}
	// 兜底那条必须是直连（不继承 primary 的代理，否则兜底等于没兜）
	if ft.direct == tr {
		t.Fatal("兜底传输层与代理传输层是同一个")
	}
	if got, _ := ft.direct.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "github.com"}}); got != nil {
		// ProxyFromEnvironment 在测试环境通常返回 nil；非 nil 说明继承了显式代理
		t.Fatalf("兜底传输层带着代理: %v", got)
	}

	// 空串 = 回到跟随环境变量
	if err := SetProxy(""); err != nil {
		t.Fatalf("空代理应合法: %v", err)
	}
	if tr2 := httpClient.Transport.(*http.Transport); tr2.Proxy == nil {
		t.Fatal("空串应回落到 ProxyFromEnvironment")
	}
}

/* ── 代理掉线兜底 ──────────────────────────────────────────────── */

// deadProxyURL 返回一个**确定没人监听**的代理地址。
// 先占端口再关掉：操作系统刚释放的端口不会立刻被别人抢走。
func deadProxyURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

// 代理进程没开（用户关了代理软件）时必须直连兜底，而不是让整条通道瘫掉。
// 这是实测踩过的坑：配了 schedule.copilot.proxy 但代理没运行，
// 设备码申请直接死在 "proxyconnect ... actively refused"。
func TestFallsBackToDirectWhenProxyIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	primary := newTransport()
	u, _ := url.Parse(deadProxyURL(t))
	primary.Proxy = http.ProxyURL(u)

	direct := newTransport()
	direct.Proxy = nil // 直连兜底不该再走代理

	ft := &fallbackTransport{primary: primary, direct: direct}
	client := &http.Client{Transport: ft, Timeout: 10 * time.Second}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("代理挂了应直连兜底，却失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("兜底后状态码 = %d", resp.StatusCode)
	}
}

// 代理正常时不该触发兜底：把兜底那条指到死代理上，一旦误触发就会失败。
func TestNoFallbackWhenProxyWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	// primary 直连（等价于「代理可用」——请求正常完成）
	primary := newTransport()
	primary.Proxy = nil

	// direct 指向死代理：只有错误地走了兜底才会用到它
	direct := newTransport()
	u, _ := url.Parse(deadProxyURL(t))
	direct.Proxy = http.ProxyURL(u)

	ft := &fallbackTransport{primary: primary, direct: direct}
	client := &http.Client{Transport: ft, Timeout: 10 * time.Second}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("代理可用时不该兜底: %v", err)
	}
	resp.Body.Close()
}

func TestIsProxyConnError(t *testing.T) {
	// Go 连不上代理时的真实形状：OpError.Op == "proxyconnect"
	refused := &url.Error{Op: "Post", URL: "https://github.com/", Err: &net.OpError{
		Op: "proxyconnect", Net: "tcp",
		Err: errors.New("dial tcp 127.0.0.1:2080: connectex: No connection could be made"),
	}}
	if !isProxyConnError(refused) {
		t.Error("proxyconnect 错误应被识别")
	}
	// 普通连接超时不是代理问题，不该兜底（兜底只会再慢一轮）
	timeout := &url.Error{Op: "Post", URL: "https://github.com/", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: errors.New("i/o timeout"),
	}}
	if isProxyConnError(timeout) {
		t.Error("普通超时不该被当成代理故障")
	}
}

// 真实故障路径是 POST（设备码申请）：请求体必须被完整重放，
// 否则兜底会发出一个空体请求、上游回 400，看起来像「参数错误」而不是代理问题。
func TestFallbackReplaysPostBody(t *testing.T) {
	const payload = `{"client_id":"Iv1.b507a08c87ecfe98","scope":"read:user"}`

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		if r.Method != http.MethodPost {
			t.Errorf("兜底后方法变成了 %s", r.Method)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	primary := newTransport()
	u, _ := url.Parse(deadProxyURL(t))
	primary.Proxy = http.ProxyURL(u)
	direct := newTransport()
	direct.Proxy = nil

	client := &http.Client{Transport: &fallbackTransport{primary: primary, direct: direct}, Timeout: 10 * time.Second}
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST 兜底失败: %v", err)
	}
	resp.Body.Close()
	if gotBody != payload {
		t.Fatalf("兜底重放的请求体不对:\n  got  %s\n  want %s", gotBody, payload)
	}
}
