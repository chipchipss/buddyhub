// daystate.go 日级任务状态：哪些「任务 × 账号」今天已经跑成，落盘。
//
// 为什么需要：排程只在进程内存里记「今天跑过」，进程重启（改配置部署、崩溃、
// 笔记本重启）后同一天的任务会**整批重跑**——签到本身幂等，但每次重跑都是对
// 上游真实发一轮请求，这类重复请求正是风控最想看到的。外部账号早有这个落盘
// 语义（extstore 的 LastCheckin 记在 data/ext-accounts.json），池内账号没有，
// 于是「重启一次 = 全池再签到一遍」。这里补上同一份事实。
//
// 记法：只记「今天成了」，**失败一律不记**——留着才算待办，重试才有依据
// （与 extstore.markCheckedIn 同一条铁律：失败记了等于把失败当成功吞掉）。
package scheduler

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dayStateFileName 由 Config.StateFile 的兄弟路径推导（与 model.json 同风格）。
const dayStateFile = "task-state.json"

// dayRecord 落盘结构。Day 是记录所属自然日（CST）：与当前日不符即整表作废，
// 相当于按日轮换，不需要额外的清理任务。
type dayRecord struct {
	Day string `json:"day"`
	// Done key(task|uid) → 完成时刻（Unix 秒）。只为对账用，判定只看存在性。
	Done map[string]int64 `json:"done"`
}

// dayState 日级完成标记（进程内缓存 + 每次变更原子落盘）。
type dayState struct {
	path string

	mu   sync.Mutex
	day  string
	done map[string]int64
	// dirty 落盘失败过：下次变更时重试（不因一次写失败就丢掉整表语义）。
	dirty bool
}

func newDayKey(task, uid string) string { return task + "|" + uid }

// newDayState 建表并读入当日记录。path 为空 → 纯内存（老测试/无数据目录时
// 行为与引入前一致：重启即清零）。
func newDayState(path string) *dayState {
	d := &dayState{path: path, done: map[string]int64{}}
	if path == "" {
		return d
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return d // 首次运行没有文件，正常
	}
	var rec dayRecord
	if json.Unmarshal(raw, &rec) != nil || rec.Day != travelDay(time.Now()) {
		return d // 换日/损坏：从空表开始，不去猜
	}
	d.day = rec.Day
	if rec.Done != nil {
		d.done = rec.Done
	}
	return d
}

// rollLocked 跨日清零（调用方须持 mu）。
func (d *dayState) rollLocked() {
	today := travelDay(time.Now())
	if d.day != today {
		d.day = today
		d.done = map[string]int64{}
	}
}

// Done 该任务今天是否已对该账号跑成。
func (d *dayState) Done(task, uid string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollLocked()
	_, ok := d.done[newDayKey(task, uid)]
	return ok
}

// MarkDone 记「今天跑成」并落盘。
func (d *dayState) MarkDone(task, uid string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.rollLocked()
	key := newDayKey(task, uid)
	if _, ok := d.done[key]; ok {
		d.mu.Unlock()
		return // 已是今日状态，不必再写盘
	}
	d.done[key] = time.Now().Unix()
	d.mu.Unlock()
	d.save()
}

// saveLocked 之外独立加锁写盘：调用方不得持 mu（写盘耗时不该挡住判定读）。
func (d *dayState) save() {
	d.mu.Lock()
	rec := dayRecord{Day: d.day, Done: make(map[string]int64, len(d.done))}
	for k, v := range d.done {
		rec.Done[k] = v
	}
	d.mu.Unlock()

	if d.path == "" {
		return
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWrite(d.path, raw); err != nil {
		log.Printf("scheduler: 任务日状态写盘失败 %s: %v", d.path, err)
		d.mu.Lock()
		d.dirty = true
		d.mu.Unlock()
	}
}

// PendingCount 当日「已成」条目数（面板汇总用）。
func (d *dayState) PendingCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollLocked()
	return len(d.done)
}

// atomicWrite 临时文件 + rename：进程被砍在写中间也不会留下半截 JSON。
func atomicWrite(path string, raw []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// Windows：rename 不覆盖已存在目标，先删再改名。
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			_ = os.Remove(tmp)
			return err2
		}
	}
	return nil
}
