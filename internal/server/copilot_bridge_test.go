package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 测试脚手架 ──────────────────────────────────────────────── */

// copilotUpstream 可编程的 Copilot 上游：按调用序返回预设响应，并记录每次请求。
type copilotUpstream struct {
	mu    sync.Mutex
	calls []copilotCall
	// respond 返回 (状态码, Content-Type, 响应体)；idx 为第几次 chat 调用（从 0 起）。
	respond func(kind string, idx int) (int, string, string)
}

type copilotCall struct {
	kind  string // "token" | "chat" | "models"
	auth  string
	body  string
	model string
}

func (u *copilotUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var kind string
		switch {
		case strings.Contains(r.URL.Path, "copilot_internal/v2/token"):
			kind = "token"
		case strings.Contains(r.URL.Path, "/models"):
			kind = "models"
		case strings.Contains(r.URL.Path, "/chat/completions"):
			kind = "chat"
		default:
			kind = "other"
		}
		// GET 请求（兑换 token / 拉模型）没有 body，ReadAll(nil) 会 panic。
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
			if c.kind == kind {
				idx++
			}
		}
		u.calls = append(u.calls, copilotCall{
			kind: kind, auth: r.Header.Get("authorization"),
			body: string(raw), model: peek.Model,
		})
		u.mu.Unlock()

		status, ct, body := u.respond(kind, idx)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func (u *copilotUpstream) count(kind string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, c := range u.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// fakeExtMgr 记录 ReplaceCred 回写（验证续期后凭据落库）。
type fakeExtMgr struct {
	mu    sync.Mutex
	last  map[string]json.RawMessage
	notes []noteRec
}

func (f *fakeExtMgr) ReplaceCred(provider, id string, cred json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[string]json.RawMessage{}
	}
	f.last[provider+"/"+id] = cred
}

// NoteChatResult 记录桥接上报的对话结果（供断言冷却/退避是否被触发）。
func (f *fakeExtMgr) NoteChatResult(provider, id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, noteRec{provider: provider, id: id, failed: err != nil})
}

// noteRec 一次上报的记录。
type noteRec struct {
	provider string
	id       string
	failed   bool
}

// noteRecords 返回已记录的上报（并发安全快照）。
func (f *fakeExtMgr) noteRecords() []noteRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]noteRec, len(f.notes))
	copy(out, f.notes)
	return out
}

// getRaw 取回写的原始凭据（各通道自解各家的 Credential 形状）。
func (f *fakeExtMgr) getRaw(provider, id string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[provider+"/"+id]
}

func (f *fakeExtMgr) get(provider, id string) *copilot.Credential {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.last[provider+"/"+id]
	if !ok {
		return nil
	}
	var c copilot.Credential
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return &c
}

// newCopilotHandler 装配一个只挂 Copilot 账号的 handler。
// 返回 handler、凭据回写记录器、以及底层 extstore 管理器（改启用状态用）。
func newCopilotHandler(t *testing.T, up *copilotUpstream, creds ...copilot.Credential) (*Handler, *fakeExtMgr, *extstore.Manager) {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal cred: %v", err)
		}
		id := "acct" + string(rune('0'+i))
		if c.Login != "" {
			id = c.Login
		}
		if err := mgr.Add(extstore.PCopilot, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	copilot.SetHTTPClient(up.client())
	t.Cleanup(func() { copilot.SetHTTPClient(&http.Client{}) })

	// Pool/Upstream 是 /v1/models 与其它路由的装配前提（Copilot 分支本身不用），
	// 留空会 nil 解引用。
	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	fm := &fakeExtMgr{}
	h.ExtSetManager(fm)
	return h, fm, mgr
}

// postCopilot 走 /v1/chat/completions 的 Copilot 分支，返回 recorder。
func postCopilot(t *testing.T, h *Handler, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

/* ── 用例 ────────────────────────────────────────────────────── */

func TestCopilotNoAccounts(t *testing.T) {
	up := &copilotUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, _ := newCopilotHandler(t, up) // 无账号

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_copilot_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	if up.count("chat") != 0 {
		t.Fatal("无账号时不应触达上游")
	}
}

func TestCopilotDisabledAccountSkipped(t *testing.T) {
	up := &copilotUpstream{respond: func(string, int) (int, string, string) { return 200, "application/json", "{}" }}
	h, _, mgr := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "gho_1", CopilotToken: "cop_1", ExpiresAt: future()})

	accts := h.cfg.ExtAccounts()
	if len(accts) != 1 {
		t.Fatalf("前置条件：应有 1 个账号，得到 %d", len(accts))
	}
	if err := mgr.SetDisabled(extstore.PCopilot, accts[0].ID, true); err != nil {
		t.Fatalf("停用: %v", err)
	}

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("停用账号仍被使用：状态码 %d", w.Code)
	}
	if up.count("chat") != 0 {
		t.Fatal("停用账号不应触达上游")
	}
}

func TestCopilotHappyPathSSE(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		switch kind {
		case "chat":
			return 200, "text/event-stream", sseOK
		default:
			return 200, "application/json", `{"token":"cop_new","expires_at":` + itoa64(future()) + `}`
		}
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "gho_1", CopilotToken: "cop_1", ExpiresAt: future()})

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %s", ct)
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("SSE 未透传完整: %s", w.Body.String())
	}
	if up.count("token") != 0 {
		t.Fatal("token 未过期时不应续期")
	}
}

func TestCopilotRewritesModelPrefix(t *testing.T) {
	up := &copilotUpstream{respond: func(string, int) (int, string, string) {
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "g", CopilotToken: "cop_1", ExpiresAt: future()})

	postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[]}`)

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.calls) == 0 {
		t.Fatal("未触达上游")
	}
	// 前缀是网关侧路由协议，出站必须是裸模型名
	if got := up.calls[len(up.calls)-1].model; got != "gpt-4o" {
		t.Fatalf("出站 model = %q, 期望 gpt-4o", got)
	}
}

func TestCopilotAutoRefreshBeforeExpiry(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "token" {
			return 200, "application/json", `{"token":"cop_fresh","expires_at":` + itoa64(future()) + `}`
		}
		return 200, "text/event-stream", sseOK
	}}
	// Copilot token 已临近过期 → 应先续期再对话
	h, fm, _ := newCopilotHandler(t, up, copilot.Credential{
		GitHubToken: "gho_1", CopilotToken: "cop_stale",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if up.count("token") != 1 {
		t.Fatalf("应续期 1 次，实际 %d", up.count("token"))
	}
	// 续期后必须回写凭据，否则下一个请求又要续一次
	got := fm.get(extstore.PCopilot, "acct0")
	if got == nil || got.CopilotToken != "cop_fresh" {
		t.Fatalf("凭据未回写: %+v", got)
	}
	if got.GitHubToken != "gho_1" {
		t.Fatal("续期不应丢失 GitHub token")
	}
	// 对话用的必须是新 token
	up.mu.Lock()
	defer up.mu.Unlock()
	for _, c := range up.calls {
		if c.kind == "chat" && c.auth != "Bearer cop_fresh" {
			t.Fatalf("对话鉴权 = %q, 期望 Bearer cop_fresh", c.auth)
		}
	}
}

func TestCopilotForcedRefreshOn401(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, idx int) (int, string, string) {
		if kind == "token" {
			return 200, "application/json", `{"token":"cop_retry","expires_at":` + itoa64(future()) + `}`
		}
		if idx == 0 {
			// 第一次对话：token 被上游提前失效
			return 401, "application/json", `{"error":{"message":"token expired"}}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, fm, _ := newCopilotHandler(t, up, copilot.Credential{
		GitHubToken: "gho_1", CopilotToken: "cop_valid", ExpiresAt: future(),
	})

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("401 后应续期重试成功，状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if up.count("chat") != 2 {
		t.Fatalf("应重试 1 次（共 2 次 chat），实际 %d", up.count("chat"))
	}
	if got := fm.get(extstore.PCopilot, "acct0"); got == nil || got.CopilotToken != "cop_retry" {
		t.Fatalf("401 续期未回写: %+v", got)
	}
}

func TestCopilotFailoverToSecondAccount(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, idx int) (int, string, string) {
		if kind == "token" {
			return 200, "application/json", `{"token":"cop_r","expires_at":` + itoa64(future()) + `}`
		}
		if idx == 0 {
			return 429, "application/json", `{"error":{"message":"rate limited"}}`
		}
		return 200, "text/event-stream", sseOK
	}}
	h, _, _ := newCopilotHandler(t, up,
		copilot.Credential{GitHubToken: "g1", CopilotToken: "c1", ExpiresAt: future()},
		copilot.Credential{GitHubToken: "g2", CopilotToken: "c2", ExpiresAt: future()},
	)

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("换号后应成功，状态码 = %d，体: %s", w.Code, w.Body.String())
	}
	if up.count("chat") != 2 {
		t.Fatalf("应尝试 2 个账号，实际 chat 次数 %d", up.count("chat"))
	}
}

func TestCopilotUpstreamErrorSurfaced(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "token" {
			return 200, "application/json", `{"token":"c","expires_at":` + itoa64(future()) + `}`
		}
		return 500, "application/json", `{"error":{"message":"boom"}}`
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "g", CopilotToken: "c", ExpiresAt: future()})

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}
	// 失败原因要透给调用方，否则用户只看到"无可用账号"无从排查
	if !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("未带出上游原因: %s", w.Body.String())
	}
}

func TestCopilotJSONUpstreamWrappedForStreamClient(t *testing.T) {
	// 客户端要流式、上游回了 JSON（非流式）→ 包成单帧 SSE 再补 [DONE]
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "token" {
			return 200, "application/json", `{"token":"c","expires_at":` + itoa64(future()) + `}`
		}
		return 200, "application/json", `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "g", CopilotToken: "c", ExpiresAt: future()})

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","stream":true,"messages":[]}`)
	body := w.Body.String()
	if !strings.HasPrefix(body, "data: ") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("未包装成 SSE: %s", body)
	}
	if !strings.Contains(body, "chatcmpl-1") {
		t.Fatalf("原始体丢失: %s", body)
	}
}

func TestCopilotJSONUpstreamPassedToNonStreamClient(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		if kind == "token" {
			return 200, "application/json", `{"token":"c","expires_at":` + itoa64(future()) + `}`
		}
		return 200, "application/json", `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "g", CopilotToken: "c", ExpiresAt: future()})

	w := postCopilot(t, h, "copilot:gpt-4o", `{"model":"copilot:gpt-4o","messages":[]}`)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %s", ct)
	}
	if strings.Contains(w.Body.String(), "data: ") {
		t.Fatalf("非流式客户端不应收到 SSE 包装: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chatcmpl-1") {
		t.Fatalf("体未透传: %s", w.Body.String())
	}
}

func TestCopilotModelListedInModelsEndpoint(t *testing.T) {
	up := &copilotUpstream{respond: func(kind string, _ int) (int, string, string) {
		switch kind {
		case "token":
			return 200, "application/json", `{"token":"c","expires_at":` + itoa64(future()) + `}`
		case "models":
			return 200, "application/json", `{"data":[{"id":"gpt-4o","name":"GPT-4o","vendor":"OpenAI"}]}`
		}
		return 200, "application/json", "{}"
	}}
	h, _, _ := newCopilotHandler(t, up, copilot.Credential{GitHubToken: "g", CopilotToken: "c", ExpiresAt: future()})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("状态码 = %d", w.Code)
	}
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析: %v", err)
	}
	found := false
	for _, m := range doc.Data {
		if m.ID == "copilot:gpt-4o" {
			found = true
			if m.OwnedBy != "copilot/OpenAI" {
				t.Fatalf("owned_by = %s", m.OwnedBy)
			}
		}
	}
	if !found {
		t.Fatal("模型列表未含 copilot:gpt-4o")
	}
}

func TestPlatformOfCopilot(t *testing.T) {
	if got := PlatformOf("copilot:gpt-4o"); got != "copilot" {
		t.Fatalf("PlatformOf = %s, 期望 copilot", got)
	}
}

func TestShorten(t *testing.T) {
	if got := shorten("abc", 10); got != "abc" {
		t.Errorf("短串不该截断: %s", got)
	}
	if got := shorten("abcdefghij", 4); got != "abcd…" {
		t.Errorf("截断错误: %s", got)
	}
	// 多字节字符不能被切成半个（否则日志里是乱码）
	got := shorten("中文中文中文", 7)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断错误: %q", got)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("切断了多字节字符: %q", got)
		}
	}
}

/* ── 小工具 ──────────────────────────────────────────────────── */

func future() int64 { return time.Now().Add(25 * time.Minute).Unix() }

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
