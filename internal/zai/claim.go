package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// 套餐领取（Z.AI billing/preview + billing/claim）。协议事实来源：zcode2api
// app/claim.py + telemetry.py。
//
// 链路：
//
//	1. 激活上报（app_launch / app_daily_active）—— 官方客户端当日的活跃信号，
//	   疑似活动套餐的投放资格依据；失败不阻断领取
//	2. GET  /billing/preview?app_version=&platform=  → 可领取套餐（按优先级降序）
//	3. POST /billing/claim {"plan_id": ...}          → 需验证码
//
// 上游业务码：1001 套餐不存在 / 1002 活动结束 / 1003 已领取过 / 1004 不符合条件 /
// 1005 今日名额用完（带名额恢复时间）/ 3001 参数错误 / 3007 验证码失败（换码重试一次）。
const (
	claimPlanNotFound   = 1001
	claimActivityEnded  = 1002
	claimAlreadyClaimed = 1003
	claimNotEligible    = 1004
	claimQuotaFull      = 1005
	claimBadParams      = 3001
	claimCaptchaFailed  = 3007
	claimNotLoggedIn    = 401

	previewPath = "/billing/preview"
	claimPath   = "/billing/claim"
)

// eventReportURL 激活事件上报端点（由 origin 变量派生，测试可注入）。
func eventReportURL() string { return PlanOrigin + "/api/v1/event/report" }

// ClaimError 领取失败（含上游业务码语义，Message 面向用户）。
type ClaimError struct {
	Code    int
	Message string
	NextAt  time.Time // 仅 1005：名额恢复时间
}

func (e *ClaimError) Error() string { return e.Message }

// Grant 套餐内的授权项（模型额度）。
type Grant struct {
	Name   string  `json:"name"`
	Units  float64 `json:"units"`
	Period string  `json:"period"`
}

// Plan 可领取套餐。
type Plan struct {
	ID          string  `json:"plan_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Priority    int     `json:"priority"`
	Grants      []Grant `json:"grants,omitempty"`
}

// ClaimOutcome 一次领取的结果（成功载荷含套餐窗口与上游时钟）。
type ClaimOutcome struct {
	PlanID     string  `json:"plan_id"`
	PlanName   string  `json:"plan_name,omitempty"`
	Grants     []Grant `json:"grants,omitempty"`
	StartsAt   int64   `json:"starts_at,omitempty"`
	EndsAt     int64   `json:"ends_at,omitempty"`
	ServerTime int64   `json:"server_time,omitempty"`
	OK         bool    `json:"ok"`
	Message    string  `json:"message,omitempty"`
	Code       int     `json:"code,omitempty"`
	NextAt     int64   `json:"next_at,omitempty"` // 1005：名额恢复时间（毫秒）
}

// PreviewPlans 拉取账号当前可领取的套餐（按优先级降序）。
func (c *Client) PreviewPlans(ctx context.Context, id string) ([]Plan, error) {
	acc := c.Store().Get(id)
	if acc == nil {
		return nil, &ClaimError{Message: "账号不存在"}
	}
	if reason := billingBlockReason(acc); reason != "" {
		return nil, &ClaimError{Message: reason}
	}
	acc.EnsureProfile()

	q := url.Values{}
	q.Set("app_version", ClientAppVersion)
	q.Set("platform", acc.Fingerprint.PlatformFull())
	body, status, err := c.billingCall(ctx, http.MethodGet, previewPath+"?"+q.Encode(), billingHeaders(acc), nil)
	if err != nil {
		return nil, &ClaimError{Message: "上游网络错误：" + err.Error()}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if !containsAny(lowerString(body), []string{"captcha", "verify"}) {
			_ = c.Store().Update(id, func(a *Account) { a.Invalidate("领取查询被拒：凭证失效") })
			return nil, &ClaimError{Code: claimNotLoggedIn, Message: "凭证失效，请重新授权"}
		}
	}
	doc := parseJSON(body)
	if code := businessCode(doc); code != 0 {
		return nil, claimFailError(code, doc)
	}
	data, _ := doc["data"].(map[string]any)
	raw, _ := data["plans"].([]any)
	plans := make([]Plan, 0, len(raw))
	for _, p := range raw {
		if m, ok := p.(map[string]any); ok {
			if plan := parsePlan(m); plan != nil {
				plans = append(plans, *plan)
			}
		}
	}
	// 优先级降序（同优先级按 id 稳定排序）
	for i := 1; i < len(plans); i++ {
		for j := i; j > 0; j-- {
			if plans[j].Priority > plans[j-1].Priority ||
				(plans[j].Priority == plans[j-1].Priority && plans[j].ID < plans[j-1].ID) {
				plans[j], plans[j-1] = plans[j-1], plans[j]
				continue
			}
			break
		}
	}
	return plans, nil
}

// ClaimAll 领取账号当前全部可领套餐（需要验证码求解器）。
//
// 逐个领取：单个套餐失败不影响其余；3007 换一枚验证码重试一次。
func (c *Client) ClaimAll(ctx context.Context, id string) ([]ClaimOutcome, error) {
	acc := c.Store().Get(id)
	if acc == nil {
		return nil, &ClaimError{Message: "账号不存在"}
	}
	if reason := billingBlockReason(acc); reason != "" {
		return nil, &ClaimError{Message: reason}
	}
	if c.Captcha == nil || !c.Captcha.Enabled() {
		return nil, &ClaimError{Message: "领取需要验证码求解器（schedule.zai.captcha_solver 未配置）"}
	}

	// 激活上报是资格信号，失败只记日志不阻断
	if err := c.ReportActivation(ctx, id); err != nil {
		logf("zai: 账号 %s 激活上报失败：%v", acc.Name, err)
	}

	plans, err := c.PreviewPlans(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return nil, nil
	}

	out := make([]ClaimOutcome, 0, len(plans))
	for _, p := range plans {
		res, cerr := c.ClaimOne(ctx, id, p.ID, p.Name, p.Grants)
		if cerr != nil {
			var ce *ClaimError
			if asClaimError(cerr, &ce) {
				item := ClaimOutcome{PlanID: p.ID, PlanName: p.Name, Message: ce.Message, Code: ce.Code}
				if !ce.NextAt.IsZero() {
					item.NextAt = ce.NextAt.UnixMilli()
				}
				out = append(out, item)
				continue
			}
			out = append(out, ClaimOutcome{PlanID: p.ID, PlanName: p.Name, Message: cerr.Error()})
			continue
		}
		res.OK = true
		out = append(out, *res)
	}
	return out, nil
}

// ClaimOne 领取指定套餐（planID 为空则自动选优先级最高的）。
func (c *Client) ClaimOne(ctx context.Context, id, planID, planName string, grants []Grant) (*ClaimOutcome, error) {
	acc := c.Store().Get(id)
	if acc == nil {
		return nil, &ClaimError{Message: "账号不存在"}
	}
	if reason := billingBlockReason(acc); reason != "" {
		return nil, &ClaimError{Message: reason}
	}
	if c.Captcha == nil || !c.Captcha.Enabled() {
		return nil, &ClaimError{Message: "领取需要验证码求解器（schedule.zai.captcha_solver 未配置）"}
	}
	if planID == "" {
		plans, err := c.PreviewPlans(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(plans) == 0 {
			return nil, &ClaimError{Message: "没有待领取的套餐"}
		}
		planID, planName, grants = plans[0].ID, plans[0].Name, plans[0].Grants
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		verifyParam, region, err := c.Captcha.Get(ctx)
		if err != nil {
			return nil, &ClaimError{Message: "验证码求解失败：" + err.Error()}
		}
		headers := billingHeaders(acc)
		headers["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		if region != "" {
			headers["X-Aliyun-Captcha-Verify-Region"] = region
		}
		// 实测缺版本/平台头时即使验证码有效也会 3007，显式兜底
		headers["X-ZCode-App-Version"] = ClientAppVersion

		payload, _ := json.Marshal(map[string]string{"plan_id": planID})
		body, status, err := c.billingCall(ctx, http.MethodPost, claimPath, headers, payload)
		if err != nil {
			return nil, &ClaimError{Message: "上游网络错误：" + err.Error()}
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			if !containsAny(lowerString(body), []string{"captcha", "verify"}) {
				_ = c.Store().Update(id, func(a *Account) { a.Invalidate("领取被拒：凭证失效") })
				return nil, &ClaimError{Code: claimNotLoggedIn, Message: "凭证失效，请重新授权"}
			}
		}
		doc := parseJSON(body)
		code := businessCode(doc)
		if code == 0 {
			out := claimOutcome(doc, planID, planName, grants)
			out.OK = true
			return out, nil
		}
		if code == claimCaptchaFailed && attempt == 1 {
			logf("zai: 账号 %s 领取验证码被拒，换码重试", acc.Name)
			c.Captcha.Invalidate()
			lastErr = claimFailError(code, doc)
			continue
		}
		return nil, claimFailError(code, doc)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &ClaimError{Message: "领取失败"}
}

// ReportActivation 上报官方客户端激活事件（app_launch + app_daily_active）。
//
// 端点不校验登录态（无 Authorization）；日活键在上游按 device_mid+日期去重，
// 故首个失败即中止（重试无意义）。失败只影响"资格信号"，不阻断领取。
func (c *Client) ReportActivation(ctx context.Context, id string) error {
	acc := c.Store().Get(id)
	if acc == nil {
		return fmt.Errorf("账号不存在")
	}
	acc.EnsureProfile()
	userID := jwtUserID(acc.JWT)
	if userID == "" {
		return fmt.Errorf("JWT 无 user_id，跳过激活上报")
	}
	for _, element := range []string{"app_launch", "app_daily_active"} {
		payload, _ := json.Marshal(activationEventBody(element, acc.Fingerprint, userID))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, eventReportURL(), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "")
		resp, err := zaiHTTP().Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("event/report %s HTTP %d", element, resp.StatusCode)
		}
		if code := businessCode(parseJSON(raw)); code != 0 {
			return fmt.Errorf("event/report %s 业务码异常(%d)", element, code)
		}
	}
	return nil
}

// activationEventBody 激活事件体（字段集与官方 sendReport 一致，共 16 个）。
func activationEventBody(element string, p *Profile, userID string) map[string]any {
	return map[string]any{
		"event_id":            UUID(),
		"client_timezone":     p.Timezone,
		"client_language":     p.Language,
		"element_name":        element,
		"event_region":        "app",
		"event_type":          "view",
		"event_text":          "",
		"event_extra_detail":  map[string]any{},
		"user_id":             userID,
		"screen_resolution":   p.Screen,
		"app_version":         ClientAppVersion,
		"device_os_category":  p.OSCategory(),
		"device_os_version":   p.OSVersion,
		"device_mid":          p.DeviceMid,
		"mac_id":              "",
		"marketing_params":    "{}",
	}
}

// StartClaimLoop 后台周期领取轮（默认 10 分钟一轮，0 = 关闭）。
//
// 1005（今日名额用完）按服务端 next_at 退避：等待期内该套餐不再打 claim
// （省验证码求解与上游写流量），preview 照常以发现新套餐。
func (c *Client) StartClaimLoop(interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	var mu sync.Mutex
	holds := map[string]map[string]time.Time{} // 账号 → 套餐 → 名额恢复时间

	go func() {
		timer := time.NewTimer(30 * time.Second) // 启动先让位给服务初始化
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			c.runClaimRound(holds, &mu)
			timer.Reset(interval)
		}
	}()
	return func() { close(stop) }
}

func (c *Client) runClaimRound(holds map[string]map[string]time.Time, mu *sync.Mutex) {
	var ids []string
	for _, a := range c.Snapshots() {
		if a.Mode == ModeJWT && a.Enabled && a.Status != StatusInvalid && a.Status != StatusDisabled {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return
	}

	now := time.Now()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)

		mu.Lock()
		accHolds := holds[id]
		skip := map[string]bool{}
		for pid, at := range accHolds {
			if at.After(now) {
				skip[pid] = true
			} else {
				delete(accHolds, pid) // 名额恢复，本轮重试
			}
		}
		mu.Unlock()

		if len(skip) > 0 {
			logf("zai: 账号 %s 有 %d 个套餐在名额等待期，本轮跳过领取", id, len(skip))
		}

		plans, err := c.PreviewPlans(ctx, id)
		if err != nil {
			cancel()
			continue
		}
		claimed := false
		for _, p := range plans {
			if skip[p.ID] {
				continue
			}
			res, cerr := c.ClaimOne(ctx, id, p.ID, p.Name, p.Grants)
			if cerr != nil {
				var ce *ClaimError
				if asClaimError(cerr, &ce) && ce.Code == claimQuotaFull && !ce.NextAt.IsZero() {
					mu.Lock()
					if holds[id] == nil {
						holds[id] = map[string]time.Time{}
					}
					holds[id][p.ID] = ce.NextAt
					mu.Unlock()
					logf("zai: 账号 %s 套餐 %s 今日名额用完，退避至 %s", id, p.ID, ce.NextAt.Format("15:04"))
				}
				continue
			}
			if res != nil && res.OK {
				claimed = true
				logf("zai: 账号 %s 领取成功：%s", id, res.PlanName)
			}
		}
		if claimed {
			_ = c.FetchQuota(ctx, id) // 领到额度立即反映到面板
		}
		cancel()
	}
}

// ---- 内部 ----

// billingCall 发一次 billing 请求（GET/POST），返回体与状态码。
func (c *Client) billingCall(ctx context.Context, method, path string, headers map[string]string, payload []byte) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, BillingBase+path, body)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := zaiHTTP().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// billingBlockReason 该账号当前是否**不该**打 billing（返回原因文案，空 = 可以打）。
func billingBlockReason(a *Account) string {
	if a.Mode != ModeJWT || a.JWT == "" {
		return "非 Coding Plan 账号，跳过领取"
	}
	if !a.Enabled {
		return "账号已停用，跳过领取"
	}
	switch a.Status {
	case StatusDisabled:
		return "账号风控封禁，跳过领取"
	case StatusInvalid:
		return "凭证失效，请重新授权"
	case StatusCooling:
		if time.Now().Before(a.CoolingUntil) {
			return "账号冷却中，跳过领取"
		}
	}
	return ""
}

func parsePlan(m map[string]any) *Plan {
	id := str(m["plan_id"])
	if id == "" {
		id = str(m["planId"])
	}
	if id == "" {
		return nil
	}
	p := &Plan{ID: id, Name: str(m["name"]), Description: str(m["description"])}
	if v, ok := m["priority"].(float64); ok {
		p.Priority = int(v)
	}
	for _, e := range asSlice(m["entitlements"]) {
		em, ok := e.(map[string]any)
		if !ok || str(em["meter"]) != "model_usage" || str(em["unit_type"]) != "token" {
			continue
		}
		name := str(em["show_name"])
		if name == "" {
			continue
		}
		units, _ := em["grant_units"].(float64)
		period := str(em["period"])
		if period == "" {
			period = "one_time"
		}
		p.Grants = append(p.Grants, Grant{Name: name, Units: units, Period: period})
	}
	return p
}

func claimOutcome(doc map[string]any, planID, planName string, grants []Grant) *ClaimOutcome {
	data, _ := doc["data"].(map[string]any)
	plan, _ := data["plan"].(map[string]any)
	ms := func(key string) int64 {
		if v, ok := plan[key].(float64); ok && v > 0 {
			return int64(v * 1000)
		}
		return 0
	}
	out := &ClaimOutcome{
		PlanID: planID, PlanName: planName, Grants: grants,
		StartsAt: ms("starts_at"), EndsAt: ms("ends_at"),
	}
	if v, ok := data["server_time"].(float64); ok && v > 0 {
		out.ServerTime = int64(v * 1000)
	}
	return out
}

func claimFailError(code int, doc map[string]any) *ClaimError {
	messages := map[int]string{
		claimPlanNotFound:   "套餐不存在",
		claimActivityEnded:  "活动已结束或套餐暂不可领取",
		claimAlreadyClaimed: "该套餐已经领取过",
		claimNotEligible:    "不符合领取条件",
		claimQuotaFull:      "今日领取名额已用完",
		claimBadParams:      "领取参数错误，请刷新后重试",
		claimCaptchaFailed:  "验证码校验失败，请重试",
		claimNotLoggedIn:    "请先登录后再领取",
	}
	base := messages[code]
	if base == "" {
		base = "领取失败"
	}
	server := str(doc["msg"])
	if server == "" {
		server = str(doc["message"])
	}
	msg := base
	if server != "" {
		msg = base + "（" + server + "）"
	}
	err := &ClaimError{Code: code, Message: msg}
	if code == claimQuotaFull {
		if data, ok := doc["data"].(map[string]any); ok {
			if plan, ok := data["plan"].(map[string]any); ok {
				if v, ok := plan["ends_at"].(float64); ok && v > 0 {
					err.NextAt = time.UnixMilli(int64(v * 1000))
				}
			}
		}
	}
	return err
}

func businessCode(doc map[string]any) int {
	v, ok := doc["code"]
	if !ok {
		return -1
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i
		}
	}
	return -1
}

func parseJSON(b []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return map[string]any{}
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asClaimError(err error, out **ClaimError) bool {
	if ce, ok := err.(*ClaimError); ok {
		*out = ce
		return true
	}
	return false
}
