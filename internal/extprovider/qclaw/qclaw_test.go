package qclaw

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type rewriteTransport struct {
	target *httptest.Server
	reqs   *[]*http.Request
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.target.URL, "http://")
	if t.reqs != nil {
		*t.reqs = append(*t.reqs, req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func withMock(t *testing.T, h http.HandlerFunc) *[]*http.Request {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var reqs []*http.Request
	old := httpClient
	SetHTTPClient(&http.Client{Transport: &rewriteTransport{target: srv, reqs: &reqs}})
	t.Cleanup(func() { httpClient = old })
	return &reqs
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// jprxOK 造一个成功信封：{ret:0, data:{resp:{common:{code:0}, data:{…}}}}。
func jprxOK(inner map[string]any) map[string]any {
	return map[string]any{
		"ret": 0,
		"data": map[string]any{
			"resp": map[string]any{
				"common": map[string]any{"code": 0},
				"data":   inner,
			},
		},
	}
}

/* ── 签名 ────────────────────────────────────────────────────── */

var ctxRe = regexp.MustCompile(`^rnd=([a-z0-9]{32}); date=(\d+); gid=(\S+); sg=([0-9a-f]{32})$`)

// JPrx-Ctx 必须逐字节符合官方口径：sg = md5(body + KEY + rnd + date + gid)。
// 拼错顺序或用了毫秒时间戳，上游回 ret != 0。
func TestJPrxCtxFormat(t *testing.T) {
	body := `{"web_version":"1.4.0","web_env":"release"}`
	gid := "abc-123"
	ctx := JPrxCtx(body, gid)

	m := ctxRe.FindStringSubmatch(ctx)
	if m == nil {
		t.Fatalf("JPrx-Ctx 格式不符: %q", ctx)
	}
	rnd, date, gotGid, sg := m[1], m[2], m[3], m[4]
	if gotGid != gid {
		t.Fatalf("gid = %q，期望 %q", gotGid, gid)
	}
	// 秒级时间戳（10 位）
	if len(date) != 10 {
		t.Fatalf("date 不是秒级时间戳: %q", date)
	}
	want := md5.Sum([]byte(body + JPrxSignatureKey + rnd + date + gid))
	if sg != hex.EncodeToString(want[:]) {
		t.Fatalf("签名不对:\n got %s\nwant %s", sg, hex.EncodeToString(want[:]))
	}
}

func TestJPrxCtxEmptyGidFallsBack(t *testing.T) {
	ctx := JPrxCtx("{}", "")
	if m := ctxRe.FindStringSubmatch(ctx); m == nil || m[3] != "1" {
		t.Fatalf("空 gid 应回落 \"1\": %q", ctx)
	}
}

func TestJPrxCtxIsRandomized(t *testing.T) {
	// rnd 每次不同，否则同一秒内的重复请求签名会撞
	a, b := JPrxCtx("{}", "g"), JPrxCtx("{}", "g")
	if a == b {
		t.Fatal("两次签名相同（rnd 没随机）")
	}
}

/* ── 两层信封 ────────────────────────────────────────────────── */

func TestUnwrapJPRX(t *testing.T) {
	t.Run("成功取最里层", func(t *testing.T) {
		got, err := unwrapJPRX(jprxOK(map[string]any{"key": "sk-1"}))
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if got["key"] != "sk-1" {
			t.Fatalf("没取到最里层: %v", got)
		}
	})
	t.Run("ret 非 0 报错", func(t *testing.T) {
		_, err := unwrapJPRX(map[string]any{"ret": 1001, "msg": "签名校验失败"})
		if err == nil || !strings.Contains(err.Error(), "签名校验失败") {
			t.Fatalf("应带出上游原文，得到: %v", err)
		}
	})
	t.Run("common.code 非 0 报错", func(t *testing.T) {
		doc := map[string]any{"ret": 0, "data": map[string]any{
			"resp": map[string]any{"common": map[string]any{"code": 3007, "message": "需要验证"}},
		}}
		_, err := unwrapJPRX(doc)
		if err == nil || !strings.Contains(err.Error(), "3007") {
			t.Fatalf("应报 common.code，得到: %v", err)
		}
	})
	t.Run("没有 resp 层时原样返回", func(t *testing.T) {
		got, err := unwrapJPRX(map[string]any{"ret": 0, "data": map[string]any{"a": "b"}})
		if err != nil || got["a"] != "b" {
			t.Fatalf("得到 %v / %v", got, err)
		}
	})
}

/* ── 微信登录 ────────────────────────────────────────────────── */

func TestStartLoginBuildsWeChatURL(t *testing.T) {
	reqs := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/data/"+CmdWXLoginState+"/forward") {
			t.Errorf("命令号路径错误: %s", r.URL.Path)
		}
		writeJSON(w, jprxOK(map[string]any{"state": "st-1"}))
	})

	flow, err := StartLogin(context.Background(), "guid-1")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if flow.State != "st-1" || flow.GUID != "guid-1" {
		t.Fatalf("回执解析错误: %+v", flow)
	}
	// 授权链接必须带 appid / 回调 / state，否则扫了也回不来
	for _, want := range []string{"appid=" + WXAppID, "response_type=code", "scope=snsapi_login", "state=st-1"} {
		if !strings.Contains(flow.URL, want) {
			t.Errorf("授权链接缺少 %s：%s", want, flow.URL)
		}
	}
	// 业务请求必须带签名
	if got := (*reqs)[0].Header.Get("JPrx-Ctx"); !strings.HasPrefix(got, "rnd=") {
		t.Errorf("业务请求缺 JPrx-Ctx: %q", got)
	}
}

func TestCompleteLoginExchangesCodeForKey(t *testing.T) {
	var cmds []string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Path[strings.LastIndex(r.URL.Path, "/data/")+6:]
		cmd = strings.TrimSuffix(cmd, "/forward")
		cmds = append(cmds, cmd)
		switch cmd {
		case CmdWXLogin:
			writeJSON(w, jprxOK(map[string]any{
				"token": "jwt-1", "openclaw_channel_token": "ch-1",
				"user_info": map[string]any{"userId": "u-9", "nickname": "小明"},
			}))
		case CmdCreateAPIKey:
			writeJSON(w, jprxOK(map[string]any{"key": "sk-abc"}))
		default:
			t.Errorf("未预期命令: %s", cmd)
		}
	})

	cred, err := CompleteLogin(context.Background(), "guid-1", "code-1", "st-1")
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	// 对话用的是建出来的 sk key，不是 JWT —— 这是最容易搞错的一处
	if cred.APIKey != "sk-abc" {
		t.Fatalf("APIKey = %q，期望 sk-abc", cred.APIKey)
	}
	if cred.JWT != "jwt-1" || cred.UID != "u-9" || cred.Nickname != "小明" {
		t.Fatalf("凭据字段错误: %+v", cred)
	}
	if cred.ChannelToken != "ch-1" || cred.GUID != "guid-1" {
		t.Fatalf("渠道/设备字段缺失: %+v", cred)
	}
	// 顺序必须是先登录再建 key
	if len(cmds) != 2 || cmds[0] != CmdWXLogin || cmds[1] != CmdCreateAPIKey {
		t.Fatalf("命令顺序错误: %v", cmds)
	}
}

func TestCompleteLoginFailsWithoutKey(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, CmdCreateAPIKey) {
			writeJSON(w, jprxOK(map[string]any{})) // 没有 key
			return
		}
		writeJSON(w, jprxOK(map[string]any{"token": "jwt-1"}))
	})
	_, err := CompleteLogin(context.Background(), "g", "c", "s")
	if err == nil || !strings.Contains(err.Error(), "sk key") {
		t.Fatalf("建 key 失败应报出来，得到: %v", err)
	}
}

func TestCompleteLoginRejectsEmptyCode(t *testing.T) {
	if _, err := CompleteLogin(context.Background(), "g", "  ", "s"); err == nil {
		t.Fatal("空 code 应本地拦截")
	}
}

func TestParseCallback(t *testing.T) {
	code, state := ParseCallback("https://security.guanjia.qq.com/login?code=abc&state=xyz")
	if code != "abc" || state != "xyz" {
		t.Fatalf("URL 解析错误: %q %q", code, state)
	}
	// 用户可能只粘了裸 code
	code, state = ParseCallback("  bare-code-123  ")
	if code != "bare-code-123" || state != "" {
		t.Fatalf("裸 code 解析错误: %q %q", code, state)
	}
	if c, _ := ParseCallback(""); c != "" {
		t.Fatal("空输入应返回空")
	}
}

/* ── 对话 ────────────────────────────────────────────────────── */

func TestChatHeaders(t *testing.T) {
	reqs := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/aizone/v1/chat/completions") {
			t.Errorf("对话路径错误: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	cred := &Credential{APIKey: "sk-abc", JWT: "jwt-1", GUID: "g-1", UID: "u-1"}
	resp, err := Chat(context.Background(), cred, []byte(`{"model":"default"}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()

	h := (*reqs)[0].Header
	if h.Get("Authorization") != "Bearer sk-abc" {
		t.Fatalf("鉴权头 = %q（对话要用 sk key 不是 JWT）", h.Get("Authorization"))
	}
	// 不带这个头上游直接 400
	if h.Get("X-Conversation-Request-ID") == "" {
		t.Fatal("缺 X-Conversation-Request-ID（上游会 400）")
	}
	if h.Get("X-Conversation-ID") == "" || h.Get("X-Conversation-Message-ID") == "" {
		t.Fatal("缺会话 id 头")
	}
	if h.Get("X-Trigger") != "webchat" || h.Get("X-Guid") != "g-1" || h.Get("X-Account") != "u-1" {
		t.Fatalf("身份头不全: %+v", h)
	}
	if !strings.HasPrefix(h.Get("User-Agent"), "QClaw/") {
		t.Fatalf("UA 不对: %s", h.Get("User-Agent"))
	}
	if h.Get("X-OpenClaw-Token") != "jwt-1" {
		t.Fatalf("缺 X-OpenClaw-Token")
	}
}

// 每次对话的 X-Conversation-Request-ID 必须不同（上游按它去重/串联）。
func TestChatRequestIDIsUnique(t *testing.T) {
	reqs := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	cred := &Credential{APIKey: "sk", GUID: "g", UID: "u"}
	for i := 0; i < 3; i++ {
		resp, err := Chat(context.Background(), cred, []byte(`{}`))
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		resp.Body.Close()
	}
	seen := map[string]bool{}
	for _, r := range *reqs {
		id := r.Header.Get("X-Conversation-Request-ID")
		if seen[id] {
			t.Fatalf("请求 id 重复: %s", id)
		}
		seen[id] = true
	}
}

/* ── 模型 ────────────────────────────────────────────────────── */

func TestListModels(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, CmdModelList) {
			t.Errorf("命令号错误: %s", r.URL.Path)
		}
		writeJSON(w, jprxOK(map[string]any{
			"model_status_list": []any{
				map[string]any{"id": "pool-glm-5.2", "name": "GLM-5.2", "description": "智谱"},
				map[string]any{"id": "pool-glm-5.2"}, // 重复，应去重
				"pool-kimi-k2.6",                     // 字符串形态
				map[string]any{"name": "无 id 的"},     // 应跳过
			},
		}))
	})
	mods, err := ListModels(context.Background(), &Credential{JWT: "j"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(mods) != 2 {
		t.Fatalf("模型数 = %d，期望 2（去重 + 跳过无 id）: %+v", len(mods), mods)
	}
	if mods[0].ID != "pool-glm-5.2" || mods[0].Name != "GLM-5.2" {
		t.Fatalf("首条解析错误: %+v", mods[0])
	}
	if mods[1].ID != "pool-kimi-k2.6" || mods[1].Name != "pool-kimi-k2.6" {
		t.Fatalf("字符串形态解析错误: %+v", mods[1])
	}
}

// 拿不到目录时回落到静态名单，而不是给个空列表（界面会显得平台是坏的）。
func TestListModelsFallsBackToStatic(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ret": 1, "msg": "boom"})
	})
	mods, err := ListModels(context.Background(), &Credential{JWT: "j"})
	if err == nil {
		t.Log("上游报错时也应带出错误（便于诊断）")
	}
	if len(mods) != len(StaticModels) {
		t.Fatalf("应回落到静态名单 %d 条，得到 %d", len(StaticModels), len(mods))
	}
}

/* ── 续期判定 ────────────────────────────────────────────────── */

func TestNeedsRefreshRequiresExpiry(t *testing.T) {
	if (&Credential{JWT: "j"}).NeedsRefresh() {
		t.Fatal("没有过期时间不该判临期（会让每个请求都去打上游）")
	}
	if (&Credential{}).NeedsRefresh() {
		t.Fatal("无 JWT 不该判临期")
	}
}

func TestJWTExpMs(t *testing.T) {
	// payload = {"exp":1893456000}
	payload := "eyJleHAiOjE4OTM0NTYwMDB9"
	tok := "h." + payload + ".s"
	if got := jwtExpMs(tok); got != 1893456000000 {
		t.Fatalf("jwtExpMs = %d", got)
	}
	if got := jwtExpMs("not-a-jwt"); got != 0 {
		t.Fatalf("非 JWT 应返回 0，得到 %d", got)
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
	if httpClient.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("代理未生效")
	}
}
