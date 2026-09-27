// Package extstore 外部积分账号统一存储与签到调度。
//
// 管理腾讯账号池之外的平台账号（lobsterai / raccoon / qoder / codearts）：
// 凭据持久化到 data/ext-accounts.json，CheckinAll/StatusAll 遍历全部账号执行
// 平台各自的签到/余额逻辑。结果统一为 ExtCheckinResult / ExtAccountView。
package extstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/extprovider/codearts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/extprovider/lobsterai"
	"github.com/linguo2625469/workbuddy2api-panel/internal/extprovider/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/extprovider/raccoon"
)

// Provider 平台标识。
const (
	PLogsterAI = "lobsterai"
	PRaccoon   = "raccoon"
	PQoder     = "qoder"
	PCodeArts  = "codearts"
)

// ExtAccount 一个外部平台账号。
type ExtAccount struct {
	Provider string          `json:"provider"` // lobsterai | raccoon | qoder | codearts
	ID       string          `json:"id"`       // 账号标识（uid / user_id / 昵称派生）
	Label    string          `json:"label"`    // 展示名
	AddedAt  string          `json:"added_at"`
	Disabled bool            `json:"disabled,omitempty"`
	Cred     json.RawMessage `json:"cred"` // 平台各自凭据 JSON
}

// store 持久化文件结构。
type store struct {
	Accounts []*ExtAccount `json:"accounts"`
}

// Manager 外部账号管理器。
type Manager struct {
	mu       sync.Mutex
	path     string
	accounts []*ExtAccount
}

// NewManager 创建管理器并加载 data/ext-accounts.json（不存在则空表）。
func NewManager(dataDir string) *Manager {
	m := &Manager{path: filepath.Join(dataDir, "ext-accounts.json")}
	if raw, err := os.ReadFile(m.path); err == nil {
		var s store
		if json.Unmarshal(raw, &s) == nil {
			m.accounts = s.Accounts
		}
	}
	return m
}

func (m *Manager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(store{Accounts: m.accounts}, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// List 返回全部账号快照（按 provider + label 排序）。
func (m *Manager) List() []*ExtAccount {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ExtAccount, len(m.accounts))
	copy(out, m.accounts)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// Add 添加账号（同 provider + 同 ID 幂等覆盖）。
func (m *Manager) Add(provider, id, label string, cred json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Provider == provider && a.ID == id {
			a.Cred = cred
			a.Label = label
			if err := m.saveLocked(); err != nil {
				return err
			}
			return nil
		}
	}
	m.accounts = append(m.accounts, &ExtAccount{
		Provider: provider,
		ID:       id,
		Label:    label,
		AddedAt:  time.Now().Format(time.RFC3339),
		Cred:     cred,
	})
	return m.saveLocked()
}

// Remove 删除账号。
func (m *Manager) Remove(provider, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.accounts[:0]
	found := false
	for _, a := range m.accounts {
		if a.Provider == provider && a.ID == id {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		return fmt.Errorf("账号不存在")
	}
	m.accounts = out
	return m.saveLocked()
}

// Find 按 provider + id 取单个账号；不存在返回 nil。
func (m *Manager) Find(provider, id string) *ExtAccount {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Provider == provider && a.ID == id {
			out := *a
			return &out
		}
	}
	return nil
}

// SetDisabled 启用/停用。
func (m *Manager) SetDisabled(provider, id string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Provider == provider && a.ID == id {
			a.Disabled = disabled
			return m.saveLocked()
		}
	}
	return fmt.Errorf("账号不存在")
}

// replaceCred 更新凭据（refresh 轮换后回写）。
func (m *Manager) replaceCred(provider, id string, cred json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Provider == provider && a.ID == id {
			a.Cred = cred
			_ = m.saveLocked()
			return
		}
	}
}

// ExtAccountView 面板展示视图。
type ExtAccountView struct {
	Provider  string  `json:"provider"`
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Disabled  bool    `json:"disabled"`
	Balance   float64 `json:"balance,omitempty"`
	BalanceOK bool    `json:"balance_ok"`
	Note      string  `json:"note,omitempty"`
}

// CheckinResult 统一签到结果。
type CheckinResult struct {
	Provider string  `json:"provider"`
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Kind     string  `json:"kind"` // claimed | already-claimed | inactive | failed
	Credit   float64 `json:"credit"`
	Message  string  `json:"message"`
}

// refreshIfNeeded 到期平台自动续期并回写凭据（当前仅 raccoon；qoder/lobsterai
// 续期协议变数大，失败时结果里带提示让用户重新登录）。
func (m *Manager) refreshIfNeeded(ctx context.Context, a *ExtAccount) {
	if a.Provider != PRaccoon {
		return
	}
	var cred raccoon.Credential
	if json.Unmarshal(a.Cred, &cred) != nil {
		return
	}
	if !cred.IsExpired() {
		return
	}
	cli := raccoon.New()
	fresh, err := cli.Refresh(ctx, &cred)
	if err != nil {
		log.Printf("extstore: raccoon %s 续期失败: %v", a.ID, err)
		return
	}
	raw, err := json.Marshal(fresh)
	if err != nil {
		return
	}
	m.replaceCred(a.Provider, a.ID, raw)
	a.Cred = raw
}

// CheckinOne 对单个账号执行平台签到/领取。
func (m *Manager) CheckinOne(ctx context.Context, a *ExtAccount) *CheckinResult {
	res := &CheckinResult{Provider: a.Provider, ID: a.ID, Label: a.Label}
	if a.Disabled {
		res.Kind = "inactive"
		res.Message = "已停用"
		return res
	}
	m.refreshIfNeeded(ctx, a)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	switch a.Provider {
	case PLogsterAI:
		var cred lobsterai.Credential
		if err := json.Unmarshal(a.Cred, &cred); err != nil {
			res.Kind, res.Message = "failed", "凭据解析失败"
			return res
		}
		r := lobsterai.New().CheckinDaily(ctx, &cred)
		res.Kind, res.Credit, res.Message = r.Kind, r.Credit, r.Message
	case PRaccoon:
		var cred raccoon.Credential
		if err := json.Unmarshal(a.Cred, &cred); err != nil {
			res.Kind, res.Message = "failed", "凭据解析失败"
			return res
		}
		r := raccoon.New().ClaimLoginReward(ctx, &cred)
		res.Kind = r.Kind
		res.Credit = float64(r.Credit)
		res.Message = r.Message
	case PQoder:
		var cred qoder.Credential
		if err := json.Unmarshal(a.Cred, &cred); err != nil {
			res.Kind, res.Message = "failed", "凭据解析失败"
			return res
		}
		r, err := qoder.New().ClaimDaily(ctx, &cred)
		if err != nil {
			res.Kind, res.Message = "failed", err.Error()
			return res
		}
		res.Kind, res.Credit, res.Message = r.Kind, float64(r.Credit), r.Message
	case PCodeArts:
		var cred codearts.Credential
		if err := json.Unmarshal(a.Cred, &cred); err != nil {
			res.Kind, res.Message = "failed", "凭据解析失败"
			return res
		}
		r := codearts.New().CheckinDaily(ctx, &cred)
		res.Kind, res.Credit, res.Message = r.Kind, r.Credit, r.Message
	default:
		res.Kind, res.Message = "failed", "未知平台: "+a.Provider
	}
	return res
}

// CheckinAll 遍历全部账号签到（账号间串行：外部接口限速优先于并发）。
func (m *Manager) CheckinAll(ctx context.Context) []*CheckinResult {
	var out []*CheckinResult
	for _, a := range m.List() {
		out = append(out, m.CheckinOne(ctx, a))
	}
	return out
}

// ViewOne 单账号余额视图。
func (m *Manager) ViewOne(ctx context.Context, a *ExtAccount) *ExtAccountView {
	v := &ExtAccountView{Provider: a.Provider, ID: a.ID, Label: a.Label, Disabled: a.Disabled}
	if a.Disabled {
		v.Note = "已停用"
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch a.Provider {
	case PLogsterAI:
		var cred lobsterai.Credential
		if json.Unmarshal(a.Cred, &cred) == nil {
			if bal, err := lobsterai.New().FetchBalance(ctx, &cred); err == nil {
				v.Balance, v.BalanceOK = bal.Total, true
			} else {
				v.Note = err.Error()
			}
		}
	case PRaccoon:
		var cred raccoon.Credential
		if json.Unmarshal(a.Cred, &cred) == nil {
			if bal, err := raccoon.New().FetchBalance(ctx, &cred); err == nil {
				v.Balance, v.BalanceOK = bal.Total, true
			} else {
				v.Note = err.Error()
			}
		}
	case PQoder:
		// Qoder 无独立余额端点（用量在 /sash/api/v2/me/usage，形状复杂），先只展示签到状态。
		var cred qoder.Credential
		if json.Unmarshal(a.Cred, &cred) == nil {
			if st, err := qoder.New().GetCheckinStatus(ctx, &cred); err == nil {
				v.BalanceOK = true
				if st.TodayCheckedIn {
					v.Note = "今日已领"
				} else if st.ClaimableID != "" {
					v.Note = fmt.Sprintf("可领 %d", st.DailyCredit)
				} else {
					v.Note = "暂无可领活动"
				}
			} else {
				v.Note = err.Error()
			}
		}
	case PCodeArts:
		var cred codearts.Credential
		if json.Unmarshal(a.Cred, &cred) == nil {
			if info, err := codearts.New().FetchAccountInfo(ctx, &cred); err == nil {
				v.Balance, v.BalanceOK = info.TotalCredits, true
				if !info.IsCreditPackage {
					v.Note = "非积分账户"
				}
			} else {
				v.Note = err.Error()
			}
		}
	}
	return v
}

// StatusAll 全部账号视图。
func (m *Manager) StatusAll(ctx context.Context) []*ExtAccountView {
	var out []*ExtAccountView
	for _, a := range m.List() {
		out = append(out, m.ViewOne(ctx, a))
	}
	return out
}
