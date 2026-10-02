package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/ima"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// 没账号必须 503 + 明确错误码，且一次上游都不能打。
func TestIMANoAccounts(t *testing.T) {
	mgr := extstore.NewManager(t.TempDir())
	ima.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("没有账号时不该打上游")
		return nil, nil
	})})
	t.Cleanup(func() { ima.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ima:hy3-preview","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_ima_account") {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
}

// 有账号时走通：init_session → qa → SSE 翻译成 OpenAI chunk。
// 上游是**会话式协议**，mock 按路径分派。
func TestIMAHappyPath(t *testing.T) {
	mgr := extstore.NewManager(t.TempDir())
	raw, _ := json.Marshal(ima.Credential{Cookie: "IMA-TOKEN=tok; IMA-UID=u1", UserID: "u1"})
	if err := mgr.Add(extstore.PIMA, "acct1", "acct1", raw); err != nil {
		t.Fatalf("add: %v", err)
	}

	initCalls := 0
	qaCalls := 0
	ima.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "init_session"):
			initCalls++
			return jsonResponse(200, `{"code":0,"msg":"ok","data":{"session_id":"sess-1"}}`), nil
		case strings.Contains(r.URL.Path, "assistant/qa"):
			qaCalls++
			// ima 的 SSE：event 全大写 + data JSON（文本字段名漂移）
			sse := "event: TEXT_DELTA\n" +
				`data: {"Text":"你"}` + "\n\n" +
				"event: TEXT_DELTA\n" +
				`data: {"Text":"好"}` + "\n\n" +
				"event: COMPLETED\n" + "data: {}\n\n"
			return sseResponse(200, sse), nil
		}
		t.Errorf(" unexpected 上游路径: %s", r.URL.Path)
		return jsonResponse(404, `{"code":404,"msg":"nf"}`), nil
	})})
	t.Cleanup(func() { ima.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ima:hy3-preview","stream":true,"messages":[{"role":"user","content":"1+1=?"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	if initCalls != 1 || qaCalls != 1 {
		t.Fatalf("init=%d qa=%d，期望各 1 次", initCalls, qaCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, `"content":"好"`) {
		t.Errorf("增量文本没透传: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("缺 [DONE] 收尾")
	}
	// 会话缓存应已写入（下一轮同 conversation 不再 init）
}

// 会话满（INNER_EXCEPTION）→ 自动重建会话重试一次（参考实现同款行为）。
func TestIMARebuildsSessionOnInnerException(t *testing.T) {
	mgr := extstore.NewManager(t.TempDir())
	raw, _ := json.Marshal(ima.Credential{Cookie: "IMA-TOKEN=tok; IMA-UID=u1", UserID: "u1"})
	if err := mgr.Add(extstore.PIMA, "acct1", "acct1", raw); err != nil {
		t.Fatalf("add: %v", err)
	}

	initCalls := 0
	qaErrs := 0
	ima.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "init_session") {
			initCalls++
			return jsonResponse(200, `{"code":0,"msg":"ok","data":{"session_id":"sess-`+itoa(initCalls)+`"}}`), nil
		}
		// qa 请求无法从 URL 区分会话——按「已回过几次错误」计数：
		// 第一次 qa 回 INNER_EXCEPTION（触发重建会话），之后正常
		qaErrs++
		t.Logf("mock: qa 第 %d 次被调用", qaErrs)
		if qaErrs == 1 {
			return sseResponse(200, "event: INNER_EXCEPTION\ndata: {\"Msg\":\"session full\"}\n\n"), nil
		}
		return sseResponse(200, "event: TEXT_DELTA\ndata: {\"Text\":\"ok\"}\n\nevent: COMPLETED\ndata: {}\n\n"), nil
	})})
	t.Cleanup(func() { ima.SetHTTPClient(&http.Client{}) })

	h := NewHandler(Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()})
	h.ExtSetManager(&fakeExtMgr{})

	// 预置一个会话缓存（模拟上一轮留下的满会话）
	ima.PutSession("conv-test", "acct1", "stale-sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ima:hy3-preview","stream":false,"conversation_id":"conv-test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	// 预置了 stale 会话缓存 → AskStream 直接用缓存（init 0 次）→ qa 报会话满
	// → collectIMAWithRetry 重建（init 1 次）→ qa 成功。qa 共 2 次、init 共 1 次。
	if initCalls != 1 {
		t.Fatalf("init_session 应调用 1 次（缓存命中后重建），实际 %d", initCalls)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("重建后的回答没透传: %s", w.Body.String())
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
