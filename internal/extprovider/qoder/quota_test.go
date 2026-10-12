package qoder

import (
	"math"
	"testing"
)

// ── 阶段 4：CreditLedger（fork apiKeyPolicy.js 语义）──

func TestCreditLedgerRecordAndExhaust(t *testing.T) {
	l := NewCreditLedger()
	key := "qoder/acct-1"
	l.SetAllocation(key, 100)
	if got := l.Gate(key, -1); got != QuotaUnknown {
		t.Fatalf("no data gate = %v, want QuotaUnknown (fail-open)", got)
	}
	if got := l.Gate(key, 0); got != QuotaOK {
		t.Fatalf("zero-used gate = %v, want OK", got)
	}
	// 精确记账 60：未超限。
	l.RecordCredits(key, 60)
	if got := l.Gate(key, -1); got != QuotaOK {
		t.Fatalf("60/100 gate = %v, want OK", got)
	}
	// 再记 45：累计 105 ≥ 100 → exhausted，不再路由到该号。
	l.RecordCredits(key, 45)
	if got := l.Gate(key, -1); got != QuotaExhausted {
		t.Fatalf("105/100 gate = %v, want Exhausted", got)
	}
	if !l.IsPrecise(key) {
		t.Fatal("precise flag not set after RecordCredits")
	}
	if r := l.Remaining(key, -1); r != 0 {
		t.Fatalf("remaining = %v, want 0", r)
	}
}

func TestCreditLedgerPreciseWinsOverEstimate(t *testing.T) {
	l := NewCreditLedger()
	key := "qoder/acct-2"
	l.SetAllocation(key, 100)
	l.RecordCredits(key, 80) // precise
	// 余额差估算说只用了 10（滞后/别的客户端消耗）——precise 后以账本为准。
	if got := l.EffectiveUsed(key, 10); got != 80 {
		t.Fatalf("effective used = %v, want 80 (precise wins)", got)
	}
	// 未 precise 场景：直接注账本（不经 RecordCredits，不置 precise 标志），
	// 估算 50 与账本 30 取大者 = 50（fork L220-224 max(consumed, previousUsed)）。
	l2 := NewCreditLedger()
	l2.mu.Lock()
	l2.used["qoder/acct-3"] = 30
	l2.mu.Unlock()
	if got := l2.EffectiveUsed("qoder/acct-3", 50); got != 50 {
		t.Fatalf("effective used = %v, want 50 (max of ledger/estimate)", got)
	}
	if got := l2.EffectiveUsed("qoder/acct-3", 10); got != 30 {
		t.Fatalf("effective used = %v, want 30 (ledger wins over smaller estimate)", got)
	}
}

func TestCreditLedgerNoLimitOK(t *testing.T) {
	l := NewCreditLedger()
	// 未设上限：有数据 OK，无数据 Unknown（fail-open 由调用方放行）。
	if got := l.Gate("qoder/x", -1); got != QuotaUnknown {
		t.Fatalf("no-limit no-data gate = %v", got)
	}
	if got := l.Gate("qoder/x", 999); got != QuotaOK {
		t.Fatalf("no-limit with-data gate = %v", got)
	}
	if r := l.Remaining("qoder/x", -1); r != -1 {
		t.Fatalf("no-limit remaining = %v, want -1", r)
	}
}

func TestCreditLedgerSnapshot(t *testing.T) {
	l := NewCreditLedger()
	l.SetAllocation("qoder/a", 100)
	l.RecordCredits("qoder/a", 40)
	snaps := l.Snapshot(map[string]float64{})
	if len(snaps) != 1 {
		t.Fatalf("snapshot len = %d", len(snaps))
	}
	s := snaps[0]
	if s.AccountKey != "qoder/a" || s.Allocated != 100 || s.Used != 40 || s.Remaining != 60 || s.Exhausted || !s.Precise {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestEstimateUsedFromBalance(t *testing.T) {
	// 正常消耗：baseline 100 - current 30 = 70。
	if got := EstimateUsedFromBalance(CreditObservation{InitialRemaining: 100, CurrentRemaining: 30}); got != 70 {
		t.Fatalf("used = %v, want 70", got)
	}
	// 额度上涨（活动/重置）：不算消耗（fork hasQuotaGrownAboveBaseline 同向）。
	if got := EstimateUsedFromBalance(CreditObservation{InitialRemaining: 100, CurrentRemaining: 150}); got != 0 {
		t.Fatalf("grown used = %v, want 0", got)
	}
	// 上游没给数：-1 不可用。
	if got := EstimateUsedFromBalance(CreditObservation{InitialRemaining: 100, CurrentRemaining: -1}); got != -1 {
		t.Fatalf("unavailable used = %v, want -1", got)
	}
}

func TestUsageAccumulatorOverwriteSemantics(t *testing.T) {
	a := NewUsageAccumulator()
	key := "qoder/acct-1"
	a.Reset(key)
	// 中途帧只有 completion 增量：只覆盖对应字段。
	a.AddFrame(key, `{"choices":[{"delta":{"content":"hi"}}]}`)
	a.AddFrame(key, `{"choices":[],"usage":{"completion_tokens":10}}`)
	// 末帧全量统计：覆盖出现的字段，未出现的保留。
	a.AddFrame(key, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)
	got := a.Take(key)
	if got.PromptTokens != 100 || got.CompletionTokens != 20 || got.TotalTokens != 120 {
		t.Fatalf("usage = %+v", got)
	}
	// Take 后清空：再 Take 零值。
	if again := a.Take(key); again.TotalTokens != 0 {
		t.Fatalf("take must clear: %+v", again)
	}
	// 无 usage 字段的帧不产生记录。
	a.AddFrame(key, `{"choices":[{"delta":{"content":"x"}}]}`)
	if u := a.Take(key); u.TotalTokens != 0 {
		t.Fatalf("frame without usage must not record: %+v", u)
	}
}

func TestParseUsageFrame(t *testing.T) {
	u, ok := ParseUsageFrame(`{"choices":[],"usage":{"prompt_tokens":5,"total_tokens":8}}`)
	if !ok || u.PromptTokens != 5 || u.TotalTokens != 8 {
		t.Fatalf("frame usage = %+v ok=%v", u, ok)
	}
	if _, ok := ParseUsageFrame("not json"); ok {
		t.Fatal("non-json must be !ok")
	}
	if _, ok := ParseUsageFrame(`{"choices":[{"delta":{"content":"x"}}]}`); ok {
		t.Fatal("frame without usage must be !ok")
	}
}

func TestParseChatUsage(t *testing.T) {	resp := map[string]any{"usage": map[string]any{
		"prompt_tokens":     float64(100),
		"completion_tokens": float64(50),
		"total_tokens":      float64(150),
	}}
	u, ok := ParseChatUsage(resp)
	if !ok || u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}
	if _, ok := ParseChatUsage(map[string]any{}); ok {
		t.Fatal("missing usage must be !ok")
	}
	// 超大值不溢出（float64 → int64 边界安全）。
	big := map[string]any{"usage": map[string]any{"total_tokens": float64(math.MaxInt32)}}
	u2, _ := ParseChatUsage(big)
	if u2.TotalTokens != math.MaxInt32 {
		t.Fatalf("big total = %d", u2.TotalTokens)
	}
}
