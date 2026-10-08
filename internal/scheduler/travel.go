// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
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

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。测试可置 0。
var adoptReportGap = 1050 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检（手动入口）。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
func (s *Scheduler) RunTravelNow() {
	s.manualRound("manual travel", func(ctx context.Context) { s.runTravel(ctx, nil, true) })
}

// runTravel 旅行巡检遍历。uids 非 nil 时只跑列出的账号（重试补跑）。
// 旅行不吃日状态（见 dayTracked）：09 点派出、21 点领奖是同一天两趟。
func (s *Scheduler) runTravel(ctx context.Context, uids []string, bypassDay bool) {
	first := true
	for _, st := range s.eligibleAccounts(taskTravel, uids, bypassDay) {
		a, res, msg := s.poolAccount(st, true, true)
		if a == nil {
			s.finish(taskTravel, st.UID, res, msg)
			continue
		}
		if !first {
			if !sleepCtx(ctx, travelAccountDelay) {
				return // 优雅停机：不等限速睡满
			}
		}
		first = false
		r, m := s.travelOne(a)
		s.finish(taskTravel, st.UID, r, m)
	}
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
func (s *Scheduler) travelOne(a *auth.Auth) (accountResult, string) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), "buddy-info: " + err.Error()
	}
	if buddy == nil {
		return s.travelAdopt(a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), "status: " + err.Error()
	}
	switch ts.State {
	case travelStateArrived:
		return s.travelClaim(a, ts)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
		return resSkip, ""
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
		return resSkip, "未知状态 " + ts.State
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) (accountResult, string) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return resSkip, "当日派出已达上限"
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), "depart: " + err.Error()
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
	return resDone, "已派出"
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) (accountResult, string) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return resSkip, "到站但无 record_id"
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return classifyUpstream(err), "claim: " + err.Error()
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
	return resDone, fmt.Sprintf("领奖 +%d", reward)
}

// travelAdopt 无猫时领养，链路：report → agreement → buddy/first。
//
// report 必须先跑（scripts/task_first_buddy.py 实测）：一条 chat_request_send 上报
// 点亮 growth 连登并**解锁 first_buddy 任务**；未上报时 buddy/first 会返回
// 400 "first_buddy task not completed yet"——该门槛的真实来源是"当日无活跃上报"，
// 不是账号问题（report.go 注释亦明确「解锁 first_buddy 任务（领养前置）」）。
// conversation 门槛未达标仍属预期行为，记一次当日已试后静默跳过，不再重试。
func (s *Scheduler) travelAdopt(a *auth.Auth) (accountResult, string) {
	if s.adoptTriedToday(a.UID) {
		return resSkip, ""
	}
	// 前置：解锁 first_buddy 任务（幂等；失败不阻塞，让 buddy/first 按既有错误路径暴露）。
	if err := s.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("buddyhub-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", logfmt.Label(a.UID, a.Nickname), err)
	} else {
		time.Sleep(adoptReportGap) // 给上游事件处理留时间（对齐脚本实测的 1.05s 间隔口径）
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), "agreement: " + err.Error()
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
		return resDone, "领养成功"
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
		return resSkip, "对话门槛未达，当日不再试"
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), "adopt: " + err.Error()
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
