// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chipchipss/buddyhub/internal/auth"
	"github.com/chipchipss/buddyhub/internal/logfmt"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此优先消耗，见
	// entry.creditsExpiring）。<=0 时禁用分桶（全部归长期，行为与引入前一致）。
	// 默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool
	// ExtCheckinDisabled 显式关闭外部积分账号签到排程（schedule.ext_checkin_enabled=false）。
	ExtCheckinDisabled bool
	// ExtCheckinHours 外部签到时点（默认 [10]：各家积分多为每日一次，10 点整领）。
	ExtCheckinHours []int

	// TaskStateFile 日级任务状态落盘路径（state.json 的兄弟文件 task-state.json）。
	// 空 = 纯内存：重启即清零，与引入日状态前的行为逐字一致（供测试、以及没有
	// 数据目录的部署）。有了它，「进程重启 → 全池当天再签一遍」不再发生。
	TaskStateFile string

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()

	// ExtHook 外部积分账号签到回调（panel.extCheckinAll 同管线：遍历
	// lobsterai/raccoon/qoder/codearts 全部账号执行各自签到/领取）。nil 时到点跳过。
	// targets 非 nil 时只跑这些 "provider/id"（失败重试补跑用），nil = 全部账号。
	// 返回的结论进本轮台账、失败项进重试链。外部账号「今天跑过没」的事实源是
	// extstore.LastCheckin（已落 data/ext-accounts.json），所以这类任务不在这里
	// 另记一份日状态——同一件事两处记账，日后必漂移。
	ExtHook func(targets []string) []Outcome
	// BalanceExtHook 外部积分账号的余额刷新回调（panel.RunExtBalanceRefresh）。
	// 与上面的 ExtHook 不同：不签到、不续期，只让余额这个观测量保持新鲜；
	// 挂在同一个 5 分钟 ticker 上，不新增调度任务。
	BalanceExtHook func()
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// days 「今天这个账号这个任务成了没」的落盘台账（见 daystate.go）。
	// 排程槽位与重试补跑据此跳过已成的账号；面板手动触发不看它（按钮要能强刷）。
	days *dayState

	// tlog 本轮结论 + 失败重试链（见 tasklog.go）。
	tlog *taskLog

	// stateOnce 兜住「不经 New 直接构造 Scheduler{...}」的老写法（测试里仍有）：
	// 首次用到台账/日状态时补齐，避免 nil 解引用。
	stateOnce sync.Once
}

// ensureState 惰性补齐日状态与台账（New 已经建好，这里只兜直接构造的场景）。
func (s *Scheduler) ensureState() {
	s.stateOnce.Do(func() {
		if s.tlog == nil {
			s.tlog = newTaskLog()
		}
		if s.days == nil {
			s.days = newDayState(s.cfg.TaskStateFile)
		}
	})
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	if len(cfg.ExtCheckinHours) == 0 {
		cfg.ExtCheckinHours = []int{10}
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
		days:          newDayState(cfg.TaskStateFile),
		tlog:          newTaskLog(),
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// SetExtHook 挂载/替换外部积分签到回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetExtHook(fn func([]string) []Outcome) {
	s.schedMu.Lock()
	s.cfg.ExtHook = fn
	s.schedMu.Unlock()
}

// SetBalanceExtHook 挂载/替换外部平台余额刷新回调（与 SetExtHook 同批接线）。
func (s *Scheduler) SetBalanceExtHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.BalanceExtHook = fn
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
	taskExtCheckin // 外部积分账号签到（lobsterai/raccoon/qoder/codearts），extHook 回调
	taskStreak     // 连登管家（兑换已解锁档位 + 抽完次数），挂在签到排程里
)

// taskNames 任务的可读名（台账、日状态键、面板显示共用）。新增任务族必须在这里
// 有名，否则日状态键会变成 "6|uid" 这种没人看得懂的字符串。
var taskNames = map[taskKind]string{
	taskCheckin:    "checkin",
	taskTravel:     "travel",
	taskActivity:   "activity",
	taskKeepalive:  "keepalive",
	taskBlackcat:   "blackcat",
	taskGrowth:     "growth",
	taskExtCheckin: "ext-checkin",
	taskStreak:     "streak-bonus",
}

func taskName(k taskKind) string {
	if n, ok := taskNames[k]; ok {
		return n
	}
	return fmt.Sprintf("task%d", int(k))
}

// dayTracked 这类任务是否吃「今日已成」标记。
//
// travel 特意排除：它的状态机一天要跑两趟（09 点派出、21 点领奖），当日上限由
// 上游 travel/status 的 DailyLimitReached 与在途状态自己判——本地若按「今天跑过」
// 一刀切，会把 21 点那趟领奖直接砍掉，猫在站上领奖无人认领。
// growth 由 panel 侧异步队列自己按待办扫（队列本身即去重），ext-checkin 的日状态
// 在 extstore.LastCheckin（已落盘），两处都不在此重复记账。
func dayTracked(k taskKind) bool {
	switch k {
	case taskCheckin, taskActivity, taskKeepalive, taskBlackcat, taskStreak:
		return true
	}
	return false
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	extHours, extOff := s.cfg.ExtCheckinHours, s.cfg.ExtCheckinDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	if !extOff {
		slots = append(slots, slot{nextFire(now, extHours), taskExtCheckin})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// slotWaitSlice 槽位等待的分片时长：Windows Modern Standby 下挂钟会被冻结，
// 单个「一次性睡到点」的长 timer 在唤醒后可能既不错过也不立即到期——它的剩余
// 量是按睡眠前的单调时钟算的，于是数小时后的槽位实际会拖到唤醒后再补一大段
// 才触发（实测形态：09:00 槽位在唤醒 + 剩余单调片之后才跑）。改为每片重新
// 按墙钟算一次剩余，最坏漂移一片时长（90s），迟到部分交给 awaitWakeupGrace
// 的补跑宽限处理。测试可缩短（与 wakeupGraceDelay「测试可缩短」同口径）。
var slotWaitSlice = 90 * time.Second

// waitSlot 分段等待到 next（每片重读墙钟）。返回 true 表示已到点应当派发；
// 返回 false 表示 ctx 已取消或排程变更（调用方 return / 重算 nextWake）。
// next 已过期时立即返回 true（不睡负时长）。
func waitSlot(ctx context.Context, next time.Time, rearm <-chan struct{}) bool {
	for {
		remaining := time.Until(next)
		if remaining <= 0 {
			return true // 墙钟已过点：派发（迟到量由 awaitWakeupGrace 判定）
		}
		if remaining > slotWaitSlice {
			remaining = slotWaitSlice
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-rearm:
			timer.Stop()
			return false
		case <-timer.C:
			// 一片睡完：回到循环顶重读墙钟，睡眠冻结过的单调时钟在这里被纠正。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
//
// 两类唤醒共用这个循环：整点槽位（排程）与失败重试链（退避到点）。重试链优先——
// 它带的是刚失败的账号，把它拖到下一个整点等于把「瞬时失败」拖成「今天没跑成」
// （实测形态：唤醒瞬间的 TLS handshake timeout，一轮签到 6 条，原先只能等下一槽）。
func (s *Scheduler) Run(ctx context.Context) {
	s.ensureState()
	for {
		if kinds, targets, dueAt := s.dueRetries(time.Now()); len(kinds) > 0 {
			// 补跑也可能是「睡过槽位后刚醒」，同一套网络宽限（准点 due 不额外等）。
			if !awaitWakeupGrace(ctx, dueAt) {
				return
			}
			s.dispatchBatch(ctx, kinds, targets, "retry "+dueAt.Format("15:04"), false)
			continue
		}
		next, kinds := s.nextWake(time.Now())
		if rAt := s.tlog.nextRetryAt(); !rAt.IsZero() && (next.IsZero() || rAt.Before(next)) {
			// 下一次待补跑比任何槽位都早：睡到它，回循环顶由 dueRetries 分支派发。
			if !waitSlot(ctx, rAt, s.rearmSchedule) {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			continue
		}
		if next.IsZero() {
			// 全部任务禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		// 分段墙钟等待（见 waitSlot）：不把槽位压在一次性长 timer 上。
		if !waitSlot(ctx, next, s.rearmSchedule) {
			if ctx.Err() != nil {
				return // 退出信号：优雅停机
			}
			continue // 排程已变：重算下一次唤醒
		}
		// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
		// 迟到唤醒（睡眠跨过槽位时刻，分段等待在唤醒后才看到过点）先等网络宽限：
		// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
		// 的窗口里（issue #152）；准点触发零延迟不受影响。
		if !awaitWakeupGrace(ctx, next) {
			return // ctx 取消：放弃本批，优雅退出
		}
		s.dispatchBatch(ctx, kinds, nil, "slot "+next.Format("15:04"), true)
	}
}

// dueRetries 取出已到点的重试链（kind → 待试账号），并返回这批里最早的计划时刻。
func (s *Scheduler) dueRetries(now time.Time) ([]taskKind, map[taskKind][]string, time.Time) {
	kinds, targets := s.tlog.retryTargets(now)
	if len(kinds) == 0 {
		return nil, nil, time.Time{}
	}
	earliest := now
	for _, k := range kinds {
		if at := s.tlog.chainAt(k); !at.IsZero() && at.Before(earliest) {
			earliest = at
		}
	}
	return kinds, targets, earliest
}

// dispatchBatch 派发一批任务（同一时刻的多类，或一轮重试补跑），等全部完成返回。
// targets 为 nil 时各任务跑全部合格账号；newSlot 为真表示这是排程槽位——该族
// 的重试链清零（重试次数按轮计，不按日累积，否则 09:00 用完 3 次就轮到 21:00 没得试）。
//
// 每类一个 goroutine：慢任务族（如活跃上报 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞
// 同槽其他任务族。签到槽位顺带把连登管家带上（兑换/抽奖按「到天数那天」解锁，
// 挂在签到后跑，与引入重试链前的「RunCheckinNow 末尾调 RunStreakBonusNow」同语义）。
func (s *Scheduler) dispatchBatch(ctx context.Context, kinds []taskKind, targets map[taskKind][]string, trigger string, newSlot bool) {
	s.ensureState()
	if newSlot {
		for _, k := range kinds {
			s.tlog.resetChain(k)
		}
		if kindIn(kinds, taskCheckin) && !kindIn(kinds, taskStreak) {
			kinds = append(kinds, taskStreak)
		}
	}
	s.tlog.beginRound(trigger)
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.runKind(ctx, k, targets[k], false)
		}(k)
	}
	wg.Wait()
	s.endRoundLog()
}

// endRoundLog 收尾一轮并把汇总行打进日志（`task 轮[trigger]: checkin/done×2 …`）。
// 没有并发轮时返回非 nil，汇总行必打；有则等最后结束的那轮打，一次带全结论。
func (s *Scheduler) endRoundLog() {
	r := s.tlog.endRound()
	if r == nil {
		return
	}
	var pending int
	for _, uids := range s.tlog.snapshotChains() {
		pending += len(uids)
	}
	log.Printf("task 轮[%s]:%s 待补跑 %d 条", r.Trigger, tally(r.Outcomes), pending)
}

// manualRound 面板按钮的公共外壳：手动那一趟也要进台账。
// 否则用户点完「一键签到」回来看「最近一轮」，显示的还是几小时前的排程结果。
// trigger 带任务名，多个按钮同刻按下也能在日志里分得开。
func (s *Scheduler) manualRound(trigger string, run func(ctx context.Context)) {
	s.ensureState()
	s.tlog.beginRound(trigger)
	run(context.Background())
	s.endRoundLog()
}

func kindIn(kinds []taskKind, want taskKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// runKind 单类任务的一趟执行。uids 非 nil 时只跑这些账号（重试补跑）。
func (s *Scheduler) runKind(ctx context.Context, k taskKind, uids []string, bypassDay bool) {
	switch k {
	case taskCheckin:
		s.runCheckin(ctx, uids, bypassDay)
	case taskTravel:
		s.runTravel(ctx, uids, bypassDay)
	case taskActivity:
		s.runActivity(ctx, uids, bypassDay)
	case taskKeepalive:
		s.runKeepalive(ctx, uids, bypassDay)
	case taskBlackcat:
		s.runBlackcat(ctx, uids, bypassDay)
	case taskStreak:
		s.runStreak(ctx, uids, bypassDay)
	case taskGrowth:
		// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
		s.schedMu.Lock()
		hook := s.cfg.GrowthHook
		s.schedMu.Unlock()
		if hook != nil {
			hook()
		}
	case taskExtCheckin:
		// 外部积分签到：回调在 panel 侧（extstore.CheckinAll），nil 未挂载则跳过。
		s.schedMu.Lock()
		extHook := s.cfg.ExtHook
		s.schedMu.Unlock()
		if extHook == nil {
			return
		}
		for _, o := range extHook(uids) {
			s.noteExternal(o)
		}
	}
}

// RecordExternalCheckin 面板「一键外部签到」按钮的结论进台账。
// 按钮本身不走排程（它要在页面上同步把逐条结果回给用户），但结论必须落同一份
// 台账：不然用户点完按钮后台账里什么都看不见，失败项也进不了重试链——
// 「这一轮谁成了、谁为什么没成」就只剩排程那几轮。
func (s *Scheduler) RecordExternalCheckin(outcomes []Outcome) {
	s.manualRound("manual ext checkin", func(context.Context) {
		for _, o := range outcomes {
			s.noteExternal(o)
		}
	})
}

// noteExternal 外部签到的结论进台账 + 失败项进重试链。
// 账号标识用回调给的 "provider/id"（与 extstore 的账号主键一致，面板才能点对上）。
func (s *Scheduler) noteExternal(o Outcome) {
	if o.Task == "" {
		o.Task = taskName(taskExtCheckin)
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	s.tlog.note(o)
	switch o.Result {
	case resDone.String(), resSkip.String():
		// 成功/无需做：从链里摘掉（extstore 那边已记 LastCheckin）
		s.tlog.dropUID(taskExtCheckin, o.Account)
	case resFail.String():
		if s.tlog.scheduleRetry(taskExtCheckin, o.Account, time.Now()) {
			log.Printf("ext-checkin %s 失败(%s)，已排定重试", o.Account, o.Message)
		} else {
			// 与 finish 同口径：用尽即摘出，不留过期的计划时刻空转。
			s.tlog.dropUID(taskExtCheckin, o.Account)
			log.Printf("ext-checkin %s 失败(%s)，重试次数用尽，留给下一个排程槽位", o.Account, o.Message)
		}
	case resDead.String():
		s.tlog.dropUID(taskExtCheckin, o.Account)
	}
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// finish 单账号单任务的结论落地：记台账、按需记「今日已成」、按需排重试。
//
// 三条规矩都来自失败语义，不是装饰：
//   - 只有 resDone 才写日状态——失败写了就等于把失败当成功吞掉（同 extstore.markCheckedIn）；
//   - resDead 不进重试链：登录态已死，重试只是对废凭据轰炸，该由重登（自动/人工）处理；
//   - resSkip 既不记也不重试（停用 / global realm / 无凭据属正常态，不进台账免得淹没有效行）。
func (s *Scheduler) finish(k taskKind, uid string, res accountResult, msg string) {
	s.ensureState()
	switch res {
	case resDone:
		if dayTracked(k) {
			s.days.MarkDone(taskName(k), uid)
		}
		s.tlog.dropUID(k, uid)
	case resDead:
		s.tlog.dropUID(k, uid)
	case resFail:
		if s.tlog.scheduleRetry(k, uid, time.Now()) {
			log.Printf("task %s/%s 失败(%s)，已排定重试", taskName(k), uid, msg)
		} else {
			// 次数用尽必须**摘出链**：链里的计划时刻已过期，留着就是
			// 「立即再跑 → 又用尽 → 又立即再跑」的空转轰炸。摘掉后这个账号
			// 交给下一个排程槽位（槽位派发会清零重试链）。
			s.tlog.dropUID(k, uid)
			log.Printf("task %s/%s 失败(%s)，重试次数用尽，留给下一个排程槽位", taskName(k), uid, msg)
		}
	case resSkip:
		if msg != "" {
			log.Printf("task %s/%s 跳过：%s", taskName(k), uid, msg)
		}
		return
	}
	s.tlog.note(Outcome{Task: taskName(k), Account: uid, Result: res.String(), Message: msg, At: time.Now()})
}

// classifyUpstream 上游错误分诊：登录态已死 → 不重试；其余（网络 / 5xx / 未知业务错）
// → 瞬时失败，进重试链。
func classifyUpstream(err error) accountResult {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		return resDead
	}
	return resFail
}

// inScope uids 为空表示全部在范围内；否则只认列出的账号（重试补跑）。
func inScope(uids []string, uid string) bool {
	if len(uids) == 0 {
		return true
	}
	for _, u := range uids {
		if u == uid {
			return true
		}
	}
	return false
}

// poolAccount 取池内账号凭据；不合格（停用 / 无凭据 / 被 realm 门控挡住）时返回跳过原因。
// 四类任务族共用这套准入判定，避免同一份门控在五个文件里各抄一遍再各漏一条。
//
// gateGlobal 只对 CN 任务族为 true（签到/活跃/旅行/夜猫子在 global realm 没有体系）；
// token 保活必须放行 global——它的凭据一样要续，拒了就是把 global 号放任渴死。
func (s *Scheduler) poolAccount(st pool.Status, needRefreshToken, gateGlobal bool) (*auth.Auth, accountResult, string) {
	if st.Disabled {
		return nil, resSkip, ""
	}
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil {
		return nil, resSkip, ""
	}
	if needRefreshToken && a.RefreshTokenValue() == "" {
		return nil, resSkip, ""
	}
	if !needRefreshToken && a.AccessTokenValue() == "" {
		return nil, resSkip, ""
	}
	// D4 门控：realm=global 账号无 CN 任务体系，不发起任何上游调用。经 auth 统一
	// 判定：逃生门（global.enabled=false）下 global 账号降级为 cn、按 CN 处理。
	if gateGlobal && a.IsGlobal() {
		return nil, resSkip, ""
	}
	return a, resSkip, ""
}

// eligibleAccounts 一趟任务要处理的账号（按池内顺序）。
//   - uids 非 nil：只保留其中列出的账号（重试补跑）；
//   - bypassDay=false 且该类任务吃日状态：今日已成的直接不去问上游。
func (s *Scheduler) eligibleAccounts(k taskKind, uids []string, bypassDay bool) []pool.Status {
	var out []pool.Status
	for _, st := range s.cfg.Pool.List() {
		if !inScope(uids, st.UID) {
			continue
		}
		if !bypassDay && dayTracked(k) && s.days.Done(taskName(k), st.UID) {
			continue
		}
		out = append(out, st)
	}
	return out
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻（手动入口：不看日状态）。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 连登管家（streak.go）由排程槽位随签到一起派发（见 dispatchBatch）。
func (s *Scheduler) RunCheckinNow() {
	s.manualRound("manual checkin", func(ctx context.Context) { s.runCheckin(ctx, nil, true) })
}

// runCheckin 签到遍历。uids 非 nil 时只跑列出的账号（重试补跑）。
func (s *Scheduler) runCheckin(ctx context.Context, uids []string, bypassDay bool) {
	for _, st := range s.eligibleAccounts(taskCheckin, uids, bypassDay) {
		a, res, msg := s.poolAccount(st, true, true)
		if a == nil {
			s.finish(taskCheckin, st.UID, res, msg)
			continue
		}
		res, msg = s.checkinAccount(a)
		s.finish(taskCheckin, st.UID, res, msg)
	}
}

// checkinAccount 单账号：签到 + 分桶查余额 + 按余额解冻。
// 两步任一失败都算这趟失败（进重试链），因为「签到没成但余额查了」不是完成态。
func (s *Scheduler) checkinAccount(a *auth.Auth) (accountResult, string) {
	var msgs []string
	res := resDone
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
		if upstream.IsAlreadyCheckin(err) {
			msgs = append(msgs, "今天已签到（幂等）")
		} else {
			log.Printf("checkin %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			res = classifyUpstream(err)
			msgs = append(msgs, "checkin: "+err.Error())
		}
	}
	// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗。
	// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
	remain, total, expiring, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
	if err != nil {
		log.Printf("user-resource %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		if res == resDone {
			res = classifyUpstream(err)
		}
		msgs = append(msgs, "balance: "+err.Error())
		return res, joinMsg(msgs)
	}
	s.cfg.Pool.ReenableIfCredits(a.UID, remain, total)
	if expiring > 0 {
		s.cfg.Pool.SetCreditsDetailed(a.UID, remain, total, expiring)
	}
	return res, joinMsg(msgs)
}

// joinMsg 结论摘要拼一行（台账/message 字段单行可读）。
func joinMsg(msgs []string) string {
	return strings.Join(msgs, "; ")
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报（手动入口：不看日状态）。
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
func (s *Scheduler) RunActivityNow() {
	s.manualRound("manual activity", func(ctx context.Context) { s.runActivity(ctx, nil, true) })
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。账号间限速 activityAccountDelay。
// uids 非 nil 时只跑列出的账号（重试补跑）。
func (s *Scheduler) runActivity(ctx context.Context, uids []string, bypassDay bool) {
	first := true
	for _, st := range s.eligibleAccounts(taskActivity, uids, bypassDay) {
		a, res, msg := s.poolAccount(st, false, true)
		if a == nil {
			s.finish(taskActivity, st.UID, res, msg)
			continue
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		r, m := s.activityAccount(a)
		s.finish(taskActivity, st.UID, r, m)
	}
}

// activityAccount 单号上报 + streak 回读自检。
// streak 回读可疑**不算这趟失败**：上报本身已成功且按天幂等，重试它只会多打一次上报。
func (s *Scheduler) activityAccount(a *auth.Auth) (accountResult, string) {
	cid := fmt.Sprintf("buddyhub-%d", time.Now().UnixMilli())
	if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
		log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return classifyUpstream(err), err.Error()
	}
	if s.checkActivityStreak(a) {
		return resDone, "上报成功，streak 回读可疑（见日志）"
	}
	return resDone, ""
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token（手动入口：不看日状态）。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	s.manualRound("manual keepalive", func(ctx context.Context) { s.runKeepalive(ctx, nil, true) })
}

// runKeepalive token 保活遍历。uids 非 nil 时只跑列出的账号（重试补跑）。
//
// 分诊口径：session dead 不进重试链（重试废凭据没有意义，剩下的交给重登/人工），
// 网络类失败进链——唤醒瞬间那批 TLS 超时正是靠这条链在 2 分钟后补回来的。
func (s *Scheduler) runKeepalive(ctx context.Context, uids []string, bypassDay bool) {
	for _, st := range s.eligibleAccounts(taskKeepalive, uids, bypassDay) {
		a, res, msg := s.poolAccount(st, true, false)
		if a == nil {
			s.finish(taskKeepalive, st.UID, res, msg)
			continue
		}
		r, m := s.keepaliveAccount(a, st)
		s.finish(taskKeepalive, st.UID, r, m)
	}
}

// keepaliveAccount 单号 token 刷新 + 落盘。
func (s *Scheduler) keepaliveAccount(a *auth.Auth, st pool.Status) (accountResult, string) {
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			if s.cfg.Pool.NoteSessionDead(st.UID) {
				log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
			}
			return resDead, err.Error()
		}
		return resFail, err.Error()
	}
	s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
	if err := a.SaveAtomic(); err != nil {
		log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		return resFail, "凭据落盘失败: " + err.Error()
	}
	return resDone, ""
}

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			remain, total, expiring, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				return
			}
			if expiring > 0 {
				s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring)
			} else {
				s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			}
		}(a, st.UID)
	}
	wg.Wait()

	// 外部平台的余额此前只在打开面板时按需拉，不刷新的话面板显示的
	// 一直是上一次打开时的旧数。挂同一个 ticker，不新增调度任务。
	s.schedMu.Lock()
	extFn := s.cfg.BalanceExtHook
	s.schedMu.Unlock()
	if extFn != nil {
		extFn()
	}
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// Report 最近几轮任务台账 + 仍在等补跑的链 + 今日已成条数 + 下一个槽位。
// 面板 GET /panel/api/task_report 的唯一数据源。
func (s *Scheduler) Report() ReportView {
	s.ensureState()
	v := s.tlog.view()
	v.DayDone = s.days.PendingCount()
	if at, kinds := s.nextWake(time.Now()); !at.IsZero() {
		v.NextFire = at.Format("2006-01-02 15:04:05")
		v.NextKinds = make([]string, 0, len(kinds))
		for _, k := range kinds {
			name := taskName(k)
			if k == taskCheckin {
				name += "+" + taskName(taskStreak) // 连胜奖励与签到同批派发
			}
			v.NextKinds = append(v.NextKinds, name)
		}
	}
	return v
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}
