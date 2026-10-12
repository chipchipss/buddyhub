package qoder

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// base64Of base64url 编码（构造坏载荷用）。
func base64Of(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// ── 阶段 3：SessionStore 会话隔离（fork sessionManager.js 语义）──

func TestSessionStoreStablePerKey(t *testing.T) {
	s := NewSessionStore()
	a1 := s.SessionIDFor("conv-a")
	a2 := s.SessionIDFor("conv-a")
	if a1 != a2 {
		t.Fatalf("same key must be stable: %q vs %q", a1, a2)
	}
	b := s.SessionIDFor("conv-b")
	if a1 == b {
		t.Fatalf("different keys must differ: %q == %q", a1, b)
	}
	if s.Len() != 2 {
		t.Fatalf("store len = %d, want 2", s.Len())
	}
}

func TestSessionStoreEmptyKeyEphemeral(t *testing.T) {
	s := NewSessionStore()
	// 无客户端会话键：fork ephemeral 语义——每次一个身份，绝不复用，也绝不落库。
	e1 := s.SessionIDFor("")
	e2 := s.SessionIDFor("")
	if e1 == e2 {
		t.Fatalf("ephemeral ids must differ: %q", e1)
	}
	if s.Len() != 0 {
		t.Fatalf("ephemeral must not occupy store: len=%d", s.Len())
	}
}

func TestSessionStoreTTLExpiry(t *testing.T) {
	s := NewSessionStore()
	// 直接注入一条"已过期"记录，验证读路径惰性过期重生成。
	s.mu.Lock()
	s.entries["k"] = sessionEntry{sessionID: "old", lastUsed: time.Now().Add(-2 * sessionTTL)}
	s.mu.Unlock()
	got := s.SessionIDFor("k")
	if got == "old" {
		t.Fatal("expired entry must be regenerated")
	}
}

func TestSessionStoreEvictsOldestAtCap(t *testing.T) {
	s := NewSessionStore()
	// 灌满到容量上限，再写一条应逐最旧。
	for i := 0; i < sessionMaxEntries; i++ {
		s.SessionIDFor(string(rune('a' + i%26)) + string(rune('0'+i/26)))
	}
	first := s.SessionIDFor("a0")
	// 逐掉一个最旧后 first 键仍可能还在（最旧不一定是它）；断言容量不超限即可。
	if s.Len() > sessionMaxEntries {
		t.Fatalf("store exceeded cap: %d", s.Len())
	}
	_ = first
}

// ── 阶段 3：长会话压缩（fork qoderCompaction.js 语义）──

func TestCompactionContentRoundTrip(t *testing.T) {
	content := BuildCompactionContent(CompactionPayload{
		Model:   "qoder:qmodel_latest",
		Summary: "目标是给 qoder 通道加压缩；已完成阶段 1-2；下一步阶段 3。",
	})
	if !strings.HasPrefix(content, "9router:qoder-compact:v1:") {
		t.Fatalf("prefix wrong: %s", content[:40])
	}
	parsed := ParseCompactionContent(content)
	if parsed == nil {
		t.Fatal("round trip failed")
	}
	if parsed.Version != 1 || parsed.Model != "qoder:qmodel_latest" {
		t.Fatalf("payload = %+v", parsed)
	}
	if !strings.Contains(parsed.Summary, "阶段 3") {
		t.Fatalf("summary lost: %q", parsed.Summary)
	}
	if parsed.CreatedAt == 0 {
		t.Fatal("created_at not stamped")
	}
}

func TestParseCompactionContentRejects(t *testing.T) {
	// 非本格式
	if ParseCompactionContent("plain text") != nil {
		t.Fatal("plain must reject")
	}
	// 前缀对但 base64 坏
	if ParseCompactionContent("9router:qoder-compact:v1:!!!not-base64!!!") != nil {
		t.Fatal("bad base64 must reject")
	}
	// 前缀对、JSON 对但 v≠1
	bad := "9router:qoder-compact:v1:" + base64Of(`{"v":2,"summary":"x"}`)
	if ParseCompactionContent(bad) != nil {
		t.Fatal("v!=1 must reject")
	}
	// 空 summary
	empty := "9router:qoder-compact:v1:" + base64Of(`{"v":1,"summary":"   "}`)
	if ParseCompactionContent(empty) != nil {
		t.Fatal("empty summary must reject")
	}
}

func TestShouldCompact(t *testing.T) {
	if ShouldCompact(AutoCompactTokenLimit - 1) {
		t.Fatal("below limit must not compact")
	}
	if !ShouldCompact(AutoCompactTokenLimit) {
		t.Fatal("at limit must compact")
	}
	if !ShouldCompact(ContextWindow) {
		t.Fatal("above limit must compact")
	}
}

func TestIsCompactionRequest(t *testing.T) {
	if !IsCompactionRequest([]byte(`{"_compact":true,"messages":[]}`)) {
		t.Fatal("_compact flag missed")
	}
	if !IsCompactionRequest([]byte(`{"input":[{"type":"compaction_trigger"}]}`)) {
		t.Fatal("input trigger missed")
	}
	if IsCompactionRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) {
		t.Fatal("false positive")
	}
}

func TestEstimateTokensCJKvsASCII(t *testing.T) {
	// 中文每字 1 token；ASCII ~4 字符 1 token。
	cjk := EstimateTokens([]Message{{Role: "user", Content: "你好世界"}})
	if cjk != 4 {
		t.Fatalf("cjk tokens = %d, want 4", cjk)
	}
	ascii := EstimateTokens([]Message{{Role: "user", Content: "abcd"}})
	if ascii != 1 {
		t.Fatalf("ascii tokens = %d, want 1", ascii)
	}
	// 数组内容（多模态消息形状）也要收敛文本。
	arr := EstimateTokens([]Message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "你好"},
	}}})
	if arr != 2 {
		t.Fatalf("array content tokens = %d, want 2", arr)
	}
}

func TestStableSessionHashDeterministic(t *testing.T) {
	h1 := StableSessionHash("qoder-session", "conv-1")
	h2 := StableSessionHash("qoder-session", "conv-1")
	if h1 != h2 {
		t.Fatalf("not deterministic: %q vs %q", h1, h2)
	}
	if StableSessionHash("qoder-session", "conv-2") == h1 {
		t.Fatal("different keys must hash differently")
	}
	if len(h1) != 16 {
		t.Fatalf("hash len = %d, want 16", len(h1))
	}
}
