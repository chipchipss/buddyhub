package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/zcode"
)

/* ── 阶段②复核整改：观察 2 + 1 + 3 的测试补充 ─────────────── */

// 观察 2：zcodeMessagesURL 表驱动（cn/→bigmodel 是核心路由语义，钉死）。

func TestZcodeMessagesURL(t *testing.T) {
	cases := []struct {
		site string
		want string
	}{
		{"zai", "https://api.z.ai/api/anthropic/v1/messages"},
		{"bigmodel", "https://open.bigmodel.cn/api/anthropic/v1/messages"},
		{"unknown-site", "https://api.z.ai/api/anthropic/v1/messages"}, // fork siteOf：非 bigmodel 一律 zai
		{"", "https://api.z.ai/api/anthropic/v1/messages"},
	}
	for _, tc := range cases {
		if got := zcodeMessagesURL(tc.site); got != tc.want {
			t.Errorf("zcodeMessagesURL(%q) = %q, want %q", tc.site, got, tc.want)
		}
	}
}

func TestZcodeSiteFor(t *testing.T) {
	if site, model := zcodeSiteFor("cn/GLM-5.3"); site != "bigmodel" || model != "GLM-5.3" {
		t.Fatalf("cn/ = %s/%s", site, model)
	}
	if site, model := zcodeSiteFor("GLM-5.3"); site != "zai" || model != "GLM-5.3" {
		t.Fatalf("bare = %s/%s", site, model)
	}
}

// 观察 1 端到端：非流式 200+1113 → 入账 + 429 语义回客户端（不再伪装成功）。

func TestZcodeBridge200SpentEndToEnd(t *testing.T) {
	up := &fakeZcodeUpstream{status: 200, body: `{"code":1113,"msg":"no resource package"}`}
	h := newZcodeBridgeHarness(t, up, zcode.Credential{APIKey: "sk"})
	h.qoderLedger.SetAllocation(qoder.AccountKey("zcode", "zc0"), 10)

	w := postZcode(t, h, "zcode:GLM-5.3", `{"model":"zcode:GLM-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (spent must not pass as success): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "quota_exhausted") {
		t.Fatalf("client error shape wrong: %s", w.Body.String())
	}
	if got := h.qoderLedger.UsedCredits(qoder.AccountKey("zcode", "zc0")); got != 1 {
		t.Fatalf("ledger used = %v, want 1", got)
	}
}

// 观察 3 端到端：JWT-only 凭据在桥入口被拦截（范围声明变强制约束）。

func TestZcodeBridgeJWTOnlySkipped(t *testing.T) {
	hits := 0
	up := &fakeZcodeUpstream{status: 200, body: `{"content":[]}`,
		onRequest: func() { hits++ }}
	h := newZcodeBridgeHarness(t, up, zcode.Credential{AccessToken: "jwt-only"})
	w := postZcode(t, h, "zcode:GLM-5.3", `{"model":"zcode:GLM-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "API Key") {
		t.Fatalf("must tell user to configure API key: %s", w.Body.String())
	}
	if hits != 0 {
		t.Fatalf("JWT-only must not hit upstream, hits = %d", hits)
	}
}

// 双凭据并存（API key + JWT）：走 coding key 路径，不受 JWT 拦截影响。

func TestZcodeBridgeDualCredentialUsesAPIKey(t *testing.T) {
	hits := 0
	up := &fakeZcodeUpstream{status: 200, body: `{"content":[{"type":"text","text":"ok"}]}`,
		onRequest: func() { hits++ }}
	h := newZcodeBridgeHarness(t, up, zcode.Credential{AccessToken: "jwt", APIKey: "sk-x"})
	w := postZcode(t, h, "zcode:GLM-5.3", `{"model":"zcode:GLM-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK || hits != 1 {
		t.Fatalf("dual credential must route on api key: status=%d hits=%d", w.Code, hits)
	}
}

// Chief 裁定①流内版：SSE 流里的 error 事件（1113/1005）触发入账 + noteChat，
// 事件本身照常透传（不消费流）。

func TestZcodeBridgeStreamSpentCheck(t *testing.T) {
	up := &fakeZcodeUpstream{status: 200,
		body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":1113,\"message\":\"spent\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"tail\"}}\n\n",
		contentType: "text/event-stream",
		onRequest:   func() {}}
	h := newZcodeBridgeHarness(t, up, zcode.Credential{APIKey: "sk"})
	h.qoderLedger.SetAllocation(qoder.AccountKey("zcode", "zc0"), 5)

	// 流式请求：客户端要流。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zcode:GLM-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := h.qoderLedger.UsedCredits(qoder.AccountKey("zcode", "zc0")); got != 1 {
		t.Fatalf("stream spent not recorded: ledger = %v", got)
	}
	// 流照常透传（后续正文帧不丢）——error 事件本身被翻译泵丢弃是 zai 转换器
	// 的既有行为（Anthropic error event 无 OpenAI chunk 对应），账本+冷却才是
	// 流内耗尽的交付物。
	if !strings.Contains(w.Body.String(), "tail") {
		t.Fatalf("stream must continue after error event: %s", w.Body.String())
	}
}
