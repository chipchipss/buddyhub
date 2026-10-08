// extcheckin.go 外部积分账号签到 → 调度器台账（scheduler.Outcome）的转换层。
//
// 为什么单列一个文件：外部账号的**结论语义**和池内账号不一样。池内是
// "成功 / 上游说今天已签 / 瞬时失败"；外部平台还会回 `inactive`——那在面板上
// 是"Copilot 无签到体系"这种正常态，不是失败。若把 inactive 直接映射成失败，
// 重试链就会每天对同一个"根本没签到"的平台白打三轮上游。
// 所以这里显式分诊：claimed/already→resDone、inactive→resSkip、
// relogin/重登话术→resDead（不进重试链）、其余 failed→resFail。
package panel

import (
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/scheduler"
)

// extOutcomes 把外部签到结果翻译成调度器台账结论。
// 账号主键统一 "provider/id"（与 extstore 一致，面板点得对行）。
func extOutcomes(results []*extstore.CheckinResult) []scheduler.Outcome {
	out := make([]scheduler.Outcome, 0, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		var res scheduler.TaskResult
		switch {
		case r.Kind == "claimed" || r.Kind == "already-claimed":
			res = scheduler.ResultDone
		case r.Kind == "inactive":
			res = scheduler.ResultSkip
		case r.Kind == "relogin":
			// 续期能力表判死的凭据（renew.go）：重试它等于对注定失败的凭据
			// 反复打 token 端点，台账里写清「需人工重新授权」就到此为止。
			res = scheduler.ResultDead
		case r.Kind == "failed" && extstore.NeedsReloginText(r.Message):
			// 无续期路径的平台（扫码 / 粘贴 cookie / 设备码）只能靠上游话术判：
			// 它说「重新登录」，重试链就不该再补跑三轮。措辞表取自 extstore
			// （与续期判定同一份口径，两份列表早晚会漂）。
			res = scheduler.ResultDead
		default:
			res = scheduler.ResultFailed
		}
		out = append(out, scheduler.Outcome{
			Task:    "ext-checkin",
			Account: r.Provider + "/" + r.ID,
			Result:  res.String(),
			Message: r.Message,
		})
	}
	return out
}
