package copilot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rewriteTransport 把上游常量域名改写到 httptest 服务器（协议事实不变，只换 host）。
type rewriteTransport struct {
	target *httptest.Server
	seen   *[]string // 记录 (method, path, 关键头)
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.target.URL, "http://")
	if t.seen != nil {
		*t.seen = append(*t.seen, req.Method+" "+req.URL.Path+
			" int="+req.Header.Get("Copilot-Integration-Id")+
			" auth="+maskToken(req.Header.Get("authorization")))
	}
	return http.DefaultTransport.RoundTrip(req)
}

func maskToken(v string) string {
	v = strings.TrimPrefix(v, "Bearer ")
	v = strings.TrimPrefix(v, "token ")
	if len(v) > 6 {
		return v[:6]
	}
	return v
}

func withMock(t *testing.T, h http.HandlerFunc, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := httpClient
	SetHTTPClient(&http.Client{Transport: &rewriteTransport{target: srv, seen: seen}})
	t.Cleanup(func() { SetHTTPClient(old) })
	return srv
}

/* ── 设备流 ──────────────────────────────────────────────────── */

func TestStartDeviceFlow(t *testing.T) {
	var seen []string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/device/code" {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != ClientID {
			t.Errorf("client_id = %q, 期望 %q", body["client_id"], ClientID)
		}
		// 官方扩展形态：GitHub 按这些头识别客户端
		if r.Header.Get("editor-version") == "" || r.Header.Get("user-agent") == "" {
			t.Error("缺少设备流识别头")
		}
		writeJSONT(w, map[string]any{
			"device_code": "dev-1", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900, "interval": 5,
		})
	}, &seen)

	f, err := StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if f.UserCode != "ABCD-1234" || f.DeviceCode != "dev-1" {
		t.Fatalf("设备码解析错误: %+v", f)
	}
	if f.Interval != 5 || f.ExpiresIn != 900 {
		t.Fatalf("间隔/有效期错误: %+v", f)
	}
}

func TestStartDeviceFlowDefaultInterval(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		// GitHub 未下发 interval 时必须兜底，否则调用方轮询间隔为 0 → 狂刷
		writeJSONT(w, map[string]any{"device_code": "d", "user_code": "u", "expires_in": 900})
	}, nil)
	f, err := StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if f.Interval != 5 {
		t.Fatalf("interval 兜底失败: %d", f.Interval)
	}
}

func TestStartDeviceFlowRejected(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad_client"}`))
	}, nil)
	if _, err := StartDeviceFlow(context.Background()); err == nil {
		t.Fatal("非 200 应报错")
	}
}

func TestPollPendingThenDone(t *testing.T) {
	calls := 0
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			calls++
			if calls == 1 {
				writeJSONT(w, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSONT(w, map[string]any{"access_token": "gho_abcdef123456", "token_type": "bearer"})
		case "/copilot_internal/v2/token":
			// 兑换 Copilot token：authorization 必须是 "token <github_token>"
			if !strings.HasPrefix(r.Header.Get("authorization"), "token gho_") {
				t.Errorf("兑换鉴权头错误: %q", r.Header.Get("authorization"))
			}
			writeJSONT(w, map[string]any{"token": "cop_tok_1", "expires_at": time.Now().Add(25 * time.Minute).Unix()})
		case "/user":
			writeJSONT(w, map[string]any{"login": "octocat", "plan": map[string]any{"name": "pro"}})
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
		t.Fatal("authorization_pending 时不应 Done")
	}

	r2, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("二次轮询: %v", err)
	}
	if !r2.Done || r2.Cred == nil {
		t.Fatal("授权后应 Done 并带回凭据")
	}
	if r2.Cred.GitHubToken != "gho_abcdef123456" || r2.Cred.CopilotToken != "cop_tok_1" {
		t.Fatalf("凭据错误: %+v", r2.Cred)
	}
	if r2.Cred.Login != "octocat" || r2.Cred.Plan != "pro" {
		t.Fatalf("展示信息错误: %+v", r2.Cred)
	}
}

func TestPollTerminalErrors(t *testing.T) {
	for _, tc := range []struct{ ghErr, wantSub string }{
		{"expired_token", "过期"},
		{"access_denied", "拒绝"},
	} {
		withMock(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSONT(w, map[string]any{"error": tc.ghErr})
		}, nil)
		f := &DeviceFlow{DeviceCode: "d"}
		res, err := f.Poll(context.Background())
		if err != nil {
			t.Fatalf("%s: 不应返回底层错误: %v", tc.ghErr, err)
		}
		if res.Done || !strings.Contains(res.Error, tc.wantSub) {
			t.Fatalf("%s: 期望面向用户的错误含 %q，得到 %+v", tc.ghErr, tc.wantSub, res)
		}
	}
}

// TestPollIncorrectDeviceCodeIsTransient 覆盖实测行为：GitHub 在设备码刚下发的一小段
// 窗口内会回 incorrect_device_code（尚未生效），随后才转为 authorization_pending。
// 若把它当终态，用户刚点「开始授权」就会被判失败。
func TestPollIncorrectDeviceCodeIsTransient(t *testing.T) {
	n := 0
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n <= badCodeTolerance {
			writeJSONT(w, map[string]any{
				"error": "incorrect_device_code", "error_description": "The device_code provided is not valid.",
			})
			return
		}
		writeJSONT(w, map[string]any{"error": "authorization_pending"})
	}, nil)

	f := &DeviceFlow{DeviceCode: "d"}
	for i := 1; i <= badCodeTolerance; i++ {
		res, err := f.Poll(context.Background())
		if err != nil {
			t.Fatalf("第 %d 次: %v", i, err)
		}
		if res.Error != "" || res.Done {
			t.Fatalf("第 %d 次应容忍为 pending，得到 %+v", i, res)
		}
	}
	// 之后转为正常的 authorization_pending，容忍计数应复位
	res, err := f.Poll(context.Background())
	if err != nil || res.Error != "" || res.Done {
		t.Fatalf("转为 pending 后应正常，得到 %+v err=%v", res, err)
	}
	if f.badCodeStreak != 0 {
		t.Fatalf("容忍计数未复位: %d", f.badCodeStreak)
	}
}

func TestPollIncorrectDeviceCodeGivesUp(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONT(w, map[string]any{"error": "incorrect_device_code"})
	}, nil)
	f := &DeviceFlow{DeviceCode: "d"}
	var res *PollResult
	var err error
	for i := 0; i <= badCodeTolerance; i++ {
		res, err = f.Poll(context.Background())
		if err != nil {
			t.Fatalf("第 %d 次: %v", i, err)
		}
	}
	// 一直无效就必须报出来，不能无限轮询
	if !strings.Contains(res.Error, "无效") {
		t.Fatalf("持续无效应报错，得到 %+v", res)
	}
}

func TestPollSurfacesErrorDescription(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONT(w, map[string]any{
			"error": "unsupported_grant_type", "error_description": "grant_type is not supported",
		})
	}, nil)
	res, err := (&DeviceFlow{DeviceCode: "d"}).Poll(context.Background())
	if err != nil {
		t.Fatalf("不应返回底层错误: %v", err)
	}
	if !strings.Contains(res.Error, "grant_type is not supported") {
		t.Fatalf("未带出 error_description: %q", res.Error)
	}
}

/* ── 凭据与续期 ──────────────────────────────────────────────── */

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		cred Credential
		want bool
	}{
		{"空 token", Credential{}, true},
		{"无过期时间", Credential{CopilotToken: "t"}, true},
		{"剩余 1 分钟（提前 3 分钟窗口内）", Credential{CopilotToken: "t", ExpiresAt: now.Add(time.Minute).Unix()}, true},
		{"剩余 10 分钟", Credential{CopilotToken: "t", ExpiresAt: now.Add(10 * time.Minute).Unix()}, false},
		{"已过期", Credential{CopilotToken: "t", ExpiresAt: now.Add(-time.Minute).Unix()}, true},
	}
	for _, c := range cases {
		if got := c.cred.NeedsRefresh(); got != c.want {
			t.Errorf("%s: NeedsRefresh()=%v, 期望 %v", c.name, got, c.want)
		}
	}
}

func TestRefreshKeepsGitHubToken(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		writeJSONT(w, map[string]any{"token": "cop_new", "expires_at": time.Now().Add(25 * time.Minute).Unix()})
	}, nil)

	old := Credential{GitHubToken: "gho_keep", CopilotToken: "cop_old", Login: "octocat", Plan: "pro"}
	fresh, err := Refresh(context.Background(), &old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.CopilotToken != "cop_new" {
		t.Fatalf("token 未更新: %s", fresh.CopilotToken)
	}
	// GitHub token / 展示字段必须保留，否则下次续期就断了
	if fresh.GitHubToken != "gho_keep" || fresh.Login != "octocat" || fresh.Plan != "pro" {
		t.Fatalf("续期丢失了字段: %+v", fresh)
	}
	if old.CopilotToken != "cop_old" {
		t.Fatal("Refresh 不应修改入参")
	}
}

func TestRefreshWithoutGitHubToken(t *testing.T) {
	if _, err := Refresh(context.Background(), &Credential{}); err == nil {
		t.Fatal("缺 GitHub token 应报错（提示重新登录）")
	}
}

func TestRefreshNoSubscription(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}, nil)
	_, err := Refresh(context.Background(), &Credential{GitHubToken: "gho_x"})
	if err == nil || !strings.Contains(err.Error(), "未订阅") {
		t.Fatalf("401 应提示无效/未订阅，得到: %v", err)
	}
}

func TestExchangeFallsBackWhenUserFails(t *testing.T) {
	// /user 失败只是拿不到展示信息，不应让整个兑换失败
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSONT(w, map[string]any{"token": "cop_1", "expires_at": time.Now().Add(20 * time.Minute).Unix()})
	}, nil)
	cred, err := Exchange(context.Background(), "gho_1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if cred.CopilotToken != "cop_1" || cred.Login != "" {
		t.Fatalf("凭据错误: %+v", cred)
	}
}

func TestExchangeMissingToken(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONT(w, map[string]any{"expires_at": time.Now().Add(time.Hour).Unix()})
	}, nil)
	if _, err := Exchange(context.Background(), "gho_1"); err == nil {
		t.Fatal("回执无 token 应报错")
	}
}

/* ── 对话与模型 ──────────────────────────────────────────────── */

func TestChatHeadersAndPassThrough(t *testing.T) {
	var seen []string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		// 网关不做协议翻译：请求体必须原样到达上游
		if string(raw) != `{"model":"gpt-4o","stream":true}` {
			t.Errorf("请求体被改动: %s", raw)
		}
		if r.Header.Get("x-github-api-version") == "" || r.Header.Get("editor-version") == "" {
			t.Error("缺少对话识别头")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\"}\n\n"))
	}, &seen)

	cred := &Credential{CopilotToken: "cop_1"}
	resp, err := Chat(context.Background(), cred, []byte(`{"model":"gpt-4o","stream":true}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "chat.completion") && !strings.Contains(string(raw), `"id":"1"`) {
		t.Fatalf("响应体异常: %s", raw)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "int=vscode-chat") {
		t.Fatalf("集成标识缺失: %v", seen)
	}
}

func TestListModels(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		writeJSONT(w, map[string]any{"data": []map[string]any{
			{"id": "gpt-4o", "name": "GPT-4o", "vendor": "OpenAI"},
			{"id": "claude-sonnet-4", "name": "Claude Sonnet 4", "vendor": "Anthropic"},
		}})
	}, nil)

	mods, err := ListModels(context.Background(), &Credential{CopilotToken: "cop_1"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(mods) != 2 || mods[0].ID != "gpt-4o" || mods[1].Vendor != "Anthropic" {
		t.Fatalf("模型解析错误: %+v", mods)
	}
}

func TestListModelsUpstreamError(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token expired"}`))
	}, nil)
	if _, err := ListModels(context.Background(), &Credential{CopilotToken: "bad"}); err == nil {
		t.Fatal("非 200 应报错")
	}
}

// ModelUnsupported 只认模型级词表：账号级的 402/429/5xx 必须留给账号轮转。
// 400 model_not_supported 是实测包（池内账号 wjhjq 请求目录外的 claude-sonnet-4）。
func TestModelUnsupported(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":{"message":"The requested model is not supported.","code":"model_not_supported"}}`, true},
		{400, `{"error":{"code":"model_not_found"}}`, true},
		{404, `{"error":{"code":"model_not_supported"}}`, true},
		{403, `{"error":{"message":"Must have a paid plan"}}`, false},
		{402, `{"error":{"message":"insufficient credits"}}`, false},
		{429, `{"error":{"message":"rate limited"}}`, false},
		{500, `Internal Server Error`, false},
		{400, `{"error":{"message":"invalid request parameters"}}`, false},
	}
	for _, c := range cases {
		if got := ModelUnsupported(c.status, []byte(c.body)); got != c.want {
			t.Errorf("ModelUnsupported(%d, %s) = %v, 期望 %v", c.status, c.body, got, c.want)
		}
	}
}

func TestFormatExpiry(t *testing.T) {
	if got := FormatExpiry(0); got != "未知" {
		t.Errorf("零值: %s", got)
	}
	if got := FormatExpiry(time.Now().Add(-time.Minute).Unix()); got != "已过期" {
		t.Errorf("过期: %s", got)
	}
	if got := FormatExpiry(time.Now().Add(10 * time.Minute).Unix()); !strings.Contains(got, "分钟后") {
		t.Errorf("未来: %s", got)
	}
}

func writeJSONT(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
