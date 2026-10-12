package server

import (
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/dingtalk"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 阶段 5：桥接层告警接线测试 ─────────────────────────────
 * 不发真 HTTP（Sender 无 server）——验证的是状态机接线：
 * 耗尽→告警一次→活跃态抑制；记账→阈值判定；调用记录→闲时事实源。
 */

func TestQoderExhaustedAlertOnceThenSuppressed(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	set := &dingtalk.Settings{
		Enabled: true, CooldownMinutes: 5, DingTalkWebhook: "https://example.invalid/hook",
	}
	h.cfg.DingTalk = set
	key := qoder.AccountKey("qoder", "acct-a")
	h.qoderLedger.SetAllocation(key, 100)
	h.qoderLedger.RecordCredits(key, 100)

	// 无 webhook server：发送必失败——但状态机先 Mark 再 Rollback，
	// 因此这里只验证"评过、试图发过"（日志可见），重点验抑制语义的另一面：
	// 手动预置活跃态后 PRE 门告警判定不再触发。
	h.qoderKeyQuota.MarkExhaustedAlerted(key, time.Now())
	ev := h.qoderKeyQuota.EvaluateExhausted(h.qoderAlertCfg(), key, time.Now())
	if ev.ShouldAlert {
		t.Fatal("active exhausted must suppress alert")
	}
	// 恢复后可再判。
	h.qoderKeyQuota.MarkQuotaAvailable(key)
	if ev := h.qoderKeyQuota.EvaluateExhausted(h.qoderAlertCfg(), key, time.Now()); !ev.ShouldAlert {
		t.Fatalf("after recovery must alert: %+v", ev)
	}
}

func TestQoderThresholdAlertAfterRecord(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	set := &dingtalk.Settings{
		Enabled: true, CooldownMinutes: 5, QuotaUsageThresholdEnable: true,
		QuotaUsageThresholdPct: 80, DingTalkWebhook: "https://example.invalid/hook",
	}
	h.cfg.DingTalk = set
	key := qoder.AccountKey("qoder", "acct-b")
	h.qoderLedger.SetAllocation(key, 100)
	h.qoderLedger.RecordCredits(key, 85) // 85% ≥ 80%

	ev := h.qoderKeyQuota.EvaluateThreshold(h.qoderAlertCfg(), key, 85, 100, time.Now())
	if !ev.ShouldAlert || ev.UsagePercent != 85 {
		t.Fatalf("threshold eval: %+v", ev)
	}
	// 冷却抑制。
	h.qoderKeyQuota.MarkThresholdAlerted(key, time.Now())
	if ev := h.qoderKeyQuota.EvaluateThreshold(h.qoderAlertCfg(), key, 90, 100, time.Now()); ev.ShouldAlert {
		t.Fatalf("cooldown must suppress: %+v", ev)
	}
}

func TestQoderRecordCallFeedsIdleState(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	h.qoderRecordCall()
	if h.qoderIdle.LastCallAt.IsZero() {
		t.Fatal("record call must stamp LastCallAt")
	}
	// 未启用时闲时判定短路。
	if ev := h.qoderIdle.EvaluateIdle(h.qoderAlertCfg(), time.Now().Add(time.Hour)); ev.ShouldAlert {
		t.Fatalf("disabled must not alert: %+v", ev)
	}
	// 启用 + 超闲时才报。
	h.cfg.DingTalk = &dingtalk.Settings{Enabled: true, IdleMinutes: 5, DingTalkWebhook: "https://example.invalid/hook"}
	ev := h.qoderIdle.EvaluateIdle(h.qoderAlertCfg(), h.qoderIdle.LastCallAt.Add(10*time.Minute))
	if !ev.ShouldAlert {
		t.Fatalf("idle 10min must alert: %+v", ev)
	}
}

func TestQoderAlertDisabledByDefault(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	if h.qoderAlertCfg().Enabled {
		t.Fatal("alerts must be disabled by default")
	}
	// 全关闭路径不 panic。
	h.qoderRecordCall()
	acct := &extstore.ExtAccount{Provider: extstore.PQoder, ID: "x"}
	h.qoderAlertQuotaExhausted(acct, 100, 100, 0)
	h.qoderAlertQuotaThreshold(acct, 100, 100)
}
