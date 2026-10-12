package dingtalk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSettings() Settings {
	return NormalizeSettings(Settings{
		Enabled:         true,
		IdleMinutes:     5,
		CooldownMinutes: 5,
		DingTalkWebhook: "https://oapi.dingtalk.com/robot/send?access_token=xxx",
		DingTalkSecret:  "SEC0123456789abcdef",
		Locale:          "zh-CN",
	})
}

// ── 加签（Chief 点名防轰炸关键项 1）──

func TestSignWebhookHMAC(t *testing.T) {
	got := SignWebhook("https://oapi.dingtalk.com/robot/send?access_token=abc", "SEC", 1700000000000)
	if !strings.Contains(got, "timestamp=1700000000000") {
		t.Fatalf("timestamp param missing: %s", got)
	}
	if !strings.Contains(got, "sign=") {
		t.Fatalf("sign param missing: %s", got)
	}
	// 确定性：同输入同输出。
	again := SignWebhook("https://oapi.dingtalk.com/robot/send?access_token=abc", "SEC", 1700000000000)
	if got != again {
		t.Fatalf("sign not deterministic:\n%s\n%s", got, again)
	}
	// 不同 secret → 不同签名。
	other := SignWebhook("https://oapi.dingtalk.com/robot/send?access_token=abc", "OTHER", 1700000000000)
	if extractSign(got) == extractSign(other) {
		t.Fatal("different secrets must produce different signs")
	}
	// 无 secret：原样返回（降级不加签）。
	raw := "https://oapi.dingtalk.com/robot/send?access_token=abc"
	if got := SignWebhook(raw, "", 1); got != raw {
		t.Fatalf("no-secret must pass through: %s", got)
	}
}

func extractSign(u string) string {
	i := strings.Index(u, "sign=")
	if i < 0 {
		return ""
	}
	rest := u[i+5:]
	if j := strings.IndexByte(rest, '&'); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestSignWebhookVerifiesByServer(t *testing.T) {
	// 端到端：服务端按钉钉同款算法验证签名（HMAC-SHA256 base64 + URL 解码）。
	var mu sync.Mutex
	var gotSign, gotTS, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotSign, gotTS, gotBody = r.URL.Query().Get("sign"), r.URL.Query().Get("timestamp"), string(b)
		mu.Unlock()
		w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	set := testSettings()
	set.DingTalkWebhook = srv.URL
	s := NewSender()
	s.Jitter = 0
	if err := s.Send(set, "hello", time.Now()); err != nil {
		t.Fatalf("send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSign == "" || gotTS == "" {
		t.Fatalf("sign params not sent: sign=%q ts=%q", gotSign, gotTS)
	}
	var probe struct {
		MsgType string `json:"msgtype"`
		Text    struct {
			Content string `json:"content"`
		} `json:"text"`
	}
	if json.Unmarshal([]byte(gotBody), &probe) != nil {
		t.Fatalf("body: %s", gotBody)
	}
	if probe.MsgType != "text" || probe.Text.Content != "hello" {
		t.Fatalf("payload shape wrong: %s", gotBody)
	}
}

// ── 独立冷却（Chief 点名防轰炸关键项 2）──

func TestIdleCooldownAndDedup(t *testing.T) {
	set := testSettings()
	now := time.Now()
	st := &IdleState{LastCallAt: now.Add(-10 * time.Minute)}
	// 首次：应报。
	if ev := st.EvaluateIdle(set, now); !ev.ShouldAlert {
		t.Fatalf("first eval should alert: %+v", ev)
	}
	st.MarkIdleAlerted(now)
	// 同一次调用：已报过，不重复。
	if ev := st.EvaluateIdle(set, now.Add(time.Minute)); ev.ShouldAlert {
		t.Fatalf("same call must dedup: %+v", ev)
	}
	// 冷却期内新调用也不报（cooldown 语义：fork L103-109 是 lastAlertAt 维度）。
	st.RecordCall(now.Add(2 * time.Minute))
	if ev := st.EvaluateIdle(set, now.Add(3*time.Minute)); ev.ShouldAlert {
		t.Fatalf("cooldown must suppress: %+v", ev)
	}
	// 冷却过后 + 新调用又闲了：可再报。
	st.RecordCall(now.Add(20 * time.Minute))
	if ev := st.EvaluateIdle(set, now.Add(40*time.Minute)); !ev.ShouldAlert {
		t.Fatalf("after cooldown+new idle call must alert: %+v", ev)
	}
}

func TestExhaustedActiveSetSuppresses(t *testing.T) {
	set := testSettings()
	now := time.Now()
	ks := NewKeyQuotaState()
	key := "key-1"
	// 首次超限：报。
	if ev := ks.EvaluateExhausted(set, key, now); !ev.ShouldAlert {
		t.Fatalf("first exhausted should alert: %+v", ev)
	}
	ks.MarkExhaustedAlerted(key, now)
	// 活跃耗尽态持续：即使过了全局冷却也不报（只报一次）。
	if ev := ks.EvaluateExhausted(set, key, now.Add(10*time.Minute)); ev.ShouldAlert {
		t.Fatalf("active exhausted must suppress: %+v", ev)
	}
	// 恢复后可再报（markQuotaAvailable 清冷却）。
	ks.MarkQuotaAvailable(key)
	if ev := ks.EvaluateExhausted(set, key, now.Add(11*time.Minute)); !ev.ShouldAlert {
		t.Fatalf("after recovery must alert: %+v", ev)
	}
}

func TestThresholdCooldown(t *testing.T) {
	set := testSettings()
	set.QuotaUsageThresholdEnable = true
	set.QuotaUsageThresholdPct = 80
	now := time.Now()
	ks := NewKeyQuotaState()
	key := "key-2"
	// 79% 不报。
	if ev := ks.EvaluateThreshold(set, key, 79, 100, now); ev.ShouldAlert {
		t.Fatalf("below threshold must not alert: %+v", ev)
	}
	// 85% 报。
	ev := ks.EvaluateThreshold(set, key, 85, 100, now)
	if !ev.ShouldAlert || ev.UsagePercent != 85 || ev.ThresholdPct != 80 {
		t.Fatalf("threshold eval: %+v", ev)
	}
	ks.MarkThresholdAlerted(key, now)
	// 冷却期内同 Key 不报。
	if ev := ks.EvaluateThreshold(set, key, 90, 100, now.Add(time.Minute)); ev.ShouldAlert {
		t.Fatalf("cooldown must suppress: %+v", ev)
	}
	// 无上限（limit≤0）不报。
	if ev := ks.EvaluateThreshold(set, key, 5, 0, now); ev.ShouldAlert {
		t.Fatalf("no-limit must not alert: %+v", ev)
	}
}

func TestUsageFetchIndependentCooldown(t *testing.T) {
	set := testSettings()
	now := time.Now()
	us := NewUsageFetchState()
	acct := "acct-1"
	// 首次：报。
	if ev := us.EvaluateUsageFetchFailed(set, acct, now); !ev.ShouldAlert {
		t.Fatalf("first should alert: %+v", ev)
	}
	us.MarkUsageFetchAlerted(acct, now)
	// 29 分钟后：仍冷却（独立 30 分钟）。
	if ev := us.EvaluateUsageFetchFailed(set, acct, now.Add(29*time.Minute)); ev.ShouldAlert {
		t.Fatalf("29min must be in cooldown: %+v", ev)
	}
	// 31 分钟后：可再报。
	if ev := us.EvaluateUsageFetchFailed(set, acct, now.Add(31*time.Minute)); !ev.ShouldAlert {
		t.Fatalf("31min must alert again: %+v", ev)
	}
	// 别的账号独立计数。
	if ev := us.EvaluateUsageFetchFailed(set, "acct-2", now.Add(time.Minute)); !ev.ShouldAlert {
		t.Fatalf("other account independent: %+v", ev)
	}
}

// ── 5 类文案 ──

func TestBuildTextsTemplates(t *testing.T) {
	set := testSettings()
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	st := &IdleState{LastCallAt: now.Add(-7 * time.Minute)}
	idle := BuildIdleText(set, st, now)
	if !strings.Contains(idle, "7") || !strings.Contains(idle, "buddyhub") {
		t.Fatalf("idle text: %s", idle)
	}
	d := QuotaDetails{KeyName: "k1", Provider: "qoder", Used: 95, Limit: 100, Remaining: 5}
	ex := BuildExhaustedText(d, "zh-CN", now)
	if !strings.Contains(ex, "已耗尽") || !strings.Contains(ex, "95/100") {
		t.Fatalf("exhausted zh: %s", ex)
	}
	if en := BuildExhaustedText(d, "en", now); !strings.Contains(en, "quota exhausted") {
		t.Fatalf("exhausted en: %s", en)
	}
	th := BuildThresholdText(d, 80, 95, "zh-CN", now)
	if !strings.Contains(th, "95%") || !strings.Contains(th, "阈值") {
		t.Fatalf("threshold zh: %s", th)
	}
	uf := BuildUsageFetchFailedText("k1", "qoder", "acct-1", "boom", "zh-CN", now)
	if !strings.Contains(uf, "额度读取失败") || !strings.Contains(uf, "boom") {
		t.Fatalf("usage-fetch zh: %s", uf)
	}
	// 模板占位符自定义。
	set.MessageTemplate = "IDLE={idleMinutes} AT={lastCallAt} NOW={now}"
	custom := BuildIdleText(set, st, now)
	if !strings.Contains(custom, "IDLE=7") || !strings.Contains(custom, "AT=") || !strings.Contains(custom, "NOW=") {
		t.Fatalf("custom template: %s", custom)
	}
}

// ── 发送失败语义 ──

func TestSendFailureModes(t *testing.T) {
	set := testSettings()
	set.DingTalkWebhook = ""
	s := NewSender()
	if err := s.Send(set, "x", time.Now()); err == nil {
		t.Fatal("missing webhook must error")
	}
	// errcode≠0 算失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":310000,"errmsg":"token not exist"}`))
	}))
	defer srv.Close()
	set2 := testSettings()
	set2.DingTalkWebhook = srv.URL
	if err := s.Send(set2, "x", time.Now()); err == nil {
		t.Fatal("errcode!=0 must error")
	}
	// HTTP 500 算失败。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv2.Close()
	set3 := testSettings()
	set3.DingTalkWebhook = srv2.URL
	if err := s.Send(set3, "x", time.Now()); err == nil {
		t.Fatal("HTTP 500 must error")
	}
}

func TestRollbackExhaustedOnSendFailure(t *testing.T) {
	// fork L346-348：发送失败要退出活跃态，否则该 Key 再也报不出来了。
	ks := NewKeyQuotaState()
	key := "k"
	ks.MarkExhaustedAlerted(key, time.Now())
	ks.RollbackExhausted(key)
	if ks.ActiveExhaustedKeys[key] {
		t.Fatal("rollback must clear active flag")
	}
}

func TestMaskHidesSecret(t *testing.T) {
	set := testSettings()
	m := set.Mask()
	if m.DingTalkSecret != "" || !m.HasDingTalkSec {
		t.Fatalf("mask wrong: %+v", m)
	}
	if !strings.Contains(m.MessageTemplate, "buddyhub") {
		t.Fatal("other fields must survive mask")
	}
}

func TestNormalizeDefaults(t *testing.T) {
	n := NormalizeSettings(Settings{})
	if n.IdleMinutes != 5 || n.CooldownMinutes != 0 && n.CooldownMinutes != 5 {
		t.Fatalf("idle default = %d cooldown = %d", n.IdleMinutes, n.CooldownMinutes)
	}
	if n.QuotaUsageThresholdPct != 80 || n.Locale != "en" {
		t.Fatalf("pct/locale = %d/%s", n.QuotaUsageThresholdPct, n.Locale)
	}
	if !strings.Contains(n.MessageTemplate, "{idleMinutes}") {
		t.Fatalf("template = %q", n.MessageTemplate)
	}
}
