package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type raccoonUpstream struct {
	mu        sync.Mutex
	calls     []string
	chatBody  string
	chatTries int
	respond   func(kind string) (int, string, string)
}

// attempt 返回第几次 chat 调用（0 起），供 respond 用来模拟「第一次 401、
// 续期后第二次成功」。
func (u *raccoonUpstream) attempt() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chatTries - 1
}

func (u *raccoonUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		kind := "other"
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			kind = "chat"
		case strings.HasSuffix(r.URL.Path, "/model_catalog"):
			kind = "catalog"
		case strings.HasSuffix(r.URL.Path, "/auth/v1/refresh"):
			kind = "refresh"
		}
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		u.mu.Lock()
		if kind == "chat" {
			u.chatBody = string(raw)
			u.chatTries++
		}
		u.calls = append(u.calls, kind+"|"+r.Method+"|"+r.URL.Path+"|"+r.Header.Get("Authorization"))
		u.mu.Unlock()

		status, ct, body := u.respond(kind)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func (u *raccoonUpstream) count(kind string) int {
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

func (u *raccoonUpstream) lastCall(kind string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(u.calls[i], kind+"|") {
			return u.calls[i]
		}
	}
	return ""
}

func newRaccoonHandler(t *testing.T, up *raccoonUpstream, creds ...raccoon.Credential) (*Handler, *fakeExtMgr) {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal cred: %v", err)
		}
		id := c.UserID
		if id == "" {
			id = "raccoon-" + string(rune('0'+i))
		}
		if err := mgr.Add(extstore.PRaccoon, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	raccoon.SetHTTPClient(up.client())
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	fm := &fakeExtMgr{}
	h.ExtSetManager(fm)
	return h, fm
}

func postRaccoon(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

/* ── 用例 ────────────────────────────────────────────────────── */

// 没账号时必须 503 + 明确错误码，且**一次上游请求都不能发**。
func TestRaccoonNoAccounts(t *testing.T) {
	up := &raccoonUpstream{respond: func(string) (int, string, string) {
		return 200, "application/json", "{}"
	}}
	h, _ := newRaccoonHandler(t, up)

	w := postRaccoon(t, h, `{"model":"raccoon:raccoon-8c4485","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_raccoon_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	if n := up.count("chat"); n != 0 {
		t.Fatalf("没有账号时不该打上游，实际打了 %d 次", n)
	}
}

// 上游回的是 OpenAI 形状直通，但**响应帧的 model 会被上游换成内部名** ——
// 不回写的话客户端会看到一个它从没请求过的模型名（参考实现
// `raccoon-sse-pipe.mjs` 的 pipeSseWithModelRewrite 专处理这件事）。
func TestRaccoonRewritesModelInSSE(t *testing.T) {
	const clientModel = "raccoon:raccoon-8c4485"
	const upstreamModel = "internal-upstream-name-9f2"
	up := &raccoonUpstream{respond: func(kind string) (int, string, string) {
		if kind == "chat" {
			frame := `data: {"id":"c1","object":"chat.completion.chunk","model":"` + upstreamModel +
				`","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
				"data: [DONE]\n\n"
			return 200, "text/event-stream", frame
		}
		return 200, "application/json", `{"data":{"categories":[]}}`
	}}
	h, _ := newRaccoonHandler(t, up, raccoon.Credential{AccessToken: "tok-1", UserID: "u1"})

	w := postRaccoon(t, h, `{"model":"`+clientModel+`","stream":true,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, upstreamModel) {
		t.Fatalf("上游内部模型名泄漏给了客户端: %s", body)
	}
	// 回写的目标是**我们发给上游的那个名字**（剥掉 `raccoon:` 前缀后的），
	// 不是客户端请求的全名 —— 这与网关其它通道的口径一致（qoder/copilot 同）：
	// 前缀是网关自己的路由协议，不进入上游协议，也不回流。
	if !strings.Contains(body, `"model":"raccoon-8c4485"`) {
		t.Fatalf("应回写成发往上游的模型名，实际: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("[DONE] 哨兵不该被吃掉: %s", body)
	}
	// 出站体原样透传（raccoon 不做白名单重建）
	if !strings.Contains(up.chatBody, `"messages":[]`) {
		t.Fatalf("请求体没被透传: %s", up.chatBody)
	}
	// 只有 3 个头 + Bearer
	call := up.lastCall("chat")
	if !strings.HasSuffix(call, "|Bearer tok-1") {
		t.Fatalf("鉴权头不对: %s", call)
	}
}

// 非流式：本地聚合，model 同样要回写。
func TestRaccoonNonStreamAggregatesAndRewrites(t *testing.T) {
	up := &raccoonUpstream{respond: func(kind string) (int, string, string) {
		if kind == "chat" {
			frame := `data: {"id":"c1","object":"chat.completion.chunk","model":"internal-xyz",` +
				`"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
			return 200, "text/event-stream", frame
		}
		return 200, "application/json", `{"data":{"categories":[]}}`
	}}
	h, _ := newRaccoonHandler(t, up, raccoon.Credential{AccessToken: "tok-1", UserID: "u1"})

	w := postRaccoon(t, h, `{"model":"raccoon:raccoon-8c4485","stream":false,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "internal-xyz") {
		t.Fatalf("内部模型名泄漏: %s", body)
	}
	if !strings.Contains(body, "chat.completion") {
		t.Fatalf("应聚合为非流式 completion: %s", body)
	}
}

// 上游 401 → 续期 → 同账号重试一次。
func TestRaccoonRefreshesOn401AndRetries(t *testing.T) {
	// 先声明再赋值：respond 闭包要回看 up.chatTries
	var up *raccoonUpstream
	up = &raccoonUpstream{respond: func(kind string) (int, string, string) {
		switch kind {
		case "refresh":
			// raccoon.do() 要 {code:0,data:{...}} 信封
			return 200, "application/json",
				`{"code":0,"data":{"access_token":"new-at","refresh_token":"new-rt"}}`
		case "chat":
			// 第一次 401，续期后的第二次成功
			if up.attempt() == 0 {
				return 401, "application/json", `{"message":"token expired"}`
			}
			return 200, "text/event-stream", "data: [DONE]\n\n"
		case "catalog":
			return 401, "application/json", `{"message":"unauthorized"}`
		}
		return 404, "text/html", "404"
	}}
	h, fm := newRaccoonHandler(t, up, raccoon.Credential{
		AccessToken: "old", RefreshToken: "rt-1", UserID: "u1",
	})

	w := postRaccoon(t, h, `{"model":"raccoon:raccoon-8c4485","stream":true,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("续期后重试应成功，状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	var cred raccoon.Credential
	if raw := fm.getRaw(extstore.PRaccoon, "u1"); len(raw) == 0 {
		t.Fatal("续期成功后应把新凭据回写账号表（否则下次还要再续一次）")
	} else if err := json.Unmarshal(raw, &cred); err != nil {
		t.Fatalf("回写凭据解析失败: %v", err)
	}
	if cred.RefreshToken == "" {
		t.Error("回写的凭据里没有 refresh_token，下次续期会失败")
	}
	if got := up.attempt(); got < 1 {
		t.Errorf("401 后应重试一次，实际 chat 调用序号 = %d", got)
	}
}

// 目录：远程拉不到时**必须**回落静态兜底，不能返回空
// （空目录会让整条通道在 /v1/models 里「不存在」）。
func TestRaccoonModelListFallsBackWhenRemoteFails(t *testing.T) {
	up := &raccoonUpstream{respond: func(kind string) (int, string, string) {
		if kind == "catalog" {
			return 401, "application/json", `{"message":"unauthorized"}`
		}
		return 200, "text/event-stream", "data: [DONE]\n\n"
	}}
	h, _ := newRaccoonHandler(t, up, raccoon.Credential{AccessToken: "tok", UserID: "u1"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "raccoon:raccoon-8c4485") {
		t.Fatalf("远程目录失败时应回落静态兜底，实际: %s", w.Body.String())
	}
}

// PlatformOf 必须认得 raccoon 前缀——漏了会让带平台授权的 API key 对
// 所有 raccoon:* 模型返回 403。
func TestPlatformOfRaccoon(t *testing.T) {
	if got := PlatformOf("raccoon:raccoon-8c4485"); got != "raccoon" {
		t.Fatalf("PlatformOf = %q, 期望 raccoon", got)
	}
	if !isRaccoonModel("raccoon:x") {
		t.Fatal("isRaccoonModel 认不出自己的前缀")
	}
	if isRaccoonModel("bare") {
		t.Fatal("isRaccoonModel 劫持了裸模型名")
	}
}
