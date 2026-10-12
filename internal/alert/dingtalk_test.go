package alert

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func testSettings() Settings {
	return Settings{
		Enabled:                true,
		IdleMinutes:            5,
		CooldownMinutes:        5,
		DingTalkWebhook:        "https://oapi.dingtalk.com/robot/send?access_token=x",
		QuotaUsageThresholdOn:  true,
		QuotaUsageThresholdPct: 80,
		Locale:                 "en",
	}
}

func TestNormalizeDefaults(t *testing.T) {
	n := Normalize(Settings{})
	if n.IdleMinutes != 5 || n.CooldownMinutes != 0 || n.QuotaUsageThresholdPct != 80 {
		t.Fatalf("defaults wrong: %+v", n)
	}
	if n.Locale != "en" {
		t.Fatalf("locale default = %q", n.Locale)
	}
	if n.MessageTemplate != defaultTemplate {
		t.Fatalf("template default wrong: %q", n.MessageTemplate)
	}
	// 越界钳制
	n = Normalize(Settings{QuotaUsageThresholdPct: 200})
	if n.QuotaUsageThresholdPct != 100 {
		t.Fatalf("pct clamp = %d", n.QuotaUsageThresholdPct)
	}
	n = Normalize(Settings{CooldownMinutes: -3})
	if n.CooldownMinutes != 5 {
		t.Fatalf("cooldown negative = %d", n.CooldownMinutes)
	}
}

func TestMaskSecret(t *testing.T) {
	s := testSettings()
	s.DingTalkSecret = "SEC123"
	m := Mask(s)
	if m.DingTalkSecret != "" {
		t.Fatal("secret leaked in masked settings")
	}
	if !m.HasDingTalkSecret {
		t.Fatal("hasDingtalkSecret should be true")
	}
	m2 := Mask(testSettings())
	if m2.HasDingTalkSecret {
		t.Fatal("empty secret should report false")
	}
}

func TestEvaluateIdle(t *testing.T) {
	s := testSettings()
	st := &IdleState{}
	now := int64(1_700_000_000_000)

	// 从未调用
	if v := EvaluateIdle(st, s, now); v.ShouldAlert || v.Reason != "no-call" {
		t.Fatalf("no-call: %+v", v)
	}
	// 未达阈值
	st.RecordCall(now - 2*60_000)
	if v := EvaluateIdle(st, s, now); v.ShouldAlert || v.Reason != "below-idle-threshold" {
		t.Fatalf("below threshold: %+v", v)
	}
	// 超阈值 → 应告警
	st.RecordCall(now - 10*60_000)
	v := EvaluateIdle(st, s, now)
	if !v.ShouldAlert || v.Reason != "idle" || v.IdleMS != 10*60_000 {
		t.Fatalf("idle: %+v", v)
	}
	// 同一次调用已告过 → 不重复
	st.LastAlertAt = now
	st.LastAlertedCallAt = st.LastCallAt
	if v := EvaluateIdle(st, s, now+60_000); v.ShouldAlert || v.Reason != "already-alerted" {
		t.Fatalf("already-alerted: %+v", v)
	}
	// 新调用后冷却内不告警
	st.RecordCall(now - 10*60_000)
	st.LastAlertAt = now
	if v := EvaluateIdle(st, s, now+60_000); v.ShouldAlert || v.Reason != "cooldown" {
		t.Fatalf("cooldown: %+v", v)
	}
	// 关闭 / 缺 webhook
	off := s
	off.Enabled = false
	if v := EvaluateIdle(&IdleState{LastCallAt: now - 10*60_000}, off, now); v.ShouldAlert {
		t.Fatal("disabled should not alert")
	}
	noHook := s
	noHook.DingTalkWebhook = ""
	if v := EvaluateIdle(&IdleState{LastCallAt: now - 10*60_000}, noHook, now); v.ShouldAlert {
		t.Fatal("missing webhook should not alert")
	}
}

func TestQuotaExhaustedFlow(t *testing.T) {
	s := testSettings()
	st := NewKeyQuotaState()
	now := int64(1_700_000_000_000)

	if v := EvaluateQuotaExhausted(st, s, "k1", now); !v.ShouldAlert {
		t.Fatalf("first should alert: %+v", v)
	}
	st.MarkQuotaExhausted("k1", now)
	if v := EvaluateQuotaExhausted(st, s, "k1", now+60_000); v.ShouldAlert || v.Reason != "already-alerted" {
		t.Fatalf("second should be suppressed: %+v", v)
	}
	// 恢复后可再告警
	st.MarkQuotaRecovered("k1")
	if v := EvaluateQuotaExhausted(st, s, "k1", now+120_000); !v.ShouldAlert {
		t.Fatalf("recovered should alert again: %+v", v)
	}
	// 冷却内抑制（未标记 exhausted 的场景）
	st2 := NewKeyQuotaState()
	st2.MarkQuotaExhausted("k2", now)
	st2.MarkQuotaRecovered("k2") // 清掉 exhausted 但保留 lastAlertByKey
	// MarkQuotaRecovered 会删 lastAlertByKey，所以这里手动压入冷却记录
	st2.LastAlertByKey["k2"] = now
	if v := EvaluateQuotaExhausted(st2, s, "k2", now+60_000); v.ShouldAlert || v.Reason != "cooldown" {
		t.Fatalf("cooldown: %+v", v)
	}
}

func TestQuotaThreshold(t *testing.T) {
	s := testSettings()
	st := NewKeyQuotaState()
	now := int64(1_700_000_000_000)
	d := QuotaDetails{KeyID: "k1", Used: 85, Limit: 100}
	v := EvaluateQuotaThreshold(st, s, d, now)
	if !v.ShouldAlert || v.UsagePct != 85 || v.ThresholdPct != 80 {
		t.Fatalf("threshold: %+v", v)
	}
	st.MarkQuotaThreshold("k1", now)
	if v := EvaluateQuotaThreshold(st, s, d, now+60_000); v.ShouldAlert || v.Reason != "cooldown" {
		t.Fatalf("cooldown: %+v", v)
	}
	// 低于阈值
	d2 := QuotaDetails{KeyID: "k2", Used: 50, Limit: 100}
	if v := EvaluateQuotaThreshold(st, s, d2, now); v.ShouldAlert || v.Reason != "below-threshold" {
		t.Fatalf("below: %+v", v)
	}
	// 无 limit
	if v := EvaluateQuotaThreshold(st, s, QuotaDetails{KeyID: "k3", Used: 50}, now); v.ShouldAlert || v.Reason != "missing-limit" {
		t.Fatalf("missing-limit: %+v", v)
	}
	// 阈值开关关
	off := s
	off.QuotaUsageThresholdOn = false
	if v := EvaluateQuotaThreshold(st, off, d, now); v.ShouldAlert || v.Reason != "threshold-disabled" {
		t.Fatalf("threshold-disabled: %+v", v)
	}
}

func TestUsageFetchFailedCooldown(t *testing.T) {
	st := NewKeyQuotaState()
	now := int64(1_700_000_000_000)
	if !EvaluateUsageFetchFailed(st, "acct1", now) {
		t.Fatal("first should alert")
	}
	st.MarkUsageFetchFailed("acct1", now)
	if EvaluateUsageFetchFailed(st, "acct1", now+10*60_000) {
		t.Fatal("within 30min cooldown should not alert")
	}
	if !EvaluateUsageFetchFailed(st, "acct1", now+31*60_000) {
		t.Fatal("after cooldown should alert")
	}
	if EvaluateUsageFetchFailed(st, "", now) {
		t.Fatal("empty account should not alert")
	}
}

func TestBuildIdleText(t *testing.T) {
	s := testSettings()
	s.MessageTemplate = "idle={idleMinutes} last={lastCallAt} now={now}"
	st := &IdleState{LastCallAt: 1_700_000_000_000}
	out := BuildIdleText(s, st, 1_700_000_000_000+7*60_000)
	if !strings.Contains(out, "idle=7") {
		t.Fatalf("idleMinutes not substituted: %s", out)
	}
	if !strings.Contains(out, "last=2023-11-15 06:13:20") {
		t.Fatalf("lastCallAt not substituted: %s", out)
	}
	// 无调用时 last=unknown
	out2 := BuildIdleText(s, &IdleState{}, 1_700_000_000_000)
	if !strings.Contains(out2, "last=unknown") || !strings.Contains(out2, "idle=0") {
		t.Fatalf("empty state text: %s", out2)
	}
}

func TestQuotaTextsBilingual(t *testing.T) {
	en := testSettings()
	out := BuildQuotaExhaustedText(en, QuotaTextDetails{KeyName: "k", Provider: "qoder", Used: 9, Limit: 10, Remaining: 1}, 1_700_000_000_000)
	if !strings.Contains(out, "exhausted") || !strings.Contains(out, "9/10") {
		t.Fatalf("en text: %s", out)
	}
	zh := en
	zh.Locale = "zh-CN"
	out = BuildQuotaExhaustedText(zh, QuotaTextDetails{KeyName: "k", Used: 9, Limit: 10, Remaining: 1}, 1_700_000_000_000)
	if !strings.Contains(out, "耗尽") {
		t.Fatalf("zh text: %s", out)
	}
	// 阈值文案
	out = BuildQuotaThresholdText(en, QuotaTextDetails{KeyName: "k", Used: 85, Limit: 100, Remaining: 15, ThresholdP: 80, UsagePct: 85}, 1_700_000_000_000)
	if !strings.Contains(out, "85%") || !strings.Contains(out, "Threshold: 80%") {
		t.Fatalf("threshold text: %s", out)
	}
	// 读取失败文案（原因截断 200）
	long := strings.Repeat("x", 300)
	out = BuildUsageFetchFailedText(en, UsageFetchTextDetails{Err: long}, 1_700_000_000_000)
	if strings.Count(out, "x") > 200 {
		t.Fatalf("reason not truncated to 200: len=%d", strings.Count(out, "x"))
	}
}

func TestSignWebhook(t *testing.T) {
	// 无 secret 原样
	if got, err := SignWebhook("https://example.com/hook", "", 123); err != nil || got != "https://example.com/hook" {
		t.Fatalf("no-secret passthrough: %q %v", got, err)
	}
	// 有 secret：query 带 timestamp/sign，sign 是 base64 的 urlencode
	got, err := SignWebhook("https://oapi.dingtalk.com/robot/send?access_token=x", "SEC", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("sign err: %v", err)
	}
	if !strings.Contains(got, "timestamp=1700000000000") || !strings.Contains(got, "sign=") {
		t.Fatalf("signed url missing params: %s", got)
	}
	_ = got // 已在上面统一解析
	// sign 值：query 里是单次 urlencode 的 base64（先 decode query 再 base64 解）。
	// 双重编码（%252B/%253D）是历史 bug，测试钉死单次口径。
	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("parse signed url: %v", perr)
	}
	enc := u.Query().Get("sign")
	if strings.Contains(enc, "%25") {
		t.Fatalf("sign double-encoded: %s", enc)
	}
	raw, derr := base64.StdEncoding.DecodeString(enc)
	if derr != nil {
		t.Fatalf("sign not base64: %v", derr)
	}
	if len(raw) != 32 {
		t.Fatalf("hmac-sha256 digest len = %d", len(raw))
	}
	if ts := u.Query().Get("timestamp"); ts != "1700000000000" {
		t.Fatalf("timestamp param = %q", ts)
	}
}

func TestSendDingTalk(t *testing.T) {
	s := testSettings()
	// 成功
	ok := func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s", req.Method)
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "hello") {
			t.Errorf("payload missing content: %s", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":0}`))}, nil
	}
	if err := SendDingTalk(ok, s, "hello"); err != nil {
		t.Fatalf("send ok: %v", err)
	}
	// errcode≠0 → 失败
	bad := func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":310,"errmsg":"bad token"}`))}, nil
	}
	if err := SendDingTalk(bad, s, "hello"); err == nil || !strings.Contains(err.Error(), "310") {
		t.Fatalf("errcode should fail: %v", err)
	}
	// HTTP 500 → 失败
	fail := func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	}
	if err := SendDingTalk(fail, s, "hello"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("http 500 should fail: %v", err)
	}
	// 无 webhook → 失败
	noHook := s
	noHook.DingTalkWebhook = ""
	if err := SendDingTalk(ok, noHook, "x"); err == nil {
		t.Fatal("missing webhook should fail")
	}
}
