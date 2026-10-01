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
	signedHdr string // 出站 Authorization 里的 SignedHeaders
	body      string
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
		u.mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
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
