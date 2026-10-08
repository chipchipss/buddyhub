// renew_test.go 续期能力表的边界：判「只能人工」的口径、未注册平台一律不动，
// 以及能力表与签到能力表各自的账号面。
//
// 真打上游的续期（各家 token 端点）不在单测里跑——那是网络行为，归部署后
// 实测；这里钉的是「什么情况下网关会自己动手、什么情况下它必须闭嘴」。
package extstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestManualRenewHintCoversProviderWording(t *testing.T) {
	for _, msg := range []string{
		"缺少 refresh_token，需重新登录",
		"cookie 里没有 IMA-REFRESH-TOKEN，需重新抓取 cookie",
		// 没有续期路径的平台（扫码类）靠话术判：这句来自余额/签到侧的
		// 「缺少对话用的 sk key，请重新扫码登录」，重试链据此摘出账号。
		"缺少对话用的 sk key，请重新扫码登录",
	} {
		if !manualRenewHint(errors.New(msg)) {
			t.Errorf("%q 应判为「只能人工重登」", msg)
		}
	}
	// 瞬时错（网络 / 上游 5xx）不是重登信号：误判会让账号白白退出重试链。
	for _, msg := range []string{"续期失败：dial tcp timeout", "上游 503"} {
		if manualRenewHint(errors.New(msg)) {
			t.Errorf("%q 不该判为人工重登", msg)
		}
	}
}

// 未注册续期器的平台（扫码 / 粘贴凭据 / 永久 AK/SK）一律 renewNone：
// 一次上游都不替它试，交给原动作去撞，撞出来的原因由签到侧判定。
func TestRenewSkipsProvidersWithoutRenewer(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, id := range []string{PQClaw, PTraeWork, PCodeArts, PMarvis, PLogsterAI, PLoomyCLI} {
		a := &ExtAccount{Provider: id, ID: "x", Cred: json.RawMessage(`{}`)}
		st, reason := m.renew(context.Background(), a)
		if st != renewNone {
			t.Errorf("%s: 无续期路径却返回 %s(%s)", id, st, reason)
		}
	}
}

// 凭据解析失败 ≠ 需要重登：JSON 坏了是本地问题，不该把「需人工重新授权」
// 这句谎话写进台账。
func TestRenewOnUnparseableCredIsNoneNotManual(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, id := range RenewableProviders() {
		a := &ExtAccount{Provider: id, ID: "x", Cred: json.RawMessage("this is not json")}
		st, reason := m.renew(context.Background(), a)
		if st != renewNone {
			t.Errorf("%s: 坏凭据应 renewNone（让原动作报「凭据解析失败」），实得 %s: %s", id, st, reason)
		}
	}
}

// 没到续期点的账号一律不打 token 端点——否则每次签到都多一发续期请求。
func TestRenewSkipsFreshCredentials(t *testing.T) {
	m := NewManager(t.TempDir())
	cases := map[string]string{
		// accio/trae/cline/qoder：expires_at 远在未来；raccoon 同口径。
		PAccio:   `{"access_token":"a","refresh_token":"r","expires_at":9999999999999}`,
		PTrae:    `{"access_token":"a","refresh_token":"r","expires_at":9999999999999}`,
		PCline:   `{"access_token":"a","refresh_token":"r","expires_at":9999999999999}`,
		PQoder:   `{"access_token":"a","refresh_token":"r","expire_time":9999999999999}`,
		PRaccoon: `{"access_token":"a","refresh_token":"r","expires_at":9999999999999}`,
	}
	for id, cred := range cases {
		a := &ExtAccount{Provider: id, ID: "x", Cred: json.RawMessage(cred)}
		st, reason := m.renew(context.Background(), a)
		if st != renewNone {
			t.Errorf("%s: 未到期不该续期，实得 %s: %s", id, st, reason)
		}
	}
}

// 无 refresh_token 的账号（accio/trae/cline）到点时：Refresh 会说「缺少
// refresh_token，需重新登录」——那是人工重登，但**不在这一次打出去**：
// NeedsRefresh 依赖 expires_at，缺 expires_at 时先判未到期。
func TestRenewWithoutRefreshTokenDoesNotCallUpstream(t *testing.T) {
	m := NewManager(t.TempDir())
	a := &ExtAccount{Provider: PAccio, ID: "x", Cred: json.RawMessage(`{"access_token":"a"}`)}
	st, _ := m.renew(context.Background(), a)
	if st != renewNone {
		t.Fatalf("无过期时间应不续期，实得 %s", st)
	}
	// 到点且无 refresh_token 时才是人工：这条靠 manualRenewHint 的分类，见上面的话术测试。
	a.Cred = json.RawMessage(`{"access_token":"a","expires_at":1}`)
	st, reason := m.renew(context.Background(), a)
	if st != renewManual {
		t.Fatalf("到点且无 refresh_token 应判人工，实得 %s: %s", st, reason)
	}
	if !strings.Contains(reason, "refresh_token") {
		t.Fatalf("人工重登要带上缺什么: %s", reason)
	}
}

// CheckinOne 在判人工重登时不能打签到上游，Kind 必须是 relogin（面板与调度器
// 都靠它把账号从重试链里摘出来）。
func TestCheckinOneReturnsReloginWithoutUpstreamCall(t *testing.T) {
	m := NewManager(t.TempDir())
	// accio 没有签到体系（checkinCapable 里不存在），但续期判定先于分派：
	// 到点且无 refresh_token → 直接 relogin，不进 switch。
	if err := m.Add(PAccio, "x", "x", json.RawMessage(`{"access_token":"a","expires_at":1}`)); err != nil {
		t.Fatalf("预置账号: %v", err)
	}
	res := m.CheckinOne(context.Background(), m.Find(PAccio, "x"))
	if res.Kind != "relogin" || !strings.Contains(res.Message, "需人工重新授权") {
		t.Fatalf("Kind=%s Message=%s，期望 relogin + 需人工重新授权", res.Kind, res.Message)
	}
	// 判人工重登不记「今天跑过」：重登成功后当天还该能签。
	if a := m.Find(PAccio, "x"); a.LastCheckin != "" {
		t.Fatalf("失败不该记日状态，实得 %s", a.LastCheckin)
	}
}

// 能力表导出口径：可续期平台必须成对齐全（面板注册表靠它对齐「自动续期」的承诺）。
func TestRenewableProvidersNonEmptyAndStable(t *testing.T) {
	got := RenewableProviders()
	if len(got) < 5 {
		t.Fatalf("续期能力表只剩 %v —— 签到/任务路径又会带着过期凭据打上游", got)
	}
	for _, id := range got {
		if !CanRenew(id) {
			t.Errorf("%s 在名单里却 CanRenew=false", id)
		}
	}
	if CanRenew(PQClaw) || CanRenew(PTraeWork) {
		t.Error("扫码/粘贴凭据类平台不该有自动续期路径")
	}
}
