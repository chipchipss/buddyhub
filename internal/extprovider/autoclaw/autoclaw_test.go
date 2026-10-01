package autoclaw

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type rewriteTransport struct {
	target *httptest.Server
	seen   *[]*http.Request
	bodies *[]string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.target.URL, "http://")
	if t.seen != nil {
		*t.seen = append(*t.seen, req)
	}
	if t.bodies != nil {
		var raw []byte
		if req.Body != nil {
			raw, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		*t.bodies = append(*t.bodies, string(raw))
	}
	return http.DefaultTransport.RoundTrip(req)
}

type recorder struct {
	reqs   []*http.Request
	bodies []string
}

func withMock(t *testing.T, h http.HandlerFunc) *recorder {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	rec := &recorder{}
	old := httpClient
	SetHTTPClient(&http.Client{Transport: &rewriteTransport{target: srv, seen: &rec.reqs, bodies: &rec.bodies}})
	t.Cleanup(func() { httpClient = old })
	return rec
}

func (r *recorder) last() *http.Request {
	if len(r.reqs) == 0 {
		return nil
	}
	return r.reqs[len(r.reqs)-1]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

/* ── 客户端指纹 ──────────────────────────────────────────────── */

// 签名必须与官方客户端逐字节一致：X-Auth-Sign = MD5("appId&ts&appKey")，
// ts 是**秒**（不是毫秒）。签名错时上游回 code 400002。
func TestSignedHeaderIsByteExact(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"result": true}})
	})

	before := time.Now().Unix()
	if _, err := SendCode(context.Background(), RegionCN, "13800000000"); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	after := time.Now().Unix()

	req := rec.last()
	if req == nil {
		t.Fatal("未发出请求")
	}
	ts := req.Header.Get("X-Auth-TimeStamp")
	n, err := parseInt64(ts)
	if err != nil {
		t.Fatalf("时间戳不是整数: %q", ts)
	}
	// 秒级时间戳：落在调用前后的秒数窗口内
	if n < before || n > after {
		t.Fatalf("时间戳 %d 不在 [%d,%d] —— 用了毫秒？", n, before, after)
	}
	want := md5.Sum([]byte(AuthAppID + "&" + ts + "&" + AuthAppKey))
	if got := req.Header.Get("X-Auth-Sign"); got != hex.EncodeToString(want[:]) {
		t.Fatalf("签名不对: got %s want %s", got, hex.EncodeToString(want[:]))
	}
	if req.Header.Get("X-Auth-Appid") != AuthAppID {
		t.Fatalf("appId 头不对: %s", req.Header.Get("X-Auth-Appid"))
	}
	// X-Version 是模型目录的版本门控，缺了只下发 3–4 条模型
	if req.Header.Get("X-Version") != ClientVersion {
		t.Fatalf("缺 X-Version（模型目录会被门控）: %q", req.Header.Get("X-Version"))
	}
}

// userapi 域要 X-Harness-Type，chat 域**不能**带 —— 上游对 /chat/completions
// 上的这个值区别对待（403 pay-view / 406）。两条链路刻意不一致。
func TestHarnessTypeOnlyOnUserAPI(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"result": true}})
	})
	if _, err := SendCode(context.Background(), RegionCN, "13800000000"); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	if rec.last().Header.Get("X-Harness-Type") != "zcode" {
		t.Fatalf("userapi 域应带 X-Harness-Type: zcode")
	}

	// chat 域
	rec2 := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	resp, err := Chat(context.Background(), &Credential{Region: RegionCN, Token: "tok"}, "glm-5.3",
		[]byte(`{"model":"glm-5.3"}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()
	if got := rec2.last().Header.Get("X-Harness-Type"); got != "" {
		t.Fatalf("chat 域不该带 X-Harness-Type（上游 403/406），得到 %q", got)
	}
	if rec2.last().Header.Get("X-Authorization") != "Bearer tok" {
		t.Fatalf("chat 鉴权头不对: %s", rec2.last().Header.Get("X-Authorization"))
	}
	if rec2.last().Header.Get("X-Request-Model") != "glm-5.3" {
		t.Fatalf("缺 X-Request-Model")
	}
	if rec2.last().Header.Get("X-Version") != ClientVersion {
		t.Fatalf("chat 域也带 X-Version（官方客户端所有请求都发）")
	}
}

/* ── 地区 ────────────────────────────────────────────────────── */

func TestRegionEndpoints(t *testing.T) {
	if got := RegionCN.UserAPI(); got != "https://autoglm-acceleration-api.zhipuai.cn" {
		t.Errorf("国内 userapi = %s", got)
	}
	if got := RegionIntl.UserAPI(); got != "https://autoglm-api.autoglm.ai" {
		t.Errorf("国际 userapi = %s", got)
	}
	// LLM 代理在 userapi 同 host 的 /autoclaw-proxy/proxy/autoclaw
	for _, r := range []Region{RegionCN, RegionIntl} {
		if !strings.HasSuffix(r.ChatBase(), "/autoclaw-proxy/proxy/autoclaw") {
			t.Errorf("%s ChatBase = %s", r, r.ChatBase())
		}
		if !strings.HasPrefix(r.ChatBase(), r.UserAPI()) {
			t.Errorf("%s ChatBase 与 userapi 不同 host", r)
		}
	}
	if !RegionCN.SupportsSMS() {
		t.Error("国内版应支持短信登录")
	}
	if RegionIntl.SupportsSMS() {
		t.Error("国际版上游已关闭短信入口")
	}
}

func TestParseRegion(t *testing.T) {
	cases := map[string]Region{"": RegionCN, "cn": RegionCN, "CN": RegionCN, "intl": RegionIntl, "INTL": RegionIntl, "  intl ": RegionIntl, "garbage": RegionCN}
	for in, want := range cases {
		if got := ParseRegion(in); got != want {
			t.Errorf("ParseRegion(%q) = %v，期望 %v", in, got, want)
		}
	}
}

/* ── 短信登录 ────────────────────────────────────────────────── */

func TestSendCodeAndLoginShareDeviceID(t *testing.T) {
	var seenDeviceIDs []string
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenDeviceIDs = append(seenDeviceIDs, body["device_id"])
		if strings.Contains(r.URL.Path, "send-code") {
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"result": true}})
			return
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"token": "at-1", "refresh_token": "rt-1", "user_id": "u-1",
		}})
	})

	deviceID, err := SendCode(context.Background(), RegionCN, "13800000000")
	if err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	if deviceID == "" {
		t.Fatal("SendCode 应返回 device_id")
	}
	cred, err := LoginWithCode(context.Background(), RegionCN, "13800000000", "123456", deviceID)
	if err != nil {
		t.Fatalf("LoginWithCode: %v", err)
	}
	// 上游把设备与登录会话绑定：两步必须用同一个 device_id
	if len(seenDeviceIDs) != 2 || seenDeviceIDs[0] != seenDeviceIDs[1] {
		t.Fatalf("device_id 不一致: %v", seenDeviceIDs)
	}
	if cred.Token != "at-1" || cred.RefreshToken != "rt-1" || cred.UserID != "u-1" {
		t.Fatalf("凭据解析错误: %+v", cred)
	}
	if cred.Region != RegionCN {
		t.Fatalf("地区未写入凭据: %v", cred.Region)
	}
	if cred.PhoneTail != "138****0000" {
		t.Fatalf("手机号掩码错误: %q", cred.PhoneTail)
	}
	// 登录路径不带结尾斜杠（带了会被 307 重定向一次，见 loginPath 注释）
	if strings.HasSuffix(rec.reqs[1].URL.Path, "/") {
		t.Fatalf("登录路径带结尾斜杠（上游会 307）: %s", rec.reqs[1].URL.Path)
	}
}

func TestLoginRejectsBadCode(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 1002, "msg": "验证码错误"})
	})
	if _, err := LoginWithCode(context.Background(), RegionCN, "13800000000", "123456", "dev"); err == nil {
		t.Fatal("业务码非 0 应报错")
	} else if !strings.Contains(err.Error(), "验证码错误") {
		t.Fatalf("错误信息不明确: %v", err)
	}
	// 非 6 位验证码本地就拦掉，不打上游
	if _, err := LoginWithCode(context.Background(), RegionCN, "13800000000", "12", "dev"); err == nil {
		t.Fatal("非 6 位验证码应本地拦截")
	}
}

func TestIntlRegionRejectsSMS(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("国际版不该发出请求")
	})
	if _, err := SendCode(context.Background(), RegionIntl, "13800000000"); err == nil {
		t.Fatal("国际版应拒绝短信登录")
	}
	if _, err := LoginWithCode(context.Background(), RegionIntl, "13800000000", "123456", "d"); err == nil {
		t.Fatal("国际版应拒绝短信登录")
	}
}

/* ── 续期 ────────────────────────────────────────────────────── */

func TestRefreshRotatesToken(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"token": "at-2", "refresh_token": "rt-2", "expires_at": float64(1893456000000),
		}})
	})
	old := &Credential{Region: RegionCN, Token: "at-1", RefreshToken: "rt-1", DeviceID: "dev-1", PhoneTail: "138****0000"}
	fresh, err := Refresh(context.Background(), old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.Token != "at-2" || fresh.RefreshToken != "rt-2" {
		t.Fatalf("令牌未更新: %+v", fresh)
	}
	if fresh.ExpiresAt != 1893456000000 {
		t.Fatalf("过期时间解析错误: %d", fresh.ExpiresAt)
	}
	// 展示字段与设备标识必须保留
	if fresh.PhoneTail != "138****0000" || fresh.DeviceID != "dev-1" {
		t.Fatalf("续期丢失了字段: %+v", fresh)
	}
	if old.Token != "at-1" {
		t.Fatal("Refresh 不应修改入参")
	}
	// 刷新走签名头
	if rec.last().Header.Get("X-Auth-Sign") == "" {
		t.Fatal("刷新缺签名头")
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(rec.bodies[0]), &body)
	if body["source_id"] != SourceID || body["refresh_token"] != "rt-1" {
		t.Fatalf("刷新请求体错误: %v", body)
	}
}

// 签名校验失败（code 400002）要降级到 agent-refresh —— 上游对签名头的兜底路径。
func TestRefreshFallsBackOnSignatureFailure(t *testing.T) {
	var paths []string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/refresh") {
			writeJSON(w, map[string]any{"code": 400002, "msg": "签名校验失败"})
			return
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"token": "at-2"}})
	})
	fresh, err := Refresh(context.Background(), &Credential{Region: RegionCN, Token: "at-1", RefreshToken: "rt-1"})
	if err != nil {
		t.Fatalf("降级后应成功: %v", err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[1], "/agent-refresh") {
		t.Fatalf("未降级到 agent-refresh: %v", paths)
	}
	if fresh.Token != "at-2" {
		t.Fatalf("token 未更新: %+v", fresh)
	}
}

func TestRefreshWithoutToken(t *testing.T) {
	if _, err := Refresh(context.Background(), &Credential{}); err == nil {
		t.Fatal("缺 refresh_token 应报错")
	}
}

// 没有过期时间就不刷新 —— 没有依据就说「临期」会让每个请求都去打一次刷新接口。
func TestNeedsRefreshRequiresExpiry(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		cred Credential
		want bool
	}{
		{"无过期时间", Credential{Token: "t"}, false},
		{"空 token", Credential{}, false},
		{"剩 1 分钟", Credential{Token: "t", ExpiresAt: now.Add(time.Minute).UnixMilli()}, true},
		{"剩 30 分钟", Credential{Token: "t", ExpiresAt: now.Add(30 * time.Minute).UnixMilli()}, false},
	}
	for _, c := range cases {
		if got := c.cred.NeedsRefresh(); got != c.want {
			t.Errorf("%s: %v，期望 %v", c.name, got, c.want)
		}
	}
}

/* ── 模型目录 ────────────────────────────────────────────────── */

func TestListModels(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/autoclaw-model-config") {
			t.Errorf("目录路径错误: %s", r.URL.Path)
		}
		// 顶层直接是 {"models":[...]}，没有 {code,data} 信封
		writeJSON(w, map[string]any{"models": []map[string]any{
			{"id": "zai_glm-5.3", "name": "GLM-5.3"},
			{"model_id": "tdpsk_deepseek-v4-flash-202605", "name": "DeepSeek V4 Flash"},
			{"name": "无 id 的条目"},
		}})
	})
	mods, err := ListModels(context.Background(), RegionCN, "tok")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(mods) != 2 {
		t.Fatalf("模型数 = %d，期望 2（无 id 的应跳过）", len(mods))
	}
	if mods[0].ID != "zai_glm-5.3" || mods[1].ID != "tdpsk_deepseek-v4-flash-202605" {
		t.Fatalf("id 解析错误: %+v", mods)
	}
	// 版本门控头必须带上，否则只下发 3–4 条
	if rec.last().Header.Get("X-Version") != ClientVersion {
		t.Fatal("目录请求缺 X-Version（会被版本门控）")
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

/* ── 小工具 ──────────────────────────────────────────────────── */

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("13800000000"); got != "138****0000" {
		t.Errorf("MaskPhone = %q", got)
	}
	if got := MaskPhone("123"); got != "123" {
		t.Errorf("短号不该掩码: %q", got)
	}
}

func parseInt64(s string) (int64, error) {
	var n int64
	err := json.Unmarshal([]byte(s), &n)
	return n, err
}

// 用户从各处复制来的号码常带空格/横线/+86 —— 原样透传上游会判「格式不正确」。
func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"13800000000":         "13800000000",
		"+8613800000000":      "13800000000",
		"8613800000000":       "13800000000",
		"138 0000 0000":       "13800000000",
		"138-0000-0000":       "13800000000",
		" +86 138 0000 0000 ": "13800000000",
	}
	for in, want := range ok {
		got, err := NormalizePhone(in)
		if err != nil {
			t.Errorf("%q 应通过，却报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
	bad := []string{"", "1380000000", "138000000000", "23800000000", "1380000000a", "+1 415 555 0100"}
	for _, in := range bad {
		if _, err := NormalizePhone(in); err == nil {
			t.Errorf("%q 应被拒绝", in)
		}
	}
}

// 发码与登录两步都必须用规范化后的号码（上游两步校验一致）。
func TestSendCodeNormalizesPhone(t *testing.T) {
	var sent string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body["phone"]
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"result": true}})
	})
	if _, err := SendCode(context.Background(), RegionCN, "+86 138-0000-0000"); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	if sent != "13800000000" {
		t.Fatalf("上游收到的是 %q，期望规范化后的 13800000000", sent)
	}
}

// ⚠️ 上游要求 body 里的 `code` 是 **JSON 数字**：传字符串回
// `400001 请求数据有问题`（参数错误），传数字才进入业务判断
// （630201 已过期 / 630202 不正确）。这个类型错会让用户拿着**正确**的
// 验证码也永远登不上，且错误文案被误译成「验证码不正确」，把排查方向带偏。
func TestLoginCodeIsJSONNumber(t *testing.T) {
	var raw string
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		writeJSON(w, map[string]any{"code": 630202, "msg": "验证码错误"})
	})
	_, _ = LoginWithCode(context.Background(), RegionCN, "13800000000", "123456", "dev")

	// 直接看原始 JSON：`"code":123456` 合法，`"code":"123456"` 不合法
	if !strings.Contains(raw, `"code":123456`) {
		t.Fatalf("code 必须是 JSON 数字，实际发出的是: %s", raw)
	}
	if strings.Contains(raw, `"code":"`) {
		t.Fatalf("code 被序列化成了字符串（上游会回 400001）: %s", raw)
	}
}

// 400001 是**参数错误**（请求没被受理），不是验证码错误 —— 文案不能把用户
// 引去反复重发验证码；630xxx 才是真的在讨论验证码。
func TestLoginErrorIsActionable(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 400001, "msg": "请求数据有问题,请检查后重试"})
	})
	_, err := LoginWithCode(context.Background(), RegionCN, "13800000000", "123456", "dev")
	if err == nil {
		t.Fatal("非 0 业务码应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "400001") {
		t.Errorf("错误里要留着上游码便于排障: %q", msg)
	}
	if strings.Contains(msg, "请求数据有问题,请检查后重试") {
		t.Errorf("别把上游原样那句没法行动的话抛给用户: %q", msg)
	}
	if strings.Contains(msg, "重发") || strings.Contains(msg, "重新发送") {
		t.Errorf("400001 是参数错误，不该让用户去重发验证码: %q", msg)
	}

	// 对照：630202 才是验证码错，文案要给出可行动的方向
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 630202, "msg": "抱歉,验证码错误，请输入正确验证码！"})
	})
	_, err = LoginWithCode(context.Background(), RegionCN, "13800000000", "123456", "dev")
	if err == nil || !strings.Contains(err.Error(), "验证码不正确") {
		t.Errorf("630202 应译成验证码不正确，得到: %v", err)
	}
}
