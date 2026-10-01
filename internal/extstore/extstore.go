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

	"github.com/chipchipss/buddyhub/internal/extprovider/accio"
	"github.com/chipchipss/buddyhub/internal/extprovider/autoclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/codearts"
	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extprovider/lobsterai"
	"github.com/chipchipss/buddyhub/internal/extprovider/qclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extprovider/trae"
	"github.com/chipchipss/buddyhub/internal/extprovider/traework"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// Provider 平台标识。
const (
	PLogsterAI = "lobsterai"
	PRaccoon   = "raccoon"
	PQoder     = "qoder"
	PCodeArts  = "codearts"
	// PLoomyCLI 密码/短信登录落库的 Loomy 账号（区别于本机客户端检测的
	// "loomy" session 来源；凭据含可选加密密码，支持无人值守续期）。
	PLoomyCLI = "loomy-cli"
	// PCopilot GitHub Copilot（设备流登录；凭据为 GitHub token + 短期 Copilot token）。
	PCopilot = "copilot"
	// PCline Cline（cline.bot，WorkOS 设备授权；免费池无需订阅）。
	PCline = "cline"
	// PAutoClaw AutoClaw（智谱 autoglm；国内版支持手机短信登录）。
	PAutoClaw = "autoclaw"
	// PQClaw QClaw（腾讯；微信扫码登录）。
	PQClaw = "qclaw"
	// PTrae Trae（字节 SOLO；本机回调授权）。
	PTrae = "trae"
	// PAccio Accio（阿里；本机回调授权）。
	PAccio = "accio"
	// PTraeWork TraeWork（字节 TRAE SOLO CN；粘贴客户端凭据入池）。
	PTraeWork = "traework"
)

// ExtAccount 一个外部平台账号。
type ExtAccount struct {
	Provider string `json:"provider"` // lobsterai | raccoon | qoder | codearts
	ID       string `json:"id"`       // 账号标识（uid / user_id / 昵称派生）
	Label    string `json:"label"`    // 展示名
	AddedAt  string `json:"added_at"`
	Disabled bool   `json:"disabled,omitempty"`
	// LastCheckin 最后一次**执行过签到动作**的日期（YYYY-MM-DD）。
	// 任务中心的「今日待办」靠它判——扫描是只读操作，不能再打一次上游状态
	// （那等于每次扫描都对每个平台发请求）。`claimed` 与 `already-claimed`
	// 都算已跑过；`failed` 不记，留在待办里重试。
	LastCheckin string          `json:"last_checkin,omitempty"`
	Cred        json.RawMessage `json:"cred"` // 平台各自凭据 JSON
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

	// selOnce / sel 选择与健康的运行时状态（select.go）。惰性初始化：
	// 不是所有 Manager 用例都要它（纯持久化测试用不到），而字段为 nil 时
	// selState 会 panic——用 Once 一次建好，省得每个入口都判空。
	selOnce sync.Once
	sel     *selectState
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
// markCheckedIn 记下「今天跑过了」，任务中心据此把它从待办里摘掉。
//
// 失败**不记**：留着待办才有重试机会，记了等于把失败当成功吞掉。
func (m *Manager) markCheckedIn(provider, id string) {
	today := time.Now().Format("2006-01-02")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Provider == provider && a.ID == id {
			if a.LastCheckin == today {
				return
			}
			a.LastCheckin = today
			_ = m.saveLocked()
			return
		}
	}
}

// ExtPendingOne 判断该账号**今天是否还有待办**（只读，不打上游）。
//
// 扫描若每次都要去问上游，就是「每点一次扫描 → 对每个平台发一次请求」，
// 而签到本身是幂等的、本地就能判——所以靠 LastCheckin 判。
// 判据：停用 / 平台无签到能力 / 今天跑过 → 不是待办；其余 → 是待办。
func (m *Manager) ExtPendingOne(a *ExtAccount) (bool, string) {
	if a == nil || a.Disabled {
		return false, ""
	}
	if !checkinCapable[a.Provider] {
		return false, ""
	}
	if a.LastCheckin == time.Now().Format("2006-01-02") {
		return false, ""
	}
	return true, "今日待签到"
}

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
// checkinCapable 该平台是否有每日签到能力。
//
// 与面板注册表的 Checkin 字段是**同一份事实的两个投影**（那份驱动按钮、
// 这份驱动待办判定）。extstore 不 import panel（会成环），只能各自声明——
// platforms_test.go 的注册表一致性测试兜住它们漂移。
// CheckinCapable 导出的签到能力查询——面板包拿它与注册表的 Checkin 字段
// 做一致性断言（注册表驱动按钮，这张表驱动待办判定，漂移了就会出现
// 「待办里有却按不了」或反过来的不一致）。
func CheckinCapable(provider string) bool { return checkinCapable[provider] }

// CheckinCapableProviders 枚举有签到能力的平台（给跨包一致性断言用）。
func CheckinCapableProviders() []string {
	out := make([]string, 0, len(checkinCapable))
	for id := range checkinCapable {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

var checkinCapable = map[string]bool{
	PLogsterAI: true,
	PRaccoon:   true,
	PQoder:     true,
	PCodeArts:  true,
	PLoomyCLI:  true,
	PTraeWork:  true,
	PAutoClaw:  true,
}

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
		// 双通道：campaigns（/sash/，Jet-Hub 路线）优先；无可领项时回退
		// activity claim（openapi/v2，qoder-workflow 路线，COSY MD5 签名）。
		r, err := qoder.New().ClaimDaily(ctx, &cred)
		if err != nil || r.Kind == "failed" || (r.Kind == "already-claimed" && res.Message == "") {
			ca, cerr := qoder.New().ClaimActivities(ctx, &cred)
			if cerr == nil {
				if len(ca.Claimed) > 0 {
					res.Kind, res.Message = "claimed", fmt.Sprintf("活动领取成功 %d 项", len(ca.Claimed))
					return res
				}
				if ca.Note != "" && (err != nil || r == nil || r.Kind == "failed") {
					res.Kind, res.Message = "inactive", ca.Note
					return res
				}
			}
		}
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
	case PCopilot:
		// Copilot 不是积分平台，没有每日签到；凭据有效性由余额视图（ViewOne）验证。
		res.Kind, res.Message = "inactive", "Copilot 无需签到（网关直连通道）"
	case PCline:
		// Cline 同样没有每日签到；凭据有效性由 ViewOne 续期验证。
		res.Kind, res.Message = "inactive", "Cline 无需签到（网关直连通道）"
	case PAutoClaw:
		// 每日签到走 userapi 域的「完成客户端任务」通用入口
		// （`autoclaw-task-complete`，task_id=daily_signin）。判据不是 HTTP
		// 状态码——上游恒回 200，要按 reward_points / already_completed 判，
		// 见 autoclaw.CheckinDaily。
		var cred autoclaw.Credential
		if err := json.Unmarshal(a.Cred, &cred); err != nil {
			res.Kind, res.Message = "failed", "凭据解析失败"
			return res
		}
		r := autoclaw.CheckinDaily(ctx, &cred)
		res.Kind, res.Credit, res.Message = r.Kind, r.Credit, r.Message
	case PQClaw:
		res.Kind, res.Message = "inactive", "QClaw 无需签到（网关直连通道）"
	case PTrae:
		res.Kind, res.Message = "inactive", "Trae 无需签到（网关直连通道）"
	case PAccio:
		res.Kind, res.Message = "inactive", "Accio 无需签到（网关直连通道）"
	case PLoomyCLI:
		// Loomy 的签到是「激活每日赠送额度」，走 upstream 的 Loomy 客户端
		// （凭据里存的是登录 session，不是 extprovider 那套密码登录接口）。
		var cred struct {
			Session string `json:"session"`
		}
		if err := json.Unmarshal(a.Cred, &cred); err != nil || cred.Session == "" {
			res.Kind, res.Message = "failed", "凭据里没有 session，需重新登录"
			return res
		}
		r, lerr := upstream.NewLoomyClient("").CheckinDailyQuota(cred.Session)
		if lerr != nil {
			res.Kind, res.Message = "failed", lerr.Error()
			return res
		}
		if r.AlreadyProcessed {
			res.Kind, res.Message = "already-claimed", r.Message
		} else {
			res.Kind, res.Credit = "claimed", float64(r.Granted)
			res.Message = r.Message
		}
	case PTraeWork:
		// TraeWork 有每日签到积分（UG 域）。
		var cred traework.Credential
		if err := json.Unmarshal(a.Cred, &cred); err != nil {
			res.Kind, res.Message = "failed", "凭据解析失败"
			return res
		}
		r := traework.CheckinDaily(ctx, &cred)
		res.Kind, res.Credit, res.Message = r.Kind, r.Credit, r.Message
	default:
		res.Kind, res.Message = "failed", "未知平台: "+a.Provider
	}
	// 只有真跑过（成功 / 今天已领）才记日期——失败留着待办才有重试机会。
	// 这条是任务中心「今日待办」的反向依赖：不记，扫描就会把已完成的一直列出来。
	if res.Kind == "claimed" || res.Kind == "already-claimed" {
		m.markCheckedIn(a.Provider, a.ID)
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
	case PTraeWork:
		// TraeWork 视图：能拉到模型目录即视为可用。
		var cred traework.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if _, err := traework.ListModels(ctx, &cred); err == nil {
			v.BalanceOK = true
			v.Note = "TraeWork"
		} else {
			v.Note = err.Error()
		}
	case PAccio:
		// Accio 视图：能查到额度即视为可用。
		var cred accio.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if q, err := accio.FetchQuota(ctx, &cred); err == nil {
			v.Balance, v.BalanceOK = q.Remaining, true
			v.Note = "Accio " + accio.ParseRegion(string(cred.Region)).Label()
			if q.PlanName != "" {
				v.Note = "Accio " + q.PlanName
			}
		} else {
			v.Note = err.Error()
		}
	case PTrae:
		// Trae 视图：能拉到模型目录即视为可用。
		var cred trae.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if _, err := trae.ListModels(ctx, &cred); err == nil {
			v.BalanceOK = true
			v.Note = "Trae SOLO"
		} else {
			v.Note = err.Error()
		}
	case PQClaw:
		// QClaw 视图：对话用的 sk key 是长期凭据，能列模型即视为可用。
		var cred qclaw.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if cred.APIKey == "" {
			v.Note = "缺少对话用的 sk key，请重新扫码登录"
			return v
		}
		v.BalanceOK = true
		v.Note = "QClaw"
		if cred.Nickname != "" {
			v.Note = "QClaw " + cred.Nickname
		}
	case PAutoClaw:
		// AutoClaw 视图：续期一次验证凭据有效性（服务端会轮换 refresh_token）。
		var cred autoclaw.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if cred.NeedsRefresh() && cred.CanRefresh() {
			if fresh, err := autoclaw.Refresh(ctx, &cred); err == nil {
				if raw, merr := json.Marshal(fresh); merr == nil {
					m.replaceCred(a.Provider, a.ID, raw)
				}
				cred = *fresh
			} else {
				v.Note = err.Error()
				return v
			}
		}
		v.BalanceOK = cred.Token != ""
		v.Note = "AutoClaw " + autoclaw.ParseRegion(string(cred.Region)).Label()
		if cred.PhoneTail != "" {
			v.Note += " · " + cred.PhoneTail
		}
	case PCline:
		// Cline 视图：续期一次验证凭据有效性，并展示 credit 余额。
		var cred cline.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if cred.NeedsRefresh() {
			if fresh, err := cline.Refresh(ctx, &cred); err == nil {
				if raw, merr := json.Marshal(fresh); merr == nil {
					m.replaceCred(a.Provider, a.ID, raw)
				}
				cred = *fresh
			} else {
				v.Note = err.Error()
				return v
			}
		}
		if bal, err := cline.FetchBalance(ctx, &cred); err == nil {
			v.Balance, v.BalanceOK = bal.Credits, true
			v.Note = "Cline credit"
			if bal.PlanName != "" {
				v.Note = "Cline " + bal.PlanName
			}
		} else {
			v.Note = err.Error()
		}
	case PCopilot:
		// Copilot 没有积分余额；视图展示订阅类型 + token 是否仍可续期，
		// 顺带把续期后的短期 token 写回（等价于一次健康检查）。
		var cred copilot.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			v.Note = "凭据解析失败"
			return v
		}
		if fresh, err := copilot.Refresh(ctx, &cred); err == nil {
			v.BalanceOK = true
			if raw, merr := json.Marshal(fresh); merr == nil {
				m.replaceCred(a.Provider, a.ID, raw)
			}
			plan := fresh.Plan
			if plan == "" {
				plan = "已订阅"
			}
			v.Note = "Copilot " + plan + " · " + copilot.FormatExpiry(fresh.ExpiresAt)
		} else {
			v.Note = err.Error()
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

// ExtList 导出 List（server 包 Qoder 桥接读取账号）。
// ExtList 桥接专用的账号列表：**按健康与轮转序排好**返回。
//
// 只有 chat 桥接用它（面板内部走 List()，展示顺序不该被路由策略左右）。
// 桥接的循环是「成功即停」，所以顺序直接决定负载落点——不排的话所有
// 请求都打在按 provider+label 排序后的第一个号上。
func (m *Manager) ExtList() []*ExtAccount { return m.SelectChatOrder(m.List()) }

// ReplaceCred 导出 replaceCred（server 包 Qoder 桥接刷新后回写凭据）。
func (m *Manager) ReplaceCred(provider, id string, cred json.RawMessage) {
	m.replaceCred(provider, id, cred)
}
