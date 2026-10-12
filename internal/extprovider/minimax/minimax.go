// Package minimax — MiniMax Code 平台客户端（magpie-community/plugins
// packages/minimax/index.mjs 首批移植，MIT）。
//
// 协议要点（对齐 fork）：
//   - 双站点：CN（account.minimax.cn / agent.minimax.cn）+ Global（account.minimax.io）；
//   - 标准 OAuth device flow + PKCE S256，client `mcode-public`，
//     scope `agent.default`，audience `agent-backend`；
//   - 变体①：startDevice 可能无 device_code，用 user_code 轮询；
//   - 变体②：expires_in 可能是绝对毫秒（≥1e12）而非秒；
//     变体③：interval 单位毫秒；
//   - 轮询：POST /oauth2/token grant=device_code，五态
//     pending/slow_down(+5s)/denied/expired/其他（authcore 状态机）；
//   - 刷新：grant=refresh_token 同端点；登出只认 invalid_grant+400；
//   - 请求签名：x-mini-max-* 头（md5 口径，fork signed()/SIGN_SECRET）。
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
)

// Site 站点（CN / Global）。
type Site int

const (
	SiteCN Site = iota
	SiteGlobal
)

// siteParams 双站点端点（fork SITES L31-51）。
type siteParams struct {
	Account string // OAuth 端点 host
	Agent   string // 推理端点 host
}

var sites = map[Site]siteParams{
	SiteCN:     {Account: "https://account.minimax.cn", Agent: "https://agent.minimax.cn"},
	SiteGlobal: {Account: "https://account.minimax.io", Agent: "https://agent.minimax.io"},
}

// oauth 常量（fork startDevice L226-257）。
const (
	oauthClient   = "mcode-public"
	oauthScope    = "agent.default"
	oauthAudience = "agent-backend"
	signInTimeout = 10 * time.Minute // fork SIGN_IN_MS
	pollBase      = 5 * time.Second
)

// DeviceSession 一次设备登录会话。
type DeviceSession struct {
	Site            Site
	DeviceCode      string // 可能空（变体①：无 device_code 用 user_code 轮询）
	UserCode        string // 用户在浏览器输入的码
	ByUserCode      bool   // fork：仅无 device_code + 有 user_code + 有 expired_in 时启用
	Verifier        string
	Interval        time.Duration
	VerificationURL string
	ExpiresAt       time.Time
}

// HTTPDo 网络依赖（测试替身）。
type HTTPDo func(req *http.Request) (*http.Response, error)

// Client MiniMax 客户端。
type Client struct {
	Do HTTPDo
}

// New 建（默认 http.DefaultClient.Do）。
func New() *Client {
	return &Client{Do: http.DefaultClient.Do}
}

func (c *Client) post(ctx context.Context, url string, form map[string]string) ([]byte, int, error) {
	q := make([]string, 0, len(form))
	for k, v := range form {
		q = append(q, k+"="+v)
	}
	body := strings.Join(q, "&")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.doWithRetry(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	return raw, resp.StatusCode, nil
}

func (c *Client) doWithRetry(req *http.Request) (*http.Response, error) {
	return c.Do(req)
}

// StartDevice 发起设备流（fork startDevice）。
func (c *Client) StartDevice(ctx context.Context, site Site) (*DeviceSession, error) {
	p := sites[site]
	verifier, err := authcore.NewVerifier(32)
	if err != nil {
		return nil, err
	}
	raw, status, err := c.post(ctx, p.Account+"/oauth2/device/code", map[string]string{
		"client_id":             oauthClient,
		"scope":                 oauthScope,
		"audience":              oauthAudience,
		"code_challenge":        authcore.ChallengeS256(verifier),
		"code_challenge_method": "S256",
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("device/code HTTP %d: %s", status, truncate(raw, 200))
	}
	var doc struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURL         string `json:"verification_uri"`
		VerificationURLComplete string `json:"verification_uri_complete"`
		Interval                any    `json:"interval"`
		ExpiresIn               any    `json:"expires_in"`
		ExpiredIn               any    `json:"expired_in"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("device/code 响应解析失败: %s", truncate(raw, 200))
	}
	now := time.Now()
	s := &DeviceSession{
		Site:            site,
		Verifier:        verifier,
		DeviceCode:      strings.TrimSpace(doc.DeviceCode),
		UserCode:        strings.TrimSpace(doc.UserCode),
		Interval:        pollBase,
		VerificationURL: doc.VerificationURLComplete,
	}
	if s.VerificationURL == "" {
		s.VerificationURL = doc.VerificationURL
	}
	// MiniMax fork variant: expired_in is either seconds or an absolute ms timestamp.
	// The no-device-code polling path is valid only when user_code+expired_in are present.
	if expired, ok := positiveInt64(doc.ExpiredIn); ok {
		s.ExpiresAt = authcore.ExpiryDeadline(expired, now)
		s.ByUserCode = s.DeviceCode == "" && s.UserCode != ""
	}
	if s.ExpiresAt.IsZero() {
		if expires, ok := positiveInt64(doc.ExpiresIn); ok {
			s.ExpiresAt = authcore.ExpiryDeadline(expires, now)
		}
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = now.Add(signInTimeout)
	}
	// Without device_code, never invent a polling grant unless the documented variant is complete.
	if s.DeviceCode == "" && !s.ByUserCode {
		return nil, fmt.Errorf("device/code 响应缺 device_code（user_code fallback 需要 expired_in）")
	}
	// fork interval is milliseconds; NormalizeIntervalMS handles small values in seconds.
	if iv, ok := positiveInt64(doc.Interval); ok {
		s.Interval = authcore.NormalizeIntervalMS(iv)
	}
	return s, nil
}

func positiveInt64(v any) (int64, bool) {
	n, ok := toInt(v)
	return n, ok && n > 0
}

func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	}
	return 0, false
}

// PollOutcome 轮询结果（透传 authcore 判定 + 收敛 token）。
type PollOutcome struct {
	Status authcore.PollStatus
	Cred   *Credential
}

// Poll 单次轮询（fork pollDevice：grant=device_code，无 device_code 用 user_code）。
func (c *Client) Poll(ctx context.Context, s *DeviceSession) PollOutcome {
	p := sites[s.Site]
	grant := map[string]string{
		"client_id":  oauthClient,
		"grant_type": "urn:ietf:params:oauth:grant-type:device_code",
	}
	if s.DeviceCode != "" {
		grant["device_code"] = s.DeviceCode
	} else if s.ByUserCode && strings.TrimSpace(s.UserCode) != "" {
		// Fork variant: no device_code; send user_code only.
		grant["user_code"] = s.UserCode
	} else {
		return PollOutcome{Status: authcore.PollError}
	}
	raw, status, err := c.post(ctx, p.Account+"/oauth2/token", grant)
	out := authcore.ClassifyPollResponse(status, raw, err)
	if out.Status != authcore.PollDone {
		return PollOutcome{Status: out.Status}
	}
	cred, err := credentialFromToken(raw)
	if err != nil {
		return PollOutcome{Status: authcore.PollError}
	}
	return PollOutcome{Status: authcore.PollDone, Cred: cred}
}

// WaitDevice 跑完整轮询循环直到授权/拒绝/超时（fork 2s~5s 节奏由 authcore 管）。
func (c *Client) WaitDevice(ctx context.Context, s *DeviceSession) (*Credential, error) {
	deadline := s.ExpiresAt
	if deadline.IsZero() || deadline.Before(time.Now()) {
		deadline = time.Now().Add(signInTimeout)
	}
	interval := s.Interval
	for {
		out := c.Poll(ctx, s)
		switch out.Status {
		case authcore.PollDone:
			return out.Cred, nil
		case authcore.PollDenied:
			return nil, fmt.Errorf("用户拒绝授权")
		case authcore.PollExpiredToken:
			return nil, fmt.Errorf("设备码已过期，请重新发起登录")
		case authcore.PollSlowDown:
			interval = authcore.NextInterval(interval, true)
		case authcore.PollError:
			interval = authcore.NextInterval(s.Interval, false)
		case authcore.PollPending:
			interval = authcore.NextInterval(s.Interval, false)
		}
		if time.Now().Add(interval).After(deadline) {
			return nil, fmt.Errorf("登录等待超时（%s）", deadline.Format("15:04:05"))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// credentialFromToken 从 token 响应收敛凭据（宽容解析：access_token 必有，
// expires_in 走毫秒歧义收敛，refresh_token 可选）。
func credentialFromToken(raw []byte) (*Credential, error) {
	var doc struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    any    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if json.Unmarshal(raw, &doc) != nil || strings.TrimSpace(doc.AccessToken) == "" {
		return nil, fmt.Errorf("token 响应缺 access_token")
	}
	c := &Credential{
		AccessToken:  doc.AccessToken,
		RefreshToken: doc.RefreshToken,
	}
	if ms := authcore.ExpiresAtMS(doc.ExpiresIn); ms > 0 {
		now := time.Now().UnixMilli()
		if ms > now+1_000_000 {
			c.ExpireAtMS = ms
		} else {
			c.ExpireAtMS = now + ms*1 // 相对秒 → 毫秒
			if ms < 1e12 {
				c.ExpireAtMS = now + ms*1000
			}
		}
	}
	return c, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ── 凭据与刷新 ──

// Credential MiniMax 凭据。
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpireAtMS   int64  `json:"expire_at_ms,omitempty"`
}

// Refresh 刷新（grant=refresh_token 同端点）。
// 返回 (新凭据, 登出判定)。登出判定照抄 fork L304：仅 invalid_grant+400。
func (c *Client) Refresh(ctx context.Context, site Site, cred *Credential) (*Credential, bool, error) {
	if cred == nil || strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, false, fmt.Errorf("无 refresh_token，需重新设备登录")
	}
	p := sites[site]
	raw, status, err := c.post(ctx, p.Account+"/oauth2/token", map[string]string{
		"client_id":     oauthClient,
		"grant_type":    "refresh_token",
		"refresh_token": cred.RefreshToken,
	})
	if err != nil {
		return nil, false, err // 传输层错误：保留登录态
	}
	if authcore.LogoutPredicate(status, raw) {
		return nil, true, fmt.Errorf("refresh_token 已失效（invalid_grant），账号需重新登录")
	}
	if status < 200 || status >= 300 {
		// 5xx/其他：短暂故障，保留登录态（fork 语义：绝不固化禁用）。
		return nil, false, fmt.Errorf("refresh HTTP %d: %s", status, truncate(raw, 200))
	}
	fresh, err := credentialFromToken(raw)
	if err != nil {
		return nil, false, err
	}
	return fresh, false, nil
}
