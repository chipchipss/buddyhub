package authcore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPKCE(t *testing.T) {
	v, err := NewVerifier(64)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if len(v) != 64 {
		t.Fatalf("verifier len = %d", len(v))
	}
	c1 := ChallengeS256(v)
	c2 := ChallengeS256(v)
	if c1 != c2 || len(c1) != 43 {
		t.Fatalf("challenge not stable S256 base64url: %q", c1)
	}
	// 两次生成必须不同（随机源活着）
	v2, _ := NewVerifier(64)
	if v == v2 {
		t.Fatal("verifier not random")
	}
	// 边界钳制
	if s, _ := NewVerifier(1); len(s) != 32 {
		t.Fatalf("min clamp = %d", len(s))
	}
}

func TestExpiresAtMS(t *testing.T) {
	// 秒 → 毫秒
	if got := ExpiresAtMS(float64(1_700_000_000)); got != 1_700_000_000_000 {
		t.Fatalf("seconds: %d", got)
	}
	// 已是毫秒
	if got := ExpiresAtMS(float64(1_700_000_000_000)); got != 1_700_000_000_000 {
		t.Fatalf("millis: %d", got)
	}
	// 非法
	if got := ExpiresAtMS("junk"); got != 0 {
		t.Fatalf("junk: %d", got)
	}
	if got := ExpiresAtMS(float64(-5)); got != 0 {
		t.Fatalf("negative: %d", got)
	}
	// 字符串数字
	if got := ExpiresAtMS("1700000000"); got != 1_700_000_000_000 {
		t.Fatalf("string seconds: %d", got)
	}
	// RFC3339
	if got := ExpiresAtMS("2026-01-01T00:00:00Z"); got == 0 {
		t.Fatal("RFC3339 failed")
	}
}

func TestClassifyPollResponse(t *testing.T) {
	// 五态各一
	if o := ClassifyPollResponse(200, []byte(`{"token":"x"}`), nil); o.Status != PollDone {
		t.Fatalf("200 → %v", o.Status)
	}
	if o := ClassifyPollResponse(400, []byte(`{"error":"authorization_pending"}`), nil); o.Status != PollPending {
		t.Fatalf("pending → %v", o.Status)
	}
	if o := ClassifyPollResponse(400, []byte(`{"error":"slow_down"}`), nil); o.Status != PollSlowDown {
		t.Fatalf("slow_down → %v", o.Status)
	}
	if o := ClassifyPollResponse(403, []byte(`{"error":"access_denied"}`), nil); o.Status != PollDenied {
		t.Fatalf("denied → %v", o.Status)
	}
	if o := ClassifyPollResponse(400, []byte(`{"error":"expired_token"}`), nil); o.Status != PollExpiredToken {
		t.Fatalf("expired → %v", o.Status)
	}
	if o := ClassifyPollResponse(503, nil, nil); o.Status != PollError {
		t.Fatalf("5xx → %v", o.Status)
	}
	if o := ClassifyPollResponse(0, nil, errors.New("timeout")); o.Status != PollError {
		t.Fatalf("transport → %v", o.Status)
	}
	// 400 未知错误码 → Error（继续轮，不当掉号）
	if o := ClassifyPollResponse(400, []byte(`{"error":"bad_request"}`), nil); o.Status != PollError {
		t.Fatalf("unknown 400 → %v", o.Status)
	}
}

func TestNextIntervalAndNormalize(t *testing.T) {
	if d := NextInterval(0, false); d != 5*time.Second {
		t.Fatalf("default interval = %v", d)
	}
	if d := NextInterval(5*time.Second, true); d != 10*time.Second {
		t.Fatalf("slow_down penalty = %v", d)
	}
	// MiniMax 毫秒 interval
	if d := NormalizeIntervalMS(5000); d != 5*time.Second {
		t.Fatalf("ms interval = %v", d)
	}
	// 其他实现秒 interval
	if d := NormalizeIntervalMS(5); d != 5*time.Second {
		t.Fatalf("s interval = %v", d)
	}
	if d := NormalizeIntervalMS(0); d != 5*time.Second {
		t.Fatalf("zero interval = %v", d)
	}
}

func TestPollLoopDeadline(t *testing.T) {
	// pending 两轮后 done
	calls := 0
	out, done, err := PollLoop(context.Background(),
		time.Now().Add(10*time.Second), time.Millisecond,
		func(context.Context) PollOutcome {
			calls++
			if calls < 3 {
				return PollOutcome{Status: PollPending}
			}
			return PollOutcome{Status: PollDone, Payload: []byte(`ok`)}
		})
	if err != nil || !done || calls != 3 || string(out.Payload) != "ok" {
		t.Fatalf("loop = done=%v calls=%d err=%v", done, calls, err)
	}
	// denied 终止
	_, done, err = PollLoop(context.Background(), time.Now().Add(10*time.Second), time.Millisecond,
		func(context.Context) PollOutcome { return PollOutcome{Status: PollDenied} })
	if err != nil || done {
		t.Fatalf("denied = done=%v err=%v", done, err)
	}
	// deadline 打断 pending 循环
	_, _, err = PollLoop(context.Background(), time.Now().Add(50*time.Millisecond), time.Millisecond,
		func(context.Context) PollOutcome { return PollOutcome{Status: PollPending} })
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("deadline err = %v", err)
	}
	// slow_down 拉长间隔（过 deadline）
	_, _, err = PollLoop(context.Background(), time.Now().Add(50*time.Millisecond), time.Second,
		func(context.Context) PollOutcome { return PollOutcome{Status: PollSlowDown} })
	if err == nil {
		t.Fatal("slow_down beyond deadline should stop")
	}
}

func TestLeadMode(t *testing.T) {
	p := DefaultLeadPolicy() // early 5m / renew 10m
	now := time.UnixMilli(1_700_000_000_000)
	// 无过期信息 → 不动
	if m := p.Mode(0, now); m != LeadNone {
		t.Fatalf("no-expiry → %v", m)
	}
	// 已过期
	if m := p.Mode(now.UnixMilli()-1000, now); m != LeadOverdue {
		t.Fatalf("overdue → %v", m)
	}
	// 剩 3 分钟 → Early
	if m := p.Mode(now.UnixMilli()+3*60_000, now); m != LeadEarly {
		t.Fatalf("early → %v", m)
	}
	// 剩 8 分钟 → Renew
	if m := p.Mode(now.UnixMilli()+8*60_000, now); m != LeadRenew {
		t.Fatalf("renew → %v", m)
	}
	// 剩 30 分钟 → None
	if m := p.Mode(now.UnixMilli()+30*60_000, now); m != LeadNone {
		t.Fatalf("none → %v", m)
	}
}

func TestSpentCache(t *testing.T) {
	c := NewSpentCache(50 * time.Millisecond)
	// 空 token 忽略
	c.Put("", SpentEntry{Cred: []byte("x")})
	if _, ok := c.Get(""); ok {
		t.Fatal("empty token should not cache")
	}
	// 命中
	c.Put("old-tok", SpentEntry{Cred: []byte("new")})
	e, ok := c.Get("old-tok")
	if !ok || string(e.Cred) != "new" {
		t.Fatalf("hit = %v %v", ok, e)
	}
	// TTL 过期
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get("old-tok"); ok {
		t.Fatal("expired entry should evict")
	}
	// 失败结果也缓存（防坏 token 被并发轰炸）
	c.Put("bad-tok", SpentEntry{Err: "401"})
	e, ok = c.Get("bad-tok")
	if !ok || e.Err == "" {
		t.Fatal("failure result should cache")
	}
}

func TestLogoutPredicate(t *testing.T) {
	// MiniMax 口径：仅 invalid_grant + 400
	if !LogoutPredicate(400, []byte(`{"error":"invalid_grant"}`)) {
		t.Fatal("invalid_grant+400 should logout")
	}
	if LogoutPredicate(401, []byte(`{"error":"invalid_grant"}`)) {
		t.Fatal("401 should NOT logout (transient)")
	}
	if LogoutPredicate(500, []byte(`{"error":"invalid_grant"}`)) {
		t.Fatal("5xx should NOT logout")
	}
	if LogoutPredicate(400, []byte(`{"error":"slow_down"}`)) {
		t.Fatal("other 400 codes should NOT logout")
	}
}
