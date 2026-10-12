// Package dingtalk — 阶段 5：钉钉告警。
//
// 移植自 9router-qoder-plus（HEAD 290c68c）src/shared/services/modelIdleAlert.js
// （427 行全文）：
//   - 加签（signDingTalkWebhook L238-248）：HMAC-SHA256(timestamp\nsecret)
//     base64 后 URL 编码，拼 timestamp+sign  query 参数——**防重放/防伪造**，
//     没有加签的 webhook 任何人拿到 URL 就能推消息，是轰炸的入口；
//   - 独立冷却（两套）：
//     · 全局告警冷却（idle/exhausted/threshold 共用 cooldownMinutes，默认 5 分钟）；
//     · usage-fetch 失败按账号独立冷却（DEFAULT_USAGE_FETCH_ALERT_COOLDOWN_MINUTES
//     =30 分钟，L20/L408）——一个坏账号每小时最多吵 2 次；
//   - 5 类告警模板（闲时/耗尽/阈值/用量读取失败 + 通用 text 模板占位符替换）：
//     · buildDingTalkText（L188-198）：{idleMinutes}/{lastCallAt}/{now} 占位符；
//     · buildApiKeyQuotaExhaustedText（L200-210）与 ThresholdText（L212-236）：
//     中英双语文案（locale zh-CN/zh → 中文）；
//     · buildUsageFetchFailedText（L386-395）：账号额度读取失败；
//   - 抑制语义（防同一事件反复轰炸）：
//     · lastAlertedCallAt === lastCallAt → 已报过不再报（L94-96）；
//     · activeExhaustedKeys：耗尽态持续期间只报一次，恢复
//     （markApiKeyQuotaAvailable）后才可再报（L121-123/L135-139）；
//     · 发送失败回滚 activeExhaustedKeys（L346-348）——下次可重试。
//   - sendDingTalkAlert（L250-277）：msgtype=text；HTTP 非 2xx 或
//     errcode≠0 都抛错（调用方记日志，不中断主链路）。
//
// buddyhub 侧差异：fork 的 state 是模块级单例（global.__modelIdleAlert），
// Go 侧用显式 Notifier 结构体（可多个实例/测试隔离）；定时检查循环由
// 调用方（server 层的 watchdog goroutine）驱动，本包只提供
// Evaluate/Mark/Send 原语。
package dingtalk

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 默认参数（fork 文件头常量）。
const (
	defaultIdleMinutes       = 5
	defaultCooldownMinutes   = 5
	defaultQuotaThresholdPct = 80
	// usageFetchCooldownMinutes 账号额度读取失败告警的独立冷却（fork L20）。
	usageFetchCooldownMinutes = 30
	defaultTemplate           = "[buddyhub] No successful model call for {idleMinutes} minutes. Last call: {lastCallAt}."
)

// Settings 告警配置（fork normalizeModelIdleAlertSettings 输出形状）。
type Settings struct {
	Enabled                   bool
	IdleMinutes               int
	CooldownMinutes           int
	DingTalkWebhook           string
	DingTalkSecret            string
	QuotaUsageThresholdEnable bool
	QuotaUsageThresholdPct    int
	Locale                    string // "en" | "zh-CN" | "zh"
	MessageTemplate           string
}

// NormalizeSettings 归一化配置（fork normalizeModelIdleAlertSettings L54-72）：
// 空值/非法值回落默认；secret 单独持有（面板回显一律脱敏）。
func NormalizeSettings(in Settings) Settings {
	out := in
	if out.IdleMinutes <= 0 {
		out.IdleMinutes = defaultIdleMinutes
	}
	if out.CooldownMinutes < 0 {
		out.CooldownMinutes = 0
	}
	out.DingTalkWebhook = strings.TrimSpace(out.DingTalkWebhook)
	out.DingTalkSecret = strings.TrimSpace(out.DingTalkSecret)
	if out.QuotaUsageThresholdPct <= 0 || out.QuotaUsageThresholdPct > 100 {
		out.QuotaUsageThresholdPct = defaultQuotaThresholdPct
	}
	if strings.TrimSpace(out.Locale) == "" {
		out.Locale = "en"
	}
	if strings.TrimSpace(out.MessageTemplate) == "" {
		out.MessageTemplate = defaultTemplate
	}
	return out
}

// MaskedSettings 面板回显用：secret 抹成空串，只留 hasSecret 标志
// （fork maskModelIdleAlertSettings L74-81）。
type MaskedSettings struct {
	Settings
	DingTalkSecret string `json:"dingtalk_secret"`
	HasDingTalkSec bool   `json:"has_dingtalk_secret"`
}

// Mask 脱敏副本。
func (s Settings) Mask() MaskedSettings {
	return MaskedSettings{Settings: s, DingTalkSecret: "", HasDingTalkSec: s.DingTalkSecret != ""}
}

// ── 闲时告警状态 ──

// IdleState 闲时告警状态（fork createModelIdleAlertState L22-28）。
type IdleState struct {
	LastCallAt        time.Time
	LastAlertAt       time.Time
	LastAlertedCallAt time.Time
}

// RecordCall 记一次成功调用（fork recordModelCall L83-87）：
// 同时清 lastAlertedCallAt——新的一次调用应该可再次触发闲时告警。
func (s *IdleState) RecordCall(now time.Time) {
	s.LastCallAt = now
	s.LastAlertedCallAt = time.Time{}
}

// IdleEval 闲时判定结果（fork evaluateModelIdleAlert 返回形状）。
type IdleEval struct {
	ShouldAlert bool
	Reason      string // disabled|missing-webhook|no-call|already-alerted|below-idle-threshold|cooldown|idle
	IdleFor     time.Duration
	Now         time.Time
}

// EvaluateIdle 闲时判定（fork L89-112 逐分支对齐）。
func (s *IdleState) EvaluateIdle(set Settings, now time.Time) IdleEval {
	if !set.Enabled {
		return IdleEval{Reason: "disabled", Now: now}
	}
	if set.DingTalkWebhook == "" {
		return IdleEval{Reason: "missing-webhook", Now: now}
	}
	if s.LastCallAt.IsZero() {
		return IdleEval{Reason: "no-call", Now: now}
	}
	// 已报过这一次调用（同一次 lastCallAt）→ 不重复报。
	if !s.LastAlertedCallAt.IsZero() && s.LastAlertedCallAt.Equal(s.LastCallAt) {
		return IdleEval{Reason: "already-alerted", Now: now}
	}
	idle := now.Sub(s.LastCallAt)
	if idle < time.Duration(set.IdleMinutes)*time.Minute {
		return IdleEval{Reason: "below-idle-threshold", IdleFor: idle, Now: now}
	}
	if !s.LastAlertAt.IsZero() && set.CooldownMinutes > 0 &&
		now.Sub(s.LastAlertAt) < time.Duration(set.CooldownMinutes)*time.Minute {
		return IdleEval{Reason: "cooldown", IdleFor: idle, Now: now}
	}
	return IdleEval{ShouldAlert: true, Reason: "idle", IdleFor: idle, Now: now}
}

// MarkIdleAlerted 标记已报（fork L327-328）。
func (s *IdleState) MarkIdleAlerted(now time.Time) {
	s.LastAlertAt = now
	s.LastAlertedCallAt = s.LastCallAt
}

// ── Key 配额告警状态（耗尽 + 阈值，双 Map 冷却 + 活跃耗尽集）──

// KeyQuotaState Key 配额告警状态（fork createApiKeyQuotaAlertState L30-36）。
type KeyQuotaState struct {
	LastAlertByKey          map[string]time.Time
	LastThresholdAlertByKey map[string]time.Time
	ActiveExhaustedKeys     map[string]bool
}

// NewKeyQuotaState 建空状态。
func NewKeyQuotaState() *KeyQuotaState {
	return &KeyQuotaState{
		LastAlertByKey:          map[string]time.Time{},
		LastThresholdAlertByKey: map[string]time.Time{},
		ActiveExhaustedKeys:     map[string]bool{},
	}
}

// QuotaEval 配额告警判定结果（fork evaluateApiKeyQuotaAlert L114-133 形状）。
type QuotaEval struct {
	ShouldAlert bool
	Reason      string // disabled|missing-webhook|missing-key|already-alerted|cooldown|quota-exhausted
	KeyID       string
	LastAlertAt time.Time
	Now         time.Time
}

// EvaluateExhausted 耗尽告警判定（fork L114-133）：
// 同一 Key 在耗尽态持续期间只报一次（activeExhaustedKeys），恢复后才解禁。
func (s *KeyQuotaState) EvaluateExhausted(set Settings, keyID string, now time.Time) QuotaEval {
	if !set.Enabled {
		return QuotaEval{Reason: "disabled", Now: now}
	}
	if set.DingTalkWebhook == "" {
		return QuotaEval{Reason: "missing-webhook", Now: now}
	}
	if keyID == "" {
		return QuotaEval{Reason: "missing-key", Now: now}
	}
	last := s.LastAlertByKey[keyID]
	if s.ActiveExhaustedKeys[keyID] {
		return QuotaEval{Reason: "already-alerted", KeyID: keyID, LastAlertAt: last, Now: now}
	}
	if !last.IsZero() && set.CooldownMinutes > 0 &&
		now.Sub(last) < time.Duration(set.CooldownMinutes)*time.Minute {
		return QuotaEval{Reason: "cooldown", KeyID: keyID, LastAlertAt: last, Now: now}
	}
	return QuotaEval{ShouldAlert: true, Reason: "quota-exhausted", KeyID: keyID, LastAlertAt: last, Now: now}
}

// MarkExhaustedAlerted 标记已报（进活跃耗尽集 + 记冷却时间）。
func (s *KeyQuotaState) MarkExhaustedAlerted(keyID string, now time.Time) {
	s.ActiveExhaustedKeys[keyID] = true
	s.LastAlertByKey[keyID] = now
}

// RollbackExhausted 发送失败回滚（fork L346-348：退出活跃态，下次可重试）。
func (s *KeyQuotaState) RollbackExhausted(keyID string) {
	delete(s.ActiveExhaustedKeys, keyID)
}

// MarkQuotaAvailable 恢复解禁（fork markApiKeyQuotaAvailable L135-139：
// 清活跃态 + 清冷却——恢复后的首次超限要能立刻报）。
func (s *KeyQuotaState) MarkQuotaAvailable(keyID string) {
	delete(s.ActiveExhaustedKeys, keyID)
	delete(s.LastAlertByKey, keyID)
}

// ClearKey 全清某 Key 的告警状态（fork clearApiKeyQuotaAlertStateForKey L141-146）。
func (s *KeyQuotaState) ClearKey(keyID string) {
	delete(s.ActiveExhaustedKeys, keyID)
	delete(s.LastAlertByKey, keyID)
	delete(s.LastThresholdAlertByKey, keyID)
}

// ThresholdEval 阈值告警判定（fork evaluateApiKeyQuotaThresholdAlert L148-179）。
type ThresholdEval struct {
	ShouldAlert  bool
	Reason       string
	KeyID        string
	Used         float64
	Limit        float64
	UsagePercent int
	ThresholdPct int
	LastAlertAt  time.Time
	Now          time.Time
}

// EvaluateThreshold 使用率阈值告警判定（fork L148-179）：
// used/limit ≥ thresholdPct 且过冷却才报；limit≤0（无分配上限）不报。
func (s *KeyQuotaState) EvaluateThreshold(set Settings, keyID string, used, limit float64, now time.Time) ThresholdEval {
	base := ThresholdEval{KeyID: keyID, Used: used, Limit: limit, Now: now}
	if !set.Enabled || !set.QuotaUsageThresholdEnable {
		base.Reason = "threshold-disabled"
		return base
	}
	if set.DingTalkWebhook == "" {
		base.Reason = "missing-webhook"
		return base
	}
	if keyID == "" {
		base.Reason = "missing-key"
		return base
	}
	if limit <= 0 {
		base.Reason = "missing-limit"
		return base
	}
	pct := int(used / limit * 100)
	base.UsagePercent = pct
	base.ThresholdPct = set.QuotaUsageThresholdPct
	if pct < set.QuotaUsageThresholdPct {
		base.Reason = "below-threshold"
		return base
	}
	last := s.LastThresholdAlertByKey[keyID]
	if !last.IsZero() && set.CooldownMinutes > 0 &&
		now.Sub(last) < time.Duration(set.CooldownMinutes)*time.Minute {
		base.Reason = "cooldown"
		base.LastAlertAt = last
		return base
	}
	base.ShouldAlert = true
	base.Reason = "quota-threshold"
	return base
}

// MarkThresholdAlerted 标记已报。
func (s *KeyQuotaState) MarkThresholdAlerted(keyID string, now time.Time) {
	s.LastThresholdAlertByKey[keyID] = now
}

// ── 账号级用量读取失败告警（独立 30 分钟冷却）──

// UsageFetchState 用量读取失败告警状态（fork g.usageFetchAlertAt）。
type UsageFetchState struct {
	mu        sync.Mutex
	lastAlert map[string]time.Time
}

// NewUsageFetchState 建空状态。
func NewUsageFetchState() *UsageFetchState {
	return &UsageFetchState{lastAlert: map[string]time.Time{}}
}

// UsageFetchEval 判定结果（fork notifyUsageFetchFailed 返回形状）。
type UsageFetchEval struct {
	ShouldAlert bool
	Reason      string // disabled|cooldown
	AccountID   string
	LastAlertAt time.Time
	Now         time.Time
}

// EvaluateUsageFetchFailed（fork L402-417）：按账号独立 30 分钟冷却——
// 一个坏账号每小时最多吵 2 次，不占全局冷却额度。
func (s *UsageFetchState) EvaluateUsageFetchFailed(set Settings, accountID string, now time.Time) UsageFetchEval {
	if !set.Enabled || set.DingTalkWebhook == "" {
		return UsageFetchEval{Reason: "disabled", Now: now}
	}
	if accountID == "" {
		accountID = "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.lastAlert[accountID]
	cooldown := time.Duration(usageFetchCooldownMinutes) * time.Minute
	if !last.IsZero() && now.Sub(last) < cooldown {
		return UsageFetchEval{Reason: "cooldown", AccountID: accountID, LastAlertAt: last, Now: now}
	}
	return UsageFetchEval{ShouldAlert: true, AccountID: accountID, Now: now}
}

// MarkUsageFetchAlerted 标记已报。
func (s *UsageFetchState) MarkUsageFetchAlerted(accountID string, now time.Time) {
	if accountID == "" {
		accountID = "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAlert[accountID] = now
}

// ── 文案构造（fork buildXxxText）──

// BuildIdleText 闲时告警文案（fork buildDingTalkText L188-198）：
// messageTemplate 占位符替换 {idleMinutes}/{lastCallAt}/{now}。
func BuildIdleText(set Settings, st *IdleState, now time.Time) string {
	idleMinutes := 0
	if st != nil && !st.LastCallAt.IsZero() {
		idleMinutes = int(now.Sub(st.LastCallAt).Minutes())
	}
	lastCall := "unknown"
	if st != nil && !st.LastCallAt.IsZero() {
		lastCall = formatCN(st.LastCallAt)
	}
	out := set.MessageTemplate
	out = strings.ReplaceAll(out, "{idleMinutes}", strconv.Itoa(idleMinutes))
	out = strings.ReplaceAll(out, "{lastCallAt}", lastCall)
	out = strings.ReplaceAll(out, "{now}", formatCN(now))
	return out
}

// QuotaDetails 配额告警详情（fork details 参数）。
type QuotaDetails struct {
	KeyName   string
	Provider  string
	Used      float64
	Limit     float64
	Remaining float64
}

// BuildExhaustedText 耗尽文案（fork buildApiKeyQuotaExhaustedText L200-210，中英双语）。
func BuildExhaustedText(d QuotaDetails, locale string, now time.Time) string {
	name, prov := orUnknown(d.KeyName), orUnknown(d.Provider)
	if isZh(locale) {
		return fmt.Sprintf("[buddyhub] API Key 分配额度已耗尽。Key：%s。提供商：%s。用量：%v/%v。剩余：%v。时间：%s。",
			name, prov, d.Used, d.Limit, d.Remaining, formatCN(now))
	}
	return fmt.Sprintf("[buddyhub] API key quota exhausted. Key: %s. Provider: %s. Usage: %v/%v. Remaining: %v. Time: %s.",
		name, prov, d.Used, d.Limit, d.Remaining, formatCN(now))
}

// BuildThresholdText 阈值文案（fork buildApiKeyQuotaThresholdText L212-236，中英双语）。
func BuildThresholdText(d QuotaDetails, thresholdPct, usagePct int, locale string, now time.Time) string {
	name, prov := orUnknown(d.KeyName), orUnknown(d.Provider)
	if isZh(locale) {
		return fmt.Sprintf("[buddyhub] API Key 使用量已达到阈值。Key：%s。提供商：%s。使用率：%d%%。阈值：%d%%。用量：%v/%v。剩余：%v。时间：%s。",
			name, prov, usagePct, thresholdPct, d.Used, d.Limit, d.Remaining, formatCN(now))
	}
	return fmt.Sprintf("[buddyhub] API key usage reached threshold. Key: %s. Provider: %s. Usage rate: %d%%. Threshold: %d%%. Usage: %v/%v. Remaining: %v. Time: %s.",
		name, prov, usagePct, thresholdPct, d.Used, d.Limit, d.Remaining, formatCN(now))
}

// BuildUsageFetchFailedText 用量读取失败文案（fork L386-395，中英双语）。
func BuildUsageFetchFailedText(keyName, provider, accountName, errText, locale string, now time.Time) string {
	name, prov, acct := orUnknown(keyName), orUnknown(provider), orUnknown(accountName)
	reason := errText
	if len(reason) > 200 {
		reason = reason[:200]
	}
	if isZh(locale) {
		return fmt.Sprintf("[buddyhub] 账号额度读取失败，该账号已被临时跳过额度校验。Key：%s。提供商：%s。账号：%s。原因：%s。时间：%s。若持续出现请重新授权该账号。",
			name, prov, acct, reason, formatCN(now))
	}
	return fmt.Sprintf("[buddyhub] Failed to read account quota; quota enforcement for this account is temporarily skipped. Key: %s. Provider: %s. Account: %s. Reason: %s. Time: %s. Re-authenticate the account if this persists.",
		name, prov, acct, reason, formatCN(now))
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func isZh(locale string) bool { return locale == "zh-CN" || locale == "zh" }

// formatCN 固定 Asia/Shanghai 时区格式化（fork formatTime L181-186）。
func formatCN(t time.Time) string {
	return t.In(cnLoc).Format("2006-01-02 15:04:05")
}

var cnLoc = time.FixedZone("CST", 8*3600)

// ── 发送（加签 + HTTP）──

// SignWebhook 加签（fork signDingTalkWebhook L238-248）：
// sign = urlencode(base64(hmacSHA256(secret, "timestamp\nsecret")))，
// webhook 追加 timestamp+sign 两个 query 参数。无 secret 原样返回
// （钉钉机器人"加签"未配置时只能用 IP 白名单，功能降级但不报错）。
func SignWebhook(webhook, secret string, timestampMs int64) string {
	if secret == "" {
		return webhook
	}
	stringToSign := strconv.FormatInt(timestampMs, 10) + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	u, err := url.Parse(webhook)
	if err != nil {
		return webhook // URL 坏掉：原样返回，发送层会报错
	}
	q := u.Query()
	q.Set("timestamp", strconv.FormatInt(timestampMs, 10))
	q.Set("sign", url.QueryEscape(sign))
	u.RawQuery = q.Encode()
	return u.String()
}

// SendResult 发送结果（fork sendDingTalkAlert 返回 {ok:true} 的 Go 版）。
type SendResult struct {
	OK    bool
	Error string
}

// Sender 钉钉发送器（HTTP 客户端可注入，测试用 mock）。
type Sender struct {
	Client *http.Client
	// Jitter 抖动比例（0-1），测试可置 0 关抖动。
	Jitter float64
}

// NewSender 建发送器（默认 10s 超时——告警不能挂住主链路）。
func NewSender() *Sender {
	return &Sender{Client: &http.Client{Timeout: 10 * time.Second}, Jitter: 0.1}
}

// Send 发一条文本告警（fork sendDingTalkAlert L250-277）：
// 加签 → POST msgtype=text → HTTP 非 2xx 或 errcode≠0 都算失败
// （返回错误由调用方记日志；本函数绝不 panic）。
func (s *Sender) Send(set Settings, text string, now time.Time) error {
	if set.DingTalkWebhook == "" {
		return fmt.Errorf("dingtalk webhook is required")
	}
	ts := now.UnixMilli()
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	target := SignWebhook(set.DingTalkWebhook, set.DingTalkSecret, ts)
	payload, err := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
	if err != nil {
		return err
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Post(target, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("dingtalk post: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("dingtalk HTTP %d: %s", resp.StatusCode, truncateTail(string(body), 200))
	}
	// errcode≠0 也算失败（钉钉的 200 里藏着业务错误）。
	var probe struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.ErrCode != 0 {
		return fmt.Errorf("dingtalk errcode %d: %s", probe.ErrCode, probe.ErrMsg)
	}
	return nil
}

// SendWithRetry 带抖动的重试发送（防瞬时网络抖动丢告警）：
// attempts 次指数退避（200ms 起 ×2，±Jitter 抖动），全部失败返回末错。
func (s *Sender) SendWithRetry(set Settings, text string, now time.Time, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := s.Send(set, text, now); err != nil {
			lastErr = err
			if i == attempts-1 {
				break
			}
			d := time.Duration(200<<uint(i)) * time.Millisecond
			if s.Jitter > 0 {
				f := 1 - s.Jitter + rand.Float64()*2*s.Jitter
				d = time.Duration(float64(d) * f)
			}
			time.Sleep(d)
			continue
		}
		return nil
	}
	return lastErr
}

func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
