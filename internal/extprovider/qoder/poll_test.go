package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

func withMock(t *testing.T, h http.HandlerFunc) *[]*http.Request {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var seen []*http.Request
	old := httpClient
	SetHTTPClient(&http.Client{Transport: rec{target: srv, seen: &seen}})
	t.Cleanup(func() { httpClient = old })
	return &seen
}

type rec struct {
	target *httptest.Server
	seen   *[]*http.Request
}

func (r rec) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(r.target.URL, "http://")
	*r.seen = append(*r.seen, req)
	return http.DefaultTransport.RoundTrip(req)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(v.(string)))
}

func newTestSession() *DeviceSession {
	return &DeviceSession{Nonce: "n-1", Verifier: "v-1", MachineID: "m-1"}
}

/* ── 轮询回执字段 ────────────────────────────────────────────── */

// 实测踩过的坑：deviceToken/poll 回的是 **`token`**，不是 `access_token`。
// 只读 access_token 会永远取不到 → 轮询永远 pending，用户看到「授权成功了
// 却一直显示等待授权」。参考实现（QoderGateway tokens.py）读的也是 `token`。
func TestPollReadsTokenField(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"id":"01a0f615","token":"dt-sUFMV8Cpz","user_id":"01a0f219",
			"user_name":"测试","refresh_token":"drt-1"}`)
	})
	cred, status, err := New().Poll(context.Background(), newTestSession())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if cred == nil {
		t.Fatalf("应取到凭据，实际 status=%s", status)
	}
	if cred.AccessToken != "dt-sUFMV8Cpz" {
		t.Errorf("AccessToken = %q，期望来自 token 字段", cred.AccessToken)
	}
	// 双写：服务端取用顺序是 security_oauth_token ?? access_token
	if cred.SecurityOAuthToken != cred.AccessToken {
		t.Errorf("两条链路要双写同值: %q vs %q", cred.SecurityOAuthToken, cred.AccessToken)
	}
	if cred.RefreshToken != "drt-1" {
		t.Errorf("refresh_token 未解析: %q", cred.RefreshToken)
	}
	if cred.UID != "01a0f219" {
		t.Errorf("uid 未解析: %q", cred.UID)
	}
	if status != "ok" {
		t.Errorf("status = %q，成功应为 ok", status)
	}
}

// access_token 仍要能认（上游不同链路字段名不一致）。
func TestPollFallsBackToAccessToken(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"access_token":"at-1"}`)
	})
	cred, _, err := New().Poll(context.Background(), newTestSession())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if cred == nil || cred.AccessToken != "at-1" {
		t.Fatalf("access_token 未回退: %+v", cred)
	}
}

// 404 = 尚未授权（不是错误），要回 status 让日志能看出轮询确实在跑。
func TestPollNotYetAuthorized(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	cred, status, err := New().Poll(context.Background(), newTestSession())
	if err != nil {
		t.Fatalf("404 不该是错误: %v", err)
	}
	if cred != nil {
		t.Error("404 时不该返回凭据")
	}
	if status == "" {
		t.Error("status 要能被日志打印（区分「没在轮询」和「上游说还没授权」）")
	}
}

// 200 但没有任何 token 字段 —— 回执里应能看出上游给了什么。
func TestPollEmptyTokenSurfacesRaw(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"id":"01a0","kind":"polling"}`)
	})
	cred, status, err := New().Poll(context.Background(), newTestSession())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if cred != nil {
		t.Fatal("没有 token 时不该返回凭据")
	}
	if !strings.Contains(status, "kind") {
		t.Errorf("status 应带上回执原文以便定位字段名，得到 %q", status)
	}
}

// 实测踩过的坑：上游 `expires_at` 是**字符串**（参考实现直接当字符串存进库），
// 而我上一版声明成了 int64 —— 一个字段类型不符就让整份回执解析失败，
// 授权明明已经成功，日志里却报成
// 「cannot unmarshal string into Go struct field .expires_at of type int64」，
// 连续 5 次后被判终态，用户看到的还是「等待授权」。
// 回执解析必须是宽容的：一个字段的形态不该拖垮整包。
func TestPollToleratesStringExpiry(t *testing.T) {
	for name, body := range map[string]string{
		"RFC3339 字符串": `{"token":"dt-1","expires_at":"2026-10-02T00:00:00Z"}`,
		"纯数字字符串":      `{"token":"dt-1","expires_at":"1790835411"}`,
		"数字毫秒":        `{"token":"dt-1","expires_at":1790835411267}`,
		"数字秒":         `{"token":"dt-1","expires_at":1790835411}`,
		"对象里的无关字段":    `{"token":"dt-1","meta":{"a":1,"b":[2,3]}}`,
	} {
		withMock(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, body)
		})
		cred, status, err := New().Poll(context.Background(), newTestSession())
		if err != nil {
			t.Errorf("%s: 不该解析失败 —— %v", name, err)
			continue
		}
		if cred == nil {
			t.Errorf("%s: 应取到凭据，status=%s", name, status)
			continue
		}
		if cred.AccessToken != "dt-1" {
			t.Errorf("%s: token 丢失: %q", name, cred.AccessToken)
		}
		// 秒级必须换算成毫秒，否则「还没到期」会被判成「早已过期」
		if cred.ExpireTime != 0 && cred.ExpireTime < 1e12 {
			t.Errorf("%s: 过期时间没换算成毫秒: %d", name, cred.ExpireTime)
		}
	}
}

// 不能因为一个不认识的字段类型就整包失败。
func TestPollIgnoresUnknownFieldTypes(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"token":"dt-1","features":["a"],"nested":{"deep":true},"count":3}`)
	})
	cred, _, err := New().Poll(context.Background(), newTestSession())
	if err != nil {
		t.Fatalf("不该因未知字段类型失败: %v", err)
	}
	if cred == nil || cred.AccessToken != "dt-1" {
		t.Fatalf("凭据没取到: %+v", cred)
	}
}
