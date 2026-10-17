package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── R-1：h.last*Err 跨请求字段收敛为请求级局部量后的并发回归 ──────────────
 *
 * 迁移前，15 个 last<Provider>Err 挂在 *Handler 单例上，两条并发请求同时走
 * 同一条桥接时：
 *   1. 每次写都是一次数据竞争（`go test -race` 稳定复现，基线见
 *      TestClineRefreshIsSingleFlight / TestAutoClawRefreshIsSingleFlight）；
 *   2. 更要紧的是**串味**：B 的失败原因会盖掉 A 的，A 那条 503 的 message
 *      里会出现 B 的上游错误——用户拿到的失败原因属于另一个人。
 *
 * 迁移后每条桥接返回自己的 bridgeOutcome（含本次 lastErr），dispatch 只做
 * 转发。本用例把两件事都钉住：-race 下无 WARNING，且各自的响应只含自己的
 * 错误串。
 */

// clineReasonUpstream 按请求体里的会话键返回不同失败原因的上游 stub。
//
// keyA / keyB 是两条互不相同的 marker（本例用「请求级随机 conversation_id」），
// 各自绑一条上游错误文案。并发时两个 goroutine 打的是同一个 Handler、
// 同一个账号、同一条桥接——与线上同通道两并发请求同形。
type clineReasonUpstream struct {
	mu       sync.Mutex
	reasonA  string
	reasonB  string
	seenA    int
	seenB    int
	delayA   bool
	delayB   bool
	startedA chan struct{}
	startedB chan struct{}
	release  chan struct{}
}

func (u *clineReasonUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		kind := "other"
		switch {
		case strings.Contains(r.URL.Path, "/auth/refresh"):
			kind = "refresh"
		case strings.Contains(r.URL.Path, "/chat/completions"):
			kind = "chat"
		}
		body := ""
		if r.Body != nil {
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			body = string(raw[:n])
		}
		reason := ""
		switch {
		case strings.Contains(body, "markerA"):
			u.mu.Lock()
			u.seenA++
			reason = u.reasonA
			wait := u.delayA
			started := u.startedA
			u.mu.Unlock()
			if wait && started != nil {
				close(started)
				if u.release != nil {
					<-u.release
				}
			}
		case strings.Contains(body, "markerB"):
			u.mu.Lock()
			u.seenB++
			reason = u.reasonB
			wait := u.delayB
			started := u.startedB
			u.mu.Unlock()
			if wait && started != nil {
				close(started)
				if u.release != nil {
					<-u.release
				}
			}
		}
		if kind == "refresh" {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       http.NoBody,
			}, nil
		}
		return &http.Response{
			StatusCode: 500,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       nopCloser(`{"error":{"message":"` + reason + `"}}`),
		}, nil
	})}
}

// nopCloser 把字符串包成 ReadCloser（避免每个用例都 import io/strings 两次）。
func nopCloser(s string) *stringReader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, ioEOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func (r *stringReader) Close() error { return nil }

// ioEOF 独立符号，避免与测试包别处已 import 的 io 冲突。
var ioEOF = errEOF{}

type errEOF struct{}

func (errEOF) Error() string { return "EOF" }

// newClineRaceHandler 单账号 cline handler（凭据永不过期，绝不触发续期路径）。
func newClineRaceHandler(t *testing.T, up *clineReasonUpstream) *Handler {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	raw, err := json.Marshal(cline.Credential{AccessToken: "workos:t", ExpiresAt: clineFuture()})
	if err != nil {
		t.Fatalf("marshal cred: %v", err)
	}
	if err := mgr.Add(extstore.PCline, "acct0", "acct0", raw); err != nil {
		t.Fatalf("add: %v", err)
	}
	cline.SetHTTPClient(up.client())
	t.Cleanup(func() { cline.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	h.ExtSetManager(&fakeExtMgr{})
	return h
}

// postClineBody 发一条带指定 marker 的 cline 请求，返回 recorder。
func postClineBody(t *testing.T, h *Handler, marker string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cline:cline-pass/glm-5.3","stream":true,"`+
			`conversation_id":"`+marker+`","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestClineLastErrRaceConcurrentRequestsDoNotCrossContaminate
// 同通道两并发请求，各自的上游失败原因不得串到对方的 503 文案里。
//
// 这是 R-1 的核心红线之一（「上一位用户的失败原因不挂进这一次」）：
// 迁移前 A 的 message 里有概率出现 B 的 reasonB，迁移后必须彻底不可能。
// -race 下同时兜住数据竞争（迁移前这里会稳定 WARNING）。
func TestClineLastErrRaceConcurrentRequestsDoNotCrossContaminate(t *testing.T) {
	const reasonA = "ERR_A_MARKER_reason_from_A_upstream"
	const reasonB = "ERR_B_MARKER_reason_from_B_upstream"

	up := &clineReasonUpstream{
		reasonA:  reasonA,
		reasonB:  reasonB,
		startedA: make(chan struct{}),
		startedB: make(chan struct{}),
		release:  make(chan struct{}),
		delayA:   true,
		delayB:   true,
	}
	h := newClineRaceHandler(t, up)

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i, marker := range []string{"markerA", "markerB"} {
		wg.Add(1)
		go func(idx int, m string) {
			defer wg.Done()
			results[idx] = postClineBody(t, h, m)
		}(i, marker)
	}
	// 两个请求都进到桥接、都碰过上游之后再放行，把竞争窗口撑到最大。
	<-up.startedA
	<-up.startedB
	close(up.release)
	wg.Wait()

	if results[0] == nil || results[1] == nil {
		t.Fatalf("有请求没跑完：%v", results)
	}
	for i, rec := range results {
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("请求 %d 状态码 = %d，期望 503: %s", i, rec.Code, rec.Body.String())
		}
	}
	bodyA, bodyB := results[0].Body.String(), results[1].Body.String()

	// 各自的失败原因必须出现在自己的响应里（说明原因没丢）。
	if !strings.Contains(bodyA, reasonA) {
		t.Errorf("A 的 503 没带出自己的失败原因: %s", bodyA)
	}
	if !strings.Contains(bodyB, reasonB) {
		t.Errorf("B 的 503 没带出自己的失败原因: %s", bodyB)
	}
	// 红线：不得出现对方的错误串。
	if strings.Contains(bodyA, reasonB) {
		t.Errorf("A 的 503 串进了 B 的失败原因（跨请求污染）: %s", bodyA)
	}
	if strings.Contains(bodyB, reasonA) {
		t.Errorf("B 的 503 串进了 A 的失败原因（跨请求污染）: %s", bodyB)
	}
	// 文案口径也不能串：A/B 都是「账号都试过了且全失败」，不是「没有可用账号」。
	for i, body := range []string{bodyA, bodyB} {
		if !strings.Contains(body, "no_cline_account") {
			t.Errorf("请求 %d 错误码应为 no_cline_account: %s", i, body)
		}
		if strings.Contains(body, "没有可用的 Cline 账号") {
			t.Errorf("请求 %d 池里有账号，不该说「没有可用账号」: %s", i, body)
		}
	}
}

// TestClineLastErrRaceSequentialRequestsDoNotLeakStaleReason
// 串行版：先失败一次留下原因，再发一次「池里没账号」的请求，
// 第二次的 503 里不许出现第一次的原因（「本请求从零开始记」）。
func TestClineLastErrRaceSequentialRequestsDoNotLeakStaleReason(t *testing.T) {
	const reasonA = "ERR_A_MARKER_reason_from_A_upstream"

	up := &clineReasonUpstream{reasonA: reasonA, reasonB: "unused"}
	h := newClineRaceHandler(t, up)

	first := postClineBody(t, h, "markerA")
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("第一次状态码 = %d，期望 503: %s", first.Code, first.Body.String())
	}
	if !strings.Contains(first.Body.String(), reasonA) {
		t.Fatalf("第一次没带出失败原因: %s", first.Body.String())
	}

	// 把账号全停用 → 桥接一条上游都不碰（lastErr 必须是空串）。
	// 复用同一 handler：若失败原因还挂在共享字段上，这里会读到上一次的 reasonA。
	h.cfg.ExtAccounts = func() []*extstore.ExtAccount { return nil }

	second := postClineBody(t, h, "markerA")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("第二次状态码 = %d，期望 503: %s", second.Code, second.Body.String())
	}
	if strings.Contains(second.Body.String(), reasonA) {
		t.Fatalf("陈旧原因泄漏进第二次请求: %s", second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "没有可用的 Cline 账号") {
		t.Fatalf("一次上游都没碰过时应说「没有可用账号」: %s", second.Body.String())
	}
}

// TestClineLastErrRaceUnservedPathCarriesReason
// 红线一：dispatch 侧读未 served 路径的 lastErr，必须读到**本次**的原因，
// 不是零值。这条直接锁 codex/zai 那类「桥接返回未 served、dispatch 拼文案」
// 的形状（用 cline 复现同一形状，避免依赖本机 ~/.codex 凭据）。
func TestClineLastErrRaceUnservedPathCarriesReason(t *testing.T) {
	const reasonA = "ERR_A_MARKER_reason_from_A_upstream"
	up := &clineReasonUpstream{reasonA: reasonA, reasonB: "unused"}
	h := newClineRaceHandler(t, up)

	out := h.tryBridge(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		[]byte(`{"model":"cline:cline-pass/glm-5.3","conversation_id":"markerA","messages":[]}`), "cline:cline-pass/glm-5.3")

	if !out.matched {
		t.Fatalf("cline 前缀应被认下: %+v", out)
	}
	if out.served {
		t.Fatalf("账号全失败时不该 served: %+v", out)
	}
	if out.code != "no_cline_account" {
		t.Errorf("code = %q，期望 no_cline_account", out.code)
	}
	if !strings.Contains(out.lastErr, reasonA) {
		t.Errorf("lastErr = %q，期望含本次的原因 %q（未 served 路径读到零值即红线一被破）", out.lastErr, reasonA)
	}
	if out.lastErr == "" {
		t.Errorf("lastErr 不应为空（未 served 路径读到零值即红线一被破）")
	}
	if !strings.Contains(out.detail, reasonA) {
		t.Errorf("detail 未带出本次原因: %s", out.detail)
	}
}

// TestClineLastErrRaceEarlyServedPathKeepsReason
// 红线三：提前 return true（已写响应）的路径，迁移前也写 h.lastXErr——
// 现在那份原因必须跟着 outcome 回来，不能丢。
//
// 用 copilot 的「模型不在目录内」路径复现（它 404 后 return served，
// 迁移前那一步写的是 h.lastCopilotErr = reason）。
func TestClineLastErrRaceEarlyServedPathKeepsReason(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "chat" {
			return 400, "application/json",
				`{"error":{"message":"The requested model is not supported.","code":"model_not_supported","param":"model","type":"invalid_request_error"}}`
		}
		return 200, "application/json", `{}`
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "g", CopilotToken: "c", ExpiresAt: future()})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	out := h.tryBridge(rec, req, []byte(`{"model":"copilot:gpt-4o","messages":[]}`), "copilot:gpt-4o")

	if !out.matched || !out.served {
		t.Fatalf("模型不存在应 matched+served: %+v", out)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404: %s", rec.Code, rec.Body.String())
	}
	if out.lastErr == "" {
		t.Fatalf("提前 served 路径把 reason 丢了: %+v", out)
	}
	if !strings.Contains(out.lastErr, "gpt-4o") {
		t.Errorf("lastErr 未点名模型: %q", out.lastErr)
	}
}

// TestBridgeMissBuildsDetailFromRequestReason
// dispatch 的兜底组装（bridgeMiss）语义：lastErr 空 ⇒ 「没有可用账号」；
// 非空 ⇒ 「账号均已尝试并失败」。这是 bridgeDetail 的既有口径，
// 收敛后由请求级 lastErr 驱动，逐字不变。
func TestBridgeMissBuildsDetailFromRequestReason(t *testing.T) {
	empty := bridgeMiss("no_x_account", "没有可用的 X 账号", "X", "")
	if empty.detail != "没有可用的 X 账号" {
		t.Errorf("空原因时 detail = %q", empty.detail)
	}
	if empty.lastErr != "" {
		t.Errorf("空原因时 lastErr 应为空: %q", empty.lastErr)
	}
	withReason := bridgeMiss("no_x_account", "没有可用的 X 账号", "X", "上游 HTTP 429：quota")
	if !strings.Contains(withReason.detail, "池内账号均已尝试并失败") ||
		!strings.Contains(withReason.detail, "quota") {
		t.Errorf("有原因时 detail = %q", withReason.detail)
	}
	if withReason.lastErr != "上游 HTTP 429：quota" {
		t.Errorf("lastErr 未透传: %q", withReason.lastErr)
	}
}
