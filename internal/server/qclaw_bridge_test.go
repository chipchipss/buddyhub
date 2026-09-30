package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/qclaw"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type qclawUpstream struct {
	mu      sync.Mutex
	calls   []string
	respond func(kind string, idx int) (int, string, string)
}

func (u *qclawUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		kind := "other"
		switch {
		case strings.Contains(r.URL.Path, "/data/"):
			kind = "jprx"
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
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
		u.calls = append(u.calls, kind+"|"+r.URL.Path+"|"+peek.Model+"|"+r.Header.Get("Authorization"))
		u.mu.Unlock()

		status, ct, body := u.respond(kind, idx)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func (u *qclawUpstream) count(kind string) int {
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

func newQClawHandler(t *testing.T, up *qclawUpstream, creds ...qclaw.Credential) (*Handler, *fakeExtMgr, *extstore.Manager) {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal cred: %v", err)
		}
		id := "acct" + string(rune('0'+i))
		if c.UID != "" {
			id = c.UID
		}
		if err := mgr.Add(extstore.PQClaw, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	qclaw.SetHTTPClient(up.client())
	t.Cleanup(func() { qclaw.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	fm := &fakeExtMgr{}
	h.ExtSetManager(fm)
	return h, fm, mgr
}

func postQClaw(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

/* ── 用例 ────────────────────────────────────────────────────── */

func TestQClawNoAccounts(t *testing.T) {
	up := &qclawUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, _ := newQClawHandler(t, up)

	w := postQClaw(t, h, `{"model":"qclaw:default","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_qclaw_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	if up.count("chat") != 0 {
		t.Fatal("无账号时不该触达上游")
	}
}

func TestQClawHappyPathSSE(t *testing.T) {
	up := &qclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newQClawHandler(t, up, qclaw.Credential{APIKey: "sk-abc", GUID: "g", UID: "u"})

	w := postQClaw(t, h, `{"model":"qclaw:default","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("SSE 未透传完整: %s", w.Body.String())
	}
}

// 出站模型名必须剥掉 qclaw: 前缀（前缀是网关路由协议，上游只认裸名）。
func TestQClawRewritesModelPrefix(t *testing.T) {
	up := &qclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newQClawHandler(t, up, qclaw.Credential{APIKey: "sk", GUID: "g", UID: "u"})
	postQClaw(t, h, `{"model":"qclaw:pool-glm-5.2","stream":true,"messages":[]}`)

	up.mu.Lock()
	defer up.mu.Unlock()
	for _, c := range up.calls {
		if strings.HasPrefix(c, "chat|") {
			if got := strings.Split(c, "|")[2]; got != "pool-glm-5.2" {
				t.Fatalf("出站 model = %q，期望 pool-glm-5.2", got)
			}
			return
		}
	}
	t.Fatal("未触达 chat")
}

// 对话用建出来的 sk key（Bearer），不是 JWT —— 这是最容易搞错的一处。
func TestQClawUsesSkKeyNotJWT(t *testing.T) {
	up := &qclawUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newQClawHandler(t, up, qclaw.Credential{APIKey: "sk-real", JWT: "jwt-1", GUID: "g", UID: "u"})
	postQClaw(t, h, `{"model":"qclaw:default","stream":true,"messages":[]}`)

	up.mu.Lock()
	defer up.mu.Unlock()
	for _, c := range up.calls {
		if strings.HasPrefix(c, "chat|") {
			if !strings.HasSuffix(c, "|Bearer sk-real") {
				t.Fatalf("鉴权应为 Bearer sk-real，得到 %s", c)
			}
			return
		}
	}
	t.Fatal("未触达 chat")
}

// 缺 sk key 的账号要跳过（并说明原因），不能拿着空 Bearer 去打上游。
func TestQClawSkipsAccountWithoutKey(t *testing.T) {
	up := &qclawUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, _ := newQClawHandler(t, up, qclaw.Credential{JWT: "jwt-only", GUID: "g", UID: "u"})

	w := postQClaw(t, h, `{"model":"qclaw:default","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if up.count("chat") != 0 {
		t.Fatal("没有 sk key 不该发对话请求")
	}
	if !strings.Contains(w.Body.String(), "sk key") {
		t.Fatalf("应说明缺 sk key: %s", w.Body.String())
	}
}

// 401 时用业务域重建 key 重试一次，并把新凭据回写。
func TestQClawReissuesKeyOn401(t *testing.T) {
	up := &qclawUpstream{respond: func(kind string, idx int) (int, string, string) {
		if kind == "jprx" {
			// JPRX 成功信封 + 新的 sk key
			return 200, "application/json",
				`{"ret":0,"data":{"resp":{"common":{"code":0},"data":{"key":"sk-new"}}}}`
		}
		if idx == 0 {
			return 401, "application/json", `{"error":"unauthorized"}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, fm, _ := newQClawHandler(t, up, qclaw.Credential{APIKey: "sk-old", JWT: "jwt-1", GUID: "g", UID: "u"})

	w := postQClaw(t, h, `{"model":"qclaw:default","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("401 后应重建 key 重试成功，状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if up.count("chat") != 2 {
		t.Fatalf("chat 次数 = %d，期望 2", up.count("chat"))
	}
	// 新 key 必须回写，否则每个请求都要重建一次
	var got qclaw.Credential
	raw := fm.getRaw(extstore.PQClaw, "u")
	if len(raw) == 0 || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("凭据未回写: %s", raw)
	}
	if got.APIKey != "sk-new" {
		t.Fatalf("回写的 key = %q，期望 sk-new", got.APIKey)
	}
}

func TestQClawDisabledAccountSkipped(t *testing.T) {
	up := &qclawUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, mgr := newQClawHandler(t, up, qclaw.Credential{APIKey: "sk", GUID: "g", UID: "u"})
	accts := h.cfg.ExtAccounts()
	if len(accts) != 1 {
		t.Fatalf("前置条件：应有 1 个账号，得到 %d", len(accts))
	}
	if err := mgr.SetDisabled(extstore.PQClaw, accts[0].ID, true); err != nil {
		t.Fatalf("停用: %v", err)
	}
	w := postQClaw(t, h, `{"model":"qclaw:default","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable || up.count("chat") != 0 {
		t.Fatalf("停用账号仍被使用: code=%d chat=%d", w.Code, up.count("chat"))
	}
}

// 模型目录拿不到时回落到静态名单（界面不会显得平台是坏的）。
func TestQClawModelListedWithStaticFallback(t *testing.T) {
	up := &qclawUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "jprx" {
			return 200, "application/json", `{"ret":1,"msg":"boom"}`
		}
		return 200, "application/json", "{}"
	}}
	h, _, _ := newQClawHandler(t, up, qclaw.Credential{APIKey: "sk", GUID: "g", UID: "u"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "qclaw:default") {
		t.Fatalf("模型列表缺少静态兜底条目: %s", w.Body.String())
	}
}

func TestPlatformOfQClaw(t *testing.T) {
	if got := PlatformOf("qclaw:default"); got != "qclaw" {
		t.Fatalf("PlatformOf = %s，期望 qclaw", got)
	}
}

// 微信授权链接有 220+ 字节，必须落在二维码编码器的容量内（v10 上限 271），
// 否则扫码登录会退化成「显示一串 URL」。
func TestQClawWeChatURLFitsQrEncoder(t *testing.T) {
	const qrMaxBytes = 271
	// 与面板 start 接口同形：redirect_uri 编码后 + 32 位 state
	u := qclaw.WXQRConnect + "?appid=" + qclaw.WXAppID +
		"&redirect_uri=https%3A%2F%2Fsecurity.guanjia.qq.com%2Flogin" +
		"&response_type=code&scope=snsapi_login&state=" +
		strings.Repeat("a", 32) + "#wechat_redirect"
	if n := len([]byte(u)); n > qrMaxBytes {
		t.Fatalf("微信授权链接 %d 字节，超过二维码上限 %d", n, qrMaxBytes)
	}
}
