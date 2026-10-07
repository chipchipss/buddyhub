package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/codearts"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

type codeArtsUpstream struct {
	mu        sync.Mutex
	calls     []string
	chatCalls int
	signedHdr string // 出站 Authorization 里的 SignedHeaders
	body      string
	// chatStatus/chatBody 非零时，对话端点回这个状态码与正文（模拟上游判死）。
	chatStatus int
	chatBody   string
}

func (u *codeArtsUpstream) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		u.mu.Lock()
		u.calls = append(u.calls, r.Method+"|"+r.URL.Path)
		if auth := r.Header.Get("Authorization"); strings.Contains(auth, "SignedHeaders=") {
			u.signedHdr = strings.Split(strings.Split(auth, "SignedHeaders=")[1], ",")[0]
		}
		u.body = string(raw)
		isChat := strings.HasSuffix(r.URL.Path, "/chat/completions")
		if isChat {
			u.chatCalls++
		}
		status, body := u.chatStatus, u.chatBody
		u.mu.Unlock()

		if isChat {
			if status != 0 {
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			// 上游对话是 SSE，帧形为标准 OpenAI chunk
			frame := `data: {"id":"c1","object":"chat.completion.chunk","model":"upstream-internal",` +
				`"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(frame)),
			}, nil
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"models":[]}`)),
		}, nil
	})}
}

func (u *codeArtsUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *codeArtsUpstream) chats() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chatCalls
}

func newCodeArtsHandler(t *testing.T, up *codeArtsUpstream, creds ...codearts.Credential) *Handler {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		id := "ca" + string(rune('0'+i))
		if err := mgr.Add(extstore.PCodeArts, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	codearts.SetHTTPClient(up.client())
	t.Cleanup(func() { codearts.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	h.ExtSetManager(&fakeExtMgr{})
	return h
}

// 没账号必须 503 + 明确错误码，且一次上游都不能打。
func TestCodeArtsNoAccounts(t *testing.T) {
	up := &codeArtsUpstream{}
	h := newCodeArtsHandler(t, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts:glm-5.2","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_codearts_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	if up.count() != 0 {
		t.Fatalf("不该打上游，打了 %d 次", up.count())
	}
}

// 有账号时走通，并且出站请求带的是**剥掉前缀后的**上游模型名 +
// 三个模型头（少一个上游判成"没这个模型"）。
func TestCodeArtsChatSendsUpstreamModelAndHeaders(t *testing.T) {
	up := &codeArtsUpstream{}
	h := newCodeArtsHandler(t, up, codearts.Credential{
		AccessKeyID: "AK", SecretAccessKey: "SK",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts:glm-5.2","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	if up.count() == 0 {
		t.Fatal("没有发出对话请求")
	}
	// 出站体：前缀已剥、模型名进了 body
	if !strings.Contains(up.body, `"model":"glm-5.2"`) {
		t.Errorf("出站 model 应是剥掉前缀后的上游名，实际: %s", up.body)
	}
	if strings.Contains(up.body, "codearts:glm-5.2") {
		t.Errorf("带前缀的模型名不该发给上游: %s", up.body)
	}
	// 签名头集合：**host 绝不能在里面**（对话链路 include_host=false），
	// 少了 model 相关头则上游会判成"没这个模型"。
	for _, want := range []string{"content-type", "model-id", "model-name", "x-model-id"} {
		if !hasHeader(up.signedHdr, want) {
			t.Errorf("签名集合缺 %s: %s", want, up.signedHdr)
		}
	}
	if hasHeader(up.signedHdr, "host") {
		t.Errorf("对话链路不该签 host: %s", up.signedHdr)
	}
	// 响应里的内部模型名要回写成我们发出去的那个
	if !strings.Contains(w.Body.String(), `"model":"glm-5.2"`) {
		t.Errorf("响应 model 没回写: %s", w.Body.String())
	}
}

// PlatformOf 必须认得 codearts 前缀——漏了会让带平台授权的 API key
// 对所有 codearts:* 模型返回 403。
func TestPlatformOfCodeArts(t *testing.T) {
	if got := PlatformOf("codearts:glm-5.2"); got != "codearts" {
		t.Fatalf("PlatformOf = %q, 期望 codearts", got)
	}
	if !isCodeArtsModel("codearts:x") {
		t.Fatal("isCodeArtsModel 认不出自己的前缀")
	}
	if isCodeArtsModel("bare-model") {
		t.Fatal("isCodeArtsModel 劫持了裸模型名")
	}
}

// 缺 AK/SK 的账号要被跳过而不是打上游（打出去也是稳定 401，白耗一次）。
func TestCodeArtsSkipsCredentialWithoutKeys(t *testing.T) {
	up := &codeArtsUpstream{}
	h := newCodeArtsHandler(t, up, codearts.Credential{AccessKeyID: "", SecretAccessKey: ""})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts:glm-5.2","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if up.count() != 0 {
		t.Fatalf("缺 AK/SK 不该打上游，打了 %d 次", up.count())
	}
}

// hasHeader 判断 `;` 连接的 SignedHeaders 集合里有没有某个头（全小写比较）。
func hasHeader(joined, name string) bool {
	for _, p := range strings.Split(joined, ";") {
		if strings.EqualFold(strings.TrimSpace(p), name) {
			return true
		}
	}
	return false
}

// 模型名在上游不存在是**路由表级**判定，与账号无关。旧实现把它当成账号故障：
// 换下一个账号再打一发，然后 lastErr 被那个账号的账号级错误（403 未分配席位）
// 覆盖——用户看到「Contact your organization administrator to assign a seat」，
// 真正原因却是「这个模型没注册」，于是把可用账号当成坏账号反复重登。
// 实测（2026-10-08 个人版账号）就是这么误诊的。
func TestCodeArtsModelNotRegisteredStopsRetryAndNamesModel(t *testing.T) {
	up := &codeArtsUpstream{
		chatStatus: http.StatusNotFound,
		chatBody: `{"error_code":"InferHub.002002009.404","error_msg":"The model is not registered, ` +
			`please request other model","details":[{"error_msg":"Route missed for model: glm-5.3-flash"}]}`,
	}
	h := newCodeArtsHandler(t, up,
		codearts.Credential{AccessKeyID: "AK1", SecretAccessKey: "SK1"},
		codearts.Credential{AccessKeyID: "AK2", SecretAccessKey: "SK2"},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts:glm-5.3-flash","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("未注册模型应回 404，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_not_found") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
	// 说清楚是**这个模型**没注册，并给出实测可用名单——不是"没有可用账号"
	if !strings.Contains(w.Body.String(), "glm-5.3-flash") ||
		!strings.Contains(w.Body.String(), "deepseek-v4-flash") {
		t.Fatalf("原因里要带模型名与可用名单: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "no_codearts_account") {
		t.Fatalf("不该说成没有账号: %s", w.Body.String())
	}
	// 换号无用 → 只打一发上游
	if up.chats() != 1 {
		t.Fatalf("未注册不该换号重试，打了 %d 发", up.chats())
	}
	// 账号健康不受影响（否则一次误点就让健康号进冷却退避）
	if n := len(h.extManager.(*fakeExtMgr).notes); n != 0 {
		t.Fatalf("模型名错误不该记在账号上，记了 %d 条", n)
	}
}

// 账号级故障（403 未分配席位）仍然要：换下一个账号 + 给这个号记失败 +
// 把账号级原因说清楚。与上一条测试成对，防止把「判模型」写成「判账号」。
func TestCodeArtsAccountLevelErrorStillRotates(t *testing.T) {
	up := &codeArtsUpstream{
		chatStatus: http.StatusForbidden,
		chatBody:   `{"error_code":"TM.00001005","error_msg":"Access denied. Contact your organization administrator to assign a seat to you."}`,
	}
	h := newCodeArtsHandler(t, up,
		codearts.Credential{AccessKeyID: "AK1", SecretAccessKey: "SK1"},
		codearts.Credential{AccessKeyID: "AK2", SecretAccessKey: "SK2"},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts:GLM-5.2","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("全账号故障应回 503，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "TM.00001005") {
		t.Fatalf("账号级原因没透出: %s", w.Body.String())
	}
	if up.chats() != 2 {
		t.Fatalf("账号级故障该换号再试，打了 %d 发", up.chats())
	}
	mgr := h.extManager.(*fakeExtMgr)
	if len(mgr.notes) != 2 {
		t.Fatalf("两个号都该记一次失败，得到 %d 条", len(mgr.notes))
	}
}

// 失败原因字段是**上一次请求**的事实，跨请求残留会把好的那一次讲成坏的。
func TestCodeArtsStaleReasonClearedPerRequest(t *testing.T) {
	up := &codeArtsUpstream{chatStatus: http.StatusInternalServerError, chatBody: `{"e":"boom"}`}
	mgr := extstore.NewManager(t.TempDir())
	codearts.SetHTTPClient(up.client())
	t.Cleanup(func() { codearts.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	// 没有任何账号：连上游都不该打，也不该带出历史失败原因
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts:GLM-5.2","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || up.count() != 0 {
		t.Fatalf("无账号应 503 且零上游，得到 code=%d calls=%d", w.Code, up.count())
	}
	if strings.Contains(w.Body.String(), "最近失败原因") {
		t.Fatalf("无账号时不该带历史原因: %s", w.Body.String())
	}
}
