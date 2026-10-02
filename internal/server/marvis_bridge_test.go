package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/marvis"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// 没账号必须 503 + 明确错误码，且一次上游都不能打。
func TestMarvisNoAccounts(t *testing.T) {
	mgr := extstore.NewManager(t.TempDir())
	marvis.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("没有账号时不该打上游")
		return nil, nil
	})})
	t.Cleanup(func() { marvis.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"marvis:main-auto","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_marvis_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
}

// happy path：上游是标准 OpenAI SSE 直通，响应帧的 model 要回写成请求名。
// 同时验证 Ual-Access-* 头组齐全（uskey 每请求唯一）。
func TestMarvisHappyPathPassthrough(t *testing.T) {
	mgr := extstore.NewManager(t.TempDir())
	raw, _ := json.Marshal(marvis.Credential{
		AccessToken: "mv_test-token", OpenID: "oid-1",
		LoginType: "6", DeviceGuid: "guid-1", Nickname: "测试",
	})
	if err := mgr.Add(extstore.PMarvis, "acct1", "acct1", raw); err != nil {
		t.Fatalf("add: %v", err)
	}

	var mu sync.Mutex
	var seenAuth, seenExt []string
	marvis.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seenAuth = append(seenAuth, r.Header.Get("Ual-Access-Access-Token"))
		seenExt = append(seenExt, r.Header.Get("Ual-Access-MarvisExt"))
		mu.Unlock()
		frame := `data: {"id":"c1","object":"chat.completion.chunk","model":"upstream-internal",` +
			`"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(frame)),
		}, nil
	})})
	t.Cleanup(func() { marvis.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"marvis:main-auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("增量没透传: %s", body)
	}
	if strings.Contains(body, "upstream-internal") {
		t.Errorf("上游内部模型名泄漏: %s", body)
	}
	if !strings.Contains(body, `"model":"main-auto"`) {
		t.Errorf("model 未回写: %s", body)
	}

	// 鉴权头与 uskey
	mu.Lock()
	defer mu.Unlock()
	if len(seenAuth) != 1 || seenAuth[0] != "mv_test-token" {
		t.Errorf("Ual-Access-Access-Token 不对: %v", seenAuth)
	}
	if len(seenExt) != 1 {
		t.Fatalf("Ual-Access-MarvisExt 未发送")
	}
	var ext map[string]any
	if err := json.Unmarshal([]byte(seenExt[0]), &ext); err != nil {
		t.Fatalf("MarvisExt 不是 JSON: %v", err)
	}
	uskey, _ := ext["uskey"].(string)
	if !strings.HasPrefix(uskey, "CiDVDCER ") {
		t.Errorf("uskey 形状不对（应 CiDVDCER 前缀）: %q", uskey[:min(20, len(uskey))])
	}
	if len(uskey) != 992 {
		t.Errorf("uskey 长度 = %d，期望 992（与真实抓包同构）", len(uskey))
	}
	if ext["qimei36"] != "guid-1" || ext["guid"] != "guid-1" {
		t.Errorf("qimei36/guid 应取 DeviceGuid: %v", ext)
	}
}

// token 失效（401 4100403）→ 错误提示要指明「重新抓包」。
func TestMarvisTokenErrorHint(t *testing.T) {
	mgr := extstore.NewManager(t.TempDir())
	raw, _ := json.Marshal(marvis.Credential{AccessToken: "mv_dead", OpenID: "oid", DeviceGuid: "g"})
	if err := mgr.Add(extstore.PMarvis, "acct1", "acct1", raw); err != nil {
		t.Fatalf("add: %v", err)
	}
	marvis.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"error":{"code":4100403,"message":"token invalid"}}`), nil
	})})
	t.Cleanup(func() { marvis.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"marvis:main-auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "重新抓包") {
		t.Errorf("错误提示应指明「重新抓包」: %s", w.Body.String())
	}
}
