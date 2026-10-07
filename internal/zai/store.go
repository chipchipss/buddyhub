package zai

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store 账号池持久化（JSON 单文件 + 原子替换）。
//
// 与 ext-accounts.json / state.json 同目录，随 data 卷一起备份；文件权限 0600
// （内含 JWT / API Key 明文，与 auths/ 下的凭证同级敏感）。
type Store struct {
	mu       sync.RWMutex
	path     string
	accounts map[string]*Account
	order    []string // 保持插入顺序，选号按此轮转
}

// NewStore 打开（或初始化）账号池文件。文件不存在时返回空池。
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, accounts: map[string]*Account{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var file struct {
		Accounts []*Account `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	for _, a := range file.Accounts {
		if a == nil || a.ID == "" {
			continue
		}
		a.EnsureProfile()
		if a.Status == "" {
			a.Status = StatusActive
		}
		s.accounts[a.ID] = a
		s.order = append(s.order, a.ID)
	}
	return s, nil
}

// Path 返回持久化文件路径。
func (s *Store) Path() string { return s.path }

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	list := make([]*Account, 0, len(s.order))
	for _, id := range s.order {
		if a := s.accounts[id]; a != nil {
			list = append(list, a)
		}
	}
	buf, err := json.MarshalIndent(struct {
		Accounts []*Account `json:"accounts"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Save 落盘（调用方通常不必直接调，增删改会自动落盘）。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// Add 新增账号（同名视为替换凭证并复位状态）。
func (s *Store) Add(a *Account) error {
	if a == nil {
		return errors.New("账号为空")
	}
	a.EnsureProfile()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.accounts[a.ID]; !exists {
		s.order = append(s.order, a.ID)
	}
	s.accounts[a.ID] = a
	return s.saveLocked()
}

// Remove 删除账号。
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return errors.New("账号不存在")
	}
	delete(s.accounts, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return s.saveLocked()
}

// Get 取账号（返回副本，避免调用方无锁改状态）。
func (s *Store) Get(id string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAccount(s.accounts[id])
}

// List 按插入顺序返回副本。
func (s *Store) List() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.order))
	for _, id := range s.order {
		if a := cloneAccount(s.accounts[id]); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// Update 在锁内修改账号并落盘（状态机迁移统一走这里，避免丢更新）。
func (s *Store) Update(id string, fn func(*Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[id]
	if a == nil {
		return errors.New("账号不存在")
	}
	fn(a)
	return s.saveLocked()
}

// SetEnabled 人工启用/停用。
func (s *Store) SetEnabled(id string, enabled bool) error {
	return s.Update(id, func(a *Account) {
		a.Enabled = enabled
		// 人工确认恢复：风控封禁(DISABLED)只有人能解除；凭证失效(INVALID)也给人工
		// 复核入口——失效常由上游 WAF 抖动 / billing 误判造成，若凭证真死，下一次
		// 对话会重新判失效（自愈），故人工放行是安全且必要的恢复通道。
		// 冷却(COOLING)/额度用完(EXHAUSTED)属时间/额度驱动，不归人工 toggle 强解。
		if enabled && (a.Status == StatusDisabled || a.Status == StatusInvalid) {
			a.Status = StatusActive
			a.LastError = ""
		}
	})
}

// RotateFingerprint 换发设备档案（换一整台 SKU + 新 MID）。
func (s *Store) RotateFingerprint(id string) error {
	return s.Update(id, func(a *Account) { a.Fingerprint = newProfile() })
}

// Reap 到期状态回迁：冷却/额度再探时间已过 → 回 ACTIVE 待选。
// 返回发生迁移的账号数（调用方可据此落盘一次）。
func (s *Store) Reap(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.accounts {
		if (a.Status == StatusCooling || a.Status == StatusExhausted) && !a.CoolingUntil.IsZero() && now.After(a.CoolingUntil) {
			a.Status = StatusActive
			a.CoolingUntil = time.Time{}
			n++
		}
		if a.ReapModelHealth(now) {
			n++
		}
	}
	if n > 0 {
		_ = s.saveLocked()
	}
	return n
}

// Stats 概览计数（面板状态条用）。
func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{"total": len(s.accounts)}
	for _, a := range s.accounts {
		out[string(a.Status)]++
	}
	return out
}

// SortedNames 调试/展示用。
func (s *Store) SortedNames() []string {
	list := s.List()
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}

// cloneAccount 深拷贝（指针字段逐层复制，避免数据竞争）。
func cloneAccount(a *Account) *Account {
	if a == nil {
		return nil
	}
	c := *a
	if a.Quota != nil {
		c.Quota = make(map[string]QuotaEntry, len(a.Quota))
		for k, v := range a.Quota {
			c.Quota[k] = v
		}
	}
	if a.ModelHealth != nil {
		c.ModelHealth = make(map[string]ModelPenalty, len(a.ModelHealth))
		for k, v := range a.ModelHealth {
			c.ModelHealth[k] = v
		}
	}
	if a.Fingerprint != nil {
		fp := *a.Fingerprint
		c.Fingerprint = &fp
	}
	return &c
}
