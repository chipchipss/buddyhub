// Package qoder: quota.go — 阶段 4：额度/Key 分配与每请求积分记账。
//
// 移植自 9router-qoder-plus（HEAD 290c68c）src/shared/services/apiKeyPolicy.js：
//   - 账号级分配（accountAllocations）：每个 connectionId 一个上限，路由前
//     PRE 门判定（evaluateApiKeyProviderCreditUsage L185-249）——已用量
//     >= 账号上限即 exhausted，不再路由到该号；
//   - 精确记账（applyQoderPreciseCreditUsage L278-297）：按请求 credits 增量
//     累加，precise=true 后以账本为准，不再回落余额差估算；
//   - 余额差估算兜底（calculateQuotaConsumed L309-328）：无精确记账时用
//     baseline.initialRemainingQuota - current 估算，取 max(估算, 账本)；
//   - 合并时保留历史账本（mergeQoderCreditUsageLedger L251-276）：账号余额
//     下跌可能属于别的 Key/客户端，只有请求 credits 才计入消耗——buddyhub
//     单网关场景全量请求都过本网关，仍保留该语义防多客户端串账。
//
// buddyhub 侧差异：fork 的 policy 是「一把 API Key 绑一组 connection」的多租户
// 模型；buddyhub 是单网关直连账号池，因此本文件只做三件事：
//   1. 每账号的分配上限与已用积分账本（精确优先、估算兜底）；
//   2. PRE 门：路由前判定账号是否还有额度（fail-open——基础设施出错不挡流量）；
//   3. POST 记账：流结束后把本次请求 credits 累进账本。
// 账号选择/冷却仍走既有 extstore.SelectChatOrder（fork 的 pool 骨架在
// buddyhub 对应物就是它，见 select.go 文件头“为什么不能照搬腾讯 pool”）。
package qoder

import (
	"sync"
	"time"
)

// CreditLedger 积分账本：账号 → 精确已用积分（fork creditUsageLedger）。
//
// 与「余额差估算」的关系（fork L220-224 语义）：精确记账开启后以账本为准，
// 未开启时取 max(余额差估算, 账本累计)——两条路都保留，任一路有数就能判额度。
type CreditLedger struct {
	mu    sync.Mutex
	used  map[string]float64 // "provider/id" → 精确已用积分
	prec  map[string]bool    // 该账号是否有过精确记账（有则不再回落估算）
	alloc map[string]float64 // 账号分配上限（0/缺失 = 不限制）
}

// NewCreditLedger 建空账本。
func NewCreditLedger() *CreditLedger {
	return &CreditLedger{
		used:  map[string]float64{},
		prec:  map[string]bool{},
		alloc: map[string]float64{},
	}
}

// SetAllocation 设置账号分配上限（积分）。limit<=0 = 清除限制（不限制）。
func (l *CreditLedger) SetAllocation(accountKey string, limit float64) {
	if l == nil || accountKey == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		delete(l.alloc, accountKey)
		return
	}
	l.alloc[accountKey] = limit
}

// AllocationLimit 读账号分配上限（0 = 不限制）。
func (l *CreditLedger) AllocationLimit(accountKey string) float64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alloc[accountKey]
}

// RecordCredits 精确记账：本次请求消耗 credits 累加进账本（fork
// applyQoderPreciseCreditUsage L278-297）。credits<=0 忽略；记过后该账号
// 标记 precise，后续额度判定以账本为准。
func (l *CreditLedger) RecordCredits(accountKey string, credits float64) {
	if l == nil || accountKey == "" || credits <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used[accountKey] += credits
	l.prec[accountKey] = true
}

// UsedCredits 读精确已用积分。
func (l *CreditLedger) UsedCredits(accountKey string) float64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used[accountKey]
}

// IsPrecise 该账号是否有精确记账（fork precise 标志）。
func (l *CreditLedger) IsPrecise(accountKey string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.prec[accountKey]
}

// EffectiveUsed 生效已用量（fork L220-224 语义）：precise 时以账本为准，
// 否则取 max(余额差估算, 账本累计)——估算来自上游余额观测，可能滞后，
// 故两条路取大者，任一路超限都能拦住。
func (l *CreditLedger) EffectiveUsed(accountKey string, estimatedFromBalance float64) float64 {
	if l == nil {
		return estimatedFromBalance
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ledger := l.used[accountKey]
	if l.prec[accountKey] {
		return ledger
	}
	if estimatedFromBalance > ledger {
		return estimatedFromBalance
	}
	return ledger
}

// Remaining 账号剩余额度（fork getAccountRemainingAllocation L384-387）：
// max(0, 上限 - 生效已用)。无上限时返回 -1（调用方按“不限制”处理）。
func (l *CreditLedger) Remaining(accountKey string, estimatedFromBalance float64) float64 {
	if l == nil {
		return -1
	}
	limit := l.AllocationLimit(accountKey)
	if limit <= 0 {
		return -1
	}
	used := l.EffectiveUsed(accountKey, estimatedFromBalance)
	if used >= limit {
		return 0
	}
	return limit - used
}

// QuotaGateResult PRE 门判定结果（对齐 fork evaluateApiKeyProviderCreditUsage
// 的返回形状，buddyhub 单账号版只留三态）。
type QuotaGateResult int

const (
	// QuotaOK 有额度（或未设上限）——放行。
	QuotaOK QuotaGateResult = iota
	// QuotaExhausted 已用 >= 上限——不再路由到该号。
	QuotaExhausted
	// QuotaUnknown 估算数据不可用（既无账本也无余额观测）——fail-open 放行，
	// 绝不能因配额基础设施不可用挡住正常流量（fork fail-open 规则）。
	QuotaUnknown
)

// Gate 路由前额度门（fork PRE-request 语义）：
//   - 未设分配上限 → QuotaOK（不限制）；
//   - 生效已用 >= 上限 → QuotaExhausted；
//   - 无上限但两条估算路都无数据 → QuotaUnknown（放行，由 POST 记账兜底）。
//
// estimatedFromBalance 是上游余额观测折算的“已用”（baseline - current），
// 无观测时传 -1。
func (l *CreditLedger) Gate(accountKey string, estimatedFromBalance float64) QuotaGateResult {
	if l == nil {
		return QuotaOK
	}
	limit := l.AllocationLimit(accountKey)
	if limit <= 0 {
		// 无显式上限：有账本数据也能判（精确记账超限同理）
		used := l.EffectiveUsed(accountKey, estimatedFromBalance)
		if used <= 0 && estimatedFromBalance < 0 {
			return QuotaUnknown
		}
		return QuotaOK
	}
	used := l.EffectiveUsed(accountKey, estimatedFromBalance)
	if used >= limit {
		return QuotaExhausted
	}
	if used <= 0 && estimatedFromBalance < 0 && !l.IsPrecise(accountKey) {
		return QuotaUnknown
	}
	return QuotaOK
}

// QuotaSnapshot 单账号额度快照（面板 /status 展示用）。
type QuotaSnapshot struct {
	AccountKey string  `json:"account_key"`
	Allocated  float64 `json:"allocated"`
	Used       float64 `json:"used"`
	Remaining  float64 `json:"remaining"`
	Exhausted  bool    `json:"exhausted"`
	Precise    bool    `json:"precise"`
}

// Snapshot 全部账号的额度快照（按 key 排序由调用方决定，这里稳定输出）。
func (l *CreditLedger) Snapshot(estimates map[string]float64) []QuotaSnapshot {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	keys := map[string]bool{}
	for k := range l.alloc {
		keys[k] = true
	}
	for k := range l.used {
		keys[k] = true
	}
	for k := range estimates {
		keys[k] = true
	}
	out := make([]QuotaSnapshot, 0, len(keys))
	for k := range keys {
		est := -1.0
		if v, ok := estimates[k]; ok {
			est = v
		}
		alloc := l.alloc[k]
		used := l.EffectiveUsedLocked(k, est)
		remaining := -1.0
		if alloc > 0 {
			remaining = alloc - used
			if remaining < 0 {
				remaining = 0
			}
		}
		out = append(out, QuotaSnapshot{
			AccountKey: k,
			Allocated:  alloc,
			Used:       used,
			Remaining:  remaining,
			Exhausted:  alloc > 0 && used >= alloc,
			Precise:    l.prec[k],
		})
	}
	l.mu.Unlock()
	return out
}

// EffectiveUsedLocked 持锁版 EffectiveUsed（Snapshot 内部用，避免锁重入）。
func (l *CreditLedger) EffectiveUsedLocked(accountKey string, estimatedFromBalance float64) float64 {
	ledger := l.used[accountKey]
	if l.prec[accountKey] {
		return ledger
	}
	if estimatedFromBalance > ledger {
		return estimatedFromBalance
	}
	return ledger
}

// AccountKey 账号在账本中的键（provider/id）。
func AccountKey(provider, id string) string { return provider + "/" + id }

// CreditObservation 一次上游余额观测（baseline 折算用）。
type CreditObservation struct {
	AccountKey       string
	InitialRemaining float64 // baseline 捕获时的剩余额度
	CurrentRemaining float64 // 当前剩余额度（-1 = 本次没查到）
	At               time.Time
}

// EstimateUsedFromBalance 余额差估算（fork calculateQuotaConsumed L319 的
// 简化口径）：max(0, baseline - current)。上游没给数（current<0）返回 -1
//（不可用），调用方按 fail-open 处理。
func EstimateUsedFromBalance(obs CreditObservation) float64 {
	if obs.CurrentRemaining < 0 || obs.InitialRemaining < 0 {
		return -1
	}
	used := obs.InitialRemaining - obs.CurrentRemaining
	if used < 0 {
		// 额度上涨（活动赠送/月度重置）：不算消耗（fork hasQuotaGrownAboveBaseline
		// 同向处理——增长段不计入 used）。
		return 0
	}
	return used
}

// UsageFromChatCompletion 从聚合响应里取 token 统计（Qoder 上游在末帧
// usage 对象带 prompt/completion/total）。桥接 POST 记账用：优先精确
// credits（若上游给），否则 token 不折算积分——buddyhub 侧积分汇率未知，
// 精确记账依赖上游明示的 credit 消耗，token 统计走 usage.Recorder 那条线。
type UsageFromChatCompletion struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// ParseChatUsage 从聚合响应 map 解析 usage（无 usage 字段返回零值+false）。
func ParseChatUsage(resp map[string]any) (UsageFromChatCompletion, bool) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return UsageFromChatCompletion{}, false
	}
	out := UsageFromChatCompletion{}
	if v, ok := u["prompt_tokens"].(float64); ok {
		out.PromptTokens = int64(v)
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		out.CompletionTokens = int64(v)
	}
	if v, ok := u["total_tokens"].(float64); ok {
		out.TotalTokens = int64(v)
	}
	return out, true
}
