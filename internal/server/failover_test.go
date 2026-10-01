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

	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/livecfg"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// seedFamily 预置族索引，**绕过 platformModelIDs**。
//
// 真建索引会去拉 10 个平台的目录（冷缓存单个 15–20s，有的还打外网）——
// 那是「索引怎么建」的问题，已由 TestNormalizeModelFamilies /
// TestBuildFamilyIndexGroupsByNormalizedKey 覆盖。这里只测「有了索引之后
// 会不会换平台」，所以直接把结果摆好，Cleanup 里还原。
func seedFamily(t *testing.T, key string, members ...string) {
	t.Helper()
	familyMu.Lock()
	prevIdx, prevAt := familyIdx, familyIdxAt
	familyIdx, familyIdxAt = map[string][]string{key: members}, time.Now()
	familyMu.Unlock()
	t.Cleanup(func() {
		familyMu.Lock()
		familyIdx, familyIdxAt = prevIdx, prevAt
		familyMu.Unlock()
	})
}

// mockUp 单一 mock 上游：按 kind 分派响应并计数。
type mockUp struct {
	mu      sync.Mutex
	calls   map[string]int
	respond func(kind string) (int, string, string)
}

func (m *mockUp) count(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[kind]
}

func (m *mockUp) client(classify func(*http.Request) string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		kind := classify(r)
		m.mu.Lock()
		if m.calls == nil {
			m.calls = map[string]int{}
		}
		m.calls[kind]++
		m.mu.Unlock()
		st, ct, body := m.respond(kind)
		return &http.Response{
			StatusCode: st,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func sse(text string) (int, string, string) {
	return 200, "text/event-stream", text
}

// failoverHandler 同时挂 raccoon + cline 两个平台账号的 handler。
// keys 非空时挂多 Key 体系（测平台授权用）。
func failoverHandler(t *testing.T, raccoonResp, clineResp func(string) (int, string, string),
	keys []livecfg.APIKeyEntry) (*Handler, *mockUp, *mockUp) {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	add := func(p, id string, cred any) {
		raw, err := json.Marshal(cred)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := mgr.Add(p, id, id, raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	add(extstore.PRaccoon, "r1", raccoon.Credential{AccessToken: "rt", RefreshToken: "rr", UserID: "r1"})
	add(extstore.PCline, "c1", cline.Credential{AccessToken: "workos:t", RefreshToken: "cr", Email: "c1", ExpiresAt: clineFuture()})

	ru := &mockUp{respond: raccoonResp}
	cu := &mockUp{respond: clineResp}
	raccoon.SetHTTPClient(ru.client(func(*http.Request) string { return "chat" }))
	t.Cleanup(func() { raccoon.SetHTTPClient(&http.Client{}) })
	cline.SetHTTPClient(cu.client(func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			return "chat"
		}
		return "other"
	}))
	t.Cleanup(func() { cline.SetHTTPClient(&http.Client{}) })

	cfg := Config{ExtAccounts: mgr.ExtList, Pool: pool.New(""), Upstream: upstream.New()}
	if len(keys) > 0 {
		// **必须同时给主 APIKey**：`withAuth` 先试主 key，命中就用**原始请求**
		// 往下传（不带平台授权的 context）。主 key 为空时
		// `VerifyBearer(r, "")` 会直接放行，多 Key 分支根本不执行——
		// 平台授权就整段被绕过（这个坑第一版测试就踩了）。
		cfg.APIKey = "main-key-for-tests"
		cfg.Live = livecfg.New(livecfg.Snapshot{
			APIKey:  "main-key-for-tests",
			APIKeys: keys,
		})
	}
	h := NewHandler(cfg)
	h.ExtSetManager(&fakeExtMgr{})
	return h, ru, cu
}

// postModel 发一条聊天请求。
func postModel(t *testing.T, h *Handler, model, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[]}`))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// 额度耗尽（402）→ 换到同族的另一个平台，且响应头留痕。
//
// 回包 body 的 `model` 是**服务方的模型名**（与今天的前缀剥离行为同源，
// 各桥接本来就 echo 自己发出去的名字），所以可追溯性落在响应头与日志上。
func TestFailoverOnExhaustedToSameFamily(t *testing.T) {
	seedFamily(t, "glm-5.3", "raccoon:sn-glm-5-3", "cline:cline-pass/glm-5.3")

	h, _, cu := failoverHandler(t,
		func(string) (int, string, string) { return 402, "application/json", `{"error":"insufficient credits"}` },
		func(kind string) (int, string, string) {
			if kind == "chat" {
				return sse("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"cline-pass/glm-5.3\"," +
					"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
			}
			return 200, "application/json", `{"id":"o","object":"model"}`
		}, nil)

	w := postModel(t, h, "raccoon:sn-glm-5-3", "")
	if w.Code != http.StatusOK {
		t.Fatalf("额度耗尽应降级到同族平台，状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Served-By"); got != "cline" {
		t.Errorf("X-Served-By = %q，期望 cline", got)
	}
	if got := w.Header().Get("X-Failover-From"); got != "raccoon" {
		t.Errorf("X-Failover-From = %q，期望 raccoon", got)
	}
	if cu.count("chat") == 0 {
		t.Error("没打到候选平台 —— 降级没发生")
	}
	if !strings.Contains(w.Body.String(), "hi") {
		t.Errorf("候选平台的响应没透传: %s", w.Body.String())
	}
}

// **非额度错误不降级**：上游 500 换过去大概率还是 500，而且会拿到语义
// 不同的模型——那种"看起来成功了"的降级比直接报错更难排查。
func TestNoFailoverOnServerError(t *testing.T) {
	seedFamily(t, "glm-5.3", "raccoon:sn-glm-5-3", "cline:cline-pass/glm-5.3")

	h, _, cu := failoverHandler(t,
		func(string) (int, string, string) { return 500, "application/json", `{"error":"internal error"}` },
		func(kind string) (int, string, string) {
			if kind == "chat" {
				return sse("data: [DONE]\n\n")
			}
			return 200, "application/json", `{}`
		}, nil)

	w := postModel(t, h, "raccoon:sn-glm-5-3", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("500 不该降级，状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no_raccoon_account") {
		t.Errorf("应写**原平台**的错误码: %s", w.Body.String())
	}
	if cu.count("chat") != 0 {
		t.Error("非额度错误不该打到候选平台")
	}
}

// **越权防护**：准入门只在请求开头对原始模型名查过一次，降级时必须逐候选重查。
// 只授权 raccoon 的 key 在 raccoon 耗尽后，**不能**被拿去打 cline。
func TestFailoverRespectsPlatformAuthorization(t *testing.T) {
	seedFamily(t, "glm-5.3", "raccoon:sn-glm-5-3", "cline:cline-pass/glm-5.3")

	h, _, cu := failoverHandler(t,
		func(string) (int, string, string) { return 402, "application/json", `{"error":"insufficient credits"}` },
		func(kind string) (int, string, string) {
			if kind == "chat" {
				return sse("data: [DONE]\n\n")
			}
			return 200, "application/json", `{}`
		},
		[]livecfg.APIKeyEntry{{Key: "k-restricted", Name: "only-raccoon", Platforms: []string{"raccoon"}}})

	w := postModel(t, h, "raccoon:sn-glm-5-3", "k-restricted")
	if cu.count("chat") != 0 {
		t.Fatalf("越权：只授权 raccoon 的 key 打到了 cline")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("应写原平台 503（而不是 403 或成功），状态码 = %d", w.Code)
	}
	if w.Header().Get("X-Served-By") != "" {
		t.Errorf("没成功就该把 X-Served-By 摘掉，实际 = %q", w.Header().Get("X-Served-By"))
	}
}

// 没有同族候选（孤立族）→ 不降级，写原 503。
// 这条保证**降级永远是可选增强**：没有候选时行为与改动前逐字一致。
func TestNoFailoverWithoutFamilyCandidates(t *testing.T) {
	seedFamily(t, "raccoon-prop", "raccoon:raccoon-8c4485") // 只有它自己

	h, _, _ := failoverHandler(t,
		func(string) (int, string, string) { return 402, "application/json", `{"error":"insufficient credits"}` },
		func(string) (int, string, string) {
			t.Error("不该有任何候选平台被调用")
			return 500, "application/json", `{}`
		}, nil)

	w := postModel(t, h, "raccoon:raccoon-8c4485", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("无候选时应写原 503，状态码 = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_raccoon_account") {
		t.Errorf("错误码应仍是原平台的: %s", w.Body.String())
	}
}
