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

	"github.com/chipchipss/buddyhub/internal/extprovider/autoclaw"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type autoclawUpstream struct {
	mu      sync.Mutex
	calls   []string // "kind|host|model|auth"
	respond func(kind string, idx int) (int, string, string)
}

func (u *autoclawUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		kind := "other"
		switch {
		case strings.Contains(r.URL.Path, "/userapi/v1/refresh"),
			strings.Contains(r.URL.Path, "/userapi/v1/agent-refresh"):
			kind = "refresh"
		case strings.HasSuffix(r.URL.Path, "/proxy/autoclaw"):
			kind = "chat"
		case strings.HasSuffix(r.URL.Path, "-model-config"):
			kind = "models"
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
		u.calls = append(u.calls, kind+"|"+r.Host+"|"+peek.Model+"|"+r.Header.Get("X-Authorization"))
		u.mu.Unlock()

		status, ct, body := u.respond(kind, idx)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func (u *autoclawUpstream) count(kind string) int {
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

func (u *autoclawUpstream) hostsOf(kind string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, c := range u.calls {
		if strings.HasPrefix(c, kind+"|") {
			out = append(out, strings.Split(c, "|")[1])
		}
	}
	return out
}

func newAutoClawHandler(t *testing.T, up *autoclawUpstream, creds ...autoclaw.Credential) (*Handler, *fakeExtMgr, *extstore.Manager) {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal cred: %v", err)
		}
		id := "acct" + string(rune('0'+i))
		if c.UserID != "" {
			id = c.UserID
		}
		if err := mgr.Add(extstore.PAutoClaw, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	autoclaw.SetHTTPClient(up.client())
	t.Cleanup(func() { autoclaw.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	fm := &fakeExtMgr{}
	h.ExtSetManager(fm)
	return h, fm, mgr
}

func postAutoClaw(t *testing.T, h *Handler, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func acFuture() int64 { return time.Now().Add(2 * time.Hour).UnixMilli() }

/* ── 用例 ────────────────────────────────────────────────────── */

func TestAutoClawNoAccounts(t *testing.T) {
	up := &autoclawUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, _ := newAutoClawHandler(t, up)

	w := postAutoClaw(t, h, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_autoclaw_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	if up.count("chat") != 0 {
		t.Fatal("无账号时不该触达上游")
	}
}

func TestAutoClawHappyPathSSE(t *testing.T) {
	up := &autoclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "tok", ExpiresAt: acFuture(),
	})

	w := postAutoClaw(t, h, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("SSE 未透传完整: %s", w.Body.String())
	}
	if up.count("refresh") != 0 {
		t.Fatal("token 未过期时不应续期")
	}
}

func TestAutoClawRewritesModelPrefix(t *testing.T) {
	up := &autoclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "tok", ExpiresAt: acFuture(),
	})
	postAutoClaw(t, h, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","stream":true,"messages":[]}`)

	up.mu.Lock()
	defer up.mu.Unlock()
	for _, c := range up.calls {
		if strings.HasPrefix(c, "chat|") {
			if got := strings.Split(c, "|")[2]; got != "glm-5.3" {
				t.Fatalf("出站 model = %q，期望 glm-5.3（前缀是网关路由协议，出站要剥掉）", got)
			}
			return
		}
	}
	t.Fatal("未触达 chat")
}

// 地区是**凭据的属性**：账号属于哪个站点就打哪个域名。
func TestAutoClawRoutesByRegion(t *testing.T) {
	up := &autoclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}

	// 国内版账号
	hCN, _, _ := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "t", ExpiresAt: acFuture(),
	})
	postAutoClaw(t, hCN, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","stream":true,"messages":[]}`)
	hosts := up.hostsOf("chat")
	if len(hosts) != 1 || hosts[0] != "autoglm-acceleration-api.zhipuai.cn" {
		t.Fatalf("国内版应打 zhipuai.cn，得到 %v", hosts)
	}

	// 国际版账号
	up2 := &autoclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	hIntl, _, _ := newAutoClawHandler(t, up2, autoclaw.Credential{
		Region: autoclaw.RegionIntl, Token: "t", ExpiresAt: acFuture(),
	})
	postAutoClaw(t, hIntl, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","stream":true,"messages":[]}`)
	hosts2 := up2.hostsOf("chat")
	if len(hosts2) != 1 || hosts2[0] != "autoglm-api.autoglm.ai" {
		t.Fatalf("国际版应打 autoglm.ai，得到 %v", hosts2)
	}
}

// AutoClaw 服务端每次刷新会轮换 refresh_token：并发刷新会互相作废并把人踢下线。
func TestAutoClawRefreshIsSingleFlight(t *testing.T) {
	var refreshCalls int32
	up := &autoclawUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			time.Sleep(120 * time.Millisecond) // 制造并发窗口
			return 200, "application/json",
				`{"code":0,"data":{"token":"newtok","refresh_token":"rt2","expires_at":1893456000000}}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "old", RefreshToken: "rt1",
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), // 临期
	})

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postAutoClaw(t, h, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","stream":true,"messages":[]}`)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("%d 个并发请求触发了 %d 次续期，期望 1 次（并发续期会互相作废 refresh_token）", n, got)
	}
	if up.count("chat") != n {
		t.Fatalf("chat 调用数 = %d，期望 %d", up.count("chat"), n)
	}
}

func TestAutoClawForcedRefreshOn401(t *testing.T) {
	up := &autoclawUpstream{respond: func(kind string, idx int) (int, string, string) {
		if kind == "refresh" {
			return 200, "application/json",
				`{"code":0,"data":{"token":"newtok","refresh_token":"rt2"}}`
		}
		if idx == 0 {
			return 401, "application/json", `{"code":401,"msg":"unauthorized"}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "t", RefreshToken: "rt1", ExpiresAt: acFuture(),
	})

	w := postAutoClaw(t, h, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("401 后应续期重试成功，状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if up.count("chat") != 2 || up.count("refresh") != 1 {
		t.Fatalf("chat=%d refresh=%d，期望 2/1", up.count("chat"), up.count("refresh"))
	}
}

func TestAutoClawDisabledAccountSkipped(t *testing.T) {
	up := &autoclawUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, mgr := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "t", ExpiresAt: acFuture(),
	})
	accts := h.cfg.ExtAccounts()
	if len(accts) != 1 {
		t.Fatalf("前置条件：应有 1 个账号，得到 %d", len(accts))
	}
	if err := mgr.SetDisabled(extstore.PAutoClaw, accts[0].ID, true); err != nil {
		t.Fatalf("停用: %v", err)
	}
	w := postAutoClaw(t, h, "autoclaw:glm-5.3", `{"model":"autoclaw:glm-5.3","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable || up.count("chat") != 0 {
		t.Fatalf("停用账号仍被使用: code=%d chat=%d", w.Code, up.count("chat"))
	}
}

func TestAutoClawModelListed(t *testing.T) {
	up := &autoclawUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "models" {
			return 200, "application/json", `{"models":[{"id":"zai_glm-5.3","name":"GLM-5.3"}]}`
		}
		return 200, "application/json", "{}"
	}}
	h, _, _ := newAutoClawHandler(t, up, autoclaw.Credential{
		Region: autoclaw.RegionCN, Token: "t", ExpiresAt: acFuture(),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "autoclaw:zai_glm-5.3") {
		t.Fatalf("模型列表缺少 autoclaw:zai_glm-5.3: %s", w.Body.String())
	}
}

func TestPlatformOfAutoClaw(t *testing.T) {
	if got := PlatformOf("autoclaw:glm-5.3"); got != "autoclaw" {
		t.Fatalf("PlatformOf = %s，期望 autoclaw", got)
	}
}

/* ── 单飞原语本身 ────────────────────────────────────────────── */

func TestFlightGroupMergesConcurrent(t *testing.T) {
	var g flightGroup[int]
	var runs int32
	const n = 8
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			v, err := g.Do("k", func() (int, error) {
				atomic.AddInt32(&runs, 1)
				time.Sleep(50 * time.Millisecond)
				return 42, nil
			})
			if err != nil {
				t.Errorf("Do: %v", err)
			}
			results[idx] = v
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("同 key 并发跑了 %d 次，期望 1 次", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("第 %d 个调用者拿到 %d，期望 42（应复用首次结果）", i, v)
		}
	}
}

func TestFlightGroupSeparatesKeys(t *testing.T) {
	var g flightGroup[string]
	var runs int32
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			v, _ := g.Do(key, func() (string, error) {
				atomic.AddInt32(&runs, 1)
				return key, nil
			})
			if v != key {
				t.Errorf("key %s 拿到 %s", key, v)
			}
		}(k)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&runs); got != 3 {
		t.Fatalf("不同 key 应各跑一次，实际 %d", got)
	}
}

// 首次调用失败后，下一次调用必须重新执行（不能把失败永久缓存）。
func TestFlightGroupRetriesAfterFailure(t *testing.T) {
	var g flightGroup[int]
	var runs int32
	for i := 0; i < 2; i++ {
		_, err := g.Do("k", func() (int, error) {
			atomic.AddInt32(&runs, 1)
			return 0, io.ErrUnexpectedEOF
		})
		if err == nil {
			t.Fatal("应返回错误")
		}
	}
	if got := atomic.LoadInt32(&runs); got != 2 {
		t.Fatalf("失败后应重跑，实际跑了 %d 次", got)
	}
}
