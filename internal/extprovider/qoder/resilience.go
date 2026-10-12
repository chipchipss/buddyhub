// Package qoder: resilience.go — 运行时弹性配置与上游响应分类。
//
// 移植自 9router-qoder-plus 的 open-sse/executors/qoder.js（阶段 1 范围）：
//   - 排队响应原位重试（HTTP 403 / SSE 信封内 isQueued:true / code 10605），
//     指数退避 base 5s / max 60s，最多 15 次；
//   - 首 token 超时（"first token timeout" / "upstream model timeout"）
//     独立退避预算（3s/15s、3 次、预算 ×2）与回退策略；
//   - 流空闲超时（QODER_STALL_TIMEOUT_MS）与上游头超时（QODER_STREAM_TIMEOUT_MS）；
//   - first-SSE-envelope 错误处理（首帧前判定成败，不让错误变成正文）；
//   - 隐藏自动续跑计数（检测逻辑在 server 侧 resilient 流里，配置在此）。
//
// 与 fork 的协议差异：buddyhub 的 Qoder 通道走 api2-v2.qoder.sh 纯 Bearer 新版
// 协议，上游直接返回标准 OpenAI chat.completion.chunk SSE——没有 fork 那层
// {statusCodeValue, body} 包裹信封。因此排队/超时识别只做两处：
//  1. HTTP 层：403 body 内 isQueued/10605 → 排队重试；504 body 内超时文案 → 超时重试；
//  2. SSE 层：任一 data 行 JSON 携带 isQueued/10605（防御上游改版）→ 排队重试；
//     首个有效 data 帧是 error 对象 → 首信封错误，按状态分类后上抛。
package qoder

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ResilienceConfig 全部可由环境变量覆盖（契约见 FORK_NOTES.md / README.md）。
type ResilienceConfig struct {
	// QueueMaxAttempts 排队重试上限（QODER_QUEUE_MAX_ATTEMPTS，默认 15）。
	QueueMaxAttempts int
	// QueueBaseDelayMs / QueueMaxDelayMs 排队退避（默认 5000 / 60000），
	// 第 N 次等待 min(base*N, max)，累计约 9.6 分钟。
	QueueBaseDelayMs int
	QueueMaxDelayMs  int
	// KeepaliveMs 排队/静默期间 SSE 注释心跳间隔（QODER_KEEPALIVE_MS，默认 10s）。
	KeepaliveMs int
	// TimeoutMaxAttempts / TimeoutBaseDelayMs / TimeoutMaxDelayMs
	// 首 token 超时退避（默认 3 次，3s/15s）。
	TimeoutMaxAttempts int
	TimeoutBaseDelayMs int
	TimeoutMaxDelayMs  int
	// TimeoutBudgetMultiplier 超时预算放大系数（默认 2）。
	TimeoutBudgetMultiplier float64
	// FirstTokenFallback 首 token 超时回退策略：
	//   "account-then-budget"（默认）先原账号快试一次，再走放大预算；
	//   "budget-only" 直接走放大预算；
	//   "off" 关闭超时重试。
	FirstTokenFallback string
	// AutoContinueMax 隐藏自动续跑上限（QODER_AUTO_CONTINUE_MAX，默认 1，0=关）。
	AutoContinueMax int
	// StreamTimeoutMs 上游响应头超时（QODER_STREAM_TIMEOUT_MS，默认 600s）。
	StreamTimeoutMs int
	// StallTimeoutMs 流空闲字节超时（QODER_STALL_TIMEOUT_MS，默认 600s）。
	StallTimeoutMs int
}

// FirstTokenFallback 策略常量。
const (
	FallbackAccountThenBudget = "account-then-budget"
	FallbackBudgetOnly        = "budget-only"
	FallbackOff               = "off"
)

// envInt 读环境变量整数，未设/非法回退默认值。minFloor 防呆（如 KeepaliveMs ≥1000）。
func envInt(name string, def, minFloor int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < minFloor {
		return minFloor
	}
	return v
}

// LoadResilienceConfig 从环境变量装配（全部默认值对齐 9router-qoder-plus）。
func LoadResilienceConfig() ResilienceConfig {
	mult := 2.0
	if raw := strings.TrimSpace(os.Getenv("QODER_TIMEOUT_BUDGET_MULTIPLIER")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 1 {
			mult = v
		}
	}
	fallback := strings.TrimSpace(os.Getenv("QODER_FIRST_TOKEN_FALLBACK"))
	switch fallback {
	case FallbackBudgetOnly, FallbackOff:
	case "":
		fallback = FallbackAccountThenBudget
	default:
		fallback = FallbackAccountThenBudget
	}
	return ResilienceConfig{
		QueueMaxAttempts:        envInt("QODER_QUEUE_MAX_ATTEMPTS", 15, 1),
		QueueBaseDelayMs:        envInt("QODER_QUEUE_BASE_DELAY_MS", 5000, 1),
		QueueMaxDelayMs:         envInt("QODER_QUEUE_MAX_DELAY_MS", 60000, 1),
		KeepaliveMs:             envInt("QODER_KEEPALIVE_MS", 10000, 1000),
		TimeoutMaxAttempts:      envInt("QODER_TIMEOUT_MAX_ATTEMPTS", 3, 1),
		TimeoutBaseDelayMs:      envInt("QODER_TIMEOUT_BASE_DELAY_MS", 3000, 1),
		TimeoutMaxDelayMs:       envInt("QODER_TIMEOUT_MAX_DELAY_MS", 15000, 1),
		TimeoutBudgetMultiplier: mult,
		FirstTokenFallback:      fallback,
		AutoContinueMax:         envInt("QODER_AUTO_CONTINUE_MAX", 1, 0),
		StreamTimeoutMs:         envInt("QODER_STREAM_TIMEOUT_MS", 600000, 1000),
		StallTimeoutMs:          envInt("QODER_STALL_TIMEOUT_MS", 600000, 1000),
	}
}

// QueueDelay 排队第 attempt 次（从 1 计）退避：min(base*attempt, max)，含抖动。
func (c ResilienceConfig) QueueDelay(attempt int) time.Duration {
	d := c.QueueBaseDelayMs * attempt
	if d > c.QueueMaxDelayMs {
		d = c.QueueMaxDelayMs
	}
	return jitter(time.Duration(d)*time.Millisecond, true)
}

// TimeoutDelay 首 token 超时第 attempt 次退避（同形状，独立预算，含抖动）。
func (c ResilienceConfig) TimeoutDelay(attempt int) time.Duration {
	d := c.TimeoutBaseDelayMs * attempt
	if d > c.TimeoutMaxDelayMs {
		d = c.TimeoutMaxDelayMs
	}
	return jitter(time.Duration(d)*time.Millisecond, true)
}

// TimeoutBudgetDelay 放大预算（×TimeoutBudgetMultiplier）下的第 attempt 次退避。
func (c ResilienceConfig) TimeoutBudgetDelay(attempt int) time.Duration {
	base := int(float64(c.TimeoutBaseDelayMs) * c.TimeoutBudgetMultiplier)
	maxMs := int(float64(c.TimeoutMaxDelayMs) * c.TimeoutBudgetMultiplier)
	d := base * attempt
	if d > maxMs {
		d = maxMs
	}
	return jitter(time.Duration(d)*time.Millisecond, true)
}

// TimeoutDelayFor 按 FirstTokenFallback 策略返回第 attempt 次退避：
// account-then-budget 前 TimeoutMaxAttempts 次走普通档，其后走放大预算；
// budget-only 全走放大预算；off 不应调用（调用方先判断）。
func (c ResilienceConfig) TimeoutDelayFor(attempt int) time.Duration {
	switch c.FirstTokenFallback {
	case FallbackBudgetOnly:
		return c.TimeoutBudgetDelay(attempt)
	default:
		if attempt <= c.TimeoutMaxAttempts {
			return c.TimeoutDelay(attempt)
		}
		return c.TimeoutBudgetDelay(attempt - c.TimeoutMaxAttempts)
	}
}

// TimeoutMaxTotal 单次请求内超时重试总次数上限（含快速档与放大预算）。
func (c ResilienceConfig) TimeoutMaxTotal() int {
	switch c.FirstTokenFallback {
	case FallbackBudgetOnly, FallbackOff:
		return c.TimeoutMaxAttempts
	default:
		return c.TimeoutMaxAttempts + c.TimeoutMaxAttempts
	}
}

// queueBackoff 排队第 attempt 次（从 1 计）退避：min(base*attempt, max)。
func (c ResilienceConfig) queueBackoff(attempt int) time.Duration {
	d := c.QueueBaseDelayMs * attempt
	if d > c.QueueMaxDelayMs {
		d = c.QueueMaxDelayMs
	}
	return time.Duration(d) * time.Millisecond
}

// timeoutBackoff 首 token 超时第 attempt 次退避（同形状，独立预算）。
func (c ResilienceConfig) timeoutBackoff(attempt int) time.Duration {
	d := c.TimeoutBaseDelayMs * attempt
	if d > c.TimeoutMaxDelayMs {
		d = c.TimeoutMaxDelayMs
	}
	return time.Duration(d) * time.Millisecond
}

// timeoutBudget 放大后的超时预算（预算 ×2 语义：base/max 同乘）。
func (c ResilienceConfig) timeoutBudget() (base, maxMs int) {
	return int(float64(c.TimeoutBaseDelayMs) * c.TimeoutBudgetMultiplier),
		int(float64(c.TimeoutMaxDelayMs) * c.TimeoutBudgetMultiplier)
}

// timeoutBudgetBackoff 放大预算下的第 attempt 次退避。
func (c ResilienceConfig) timeoutBudgetBackoff(attempt int) time.Duration {
	base, maxMs := c.timeoutBudget()
	d := base * attempt
	if d > maxMs {
		d = maxMs
	}
	return time.Duration(d) * time.Millisecond
}

// StallTimeout 流空闲字节超时。
func (c ResilienceConfig) StallTimeout() time.Duration {
	return time.Duration(c.StallTimeoutMs) * time.Millisecond
}

// StreamTimeout 上游响应头超时。
func (c ResilienceConfig) StreamTimeout() time.Duration {
	return time.Duration(c.StreamTimeoutMs) * time.Millisecond
}

// QueueInfo 上游排队/超时识别结果。
type QueueInfo struct {
	// Queued 本次响应判定为"排队中"（或按超时策略视作可原位重试）。
	Queued bool
	// IsTimeout 排队原因是首 token/上游超时（走独立预算与回退策略）。
	IsTimeout bool
	// Reason 人类可读原因（"queued" / "first token timeout" / ...）。
	Reason string
	// QueueCount 上游报的排队位次（拿不到为 0，仅日志用）。
	QueueCount int
}

var (
	queueCountRe = regexp.MustCompile(`queueCount["\\:]*\s*(\d+)`)
	// 首信封错误里典型的上下文溢出文案（供上层决定是否标记 context_overflow）。
	contextOverflowRe = regexp.MustCompile(`(?i)range of input length|input length|maximum context|context length|context_length_exceeded|too many tokens|input is too long|prompt is too long|max_input_tokens|reduce the length`)
)

// ParseQueueText 在响应体文本里识别 Qoder 软排队：
// 403 body 是多层嵌套 JSON（{"code":"403","message":"{\"code\":\"10605\",
// \"message\":\"{\\\"isQueued\\\":true,...}\"}"}），直接做子串匹配最稳。
func ParseQueueText(text string) QueueInfo {
	queued := strings.Contains(text, `"isQueued":true`) ||
		strings.Contains(text, `\"isQueued\":true`) ||
		strings.Contains(text, "10605")
	info := QueueInfo{Queued: queued, Reason: "queued"}
	if m := queueCountRe.FindStringSubmatch(text); m != nil {
		info.QueueCount, _ = strconv.Atoi(m[1])
	}
	return info
}

// ParseTimeoutText 识别首 token / 上游超时文案。
func ParseTimeoutText(text string) QueueInfo {
	lower := strings.ToLower(text)
	firstToken := strings.Contains(lower, "first token timeout")
	upstream := strings.Contains(lower, "upstream model timeout") ||
		strings.Contains(lower, "upstream timeout")
	if !firstToken && !upstream {
		return QueueInfo{}
	}
	reason := "upstream model timeout"
	if firstToken {
		reason = "first token timeout"
	}
	return QueueInfo{Queued: true, IsTimeout: true, Reason: reason}
}

// ClassifyUpstreamError 从 HTTP 错误 body 分类可重试性：
// 排队 → 原位重试；超时 → 按回退策略；其余返回零值（换号或直接报错）。
func ClassifyUpstreamError(status int, body string) QueueInfo {
	if info := ParseTimeoutText(body); info.Queued && (status == 504 || status == 408) {
		return info
	}
	if status == 403 || status == 429 {
		if info := ParseQueueText(body); info.Queued {
			return info
		}
	}
	return QueueInfo{}
}

// IsContextOverflow 判断错误文案是否上下文溢出（供上层换客户端能识别的措辞，
// 触发其自动压缩）。
func IsContextOverflow(message string) bool { return contextOverflowRe.MatchString(message) }

var sensitiveRe = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{8,}|dt-[A-Za-z0-9_\-]{8,}|drt-[A-Za-z0-9_\-]{8,}|jrt-[A-Za-z0-9_\-]{8,}|bearer\s+[A-Za-z0-9._\-]{8,}|"(access_token|refresh_token|security_oauth_token|authorization)"\s*:\s*"[^"]*")`)

// RedactSensitive 打日志前的脱敏：把 token 形态的串（sk-/dt-/drt-/jrt- 前缀、
// Bearer 头、token 类 JSON 字段值）替换成 [REDACTED]。上游错误 body 偶尔回显
// 凭据，直接进日志会泄漏。
func RedactSensitive(s string) string { return sensitiveRe.ReplaceAllString(s, "[REDACTED]") }

// EnvelopeError 首个 SSE data 帧即 error 对象时的分类结果。
type EnvelopeError struct {
	Status          int    // 建议给客户端的 HTTP 状态
	Message         string // 提炼后的可读信息
	Code            string // openai 错误 code
	ContextOverflow bool
}

// Error 实现 error 接口（桥接层按类型断言拿回结构体字段）。
func (e *EnvelopeError) Error() string { return e.Message }

// Jitter 对退避时长加 ±10% 抖动的导出包装（server 弹性层用，d<=0 原样返回）。
func Jitter(d time.Duration) time.Duration { return jitter(d, true) }

// ParseFirstEnvelopeData 解析一个 SSE data 行负载（首帧判定）：
//   - envErr 非 nil：首帧即错误信封，按状态分类后上抛（不让错误变正文）；
//   - signal.Queued：首帧携带排队/超时标记（防御上游改版/网关包裹），
//     IsTimeout 区分走排队预算还是首 token 超时预算；
//   - 两者皆零值：普通正文帧（buddyhub 上游标准 OpenAI chunk 的绝大多数）。
func ParseFirstEnvelopeData(data string) (envErr *EnvelopeError, signal QueueInfo) {
	var frame struct {
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		StatusCodeValue int    `json:"statusCodeValue"`
		Body            string `json:"body"`
		IsQueued        *bool  `json:"isQueued"`
		Code            any    `json:"code"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return nil, QueueInfo{}
	}
	if frame.IsQueued != nil && *frame.IsQueued {
		return nil, QueueInfo{Queued: true, Reason: "queued"}
	}
	if s, ok := frame.Code.(string); ok && s == "10605" {
		return nil, QueueInfo{Queued: true, Reason: "queued"}
	}
	if frame.Error != nil {
		msg := strings.TrimSpace(frame.Error.Message)
		overflow := IsContextOverflow(msg)
		status := 502
		if frame.StatusCodeValue >= 400 && frame.StatusCodeValue < 600 {
			status = frame.StatusCodeValue
		}
		code := "upstream_error"
		switch {
		case overflow:
			code = "context_length_exceeded"
			status = 400
		case status >= 500:
			code = "upstream_error"
		default:
			code = "provider_error"
		}
		if c, ok := frame.Error.Code.(string); ok && c != "" {
			code = c
		}
		return &EnvelopeError{Status: status, Message: msg, Code: code, ContextOverflow: overflow}, QueueInfo{}
	}
	if frame.StatusCodeValue >= 400 && frame.StatusCodeValue < 600 {
		if info := ParseQueueText(frame.Body); info.Queued {
			return nil, info
		}
		if info := ParseTimeoutText(frame.Body); info.Queued {
			return nil, info
		}
		overflow := IsContextOverflow(frame.Body) || IsContextOverflow(data)
		status := frame.StatusCodeValue
		code := "upstream_error"
		if !overflow && status < 500 {
			code = "provider_error"
		}
		if overflow {
			status = 400
			code = "context_length_exceeded"
		}
		return &EnvelopeError{Status: status, Message: truncateMsg(frame.Body, 1000), Code: code, ContextOverflow: overflow}, QueueInfo{}
	}
	return nil, QueueInfo{}
}

func truncateMsg(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// SSELineScanner 按行拆 SSE 流并在首帧判定时挂钩：server 桥接用它做
// "首个 data 帧 = 排队/错误信封" 的原位分类。这里提供最小实现供桥接复用：
// ReadLine 返回下一行（不含行尾 \n），EOF 返回 io.EOF。
type SSELineScanner struct {
	r   io.Reader
	buf []byte
}

// NewSSELineScanner 创建按行扫描器。
func NewSSELineScanner(r io.Reader) *SSELineScanner { return &SSELineScanner{r: r} }

// ReadLine 读取一行；最后一行无换行符也返回（err 为 nil，下一次调用回 io.EOF）。
func (s *SSELineScanner) ReadLine() (string, error) {
	for {
		for i, b := range s.buf {
			if b == '\n' {
				line := string(s.buf[:i])
				s.buf = s.buf[i+1:]
				return line, nil
			}
		}
		chunk := make([]byte, 4096)
		n, err := s.r.Read(chunk)
		s.buf = append(s.buf, chunk[:n]...)
		if err != nil {
			if len(s.buf) == 0 {
				return "", err
			}
			line := string(s.buf)
			s.buf = nil
			return line, err
		}
	}
}

// ClassifyFirstEnvelope 对 2xx 建流后的第一个 SSE data 帧做判定：
//   - envErr 非 nil：首帧即错误信封（调用方应直接把错误写给客户端）；
//   - signal.Queued：首帧携带排队/超时标记（可原位重试），流已消费到该帧为止；
//   - 两者皆零值：普通正文帧——返回一个新的 reader，内容为该帧及其后所有字节，
//     供下游按原样透传（帧不丢失）。
//
// 实现：先经 PeekReader 读出直到首个完整 data 行，非首帧判定路径把已读字节
// 重放进 MultiReader，保证正文一个字节不丢。
func ClassifyFirstEnvelope(rc io.ReadCloser, res ResilienceConfig) (io.ReadCloser, *EnvelopeError, QueueInfo, bool) {
	_ = res
	pr := &peekReader{rc: rc}
	var line string
	var lerr error
	for {
		line, lerr = pr.readLine()
		if lerr != nil || strings.HasPrefix(strings.TrimSpace(line), "data:") {
			break
		}
	}
	if lerr != nil {
		// 流在首帧前就断了：当作普通错误上抛（调用方按网络错误处理）。
		return nil, &EnvelopeError{Status: 502, Message: "上游在首帧前结束: " + lerr.Error(), Code: "upstream_error"}, QueueInfo{}, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
	envErr, signal := ParseFirstEnvelopeData(payload)
	if envErr != nil {
		rc.Close()
		return nil, envErr, QueueInfo{}, false
	}
	if signal.Queued {
		rc.Close()
		return nil, nil, signal, true
	}
	return &replayedCloser{r: io.MultiReader(bytes.NewReader(pr.replayed()), rc), rc: rc}, nil, QueueInfo{}, false
}

// peekReader 逐字节缓冲读，支持 replayed() 取回全部已读字节。
type peekReader struct {
	rc       io.ReadCloser
	seen     []byte
	consumed int
}

func (p *peekReader) readLine() (string, error) {
	for {
		// 从已消费位置之后扫描换行——seen 缓冲要原样保留供 replayed() 重放，
		// 绝不能从头扫（否则同一行被反复返回，调用方死循环）。
		for i := p.consumed; i < len(p.seen); i++ {
			if p.seen[i] == '\n' {
				line := string(p.seen[p.consumed:i])
				p.consumed = i + 1
				return line, nil
			}
		}
		chunk := make([]byte, 4096)
		n, err := p.rc.Read(chunk)
		p.seen = append(p.seen, chunk[:n]...)
		if err != nil {
			// 流结束但缓冲里还有最后半行：按行返回，下次调用再报 EOF。
			if n == 0 && err != nil && p.consumed < len(p.seen) {
				line := string(p.seen[p.consumed:])
				p.consumed = len(p.seen)
				return line, nil // 尾行无换行符：返回剩余数据
			}
			return "", err
		}
	}
}

// replayed 返回尚未被 readLine 消费的全部已读字节（首判定行 + 之后未读缓冲）。
func (p *peekReader) replayed() []byte { return p.seen[p.consumed:] }

// replayedCloser 把 MultiReader 包装回 ReadCloser；Close 透传给底层，
// 否则正常路径的 HTTP body 永远不被释放（连接泄漏）。
type replayedCloser struct {
	r  io.Reader
	rc io.Closer
}

func (c *replayedCloser) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *replayedCloser) Close() error {
	if c.rc != nil {
		return c.rc.Close()
	}
	return nil
}

// jitter 对退避时长加 ±10% 抖动，避免多客户端同步重试打向上游。
// fork 未做，这里是多账号部署后的稳健性补充。
func jitter(d time.Duration, wantJitter bool) time.Duration {
	if !wantJitter || d <= 0 {
		return d
	}
	f := 0.9 + rand.Float64()*0.2
	return time.Duration(float64(d) * f)
}
