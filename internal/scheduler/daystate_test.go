// daystate_test.go 只管「今天做过没有」这一件事：落盘、重启后仍在、跨日作废、
// 以及 path 为空时退回纯内存（老测试与无数据目录的场景）。
package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDayStateMarksPersistAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	d := newDayState(path)
	if d.Done("checkin", "u1") {
		t.Fatal("新状态表不该有任何已成记录")
	}
	d.MarkDone("checkin", "u1")
	if !d.Done("checkin", "u1") {
		t.Fatal("MarkDone 后同进程内就该判成")
	}

	reopened := newDayState(path)
	if !reopened.Done("checkin", "u1") {
		t.Fatal("重启后记录丢失——重启即重跑一整天上游，正是这张表要治的")
	}
	if reopened.Done("checkin", "u2") {
		t.Fatal("不该凭空白给别的账号记成")
	}
	if reopened.Done("activity", "u1") {
		t.Fatal("同账号不同任务必须分开记")
	}
}

// 跨日必须由 day 字段判：昨天的记录今天一律不算，否则今天永远排不上签到。
func TestDayStateIgnoresStaleDayOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	yesterday := travelDay(time.Now().AddDate(0, 0, -1))
	raw, _ := json.Marshal(dayRecord{Day: yesterday, Done: map[string]int64{"checkin|u1": time.Now().Unix()}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("预置昨日状态: %v", err)
	}
	d := newDayState(path)
	if d.Done("checkin", "u1") {
		t.Fatal("昨日的成不算今日的成")
	}
	if n := d.PendingCount(); n != 0 {
		t.Fatalf("PendingCount=%d，跨日后应为 0", n)
	}
}

// 进程跑到跨零点（长驻服务必然遇到）：下一次判定即整表作废。
func TestDayStateRollsOverAtMidnight(t *testing.T) {
	d := newDayState("")
	d.MarkDone("checkin", "u1")
	d.mu.Lock()
	d.day = travelDay(time.Now().AddDate(0, 0, -1)) // 假装表还是昨天建的
	d.done = map[string]int64{"checkin|u1": time.Now().Unix()}
	d.mu.Unlock()

	if d.Done("checkin", "u1") {
		t.Fatal("日切后仍判成——今天这个号不会再签到")
	}
}

func TestDayStateCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("预置坏文件: %v", err)
	}
	d := newDayState(path)
	if d.Done("checkin", "u1") {
		t.Fatal("坏文件不该判成")
	}
	d.MarkDone("checkin", "u1") // 写回去要能自愈
	var rec dayRecord
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &rec) != nil {
		t.Fatalf("写盘未自愈: %v %s", err, raw)
	}
}

func TestDayStateSkipsRedundantWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	d := newDayState(path)
	d.MarkDone("checkin", "u1")
	before, _ := os.Stat(path)
	d.MarkDone("checkin", "u1") // 今日已有记录，不该再写一次
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("重复 MarkDone 触发了多余落盘")
	}
}

// 完成时刻是排查「什么时候记的成」的唯一线索（判定本身只看存在性）。
func TestDayStateRecordsTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	d := newDayState(path)
	d.MarkDone("keepalive", "u9")
	var rec dayRecord
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读状态文件: %v", err)
	}
	if json.Unmarshal(raw, &rec) != nil {
		t.Fatalf("状态文件不是合法 JSON: %s", raw)
	}
	if ts := rec.Done["keepalive|u9"]; ts == 0 || time.Since(time.Unix(ts, 0)) > time.Minute {
		t.Fatalf("记录时间戳不可信: %d", ts)
	}
}

// 并发落盘：dispatchBatch 每类任务一个 goroutine，它们都会 MarkDone。写盘若不加
// 序列，多个 writer 会同时往同一个 path+".tmp" 写再 rename——要么写出混合内容
// （下次启动解析失败 = 当日整表作废），要么旧快照最后落盘（丢记录）。两种结果
// 都是这张表要防的那件事：重启后全池重复签到。
func TestDayStateConcurrentMarksAllPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	d := newDayState(path)

	const n = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让 n 个 writer 同一刻进入 save()
			d.MarkDone("checkin", "u"+strconv.Itoa(i))
		}(i)
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读状态文件: %v", err)
	}
	var rec dayRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("并发写留下了不可解析的文件: %v\n%s", err, raw)
	}
	if len(rec.Done) != n {
		t.Fatalf("并发写丢了 %d 条记录（剩 %d，应为 %d）——旧快照覆盖了新快照", n-len(rec.Done), len(rec.Done), n)
	}
	reopened := newDayState(path)
	for i := 0; i < n; i++ {
		uid := "u" + strconv.Itoa(i)
		if !reopened.Done("checkin", uid) {
			t.Fatalf("重启后 %s 的今日已成标记丢失", uid)
		}
	}
}

// 残留的 .tmp 不该被当成状态文件读进来（进程被砍在 rename 之前会留下它）。
func TestDayStateLeavesNoTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-state.json")
	d := newDayState(path)
	d.MarkDone("checkin", "u1")
	matches, _ := filepath.Glob(path + "*")
	for _, m := range matches {
		if m != path && !strings.HasSuffix(m, ".json") {
			t.Fatalf("写盘后残留临时文件: %s", m)
		}
	}
}

// nil 接收者：直接构造的 &Scheduler{}（测试常见）没有 days，判定必须走「没做过」。
func TestDayStateNilReceiver(t *testing.T) {
	var d *dayState
	if d.Done("checkin", "u1") || d.PendingCount() != 0 {
		t.Fatal("nil 状态表应一律判「今天没跑过」")
	}
	d.MarkDone("checkin", "u1") // 不 panic 即为通过
}
