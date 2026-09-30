package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type clineUpstream struct {
	mu      sync.Mutex
	calls   []string // "kind|model|auth"
	respond func(kind string, idx int) (int, string, string)
}

func (u *clineUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		kind := "other"
		switch {
		case strings.Contains(r.URL.Path, "/auth/refresh"):
			kind = "refresh"
		case strings.Contains(r.URL.Path, "/chat/completions"):
			kind = "chat"
		}
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &peek)

		u.mu.Lock()
		idx := 0
		for _, c := range u.calls {
			if strings.HasPrefix(c, kind+"|") {
				idx++
			}
		}
		u.calls = append(u.calls, kind+"|"+peek.Model+"|"+r.Header.Get("Authorization")+
			"|ctype="+r.Header.Get("X-CLIENT-TYPE"))
		u.mu.Unlock()

		status, ct, body := u.respond(kind, idx)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func (u *clineUpstream) count(kind string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, c := range u.calls {
		if strings.HasPrefix(c, kind+"|") {
			n++
		}
	}
	return n
}

func (u *clineUpstream) last(kind string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(u.calls[i], kind+"|") {
			return u.calls[i]
		}
	}
	return ""
}

func newClineHandler(t *testing.T, up *clineUpstream, creds ...cline.Credential) (*Handler, *fakeExtMgr, *extstore.Manager) {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal cred: %v", err)
		}
		id := "acct" + string(rune('0'+i))
		if c.Email != "" {
			id = c.Email
		}
		if err := mgr.Add(extstore.PCline, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	cline.SetHTTPClient(up.client())
	t.Cleanup(func() { cline.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	fm := &fakeExtMgr{}
	h.ExtSetManager(fm)
	return h, fm, mgr
}

func postCline(t *testing.T, h *Handler, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func clineFuture() int64 { return time.Now().Add(2 * time.Hour).UnixMilli() }

/* ── 用例 ────────────────────────────────────────────────────── */

func TestClineNoAccounts(t *testing.T) {
	up := &clineUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, _ := newClineHandler(t, up)

	w := postCline(t, h, "cline:cline-free/deepseek-v4.1-flash", `{"model":"cline:cline-free/deepseek-v4.1-flash","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_cline_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	if up.count("chat") != 0 {
		t.Fatal("无账号时不该触达上游")
	}
}

func TestClineHappyPathSSE(t *testing.T) {
	up := &clineUpstream{respond: func(kind string, _ int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{
		AccessToken: "workos:tok", RefreshToken: "rt", ExpiresAt: clineFuture(),
	})

	w := postCline(t, h, "cline:cline-free/deepseek-v4.1-flash",
		`{"model":"cline:cline-free/deepseek-v4.1-flash","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("SSE 未透传完整: %s", w.Body.String())
	}
	// 未到期不该续期
	if up.count("refresh") != 0 {
		t.Fatal("token 未过期时不应续期")
	}
}

// 计费池前缀是**上游**的选择器：只剥 `cline:` 网关前缀，池前缀必须原样带上。
func TestClineKeepsPoolPrefixUpstream(t *testing.T) {
	up := &clineUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{
		AccessToken: "workos:tok", ExpiresAt: clineFuture(),
	})

	for _, model := range []string{
		"cline:cline-free/deepseek-v4.1-flash",
		"cline:cline-pass/glm-5.3",
		"cline:cline-cloud/kimi-k3",
	} {
		postCline(t, h, model, `{"model":"`+model+`","stream":true,"messages":[]}`)
	}

	want := []string{
		"cline-free/deepseek-v4.1-flash",
		"cline-pass/glm-5.3",
		"cline-cloud/kimi-k3",
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	var got []string
	for _, c := range up.calls {
		if strings.HasPrefix(c, "chat|") {
			got = append(got, strings.Split(c, "|")[1])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("chat 调用数 = %d，期望 %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个出站 model = %q，期望 %q（池前缀被误剥？）", i, got[i], want[i])
		}
	}
}

// 产品面标识头是免费池能不能用的关键，转发路径上不能丢。
func TestClineSendsProductHeader(t *testing.T) {
	up := &clineUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{AccessToken: "workos:tok", ExpiresAt: clineFuture()})

	postCline(t, h, "cline:cline-free/x", `{"model":"cline:cline-free/x","stream":true,"messages":[]}`)
	line := up.last("chat")
	if !strings.Contains(line, "ctype=cline-sdk") {
		t.Fatalf("转发丢了产品面标识头（免费池会 403）: %s", line)
	}
	if !strings.Contains(line, "|Bearer workos:tok|") {
		t.Fatalf("鉴权头不对: %s", line)
	}
}

func TestClineAutoRefreshBeforeExpiry(t *testing.T) {
	up := &clineUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "refresh" {
			return 200, "application/json",
				`{"data":{"accessToken":"fresh_bare","refreshToken":"rt2","expiresAt":"2030-01-01T00:00:00Z"},"success":true}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, fm, _ := newClineHandler(t, up, cline.Credential{
		AccessToken: "workos:stale", RefreshToken: "rt1",
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), // 临期
	})

	w := postCline(t, h, "cline:cline-free/x", `{"model":"cline:cline-free/x","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if up.count("refresh") != 1 {
		t.Fatalf("应续期 1 次，实际 %d", up.count("refresh"))
	}
	// 续期结果必须回写，否则每个请求都要续一次
	var got cline.Credential
	rawCred := fm.getRaw(extstore.PCline, "acct0")
	if len(rawCred) == 0 || json.Unmarshal(rawCred, &got) != nil {
		t.Fatalf("凭据未回写: %s", rawCred)
	}
	if got.AccessToken != "workos:fresh_bare" {
		t.Fatalf("续期结果未补前缀: %q", got.AccessToken)
	}
	if !strings.Contains(up.last("chat"), "|Bearer workos:fresh_bare|") {
		t.Fatalf("对话未用新 token: %s", up.last("chat"))
	}
}

// 本通道最容易出事的点：refresh_token 是一次性轮换语义，
// 并发请求各自去续期会互相作废 → 401 → 用户被踢下线。必须单飞。
func TestClineRefreshIsSingleFlight(t *testing.T) {
	var refreshCalls int32
	up := &clineUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			// 模拟真实网络耗时，制造并发窗口
			time.Sleep(120 * time.Millisecond)
			return 200, "application/json",
				`{"data":{"accessToken":"fresh_bare","refreshToken":"rt2","expiresAt":"2030-01-01T00:00:00Z"},"success":true}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{
		AccessToken: "workos:stale", RefreshToken: "rt1",
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
	})

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postCline(t, h, "cline:cline-free/x", `{"model":"cline:cline-free/x","stream":true,"messages":[]}`)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("%d 个并发请求触发了 %d 次续期，期望 1 次（并发续期会互相作废 refresh_token）", n, got)
	}
	// 每个请求都该拿到成功的流
	if up.count("chat") != n {
		t.Fatalf("chat 调用数 = %d，期望 %d", up.count("chat"), n)
	}
}

func TestClineForcedRefreshOn401(t *testing.T) {
	up := &clineUpstream{respond: func(kind string, idx int) (int, string, string) {
		if kind == "refresh" {
			return 200, "application/json",
				`{"data":{"accessToken":"retry_tok","refreshToken":"rt2","expiresAt":"2030-01-01T00:00:00Z"},"success":true}`
		}
		if idx == 0 {
			return 401, "application/json", `{"error":"Unauthorized: re-authenticate"}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{
		AccessToken: "workos:tok", RefreshToken: "rt1", ExpiresAt: clineFuture(),
	})

	w := postCline(t, h, "cline:cline-free/x", `{"model":"cline:cline-free/x","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("401 后应续期重试成功，状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if up.count("chat") != 2 || up.count("refresh") != 1 {
		t.Fatalf("chat=%d refresh=%d，期望 2/1", up.count("chat"), up.count("refresh"))
	}
}

// 非流式上游包了 {"data":…,"success":true} 信封，必须取出内层。
func TestClineUnwrapsNonStreamEnvelope(t *testing.T) {
	up := &clineUpstream{respond: func(string, int) (int, string, string) {
		return 200, "application/json",
			`{"data":{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"hi"}}]},"success":true}`
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{AccessToken: "workos:t", ExpiresAt: clineFuture()})

	w := postCline(t, h, "cline:cline-free/x", `{"model":"cline:cline-free/x","messages":[]}`)
	body := w.Body.String()
	if strings.Contains(body, `"success"`) {
		t.Fatalf("信封未剥掉: %s", body)
	}
	var doc struct {
		ID      string `json:"id"`
		Choices []any  `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("体不是合法 chat.completion: %v (%s)", err, body)
	}
	if doc.ID != "chatcmpl-1" || len(doc.Choices) != 1 {
		t.Fatalf("内层内容不对: %s", body)
	}
}

func TestCline403Surfaced(t *testing.T) {
	up := &clineUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "chat" {
			return 403, "application/json",
				`{"error":{"code":"ENTITLEMENT_ERROR","message":"not subscribed to required model plan"}}`
		}
		return 200, "application/json", `{}`
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{AccessToken: "workos:t", ExpiresAt: clineFuture()})

	w := postCline(t, h, "cline:cline-pass/glm-5.3", `{"model":"cline:cline-pass/glm-5.3","stream":true,"messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", w.Code)
	}
	// 403 的原因必须透出来，否则用户不知道是没订阅
	if !strings.Contains(w.Body.String(), "not subscribed") {
		t.Fatalf("未带出上游原因: %s", w.Body.String())
	}
}

func TestClineDisabledAccountSkipped(t *testing.T) {
	up := &clineUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, mgr := newClineHandler(t, up, cline.Credential{AccessToken: "workos:t", ExpiresAt: clineFuture()})

	accts := h.cfg.ExtAccounts()
	if len(accts) != 1 {
		t.Fatalf("前置条件：应有 1 个账号，得到 %d", len(accts))
	}
	if err := mgr.SetDisabled(extstore.PCline, accts[0].ID, true); err != nil {
		t.Fatalf("停用: %v", err)
	}
	w := postCline(t, h, "cline:cline-free/x", `{"model":"cline:cline-free/x","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable || up.count("chat") != 0 {
		t.Fatalf("停用账号仍被使用: code=%d chat=%d", w.Code, up.count("chat"))
	}
}

func TestPlatformOfCline(t *testing.T) {
	if got := PlatformOf("cline:cline-free/x"); got != "cline" {
		t.Fatalf("PlatformOf = %s，期望 cline", got)
	}
}

func TestClineModelListedInModelsEndpoint(t *testing.T) {
	up := &clineUpstream{respond: func(kind string, _ int) (int, string, string) {
		return 200, "application/json", `{"recommended":[],"free":[{"id":"cline-free/deepseek-v4.1-flash","name":"DeepSeek"}],` +
			`"clinePass":[{"id":"cline-pass/glm-5.3","name":"GLM"}],"clineCloud":[]}`
	}}
	h, _, _ := newClineHandler(t, up, cline.Credential{AccessToken: "workos:t", ExpiresAt: clineFuture()})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"cline:cline-free/deepseek-v4.1-flash", "cline:cline-pass/glm-5.3"} {
		if !strings.Contains(body, want) {
			t.Errorf("模型列表缺少 %s", want)
		}
	}
}
