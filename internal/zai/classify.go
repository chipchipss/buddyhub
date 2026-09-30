package zai

import (
	"net/http"
	"strings"
)

// FailureKind 上游拒绝的语义分类 —— 状态机的输入。
//
// 判定顺序有讲究（对齐 zcode2api 的 classifyAccountFailure 教训）：
// 风控与验证码必须先于「失效」判定，否则一次人机校验续期就把账号错杀成
// INVALID，或把真风控误判成普通鉴权失败而继续打上游、加剧封禁。
type FailureKind int

const (
	FailNone        FailureKind = iota
	FailCaptcha                 // 验证码挑战：换一枚 token 重试同一账号，账号本身无罪
	FailRiskControl             // 真风控（3012/405 unusual activity）：禁用保护
	FailExhausted               // 额度用完（402 / quota 关键词）
	FailRateLimited             // 429：按 Retry-After 等待重试，不冷却账号
	FailInvalid                 // 401/403（非验证码）：凭证失效
	FailServerError             // 5xx：重试耗尽后冷却
	FailOther                   // 其它：计入失败，不改变状态
)

func (k FailureKind) String() string {
	switch k {
	case FailCaptcha:
		return "验证码挑战"
	case FailRiskControl:
		return "风控"
	case FailExhausted:
		return "额度用完"
	case FailRateLimited:
		return "限流"
	case FailInvalid:
		return "凭证失效"
	case FailServerError:
		return "上游错误"
	case FailOther:
		return "其它"
	}
	return "无"
}

// 上游文案判据（来源：zcode2api app/constants.py，逐条对齐；小写比较）。
var (
	exhaustKeywords = []string{"quota", "insufficient", "balance", "exhaust", "额度", "余额不足"}
	captchaMarkers  = []string{`"code":3007`, `"code": 3007`, "captcha", "verify", "人机", "滑块"}
	riskMarkers     = []string{`"code":3012`, `"code": 3012`, "unusual activity", "风控"}
)

// Classify 由 HTTP 状态码 + 响应体判定失败语义。
// header 可为 nil；验证码挑战也常由响应头透出（challenge / captcha 头）。
func Classify(status int, body []byte, header http.Header) FailureKind {
	text := strings.ToLower(string(body))

	if status == 0 {
		return FailOther
	}
	// 2xx/3xx：上游正常受理，没有失败语义
	if status < 400 {
		return FailNone
	}

	// 1) 真风控：405 承载 3012 / unusual activity —— 最高优先，先保护账号
	if status == http.StatusMethodNotAllowed || containsAny(text, riskMarkers) {
		return FailRiskControl
	}

	// 2) 验证码挑战：400/403 + 3007，或文案/响应头带 captcha 痕迹。
	//    注意 captcha 标记只在 4xx 上判定：200 响应体里出现 "verify" 属正常内容。
	if status == http.StatusBadRequest || status == http.StatusForbidden {
		if containsAny(text, captchaMarkers) || headerHasCaptcha(header) {
			return FailCaptcha
		}
	}

	// 3) 额度用完
	if status == http.StatusPaymentRequired || containsAny(text, exhaustKeywords) {
		return FailExhausted
	}

	// 4) 限流（不冷却）
	if status == http.StatusTooManyRequests {
		return FailRateLimited
	}

	// 5) 鉴权失效（已排除验证码）
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return FailInvalid
	}

	// 6) 上游错误
	if status >= 500 {
		return FailServerError
	}
	return FailOther
}

func containsAny(text string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}

func headerHasCaptcha(h http.Header) bool {
	if h == nil {
		return false
	}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "captcha") || strings.Contains(lk, "challenge") {
			return true
		}
		for _, v := range vs {
			if strings.Contains(strings.ToLower(v), "captcha") {
				return true
			}
		}
	}
	return false
}

// RetryAfter 解析 Retry-After（秒或 HTTP 日期），缺省 fallback。
func RetryAfter(h http.Header, fallback int) int {
	if h == nil {
		return fallback
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return fallback
	}
	if secs, err := parseInt(v); err == nil && secs > 0 {
		return secs
	}
	return fallback
}

func parseInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errNotNumber
		}
		n = n*10 + int(r-'0')
		if n > 86400 {
			return 86400, nil
		}
	}
	if n == 0 {
		return 0, errNotNumber
	}
	return n, nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errNotNumber = errString("not a number")
