// Package alert: dingtalk.go — 钉钉告警（阶段 5）。
//
// 移植自 9router-qoder-plus src/shared/services/modelIdleAlert.js（427 行全文）：
//   - 闲时告警：自最近一次成功模型调用起算 Idle Minutes，超阈值且过冷却即发；
//   - 用量/耗尽告警：API Key 维度（耗尽、阈值百分比），按 Key 独立冷却；
//   - 加签：HMAC-SHA256(timestamp\nsecret) → base64 → urlencode，拼进 webhook
//     query（signDingTalkWebhook）；secret 在设置响应里 write-only；
//   - 模板：{idleMinutes} / {lastCallAt} / {now} 三占位符，locale 双语文案；
//   - 按账号冷却：额度读取失败告警每账号 30 分钟冷却（fork L20）。
//
// buddyhub 侧差异：
//   - fork 的 global 单例 + localDb 设置 → 这里纯函数 + 显式 state/settings
//     传入（scheduler 持状态、panel 管配置），无全局可变状态；
//   - 发送走注入的 httpDo（测试替身、面板侧统一代理/超时策略）；
//   - 时间戳一律 Asia/Shanghai（fork formatTime 硬编码时区）。
package alert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── 默认值（fork 顶部常量）──

const (
	minuteMS              = 60 * 1000
	defaultCheckInterval  = 30 * time.Second
	defaultIdleMinutes    = 5
	defaultCooldownMin    = 5
	defaultQuotaPct       = 80
	usageFetchCooldownMin = 30 // fork：额度读取失败告警按账号冷却 30 分钟
)

// defaultTemplate fork DEFAULT_TEMPLATE 原文。
const defaultTemplate = "[9router] No successful model call for {idleMinutes} minutes. Last call: {lastCallAt}."

// Settings 告警配置（面板可改；secret 只进不出发送链路）。
type Settings struct {
	Enabled                bool
	IdleMinutes            int
	CooldownMinutes        int
	DingTalkWebhook        string
	DingTalkSecret         string
	QuotaUsageThresholdOn  bool
	QuotaUsageThresholdPct int
	Locale                 string
	MessageTemplate        string
}

// Normalize 归一化：非法值回落默认（fork normalizeModelIdleAlertSettings）。
// 返回新值，不修改入参（secret/webhook 原样保留 trim 结果）。
//
// CooldownMinutes 的 0 是合法语义（不冷却，fork nonNegativeNumber 口径），
// 只有负值回落默认；IdleMinutes 走 positive（fork positiveNumber）。
func Normalize(in Settings) Settings {
	out := in
	out.Enabled = in.Enabled
	out.IdleMinutes = positiveInt(in.IdleMinutes, defaultIdleMinutes)
	if in.CooldownMinutes < 0 {
		out.CooldownMinutes = defaultCooldownMin
	} else {
		out.CooldownMinutes = in.CooldownMinutes
	}
	out.DingTalkWebhook = strings.TrimSpace(in.DingTalkWebhook)
	out.DingTalkSecret = strings.TrimSpace(in.DingTalkSecret)
	out.QuotaUsageThresholdPct = boundedPct(in.QuotaUsageThresholdPct, defaultQuotaPct)
	if strings.TrimSpace(in.Locale) != "" {
		out.Locale = strings.TrimSpace(in.Locale)
	} else {
		out.Locale = "en"
	}
	if strings.TrimSpace(in.MessageTemplate) != "" {
		out.MessageTemplate = strings.TrimSpace(in.MessageTemplate)
	} else {
		out.MessageTemplate = defaultTemplate
	}
	return out
}

func positiveInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func boundedPct(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > 100 {
		return 100
	}
	if v < 1 {
		return 1
	}
	return v
}

// Masked 面板回显用：secret 抹成空 + hasSecret 标记（fork maskModelIdleAlertSettings）。
type Masked struct {
	Settings
	HasDingTalkSecret bool
}

// Mask 脱敏副本（深拷贝浅拷即可：全是标量字段）。
func Mask(s Settings) Masked {
	n := Normalize(s)
	m := Masked{Settings: n}
	if n.DingTalkSecret != "" {
		m.DingTalkSecret = ""
		m.HasDingTalkSecret = true
	}
	return m
}

// ── 状态（scheduler 持有）──

// IdleState 闲时告警状态（fork createModelIdleAlertState）。
type IdleState struct {
	LastCallAt        int64 // ms；0 = 从未调用
	LastAlertAt       int64 // ms；0 = 从未告警
	LastAlertedCallAt int64 // ms；已告过警的那次调用时间戳
}

// RecordCall 记录一次成功模型调用（fork recordModelCall）。
func (s *IdleState) RecordCall(nowMS int64) {
	s.LastCallAt = nowMS
	s.LastAlertedCallAt = 0
}

// IdleVerdict 闲时判定结果。
type IdleVerdict struct {
	ShouldAlert bool
	Reason      string
	IdleMS      int64
}

// EvaluateIdle 闲时告警判定（fork evaluateModelIdleAlert）。
func EvaluateIdle(st *IdleState, s Settings, nowMS int64) IdleVerdict {
	n := Normalize(s)
	if !n.Enabled {
		return IdleVerdict{Reason: "disabled"}
	}
	if n.DingTalkWebhook == "" {
		return IdleVerdict{Reason: "missing-webhook"}
	}
	if st == nil || st.LastCallAt == 0 {
		return IdleVerdict{Reason: "no-call"}
	}
	if st.LastAlertedCallAt == st.LastCallAt {
		return IdleVerdict{Reason: "already-alerted"}
	}
	idle := nowMS - st.LastCallAt
	if idle < int64(n.IdleMinutes)*minuteMS {
		return IdleVerdict{Reason: "below-idle-threshold", IdleMS: idle}
	}
	if st.LastAlertAt != 0 && n.CooldownMinutes > 0 &&
		nowMS-st.LastAlertAt < int64(n.CooldownMinutes)*minuteMS {
		return IdleVerdict{Reason: "cooldown", IdleMS: idle}
	}
	return IdleVerdict{ShouldAlert: true, Reason: "idle", IdleMS: idle}
}

// KeyQuotaState Key 维度告警状态（fork createApiKeyQuotaAlertState）。
type KeyQuotaState struct {
	LastAlertByKey          map[string]int64
	LastThresholdAlertByKey map[string]int64
	ActiveExhaustedKeys     map[string]bool
	UsageFetchAlertAt       map[string]int64
}

// NewKeyQuotaState 建空状态。
func NewKeyQuotaState() *KeyQuotaState {
	return &KeyQuotaState{
		LastAlertByKey:          map[string]int64{},
		LastThresholdAlertByKey: map[string]int64{},
		ActiveExhaustedKeys:     map[string]bool{},
		UsageFetchAlertAt:       map[string]int64{},
	}
}

// QuotaVerdict Key 配额告警判定结果。
type QuotaVerdict struct {
	ShouldAlert  bool
	Reason       string
	UsagePct     int
	ThresholdPct int
}

// EvaluateQuotaExhausted Key 配额耗尽判定（fork evaluateApiKeyQuotaAlert）。
func EvaluateQuotaExhausted(st *KeyQuotaState, s Settings, keyID string, nowMS int64) QuotaVerdict {
	n := Normalize(s)
	if !n.Enabled {
		return QuotaVerdict{Reason: "disabled"}
	}
	if n.DingTalkWebhook == "" {
		return QuotaVerdict{Reason: "missing-webhook"}
	}
	if keyID == "" {
		return QuotaVerdict{Reason: "missing-key"}
	}
	if st.ActiveExhaustedKeys[keyID] {
		return QuotaVerdict{Reason: "already-alerted"}
	}
	if last, ok := st.LastAlertByKey[keyID]; ok && last != 0 &&
		n.CooldownMinutes > 0 && nowMS-last < int64(n.CooldownMinutes)*minuteMS {
		return QuotaVerdict{Reason: "cooldown"}
	}
	return QuotaVerdict{ShouldAlert: true, Reason: "quota-exhausted", ThresholdPct: n.QuotaUsageThresholdPct}
}

// MarkQuotaExhausted 标记 key 已耗尽告警（发送成功后调用）。
func (st *KeyQuotaState) MarkQuotaExhausted(keyID string, nowMS int64) {
	st.ActiveExhaustedKeys[keyID] = true
	st.LastAlertByKey[keyID] = nowMS
}

// MarkQuotaRecovered key 恢复可用（fork markApiKeyQuotaAvailable）。
func (st *KeyQuotaState) MarkQuotaRecovered(keyID string) {
	delete(st.ActiveExhaustedKeys, keyID)
	delete(st.LastAlertByKey, keyID)
}

// ClearKey 清除该 key 全部告警状态（fork clearApiKeyQuotaAlertStateForKey）。
func (st *KeyQuotaState) ClearKey(keyID string) {
	delete(st.ActiveExhaustedKeys, keyID)
	delete(st.LastAlertByKey, keyID)
	delete(st.LastThresholdAlertByKey, keyID)
}

// QuotaDetails 阈值判定输入。
type QuotaDetails struct {
	KeyID   string
	KeyName string
	Used    float64
	Limit   float64
}

// EvaluateQuotaThreshold 用量阈值判定（fork evaluateApiKeyQuotaThresholdAlert）。
func EvaluateQuotaThreshold(st *KeyQuotaState, s Settings, d QuotaDetails, nowMS int64) QuotaVerdict {
	n := Normalize(s)
	if !n.Enabled {
		return QuotaVerdict{Reason: "disabled"}
	}
	if !n.QuotaUsageThresholdOn {
		return QuotaVerdict{Reason: "threshold-disabled"}
	}
	if n.DingTalkWebhook == "" {
		return QuotaVerdict{Reason: "missing-webhook"}
	}
	keyID := d.KeyID
	if keyID == "" {
		keyID = d.KeyName
	}
	if keyID == "" {
		return QuotaVerdict{Reason: "missing-key"}
	}
	if d.Limit <= 0 {
		return QuotaVerdict{Reason: "missing-limit"}
	}
	used := d.Used
	if used < 0 {
		used = 0
	}
	pct := int(used / d.Limit * 100)
	if pct < n.QuotaUsageThresholdPct {
		return QuotaVerdict{Reason: "below-threshold", UsagePct: pct, ThresholdPct: n.QuotaUsageThresholdPct}
	}
	if last, ok := st.LastThresholdAlertByKey[keyID]; ok && last != 0 &&
		n.CooldownMinutes > 0 && nowMS-last < int64(n.CooldownMinutes)*minuteMS {
		return QuotaVerdict{Reason: "cooldown", UsagePct: pct, ThresholdPct: n.QuotaUsageThresholdPct}
	}
	return QuotaVerdict{ShouldAlert: true, Reason: "quota-threshold", UsagePct: pct, ThresholdPct: n.QuotaUsageThresholdPct}
}

// MarkQuotaThreshold 记录阈值告警已发。
func (st *KeyQuotaState) MarkQuotaThreshold(keyID string, nowMS int64) {
	st.LastThresholdAlertByKey[keyID] = nowMS
}

// EvaluateUsageFetchFailed 额度读取失败告警判定（fork notifyUsageFetchFailed）：
// 每账号 30 分钟冷却（与 Key 冷却独立）。
func EvaluateUsageFetchFailed(st *KeyQuotaState, accountID string, nowMS int64) bool {
	if accountID == "" {
		return false
	}
	if last, ok := st.UsageFetchAlertAt[accountID]; ok && last != 0 &&
		nowMS-last < usageFetchCooldownMin*minuteMS {
		return false
	}
	return true
}

// MarkUsageFetchFailed 记录读取失败告警已发。
func (st *KeyQuotaState) MarkUsageFetchFailed(accountID string, nowMS int64) {
	st.UsageFetchAlertAt[accountID] = nowMS
}

// ── 文案构造 ──

// BuildIdleText 闲时告警文案（fork buildDingTalkText）：三占位符替换。
func BuildIdleText(s Settings, st *IdleState, nowMS int64) string {
	n := Normalize(s)
	idleMinutes := 0
	lastCall := "unknown"
	if st != nil && st.LastCallAt != 0 {
		idleMinutes = int((nowMS - st.LastCallAt) / minuteMS)
		lastCall = formatTime(st.LastCallAt)
	}
	out := n.MessageTemplate
	out = strings.ReplaceAll(out, "{idleMinutes}", fmt.Sprint(idleMinutes))
	out = strings.ReplaceAll(out, "{lastCallAt}", lastCall)
	out = strings.ReplaceAll(out, "{now}", formatTime(nowMS))
	return out
}

// QuotaTextDetails 配额类告警文案输入。
type QuotaTextDetails struct {
	KeyName    string
	Provider   string
	Used       float64
	Limit      float64
	Remaining  float64
	ThresholdP int
	UsagePct   int
}

// BuildQuotaExhaustedText 耗尽文案（fork buildApiKeyQuotaExhaustedText，中英双语）。
func BuildQuotaExhaustedText(s Settings, d QuotaTextDetails, nowMS int64) string {
	if isZH(s.Locale) {
		return fmt.Sprintf("[9router] API Key 分配额度已耗尽。Key：%s。提供商：%s。用量：%s/%s。剩余：%s。时间：%s。",
			orUnknown(d.KeyName), orUnknown(d.Provider),
			trimNum(d.Used), trimNum(d.Limit), trimNum(d.Remaining), formatTime(nowMS))
	}
	return fmt.Sprintf("[9router] API key quota exhausted. Key: %s. Provider: %s. Usage: %s/%s. Remaining: %s. Time: %s.",
		orUnknown(d.KeyName), orUnknown(d.Provider),
		trimNum(d.Used), trimNum(d.Limit), trimNum(d.Remaining), formatTime(nowMS))
}

// BuildQuotaThresholdText 阈值文案（fork buildApiKeyQuotaThresholdText）。
func BuildQuotaThresholdText(s Settings, d QuotaTextDetails, nowMS int64) string {
	usage := d.UsagePct
	if usage == 0 && d.Limit > 0 {
		usage = int(d.Used / d.Limit * 100)
	}
	threshold := d.ThresholdP
	if threshold == 0 {
		threshold = defaultQuotaPct
	}
	if isZH(s.Locale) {
		return fmt.Sprintf("[9router] API Key 使用量已达到阈值。Key：%s。提供商：%s。使用率：%d%%。阈值：%d%%。用量：%s/%s。剩余：%s。时间：%s。",
			orUnknown(d.KeyName), orUnknown(d.Provider), usage, threshold,
			trimNum(d.Used), trimNum(d.Limit), trimNum(d.Remaining), formatTime(nowMS))
	}
	return fmt.Sprintf("[9router] API key usage reached threshold. Key: %s. Provider: %s. Usage rate: %d%%. Threshold: %d%%. Usage: %s/%s. Remaining: %s. Time: %s.",
		orUnknown(d.KeyName), orUnknown(d.Provider), usage, threshold,
		trimNum(d.Used), trimNum(d.Limit), trimNum(d.Remaining), formatTime(nowMS))
}

// UsageFetchTextDetails 额度读取失败文案输入。
type UsageFetchTextDetails struct {
	KeyName     string
	Provider    string
	AccountName string
	Err         string
}

// BuildUsageFetchFailedText 读取失败文案（fork buildUsageFetchFailedText）。
func BuildUsageFetchFailedText(s Settings, d UsageFetchTextDetails, nowMS int64) string {
	reason := d.Err
	if len(reason) > 200 {
		reason = reason[:200]
	}
	if isZH(s.Locale) {
		return fmt.Sprintf("[9router] 账号额度读取失败，该账号已被临时跳过额度校验。Key：%s。提供商：%s。账号：%s。原因：%s。时间：%s。若持续出现请重新授权该账号。",
			orUnknown(d.KeyName), orUnknown(d.Provider), orUnknown(d.AccountName), orUnknown(reason), formatTime(nowMS))
	}
	return fmt.Sprintf("[9router] Failed to read account quota; quota enforcement for this account is temporarily skipped. Key: %s. Provider: %s. Account: %s. Reason: %s. Time: %s. Re-authenticate the account if this persists.",
		orUnknown(d.KeyName), orUnknown(d.Provider), orUnknown(d.AccountName), orUnknown(reason), formatTime(nowMS))
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// trimNum 数字去尾零（fork Number() 序列化口径：123.0 → "123"）。
func trimNum(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprint(int64(f))
	}
	return fmt.Sprintf("%g", f)
}

func isZH(locale string) bool {
	return locale == "zh-CN" || locale == "zh"
}

// formatTime fork formatTime：Asia/Shanghai，24 小时制。
func formatTime(ms int64) string {
	t := time.UnixMilli(ms).In(shanghai)
	return t.Format("2006-01-02 15:04:05")
}

// shanghai 固定时区（fork 硬编码 Asia/Shanghai）；加载失败回落 UTC。
var shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// ── 发送 ──

// HTTPDo 发送依赖（测试替身；生产传 http.DefaultClient.Do）。
type HTTPDo func(req *http.Request) (*http.Response, error)

// SignWebhook 加签（fork signDingTalkWebhook）：
// HMAC-SHA256(secret, "timestamp\nsecret") → base64 → 进 webhook query。
// 无 secret 原样返回。
//
// 编码只做一次：base64 串直接进 url.Values 交给 Encode()（+ → %2B、= → %3D）。
// 若先 QueryEscape 再 Encode 会把 % 二次编码成 %25，钉钉侧解出乱码签名。
func SignWebhook(webhook, secret string, timestamp int64) (string, error) {
	if secret == "" {
		return webhook, nil
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d\n%s", timestamp, secret)))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	u, err := url.Parse(webhook)
	if err != nil {
		return "", fmt.Errorf("webhook 解析失败: %w", err)
	}
	q := u.Query()
	q.Set("timestamp", fmt.Sprint(timestamp))
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// SendDingTalk 发一条文本告警（fork sendDingTalkAlert）：
// 非 2xx 或 errcode≠0 都算失败（调用方据此后退冷却状态）。
func SendDingTalk(do HTTPDo, s Settings, text string) error {
	n := Normalize(s)
	if n.DingTalkWebhook == "" {
		return fmt.Errorf("DingTalk webhook is required")
	}
	signed, err := SignWebhook(n.DingTalkWebhook, n.DingTalkSecret, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, signed, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("DingTalk HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(body) > 0 {
		var parsed struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if json.Unmarshal(body, &parsed) == nil && parsed.ErrCode != 0 {
			if parsed.ErrMsg != "" {
				return fmt.Errorf("DingTalk errcode %d: %s", parsed.ErrCode, parsed.ErrMsg)
			}
			return fmt.Errorf("DingTalk errcode %d", parsed.ErrCode)
		}
	}
	return nil
}
