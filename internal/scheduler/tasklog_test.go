// tasklog_test.go 重试链的语义：退避表、次数封顶、槽位清零、判死不重试。
//
// 这里最要紧的一条是「用尽即摘出」——链里的计划时刻一旦过期，留下的账号会被
// 主循环立刻再派发一次，形成「失败→用尽→立刻再跑」的空转轰炸。测试直接钉住它。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/auth"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

func withFastRetries(t *testing.T) {
	t.Helper()
	origDelays, origMax := taskRetryDelays, taskRetryMax
	taskRetryDelays = []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}
	taskRetryMax = 3
	t.Cleanup(func() { taskRetryDelays, taskRetryMax = origDelays, origMax })
}

func TestRetryChainBackoffTable(t *testing.T) {
	withFastRetries(t)
	l := newTaskLog()
	now := time.Now()

	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute} {
		if !l.scheduleRetry(taskCheckin, "u1", now) {
			t.Fatalf("第 %d 次失败应还能排重试", i+1)
		}
		got := l.chainAt(taskCheckin).Sub(now)
		if got != want {
			t.Fatalf("第 %d 次失败后延迟=%v，期望 %v", i+1, got, want)
		}
	}
	// 第 4 次：次数用尽，不再排（调用方据此把账号摘出链）。
	if l.scheduleRetry(taskCheckin, "u1", now) {
		t.Fatal("超过 taskRetryMax 还排得下——重试次数上限失效")
	}
}

// 用尽后链里不能留账号：留下的话 nextRetryAt 已过期，主循环会立刻再派发一次。
func TestRetryChainExhaustedDropsAccount(t *testing.T) {
	withFastRetries(t)
	s := &Scheduler{}
	s.ensureState()
	now := time.Now()
	for i := 0; i < taskRetryMax; i++ {
		s.tlog.scheduleRetry(taskCheckin, "u1", now)
	}
	s.finish(taskCheckin, "u1", resFail, "上游 5xx")
	if got := s.tlog.snapshotChains()[taskCheckin]; len(got) != 0 {
		t.Fatalf("重试已用尽却仍留着 %v ——主循环会空转轰炸上游", got)
	}
	if !s.tlog.nextRetryAt().IsZero() {
		t.Fatal("链已空却仍有待补跑时刻")
	}
}

func TestRetryTargetsOnlyWhenDue(t *testing.T) {
	withFastRetries(t)
	l := newTaskLog()
	now := time.Now()
	l.scheduleRetry(taskActivity, "u1", now)

	if kinds, _, _ := l.retryTargets(now.Add(30 * time.Second)); len(kinds) != 0 {
		t.Fatal("未到退避时刻就派发——退避等于没做")
	}
	kinds, targets, earliest := l.retryTargets(now.Add(2 * time.Minute))
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Fatalf("到点后应派发 activity，实得 %v", kinds)
	}
	if strings.Join(targets[taskActivity], ",") != "u1" {
		t.Fatalf("补跑范围应只含失败账号，实得 %v", targets[taskActivity])
	}
	if !earliest.Equal(l.chainAt(taskActivity)) {
		t.Fatalf("最早计划时刻=%v，应等于链上时刻 %v（迟到宽限据此判定）", earliest, l.chainAt(taskActivity))
	}
}

// 退避时刻按账号存，不按任务族共用一个最早值：B 在 A 之后 1 分钟才失败，
// 它自己还要等 2 分钟；若共用最早时刻，B 会被 A 的到点拖去提前补跑。
func TestRetryBackoffIsPerAccount(t *testing.T) {
	withFastRetries(t)
	l := newTaskLog()
	now := time.Now()
	l.scheduleRetry(taskCheckin, "a", now)                    // a: +1m
	l.scheduleRetry(taskCheckin, "b", now.Add(1*time.Minute)) // b: +2m → 09:03

	_, at1m, _ := l.retryTargets(now.Add(90 * time.Second))
	if len(at1m[taskCheckin]) != 1 || at1m[taskCheckin][0] != "a" {
		t.Fatalf("a 到点后应只补跑 a，实得 %v", at1m[taskCheckin])
	}
	_, at3m, _ := l.retryTargets(now.Add(3 * time.Minute))
	if got := strings.Join(at3m[taskCheckin], ","); got != "a,b" {
		t.Fatalf("两个账号都到点后应一并补跑，实得 %v", got)
	}
}

// 补跑批次一取就走：链里不能留下过期计划。留下的话主循环每轮都判它到点，
// 「终态跳过 / 账号已不在池」这两类结论永远消费不掉——10-08 21:00 实测
// 14 分钟空转 156 轮，日志里全是同一行「当日派出已达上限」。
func TestTakeRetriesDrainsDispatchedBatch(t *testing.T) {
	withFastRetries(t)
	l := newTaskLog()
	now := time.Now()
	l.scheduleRetry(taskCheckin, "u1", now)
	l.scheduleRetry(taskCheckin, "u2", now)
	l.scheduleRetry(taskTravel, "u3", now)
	l.scheduleRetry(taskCheckin, "u4", now.Add(2*time.Minute)) // +2m → now+3m，未到点

	kinds, targets, earliest := l.takeRetries(now.Add(90 * time.Second))
	if len(kinds) != 2 {
		t.Fatalf("两族都到点，实得 %v", kinds)
	}
	if got := strings.Join(targets[taskCheckin], ","); got != "u1,u2" {
		t.Fatalf("checkin 补跑范围=%q，期望 u1,u2", got)
	}
	if got := strings.Join(targets[taskTravel], ","); got != "u3" {
		t.Fatalf("travel 补跑范围=%q，期望 u3", got)
	}
	if want := now.Add(time.Minute); !earliest.Equal(want) {
		t.Fatalf("最早计划时刻=%v，期望 %v（迟到宽限据此判定）", earliest, want)
	}

	// 主循环下一轮立刻又问一次：已派发的那批不能再次到点（没有任何结论落地也一样，
	// 账号可能已被移出池子而根本遍历不到）。
	if k2, t2, _ := l.takeRetries(now.Add(91 * time.Second)); len(k2) != 0 {
		t.Fatalf("第二次取还有到点批次 %v %v ——主循环会无上限空转", k2, t2)
	}
	// 未到点的那条不受影响：drain 只吃掉这一批。
	if got := l.nextRetryAt(); !got.Equal(now.Add(3 * time.Minute)) {
		t.Fatalf("未到点的补跑被一起摘了：nextRetryAt=%v，期望 %v", got, now.Add(3*time.Minute))
	}
	if got := strings.Join(l.snapshotChains()[taskCheckin], ","); got != "u4" {
		t.Fatalf("待补跑快照=%q，期望只剩 u4", got)
	}
	if got := len(l.snapshotChains()[taskTravel]); got != 0 {
		t.Fatalf("travel 已全部派发，汇总行却还报 %d 条待补跑", got)
	}
}

// 摘的是计划时刻，不是重试计数：补跑那一轮再失败要接着退避表往下走
// （2→10→30 分钟），从第一档重来等于把「最多 3 次」变成「每次都还有 3 次」。
func TestTakeRetriesKeepsTryCountForBackoff(t *testing.T) {
	withFastRetries(t)
	l := newTaskLog()
	now := time.Now()
	l.scheduleRetry(taskCheckin, "u1", now) // 第 1 次失败 → +1m
	l.takeRetries(now.Add(2 * time.Minute))
	if !l.scheduleRetry(taskCheckin, "u1", now.Add(2*time.Minute)) {
		t.Fatal("第 2 次失败应还排得下")
	}
	if got := l.chainAt(taskCheckin).Sub(now.Add(2 * time.Minute)); got != 2*time.Minute {
		t.Fatalf("第 2 次失败后延迟=%v，期望 2m（计数被 drain 清掉就会退回 1m）", got)
	}
}

// 终态跳过必须消费掉待补跑：旅行 21 点撞上「当日派出已达上限」就是这一类。
func TestFinishSkipDropsPendingRetry(t *testing.T) {
	withFastRetries(t)
	s := &Scheduler{}
	s.ensureState()
	now := time.Now()
	s.tlog.scheduleRetry(taskTravel, "u1", now) // 领奖 500 → 排定补跑
	s.tlog.beginRound("retry 21:02")
	s.finish(taskTravel, "u1", resSkip, "当日派出已达上限")
	s.tlog.endRound()

	if !s.tlog.nextRetryAt().IsZero() {
		t.Fatalf("终态跳过后仍留着待补跑时刻 %v ——主循环会 5s 一轮空转", s.tlog.nextRetryAt())
	}
	if got := s.tlog.snapshotChains()[taskTravel]; len(got) != 0 {
		t.Fatalf("链里仍留着 %v", got)
	}
}

// 端到端复现 10-08 21:00 那次空转：领奖 500 排定补跑，补跑时上游已翻成
// idle + 当日上限（终态跳过），主循环必须只补一趟就安静下来。
func TestRunDoesNotSpinOnTerminalSkipRetry(t *testing.T) {
	fastTravel(t)
	withFastRetries(t)
	taskRetryDelays = []time.Duration{10 * time.Millisecond}
	oldGrace, oldSlice := wakeupGraceDelay, slotWaitSlice
	wakeupGraceDelay, slotWaitSlice = 0, 20*time.Millisecond
	t.Cleanup(func() { wakeupGraceDelay, slotWaitSlice = oldGrace, oldSlice })

	var statusCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case "/activity/growth/buddy/travel/status":
			statusCalls.Add(1)
			if statusCalls.Load() == 1 {
				// 第一趟：到站可领奖（随后 claim 500 → 排定补跑）。
				w.Write([]byte(`{"code":0,"data":{"state":"arrived","record_id":42}}`))
				return
			}
			// 补跑那一趟：上游已翻回 idle 且当日到顶——终态跳过。
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`))
		case "/activity/growth/buddy/travel/claim":
			w.WriteHeader(500)
			w.Write([]byte(`{"code":500,"msg":"grant credits failed"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	// 补跑时刻（+10ms）远早于任何整点槽位，所以这 300ms 里 Run 只可能被重试链唤醒。
	s, _ := newTravelScheduler(t, srv, "u1")

	s.RunTravelNow() // 领奖 500 → 排定补跑
	if s.tlog.nextRetryAt().IsZero() {
		t.Fatal("领奖 500 后没排定补跑，用例前提不成立")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	// 修复前这里会是几十次（每轮 5s 宽限被置 0，300ms 内狂刷）；修复后 = 首趟 + 补跑一趟。
	if n := statusCalls.Load(); n > 3 {
		t.Fatalf("travel/status 被打了 %d 次，期望 ≤3（终态跳过后主循环仍在空转补跑）", n)
	}
	if !s.tlog.nextRetryAt().IsZero() {
		t.Fatalf("补跑后仍留着待补跑时刻 %v", s.tlog.nextRetryAt())
	}
}

// 槽位派发清零：09:00 用完 3 次，21:00 那趟必须还有 3 次。
func TestResetChainPerSlot(t *testing.T) {
	withFastRetries(t)
	l := newTaskLog()
	now := time.Now()
	for i := 0; i < taskRetryMax; i++ {
		l.scheduleRetry(taskCheckin, "u1", now)
	}
	l.resetChain(taskCheckin)
	if !l.chainAt(taskCheckin).IsZero() {
		t.Fatal("resetChain 没清掉计划时刻")
	}
	if !l.scheduleRetry(taskCheckin, "u1", now) {
		t.Fatal("新槽位应从零开始计次")
	}
}

// 登录态已死：不进重试链（重试它等于对废凭据轰炸），也不写日状态。
func TestFinishDeadSessionDoesNotRetry(t *testing.T) {
	withFastRetries(t)
	s := &Scheduler{}
	s.ensureState()
	s.finish(taskKeepalive, "u1", resDead, "登录态已死")
	if got := s.tlog.snapshotChains()[taskKeepalive]; len(got) != 0 {
		t.Fatalf("判死却进了重试链: %v", got)
	}
	if s.days.Done("keepalive", "u1") {
		t.Fatal("判死不该记今日已成")
	}
}

func TestFinishDoneWritesDayStateAndSkipsRetry(t *testing.T) {
	s := &Scheduler{}
	s.ensureState()
	s.finish(taskCheckin, "u1", resDone, "签到成功")
	if !s.days.Done("checkin", "u1") {
		t.Fatal("成功必须记今日已成（重启后不再重跑）")
	}
	if !s.tlog.nextRetryAt().IsZero() {
		t.Fatal("成功不该有待补跑")
	}
	// travel 特意不吃日状态：09 点派出、21 点领奖是同一天两趟。
	s.finish(taskTravel, "u1", resDone, "旅行派出")
	if s.days.Done("travel", "u1") {
		t.Fatal("travel 记了日状态会把 21 点领奖砍掉")
	}
}

// 跳过（停用 / global / 无凭据）不进台账：淹没有效行的噪音。
func TestFinishSkipNotRecorded(t *testing.T) {
	s := &Scheduler{}
	s.ensureState()
	s.tlog.beginRound("test")
	s.finish(taskCheckin, "u1", resSkip, "")
	if r := s.tlog.endRound(); len(r.Outcomes) != 0 {
		t.Fatalf("skip 进了台账: %+v", r.Outcomes)
	}
}

func TestTallyFormat(t *testing.T) {
	line := tally([]Outcome{
		{Task: "checkin", Result: "done"},
		{Task: "checkin", Result: "done"},
		{Task: "checkin", Result: "failed"},
		{Task: "travel", Result: "needs_relogin"},
	})
	for _, want := range []string{"checkin/done×2", "checkin/failed×1", "travel/needs_relogin×1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("汇总行 %q 缺 %q", line, want)
		}
	}
}

// 面板台账快照：轮次倒序由前端排，这里保证字段齐、待补跑可读。
func TestReportViewShape(t *testing.T) {
	withFastRetries(t)
	s := &Scheduler{cfg: Config{CheckinHours: []int{9}}}
	s.ensureState()
	s.tlog.beginRound("slot 09:00")
	s.finish(taskCheckin, "u1", resFail, "网络超时")
	s.tlog.endRound()

	v := s.Report()
	if len(v.Rounds) != 1 || v.Rounds[0].Trigger != "slot 09:00" {
		t.Fatalf("轮次快照不对: %+v", v.Rounds)
	}
	if len(v.Pending) != 1 || v.Pending[0].Task != "checkin" || v.Pending[0].Tries != 1 {
		t.Fatalf("待补跑快照不对: %+v", v.Pending)
	}
	if v.NextFire == "" || len(v.NextKinds) == 0 {
		t.Fatalf("没有下一次槽位信息（面板该显示何时再跑）: %+v", v)
	}
}

// 面板按钮那一趟也要进台账：否则用户点完「一键签到」回来看「最近一轮」，
// 显示的还是几小时前排程的结果，手动跑的成功与失败全都看不见。
func TestManualRoundLandsInLedger(t *testing.T) {
	old := activityAccountDelay
	activityAccountDelay = 0
	taskRetryDelaysBak, taskRetryMaxBak := taskRetryDelays, taskRetryMax
	taskRetryDelays = []time.Duration{time.Hour}
	taskRetryMax = 0
	defer func() {
		activityAccountDelay = old
		taskRetryDelays, taskRetryMax = taskRetryDelaysBak, taskRetryMaxBak
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	s.RunActivityNow()

	v := s.Report()
	if len(v.Rounds) != 1 {
		t.Fatalf("手动那一趟没进台账: %+v", v.Rounds)
	}
	rd := v.Rounds[0]
	if !strings.HasPrefix(rd.Trigger, "manual") {
		t.Fatalf("trigger=%q，应以 manual 开头", rd.Trigger)
	}
	if len(rd.Outcomes) != 1 || rd.Outcomes[0].Result != resFail.String() {
		t.Fatalf("结论不对: %+v", rd.Outcomes)
	}
	// taskRetryMax=0：一次都不排重试，账号必须被摘出链（不留过期时刻）。
	if got := len(s.tlog.snapshotChains()[taskActivity]); got != 0 {
		t.Fatalf("重试用尽后链里仍留着 %d 个账号", got)
	}
}

// 面板「一键外部签到」按钮的结论也要落台账：它不打排程，结果只回给 HTTP 响应，
// 于是台账里「这一轮谁成了」永远看不到外部签到这一轮，失败项也进不了重试链。
func TestExternalCheckinLandsInLedger(t *testing.T) {
	taskRetryDelaysBak, taskRetryMaxBak := taskRetryDelays, taskRetryMax
	taskRetryDelays = []time.Duration{time.Minute}
	taskRetryMax = 3
	defer func() {
		taskRetryDelays, taskRetryMax = taskRetryDelaysBak, taskRetryMaxBak
	}()

	s := New(Config{Pool: pool.New("")})
	s.RecordExternalCheckin([]Outcome{
		{Task: "ext-checkin", Account: "raccoon/r1", Result: ResultDone.String(), Message: "已领取"},
		{Task: "ext-checkin", Account: "codearts/main", Result: ResultFailed.String(), Message: "HTTP 500"},
		{Task: "ext-checkin", Account: "traework/t1", Result: ResultDead.String(), Message: "需人工重新授权"},
	})

	v := s.Report()
	if len(v.Rounds) != 1 {
		t.Fatalf("按钮那一趟没进台账: %+v", v.Rounds)
	}
	rd := v.Rounds[0]
	if rd.Trigger != "manual ext checkin" {
		t.Fatalf("trigger=%q", rd.Trigger)
	}
	if len(rd.Outcomes) != 3 {
		t.Fatalf("结论条数不对: %+v", rd.Outcomes)
	}
	// 只有瞬时失败进重试链：成功/判死的账号留在链里就是「每唤醒一次补跑一遍」。
	chains := s.tlog.snapshotChains()[taskExtCheckin]
	if len(chains) != 1 || chains[0] != "codearts/main" {
		t.Fatalf("重试链应只剩瞬时失败那条，实得 %v", chains)
	}
	if rd.RetryPlanned != 1 {
		t.Fatalf("RetryPlanned=%d，期望 1", rd.RetryPlanned)
	}
}

// 结论到得比轮早也不行：note() 在没有开轮时是静默丢弃的，
// 所以 RecordExternalCheckin 必须自己开轮（这条盯着别把它改回裸 note）。
func TestExternalCheckinWithoutRoundIsNotSilentlyDropped(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	s.tlog.note(Outcome{Task: "ext-checkin", Account: "x/y", Result: ResultDone.String()})
	if len(s.Report().Rounds) != 0 {
		t.Fatalf("未开轮的结论不该凭空成轮: %+v", s.Report().Rounds)
	}
	s.RecordExternalCheckin([]Outcome{{Task: "ext-checkin", Account: "x/y", Result: ResultDone.String()}})
	if len(s.Report().Rounds) != 1 {
		t.Fatalf("走 RecordExternalCheckin 应成轮: %+v", s.Report().Rounds)
	}
}
