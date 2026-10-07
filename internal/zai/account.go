// Package zai Z.AI / ZCode 账号运营层。
//
// 协议事实来源：dengyie/zcode2api（AGPL-3.0，仅作为**协议参考**阅读，
// 未复制其代码）+ 上游实测。本包为独立 Go 实现，MIT 许可不受影响。
//
// 两条通道（zcode2api 的划分，本项目沿用）：
//
//	Plan 通道（JWT）  https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages
//	                  Bearer <三段 JWT> + 阿里云无痕验证头 + 全套桌面身份头
//	                  额度来自 Coding Plan 订阅；需验证码，风控最严
//	回退通道（API Key） https://api.z.ai/api/anthropic/v1/messages
//	                  x-api-key 鉴权，免验证码；JWT 额度耗尽/限流时自动回落
//
// 账号状态机（对齐 zcode2api 的语义，判定信号见 classify.go）：
//
//	ACTIVE ──额度用完(402/quota)──▶ EXHAUSTED（定期再探，恢复即回 ACTIVE）
//	  │  ──5xx 重试耗尽──────────▶ COOLING（冷却 N 秒）
//	  │  ──401/403(非验证码)─────▶ INVALID（凭证失效，需重新登录）
//	  └──3012/405 真风控─────────▶ DISABLED（保护资产，须人工确认恢复）
//
// 429 是**不冷却**的：按 Retry-After 原地等待重试，耗尽后换号，账号保持可用。
package zai

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Status 账号运行态。
type Status string

const (
	StatusActive    Status = "active"    // 可用
	StatusExhausted Status = "exhausted" // 额度用完（定期再探）
	StatusCooling   Status = "cooling"   // 上游 5xx 重试耗尽后的冷却
	StatusInvalid   Status = "invalid"   // 凭证失效，需重新登录
	StatusDisabled  Status = "disabled"  // 真风控命中或人工停用
)

// Mode 凭证形态。
const (
	ModeJWT    = "jwt"    // Coding Plan JWT（Plan 通道，需验证码）
	ModeAPIKey = "apikey" // API Key（回退通道，免验证码）
)

// Provider 上游提供方。
const (
	ProviderZai      = "zai"      // Z.AI（Coding Plan JWT 或 API Key）
	ProviderBigModel = "bigmodel" // 智谱开放平台（Anthropic 兼容同形端点，仅 API Key）
)

// Account Z.AI 账号（凭证 + 运行态 + 额度快照 + 设备指纹）。
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"` // zai / bigmodel
	Mode     string `json:"mode"`
	JWT      string `json:"jwt,omitempty"`     // Mode=jwt：三段点分
	APIKey   string `json:"api_key,omitempty"` // Mode=apikey：主键；jwt 账号亦可持有（回退用）
	Enabled  bool   `json:"enabled"`

	Status       Status    `json:"status"`
	CoolingUntil time.Time `json:"cooling_until,omitempty"`
	LastError    string    `json:"last_error,omitempty"`

	// 额度快照：模型显示名 → 剩余/总量（quota.go 查询写入，仅展示用）
	Quota      map[string]QuotaEntry `json:"quota,omitempty"`
	QuotaAt    time.Time             `json:"quota_at,omitempty"`
	PlanName   string                `json:"plan_name,omitempty"`
	PlanExpire time.Time             `json:"plan_expire,omitempty"`

	// ModelHealth 逐模型健康（runtime 对话失败写入）：模型代码 → 惩罚。
	// 目的：某模型上游 5xx/限流/额度抖动只冷却该(账号+模型)，不牵连同账号其他模型，
	// 更不把整号打成 5 分钟黑洞。凭证失效(401/403)与风控(3012)仍按账号级 Status 处理。
	ModelHealth map[string]ModelPenalty `json:"model_health,omitempty"`

	UseCount    int64     `json:"use_count"`
	FailCount   int64     `json:"fail_count"`
	RiskStrikes int       `json:"risk_strikes"` // 累计风控封禁次数；成功即清零
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
	LastOKAt    time.Time `json:"last_ok_at,omitempty"`
	LastProbeAt time.Time `json:"last_probe_at,omitempty"` // 额度再探时刻（EXHAUSTED 恢复判定）
	LastErrAt   time.Time `json:"last_err_at,omitempty"`

	// Fingerprint 每账号独立桌面设备档案（platform/arch/os/language/timezone/device_mid）。
	// 一号一台：多账号共用同一设备形态是上游关联信号。
	Fingerprint *Profile `json:"fingerprint,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// QuotaEntry 单模型额度快照。
type QuotaEntry struct {
	Total     int64     `json:"total"`
	Used      int64     `json:"used"`
	Remaining int64     `json:"remaining"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// PenaltyKind 逐模型惩罚的成因（供面板与健康判定展示）。
const (
	PenaltyServer    = "server"    // 上游 5xx 抖动（含 "Internal Network Failure"）
	PenaltyRate      = "rate"      // 上游限流 429
	PenaltyExhausted = "exhausted" // 该模型额度用完（对话 402）
)

// ModelPenalty 单(账号,模型)的临时惩罚记录。
type ModelPenalty struct {
	Kind    string    `json:"kind"`
	Until   time.Time `json:"until"`
	Fails   int       `json:"fails,omitempty"` // 连续失败计数（驱动退避阶梯）
	LastErr string    `json:"last_err,omitempty"`
	LastAt  time.Time `json:"last_at,omitempty"`
}

// NewAccount 由凭证串建账号：三段点分判为 JWT，其余判为 API Key。
func NewAccount(name, secret string) *Account {
	secret = strings.TrimSpace(secret)
	mode := ModeAPIKey
	if strings.Count(secret, ".") == 2 {
		mode = ModeJWT
	}
	a := &Account{
		Name:      strings.TrimSpace(name),
		Provider:  ProviderZai,
		Mode:      mode,
		Enabled:   true,
		Status:    StatusActive,
		CreatedAt: time.Now(),
	}
	if a.Name == "" {
		a.Name = "zai-" + time.Now().Format("0102-150405")
	}
	if mode == ModeJWT {
		a.JWT = secret
	} else {
		a.APIKey = secret
	}
	a.ID = accountID(a.Name)
	a.Fingerprint = newProfile()
	return a
}

// accountID 由名称派生稳定 ID。
//
// 名称多为中文（"腾讯主号"），直接过滤非 ASCII 会得到空串——多个账号撞成同一
// 个 ID 互相覆盖。所以：ASCII 部分作可读前缀 + 名称哈希作唯一后缀；纯中文名
// 则只有哈希部分。
func accountID(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return -1
		}
	}, name)
	sanitized = strings.Trim(strings.TrimSpace(sanitized), "-_")
	if len(sanitized) > 32 {
		sanitized = sanitized[:32]
	}
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(name))))
	suffix := hex.EncodeToString(sum[:3]) // 6 位十六进制，碰撞概率可忽略
	if sanitized == "" {
		return "zai-" + suffix
	}
	return sanitized + "-" + suffix
}

// HasJWTPath 当前是否可走 Plan 通道（JWT 存在、启用、且未被判失效/禁用）。
// 仅 zai 提供方有 Plan 通道；bigmodel 只有 API Key。
func (a *Account) HasJWTPath() bool {
	if a.Provider == ProviderBigModel {
		return false
	}
	if a.Mode != ModeJWT || strings.TrimSpace(a.JWT) == "" || !a.Enabled {
		return false
	}
	return a.Status != StatusInvalid && a.Status != StatusDisabled
}

// HasKeyFallback 是否可走 API Key 回退通道。
//
// 仅「JWT 账号自带的附加 Key」算回退：纯 API Key 账号在失效/风控后不得靠
// 这把主键继续被选中（否则风控账号会换个通道继续打上游）。
func (a *Account) HasKeyFallback() bool {
	if a.Mode != ModeJWT {
		return false
	}
	if !a.Enabled || a.Status == StatusDisabled {
		return false
	}
	return strings.TrimSpace(a.APIKey) != ""
}

// Selectable 是否可被选号（对话路径）。
func (a *Account) Selectable(now time.Time) bool {
	if !a.Enabled || a.Status == StatusInvalid || a.Status == StatusDisabled {
		return false
	}
	if a.Status == StatusCooling && now.Before(a.CoolingUntil) {
		return false
	}
	if a.Status == StatusExhausted && now.Before(a.CoolingUntil) {
		return false // 额度用完也带一个再探间隔（复用 CoolingUntil 字段）
	}
	return a.HasJWTPath() || a.HasKeyFallback() || a.Mode == ModeAPIKey
}

// Cool 进入冷却（5xx 重试耗尽）。
func (a *Account) Cool(d time.Duration) {
	a.Status = StatusCooling
	a.CoolingUntil = time.Now().Add(d)
}

// Exhaust 标记额度用完；probeAfter 为下次再探间隔。
func (a *Account) Exhaust(probeAfter time.Duration) {
	a.Status = StatusExhausted
	a.CoolingUntil = time.Now().Add(probeAfter)
}

// Invalidate 凭证失效（401/403 非验证码）。
func (a *Account) Invalidate(reason string) {
	a.Status = StatusInvalid
	a.LastError = reason
	a.LastErrAt = time.Now()
}

// BanForRisk 真风控命中：禁用保护，须人工恢复。
func (a *Account) BanForRisk(reason string) {
	a.RiskStrikes++
	a.Status = StatusDisabled
	a.CoolingUntil = time.Time{}
	a.LastError = reason
	a.LastErrAt = time.Now()
}

// MarkOK 成功：清除冷却/额度用完/错误，回到 ACTIVE；风控计数清零。
func (a *Account) MarkOK() {
	if a.Status != StatusDisabled {
		a.Status = StatusActive
		a.CoolingUntil = time.Time{}
		a.LastError = ""
	}
	a.RiskStrikes = 0
	a.LastOKAt = time.Now()
}

// MarkUsed 记录一次调用。
func (a *Account) MarkUsed() {
	a.UseCount++
	a.LastUsedAt = time.Now()
}

// MarkFail 记录一次失败。
func (a *Account) MarkFail(reason string) {
	a.FailCount++
	a.LastError = reason
	a.LastErrAt = time.Now()
}

// ModelPenalized 该模型此刻是否处于惩罚（冷却）中。过期条目视为不再惩罚。
func (a *Account) ModelPenalized(model string, now time.Time) bool {
	p, ok := a.ModelHealth[model]
	if !ok {
		return false
	}
	return now.Before(p.Until)
}

// PenalizeModel 给该(账号,模型)记一次失败并按阶梯冷却 d。d<=0 表示仅记失败不冷却。
// 连续失败会让阶梯更久（由调用方按 Fails 计算 d 传入）。
func (a *Account) PenalizeModel(model string, d time.Duration, kind, reason string) {
	if model == "" {
		return
	}
	if a.ModelHealth == nil {
		a.ModelHealth = map[string]ModelPenalty{}
	}
	prev := a.ModelHealth[model]
	fails := prev.Fails + 1
	if prev.Kind != kind {
		fails = 1 // 成因切换，阶梯重新计
	}
	pen := ModelPenalty{Kind: kind, Fails: fails, LastErr: reason, LastAt: time.Now()}
	if d > 0 {
		pen.Until = time.Now().Add(d)
	} else {
		pen.Until = time.Time{}
	}
	a.ModelHealth[model] = pen
}

// ClearModelPenalty 成功后清掉该模型的惩罚记录。
func (a *Account) ClearModelPenalty(model string) {
	if model == "" {
		return
	}
	if _, ok := a.ModelHealth[model]; ok {
		delete(a.ModelHealth, model)
	}
}

// ReapModelHealth 删除已过期的模型惩罚，返回是否有变化。
func (a *Account) ReapModelHealth(now time.Time) bool {
	changed := false
	for m, p := range a.ModelHealth {
		if !p.Until.IsZero() && now.After(p.Until) {
			delete(a.ModelHealth, m)
			changed = true
		}
	}
	return changed
}

// SelectableFor 模型感知的可选判定：账号级可选 **且** 该模型未在惩罚中。
func (a *Account) SelectableFor(model string, now time.Time) bool {
	return a.Selectable(now) && !a.ModelPenalized(model, now)
}

// Secret 主凭证（展示/掩码用）。
func (a *Account) Secret() string {
	if a.Mode == ModeJWT {
		return a.JWT
	}
	return a.APIKey
}

// Mask 掩码展示。
func Mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 12 {
		return "••••"
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// String 便于日志。
func (a *Account) String() string {
	return fmt.Sprintf("%s(%s/%s)", a.Name, a.Mode, a.Status)
}
