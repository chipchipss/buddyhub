package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 阶段 3：会话隔离与压缩的桥接层测试 ─────────────────────── */

// 隔离验证走 Handler.qoderSessionID（真实存储）+ BuildChatBody 链路：
// 同会话键两次派生必须同 session_id，不同键必须不同，空键一次性。

func TestQoderSessionIsolationStablePerKey(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	a := h.qoderSessionID("conv-1")
	b := h.qoderSessionID("conv-1")
	if a != b {
		t.Fatalf("same key must be stable: %q vs %q", a, b)
	}
	c := h.qoderSessionID("conv-2")
	if a == c {
		t.Fatalf("different keys must differ: %q == %q", a, c)
	}
}

func TestQoderSessionEphemeralWithoutKey(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	// 无客户端会话键：fork ephemeral 语义，一次性身份且不占存储。
	e1 := h.qoderSessionID("")
	e2 := h.qoderSessionID("")
	if e1 == e2 {
		t.Fatalf("ephemeral must differ: %q", e1)
	}
	if h.qoderSessions.Len() != 0 {
		t.Fatalf("ephemeral must not occupy store: %d", h.qoderSessions.Len())
	}
}

func TestQoderBuildChatBodyCarriesSessionID(t *testing.T) {
	// session_id 必须稳定出现在出站 body 的 metadata.context 里（隔离的载体）。
	body := []byte(`{"model":"qmodel","messages":[{"role":"user","content":"hi"}]}`)
	type ctxProbe struct {
		Metadata struct {
			Context struct {
				SessionID string `json:"session_id"`
				RequestID string `json:"request_id"`
			} `json:"context"`
		} `json:"metadata"`
	}
	out, err := qoder.BuildChatBody(body, qoder.ChatMeta{SessionID: "sess-fixed"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p1 ctxProbe
	if json.Unmarshal(out, &p1) != nil {
		t.Fatalf("unmarshal: %s", out)
	}
	if p1.Metadata.Context.SessionID != "sess-fixed" {
		t.Fatalf("session_id = %q, want sess-fixed", p1.Metadata.Context.SessionID)
	}
	// request_id 仍每次不同（防 Duplicate）。
	out2, _ := qoder.BuildChatBody(body, qoder.ChatMeta{SessionID: "sess-fixed"})
	var p2 ctxProbe
	if json.Unmarshal(out2, &p2) != nil {
		t.Fatalf("unmarshal2: %s", out2)
	}
	if p1.Metadata.Context.RequestID == "" {
		t.Fatal("request_id not generated")
	}
	if p1.Metadata.Context.RequestID == p2.Metadata.Context.RequestID {
		t.Fatalf("request_id must be regenerated: %q == %q", p1.Metadata.Context.RequestID, p2.Metadata.Context.RequestID)
	}
	// session_id 两次一致（隔离稳定）。
	if p1.Metadata.Context.SessionID != p2.Metadata.Context.SessionID {
		t.Fatalf("session_id must be stable: %q vs %q", p1.Metadata.Context.SessionID, p2.Metadata.Context.SessionID)
	}
}

// 压缩触发请求不应被当普通对话处理（fork isQoderCompactionRequest 语义）：
// _compact 标记与 compaction_trigger 项都要识别。

func TestQoderCompactionRequestDetection(t *testing.T) {
	if !qoder.IsCompactionRequest([]byte(`{"_compact":true,"messages":[]}`)) {
		t.Fatal("_compact missed")
	}
	if !qoder.IsCompactionRequest([]byte(`{"input":[{"type":"compaction_trigger"}]}`)) {
		t.Fatal("trigger missed")
	}
	if qoder.IsCompactionRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) {
		t.Fatal("false positive")
	}
}

// 长会话估算达到 900K 线时应触发压缩判断（fork 1M/900K 固定策略）。

func TestQoderCompactionThreshold(t *testing.T) {
	if qoder.ShouldCompact(qoder.AutoCompactTokenLimit - 1) {
		t.Fatal("below 900K must not compact")
	}
	if !qoder.ShouldCompact(qoder.AutoCompactTokenLimit) {
		t.Fatal("at 900K must compact")
	}
	// 压缩内容往返（桥接侧只验证原语可用）。
	content := qoder.BuildCompactionContent(qoder.CompactionPayload{Summary: "阶段 3 测试摘要"})
	if !strings.HasPrefix(content, "9router:qoder-compact:v1:") {
		t.Fatalf("prefix: %s", content[:32])
	}
	if p := qoder.ParseCompactionContent(content); p == nil || p.Summary != "阶段 3 测试摘要" {
		t.Fatalf("round trip: %+v", p)
	}
}

var _ = http.StatusOK
