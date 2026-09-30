package cline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

// rewriteTransport 把上游常量域名改写到 httptest 服务器（协议不变，只换 host）。
type rewriteTransport struct {
	target *httptest.Server
	seen   *[]string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.target.URL, "http://")
	if t.seen != nil {
		*t.seen = append(*t.seen, req.Method+" "+req.URL.Path+
			" ctype="+req.Header.Get("X-CLIENT-TYPE")+
			" ver="+req.Header.Get("X-CLIENT-VERSION")+
			" auth="+mask(req.Header.Get("Authorization")))
	}
	return http.DefaultTransport.RoundTrip(req)
}

func mask(v string) string {
	v = strings.TrimPrefix(v, "Bearer ")
	if len(v) > 40 {
		return v[:40]
	}
	return v
}

func withMock(t *testing.T, h http.HandlerFunc, seen *[]string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := httpClient
	SetHTTPClient(&http.Client{Transport: &rewriteTransport{target: srv, seen: seen}})
	t.Cleanup(func() { httpClient = old })
}

// apiPath 把相对路径补成上游全路径（APIBase 自带 /api/v1，
// 拼错成 /api/v1/v1/... 上游回 404 —— 这正是协议文档强调的那点）。
func apiPath(rel string) string {
	u, err := url.Parse(APIBase + rel)
	if err != nil {
		panic(err)
	}
	return u.Path
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

/* ── 设备授权 ────────────────────────────────────────────────── */

func TestStartDeviceFlow(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user_management/authorize/device" {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "x-www-form-urlencoded") {
			t.Errorf("Content-Type = %s，期望 form-urlencoded", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "client_id="+WorkOSClientID) {
			t.Errorf("未带 client_id: %s", body)
		}
		writeJSON(w, map[string]any{
			"device_code": "dev-abc", "user_code": "PXQF-MWRC",
			"verification_uri":          "https://authkit.cline.bot/device",
			"verification_uri_complete": "https://authkit.cline.bot/device?user_code=PXQF-MWRC",
			"expires_in":                300, "interval": 5,
		})
	}, nil)

	f, err := StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if f.UserCode != "PXQF-MWRC" || f.DeviceCode != "dev-abc" {
		t.Fatalf("设备码解析错误: %+v", f)
	}
	if f.Interval != 5 || f.ExpiresIn != 300 {
		t.Fatalf("间隔/有效期错误: %+v", f)
	}
	if f.CompleteURI == "" || f.VerificationURI == "" {
		t.Fatalf("验证链接缺失: %+v", f)
	}
}

func TestStartDeviceFlowDefaults(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		// 上游没给 interval / verification_uri 时必须兜底
		writeJSON(w, map[string]any{"device_code": "d", "user_code": "U"})
	}, nil)
	f, err := StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if f.Interval != 5 {
		t.Fatalf("interval 兜底失败: %d", f.Interval)
	}
	if f.VerificationURI != DeviceVerifyFallback {
		t.Fatalf("verification_uri 兜底失败: %s", f.VerificationURI)
	}
	// 完整链接要带上 user_code，用户点开就能直接确认
	if !strings.Contains(f.CompleteURI, "user_code=U") {
		t.Fatalf("complete uri 未带 user_code: %s", f.CompleteURI)
	}
}

func TestPollPendingThenRegister(t *testing.T) {
	calls := 0
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user_management/authenticate":
			calls++
			if calls == 1 {
				writeJSON(w, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(w, map[string]any{
				"access_token": "workos_jwt_raw", "refresh_token": "rt-workos",
			})
		case apiPath(registerPath):
			// 登记请求必须原样带上 WorkOS 令牌
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["accessToken"] != "workos_jwt_raw" || body["refreshToken"] != "rt-workos" {
				t.Errorf("登记请求体错误: %v", body)
			}
			if r.Header.Get("X-CLIENT-TYPE") != ClientType {
				t.Errorf("登记缺 X-CLIENT-TYPE")
			}
			writeJSON(w, map[string]any{
				"data": map[string]any{
					// 登记返回的令牌**不带** workos: 前缀
					"accessToken": "eyJhbGciOi.session", "refreshToken": "rt-cline",
					"expiresAt": "2027-01-01T00:00:00Z",
					"userInfo":  map[string]any{"email": "a@b.c", "name": "Alice", "clineUserId": "usr-1"},
				},
				"success": true,
			})
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
	}, nil)

	f := &DeviceFlow{DeviceCode: "dev-1", Interval: 5}
	ctx := context.Background()

	r1, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("首次轮询: %v", err)
	}
	if r1.Done {
		t.Fatal("authorization_pending 时不该 Done")
	}

	r2, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("二次轮询: %v", err)
	}
	if !r2.Done || r2.Cred == nil {
		t.Fatalf("授权后应 Done 并带回凭据: %+v", r2)
	}
	// 关键：登记返回的裸 JWT 必须补上 workos: 前缀，否则后续请求 401
	if r2.Cred.AccessToken != "workos:eyJhbGciOi.session" {
		t.Fatalf("access_token 未补前缀: %q", r2.Cred.AccessToken)
	}
	if r2.Cred.RefreshToken != "rt-cline" {
		t.Fatalf("refresh_token 错误: %q", r2.Cred.RefreshToken)
	}
	if r2.Cred.Email != "a@b.c" || r2.Cred.AccountID != "usr-1" {
		t.Fatalf("用户信息缺失: %+v", r2.Cred)
	}
	// ISO8601 字符串要转成毫秒
	if r2.Cred.ExpiresAt != time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("expiresAt 解析错误: %d", r2.Cred.ExpiresAt)
	}
}

// 拿到 WorkOS 令牌却登记失败 —— 终态，必须把原因报出来，
// 当成「还没授权」会让用户对着不会发生的结果一直等。
func TestPollRegisterFailureIsTerminal(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPath(registerPath) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid token"}}`))
			return
		}
		writeJSON(w, map[string]any{"access_token": "t", "refresh_token": "r"})
	}, nil)

	res, err := (&DeviceFlow{DeviceCode: "d"}).Poll(context.Background())
	if err != nil {
		t.Fatalf("不该返回底层错误: %v", err)
	}
	if res.Done || !strings.Contains(res.Error, "登记") {
		t.Fatalf("登记失败应报终态错误，得到 %+v", res)
	}
}

func TestPollTerminalErrors(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"expired_token", "过期"},
		{"access_denied", "拒绝"},
	} {
		withMock(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"error": tc.err, "error_description": "desc"})
		}, nil)
		res, err := (&DeviceFlow{DeviceCode: "d"}).Poll(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tc.err, err)
		}
		if res.Done || !strings.Contains(res.Error, tc.want) {
			t.Fatalf("%s: 期望含 %q，得到 %+v", tc.err, tc.want, res)
		}
	}
}

/* ── 前缀与池 ────────────────────────────────────────────────── */

func TestEnsurePrefix(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"workos:eyJ":   "workos:eyJ",
		"eyJhbGci":     "workos:eyJhbGci",
		"  eyJhbGci  ": "workos:eyJhbGci",
		"workos:":      "workos:",
	}
	for in, want := range cases {
		if got := EnsurePrefix(in); got != want {
			t.Errorf("EnsurePrefix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestPoolOfAndBareModel(t *testing.T) {
	cases := []struct {
		id   string
		pool Pool
		bare string
	}{
		{"cline-free/deepseek-v4.1-flash", PoolFree, "deepseek-v4.1-flash"},
		{"cline-pass/glm-5.3", PoolPass, "glm-5.3"},
		{"cline-cloud/kimi-k3", PoolCloud, "kimi-k3"},
		// 免费组里有无前缀条目，按上游行为无前缀归订阅池
		{"stealth/pixel-canary", PoolPass, "stealth/pixel-canary"},
	}
	for _, c := range cases {
		if got := PoolOf(c.id); got != c.pool {
			t.Errorf("PoolOf(%q) = %v，期望 %v", c.id, got, c.pool)
		}
		if got := BareModel(c.id); got != c.bare {
			t.Errorf("BareModel(%q) = %q，期望 %q", c.id, got, c.bare)
		}
	}
}

/* ── 续期 ────────────────────────────────────────────────────── */

func TestRefreshUsesCamelCaseGrantType(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPath(refreshPath) {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		// camelCase！写成 OAuth 标准的 grant_type 上游不认
		if body["grantType"] != "refresh_token" {
			t.Errorf("grantType = %q，期望 refresh_token", body["grantType"])
		}
		if _, bad := body["grant_type"]; bad {
			t.Error("不该出现下划线形态的 grant_type")
		}
		writeJSON(w, map[string]any{"data": map[string]any{
			"accessToken": "new_bare_jwt", "refreshToken": "rt2",
			"expiresAt": "2027-06-01T12:00:00Z",
		}, "success": true})
	}, nil)

	old := &Credential{AccessToken: "workos:old", RefreshToken: "rt1", Email: "a@b.c"}
	fresh, err := Refresh(context.Background(), old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// 续期返回裸 JWT，必须补前缀
	if fresh.AccessToken != "workos:new_bare_jwt" {
		t.Fatalf("未补前缀: %q", fresh.AccessToken)
	}
	if fresh.RefreshToken != "rt2" {
		t.Fatalf("refresh_token 未更新: %q", fresh.RefreshToken)
	}
	if fresh.ExpiresAt == 0 {
		t.Fatal("expiresAt 未解析")
	}
	if fresh.Email != "a@b.c" {
		t.Fatal("续期丢失了展示字段")
	}
	if old.AccessToken != "workos:old" {
		t.Fatal("Refresh 不应修改入参")
	}
}

func TestRefreshWithoutToken(t *testing.T) {
	if _, err := Refresh(context.Background(), &Credential{}); err == nil {
		t.Fatal("缺 refresh_token 应报错")
	}
}

func TestRefreshInvalidToken(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}, nil)
	_, err := Refresh(context.Background(), &Credential{RefreshToken: "rt"})
	if err == nil || !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("401 应提示重新登录，得到: %v", err)
	}
}

// 续期回执保留旧 refresh_token 时不能清空它。
func TestRefreshKeepsRefreshTokenWhenAbsent(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"accessToken": "n"}})
	}, nil)
	fresh, err := Refresh(context.Background(), &Credential{AccessToken: "workos:o", RefreshToken: "rt-keep"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.RefreshToken != "rt-keep" {
		t.Fatalf("refresh_token 被清空: %q", fresh.RefreshToken)
	}
}

func TestParseExpiresAt(t *testing.T) {
	// 同名不同型：接口回 ISO 字符串，桌面端 providers.json 是毫秒数
	if got := parseExpiresAt("2027-01-01T00:00:00Z"); got != time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("ISO 解析错误: %d", got)
	}
	if got := parseExpiresAt(float64(1789842888000)); got != 1789842888000 {
		t.Errorf("毫秒解析错误: %d", got)
	}
	if got := parseExpiresAt("1789842888000"); got != 1789842888000 {
		t.Errorf("毫秒字符串解析错误: %d", got)
	}
	if got := parseExpiresAt(nil); got != 0 {
		t.Errorf("nil 应为 0，得到 %d", got)
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		cred Credential
		want bool
	}{
		{"空 token", Credential{}, true},
		{"无过期时间", Credential{AccessToken: "t"}, true},
		{"剩 1 分钟", Credential{AccessToken: "t", ExpiresAt: now.Add(time.Minute).UnixMilli()}, true},
		{"剩 30 分钟", Credential{AccessToken: "t", ExpiresAt: now.Add(30 * time.Minute).UnixMilli()}, false},
	}
	for _, c := range cases {
		if got := c.cred.NeedsRefresh(); got != c.want {
			t.Errorf("%s: %v，期望 %v", c.name, got, c.want)
		}
	}
}

/* ── 模型目录 ────────────────────────────────────────────────── */

func TestListModelsGroupsAndPrefixes(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPath(modelsPath) {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		writeJSON(w, map[string]any{
			"recommended": []map[string]any{{"id": "openai/gpt-6-astra", "name": "gpt-6-astra"}},
			"free": []map[string]any{
				// 免费组里有无前缀条目 —— 只看前缀会漏掉它们
				{"id": "stealth/pixel-canary", "name": "Pixel Canary"},
				{"id": "cline-free/deepseek-v4.1-flash", "name": "Deepseek-v4.1-Flash"},
			},
			"clinePass":  []map[string]any{{"id": "cline-pass/glm-5.3", "name": "cline-pass/glm-5.3"}},
			"clineCloud": []map[string]any{{"id": "cline-cloud/kimi-k3", "name": "cline-cloud/kimi-k3"}},
		})
	}, nil)

	c, err := ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(c.Free) != 2 || len(c.Pass) != 1 || len(c.Cloud) != 1 || len(c.Recommended) != 1 {
		t.Fatalf("分组数量错误: free=%d pass=%d cloud=%d rec=%d",
			len(c.Free), len(c.Pass), len(c.Cloud), len(c.Recommended))
	}
	// 归池以响应分组为准：无前缀的 stealth 条目也在免费组
	if c.Free[0].ID != "stealth/pixel-canary" || c.Free[0].Pool != PoolFree {
		t.Fatalf("免费组归属错误: %+v", c.Free[0])
	}
	if c.Cloud[0].Pool != PoolCloud {
		t.Fatalf("云端组归属错误: %+v", c.Cloud[0])
	}
	if got := len(c.All()); got != 5 {
		t.Fatalf("All() = %d，期望 5", got)
	}
}

func TestListModelsUpstreamError(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)
	if _, err := ListModels(context.Background()); err == nil {
		t.Fatal("非 200 应报错")
	}
}

/* ── 对话 ────────────────────────────────────────────────────── */

func TestChatHeaders(t *testing.T) {
	var seen []string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPath(chatPath) {
			t.Errorf("路径错误: %s（注意只有一个 v1）", r.URL.Path)
		}
		if r.Header.Get("HTTP-Referer") != "https://cline.bot" || r.Header.Get("X-Title") != "Cline" {
			t.Error("缺少来源标记头")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\"}\n\n"))
	}, &seen)

	// 裸名（已剥池前缀）
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true}`)
	resp, err := Chat(context.Background(), &Credential{AccessToken: "workos:tok"}, body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"id":"1"`) {
		t.Fatalf("响应体异常: %s", raw)
	}
	if len(seen) != 1 {
		t.Fatalf("请求记录数 = %d", len(seen))
	}
	got := seen[0]
	// 缺 X-CLIENT-TYPE 时免费池一律 403 —— 这是本通道最关键的头
	if !strings.Contains(got, "ctype=cline-sdk") {
		t.Fatalf("缺少产品面标识头: %s", got)
	}
	if !strings.Contains(got, "ver=3.0.62") {
		t.Fatalf("缺少版本头: %s", got)
	}
	// 令牌必须带 workos: 前缀
	if !strings.Contains(got, "auth=workos:tok") {
		t.Fatalf("鉴权头未带前缀: %s", got)
	}
}

// 凭据里存的是裸 JWT 时（老数据/手工粘贴）也要能正确补前缀。
func TestChatAddsPrefixForBareToken(t *testing.T) {
	var seen []string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}, &seen)
	resp, err := Chat(context.Background(), &Credential{AccessToken: "bare_jwt"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()
	if len(seen) != 1 || !strings.Contains(seen[0], "auth=workos:bare_jwt") {
		t.Fatalf("裸 token 未补前缀: %v", seen)
	}
}

/* ── 余额 ────────────────────────────────────────────────────── */

func TestFetchBalanceConvertsMicrocredits(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPath(userMePath):
			writeJSON(w, map[string]any{"data": map[string]any{"id": "usr-1", "email": "a@b.c"}})
		case apiPath("/users/usr-1/balance"):
			writeJSON(w, map[string]any{"data": map[string]any{"balance": -17704000000}})
		case apiPath(userPlanPath):
			writeJSON(w, map[string]any{"data": map[string]any{"name": "ClinePass"}})
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
	}, nil)

	b, err := FetchBalance(context.Background(), &Credential{AccessToken: "workos:t"})
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	// 上游是微 credit，÷1e6 才是 credit
	if b.Credits != -17704 {
		t.Fatalf("credit 换算错误: %v", b.Credits)
	}
	if b.RawMicro != -17704000000 {
		t.Fatalf("原始微 credit 未保留: %v", b.RawMicro)
	}
	if b.PlanName != "ClinePass" || b.UserEmail != "a@b.c" {
		t.Fatalf("展示字段缺失: %+v", b)
	}
}

/* ── 代理 ────────────────────────────────────────────────────── */

func TestSetProxy(t *testing.T) {
	prev := httpClient
	t.Cleanup(func() { httpClient = prev })

	for _, bad := range []string{"://nope", "127.0.0.1:2080"} {
		if err := SetProxy(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if err := SetProxy("http://127.0.0.1:2080"); err != nil {
		t.Fatalf("合法代理被拒: %v", err)
	}
	tr := httpClient.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("代理未生效")
	}
	if err := SetProxy(""); err != nil {
		t.Fatalf("空代理应合法: %v", err)
	}
	if httpClient.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("空串应回落到 ProxyFromEnvironment")
	}
}
