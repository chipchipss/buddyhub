// Package zcode — ZCode / z.ai 平台客户端（magpie-community/plugins
// packages/zcode/index.mjs 二批移植，MIT）。
//
// 协议要点（对齐 fork 行号）：
//   - 非标准 device flow：**poll token + flow_id 双段式**（authorize L870-927）——
//     先造 poll token = "Bearer "+hex(randomBytes(32))（L871），
//     POST /api/v1/oauth/cli/init body {provider, device} 拿 flow_id + authorize_url；
//   - 回调 zcode://oauth/callback；**站点参数名不同**：bigmodel 用 redirect、
//     zai 用 redirect_uri（L886）；
//   - 轮询 GET /api/v1/oauth/cli/poll/{flow_id}（L900）：interval 取
//     poll_interval_sec（L887），deadline 取 expires_at×1000（L888）；
//     status: pending/failed/ready（L906-908）；4xx（除 408/429）终局失败，
//     其余重试（L902-903）；
//   - token 字段双站点不同：bigmodel 取 got.bigmodel.access_token、
//     zai 取 got.zai.access_token（L905）；
//   - **两路凭证过期语义不同**（L919-921）：API key → 永不失效（expires=0）；
//     Start Plan JWT → expires=jwtExpiry(jwt)。JWT 不可刷新（refresh:"none"），
//     账号池按"耗尽即弃"建模——**不用 authcore 的 refresh-lead**，只做到期判定。
package zcode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
)

// Site 站点（bigmodel / zai，fork 双站语义）。
type Site int

const (
	SiteBigModel Site = iota
	SiteZai
)

// siteParams 双站点端点与参数名差异（fork L886 redirect vs redirect_uri）。
type siteParams struct {
	OAuthBase    string // /api/v1/oauth 所在 host
	RedirectKey  string // "redirect"（bigmodel）或 "redirect_uri"（zai）
	TokenKeyPath string // token 响应里 access_token 的路径段名
}

var sites = map[Site]siteParams{
	SiteBigModel: {OAuthBase: "https://bigmodel.cn/api/v1/oauth", RedirectKey: "redirect", TokenKeyPath: "bigmodel"},
	SiteZai:      {OAuthBase: "https://z.ai/api/v1/oauth", RedirectKey: "redirect_uri", TokenKeyPath: "zai"},
}

// HTTPDo 网络依赖（测试替身）。
type HTTPDo func(req *http.Request) (*http.Response, error)

// Client ZCode 客户端。
type Client struct {
	Do      HTTPDo
	BaseURL string // 覆盖站点 OAuthBase（测试注入；空 = 按站点默认）
}

// New 建客户端。
func New() *Client { return &Client{Do: http.DefaultClient.Do} }

// DeviceSession 一次登录会话（poll token + flow_id 双段式）。
type DeviceSession struct {
	Site         Site
	PollToken    string // "Bearer "+hex（fork L871）——发起与轮询都靠它
	FlowID       string
	AuthorizeURL string
	Interval     time.Duration
	Deadline     time.Time
}

// Start 发起登录（fork authorize L870-893）。
func (c *Client) Start(ctx context.Context, site Site, device string) (*DeviceSession, error) {
	p := c.params(site)
	pollSecret := make([]byte, 32)
	if _, err := rand.Read(pollSecret); err != nil {
		return nil, err
	}
	pollToken := "Bearer " + hex.EncodeToString(pollSecret)
	form := map[string]string{
		"provider": siteName(site),
		"device":   device,
	}
	form[p.RedirectKey] = "zcode://oauth/callback"
	raw, status, err := c.post(ctx, p.OAuthBase+"/cli/init", pollToken, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("cli/init HTTP %d: %s", status, truncate(raw, 200))
	}
	var doc struct {
		FlowID          string `json:"flow_id"`
		AuthorizeURL    string `json:"authorize_url"`
		PollIntervalSec any    `json:"poll_interval_sec"`
		ExpiresAt       any    `json:"expires_at"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("cli/init 响应解析失败: %s", truncate(raw, 200))
	}
	if doc.FlowID == "" || doc.AuthorizeURL == "" {
		return nil, fmt.Errorf("cli/init 缺 flow_id/authorize_url: %s", truncate(raw, 160))
	}
	s := &DeviceSession{
		Site: site, PollToken: pollToken, FlowID: doc.FlowID, AuthorizeURL: doc.AuthorizeURL,
		Interval: 3 * time.Second,
	}
	if iv, ok := authcore.ExpiresAtMS(doc.PollIntervalSec), false; ok {
		_ = iv
	}
	if sec, ok := toInt64(doc.PollIntervalSec); ok && sec > 0 {
		s.Interval = time.Duration(sec) * time.Second
	}
	// deadline = expires_at（fork L888 ×1000；authcore.ExpiresAtMS 兼容秒/毫秒）。
	if ms := authcore.ExpiresAtMS(doc.ExpiresAt); ms > 0 {
		s.Deadline = time.UnixMilli(ms)
	} else {
		s.Deadline = time.Now().Add(10 * time.Minute)
	}
	return s, nil
}

func (c *Client) params(site Site) siteParams {
	p := sites[site]
	if c.BaseURL != "" {
		p.OAuthBase = c.BaseURL
	}
	return p
}

func siteName(s Site) string {
	if s == SiteZai {
		return "zai"
	}
	return "bigmodel"
}

func (c *Client) post(ctx context.Context, url, bearer string, form map[string]string) ([]byte, int, error) {
	parts := make([]string, 0, len(form))
	for k, v := range form {
		parts = append(parts, k+"="+v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(strings.Join(parts, "&")))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	return raw, resp.StatusCode, nil
}

// PollVerdict 一次轮询判定（fork L900-908 语义）。
type PollVerdict int

const (
	PollPending PollVerdict = iota
	PollFailed              // status=failed（上游明确失败）
	PollReady               // status=ready，可收敛 token
	PollGone                // 4xx（除 408/429）：flow 终局，重开一次
	PollRetry               // 408/429/5xx/传输错误：可重试
)

// PollResult 轮询结果。
type PollResult struct {
	Verdict PollVerdict
	Cred    *Credential
}

// Poll 单次轮询（GET /cli/poll/{flow_id}，fork L900-908）。
func (c *Client) Poll(ctx context.Context, s *DeviceSession) PollResult {
	p := c.params(s.Site)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.OAuthBase+"/cli/poll/"+s.FlowID, nil)
	if err != nil {
		return PollResult{Verdict: PollRetry}
	}
	req.Header.Set("Authorization", s.PollToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return PollResult{Verdict: PollRetry} // 传输错误重试（fork L902-903）
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	// 4xx（除 408/429）= 终局（fork L902-903）。
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
		return PollResult{Verdict: PollGone}
	}
	if resp.StatusCode >= 400 || resp.StatusCode >= 500 {
		return PollResult{Verdict: PollRetry}
	}
	var doc struct {
		Status   string `json:"status"`
		BigModel *struct {
			AccessToken string `json:"access_token"`
		} `json:"bigmodel"`
		Zai *struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return PollResult{Verdict: PollRetry}
	}
	switch doc.Status {
	case "pending":
		return PollResult{Verdict: PollPending}
	case "failed":
		return PollResult{Verdict: PollFailed}
	case "ready":
		// 双站点 token 字段不同（fork L905）。
		tok := ""
		switch s.Site {
		case SiteBigModel:
			if doc.BigModel != nil {
				tok = doc.BigModel.AccessToken
			}
		case SiteZai:
			if doc.Zai != nil {
				tok = doc.Zai.AccessToken
			}
		}
		if strings.TrimSpace(tok) == "" {
			return PollResult{Verdict: PollFailed}
		}
		return PollResult{Verdict: PollReady, Cred: &Credential{AccessToken: tok}}
	}
	return PollResult{Verdict: PollRetry}
}

// Wait 轮询直到 ready / failed / gone / 超时。
func (c *Client) Wait(ctx context.Context, s *DeviceSession) (*Credential, error) {
	deadline := s.Deadline
	if deadline.IsZero() || deadline.Before(time.Now()) {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for {
		r := c.Poll(ctx, s)
		switch r.Verdict {
		case PollReady:
			return r.Cred, nil
		case PollFailed:
			return nil, fmt.Errorf("上游登录流程失败（status=failed）")
		case PollGone:
			return nil, fmt.Errorf("登录流程已失效（4xx），请重新发起")
		}
		if time.Now().Add(s.Interval).After(deadline) {
			return nil, fmt.Errorf("登录等待超时")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.Interval):
		}
	}
}

// ── 凭据与过期（"耗尽即弃"——无 refresh）──

// Credential ZCode 凭据。两种形态（fork L919-921）：
//   - API key 模式：APIKey 非空 → 永不失效（ExpireAtMS=0）；
//   - Start Plan 模式：JWT → ExpireAtMS=jwtExpiry(jwt)。
type Credential struct {
	AccessToken string `json:"access_token,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	ExpireAtMS  int64  `json:"expire_at_ms,omitempty"`
}

// FromAPIKey API key 凭据：永不失效。
func FromAPIKey(key string) *Credential {
	return &Credential{APIKey: key}
}

// FromJWT JWT 凭据：exp 过期判定（fork jwtExpiry L110-119：非三段/解析失败
// → 0 = 永不过期，别误判）。
func FromJWT(jwt string) *Credential {
	return &Credential{AccessToken: jwt, ExpireAtMS: JWTExpiry(jwt)}
}

// JWTExpiry 解 JWT payload 的 exp（×1000）；非三段/解析失败/无 exp → 0。
func JWTExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp any `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	return authcore.ExpiresAtMS(claims.Exp)
}

// Lapsed 判定凭据是否已耗尽（fork jwtExpired L120-123：t>0 且 now>t；
// exp=0 永不过期）。耗尽即弃：调用方直接标账号 lapsed，不做刷新。
func (c *Credential) Lapsed(now time.Time) bool {
	if c.ExpireAtMS <= 0 {
		return false
	}
	return now.After(time.UnixMilli(c.ExpireAtMS))
}

// BearerToken 取请求用 token（API key 优先——fork signedIn 三级探测里
// 自有 plan mintKey 最高优先）。
func (c *Credential) BearerToken() string {
	if c.APIKey != "" {
		return c.APIKey
	}
	return c.AccessToken
}

func toInt64(v any) (int64, bool) {
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

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
