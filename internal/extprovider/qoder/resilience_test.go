package qoder

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestLoadResilienceConfigDefaults(t *testing.T) {
	c := LoadResilienceConfig()
	if c.QueueMaxAttempts != 15 || c.QueueBaseDelayMs != 5000 || c.QueueMaxDelayMs != 60000 {
		t.Fatalf("queue defaults wrong: %+v", c)
	}
	if c.TimeoutMaxAttempts != 3 || c.TimeoutBaseDelayMs != 3000 || c.TimeoutMaxDelayMs != 15000 {
		t.Fatalf("timeout defaults wrong: %+v", c)
	}
	if c.FirstTokenFallback != FallbackAccountThenBudget {
		t.Fatalf("fallback default = %q", c.FirstTokenFallback)
	}
	if c.AutoContinueMax != 1 || c.StreamTimeoutMs != 600000 || c.StallTimeoutMs != 600000 {
		t.Fatalf("misc defaults wrong: %+v", c)
	}
}

func TestQueueDelayBackoffShape(t *testing.T) {
	c := ResilienceConfig{QueueBaseDelayMs: 5000, QueueMaxDelayMs: 60000}
	// attempt 1 ≈ 5s，attempt 15 ≈ 60s 封顶（抖动 ±10% 内）。
	d1 := c.QueueDelay(1)
	if d1 < 4500*time.Millisecond || d1 > 5500*time.Millisecond {
		t.Fatalf("attempt1 delay = %v", d1)
	}
	d15 := c.QueueDelay(15)
	if d15 < 54000*time.Millisecond || d15 > 66000*time.Millisecond {
		t.Fatalf("attempt15 delay = %v", d15)
	}
}

func TestTimeoutBudgetDelay(t *testing.T) {
	c := ResilienceConfig{TimeoutBaseDelayMs: 3000, TimeoutMaxDelayMs: 15000, TimeoutBudgetMultiplier: 2}
	d := c.TimeoutBudgetDelay(1)
	if d < 5400*time.Millisecond || d > 6600*time.Millisecond {
		t.Fatalf("budget attempt1 delay = %v（期望 6s±10%%）", d)
	}
	// 封顶：budget max = 30s
	dmax := c.TimeoutBudgetDelay(100)
	if dmax > 33000*time.Millisecond {
		t.Fatalf("budget cap delay = %v", dmax)
	}
}

func TestClassifyUpstreamErrorQueuePriority(t *testing.T) {
	// 排队识别优先：403 + isQueued body → queued
	info := ClassifyUpstreamError(403, `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true}\"}"}`)
	if !info.Queued || info.IsTimeout {
		t.Fatalf("403 queued misclassified: %+v", info)
	}
	// 504 + 超时文案 → timeout
	info = ClassifyUpstreamError(504, "upstream: First token timeout after 30s")
	if !info.Queued || !info.IsTimeout {
		t.Fatalf("504 timeout misclassified: %+v", info)
	}
	// 504 但无超时文案 → 不可重试
	if info := ClassifyUpstreamError(504, "bad gateway"); info.Queued {
		t.Fatalf("504 plain should not retry: %+v", info)
	}
	// 500 普通错误 → 不可重试（换号）
	if info := ClassifyUpstreamError(500, `{"error":"boom"}`); info.Queued {
		t.Fatalf("500 should not in-place retry: %+v", info)
	}
}

func TestParseFirstEnvelopeData(t *testing.T) {
	// 普通正文帧 → 零值
	if e, s := ParseFirstEnvelopeData(`{"id":"x","choices":[{"delta":{"content":"hi"}}]}`); e != nil || s.Queued {
		t.Fatalf("normal frame misclassified: %v %v", e, s)
	}
	// 首帧即排队
	if _, s := ParseFirstEnvelopeData(`{"isQueued":true}`); !s.Queued {
		t.Fatalf("queued frame not detected")
	}
	// 首帧即 10605
	if _, s := ParseFirstEnvelopeData(`{"code":"10605"}`); !s.Queued {
		t.Fatalf("10605 not detected")
	}
	// 首帧即 error 信封
	e, _ := ParseFirstEnvelopeData(`{"error":{"message":"prompt is too long","code":"ctx"}}`)
	if e == nil || !e.ContextOverflow || e.Status != 400 {
		t.Fatalf("error envelope: %+v", e)
	}
	// statusCodeValue 信封带排队 body
	_, s := ParseFirstEnvelopeData(`{"statusCodeValue":403,"body":"...\"isQueued\":true..."}`)
	if !s.Queued {
		t.Fatalf("envelope queued body not detected")
	}
	// statusCodeValue 信封带超时文案
	_, s = ParseFirstEnvelopeData(`{"statusCodeValue":504,"body":"Upstream model timeout"}`)
	if !s.Queued || !s.IsTimeout {
		t.Fatalf("envelope timeout not detected: %+v", s)
	}
}

func TestIsContextOverflow(t *testing.T) {
	for _, msg := range []string{"maximum context length exceeded", "reduce the length of your prompt", "input is too long"} {
		if !IsContextOverflow(msg) {
			t.Fatalf("%q should be overflow", msg)
		}
	}
	if IsContextOverflow("account suspended") {
		t.Fatal("false positive")
	}
}

func TestTimeoutDelayForStrategies(t *testing.T) {
	atb := ResilienceConfig{
		FirstTokenFallback: FallbackAccountThenBudget,
		TimeoutMaxAttempts: 3, TimeoutBaseDelayMs: 3000, TimeoutMaxDelayMs: 15000,
		TimeoutBudgetMultiplier: 2,
	}
	// attempt 1-3 走普通档（约 3s/6s/9s 量级），attempt 4 走放大档（约 6s 量级）。
	if d := atb.TimeoutDelayFor(1); d > 4000*time.Millisecond {
		t.Fatalf("atb attempt1 = %v", d)
	}
	if d := atb.TimeoutDelayFor(4); d < 5000*time.Millisecond || d > 8000*time.Millisecond {
		t.Fatalf("atb attempt4 = %v（期望放大档 ~6s）", d)
	}
	bo := atb
	bo.FirstTokenFallback = FallbackBudgetOnly
	if d := bo.TimeoutDelayFor(1); d < 5000*time.Millisecond {
		t.Fatalf("budget-only attempt1 = %v", d)
	}
}

func TestRedactSensitive(t *testing.T) {
	in := `HTTP 403: {"error":"bad","access_token":"dt-abcdef1234567890","next":"sk-xyz1234567890123"}`
	out := RedactSensitive(in)
	if strings.Contains(out, "dt-abcdef") || strings.Contains(out, "sk-xyz1234") {
		t.Fatalf("token leaked: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("no redaction marker: %s", out)
	}
	if RedactSensitive("normal error text") != "normal error text" {
		t.Fatal("plain text mutated")
	}
	if RedactSensitive("Authorization: Bearer eyJhbGciOi.abc") != "Authorization: [REDACTED]" &&
		!strings.Contains(RedactSensitive("Authorization: Bearer eyJhbGciOi.abc"), "[REDACTED]") {
		t.Fatal("bearer not redacted")
	}
}

func TestSSELineScannerReplay(t *testing.T) {
	// 模拟：注释行 + data 首帧 + 更多数据，验证 peekReader.replayed 不丢字节。
	input := ": ping\n" + strings.Repeat("data: {\"chunk\":1}\n\n", 1) + "data: {\"chunk\":2}\n"
	pr := &peekReader{rc: io.NopCloser(strings.NewReader(input))}
	var firstLine string
	var err error
	for {
		firstLine, err = pr.readLine()
		if err != nil || strings.HasPrefix(strings.TrimSpace(firstLine), "data:") {
			break
		}
	}
	if err != nil {
		t.Fatalf("read err: %v", err)
	}
	if !strings.Contains(firstLine, "chunk") {
		t.Fatalf("first data line = %q", firstLine)
	}
	if len(pr.replayed()) == 0 {
		t.Fatal("replayed empty, bytes lost")
	}
}
