package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
	"github.com/chipchipss/buddyhub/internal/usage"
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

// ── 阶段 4：额度门与记账的桥接层测试 ─────────────────────────

func TestQoderQuotaGateSkipsExhausted(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	key := qoder.AccountKey("qoder", "acct-x")
	h.qoderLedger.SetAllocation(key, 100)
	h.qoderLedger.RecordCredits(key, 100) // 精确记账打满
	if got := h.qoderQuotaGate(&extstore.ExtAccount{Provider: extstore.PQoder, ID: "acct-x"}); got != qoder.QuotaExhausted {
		t.Fatalf("exhausted gate = %v, want Exhausted", got)
	}
	// 未超限 → OK
	h.qoderLedger.SetAllocation(key, 200)
	if got := h.qoderQuotaGate(&extstore.ExtAccount{Provider: extstore.PQoder, ID: "acct-x"}); got != qoder.QuotaOK {
		t.Fatalf("within-limit gate = %v, want OK", got)
	}
	// 无账本配置 → QuotaOK（不限制）
	h2 := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	h2.qoderLedger = nil
	if got := h2.qoderQuotaGate(&extstore.ExtAccount{Provider: extstore.PQoder, ID: "any"}); got != qoder.QuotaOK {
		t.Fatalf("no-ledger gate = %v, want OK", got)
	}
}

func TestQoderRecordUsageWritesUsageRecorder(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(),
		Usage: usage.New("")})
	acct := &extstore.ExtAccount{Provider: extstore.PQoder, ID: "acct-u"}
	h.qoderRecordUsage(acct, "key-abc", "conv-1", "qmodel", qoder.UsageFromChatCompletion{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
	})
	snap := h.cfg.Usage.Snapshot(0, nil)
	if snap.Totals.TotalTokens != 150 || snap.Totals.PromptTokens != 100 {
		t.Fatalf("usage recorder not written: %+v", snap.Totals)
	}
	// per-Key 小时桶同源落一笔（fork keyOwner 维度）。
	if len(snap.ByKey) != 1 || snap.ByKey[0].ID != "key-abc" || snap.ByKey[0].TotalTokens != 150 {
		t.Fatalf("per-key usage not written: %+v", snap.ByKey)
	}
	if pts := h.cfg.Usage.KeySeries("key-abc", 24); len(pts) != 1 || pts[0].PromptTokens != 100 {
		t.Fatalf("per-key series = %+v, want pt100", pts)
	}
	// 零用量不写主桶，但 per-Key 仍记一次调用（成功完成的请求没带 usage 帧）。
	h.qoderRecordUsage(acct, "key-abc", "conv-1", "qmodel", qoder.UsageFromChatCompletion{})
	snap2 := h.cfg.Usage.Snapshot(0, nil)
	if snap2.Totals.TotalTokens != 150 {
		t.Fatalf("zero usage must not write main buckets: %+v", snap2.Totals)
	}
	if len(snap2.ByKey) != 1 || snap2.ByKey[0].Calls != 2 {
		t.Fatalf("per-key call count = %+v, want calls 2（零 usage 的成功请求也计）", snap2.ByKey)
	}
	// 空 keyID 不记 per-Key（未配置鉴权时无从归属）。
	h.qoderRecordUsage(acct, "", "conv-1", "qmodel", qoder.UsageFromChatCompletion{TotalTokens: 9})
	if snap3 := h.cfg.Usage.Snapshot(0, nil); len(snap3.ByKey) != 1 {
		t.Fatalf("empty keyID must not add key rows: %+v", snap3.ByKey)
	}
}

func TestQoderBridgeUsageAccumulatorRoundTrip(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	h.bridgeUsageReset("acct-s")
	// 模拟泵内帧：中间帧 + 末帧全量。
	h.bridgeUsageAddFrame("acct-s", `{"choices":[{"delta":{"content":"hi"}}]}`)
	h.bridgeUsageAddFrame("acct-s", `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	got := h.bridgeUsageTake("acct-s")
	if got.TotalTokens != 15 || got.PromptTokens != 10 {
		t.Fatalf("accumulated usage = %+v", got)
	}
	// Take 后清空。
	if again := h.bridgeUsageTake("acct-s"); again.TotalTokens != 0 {
		t.Fatalf("take must clear: %+v", again)
	}
}

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
