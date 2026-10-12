package minimax

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
)

// roundTrip 构造固定响应的 mock client。
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

func TestStartDeviceAndExpiresVariants(t *testing.T) {
	// 变体②：expires_in 相对秒
	c := mockClient(200, `{"device_code":"dc1","user_code":"ABCD","verification_uri":"https://account.minimax.cn/activate","expires_in":600,"interval":5000}`, nil)
	s, err := c.StartDevice(context.Background(), SiteCN)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.DeviceCode != "dc1" || s.UserCode != "ABCD" {
		t.Fatalf("session = %+v", s)
	}
	if remain := time.Until(s.ExpiresAt); remain < 9*time.Minute || remain > 10*time.Minute+time.Second {
		t.Fatalf("expires_in=600s parsed as %v", remain)
	}
	if s.Interval != 5*time.Second {
		t.Fatalf("interval = %v", s.Interval)
	}
	// 变体②：expires_in 绝对毫秒（≥1e12）
	future := time.Now().Add(30 * time.Minute).UnixMilli()
	c2 := mockClient(200, `{"device_code":"dc2","user_code":"EFGH","expires_in":`+strconv.FormatInt(future, 10)+`,"interval":3000}`, nil)
	s2, err := c2.StartDevice(context.Background(), SiteGlobal)
	if err != nil {
		t.Fatalf("start2: %v", err)
	}
	if remain := time.Until(s2.ExpiresAt); remain < 29*time.Minute {
		t.Fatalf("absolute ms parsed wrong: %v", remain)
	}
	if s2.Interval != 3*time.Second {
		t.Fatalf("ms interval = %v", s2.Interval)
	}
}

func TestStartDeviceUserCodeFallbackAndExpiredInUnits(t *testing.T) {
	// Fork variant: no device_code; user_code+expired_in -> poll by user_code.
	var seen []*http.Request
	seconds := int64(600)
	c := mockClient(200, `{"user_code":"USER-1","verification_url":"https://account.minimax.io/device","expired_in":`+strconv.FormatInt(seconds, 10)+`,"interval":5000}`, &seen)
	s, err := c.StartDevice(context.Background(), SiteGlobal)
	if err != nil {
		t.Fatalf("start user-code variant: %v", err)
	}
	if !s.ByUserCode || s.DeviceCode != "" || s.UserCode != "USER-1" {
		t.Fatalf("session = %+v", s)
	}
	if remain := time.Until(s.ExpiresAt); remain < 9*time.Minute || remain > 10*time.Minute+time.Second {
		t.Fatalf("expired_in seconds parsed as %v", remain)
	}

	// Poll must send user_code only, never fabricate device_code.
	seen = nil
	poller := mockClient(400, `{"error":"authorization_pending"}`, &seen)
	out := poller.Poll(context.Background(), s)
	if out.Status != authcore.PollPending || len(seen) != 1 {
		t.Fatalf("poll = %+v seen=%d", out, len(seen))
	}
	form, _ := io.ReadAll(seen[0].Body)
	if !strings.Contains(string(form), "user_code=USER-1") || strings.Contains(string(form), "device_code=") {
		t.Fatalf("poll form = %s", form)
	}

	// expired_in absolute milliseconds (>=1e12) means timestamp, not seconds.
	futureMS := time.Now().Add(20 * time.Minute).UnixMilli()
	c2 := mockClient(200, `{"user_code":"USER-2","expired_in":`+strconv.FormatInt(futureMS, 10)+`}`, nil)
	s2, err := c2.StartDevice(context.Background(), SiteGlobal)
	if err != nil {
		t.Fatalf("start absolute expired_in: %v", err)
	}
	if remain := time.Until(s2.ExpiresAt); remain < 19*time.Minute || remain > 20*time.Minute+time.Second {
		t.Fatalf("absolute expired_in ms parsed as %v", remain)
	}
}

func TestStartDeviceRejectsIncompleteUserCodeVariant(t *testing.T) {
	// user_code without expired_in is not the fork fallback contract.
	c := mockClient(200, `{"user_code":"U","expires_in":600}`, nil)
	if _, err := c.StartDevice(context.Background(), SiteGlobal); err == nil {
		t.Fatal("must reject missing device_code without expired_in")
	}
}

func TestPollFiveStates(t *testing.T) {
	s := &DeviceSession{Site: SiteCN, DeviceCode: "dc", Verifier: "v", Interval: time.Millisecond, ExpiresAt: time.Now().Add(time.Minute)}
	// pending
	c := mockClient(400, `{"error":"authorization_pending"}`, nil)
	if o := c.Poll(context.Background(), s); o.Status != authcore.PollPending {
		t.Fatalf("pending → %v", o.Status)
	}
	// slow_down
	c = mockClient(429, `{"error":"slow_down"}`, nil)
	if o := c.Poll(context.Background(), s); o.Status != authcore.PollSlowDown {
		t.Fatalf("slow_down → %v", o.Status)
	}
	// done → cred
	c = mockClient(200, `{"access_token":"at1","refresh_token":"rt1","expires_in":3600}`, nil)
	o := c.Poll(context.Background(), s)
	if o.Status != authcore.PollDone || o.Cred == nil || o.Cred.AccessToken != "at1" {
		t.Fatalf("done → %+v", o)
	}
	if remain := time.Until(time.UnixMilli(o.Cred.ExpireAtMS)); remain < 59*time.Minute {
		t.Fatalf("expires_in=3600s → %v", remain)
	}
}

func TestPollUserCodeFallback(t *testing.T) {
	var seen []*http.Request
	// 无 device_code：变体①，body 用 user_code
	c := mockClient(400, `{"error":"authorization_pending"}`, &seen)
	s := &DeviceSession{Site: SiteCN, UserCode: "ABCD", ByUserCode: true, Interval: time.Millisecond, ExpiresAt: time.Now().Add(time.Minute)}
	c.Poll(context.Background(), s)
	if len(seen) != 1 {
		t.Fatalf("calls = %d", len(seen))
	}
	raw, _ := io.ReadAll(seen[0].Body)
	if !strings.Contains(string(raw), "user_code=ABCD") || strings.Contains(string(raw), "device_code=") {
		t.Fatalf("user_code fallback body = %s", raw)
	}
}

func TestRefreshLogoutPredicate(t *testing.T) {
	cred := &Credential{AccessToken: "old", RefreshToken: "rt", ExpireAtMS: 1}
	// invalid_grant + 400 → 登出
	c := mockClient(400, `{"error":"invalid_grant"}`, nil)
	_, loggedOut, err := c.Refresh(context.Background(), SiteCN, cred)
	if err == nil || !loggedOut {
		t.Fatalf("invalid_grant should logout: %v %v", loggedOut, err)
	}
	// 5xx → 保留登录态
	c = mockClient(503, `oops`, nil)
	_, loggedOut, err = c.Refresh(context.Background(), SiteCN, cred)
	if err == nil || loggedOut {
		t.Fatalf("503 must NOT logout: %v %v", loggedOut, err)
	}
	// 成功 → 新凭据
	c = mockClient(200, `{"access_token":"at2","refresh_token":"rt2","expires_in":7200}`, nil)
	fresh, loggedOut, err := c.Refresh(context.Background(), SiteCN, cred)
	if err != nil || loggedOut || fresh.AccessToken != "at2" || fresh.RefreshToken != "rt2" {
		t.Fatalf("refresh = %+v %v %v", fresh, loggedOut, err)
	}
	// 无 refresh_token → 报错不登出
	_, loggedOut, err = c.Refresh(context.Background(), SiteCN, &Credential{AccessToken: "x"})
	if err == nil || loggedOut {
		t.Fatalf("no rt = %v %v", loggedOut, err)
	}
}

func TestWaitDeviceFlow(t *testing.T) {
	// 两轮 pending 后 done
	step := 0
	c := &Client{Do: func(req *http.Request) (*http.Response, error) {
		step++
		if step < 3 {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"authorization_pending"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"ok","refresh_token":"r","expires_in":60}`))}, nil
	}}
	s := &DeviceSession{Site: SiteCN, DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: time.Now().Add(time.Minute)}
	cred, err := c.WaitDevice(context.Background(), s)
	if err != nil || cred == nil || cred.AccessToken != "ok" {
		t.Fatalf("wait = %+v %v", cred, err)
	}
	// 拒绝授权
	c2 := &Client{Do: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":"access_denied"}`))}, nil
	}}
	_, err = c2.WaitDevice(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("denied err = %v", err)
	}
	// 过期
	c3 := &Client{Do: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"expired_token"}`))}, nil
	}}
	_, err = c3.WaitDevice(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("expired err = %v", err)
	}
}
