package zai

import (
	"errors"
	"sync"
	"time"
)

// ErrNoAccount 池内没有可用账号（上游应回 503 no_available_account）。
var ErrNoAccount = errors.New("没有可用的 Z.AI 账号")

// Pool 账号选号 + 单账号并发控制。
//
// 选号语义（对齐 zcode2api 的实测结论）：
//   - 轮转（round-robin）而非随机：让每个账号的使用次数尽量均匀，避免某个号
//     被集中打到额度耗尽；
//   - 满并发的账号**智能跳过**——不排队、不计入重试次数，直接试下一个；
//   - 冷却/额度用完/失效/禁用一律不选；冷却到期由 Store.Reap 回迁。
type Pool struct {
	store *Store

	mu       sync.Mutex
	inflight map[string]int
	cursor   int
	maxPer   int // 单账号并发上限；0 = 不限
}

// NewPool 建池。maxPerAccount 为单账号并发上限（缺省 2，0 = 不限）。
func NewPool(store *Store, maxPerAccount int) *Pool {
	if maxPerAccount < 0 {
		maxPerAccount = 0
	}
	return &Pool{store: store, inflight: map[string]int{}, maxPer: maxPerAccount}
}

// SetMaxPerAccount 运行时调整单账号并发上限（面板热改）。
func (p *Pool) SetMaxPerAccount(n int) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	p.maxPer = n
	p.mu.Unlock()
}

// MaxPerAccount 当前并发上限。
func (p *Pool) MaxPerAccount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxPer
}

// Pick 选一个可用账号并**占用**其并发槽。调用方必须在使用后 Release。
//
// wantPlan=true 时优先返回能走 Plan（JWT）通道的账号（例如已有验证码 token
// 在手、想优先消耗订阅额度）；这类账号不可用时退回任意可用账号。
func (p *Pool) Pick(wantPlan bool) (*Account, error) {
	p.store.Reap(time.Now())

	list := p.store.List()
	if len(list) == 0 {
		return nil, ErrNoAccount
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(list)
	start := p.cursor % n
	var fallback *Account

	for i := 0; i < n; i++ {
		a := list[(start+i)%n]
		if !a.Selectable(time.Now()) {
			continue
		}
		if p.maxPer > 0 && p.inflight[a.ID] >= p.maxPer {
			continue // 满并发：跳过，不排队
		}
		if wantPlan && a.HasJWTPath() {
			p.cursor = (start + i + 1) % n
			p.inflight[a.ID]++
			return a, nil
		}
		if fallback == nil {
			fallback = a
		}
	}
	if fallback != nil {
		p.cursor = (p.cursor + 1) % n
		p.inflight[fallback.ID]++
		return fallback, nil
	}
	return nil, ErrNoAccount
}

// Release 释放并发槽（幂等：槽位为 0 时不动）。
func (p *Pool) Release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight[id] > 0 {
		p.inflight[id]--
	}
}

// InFlight 单账号在途数（面板展示）。
func (p *Pool) InFlight(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight[id]
}

// Snapshot 池内各账号的即时视图（面板用）。
type Snapshot struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Mode     string        `json:"mode"`
	Status   Status        `json:"status"`
	Enabled  bool          `json:"enabled"`
	InFlight int           `json:"in_flight"`
	UseCount int64         `json:"use_count"`
	FailCount int64        `json:"fail_count"`
	Risk     int           `json:"risk_strikes"`
	CoolLeft int64         `json:"cool_remaining_sec,omitempty"`
	Quota    map[string]QuotaEntry `json:"quota,omitempty"`
	PlanName string        `json:"plan_name,omitempty"`
	LastErr  string        `json:"last_error,omitempty"`
	LastOK   time.Time     `json:"last_ok,omitempty"`
	Masked   string        `json:"masked"`
	HasKey   bool          `json:"has_key_fallback"`
	Profile  *Profile      `json:"fingerprint,omitempty"`
}

// Snapshots 返回全部账号的即时视图（按插入顺序）。
func (p *Pool) Snapshots() []Snapshot {
	list := p.store.List()
	p.mu.Lock()
	inflight := make(map[string]int, len(p.inflight))
	for k, v := range p.inflight {
		inflight[k] = v
	}
	p.mu.Unlock()

	now := time.Now()
	out := make([]Snapshot, 0, len(list))
	for _, a := range list {
		s := Snapshot{
			ID: a.ID, Name: a.Name, Mode: a.Mode, Status: a.Status, Enabled: a.Enabled,
			InFlight: inflight[a.ID], UseCount: a.UseCount, FailCount: a.FailCount,
			Risk: a.RiskStrikes, Quota: a.Quota, PlanName: a.PlanName,
			LastErr: a.LastError, LastOK: a.LastOKAt,
			Masked: Mask(a.Secret()), HasKey: a.HasKeyFallback(), Profile: a.Fingerprint,
		}
		if !a.CoolingUntil.IsZero() && a.CoolingUntil.After(now) {
			s.CoolLeft = int64(a.CoolingUntil.Sub(now).Seconds())
		}
		out = append(out, s)
	}
	return out
}

// Store 暴露底层存储（OAuth 登录、面板增删改走它）。
func (p *Pool) Store() *Store { return p.store }

// Count 池内账号总数（面板与模型列表的可用性判定）。
func (p *Pool) Count() int {
	if p.store == nil {
		return 0
	}
	return len(p.store.List())
}

// HasSelectable 是否存在当前可选中的账号（zai: 通道对外可用性）。
func (p *Pool) HasSelectable() bool {
	if p.store == nil {
		return false
	}
	now := time.Now()
	for _, a := range p.store.List() {
		if a.Selectable(now) {
			return true
		}
	}
	return false
}
