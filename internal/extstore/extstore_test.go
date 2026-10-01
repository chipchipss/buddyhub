package extstore

import (
	"encoding/json"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(t.TempDir())
}

// 「今天跑没跑过」是任务中心待办判定的唯一依据，四个分支必须都对：
// 新账号 / 跑过了 / 停用了 / 平台本来就没有签到能力。
func TestExtPendingOne(t *testing.T) {
	m := newTestManager(t)
	if err := m.Add(PQoder, "q1", "Qoder 一号", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := m.Add(PCopilot, "c1", "Copilot", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("add: %v", err)
	}
	today := time.Now().Format("2006-01-02")

	// 1) 今天还没跑过 → 待办
	if pending, note := m.ExtPendingOne(m.Find(PQoder, "q1")); !pending {
		t.Error("新账号应该在待办里")
	} else if note == "" {
		t.Error("待办要给个说明（前端要显示）")
	}

	// 2) 跑过了 → 摘掉
	m.markCheckedIn(PQoder, "q1")
	if pending, _ := m.ExtPendingOne(m.Find(PQoder, "q1")); pending {
		t.Error("今天跑过就不该再列待办")
	}
	// 日期真的落盘了（重启后依然不重复列）
	if got := m.Find(PQoder, "q1").LastCheckin; got != today {
		t.Errorf("LastCheckin = %q，期望 %q", got, today)
	}

	// 3) 停用 → 不是待办
	_ = m.SetDisabled(PQoder, "q1", true)
	m2 := NewManager(t.TempDir())
	_ = m2.Add(PQoder, "q2", "x", json.RawMessage(`{}`))
	_ = m2.SetDisabled(PQoder, "q2", true)
	if pending, _ := m2.ExtPendingOne(m2.Find(PQoder, "q2")); pending {
		t.Error("停用账号不该列待办")
	}

	// 4) 平台没有签到能力 → 不是待办（即使从没跑过）
	if pending, _ := m.ExtPendingOne(m.Find(PCopilot, "c1")); pending {
		t.Error("Copilot 没有每日签到，不该进待办")
	}

	// 5) nil 安全
	if pending, _ := m.ExtPendingOne(nil); pending {
		t.Error("nil 账号不该报待办")
	}
}

// 有签到能力的清单与面板注册表是**同一份事实的两个投影**，
// 这里钉住它本身；与注册表的一致性由 panel 包的测试核对。
func TestCheckinCapableMatchesCheckinOne(t *testing.T) {
	want := []string{PLogsterAI, PRaccoon, PQoder, PCodeArts, PLoomyCLI, PTraeWork, PAutoClaw}
	got := map[string]bool{}
	for _, p := range allProviders() {
		got[p] = CheckinCapable(p)
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("%s 应该有签到能力（CheckinOne 里实现了）", p)
		}
	}
	// 反向：对话通道没有签到，别被误标进去
	for _, p := range []string{PCopilot, PCline, PTrae, PAccio, PQClaw} {
		if CheckinCapable(p) {
			t.Errorf("%s 是网关直连通道，没有每日签到", p)
		}
	}
}

// allProviders 枚举全部 provider 常量（新平台加进来时这个测试会跟着更新）。
func allProviders() []string {
	return []string{
		PLogsterAI, PRaccoon, PQoder, PCodeArts, PLoomyCLI,
		PCopilot, PCline, PAutoClaw, PQClaw, PTrae, PAccio, PTraeWork,
	}
}
