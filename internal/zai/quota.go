package zai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 额度（billing）族端点。协议事实来源：zcode2api app/quota.py + constants.py。
//
// ⚠️ 上游 WAF 对 billing 族的连续查询敏感：轮询必须错峰、间隔不能太密。
// 默认 5 分钟一轮（面板可调），并在每轮内串行化单账号的三次查询。
const (
	billingCurrentPath = "/billing/current"
	billingBalancePath = "/billing/balance"
	billingUsagePath   = "/usage"
)

// QuotaResult 一次额度查询的原始回执（面板透出与排障用）。
type QuotaResult struct {
	Billing map[string]any `json:"billing,omitempty"`
	Balance map[string]any `json:"balance,omitempty"`
	Usage   map[string]any `json:"usage,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// billingHeaders 组装 billing 族请求头。
//
// 与 messages 通道刻意不同：UA/版本走官方桌面端形态，并**必须带 X-Device-Mid**
// （缺失时上游返回 code=3001 parameter error）。平台/系统/语言/时区取该账号
// 自己的设备档案——一号一台，不共用。
func billingHeaders(a *Account) map[string]string {
	a.EnsureProfile()
	p := a.Fingerprint
	h := map[string]string{
		"Content-Type":        "application/json",
		"User-Agent":          "ZCode/" + ClientAppVersion,
		"HTTP-Referer":        PlanOrigin,
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": ClientAppVersion,
		"X-Platform":          p.PlatformFull(),
		"X-Release-Channel":   "stable",
		"X-Client-Language":   p.Language,
		"X-Client-Timezone":   p.Timezone,
		"X-Os-Category":       p.OSCategory(),
		"X-Os-Version":        p.OSVersion,
		"X-Device-Mid":        p.DeviceMid,
		"x-request-id":        UUID(),
	}
	if a.HasJWTPath() {
		h["Authorization"] = "Bearer " + a.JWT
	} else if a.APIKey != "" {
		h["x-api-key"] = a.APIKey
	}
	return h
}

// FetchQuota 拉取单账号的 方案 / 余额 / 用量，写回账号状态与额度快照。
//
// 状态联动（对齐上游语义）：
//   - 401/403（非验证码）→ 凭证失效 INVALID
//   - 所有日窗口剩余 ≤ 0 且无生效中的一次性赠送 → EXHAUSTED
//   - 额度恢复（窗口重置 / 赠送生效）→ 从 EXHAUSTED/COOLING 回到 ACTIVE
//     （冷却期内不提前解除；INVALID/DISABLED 绝不因额度数字复活 Plan 通道）
func (c *Client) FetchQuota(ctx context.Context, id string) *QuotaResult {
	st := c.Store()
	if st == nil {
		return &QuotaResult{Error: "账号池未启用"}
	}
	acc := st.Get(id)
	if acc == nil {
		return &QuotaResult{Error: "账号不存在"}
	}

	headers := billingHeaders(acc)
	res := &QuotaResult{}

	billingBody, billingStatus := c.billingGet(ctx, billingCurrentPath, headers)
	if billingStatus == http.StatusUnauthorized || billingStatus == http.StatusForbidden {
		lower := lowerString(billingBody)
		if !containsAny(lower, []string{"captcha", "verify"}) {
			_ = st.Update(id, func(a *Account) { a.Invalidate("额度查询被拒：凭证失效") })
			return &QuotaResult{Error: "凭证失效（HTTP " + itoa(billingStatus) + "）"}
		}
	}

	quota := map[string]QuotaEntry{}
	var plans []any
	if billingStatus == 200 {
		var doc map[string]any
		if json.Unmarshal(billingBody, &doc) == nil {
			res.Billing = doc
			if data, ok := doc["data"].(map[string]any); ok {
				plans, _ = data["plans"].([]any)
			}
		}
	}

	balanceBody, balanceStatus := c.billingGet(ctx, billingBalancePath, headers)
	if balanceStatus == 200 {
		var doc map[string]any
		if json.Unmarshal(balanceBody, &doc) == nil {
			res.Balance = doc
			if data, ok := doc["data"].(map[string]any); ok {
				if bals, ok := data["balances"].([]any); ok {
					quota = mergeBalances(bals)
				}
			}
		}
	}

	usageBody, usageStatus := c.billingGet(ctx, billingUsagePath, headers)
	if usageStatus == 200 {
		var doc map[string]any
		if json.Unmarshal(usageBody, &doc) == nil {
			res.Usage = doc
		}
	}

	if len(quota) == 0 && res.Billing == nil && res.Balance == nil {
		if res.Error == "" {
			res.Error = "无法获取额度数据"
		}
		return res
	}

	// 写回：额度快照 + 状态联动
	_ = st.Update(id, func(a *Account) {
		a.QuotaAt = time.Now()
		if len(quota) > 0 {
			a.Quota = quota
		}
		if len(plans) > 0 {
			if first, ok := plans[0].(map[string]any); ok {
				a.PlanName, _ = first["name"].(string)
				if v := numAt(first["expires_at"]); v > 0 {
					a.PlanExpire = unixTime(v)
				}
			}
		}
		applyQuotaStatus(a, quota, plans, time.Now())
	})
	return res
}

// billingGet 发一次 billing 查询，返回体与状态码（网络错误返回 0）。
func (c *Client) billingGet(ctx context.Context, path string, headers map[string]string) ([]byte, int) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, BillingBase+path, nil)
	if err != nil {
		return nil, 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := zaiHTTP().Do(req)
	if err != nil {
		return nil, 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode
}

// mergeBalances 把 balances 数组归一为「模型名 → 额度」。
//
// 同一模型可能出现多个窗口（日窗口 + 一次性赠送）：**合并而不是覆盖**，
// 否则后一个窗口会把前一个的数字冲掉，额度显示与耗尽判定都会错。
func mergeBalances(bals []any) map[string]QuotaEntry {
	out := map[string]QuotaEntry{}
	for _, b := range bals {
		m, _ := b.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["show_name"].(string)
		if name == "" {
			name, _ = m["model"].(string)
		}
		if name == "" {
			name = "model"
		}
		e := QuotaEntry{
			Total:     numAt(m["total_units"]),
			Used:      numAt(m["used_units"]),
			Remaining: numAt(m["remaining_units"]),
		}
		if v := numAt(m["expires_at"]); v > 0 {
			e.ExpiresAt = unixTime(v)
		}
		if prev, ok := out[name]; ok {
			prev.Total += e.Total
			prev.Used += e.Used
			prev.Remaining += e.Remaining
			if e.ExpiresAt.After(prev.ExpiresAt) {
				prev.ExpiresAt = e.ExpiresAt
			}
			out[name] = prev
		} else {
			out[name] = e
		}
	}
	return out
}

// applyQuotaStatus 由额度数字推导账号状态（见 FetchQuota 注释的规则）。
func applyQuotaStatus(a *Account, quota map[string]QuotaEntry, plans []any, now time.Time) {
	if len(quota) == 0 {
		return
	}
	dailyExhausted := true
	hasDaily := false
	for _, q := range quota {
		if q.Remaining > 0 {
			hasDaily = true
			dailyExhausted = false
		}
	}
	hasBonus := false
	for _, p := range plans {
		if m, ok := p.(map[string]any); ok && bonusActive(m, now) {
			hasBonus = true
			break
		}
	}

	switch {
	case dailyExhausted && !hasBonus:
		// 所有窗口都空且没有生效中的赠送池
		a.Status = StatusExhausted
		a.LastError = "额度已用完"
	case (a.Status == StatusExhausted || a.Status == StatusCooling) && (hasDaily || hasBonus):
		// 额度恢复。冷却期内不提前解除；INVALID/DISABLED 不在此列（状态已不匹配）
		if a.Status == StatusExhausted {
			a.Status = StatusActive
			a.CoolingUntil = time.Time{}
			a.LastError = ""
		}
	}
}

// bonusActive plan 是否带生效中的一次性赠送授权（balance 不含这类额度，
// 单看日窗口会把仍有赠送额度的账号误判为耗尽）。
func bonusActive(plan map[string]any, now time.Time) bool {
	ents, _ := plan["entitlements"].([]any)
	for _, e := range ents {
		m, ok := e.(map[string]any)
		if !ok || m["period"] != "one_time" {
			continue
		}
		eff := numAt(m["effective_at"])
		ends := numAt(m["ends_at"])
		if ends == 0 {
			ends = numAt(m["expires_at"])
		}
		if eff > 0 && eff <= now.Unix() && (ends == 0 || now.Unix() <= ends) {
			return true
		}
	}
	return false
}

// RefreshQuotas 并发刷新一批账号（并发上限 4，避免对上游形成突发）。
// 返回 (成功数, 失败数)。
func (c *Client) RefreshQuotas(ctx context.Context, ids []string) (int, int) {
	if len(ids) == 0 {
		return 0, 0
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var okCount, failCount int64
	var mu sync.Mutex

	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := c.FetchQuota(ctx, id)
			mu.Lock()
			if r.Error == "" {
				okCount++
			} else {
				failCount++
			}
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return int(okCount), int(failCount)
}

// StartQuotaLoop 后台周期刷新额度（错峰：默认 5 分钟一轮，只刷 JWT 账号）。
// 返回停止函数。
func (c *Client) StartQuotaLoop(interval time.Duration) func() {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	stop := make(chan struct{})
	go func() {
		timer := time.NewTimer(20 * time.Second) // 启动后先让位给服务初始化
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			var ids []string
			for _, a := range c.Snapshots() {
				if a.Mode == ModeJWT && a.Enabled {
					ids = append(ids, a.ID)
				}
			}
			if len(ids) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				ok, fail := c.RefreshQuotas(ctx, ids)
				cancel()
				if ok > 0 || fail > 0 {
					logf("zai: 额度刷新完成 成功 %d / 失败 %d", ok, fail)
				}
			}
			timer.Reset(interval)
		}
	}()
	return func() { close(stop) }
}

// ---- 小工具 ----

func numAt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// unixTime 兼容秒与毫秒时间戳（上游不同接口口径不一）。
func unixTime(v int64) time.Time {
	if v > 1e12 {
		return time.UnixMilli(v)
	}
	return time.Unix(v, 0)
}

func lowerString(b []byte) string { return strings.ToLower(string(b)) }

func itoa(n int) string { return strconv.Itoa(n) }
