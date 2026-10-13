// Package cursor — Cursor 平台客户端（magpie-community/plugins
// packages/cursor/index.mjs 二批移植，MIT，HEAD f573cfd）。
//
// 协议要点（对齐 fork 行号）：
//   - 登录三路径，优先级（Chief 口径 1）：
//     ① browserSignIn（主力，fork L270-323）：PKCE S256 verifier(32B) +
//     /loginDeepControl?challenge&uuid&mode=login&redirectTarget=cli，
//     轮询 /auth/poll?uuid&verifier——指数退避 min(1000×1.2^r, 10s)、
//     150 次上限、404=继续等、403=终局拒绝、连续 3 次网络错误失败、
//     成功要求 accessToken+refreshToken 字段同时存在；
//     ② API key 换 token（fork keyToken L238-255）：POST
//     /auth/exchange_user_api_key，按 sha256(key) 缓存（SOON=5min 内不重换）；
//     ③ CLI 模式（fork cliSignIn L327-342）：cursor-agent 现读 token、
//     永不落库——buddyhub 侧按 Chief 裁定入库硬拒，不做。
//   - 凭证形态（Chief 口径 2，fork L262-267）：Cursor 的 refresh token
//     **不能换新 access token**（refresh:"none"，cursor-agent 自己续）——
//     按"到期判定 + 标 lapsed"建模，不套 authcore 的 refresh-lead；
//     access token 约 2 个月有效、JWT 读不出 exp 时兜底 30 天（fork L315）。
//   - authFile 三平台路径（Chief 口径 3，fork L111-116）：Windows
//     %APPDATA%\Cursor\auth.json；macOS ~/.cursor/auth.json 兜底（钥匙串
//     security 命令依赖 shell，本包不做，面板可另配）；Linux
//     $XDG_CONFIG_HOME|~/.config/cursor/auth.json。
package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
)

// 端点与常量（fork L42-51）。
const (
	API       = "https://api2.cursor.sh"
	Website   = "https://cursor.com"
	APIClient = "cli"

	// pollMaxRounds 轮询上限（fork L288 for r < 150）。
	pollMaxRounds = 150
	// pollBaseMS / pollMaxMS 指数退避：min(1000×1.2^r, 10s)（fork L289）。
	pollBaseMS = 1000
	pollMaxMS  = 10_000
	// pollNetErrLimit 连续网络错误失败线（fork L294/L304：>=3 终局）。
	pollNetErrLimit = 3
	// soon token 剩余寿命低于此值即视为"快过期"（fork L144）。
	soon = 5 * time.Minute
	// defaultExpiry JWT 读不出 exp 时的兜底有效期（fork L315：30 天）。
	defaultExpiry = 30 * 24 * time.Hour
)

// HTTPDo 网络依赖（测试替身）。
type HTTPDo func(req *http.Request) (*http.Response, error)

// Client Cursor 客户端。
type Client struct {
	Do      HTTPDo
	APIBase string // 覆盖 API host（测试注入；空 = API）

	// keyCache API key → access token 兑换缓存（fork exchanged map，L234-255）。
	keyCache map[string]keyCacheItem

	// Sleep 轮询退避等待（测试注入零时跳过；nil = time.After）。
	// 150 轮 × 封顶 10s 的实测等待会让"超时用例"跑 25 分钟——注入后毫秒级。
	Sleep func(ctx context.Context, d time.Duration) bool
}

// New 建（默认 http.DefaultClient.Do）。
func New() *Client { return &Client{Do: http.DefaultClient.Do} }

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return c.APIBase
	}
	return API
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.Do != nil {
		return c.Do(req)
	}
	return http.DefaultClient.Do(req)
}

// ── PKCE（fork L273-275）──

// NewPKCE 生成 (verifier, challenge)：verifier = base64url(random 32B)。
func NewPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// ── 凭据（fork L262-267 的"耗尽即弃"形态）──

// Credential Cursor 凭据。RefreshMark 存 fork 的 refresh 字段值
// （"cursor-agent" = CLI 标记；browser 登录返回的是透传值）——
// buddyhub 不拿它换新 token（refresh:"none"），只做 lapsed 判定。
type Credential struct {
	AccessToken string `json:"access_token"`
	RefreshMark string `json:"refresh_mark,omitempty"`
	ExpireAtMS  int64  `json:"expire_at_ms,omitempty"` // 0 = 未知（按 lapsed=false 处理）
}

// Lapsed 到期判定（"耗尽即弃"：调用方标账号 lapsed，不刷新）。
// exp 未知（0）按未过期处理——不误判，失效靠上游 401 触发。
func (c *Credential) Lapsed(now time.Time) bool {
	if c.ExpireAtMS <= 0 {
		return false
	}
	return now.After(time.UnixMilli(c.ExpireAtMS))
}

// ExpiringSoon 是否快过期（fork SOON=5min；本包不自动刷新，仅供观测提示）。
func (c *Credential) ExpiringSoon(now time.Time) bool {
	if c.ExpireAtMS <= 0 {
		return false
	}
	remain := time.UnixMilli(c.ExpireAtMS).Sub(now)
	return remain > 0 && remain < soon
}

// jwtExpiry JWT payload exp → 毫秒；非三段/解析失败/无 exp → 0（fork L133-142）。
func jwtExpiry(token string) int64 {
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

// ── browserSignIn（主力路径，fork L270-323）──

// SignInSession 一次浏览器登录会话。
type SignInSession struct {
	Verifier string
	UUID     string
	URL      string
}

// StartSignIn 发起 browser 登录（fork L272-281）。
func (c *Client) StartSignIn() (*SignInSession, error) {
	verifier, challenge, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	id := newUUID()
	url := fmt.Sprintf("%s/loginDeepControl?challenge=%s&uuid=%s&mode=login&redirectTarget=cli",
		Website, challenge, id)
	return &SignInSession{Verifier: verifier, UUID: id, URL: url}, nil
}

// SignInOutcome 登录终态。
type SignInOutcome struct {
	Kind    string // "success" | "failed"
	Cred    *Credential
	ErrText string
}

// pollOnce 轮询一次（fork L285-321 循环体展开成单步）。
// 返回 (done, outcome, netErr)。netErr 非 nil = 传输层失败（调用方计数）。
func (c *Client) pollOnce(ctx context.Context, s *SignInSession, headers map[string]string) (bool, SignInOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/auth/poll?uuid=%s&verifier=%s", c.apiBase(), s.UUID, s.Verifier), nil)
	if err != nil {
		return true, SignInOutcome{Kind: "failed", ErrText: err.Error()}, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req)
	if err != nil {
		return false, SignInOutcome{}, err
	}
	if resp.Body == nil {
		// 防御：非 2xx 分支不读 body，某些 transport/测试替身给无 Body 的 Response。
		if resp.StatusCode == http.StatusNotFound {
			return false, SignInOutcome{}, nil
		}
		return true, SignInOutcome{Kind: "failed", ErrText: fmt.Sprintf("Cursor's sign-in answered HTTP %d", resp.StatusCode)}, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, SignInOutcome{}, nil // 404 = 继续等（fork L298-302）
	case resp.StatusCode == http.StatusForbidden:
		return true, SignInOutcome{Kind: "failed", ErrText: "Cursor refused the sign-in (403)"}, nil // fork L304
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return true, SignInOutcome{Kind: "failed", ErrText: fmt.Sprintf("Cursor's sign-in answered HTTP %d", resp.StatusCode)}, nil
	}
	// fork L309：accessToken 与 refreshToken 字段必须**同时存在**——
	// 先看原始 JSON 键（区分"缺字段"与"空值"），再解析值。
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		return true, SignInOutcome{Kind: "failed", ErrText: "poll 响应不是 JSON"}, nil
	}
	atRaw, hasAT := keys["accessToken"]
	rtRaw, hasRT := keys["refreshToken"]
	if !hasAT || !hasRT {
		return true, SignInOutcome{Kind: "failed", ErrText: "Cursor sent back no token"}, nil
	}
	var doc struct {
		AccessToken  string
		RefreshToken string
	}
	_ = json.Unmarshal(atRaw, &doc.AccessToken)
	_ = json.Unmarshal(rtRaw, &doc.RefreshToken)
	if strings.TrimSpace(doc.AccessToken) == "" {
		return true, SignInOutcome{Kind: "failed", ErrText: "Cursor sent back no token"}, nil
	}
	cred := &Credential{AccessToken: doc.AccessToken, RefreshMark: doc.RefreshToken}
	cred.ExpireAtMS = jwtExpiry(doc.AccessToken)
	if cred.ExpireAtMS == 0 {
		cred.ExpireAtMS = time.Now().Add(defaultExpiry).UnixMilli() // fork L315 兜底 30d
	}
	return true, SignInOutcome{Kind: "success", Cred: cred}, nil
}

// sleep 用 Client.Sleep（测试可注入零等待）；nil 回落 time.After。
func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// WaitSignIn 完整轮询循环（fork L288-321：150 次、指数退避、错误计数）。
func (c *Client) WaitSignIn(ctx context.Context, s *SignInSession) (*Credential, error) {
	headers := map[string]string{
		"Content-Type":            "application/json",
		"x-cursor-client-version": "cli",
		"x-cursor-client-type":    APIClient,
	}
	netErrs := 0
	for r := 0; r < pollMaxRounds; r++ {
		wait := pollWait(r)
		done, out, netErr := c.pollOnce(ctx, s, headers)
		if netErr != nil {
			netErrs++ // 连续 3 次网络错误终局（fork L293-296）
			if netErrs >= pollNetErrLimit {
				return nil, fmt.Errorf("couldn't reach Cursor: %v", netErr)
			}
			if !c.sleep(ctx, wait) {
				return nil, ctx.Err()
			}
			continue
		}
		netErrs = 0 // 正常应答（含 404 继续等）清零（fork L299）
		if done {
			if out.Kind == "success" {
				return out.Cred, nil
			}
			return nil, fmt.Errorf("%s", out.ErrText)
		}
		if !c.sleep(ctx, wait) {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("the sign-in timed out")
}

// pollWait 第 r 轮等待：min(1000×1.2^r, 10s) ms（fork L289）。
func pollWait(round int) time.Duration {
	ms := float64(pollBaseMS)
	for i := 0; i < round && ms < float64(pollMaxMS); i++ {
		ms *= 1.2
	}
	if ms > float64(pollMaxMS) {
		ms = pollMaxMS
	}
	return time.Duration(ms) * time.Millisecond
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strings.ReplaceAll(time.Now().Format("20060102150405.000000000"), ".", "")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ── API key 通道（fork keyToken L238-255）──

// keyCacheItem 按 sha256(key) 缓存兑换结果（SOON 内不重换）。
type keyCacheItem struct {
	token     string
	expiresAt time.Time
}

// ExchangeAPIKey 把 API key 换成 access token（结果进程内缓存）。
func (c *Client) ExchangeAPIKey(ctx context.Context, apiKey string) (*Credential, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("api key 为空")
	}
	sum := sha256.Sum256([]byte(apiKey))
	cacheKey := fmt.Sprintf("%x", sum)
	if e, ok := c.keyCache[cacheKey]; ok && time.Now().Before(e.expiresAt) {
		return &Credential{AccessToken: e.token}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase()+"/auth/exchange_user_api_key", strings.NewReader(`{"key":"`+apiKey+`"}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("exchange HTTP %d (no body)", resp.StatusCode)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("exchange HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var doc struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(raw, &doc) != nil || strings.TrimSpace(doc.AccessToken) == "" {
		return nil, fmt.Errorf("exchange 响应缺 accessToken")
	}
	if c.keyCache == nil {
		c.keyCache = map[string]keyCacheItem{}
	}
	c.keyCache[cacheKey] = keyCacheItem{token: doc.AccessToken, expiresAt: time.Now().Add(soon)}
	cred := &Credential{AccessToken: doc.AccessToken, RefreshMark: "api-key"}
	cred.ExpireAtMS = jwtExpiry(doc.AccessToken)
	if cred.ExpireAtMS == 0 {
		cred.ExpireAtMS = time.Now().Add(defaultExpiry).UnixMilli()
	}
	return cred, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ── 本机凭证读取（fork authFile L111-116 / readAuth L126）──

// AuthFilePath 本机 cursor auth.json 路径（三平台）。
// macOS 钥匙串形态（security find-generic-password）依赖 shell，不在此实现，
// 只读 auth.json 兜底路径。
func AuthFilePath() (string, bool) {
	switch runtime.GOOS {
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return "", false
		}
		return filepath.Join(appdata, "Cursor", "auth.json"), true
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		// fork：macOS 优先钥匙串；这里兜底 auth.json（同 Linux 形态）。
		return filepath.Join(home, ".cursor", "auth.json"), true
	default: // linux 等
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", false
			}
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "cursor", "auth.json"), true
	}
}

// ReadLocalAuth 读本机 auth.json 的 accessToken（读不到返回 ""）。
// 用途：已装 Cursor 客户端的机器免登录导入（fork signedIn 的本地凭证路径）。
func ReadLocalAuth() string {
	p, ok := AuthFilePath()
	if !ok {
		return ""
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var doc struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.AccessToken)
}
