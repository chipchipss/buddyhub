package zcode

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mockClient(status int, body string, seen *[]*http.Request) *Client {
	return &Client{Do: func(req *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, req)
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}}
}

func TestStartRedirectKeyPerSite(t *testing.T) {
	// fork L886：bigmodel 用 redirect，zai 用 redirect_uri——必进单测。
	var seen []*http.Request
	c := mockClient(200, `{"flow_id":"f1","authorize_url":"https://z.example/authorize","poll_interval_sec":2,"expires_at":`+itoa(time.Now().Add(10*time.Minute).UnixMilli())+`}`, &seen)
	if _, err := c.Start(context.Background(), SiteBigModel, "dev-1"); err != nil {
		t.Fatalf("start bigmodel: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("calls = %d", len(seen))
	}
	raw, _ := io.ReadAll(seen[0].Body)
	if !strings.Contains(string(raw), "redirect=") || strings.Contains(string(raw), "redirect_uri=") {
		t.Fatalf("bigmodel must use redirect=: %s", raw)
	}

	seen = nil
	c2 := mockClient(200, `{"flow_id":"f2","authorize_url":"https://z.example/authorize"}`, &seen)
	s2, err := c2.Start(context.Background(), SiteZai, "dev-1")
	if err != nil {
		t.Fatalf("start zai: %v", err)
	}
	raw2, _ := io.ReadAll(seen[0].Body)
	if !strings.Contains(string(raw2), "redirect_uri=") {
		t.Fatalf("zai must use redirect_uri=: %s", raw2)
	}
	if s2.Deadline.IsZero() || s2.Deadline.Before(time.Now()) {
		t.Fatalf("missing expires_at should default +10min: %v", s2.Deadline)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestPollTokenFieldPerSite(t *testing.T) {
	// fork L905：bigmodel 取 got.bigmodel.access_token，zai 取 got.zai.access_token。
	s := &DeviceSession{Site: SiteBigModel, PollToken: "Bearer aa", FlowID: "f1", Interval: time.Millisecond}
	c := mockClient(200, `{"status":"ready","bigmodel":{"access_token":"tok-bm"}}`, nil)
	r := c.Poll(context.Background(), s)
	if r.Verdict != PollReady || r.Cred == nil || r.Cred.AccessToken != "tok-bm" {
		t.Fatalf("bigmodel ready: %+v", r)
	}

	s2 := &DeviceSession{Site: SiteZai, PollToken: "Bearer aa", FlowID: "f2", Interval: time.Millisecond}
	c2 := mockClient(200, `{"status":"ready","zai":{"access_token":"tok-zai"}}`, nil)
	r2 := c2.Poll(context.Background(), s2)
	if r2.Verdict != PollReady || r2.Cred == nil || r2.Cred.AccessToken != "tok-zai" {
		t.Fatalf("zai ready: %+v", r2)
	}
	// ready 但 token 字段在错误的站点路径下 → failed
	c3 := mockClient(200, `{"status":"ready","zai":{"access_token":"tok-x"}}`, nil)
	r3 := c3.Poll(context.Background(), s)
	if r3.Verdict != PollFailed {
		t.Fatalf("wrong-site token must fail: %+v", r3)
	}
}

func TestPollVerdicts(t *testing.T) {
	s := &DeviceSession{Site: SiteBigModel, PollToken: "Bearer aa", FlowID: "f", Interval: time.Millisecond}
	if r := mockClient(200, `{"status":"pending"}`, nil).Poll(context.Background(), s); r.Verdict != PollPending {
		t.Fatalf("pending → %v", r.Verdict)
	}
	if r := mockClient(200, `{"status":"failed"}`, nil).Poll(context.Background(), s); r.Verdict != PollFailed {
		t.Fatalf("failed → %v", r.Verdict)
	}
	// 4xx（除 408/429）→ Gone
	if r := mockClient(404, `nope`, nil).Poll(context.Background(), s); r.Verdict != PollGone {
		t.Fatalf("404 → %v", r.Verdict)
	}
	// 408/429 → Retry
	if r := mockClient(408, `timeout`, nil).Poll(context.Background(), s); r.Verdict != PollRetry {
		t.Fatalf("408 → %v", r.Verdict)
	}
	if r := mockClient(429, `slow`, nil).Poll(context.Background(), s); r.Verdict != PollRetry {
		t.Fatalf("429 → %v", r.Verdict)
	}
	// 5xx → Retry
	if r := mockClient(503, `oops`, nil).Poll(context.Background(), s); r.Verdict != PollRetry {
		t.Fatalf("503 → %v", r.Verdict)
	}
	// 传输错误 → Retry
	c := &Client{Do: func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }}
	if r := c.Poll(context.Background(), s); r.Verdict != PollRetry {
		t.Fatalf("transport → %v", r.Verdict)
	}
}

func TestWaitFlow(t *testing.T) {
	step := 0
	c := &Client{Do: func(*http.Request) (*http.Response, error) {
		step++
		if step < 3 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"pending"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"ready","zai":{"access_token":"ok"}}`))}, nil
	}}
	s := &DeviceSession{Site: SiteZai, PollToken: "Bearer aa", FlowID: "f", Interval: time.Millisecond, Deadline: time.Now().Add(time.Minute)}
	cred, err := c.Wait(context.Background(), s)
	if err != nil || cred == nil || cred.AccessToken != "ok" {
		t.Fatalf("wait = %+v %v", cred, err)
	}
	// Gone
	c2 := mockClient(404, `nope`, nil)
	_, err = c2.Wait(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "失效") {
		t.Fatalf("gone err = %v", err)
	}
}

// ── 凭据形态（fork L919-921 / jwtExpiry L110-123）──

func TestCredentialExpirySemantics(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	// API key：永不失效
	k := FromAPIKey("sk-x")
	if k.Lapsed(now) {
		t.Fatal("api key never lapses")
	}
	if k.BearerToken() != "sk-x" {
		t.Fatal("api key preferred as bearer")
	}
	// JWT：HS256 形状的三段 token，exp 在 payload
	// {"exp":1700000100} → b64
	payload := base64RawURL(`{"exp":1700000100}`)
	jwt := "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
	j := FromJWT(jwt)
	// exp=1700000100 秒 → ×1000 = 1_700_000_100_000 ms。
	if j.ExpireAtMS != 1_700_000_100_000 {
		t.Fatalf("jwt expiry = %d", j.ExpireAtMS)
	}
	if j.Lapsed(now) {
		t.Fatal("not yet lapsed at 1700000000000")
	}
	if !j.Lapsed(time.UnixMilli(1_700_000_200_000)) {
		t.Fatal("should lapse after exp")
	}
	// 非三段 → exp=0 永不过期（fork：别误判）
	junk := FromJWT("not-a-jwt")
	if junk.ExpireAtMS != 0 || junk.Lapsed(now) {
		t.Fatalf("junk jwt must be never-expiring: %d", junk.ExpireAtMS)
	}
	// 无 exp claim → 0
	noExp := FromJWT("eyJhbGciOiJIUzI1NiJ9." + base64RawURL(`{"sub":"u"}`) + ".sig")
	if noExp.ExpireAtMS != 0 || noExp.Lapsed(now) {
		t.Fatal("missing exp must be never-expiring")
	}
	// JWT 模式 Bearer 取 access_token
	if j.BearerToken() != jwt {
		t.Fatal("jwt bearer = access_token")
	}
}

func base64RawURL(s string) string {
	// 测试专用：把 JSON 串转成 base64url（无 padding）。
	return b64(s)
}
