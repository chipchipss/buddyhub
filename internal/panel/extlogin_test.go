package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

// roundTripFn 测试用 RoundTripper（与 server 包同名类型不同包，互不干扰）。
type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jsonResponse 造一个 http.Response。
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// loginTestPanel 造一个指向临时目录、且外部账号管理器已重置的面板。
//
// extManager 是进程级单例（extOnce/extMgr），跨测试会串——这里显式重置，
// 让每个用例拿到自己的 ext-accounts.json。
func loginTestPanel(t *testing.T) *Panel {
	t.Helper()
	extOnce = sync.Once{}
	extMgr = nil
	t.Cleanup(func() { extOnce = sync.Once{}; extMgr = nil })
	return New(Config{
		Version:   "test",
		StateFile: filepath.Join(t.TempDir(), "state.json"),
	})
}

// loginPost 打一次登录接口。
func loginPost(t *testing.T, p *Panel, provider, action, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/"+provider+"/login/"+action,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

// fakeJWT 造一个只有 payload 的假 JWT（签名段随便填，本包不解签名）。
func fakeJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return "header." + enc + ".sig"
}

/* ── 小浣熊：微信扫码登录 ─────────────────────────────────────── */

func TestRaccoonLoginStartReturnsQrUrl(t *testing.T) {
	p := loginTestPanel(t)
	rec, doc := loginPost(t, p, "raccoon", "start", "")

	if rec.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", rec.Code, rec.Body.String())
	}
	if doc["mode"] != "qr" {
		t.Fatalf("mode = %v，期望 qr", doc["mode"])
	}
	session, _ := doc["session"].(string)
	if session == "" {
		t.Fatal("缺少 session")
	}
	qrURL, _ := doc["qr_url"].(string)
	if !strings.Contains(qrURL, "xiaohuanxiong.com/login/mp") {
		t.Fatalf("二维码 URL 不对: %s", qrURL)
	}
	// code 必须真的嵌进 URL，否则二维码扫了也不认
	sess := getExtLoginSession(session)
	if sess == nil || sess.qrCode == "" {
		t.Fatal("会话未保存扫码 code")
	}
	if !strings.Contains(qrURL, sess.qrCode) {
		t.Fatalf("URL 里的 code 与会话不一致: %s vs %s", qrURL, sess.qrCode)
	}
}

func TestRaccoonLoginPollPending(t *testing.T) {
	raccoon.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"data":{"status":"logging"}}`), nil
	})})
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	session := start["session"].(string)

	rec, doc := loginPost(t, p, "raccoon", "poll", `{"session":"`+session+`"}`)
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if doc["done"] != false {
		t.Fatalf("未扫码完成时不该 done: %v", doc)
	}
	if doc["status"] != "logging" {
		t.Fatalf("status = %v，期望透出上游状态", doc["status"])
	}
	// 未完成时账号不该入库
	if n := len(p.extManager().List()); n != 0 {
		t.Fatalf("未完成登录却入库了 %d 个账号", n)
	}
}

func TestRaccoonLoginSuccessAddsAccount(t *testing.T) {
	token := fakeJWT(map[string]any{"sub": "user-abc", "exp": 1893456000})
	raccoon.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"data":{"status":"success","access_token":"`+token+`","refresh_token":"rt-1"}}`), nil
	})})
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	rec, doc := loginPost(t, p, "raccoon", "poll", `{"session":"`+start["session"].(string)+`"}`)

	if rec.Code != 200 || doc["done"] != true {
		t.Fatalf("登录应成功: code=%d doc=%v", rec.Code, doc)
	}
	acct, _ := doc["account"].(map[string]any)
	if acct["id"] != "user-abc" {
		t.Fatalf("账号 ID 应取自 JWT sub，得到 %v", acct["id"])
	}

	// 凭据必须真的落库，且 token / 过期时间都在
	got := p.extManager().Find(extstore.PRaccoon, "user-abc")
	if got == nil {
		t.Fatal("账号未入库")
	}
	var cred raccoon.Credential
	if err := json.Unmarshal(got.Cred, &cred); err != nil {
		t.Fatalf("凭据解析: %v", err)
	}
	if cred.AccessToken != token || cred.RefreshToken != "rt-1" {
		t.Fatalf("凭据内容不对: %+v", cred)
	}
	// exp 秒 → 毫秒；丢了它续期判断会失效
	if cred.ExpiresAt != 1893456000000 {
		t.Fatalf("过期时间 = %d，期望 1893456000000", cred.ExpiresAt)
	}
}

func TestRaccoonLoginSessionConsumed(t *testing.T) {
	token := fakeJWT(map[string]any{"sub": "u1", "exp": 1893456000})
	raccoon.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"data":{"status":"success","access_token":"`+token+`"}}`), nil
	})})
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	session := start["session"].(string)

	loginPost(t, p, "raccoon", "poll", `{"session":"`+session+`"}`)
	// 会话用后即焚：重复 poll 必须报过期，否则会反复写库
	rec, _ := loginPost(t, p, "raccoon", "poll", `{"session":"`+session+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("已消费的会话应 404，得到 %d", rec.Code)
	}
}

func TestRaccoonLoginCanceled(t *testing.T) {
	raccoon.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"data":{"status":"canceled"}}`), nil
	})})
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	rec, _ := loginPost(t, p, "raccoon", "poll", `{"session":"`+start["session"].(string)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("取消应报错，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "取消") {
		t.Fatalf("错误信息不明确: %s", rec.Body.String())
	}
}

func TestRaccoonLoginUpstreamFlakyStaysPending(t *testing.T) {
	// 轮询期网络抖动不该判死整个登录流程
	raccoon.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})})
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	rec, doc := loginPost(t, p, "raccoon", "poll", `{"session":"`+start["session"].(string)+`"}`)
	if rec.Code != 200 || doc["done"] != false {
		t.Fatalf("上游抖动应保持 pending: code=%d doc=%v", rec.Code, doc)
	}
}

func TestRaccoonQrUrlFitsQrEncoder(t *testing.T) {
	// 面板的 QR 编码器（web/js/qr.js）做到版本 10 / ECC L，byte 模式上限 271 字节
	// （274 数据码字 − 3 字节头；v10+ 的计数指示符是 16 位，所以头是 3 字节不是 2）。
	// 超了 qrMatrix 会抛错，前端只能退化成显示一串 URL——扫码登录直接废掉。
	// 这条断言把「URL 格式变化」与「二维码静默失效」隔开：改 URL 必须同时改编码器。
	const qrMaxBytes = 271
	url := raccoon.BuildQrImageUrl(raccoon.GenerateQrCode())
	if n := len([]byte(url)); n > qrMaxBytes {
		t.Fatalf("扫码 URL %d 字节，超过 QR 编码器上限 %d：%s", n, qrMaxBytes, url)
	}
}

/* ── Qoder：设备授权登录 ─────────────────────────────────────── */

func TestQoderLoginStartReturnsAuthUrl(t *testing.T) {
	p := loginTestPanel(t)
	rec, doc := loginPost(t, p, "qoder", "start", "")

	if rec.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", rec.Code, rec.Body.String())
	}
	if doc["mode"] != "device" {
		t.Fatalf("mode = %v，期望 device", doc["mode"])
	}
	authURL, _ := doc["auth_url"].(string)
	// PKCE 四要素缺一不可：challenge / nonce / machine_id / client_id
	for _, want := range []string{"challenge=", "challenge_method=S256", "nonce=", "machine_id=", "client_id="} {
		if !strings.Contains(authURL, want) {
			t.Errorf("授权 URL 缺少 %s: %s", want, authURL)
		}
	}
	if !strings.Contains(authURL, "qoder.com") {
		t.Fatalf("授权 URL 域名不对: %s", authURL)
	}
}

func TestQoderLoginPollPendingOn404(t *testing.T) {
	// 协议层约定：404 = 尚无待授权会话（继续轮询），不是错误
	qoder.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(404, `{}`), nil
	})})
	t.Cleanup(func() { qoder.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "qoder", "start", "")
	rec, doc := loginPost(t, p, "qoder", "poll", `{"session":"`+start["session"].(string)+`"}`)
	if rec.Code != 200 || doc["done"] != false {
		t.Fatalf("404 应为 pending: code=%d doc=%v", rec.Code, doc)
	}
}

func TestQoderLoginSuccessAddsAccount(t *testing.T) {
	qoder.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/deviceToken/poll" {
			t.Errorf("轮询路径错误: %s", r.URL.Path)
		}
		// verifier 必须回传（PKCE 校验的一环）
		if r.URL.Query().Get("verifier") == "" || r.URL.Query().Get("nonce") == "" {
			t.Error("轮询缺少 verifier/nonce")
		}
		// 换行会让 JSON 里混进缩进空白，保持单行
		return jsonResponse(200, `{"access_token":"dt-abc","refresh_token":"rt",`+
			`"expire_time":1893456000000,"refresh_token_expire_time":1896057600000,`+
			`"user_id":"q-777","user_name":"阿七"}`), nil
	})})
	t.Cleanup(func() { qoder.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "qoder", "start", "")
	session := start["session"].(string)
	rec, doc := loginPost(t, p, "qoder", "poll", `{"session":"`+session+`"}`)

	if rec.Code != 200 || doc["done"] != true {
		t.Fatalf("登录应成功: code=%d doc=%v", rec.Code, doc)
	}
	acct, _ := doc["account"].(map[string]any)
	if acct["id"] != "q-777" {
		t.Fatalf("账号 ID = %v", acct["id"])
	}
	if !strings.Contains(acct["label"].(string), "阿七") {
		t.Fatalf("昵称未用于展示名: %v", acct["label"])
	}

	got := p.extManager().Find(extstore.PQoder, "q-777")
	if got == nil {
		t.Fatal("账号未入库")
	}
	var cred qoder.Credential
	if err := json.Unmarshal(got.Cred, &cred); err != nil {
		t.Fatalf("凭据解析: %v", err)
	}
	// security_oauth_token 与 access_token 双写同值（协议层取用顺序兼容）
	if cred.AccessToken != "dt-abc" || cred.SecurityOAuthToken != "dt-abc" {
		t.Fatalf("双写缺失: %+v", cred)
	}
	// machine_id 必须持久化：续期请求体要它，且参与服务端设备绑定
	if cred.MachineID == "" {
		t.Fatal("machine_id 未持久化，续期会失败")
	}
}

/* ── GitHub Copilot：设备码登录（与上两者共用统一入口） ───────── */

func TestCopilotLoginStartReturnsUserCode(t *testing.T) {
	copilot.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"device_code":"dev-1","user_code":"ABCD-1234",`+
			`"verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`), nil
	})})
	t.Cleanup(func() { copilot.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	rec, doc := loginPost(t, p, "copilot", "start", "")
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", rec.Code, rec.Body.String())
	}
	if doc["mode"] != "code" {
		t.Fatalf("mode = %v，期望 code", doc["mode"])
	}
	if doc["user_code"] != "ABCD-1234" {
		t.Fatalf("user_code = %v", doc["user_code"])
	}
	if !strings.Contains(doc["verification_uri"].(string), "github.com/login/device") {
		t.Fatalf("verification_uri = %v", doc["verification_uri"])
	}
	// 轮询间隔必须透出，否则前端只能用默认值，可能触发 slow_down
	if doc["interval"] != float64(5) {
		t.Fatalf("interval = %v，期望 5", doc["interval"])
	}
}

func TestCopilotLoginSuccessAddsAccount(t *testing.T) {
	copilot.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/login/device/code"):
			return jsonResponse(200, `{"device_code":"dev-1","user_code":"ABCD-1234",`+
				`"verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`), nil
		case strings.Contains(r.URL.Path, "/login/oauth/access_token"):
			return jsonResponse(200, `{"access_token":"gho_x"}`), nil
		case strings.Contains(r.URL.Path, "copilot_internal/v2/token"):
			return jsonResponse(200, `{"token":"cop_tok","expires_at":1893456000}`), nil
		case strings.Contains(r.URL.Path, "/user"):
			return jsonResponse(200, `{"login":"octocat","plan":{"name":"pro"}}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})})
	t.Cleanup(func() { copilot.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "copilot", "start", "")
	rec, doc := loginPost(t, p, "copilot", "poll", `{"session":"`+start["session"].(string)+`"}`)

	if rec.Code != 200 || doc["done"] != true {
		t.Fatalf("登录应成功: code=%d doc=%v", rec.Code, doc)
	}
	acct, _ := doc["account"].(map[string]any)
	if acct["id"] != "octocat" {
		t.Fatalf("账号 ID 应为 GitHub 用户名，得到 %v", acct["id"])
	}

	got := p.extManager().Find(extstore.PCopilot, "octocat")
	if got == nil {
		t.Fatal("账号未入库")
	}
	var cred copilot.Credential
	if err := json.Unmarshal(got.Cred, &cred); err != nil {
		t.Fatalf("凭据解析: %v", err)
	}
	// GitHub token 必须留着——25 分钟后要靠它续期
	if cred.GitHubToken != "gho_x" || cred.CopilotToken != "cop_tok" {
		t.Fatalf("凭据内容不对: %+v", cred)
	}
}

func TestCopilotLoginTerminalErrorSurfaced(t *testing.T) {
	copilot.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/login/device/code") {
			return jsonResponse(200, `{"device_code":"d","user_code":"U","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`), nil
		}
		return jsonResponse(200, `{"error":"access_denied"}`), nil
	})})
	t.Cleanup(func() { copilot.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "copilot", "start", "")
	rec, _ := loginPost(t, p, "copilot", "poll", `{"session":"`+start["session"].(string)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("用户拒绝授权应报错，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "拒绝") {
		t.Fatalf("错误信息不明确: %s", rec.Body.String())
	}
}

// 拿到 GitHub token 但换不到 Copilot token（无订阅）时必须**报出来**，
// 不能当成「还没授权」继续转圈——用户会一直等一个永远不会发生的结果。
func TestCopilotLoginNoSubscriptionIsTerminal(t *testing.T) {
	copilot.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/login/device/code"):
			return jsonResponse(200, `{"device_code":"d","user_code":"U",`+
				`"verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`), nil
		case strings.Contains(r.URL.Path, "/login/oauth/access_token"):
			return jsonResponse(200, `{"access_token":"gho_x"}`), nil
		case strings.Contains(r.URL.Path, "copilot_internal/v2/token"):
			return jsonResponse(403, `{"message":"no copilot subscription"}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})})
	t.Cleanup(func() { copilot.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "copilot", "start", "")
	rec, _ := loginPost(t, p, "copilot", "poll", `{"session":"`+start["session"].(string)+`"}`)

	if rec.Code == 200 {
		t.Fatalf("换不到 Copilot token 应报错而不是继续 pending，得到: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未订阅") {
		t.Fatalf("错误原因没透出: %s", rec.Body.String())
	}
}

// 轮询期的单次网络抖动必须保持 pending；连续失败才判定终态。
func TestCopilotLoginTransientErrorKeepsPending(t *testing.T) {
	copilot.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/login/device/code") {
			return jsonResponse(200, `{"device_code":"d","user_code":"U",`+
				`"verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`), nil
		}
		return nil, io.ErrUnexpectedEOF
	})})
	t.Cleanup(func() { copilot.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "copilot", "start", "")
	session := start["session"].(string)

	// 前 maxLoginErrStreak-1 次仍应是 pending
	for i := 1; i < maxLoginErrStreak; i++ {
		rec, doc := loginPost(t, p, "copilot", "poll", `{"session":"`+session+`"}`)
		if rec.Code != 200 || doc["done"] != false {
			t.Fatalf("第 %d 次抖动不该判死: code=%d doc=%v", i, rec.Code, doc)
		}
		if s, _ := doc["status"].(string); !strings.Contains(s, "重试中") {
			t.Fatalf("第 %d 次应提示重试中，得到 %q", i, s)
		}
	}
	// 第 maxLoginErrStreak 次判终态，把原因带给用户
	rec, _ := loginPost(t, p, "copilot", "poll", `{"session":"`+session+`"}`)
	if rec.Code == 200 {
		t.Fatalf("连续失败 %d 次后应报错，得到: %s", maxLoginErrStreak, rec.Body.String())
	}
}

/* ── 通用行为 ────────────────────────────────────────────────── */

func TestExtLoginUnsupportedProvider(t *testing.T) {
	p := loginTestPanel(t)
	rec, _ := loginPost(t, p, "codearts", "start", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未支持登录的平台应 400，得到 %d", rec.Code)
	}
}

func TestExtLoginUnknownSession(t *testing.T) {
	p := loginTestPanel(t)
	rec, _ := loginPost(t, p, "raccoon", "poll", `{"session":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知会话应 404，得到 %d", rec.Code)
	}
}

func TestExtLoginSessionNotCrossProvider(t *testing.T) {
	// raccoon 的 session 不能拿去轮询 qoder —— 否则会把会话状态机搞乱
	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	rec, _ := loginPost(t, p, "qoder", "poll", `{"session":"`+start["session"].(string)+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨平台复用会话应 404，得到 %d", rec.Code)
	}
}

func TestExtLoginBadJSON(t *testing.T) {
	p := loginTestPanel(t)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/raccoon/login/poll",
		bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，得到 %d", rec.Code)
	}
}

/* ── 协议层新增的解析工具 ────────────────────────────────────── */

func TestDecodeJWTUserID(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"sub", map[string]any{"sub": "s-1"}, "s-1"},
		{"uid 优先于 id", map[string]any{"id": "i-1", "uid": "u-1"}, "u-1"},
		{"数字 id", map[string]any{"user_id": float64(12345)}, "12345"},
		{"非整数数字不硬转", map[string]any{"id": 1.5, "uid": "fallback"}, "fallback"},
		{"空串跳过", map[string]any{"sub": "", "uid": "u-2"}, "u-2"},
		{"全落空", map[string]any{"foo": "bar"}, ""},
	}
	for _, c := range cases {
		if got := raccoon.DecodeJWTUserID(fakeJWT(c.claims)); got != c.want {
			t.Errorf("%s: 得到 %q，期望 %q", c.name, got, c.want)
		}
	}
	if got := raccoon.DecodeJWTUserID("not-a-jwt"); got != "" {
		t.Errorf("非 JWT 应返回空串，得到 %q", got)
	}
}

func TestTokenDigestIsStable(t *testing.T) {
	// 兜底 ID 必须对同一 token 稳定，否则每次登录都会新增一个账号
	a := raccoon.TokenDigest("tok-1")
	if a != raccoon.TokenDigest("tok-1") {
		t.Fatal("同一 token 的摘要不稳定")
	}
	if a == raccoon.TokenDigest("tok-2") {
		t.Fatal("不同 token 摘要碰撞")
	}
	if len(a) != 8 {
		t.Fatalf("摘要长度 = %d，期望 8", len(a))
	}
}

func TestRaccoonLoginFallsBackToDigestID(t *testing.T) {
	// JWT 里没有任何用户标识 claim 时，用 token 摘要兜底（而不是随机值）
	token := fakeJWT(map[string]any{"exp": 1893456000})
	raccoon.SetHTTPClient(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"data":{"status":"success","access_token":"`+token+`"}}`), nil
	})})
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	p := loginTestPanel(t)
	_, start := loginPost(t, p, "raccoon", "start", "")
	_, doc := loginPost(t, p, "raccoon", "poll", `{"session":"`+start["session"].(string)+`"}`)
	acct, _ := doc["account"].(map[string]any)
	want := "raccoon-" + raccoon.TokenDigest(token)
	if acct["id"] != want {
		t.Fatalf("兜底 ID = %v，期望 %v", acct["id"], want)
	}
}
