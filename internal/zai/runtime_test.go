package zai

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// mockUpstream 可编程的假上游：按脚本依次返回响应，并记录收到的请求。
type mockUpstream struct {
	server   *httptest.Server
	mu       chan struct{} // 串行化脚本推进（容量 1 的信号量）
	steps    []mockStep
	idx      int32
	requests []*http.Request
	bodies   []string
}

type mockStep struct {
	status  int
	body    string
	headers map[string]string
}

func newMockUpstream(t *testing.T, steps ...mockStep) *mockUpstream {
	m := &mockUpstream{steps: steps}
	m.mu = make(chan struct{}, 1)
	m.mu <- struct{}{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		<-m.mu
		m.requests = append(m.requests, r.Clone(context.Background()))
		m.bodies = append(m.bodies, string(raw))
		i := int(atomic.AddInt32(&m.idx, 1)) - 1
		m.mu <- struct{}{}
		if i >= len(m.steps) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		st := m.steps[i]
		for k, v := range st.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(st.status)
		_, _ = w.Write([]byte(st.body))
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockUpstream) lastRequest() *http.Request {
	<-m.mu
	defer func() { m.mu <- struct{}{} }()
	if len(m.requests) == 0 {
		return nil
	}
	return m.requests[len(m.requests)-1]
}

func (m *mockUpstream) count() int { return len(m.requests) }

// newTestClient 建一个指向 mock 上游的编排器。
func newTestClient(t *testing.T, accounts ...*Account) (*Client, *Store) {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if err := st.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	pool := NewPool(st, 2)
	return NewClient(pool, nil, nil), st
}

func TestDoSuccessOnPlanChannel(t *testing.T) {
	// Plan 通道需要验证码：用一个假求解器（无 node 则跳过）
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过 Plan 通道集成测试")
	}
	dir := t.TempDir()
	solver := filepath.Join(dir, "solver.js")
	if err := os.WriteFile(solver, []byte("console.log('VERIFY_PARAM='+'p'.repeat(260));"), 0o644); err != nil {
		t.Fatal(err)
	}

	up := newMockUpstream(t, mockStep{status: 200, body: `{"content":[{"type":"text","text":"hi"}]}`})
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	jwt := NewAccount("主号", "header."+b64("{\"sub\":\"u9\"}")+".sig")
	client, st := newTestClient(t, jwt)
	client.Captcha = NewCaptchaManager(SolverConfig{Command: node, Script: solver, Timeout: 10 * time.Second},
		func() bool { return true })

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("应成功建流: %v", err)
	}
	defer res.Resp.Body.Close()
	if !res.UsedPlan {
		t.Fatal("有求解器时应走 Plan 通道")
	}

	req := up.lastRequest()
	if got := req.Header.Get("X-Aliyun-Captcha-Verify-Param"); len(got) < 200 {
		t.Fatalf("Plan 通道应带验证码头，got %d 字符", len(got))
	}
	if req.Header.Get("Authorization") == "" || req.Header.Get("X-Device-Mid") == "" {
		t.Fatal("Plan 通道应带鉴权头与设备指纹头")
	}
	if got := st.Get(jwt.ID).Status; got != StatusActive {
		t.Fatalf("成功后状态 = %s, want active", got)
	}
}

func TestDoFallsBackWithoutSolver(t *testing.T) {
	up := newMockUpstream(t, mockStep{status: 200, body: `{"ok":true}`})
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	jwt := NewAccount("主号", "header."+b64("{\"sub\":\"u1\"}")+".sig")
	jwt.APIKey = "sk-fallback-key"
	client, _ := newTestClient(t, jwt) // Captcha=nil → 无求解器

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("应回退成功: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.UsedPlan {
		t.Fatal("无求解器时不应走 Plan 通道")
	}
	req := up.lastRequest()
	if req.Header.Get("x-api-key") != "sk-fallback-key" {
		t.Fatalf("回退通道应用 x-api-key，got %q", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("X-Aliyun-Captcha-Verify-Param") != "" {
		t.Fatal("回退通道不应带验证码头")
	}
}

func TestDoSwitchesAccountOnExhausted(t *testing.T) {
	up := newMockUpstream(t,
		mockStep{status: 402, body: `{"error":"quota exhausted"}`},
		mockStep{status: 200, body: `{"ok":true}`},
	)
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	a := NewAccount("号一", "sk-aaaaaaaaaaaa")
	b := NewAccount("号二", "sk-bbbbbbbbbbbb")
	client, st := newTestClient(t, a, b)

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("第二个账号应成功: %v", err)
	}
	defer res.Resp.Body.Close()

	// 402 的账号应被标记额度用完，且不再可选
	exhausted := st.Get(a.ID)
	if exhausted.Status != StatusExhausted {
		t.Fatalf("402 应标记 exhausted，got %s", exhausted.Status)
	}
	if exhausted.Selectable(time.Now()) {
		t.Fatal("额度用完的账号不应可选中")
	}
}

func TestDoDisablesAccountOnRiskControl(t *testing.T) {
	up := newMockUpstream(t,
		mockStep{status: 405, body: `{"code":3012,"msg":"unusual activity"}`},
		mockStep{status: 200, body: `{"ok":true}`},
	)
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	risky := NewAccount("风控号", "sk-risky-key-1234")
	safe := NewAccount("正常号", "sk-safe-key-5678")
	client, st := newTestClient(t, risky, safe)

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("应换号成功: %v", err)
	}
	defer res.Resp.Body.Close()

	got := st.Get(risky.ID)
	if got.Status != StatusDisabled {
		t.Fatalf("3012 应禁用账号，got %s", got.Status)
	}
	if got.RiskStrikes != 1 {
		t.Fatalf("风控计数应 +1，got %d", got.RiskStrikes)
	}
	// 人工恢复前不得被选中
	if got.Selectable(time.Now()) {
		t.Fatal("风控禁用的账号不应可选中")
	}
}

func TestDoRetriesSameAccountOnRateLimit(t *testing.T) {
	up := newMockUpstream(t,
		mockStep{status: 429, body: `slow down`, headers: map[string]string{"Retry-After": "1"}},
		mockStep{status: 200, body: `{"ok":true}`},
	)
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	acc := NewAccount("限流号", "sk-rate-limited-1")
	client, st := newTestClient(t, acc)
	client.RateLimitWait = 3 * time.Second

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("限流后原地重试应成功: %v", err)
	}
	defer res.Resp.Body.Close()

	if up.count() != 2 {
		t.Fatalf("应重试同一账号一次，实际请求 %d 次", up.count())
	}
	// 关键：限流不冷却账号
	got := st.Get(acc.ID)
	if got.Status != StatusActive {
		t.Fatalf("429 不应冷却账号，got %s", got.Status)
	}
}

func TestDoInvalidatesOn401(t *testing.T) {
	up := newMockUpstream(t,
		mockStep{status: 401, body: `{"error":"unauthorized"}`},
		mockStep{status: 200, body: `{"ok":true}`},
	)
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	bad := NewAccount("失效号", "sk-invalid-key-1")
	good := NewAccount("好号", "sk-good-key-2")
	client, st := newTestClient(t, bad, good)

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("应换号成功: %v", err)
	}
	defer res.Resp.Body.Close()
	if got := st.Get(bad.ID).Status; got != StatusInvalid {
		t.Fatalf("401 应标记 invalid，got %s", got)
	}
}

func TestDoCaptchaChallengeRetriesSameAccount(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node")
	}
	dir := t.TempDir()
	solver := filepath.Join(dir, "solver.js")
	_ = os.WriteFile(solver, []byte("console.log('VERIFY_PARAM='+'q'.repeat(260));"), 0o644)

	up := newMockUpstream(t,
		mockStep{status: 400, body: `{"code":3007,"msg":"captcha expired"}`},
		mockStep{status: 200, body: `{"ok":true}`},
	)
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	jwt := NewAccount("主号", "header."+b64("{\"sub\":\"u2\"}")+".sig")
	client, st := newTestClient(t, jwt)
	captcha := NewCaptchaManager(SolverConfig{Command: node, Script: solver, Timeout: 10 * time.Second},
		func() bool { return true })
	client.Captcha = captcha

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatalf("验证码挑战后应重试成功: %v", err)
	}
	defer res.Resp.Body.Close()

	if got := st.Get(jwt.ID).Status; got != StatusActive {
		t.Fatalf("验证码挑战不是账号的错，状态应保持 active，got %s", got)
	}
	if up.count() != 2 {
		t.Fatalf("应原地重试一次，实际 %d 次", up.count())
	}
}

func TestDoReturnsErrorWhenAllDisabled(t *testing.T) {
	up := newMockUpstream(t, mockStep{status: 200, body: `{}`})
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	a := NewAccount("停用号", "sk-disabled-key")
	a.Enabled = false
	client, _ := newTestClient(t, a)

	if _, err := client.Do(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("全部不可用时应返回错误")
	}
}

// b64 生成 JWT payload 段（测试夹具用）。
func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
