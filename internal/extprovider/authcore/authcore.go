// Package authcore — 多平台登录底座：device-flow 轮询状态机 + refresh-lead
// 提前刷新原语（magpie-community/plugins 首批移植的公共部分）。
//
// 来源参考（MIT，HEAD f573cfd）：
//   - packages/qoder/index.mjs：REFRESH_LEAD/RENEW_LEAD/KEEP_SPENT_MS
//     提前刷新三参数（L48-50）、refresh token 一次性用后即废；
//   - packages/minimax/index.mjs：poll 状态机 pending/slow_down(+5s)/denied/
//     expired（L270-283）、expired_in 毫秒/秒歧义（L245，≥1e12 视为毫秒）、
//     无 device_code 用 user_code 轮询的变体、登出判定只认 invalid_grant+400
//     （L304，短暂故障绝不固化为禁用）。
//
// 设计约束：纯函数 + 显式状态（无全局可变状态），供各 provider 填空复用。
package authcore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── PKCE（RFC 7636 S256）──

// pkceAlphabet base64url 无 padding 字符集（Qoder 插件 verifier 同口径）。
const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// NewVerifier 生成 PKCE code_verifier（n 字节随机 → base64url；n∈[32,96]）。
func NewVerifier(n int) (string, error) {
	if n < 32 {
		n = 32
	}
	if n > 96 {
		n = 96
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// 直接把随机字节映射进 alphabet（entropy 保留，长度即 n）。
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = pkceAlphabet[int(b)%len(pkceAlphabet)]
	}
	return string(out), nil
}

// ChallengeS256 code_challenge = BASE64URL(SHA256(verifier))，去 padding。
func ChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ── 过期时间收敛 ──

// ExpiresAtMS 把上游五花八门的过期表达统一成毫秒时间戳：
//   - 数值 <1e12 → 秒（×1000）；≥1e12 → 已是毫秒（MiniMax expired_in 变体）；
//   - 数值 ≤0 / 非法 → 0（未知，调用方按"不提前刷新、401 时再救"处理）；
//   - 字符串 → 先试数字，再试 RFC3339/RFC3339Nano/日期，全失败返回 0。
//
// （与 buddyhub qoder.timeMillisAt 同语义，这里抽成平台无关底座。）
func ExpiresAtMS(v any) int64 {
	toMillis := func(n int64) int64 {
		if n <= 0 {
			return 0
		}
		if n < 1e12 {
			n *= 1000
		}
		return n
	}
	switch t := v.(type) {
	case float64:
		return toMillis(int64(t))
	case int64:
		return toMillis(t)
	case int:
		return toMillis(int64(t))
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0
		}
		return toMillis(n)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return toMillis(n)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UnixMilli()
			}
		}
		return 0
	default:
		return 0
	}
}

// ExpiryDeadline 将设备授权过期字段收敛为截止时间。MiniMax expired_in
// 变体：≥1e12 是绝对 Unix 毫秒时间戳，较小正数是相对秒数（magpie
// minimax/index.mjs L240-246）。
func ExpiryDeadline(value int64, now time.Time) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value >= 1e12 {
		return time.UnixMilli(value)
	}
	return now.Add(time.Duration(value) * time.Second)
}

// ── Device-flow 轮询状态机 ──

// PollStatus 一次轮询的判定（fork minimax pollDevice 的五态）。
type PollStatus int

const (
	// PollPending 上游仍未授权（authorization_pending）。
	PollPending PollStatus = iota
	// PollSlowDown 上游要求放慢（slow_down：下次间隔 +5s）。
	PollSlowDown
	// PollDenied 用户拒绝授权（denied/access_denied）。
	PollDenied
	// PollExpiredToken device_code/user_code 过期（expired_token/expired）。
	PollExpiredToken
	// PollError 传输层/5xx 等瞬时错误（可继续轮询，不计入语义态）。
	PollError
	// PollDone 拿到 token（本包不解析 payload 形状，由 provider 收敛）。
	PollDone
)

// PollOutcome 一次轮询的结果。
type PollOutcome struct {
	Status PollStatus
	// Payload PollDone 时的原始响应（provider 自行收敛字段）。
	Payload []byte
	// Err PollError 时的传输错误（日志用）。
	Err error
}

// ClassifyPollResponse 按响应状态码 + body 错误码做五态判定。
// 上游约定（OAuth device flow + MiniMax 变体）：
//   - 200 → PollDone（body 原样交给 provider）；
//   - 400/403/429 里 body 错误码细分：
//     authorization_pending → Pending；slow_down → SlowDown；
//     access_denied/denied → Denied；expired_token/expired → ExpiredToken；
//     其余 → Error（瞬时，继续轮）；
//   - 5xx/其他 → Error。
func ClassifyPollResponse(httpStatus int, body []byte, err error) PollOutcome {
	if err != nil {
		return PollOutcome{Status: PollError, Err: err}
	}
	switch {
	case httpStatus >= 200 && httpStatus < 300:
		return PollOutcome{Status: PollDone, Payload: body}
	case httpStatus >= 500:
		return PollOutcome{Status: PollError, Err: fmt.Errorf("HTTP %d", httpStatus)}
	case httpStatus == 400 || httpStatus == 403 || httpStatus == 429:
		code := pollErrCode(body)
		switch code {
		case "authorization_pending":
			return PollOutcome{Status: PollPending}
		case "slow_down":
			return PollOutcome{Status: PollSlowDown}
		case "access_denied", "denied":
			return PollOutcome{Status: PollDenied}
		case "expired_token", "expired":
			return PollOutcome{Status: PollExpiredToken}
		}
	}
	return PollOutcome{Status: PollError, Err: fmt.Errorf("HTTP %d", httpStatus)}
}

// pollErrCode 从 body 里抠 OAuth 错误码（"error":"slow_down" 或裸 code 字段）。
func pollErrCode(body []byte) string {
	var doc struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.Error != "" {
		return doc.Error
	}
	var doc2 struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &doc2) == nil {
		return doc2.Code
	}
	return ""
}

// SlowDownPenalty slow_down 的下次间隔增量（OAuth 规范推荐 5s，fork 同值）。
const SlowDownPenalty = 5 * time.Second

// NextInterval 计算下一轮间隔：基础 interval，遇 slow_down 罚 5s。
// interval<=0 回落 5s（fork minimax 默认）。毫秒/秒歧义由调用方先行归一
// （≥1e12 视为毫秒——MiniMax 变体，见 NormalizeIntervalMS）。
func NextInterval(interval time.Duration, slowDown bool) time.Duration {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if slowDown {
		interval += SlowDownPenalty
	}
	return interval
}

// NormalizeIntervalMS 上游 interval 字段的毫秒/秒歧义收敛：
// MiniMax 的 interval 单位是毫秒（L244），其他 OAuth 实现多为秒——
// ≥1000 一律按毫秒（>=1 秒），<1000 按秒。返回毫秒时长。
func NormalizeIntervalMS(v int64) time.Duration {
	if v <= 0 {
		return 5 * time.Second
	}
	if v < 1000 {
		return time.Duration(v) * time.Second
	}
	return time.Duration(v) * time.Millisecond
}

// PollLoop 通用轮询循环：poll 由 provider 提供单次请求，本函数管
// 间隔/slow_down 罚时/截止（deadline）与 ctx 取消。
// 返回 (最终 PollOutcome, true=PollDone, err=ctx/致命错误)。
// poll 返回 PollDone/PollDenied/PollExpiredToken 即终止；Pending/Error/SlowDown 继续。
func PollLoop(ctx context.Context, deadline time.Time, interval time.Duration, poll func(ctx context.Context) PollOutcome) (PollOutcome, bool, error) {
	cur := interval
	for {
		if ctx.Err() != nil {
			return PollOutcome{}, false, ctx.Err()
		}
		out := poll(ctx)
		switch out.Status {
		case PollDone, PollDenied, PollExpiredToken:
			return out, out.Status == PollDone, nil
		case PollSlowDown:
			cur = NextInterval(cur, true)
		default:
			cur = NextInterval(interval, false) // 瞬时错误回基础间隔
		}
		if time.Now().Add(cur).After(deadline) {
			return out, false, fmt.Errorf("device-flow 轮询超时（deadline %s）", deadline.Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
			return PollOutcome{}, false, ctx.Err()
		case <-time.After(cur):
		}
	}
}

// ── Refresh-lead 提前刷新 ──

// LeadPolicy 提前刷新三参数（fork qoder REFRESH_LEAD/RENEW_LEAD/KEEP_SPENT_MS
// 与 minimax EARLY_MS/LEAD_MS 同构）。
type LeadPolicy struct {
	// Early 剩余寿命 ≤ Early 时，请求前同步刷新（阻塞当前请求）。
	Early time.Duration
	// Renew 剩余寿命 ≤ Renew 时，后台续期（不阻塞请求）。
	Renew time.Duration
	// SpentTTL 已作废 refresh_token 的"结果缓存"时长：一次性 refresh token
	// 用后即废，并发双请求拿同一个旧 token 刷新时，第二个必须直接复用
	// 第一个的结果而不是再打上游（fork KEEP_SPENT_MS=1h）。
	SpentTTL time.Duration
}

// DefaultLeadPolicy fork 默认：early 5min / renew 10min / spent 1h。
func DefaultLeadPolicy() LeadPolicy {
	return LeadPolicy{Early: 5 * time.Minute, Renew: 10 * time.Minute, SpentTTL: time.Hour}
}

// LeadMode 对 (expiresAt, now) 的刷新决策。
type LeadMode int

const (
	// LeadNone 不需要刷新。
	LeadNone LeadMode = iota
	// LeadOverdue 已过期（401 马上会来，刷新是抢救）。
	LeadOverdue
	// LeadEarly 剩余 ≤ Early：请求前同步刷新。
	LeadEarly
	// LeadRenew 剩余 ≤ Renew：后台续期。
	LeadRenew
)

// Mode 判定。expiresAtMS<=0（上游没给过期时间）一律 LeadNone——
// 不误刷新，失效时靠 401 触发（buddyhub qoder.NeedsRefresh 同口径）。
func (p LeadPolicy) Mode(expiresAtMS int64, now time.Time) LeadMode {
	if expiresAtMS <= 0 {
		return LeadNone
	}
	remain := time.UnixMilli(expiresAtMS).Sub(now)
	switch {
	case remain <= 0:
		return LeadOverdue
	case remain <= p.Early:
		return LeadEarly
	case p.Renew > 0 && remain <= p.Renew:
		return LeadRenew
	default:
		return LeadNone
	}
}

// ── Spent refresh-token 缓存 ──

// SpentEntry 缓存的刷新结果（成功或已确认失败都缓存——失败也缓存是防
// 上游在坏 token 上被并发轰炸）。
type SpentEntry struct {
	Cred      []byte // 新凭据（provider 序列化形状，原样存取）
	Err       string // 非 nil = 刷新失败的原因
	ExpiresAt time.Time
}

// SpentCache 一次性 refresh_token 的结果缓存（fork KEEP_SPENT_MS 语义）。
type SpentCache struct {
	mu      sync.Mutex
	entries map[string]SpentEntry
	ttl     time.Duration
	now     func() time.Time
}

// NewSpentCache 建（ttl<=0 用 1h）。
func NewSpentCache(ttl time.Duration) *SpentCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &SpentCache{entries: map[string]SpentEntry{}, ttl: ttl, now: time.Now}
}

// Get 取缓存（过期/缺失 → false）。命中即顺带惰性清理过期项。
func (c *SpentCache) Get(refreshToken string) (SpentEntry, bool) {
	if refreshToken == "" {
		return SpentEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[refreshToken]
	if !ok {
		return SpentEntry{}, false
	}
	if c.now().After(e.ExpiresAt) {
		delete(c.entries, refreshToken)
		return SpentEntry{}, false
	}
	return e, true
}

// Put 存（键 = 已作废的旧 refresh_token；空键忽略）。
func (c *SpentCache) Put(refreshToken string, e SpentEntry) {
	if refreshToken == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e.ExpiresAt = c.now().Add(c.ttl)
	c.entries[refreshToken] = e
	// 容量护栏：超 2048 全清一轮（TTL 惰性过期已覆盖绝大多数场景）。
	if len(c.entries) > 2048 {
		now := c.now()
		for k, v := range c.entries {
			if now.After(v.ExpiresAt) {
				delete(c.entries, k)
			}
		}
	}
}

// LogoutPredicate 登出判定口径：MiniMax 只认 invalid_grant + HTTP 400
// （L304）；5xx/超时/其他错误一律保留登录态——短暂故障绝不固化为禁用。
// 各 provider 用它统一"这算不算掉号"。
func LogoutPredicate(httpStatus int, body []byte) bool {
	if httpStatus != 400 {
		return false
	}
	return pollErrCode(body) == "invalid_grant"
}
