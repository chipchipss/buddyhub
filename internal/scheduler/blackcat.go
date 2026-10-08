// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/chipchipss/buddyhub/internal/auth"
	"github.com/chipchipss/buddyhub/internal/logfmt"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（手动入口：不看日状态）。
func (s *Scheduler) RunBlackcatNow() {
	s.manualRound("manual blackcat", func(ctx context.Context) { s.runBlackcat(ctx, nil, true) })
}

// runBlackcat 夜猫子遍历，由 blackcat_hours 排程（默认 [23]）触发；
// 执行前二次校验 InNightWindow。uids 非 nil 时只跑列出的账号（重试补跑）。
func (s *Scheduler) runBlackcat(ctx context.Context, uids []string, bypassDay bool) {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.eligibleAccounts(taskBlackcat, uids, bypassDay) {
		a, res, msg := s.poolAccount(st, false, true)
		if a == nil {
			s.finish(taskBlackcat, st.UID, res, msg)
			continue
		}
		r, m := s.blackcatAccount(a)
		s.finish(taskBlackcat, st.UID, r, m)
		if r == resDone {
			time.Sleep(activityAccountDelay) // 账号间限速，避免上游风控
		}
		if ctx.Err() != nil {
			return // 优雅停机：剩余账号留给下一轮
		}
	}
}

// blackcatAccount 单号：查还差几次 → 补足对话。
// need<=0 也算这趟完成（「今天要补的已经补上了」），明日槽位因此不去问上游。
func (s *Scheduler) blackcatAccount(a *auth.Auth) (accountResult, string) {
	need, err := s.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), err.Error()
	}
	if need <= 0 {
		return resDone, "无需补足"
	}
	ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
	if err != nil {
		log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
		return resFail, fmt.Sprintf("%d/%d 中断: %s", ok, need, err)
	}
	log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), ok)
	return resDone, fmt.Sprintf("补足 %d 次", ok)
}
