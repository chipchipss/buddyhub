// 签到分派的兜底口径：平台「没有签到体系」和「签到失败」是两件事。
//
// 实测（部署后打 /panel/api/ext/checkin_all）暴露过这一条：ima 落到 switch 的
// default 报 failed「未知平台」，于是它每天进重试链、对同一个根本没签到能力的
// 平台白打三轮上游。
package extstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 没有签到能力的平台必须判 inactive（面板/台账据此记「无需做」），
// 且不能因为「不认识这个平台」就判失败。
func TestCheckinUnknownProviderIsInactiveNotFailed(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, id := range []string{PIMA, PTrae, PAccio, PQClaw} {
		if checkinCapable[id] {
			t.Fatalf("%s 竟被登记为有签到能力，与用例前提冲突", id)
		}
		if err := m.Add(id, "x", "x", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("预置账号 %s: %v", id, err)
		}
		res := m.CheckinOne(context.Background(), m.Find(id, "x"))
		if res.Kind != "inactive" {
			t.Errorf("%s: Kind=%s Message=%s，期望 inactive", id, res.Kind, res.Message)
		}
		// 「无需签到」不是待办：扫描今日待办时不该把它列进去催用户。
		if pending, _ := m.ExtPendingOne(m.Find(id, "x")); pending {
			t.Errorf("%s: 无签到体系却算今日待办", id)
		}
	}
}

// 续期判定**先于**签到分派：空凭据的 cline/copilot 根本没有可看的签到分支，
// 先报「需人工重新授权」才是有用信息（它们确实要重登），而不是等签到那步
// 拿一份废凭据去撞上游。
func TestCheckinRenewGuardRunsBeforeDispatch(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, id := range []string{PCline, PCopilot} {
		if err := m.Add(id, "x", "x", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("预置账号 %s: %v", id, err)
		}
		res := m.CheckinOne(context.Background(), m.Find(id, "x"))
		if res.Kind != "relogin" || !strings.Contains(res.Message, "需人工重新授权") {
			t.Errorf("%s: Kind=%s Message=%s，期望 relogin + 需人工重新授权", id, res.Kind, res.Message)
		}
	}
}

// 反过来：checkinCapable 里登记了、switch 却没有分支的平台，是真故障（代码漏配），
// 必须照旧报 failed —— 把这条判成 inactive 会把「我们忘了实现」藏起来。
func TestCheckinCapableButUnimplementedStillFails(t *testing.T) {
	const fake = "unittest-capable-no-case"
	checkinCapable[fake] = true
	defer delete(checkinCapable, fake)

	m := NewManager(t.TempDir())
	if err := m.Add(fake, "x", "x", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("预置账号: %v", err)
	}
	res := m.CheckinOne(context.Background(), m.Find(fake, "x"))
	if res.Kind != "failed" || !strings.Contains(res.Message, "未知平台") {
		t.Fatalf("Kind=%s Message=%s，期望 failed + 未知平台", res.Kind, res.Message)
	}
}
