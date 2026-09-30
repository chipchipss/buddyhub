package zai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// mockOAuth 模拟 OAuth init/poll 与兑换链。
type mockOAuth struct {
	server      *httptest.Server
	pollCount   int
	doneAfter   int
	pollToken   string // 服务端下发的 poll_token（客户端必须沿用）
	initHeader  http.Header
	pollHeaders []http.Header
}

func newMockOAuth(t *testing.T, doneAfter int, serverPollToken string) *mockOAuth {
	m := &mockOAuth{doneAfter: doneAfter, pollToken: serverPollToken}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/cli/init"):
			m.initHeader = r.Header.Clone()
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"flow-1","authorize_url":"https://example.test/auth","poll_token":"` + m.pollToken + `"}}`))
		case strings.Contains(r.URL.Path, "/oauth/cli/poll/"):
			m.pollHeaders = append(m.pollHeaders, r.Header.Clone())
			m.pollCount++
			if m.pollCount < m.doneAfter {
				_, _ = w.Write([]byte(`{"data":{"status":"pending"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"access_token":"jwt.token.sig","email":"user@example.com"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/auth/z/login"):
			_, _ = w.Write([]byte(`{"data":{"access_token":"biz-token"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/biz/customer/getCustomerInfo"):
			_, _ = w.Write([]byte(`{"data":{"organizations":[{"organizationId":"org-1","organizationName":"默认机构","projects":[{"projectId":"proj-1","projectName":"默认项目"}]}]}}`))
		case strings.Contains(r.URL.Path, "/api_keys/copy/"):
			_, _ = w.Write([]byte(`{"data":{"secretKey":"secret-xyz"}}`))
		case strings.HasSuffix(r.URL.Path, "/api_keys"):
			_, _ = w.Write([]byte(`{"data":[{"name":"zcode-api-key","apiKey":"key-abc"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func TestOAuthFlowAndExchange(t *testing.T) {
	m := newMockOAuth(t, 2, "server-issued-token")
	SetEndpoints(m.server.URL, m.server.URL, "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	ctx := context.Background()
	flow, err := StartOAuth(ctx)
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	if flow.AuthorizeURL == "" || flow.FlowID != "flow-1" {
		t.Fatalf("授权信息不完整: %+v", flow)
	}
	// 服务端下发的 poll_token 必须沿用（否则 poll 会被拒）
	if flow.pollToken != "server-issued-token" {
		t.Fatalf("应采用服务端下发的 poll_token，got %q", flow.pollToken)
	}

	// 第一次轮询：未完成
	res, err := flow.Poll(ctx)
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if res.Done {
		t.Fatal("授权未完成时 Done 应为 false")
	}

	// 第二次：完成
	res, err = flow.Poll(ctx)
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if !res.Done || res.AccessToken != "jwt.token.sig" {
		t.Fatalf("应拿到 access_token，got %+v", res)
	}
	if res.Email != "user@example.com" {
		t.Fatalf("应带邮箱，got %q", res.Email)
	}

	// 兑换 API Key
	key, err := ExchangeAPIKey(ctx, res.AccessToken)
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if key != "key-abc.secret-xyz" {
		t.Fatalf("Key 形态应为 <apiKey>.<secretKey>，got %q", key)
	}

	// init/poll 只带 Authorization + Content-Type（额外伪装头会让上游产生设备绑定）
	if m.initHeader.Get("Authorization") == "" {
		t.Fatal("init 应带 Authorization")
	}
	if m.initHeader.Get("X-Device-Mid") != "" {
		t.Fatal("init 不应带伪装头（官方 CLI 只发 Authorization + Content-Type）")
	}
	if ua := m.initHeader.Get("User-Agent"); strings.Contains(ua, "Go-http-client") {
		t.Fatalf("不应暴露 Go 默认 UA（非官方客户端指纹），got %q", ua)
	}
	for _, h := range m.pollHeaders {
		if h.Get("Authorization") == "" {
			t.Fatal("poll 应带 Authorization")
		}
	}
}

func TestOAuthPollRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth/cli/init") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f","authorize_url":"https://x.test"}}`))
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":3004,"msg":"session expired"}`))
	}))
	defer srv.Close()
	SetEndpoints(srv.URL, srv.URL, "", srv.URL)
	SetOrigins(srv.URL, srv.URL)

	flow, err := StartOAuth(context.Background())
	if err != nil {
		t.Fatalf("发起失败: %v", err)
	}
	if _, err := flow.Poll(context.Background()); err == nil {
		t.Fatal("会话过期应返回错误（提示重新发起）")
	}
}

func TestOAuthInitRejectsUpstreamCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1005,"data":{}}`))
	}))
	defer srv.Close()
	SetEndpoints(srv.URL, srv.URL, "", srv.URL)
	SetOrigins(srv.URL, srv.URL)

	if _, err := StartOAuth(context.Background()); err == nil {
		t.Fatal("上游非零 code 应返回错误")
	}
}

func TestExchangeCreatesKeyWhenMissing(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/auth/z/login"):
			_, _ = w.Write([]byte(`{"data":{"accessToken":"biz"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/biz/customer/getCustomerInfo"):
			_, _ = w.Write([]byte(`{"data":{"organizations":[{"organizationId":"o","organizationName":"其它机构","projects":[{"projectId":"p","projectName":"其它项目"}]}]}}`))
		case strings.Contains(r.URL.Path, "/api_keys/copy/"):
			_, _ = w.Write([]byte(`{"data":{"secretKey":"sk"}}`))
		case strings.HasSuffix(r.URL.Path, "/api_keys"):
			if r.Method == http.MethodPost {
				created = true
				_, _ = w.Write([]byte(`{"data":{"apiKey":"new-key"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`)) // 列表为空 → 触发创建
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	SetEndpoints(srv.URL, srv.URL, "", srv.URL)
	SetOrigins(srv.URL, srv.URL)

	key, err := ExchangeAPIKey(context.Background(), "tok")
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if !created {
		t.Fatal("列表无 zcode-api-key 时应创建")
	}
	if key != "new-key.sk" {
		t.Fatalf("Key 形态不符: %q", key)
	}
}

func TestAccountStoresJWTAndFallbackKey(t *testing.T) {
	// OAuth 入池的账号：JWT 走 Plan，兑换来的 Key 走回退
	acc := NewAccount("oauth号", "header."+b64(`{"sub":"u1"}`)+".sig")
	acc.APIKey = "key-abc.secret-xyz"
	if !acc.HasJWTPath() {
		t.Fatal("OAuth 账号应可走 Plan 通道")
	}
	if !acc.HasKeyFallback() {
		t.Fatal("OAuth 账号应带回退 Key")
	}

	// 落盘后重载保持两条凭证
	st, err := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Add(acc); err != nil {
		t.Fatal(err)
	}
	got := st.Get(acc.ID)
	if got.JWT == "" || got.APIKey == "" {
		t.Fatalf("重载后两条凭证都应保留: %+v", got)
	}
}
