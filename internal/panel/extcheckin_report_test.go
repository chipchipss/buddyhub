// extcheckin_report_test.go 外部签到结论 → 调度器台账的分诊表。
//
// 错一格的代价都是看得见的：inactive 当失败 → 每天对没有签到体系的平台白打
// 三轮重试；relogin 当失败 → 对废凭据轰炸 token 端点。
package panel

import (
	"testing"

	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/scheduler"
)

func TestExtOutcomesClassification(t *testing.T) {
	cases := []struct {
		name string
		in   *extstore.CheckinResult
		want string
	}{
		{"claimed", &extstore.CheckinResult{Provider: "raccoon", ID: "a", Kind: "claimed"}, "done"},
		{"already", &extstore.CheckinResult{Provider: "raccoon", ID: "a", Kind: "already-claimed"}, "done"},
		{"inactive 不是失败", &extstore.CheckinResult{Provider: "copilot", ID: "a", Kind: "inactive"}, "skip"},
		{"relogin 判死", &extstore.CheckinResult{Provider: "accio", ID: "a", Kind: "relogin",
			Message: "需人工重新授权：缺少 refresh_token"}, "needs_relogin"},
		{"重登话术判死", &extstore.CheckinResult{Provider: "loomy-cli", ID: "a", Kind: "failed",
			Message: "凭据里没有 session，需重新登录"}, "needs_relogin"},
		{"瞬时失败留重试", &extstore.CheckinResult{Provider: "qoder", ID: "a", Kind: "failed",
			Message: "dial tcp: i/o timeout"}, "failed"},
	}
	var results []*extstore.CheckinResult
	for _, c := range cases {
		results = append(results, c.in)
	}
	got := extOutcomes(results)
	if len(got) != len(cases) {
		t.Fatalf("台账 %d 条，输入 %d 条", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i].Result != c.want {
			t.Errorf("%s: Kind=%s 映射成 %s，期望 %s", c.name, c.in.Kind, got[i].Result, c.want)
		}
		if want := c.in.Provider + "/" + c.in.ID; got[i].Account != want {
			t.Errorf("%s: 账号主键 %q，期望 %q（面板按它点对行）", c.name, got[i].Account, want)
		}
		if got[i].Task != "ext-checkin" {
			t.Errorf("%s: 任务名 %q", c.name, got[i].Task)
		}
	}
}

// nil 结果不该出现在台账里（CheckinAll 理论不产，但转换层不能 panic）。
func TestExtOutcomesSkipsNilResults(t *testing.T) {
	got := extOutcomes([]*extstore.CheckinResult{nil, {Provider: "raccoon", ID: "a", Kind: "claimed"}})
	if len(got) != 1 || got[0].Result != scheduler.ResultDone.String() {
		t.Fatalf("台账=%+v", got)
	}
}
