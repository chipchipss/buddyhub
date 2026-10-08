// tasklog.go 任务执行台账：单账号结论 → 日状态落盘 + 失败重试链 + 本轮汇总。
//
// 三件事分开做，是因为它们各自的失败语义不同：
//   - 日状态（daystate）只认「今天成了」，决定后续槽位/重启后要不要再打上游；
//   - 重试链只认「这一轮瞬时失败」，指数退避最多 taskRetryMax 次，跨轮清零；
//   - 台账只记结论，给面板出「这一轮谁成了、谁为什么没成」。
//
// 登录态已死（resDead）**不进重试链**：重试它等于对注定失败的凭据反复轰炸，
// 该走的是重登（自动或人工），由 reason 里写清楚。
package scheduler

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// accountResult 单账号单任务的执行结论。
type accountResult int

const (
	resDone accountResult = iota // 成功（含上游幂等成功）→ 记今日已成
	resSkip                      // 不适用（停用 / global / 无凭据 / 窗口外）→ 不记不重试
	resFail                      // 瞬时失败（网络 / 5xx / 未知业务错）→ 进重试链
	resDead                      // 登录态已死 → 不重试，等重登（自动或人工）
)

func (r accountResult) String() string {
	switch r {
	case resDone:
		return "done"
	case resSkip:
		return "skip"
	case resFail:
		return "failed"
	case resDead:
		return "needs_relogin"
	}
	return "unknown"
}

// TaskResult 结论枚举的跨包导出面：panel（外部签到回调）用它给 Outcome 填
// Result，而不是手写字符串去对上 "done"——字符串对齐是看不见的耦合，
// 改了一边不会编译报错。
type TaskResult = accountResult

const (
	ResultDone   = resDone
	ResultSkip   = resSkip
	ResultFailed = resFail
	ResultDead   = resDead
)

// Outcome 一条台账记录（也是跨包导出给面板/外部签到回调用的结论）。
type Outcome struct {
	Task    string    `json:"task"`
	Account string    `json:"account"`
	Result  string    `json:"result"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// round 一轮派发（一个槽位时刻上的全部任务，或一次重试补跑）的台账。
type round struct {
	Started  time.Time `json:"started"`
	Trigger  string    `json:"trigger"` // slot 09:00 / retry 09:02 / manual
	Outcomes []Outcome `json:"outcomes"`
	// RetryPlanned 本轮排定的重试条数（>0 表示还有下一次补跑）。
	RetryPlanned int `json:"retry_planned"`
}

// retryChain 某一类任务的重试进度：待试账号 → 已试次数 + 各自的到点时刻。
//
// 时刻按账号存，不按任务族存：同族里 A 在 09:00 失败、B 在 09:01 失败，若共用
// 一个「最早到点」，B 会被拖去 09:02 补跑——它的 10 分钟退避等于没执行。
type retryChain struct {
	tries map[string]int
	at    map[string]time.Time
}

// taskRetryMax 单个重试链内每账号最多再试几次（同类项目通行取 3）。
var taskRetryMax = 3

// taskRetryDelays 退避表：第 n 次失败后等 taskRetryDelays[n-1]。
// 取 2/10/30 分钟：覆盖「刚唤醒网络没起来」和「上游瞬时 5xx」两类，
// 又不至于贴着下一个整点槽位重复轰炸。测试可缩短。
var taskRetryDelays = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// reportMu 保护台账与重试链（派发循环、面板读取、账号 goroutine 三方并发）。
// 与 schedMu（排程参数）、mu（领养日记录）互不嵌套，取锁顺序无交叉。
type taskLog struct {
	mu     sync.Mutex
	rounds []round // 最近几轮，旧的丢
	cur    *round
	chains map[taskKind]*retryChain
	// open 同时开着几轮（排程槽位与面板手动触发可以重叠）。计数是为了**不丢行**：
	// 若后结束的那轮直接把 cur 关掉，先开那轮剩下的结论就没地方记了。
	open int
}

func newTaskLog() *taskLog {
	return &taskLog{chains: map[taskKind]*retryChain{}}
}

// beginRound 开一轮台账（trigger 供人读：槽位时刻 / 重试 / 手动）。
// 已有轮开着时共用同一个桶，trigger 取先开那轮的——两轮结果各自有日志行。
func (l *taskLog) beginRound(trigger string) {
	l.mu.Lock()
	l.open++
	if l.cur == nil {
		l.cur = &round{Started: time.Now(), Trigger: trigger}
	}
	l.mu.Unlock()
}

// note 记一条结论。
func (l *taskLog) note(o Outcome) {
	l.mu.Lock()
	if l.cur != nil {
		l.cur.Outcomes = append(l.cur.Outcomes, o)
	}
	l.mu.Unlock()
}

// endRound 收尾本轮并保留最近 historyRounds 轮，返回刚结束的那轮（供汇总行）。
// 还有别的轮没结束时返回 nil（汇总行由最后一个结束的人打，它带着全部结论）。
func (l *taskLog) endRound() *round {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open > 0 {
		l.open--
	}
	if l.open > 0 {
		return nil
	}
	done := l.cur
	l.cur = nil
	if done != nil {
		l.rounds = append(l.rounds, *done)
		if len(l.rounds) > historyRounds {
			l.rounds = l.rounds[len(l.rounds)-historyRounds:]
		}
	}
	return done
}

const historyRounds = 8

// chainAt 该类任务里最早的待补跑时刻（零值 = 无链）。
func (l *taskLog) chainAt(k taskKind) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.chains[k]; ch != nil {
		return earliestLocked(ch)
	}
	return time.Time{}
}

// earliestLocked 链内最早到点时刻（调用方须持 l.mu）。
func earliestLocked(ch *retryChain) time.Time {
	var earliest time.Time
	for _, at := range ch.at {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest
}

// snapshotChains 当前所有待补跑链（汇总行统计条数用）。
func (l *taskLog) snapshotChains() map[taskKind][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[taskKind][]string, len(l.chains))
	for k, ch := range l.chains {
		uids := make([]string, 0, len(ch.tries))
		for uid := range ch.tries {
			uids = append(uids, uid)
		}
		out[k] = uids
	}
	return out
}

// resetChain 新一轮槽位开跑：该类任务的重试链清零（次数按轮计，不按日累积，
// 否则 09:00 用完了 3 次，21:00 的排程槽位就没得重试了）。
func (l *taskLog) resetChain(k taskKind) {
	l.mu.Lock()
	delete(l.chains, k)
	l.mu.Unlock()
}

// scheduleRetry 记一次失败并排定该账号的下一次补跑。
// 返回是否还排得下（超出 taskRetryMax 就不再排）。
func (l *taskLog) scheduleRetry(k taskKind, uid string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := l.chains[k]
	if ch == nil {
		ch = &retryChain{tries: map[string]int{}, at: map[string]time.Time{}}
		l.chains[k] = ch
	}
	ch.tries[uid]++
	n := ch.tries[uid]
	if n > taskRetryMax {
		return false
	}
	delay := taskRetryDelays[n-1]
	if n-1 >= len(taskRetryDelays) {
		delay = taskRetryDelays[len(taskRetryDelays)-1]
	}
	ch.at[uid] = now.Add(delay)
	// 本轮排了几条补跑就记几条：面板的「待补跑」列读的就是这个数。
	// 之前它从不赋值，台账永远显示 retry_planned=0，而日志同一轮写「待补跑 6 条」——
	// 面板告诉用户「这轮全干净」，实际有六条在排队。
	if l.cur != nil {
		l.cur.RetryPlanned++
	}
	return true
}

// retryTargets 到点的重试任务 + 各自待试账号；未到点的账号留在链里等下一次。
func (l *taskLog) retryTargets(now time.Time) ([]taskKind, map[taskKind][]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var kinds []taskKind
	out := map[taskKind][]string{}
	for k, ch := range l.chains {
		var uids []string
		for uid, at := range ch.at {
			if at.IsZero() || at.After(now) {
				continue
			}
			uids = append(uids, uid)
		}
		if len(uids) == 0 {
			continue
		}
		sort.Strings(uids)
		out[k] = uids
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds, out
}

// nextRetryAt 链里最早的待补跑时刻（零值表示无待试）。
func (l *taskLog) nextRetryAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	var earliest time.Time
	for _, ch := range l.chains {
		if at := earliestLocked(ch); !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	return earliest
}

// clearKind 该类任务本轮跑完（成功或彻底放弃）后摘掉这些账号，避免下次唤醒又补跑。
func (l *taskLog) clearKind(k taskKind, uids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := l.chains[k]
	if ch == nil {
		return
	}
	for _, uid := range uids {
		delete(ch.tries, uid)
		delete(ch.at, uid)
	}
	if len(ch.tries) == 0 {
		delete(l.chains, k)
	}
}

// dropUID 该账号在这类任务上已了结（成功 / 判死 / 彻底放弃）：从链里摘掉。
func (l *taskLog) dropUID(k taskKind, uid string) {
	l.clearKind(k, []string{uid})
}

// ReportView 面板可见的台账快照。
type ReportView struct {
	Rounds  []round        `json:"rounds"`
	Pending []PendingRetry `json:"pending"`
	DayDone int            `json:"day_done"`
	// NextFire/NextKinds 下一个排程槽位（面板「下一次」列）；全禁用时为空。
	NextFire  string   `json:"next_fire,omitempty"`
	NextKinds []string `json:"next_kinds,omitempty"`
}

// PendingRetry 一条仍在等待补跑的任务。
type PendingRetry struct {
	Task   string   `json:"task"`
	UIDs   []string `json:"uids"`
	NextAt string   `json:"next_at"`
	Tries  int      `json:"tries"`
}

// view 快照最近几轮 + 当前待重试链。
func (l *taskLog) view() ReportView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := ReportView{Rounds: make([]round, 0, len(l.rounds))}
	out.Rounds = append(out.Rounds, l.rounds...)
	for k, ch := range l.chains {
		uids := make([]string, 0, len(ch.tries))
		tries := 0
		for uid, n := range ch.tries {
			uids = append(uids, uid)
			if n > tries {
				tries = n
			}
		}
		sort.Strings(uids)
		out.Pending = append(out.Pending, PendingRetry{
			Task: taskName(k), UIDs: uids, Tries: tries,
			NextAt: earliestLocked(ch).Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(out.Pending, func(i, j int) bool { return out.Pending[i].Task < out.Pending[j].Task })
	return out
}

// tally 把一轮结论聚成一行可读汇总（签到 成5/幂等0/败1/跳3）。
func tally(outcomes []Outcome) string {
	counts := map[string]int{}
	for _, o := range outcomes {
		counts[o.Task+"/"+o.Result]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	line := ""
	for _, k := range keys {
		line += fmt.Sprintf(" %s×%d", k, counts[k])
	}
	return line
}
