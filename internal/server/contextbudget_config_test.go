package server

import (
	"testing"
)

// resolveContextBudget 的 per-provider 配置化测试（本批次新增）。
// 全局/关闭的基线行为沿用 contextbudget 包既有测试，这里只钉解析口径。

func newBudgetHandler(global int, byProvider map[string]int) *Handler {
	return NewHandler(Config{ContextBudget: global, ContextBudgetByProvider: byProvider})
}

func TestResolveContextBudgetPriority(t *testing.T) {
	h := newBudgetHandler(500_000, map[string]int{"qoder": 800_000, "zcode": 120_000})

	// per-provider 命中 → 用覆盖值
	if got := h.resolveContextBudget("zcode:glm-5"); got != 120_000 {
		t.Fatalf("zcode override = %d", got)
	}
	// 未命中的 provider → 全局值
	if got := h.resolveContextBudget("cursor:default"); got != 500_000 {
		t.Fatalf("fallback to global = %d", got)
	}
	// 裸名（腾讯池 workbuddy）→ 全局值
	if got := h.resolveContextBudget("glm-5"); got != 500_000 {
		t.Fatalf("bare model = %d", got)
	}
}

func TestResolveContextBudgetDisabled(t *testing.T) {
	// 全局 0 + 空 map = 关闭（逐字透传零回归）
	h := newBudgetHandler(0, nil)
	if got := h.resolveContextBudget("glm-5"); got != 0 {
		t.Fatalf("global-off = %d", got)
	}
	// 全局 0 但 per-provider 开了 → 该 provider 生效（覆盖优先）
	h2 := newBudgetHandler(0, map[string]int{"zcode": 100_000})
	if got := h2.resolveContextBudget("zcode:x"); got != 100_000 {
		t.Fatalf("provider-enables = %d", got)
	}
	// 其他 provider 仍关
	if got := h2.resolveContextBudget("cursor:x"); got != 0 {
		t.Fatalf("other provider stays off = %d", got)
	}
	// per-provider 显式配 0 = 该 provider 关闭（负值同理）
	h3 := newBudgetHandler(500_000, map[string]int{"zcode": 0, "cursor": -1})
	if got := h3.resolveContextBudget("zcode:x"); got != 0 {
		t.Fatalf("explicit 0 = %d", got)
	}
	if got := h3.resolveContextBudget("cursor:x"); got >= 0 {
		t.Fatalf("negative = %d", got)
	}
}

func TestResolveContextBudgetQoderClamp(t *testing.T) {
	// Chief 口径：Qoder 网关出站裁剪不得超过 900K 压缩线——
	// 误配更低会让客户端 compact 永远等不到触发。
	h := newBudgetHandler(0, map[string]int{"qoder": 2_000_000})
	if got := h.resolveContextBudget("qoder:ultimate"); got != 900_000 {
		t.Fatalf("qoder clamp = %d（期望 900000）", got)
	}
	// 配得比线低 → 尊重配置（低于压缩线不抬）
	h2 := newBudgetHandler(0, map[string]int{"qoder": 300_000})
	if got := h2.resolveContextBudget("qoder:lite"); got != 300_000 {
		t.Fatalf("qoder below line = %d", got)
	}
	// 全局值超线也钳（per-provider 没配 qoder 时）
	h3 := newBudgetHandler(5_000_000, nil)
	if got := h3.resolveContextBudget("qoder:lite"); got != 900_000 {
		t.Fatalf("global qoder clamp = %d", got)
	}
	// 非 qoder 不受钳
	if got := h3.resolveContextBudget("zcode:x"); got != 5_000_000 {
		t.Fatalf("non-qoder must not clamp = %d", got)
	}
}

func TestResolveContextBudgetNilHandlerSafe(t *testing.T) {
	// nil 配置（零值 Config）不炸：全关
	h := NewHandler(Config{})
	if got := h.resolveContextBudget("glm-5"); got != 0 {
		t.Fatalf("zero config = %d", got)
	}
}
