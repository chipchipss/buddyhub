// keyusage_test.go 清单 38 端到端：走网关的聊天请求要按「调用方 Key」记调用量。
// recordAttempt 在池轮转路径上，所以复用 handler_test.go 的 fake 上游 + 测试池，
// 而不是 failoverHandler（那是桥接路径，不经过 recordAttempt）。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/auth"
	"github.com/chipchipss/buddyhub/internal/livecfg"
	"github.com/chipchipss/buddyhub/internal/usage"
)

func TestRecordAttemptPerKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	sub := livecfg.APIKeyEntry{Key: "bh-sub-key", Name: "sub"}
	rec := usage.New("")
	// 主 APIKey 必须非空：withAuth 先试主 Key，为空时 VerifyBearer 直接放行、
	// 多 Key 分支根本不执行（failover_test.go 里记过的同一个坑）。
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Live:     livecfg.New(livecfg.Snapshot{APIKey: "main-key-for-tests", APIKeys: []livecfg.APIKeyEntry{sub}}),
		Usage:    rec,
	})

	chat := func(bearer string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if c := chat("bh-sub-key"); c != http.StatusOK {
		t.Fatalf("sub key 请求 code=%d", c)
	}
	if c := chat("main-key-for-tests"); c != http.StatusOK {
		t.Fatalf("主 key 请求 code=%d", c)
	}

	byID := map[string]usage.KeyStat{}
	for _, k := range rec.Snapshot(0, nil).ByKey {
		byID[k.ID] = k
	}
	// 子 Key：context 里有命中条目，直接记在它的 id 上。
	if s := byID[sub.ID()]; s.Calls != 1 || s.Errors != 0 {
		t.Fatalf("sub key 统计 = %+v, want calls 1 / errors 0", s)
	}
	// 主 Key：withAuth 透传原始请求（context 零值），必须回退记在主 Key 的 id 上——
	// 主 Key 同样有「删了会不会打挂客户端」的问题，漏记等于诱导误删。
	if s := byID[livecfg.APIKeyEntry{Key: "main-key-for-tests"}.ID()]; s.Calls != 1 || s.Errors != 0 {
		t.Fatalf("主 key 统计 = %+v, want calls 1 / errors 0（回退归属没生效？）", s)
	}
	if len(byID) != 2 {
		t.Fatalf("by_key = %+v, want 恰两行（子 Key + 主 Key）", byID)
	}
}
