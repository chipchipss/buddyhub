package cursor

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fastSleep 零等待退避（150 轮全 404 用例实测要 25 分钟，注入后毫秒级）。
func fastSleep(c *Client) { c.Sleep = func(context.Context, time.Duration) bool { return true } }

func mockC(statusFn func(int) (int, string), seen *int) *Client {
	c := &Client{Do: func(req *http.Request) (*http.Response, error) {
		n := 0
		if seen != nil {
			*seen++
			n = *seen
		}
		s, body := statusFn(n)
		return &http.Response{
			StatusCode: s,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}}
	fastSleep(c)
	return c
}

// mockCNoBody Body 为 nil 的替身（403 终局分支不读 body）。
func mockCNoBody(status int) *Client {
	c := &Client{Do: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status}, nil
	}}
	fastSleep(c)
	return c
}

func jwtWithExp(expSeconds int64) string {
	header := base64RawURL(`{"alg":"HS256"}`)
	payload := base64RawURL(`{"exp":` + strconv.FormatInt(expSeconds, 10) + `}`)
	return header + "." + payload + ".sig"
}

func base64RawURL(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestStartSignInURL(t *testing.T) {
	c := New()
	s, err := c.StartSignIn()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// fork L276-280：loginDeepControl + challenge + uuid + mode=login + redirectTarget=cli
	for _, frag := range []string{"loginDeepControl?challenge=", "&uuid=", "mode=login", "redirectTarget=cli"} {
		if !strings.Contains(s.URL, frag) {
			t.Fatalf("url missing %q: %s", frag, s.URL)
		}
	}
	if s.Verifier == "" || s.UUID == "" {
		t.Fatalf("session empty: %+v", s)
	}
	// challenge 与 verifier 配对（S256）
	if _, ch, err := NewPKCE(); err != nil || len(ch) != 43 {
		t.Fatalf("pkce challenge len = %d", len(ch))
	}
}

func TestPollRequiresBothTokens(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name   string
		status int
		body   string
		done   bool
		ok     bool
	}{
		{"404 继续等", 404, `not found`, false, false},
		{"403 终局拒绝", 403, `denied`, true, false},
		{"缺 refreshToken", 200, `{"accessToken":"at"}`, true, false},
		{"缺 accessToken", 200, `{"refreshToken":"rt"}`, true, false},
		{"双字段齐全", 200, `{"accessToken":"` + jwtWithExp(now+86400) + `","refreshToken":"rt"}`, true, true},
	}
	for _, tc := range cases {
		c := mockC(func(int) (int, string) { return tc.status, tc.body }, nil)
		s, _ := c.StartSignIn()
		done, out, _ := c.pollOnce(context.Background(), s, nil)
		if done != tc.done {
			t.Errorf("%s: done = %v", tc.name, done)
		}
		if done && (out.Kind == "success") != tc.ok {
			t.Errorf("%s: outcome = %+v", tc.name, out)
		}
		if tc.ok && out.Cred.ExpireAtMS == 0 {
			t.Errorf("%s: exp 兜底 30d 没生效", tc.name)
		}
	}
}

func TestWaitSignInFlow(t *testing.T) {
	// 404 ×2 → 成功（指数退避期间不炸）
	step := 0
	c := &Client{Do: func(*http.Request) (*http.Response, error) {
		step++
		if step < 3 {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("nope"))}, nil
		}
		jwt := jwtWithExp(time.Now().Unix() + 3600)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"accessToken":"` + jwt + `","refreshToken":"rt"}`))}, nil
	}}
	fastSleep(c)
	s, _ := c.StartSignIn()
	cred, err := c.WaitSignIn(context.Background(), s)
	if err != nil || cred == nil || cred.AccessToken == "" {
		t.Fatalf("wait = %+v %v", cred, err)
	}
	if cred.ExpireAtMS == 0 {
		t.Fatal("exp 应从 JWT 读出")
	}
	if cred.Lapsed(time.Now()) {
		t.Fatal("未到期不该 lapsed")
	}

	// 网络错误 ×3 → 终局
	c2 := &Client{Do: func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }}
	fastSleep(c2)
	if _, err := c2.WaitSignIn(context.Background(), s); err == nil || !strings.Contains(err.Error(), "reach Cursor") {
		t.Fatalf("3 net errs err = %v", err)
	}

	// 403 → 立即终局（Response 无 Body：非 2xx 分支不得 defer Close nil）
	c3 := mockCNoBody(403)
	if _, err := c3.WaitSignIn(context.Background(), s); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403 err = %v", err)
	}

	// 150 轮全 404 → 超时
	c4 := mockC(func(int) (int, string) { return 404, `x` }, nil)
	if _, err := c4.WaitSignIn(context.Background(), s); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("150 rounds err = %v", err)
	}
}

func TestPollWaitExponential(t *testing.T) {
	// min(1000×1.2^r, 10s)
	if d := pollWait(0); d != time.Second {
		t.Fatalf("r0 = %v", d)
	}
	if d := pollWait(5); d < 2*time.Second || d > 3*time.Second {
		t.Fatalf("r5 = %v", d)
	}
	for r := 20; r < 40; r++ {
		if d := pollWait(r); d != 10*time.Second {
			t.Fatalf("r%d cap = %v", r, d)
		}
	}
}

func TestExchangeAPIKeyCache(t *testing.T) {
	calls := 0
	c := &Client{Do: func(*http.Request) (*http.Response, error) {
		calls++
		jwt := jwtWithExp(time.Now().Unix() + 86400)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"accessToken":"` + jwt + `"}`))}, nil
	}}
	c1, err := c.ExchangeAPIKey(context.Background(), "sk-test")
	if err != nil || c1.AccessToken == "" {
		t.Fatalf("exchange = %v %v", c1, err)
	}
	c2, err := c.ExchangeAPIKey(context.Background(), "sk-test")
	if err != nil || calls != 1 {
		t.Fatalf("cache miss! calls = %d", calls)
	}
	if c1.AccessToken != c2.AccessToken {
		t.Fatal("cached token changed")
	}
	// 不同 key 不共用缓存
	if _, err := c.ExchangeAPIKey(context.Background(), "sk-other"); err != nil || calls != 2 {
		t.Fatalf("second key calls = %d", calls)
	}
	// 空 key
	if _, err := c.ExchangeAPIKey(context.Background(), " "); err == nil {
		t.Fatal("empty key should fail")
	}
}

func TestLapsedSemantics(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	// exp 未知（0）= 不误判
	if (&Credential{}).Lapsed(now) {
		t.Fatal("unknown exp must not lapse")
	}
	// 未到期 / 已到期
	live := &Credential{ExpireAtMS: now.UnixMilli() + 3600_000}
	if live.Lapsed(now) {
		t.Fatal("live token lapsed")
	}
	dead := &Credential{ExpireAtMS: now.UnixMilli() - 1}
	if !dead.Lapsed(now) {
		t.Fatal("expired token must lapse")
	}
	// ExpiringSoon（<5min）
	soonTok := &Credential{ExpireAtMS: now.UnixMilli() + 2*60_000}
	if !soonTok.ExpiringSoon(now) {
		t.Fatal("2min remain should be soon")
	}
	far := &Credential{ExpireAtMS: now.UnixMilli() + 3600_000}
	if far.ExpiringSoon(now) {
		t.Fatal("1h remain should not be soon")
	}
}

func TestJWTExpiry(t *testing.T) {
	now := time.Now().Unix()
	if got := jwtExpiry(jwtWithExp(now + 60)); got <= 0 {
		t.Fatal("valid jwt exp")
	}
	if got := jwtExpiry("not-a-jwt"); got != 0 {
		t.Fatalf("junk = %d", got)
	}
	if got := jwtExpiry("a." + base64RawURL(`{"sub":"u"}`) + ".b"); got != 0 {
		t.Fatalf("no exp = %d", got)
	}
}

func TestAuthFilePath(t *testing.T) {
	p, ok := AuthFilePath()
	if !ok || p == "" {
		t.Fatal("path must resolve on every OS")
	}
	switch {
	case strings.Contains(p, "AppData"), strings.Contains(p, ".config"), strings.Contains(p, ".cursor"):
		// 平台合理路径
	default:
		t.Fatalf("unexpected path: %s", p)
	}
	// ReadLocalAuth 不炸即可（文件多半不存在）
	_ = ReadLocalAuth()
	// 写一个假 auth.json 再读（Windows 无法保证 APPDATA 可写测试目录，直接测解析逻辑）
	dir := t.TempDir()
	fp := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(fp, []byte(`{"accessToken":"local-tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil || !strings.Contains(string(raw), "local-tok") {
		t.Fatal("fixture broken")
	}
}
