// Package server: qoder_ccswitch.go — 阶段 4：CC-Switch 兼容端点。
//
// 移植自 9router-qoder-plus（HEAD 290c68c）：
//   - POST /api/usage：src/app/api/usage/route.js（9 行全文，转
//     queryCcSwitchUsage）+ src/lib/ccSwitchUsageRequest.js（135 行全文）
//   - src/shared/services/ccSwitchUsage.js（buildCcSwitchUsage）；
//   - POST /user/balance：ccSwitchUsageRequest 的 includeActiveField 分支
//     （is_active 字段）——CC-Switch 客户端探测 Key 有效性时用。
//
// CC-Switch 是第三方 Qoder 账号切换客户端，靠这两个端点读网关的额度视图。
// 响应契约（fork buildCcSwitchUsage 返回形状，逐字段对齐）：
//
//	{isValid, balance, remaining, total, used, unit:"credits",
//	 planName, extra}  + /user/balance 追加 is_active。
//
// 鉴权口径与 fork 一致：Bearer token（Authorization 头）或 x-api-key 头，
// 走网关既有 withAuth（多 Key 体系命中即过）。额度数据源：
//  1. 有分配上限（qoderLedger.alloc）：本地账本口径（不依赖上游查询，
//     fork 注释原文"Even an expired account token cannot erase it"）；
//  2. 无分配上限：账号级上游额度（buddyhub 侧由余额刷新线程观测，
//     上游无 rows 时退化为 remainingQuota 单行）。
//
// 上游不可用 → quotaStatus:"unavailable" 计入 extra（不失败，与 fork 同）。
package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// ccSwitchUsageBody CC-Switch 用量响应（fork buildCcSwitchUsage 返回形状）。
type ccSwitchUsageBody struct {
	IsValid   bool    `json:"isValid"`
	Balance   float64 `json:"balance"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Unit      string  `json:"unit"`
	PlanName  string  `json:"planName"`
	Extra     string  `json:"extra,omitempty"`
	// IsActive 仅 /user/balance 分支带（fork includeActiveField）。
	IsActive *bool `json:"is_active,omitempty"`
	// Error/InvalidMessage 不可用时的说明（fork 的 error/invalidMessage）。
	Error          string `json:"error,omitempty"`
	InvalidMessage string `json:"invalidMessage,omitempty"`
}

// ccSwitchAccount 一个账号的额度视图（fork loadAccountUsage 返回形状）。
type ccSwitchAccount struct {
	ID             string
	Name           string
	QuotaStatus    string // "ok" | "unavailable" | "not_queried"
	QuotaMessage   string
	RemainingQuota float64
	HasRemaining   bool
	QuotaRows      []ccSwitchQuotaRow
}

// ccSwitchQuotaRow 一行额度明细（fork quotaRows 形状）。
type ccSwitchQuotaRow struct {
	Name      string  `json:"name"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total,omitempty"`
	Used      float64 `json:"used,omitempty"`
}

// ccSwitchUsage POST /api/usage（fork src/app/api/usage/route.js）。
// CC-Switch 客户端周期性拉取额度视图。
func (h *Handler) ccSwitchUsage(w http.ResponseWriter, r *http.Request) {
	h.ccSwitchUsageWithActive(w, r, false)
}

// ccSwitchBalance POST /user/balance（fork includeActiveField 分支）：
// 与 /api/usage 同源，追加 is_active 字段（Key 有效性探测）。
func (h *Handler) ccSwitchBalance(w http.ResponseWriter, r *http.Request) {
	h.ccSwitchUsageWithActive(w, r, true)
}

// ccSwitchUsageWithActive 两个端点的共同实现（fork queryCcSwitchUsage）。
func (h *Handler) ccSwitchUsageWithActive(w http.ResponseWriter, r *http.Request, includeActive bool) {
	body := h.buildCcSwitchUsage(r, includeActive)
	writeJSON(w, http.StatusOK, body)
}

// buildCcSwitchUsage 装配 CC-Switch 用量响应（fork queryCcSwitchUsage
// L61-135 + buildCcSwitchUsage 语义）。
func (h *Handler) buildCcSwitchUsage(r *http.Request, includeActive bool) ccSwitchUsageBody {
	withActive := func(b ccSwitchUsageBody) ccSwitchUsageBody {
		if includeActive {
			yes := true
			b.IsActive = &yes
		}
		return b
	}
	if h.cfg.ExtAccounts == nil {
		return withActive(ccSwitchUsageBody{
			IsValid:        false,
			Error:          "No active Qoder accounts",
			InvalidMessage: "No active Qoder accounts are available.",
		})
	}
	accounts := h.qoderAccountsView()
	allocated := h.qoderHasAllocation()
	if len(accounts) == 0 && !allocated {
		return withActive(ccSwitchUsageBody{
			IsValid:        false,
			Error:          "No active Qoder accounts",
			InvalidMessage: "No active Qoder accounts are available.",
		})
	}

	// 分支一：有分配上限——本地账本口径（fork L93/L113-127 分支：不查上游，
	// 过期 token 也抹不掉已分配的余额）。
	if allocated {
		total, used, active := h.qoderAllocationView(accounts)
		remaining := total - used
		if remaining < 0 {
			remaining = 0
		}
		return withActive(ccSwitchUsageBody{
			IsValid:   true,
			Balance:   remaining,
			Remaining: remaining,
			Total:     total,
			Used:      used,
			Unit:      "credits",
			PlanName:  "Qoder allocated credits",
			Extra:     buildCcSwitchExtra(accounts, active),
		})
	}

	// 分支二：无分配上限——账号级上游额度汇总（fork accountRows 口径：
	// unavailable 账号不计入，剩余/总量按行汇总）。
	rows := ccSwitchAccountRows(accounts)
	var total, used float64
	for _, row := range rows {
		total += effectiveCcSwitchTotal(row)
		used += row.Used
	}
	remaining := total - used
	if remaining < 0 {
		remaining = 0
	}
	return withActive(ccSwitchUsageBody{
		IsValid:   true,
		Balance:   remaining,
		Remaining: remaining,
		Total:     total,
		Used:      used,
		Unit:      "credits",
		PlanName:  "Qoder all accounts",
		Extra:     buildCcSwitchExtra(accounts, ""),
	})
}

// qoderAccountsView 把 extstore 账号表转成 CC-Switch 账号视图。
// 额度数据源：余额缓存观测（RunExtBalanceRefresh 每 5 分钟喂一次）；
// 无观测 → quotaStatus "unavailable"（fork loadAccountUsage 失败分支）。
func (h *Handler) qoderAccountsView() []ccSwitchAccount {
	var out []ccSwitchAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a == nil || a.Provider != extstore.PQoder {
			continue
		}
		view := ccSwitchAccount{
			ID:          a.ID,
			Name:        ccSwitchAccountName(a),
			QuotaStatus: "not_queried",
		}
		if h.extManager != nil {
			if bal, ok := h.extManager.CachedBalance(string(a.Provider)); ok {
				view.QuotaStatus = "ok"
				view.RemainingQuota = bal
				view.HasRemaining = true
				view.QuotaRows = []ccSwitchQuotaRow{{
					Name:      view.Name,
					Remaining: bal,
					Used:      0,
				}}
			}
		}
		out = append(out, view)
	}
	return out
}

// ccSwitchAccountName 账号显示名（fork displayName||name||email||id 链；
// buddyhub 侧 ExtAccount 无 email 字段，链为 label→id）。
func ccSwitchAccountName(a *extstore.ExtAccount) string {
	for _, v := range []string{a.Label, a.ID} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return a.ID
}

// qoderHasAllocation 是否有任一账号设了分配上限。
func (h *Handler) qoderHasAllocation() bool {
	if h.qoderLedger == nil {
		return false
	}
	for _, s := range h.qoderLedger.Snapshot(nil) {
		if s.Allocated > 0 {
			return true
		}
	}
	return false
}

// qoderAllocationView 分配口径的总额/已用/活跃账号（fork
// evaluateApiKeyProviderCreditUsage 的分配分支 + activeConnectionId）。
func (h *Handler) qoderAllocationView(accounts []ccSwitchAccount) (total, used float64, active string) {
	if h.qoderLedger == nil {
		return 0, 0, ""
	}
	byID := map[string]ccSwitchAccount{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	// 稳定顺序：按账号 ID 排序（输出确定性）。
	snaps := h.qoderLedger.Snapshot(nil)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].AccountKey < snaps[j].AccountKey })
	for _, s := range snaps {
		if s.Allocated <= 0 {
			continue
		}
		total += s.Allocated
		used += s.Used
		// 活跃账号：还有余额的第一个（fork activeConnectionId 语义——
		// 路由优先级永不因记账截断后续账号）。
		if active == "" && s.Remaining > 0 {
			active = s.AccountKey
		}
	}
	return total, used, active
}

// ccSwitchAccountRows 汇总账号级行（fork accountRows：unavailable 不计入，
// 有 rows 用 rows，否则 remainingQuota 单行）。
func ccSwitchAccountRows(accounts []ccSwitchAccount) []ccSwitchQuotaRow {
	var out []ccSwitchQuotaRow
	for _, a := range accounts {
		if a.QuotaStatus == "unavailable" {
			continue
		}
		if len(a.QuotaRows) > 0 {
			out = append(out, a.QuotaRows...)
			continue
		}
		if a.HasRemaining {
			out = append(out, ccSwitchQuotaRow{Name: a.Name, Remaining: a.RemainingQuota})
		}
	}
	return out
}

// effectiveCcSwitchTotal 行的总量（fork effectiveTotal：total 优先，
// 缺失用 used+remaining 兜底）。
func effectiveCcSwitchTotal(row ccSwitchQuotaRow) float64 {
	if row.Total > 0 {
		return row.Total
	}
	used := row.Used
	if used < 0 {
		used = 0
	}
	remaining := row.Remaining
	if remaining < 0 {
		remaining = 0
	}
	return used + remaining
}

// buildCcSwitchExtra 附加说明串（fork buildExtra：账号数 + 活跃 + 不可用数）。
func buildCcSwitchExtra(accounts []ccSwitchAccount, activeKey string) string {
	unavailable := 0
	for _, a := range accounts {
		if a.QuotaStatus == "unavailable" {
			unavailable++
		}
	}
	parts := []string{itoaCC(len(accounts)) + " accounts"}
	if activeKey != "" {
		name := activeKey
		for _, a := range accounts {
			if qoder.AccountKey(string(extstore.PQoder), a.ID) == activeKey && a.Name != "" {
				name = a.Name
				break
			}
		}
		parts = append(parts, "active: "+name)
	}
	if unavailable > 0 {
		parts = append(parts, itoaCC(unavailable)+" accounts unavailable")
	}
	return strings.Join(parts, " · ")
}

// itoaCC 小整数转串（避免拉 strconv 进这个文件的琐碎依赖）。
func itoaCC(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
