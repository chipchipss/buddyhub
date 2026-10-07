// Package accio Accio（阿里）通道：本机回调授权 + ADK 信封对话。
//
// 协议事实来自公开实现（aimod-cc/agent2api 的 accio provider，MIT）。
//
// ── 登录：本机回调（与 Trae 同款）────────────────────────────
//
//	① PKCE：code_verifier 随机 → code_challenge = base64url(SHA-256(verifier))
//	② 浏览器打开 {region}/login?return_url={loopback}/auth/callback-accio
//	            &state=…&code_challenge=…&code_challenge_method=S256&client_id=accio-work
//	③ 回调带回 code → POST {gw}/api/oauth/token {code, codeVerifier, clientId, redirectUri}
//
// `redirectUri` 必须与授权时**逐字相同**（换码要原样回传）。
// 官方客户端优先用自定义协议 `accio://auth/callback`，退到自己的 local server；
// 本网关走后者同款形态——loopback 是浏览器导航、内嵌窗口与系统浏览器都走得通的那条。
//
// ── 两个地区 ─────────────────────────────────────────────────
//
//	国际版  https://www.accio.com      （域名 accio.com → GLOBAL）
//	国内版  https://www.accio-ai.com   （域名 accio-ai.com → CN）
//
// 业务网关两地同一个：`https://phoenix-gw.alibaba.com`。
//
// ── 对话：OpenAI ↔ ADK（Gemini 风格）信封 ────────────────────
//
// 上游是 protobuf-JSON 的 **snake_case** 形态，与 OpenAI 结构差异大，
// 两侧都要完整翻译（见 protocol.go）。
package accio

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Region 地区。
type Region string

const (
	RegionGlobal Region = "global"
	RegionCN     Region = "cn"
)

// ParseRegion 解析地区（空/未知回落国内版）。
func ParseRegion(s string) Region {
	if strings.EqualFold(strings.TrimSpace(s), string(RegionGlobal)) {
		return RegionGlobal
	}
	return RegionCN
}

// Label 展示名。
func (r Region) Label() string {
	if r == RegionGlobal {
		return "国际版"
	}
	return "国内版"
}

// Site 站点域名（登录页所在）。
func (r Region) Site() string {
	if r == RegionGlobal {
		return "https://www.accio.com"
	}
	return "https://www.accio-ai.com"
}

const (
	// GatewayBase 业务网关（两地同一个）。
	GatewayBase = "https://phoenix-gw.alibaba.com"

	ADKLLMPath       = "/api/adk/llm"
	OAuthTokenPath   = "/api/oauth/token"
	RefreshTokenPath = "/api/auth/refresh_token"
	UserInfoPath     = "/api/auth/userinfo"
	QuotaPath        = "/api/entitlement/quota"
	SubsPath         = "/api/entitlement/currentSubscription"
	ModelConfigPath  = "/api/llm/config/v2"

	// ClientID 桌面端常量（两地共用，公开值）。
	ClientID     = "accio-work"
	Tenant       = "accio-agent"
	IaiTag       = "phoenix-desktop"
	AppVersion   = "0.32.6"
	AppKey       = "35298846"
	AcceptLang   = "en"
	CallbackPath = "/auth/callback-accio"
)

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	Region       Region `json:"region,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒
	UserID       string `json:"user_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	// DeviceID 推理请求的设备指纹（网关按账号生成，参与上游风控）。
	DeviceID string `json:"device_id,omitempty"`
}

// NeedsRefresh 是否临近过期（提前 5 分钟）。无过期时间不刷新。
func (c *Credential) NeedsRefresh() bool {
	if c.AccessToken == "" || c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(5*time.Minute).UnixMilli() >= c.ExpiresAt
}

/* ── HTTP 客户端 ─────────────────────────────────────────────── */

var httpClient = &http.Client{Timeout: 0, Transport: newTransport()}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

// SetHTTPClient 允许宿主替换（测试注入 mock 上游）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		httpClient = c
	}
}

// SetProxy 给本通道单独设置代理；空串 = 跟随环境变量，再不行直连。
func SetProxy(rawURL string) error {
	tr := newTransport()
	if strings.TrimSpace(rawURL) != "" {
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("代理地址无效（%s）：%w", rawURL, err)
		}
		if u.Host == "" {
			return fmt.Errorf("代理地址缺少主机（%s）", rawURL)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	httpClient = &http.Client{Timeout: 0, Transport: tr}
	return nil
}

/* ── PKCE 与登录 ─────────────────────────────────────────────── */

const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

func pkcePair() (string, string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	vb := make([]byte, len(b))
	for i, x := range b {
		vb[i] = pkceAlphabet[int(x)%len(pkceAlphabet)]
	}
	v := string(vb)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// LoginContext 一次进行中的登录。
type LoginContext struct {
	Region       Region
	State        string
	CodeVerifier string
	CallbackURL  string
	DeviceID     string
	AuthURL      string
	Port         int
}

// NewLogin 绑定本机回环端口并组装授权链接。
func NewLogin(region Region) (*LoginContext, net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("无法绑定本机回调端口：%w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	verifier, challenge, err := pkcePair()
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	callback := fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)
	state := randomHex(16)
	c := &LoginContext{
		Region: region, State: state, CodeVerifier: verifier,
		CallbackURL: callback, DeviceID: randomHex(16), Port: port,
	}
	c.AuthURL = fmt.Sprintf(
		"%s/login?return_url=%s&state=%s&code_challenge=%s&code_challenge_method=S256&client_id=%s",
		region.Site(), url.QueryEscape(callback), url.QueryEscape(state),
		url.QueryEscape(challenge), ClientID)
	return c, ln, nil
}

// Callback 回调带回的参数。
type Callback struct {
	Code  string
	State string
	Error string
	// RawQuery 原始 query（未解码）。排障用：上游改回调形状时，
	// 解析器认不出的键全在这里。
	RawQuery string
}

// resolvesLogin 这次回调是否把登录推进到了可判定的状态。
//
// false 时监听器要**继续等下一个请求**：浏览器会先发 preconnect、favicon 探测
// 之类的杂音（它们也打到回调路径，但 query 里什么都没有）。一次就收尾等于把
// 真回调挡在门外——Trae 通道实测踩过：授权页刚打开 2 秒就报「回调里没有授权码」。
func (c *Callback) resolvesLogin() bool {
	return c.Error != "" || c.Code != ""
}

// ParseCallback 解析回调 query（字段名有多种写法）。
func ParseCallback(q url.Values) *Callback {
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				return v
			}
		}
		return ""
	}
	return &Callback{
		Code:  pick("code", "auth_code", "authCode"),
		State: pick("state"),
		Error: pick("error", "error_description"),
	}
}

// Complete 用回调内容换凭据。
func Complete(ctx context.Context, c *LoginContext, cb *Callback) (*Credential, error) {
	if cb.Error != "" {
		return nil, fmt.Errorf("授权被拒：%s", cb.Error)
	}
	if cb.Code == "" {
		return nil, fmt.Errorf("回调里没有授权码")
	}
	if cb.State != "" && c.State != "" && cb.State != c.State {
		return nil, fmt.Errorf("回调 state 与本次登录不匹配（可能是上一次的）")
	}
	// redirect_uri 必须与授权时逐字相同
	body, _ := json.Marshal(map[string]string{
		"code": cb.Code, "codeVerifier": c.CodeVerifier,
		"clientId": ClientID, "redirectUri": c.CallbackURL,
	})
	raw, err := postJSON(ctx, GatewayBase+OAuthTokenPath, body, "")
	if err != nil {
		return nil, fmt.Errorf("换证失败：%w", err)
	}
	cred, err := parseCredential(raw)
	if err != nil {
		return nil, err
	}
	cred.Region = c.Region
	cred.DeviceID = c.DeviceID
	// 补一次用户信息（失败不影响可用性）
	if uid, email, name := fetchUserInfo(ctx, cred); uid != "" || email != "" {
		cred.UserID, cred.Email, cred.Name = uid, email, name
	}
	return cred, nil
}

// parseCredential 解析换证/续期回执（键名宽口径）。
func parseCredential(raw []byte) (*Credential, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("回执解析失败：%w", err)
	}
	if d, ok := doc["data"].(map[string]any); ok {
		for k, v := range d {
			if _, exists := doc[k]; !exists {
				doc[k] = v
			}
		}
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := doc[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	access := pick("accessToken", "access_token", "token")
	if access == "" {
		return nil, fmt.Errorf("回执里没有 accessToken")
	}
	cred := &Credential{
		AccessToken:  access,
		RefreshToken: pick("refreshToken", "refresh_token"),
		UserID:       pick("userId", "user_id", "id"),
		Email:        pick("email"),
		Name:         pick("name"),
	}
	if exp := firstNum(doc, "expiresAt", "expires_at"); exp != 0 {
		if exp < 1e12 {
			exp *= 1000
		}
		cred.ExpiresAt = exp
	} else {
		cred.ExpiresAt = jwtExpMs(access)
	}
	return cred, nil
}

func fetchUserInfo(ctx context.Context, cred *Credential) (uid, email, name string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GatewayBase+UserInfoPath, nil)
	if err != nil {
		return
	}
	applyAuthHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	if d, ok := doc["data"].(map[string]any); ok {
		for k, v := range d {
			if _, exists := doc[k]; !exists {
				doc[k] = v
			}
		}
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := doc[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	return get("id", "userId", "user_id"), get("email"), get("name", "nickname")
}

/* ── 续期 ────────────────────────────────────────────────────── */

// Refresh 续期。
func Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if cred.RefreshToken == "" {
		return nil, fmt.Errorf("缺少 refresh_token，需重新登录")
	}
	body, _ := json.Marshal(map[string]string{
		"refreshToken": cred.RefreshToken, "clientId": ClientID,
	})
	raw, err := postJSON(ctx, GatewayBase+RefreshTokenPath, body, cred.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("续期失败：%w", err)
	}
	fresh, err := parseCredential(raw)
	if err != nil {
		return nil, err
	}
	out := *cred
	if fresh.AccessToken != "" {
		out.AccessToken = fresh.AccessToken
	}
	if fresh.RefreshToken != "" {
		out.RefreshToken = fresh.RefreshToken
	}
	if fresh.ExpiresAt != 0 {
		out.ExpiresAt = fresh.ExpiresAt
	}
	return &out, nil
}

/* ── 请求头 ──────────────────────────────────────────────────── */

func applyAuthHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", AcceptLang)
	req.Header.Set("x-app-key", AppKey)
	req.Header.Set("x-app-version", AppVersion)
	req.Header.Set("x-client-id", ClientID)
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	// 网关按地区门控下发模型/能力；不带 x-package-region，CN 号会拿到空目录
	// （HTTP 200 但 data 无模型条目），表现为静默 0 模型。
	if h := cred.Region.gatewayHeader(); h != "" {
		req.Header.Set("x-package-region", h)
	}
}

// gatewayHeader 把内部 Region 映射成上游 x-package-region 头值（两地共用一个网关）。
func (r Region) gatewayHeader() string {
	if r == RegionGlobal {
		return "GLOBAL"
	}
	return "CN"
}

/* ── 模型目录 ────────────────────────────────────────────────── */

// Model 模型条目。
type Model struct {
	ID   string
	Name string
}

// ListModels 拉模型目录。
//
// ⚠️ 是 **POST** 不是 GET（实测 GET 返回 405 Method Not Allowed）。
//
// 依次探 v2 与 v1 两个 config 端点，取**第一个解析出非空模型**的结果——
// 某些账号/地区其中一个会回 200 但载荷形状对另一档不适用，单探一路就静默 0 模型。
func ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	var lastErr error
	for _, path := range []string{ModelConfigPath, "/api/llm/config"} {
		mods, err := listModelsAt(ctx, cred, path)
		if err == nil && len(mods) > 0 {
			return mods, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func listModelsAt(ctx context.Context, cred *Credential, path string) ([]Model, error) {
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GatewayBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型目录失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	// 网关把鉴权/业务失败塞在 **HTTP 200 的响应体**里（{"success":false,"code":"403",
	// "message":"auth failed"}）。若直接丢给 parseCatalog，会得到「静默 0 模型」，
	// 把「凭证失效需重登」误报成「账号没有模型」——排查时极易误判。先分类掉。
	if reason, bad := inBodyError(raw); bad {
		return nil, fmt.Errorf("模型目录业务失败（HTTP 200 载荷）：%s", reason)
	}
	return parseCatalog(raw), nil
}

// inBodyError 识别网关「HTTP 200 但体内是错误信封」的响应。
// 仅在 success 显式为 false 时判失败（避免误伤把 code 当业务码的成功载荷）。
func inBodyError(raw []byte) (string, bool) {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return "", false
	}
	if ok, isBool := doc["success"].(bool); isBool && !ok {
		msg, _ := doc["message"].(string)
		if msg == "" {
			msg = "上游未给出原因"
		}
		code, _ := doc["code"].(string)
		if code == "" {
			if cf, isNum := doc["code"].(float64); isNum {
				code = fmt.Sprintf("%.0f", cf)
			}
		}
		if code != "" {
			return fmt.Sprintf("code=%s message=%s", code, msg), true
		}
		return "message=" + msg, true
	}
	return "", false
}

// parseCatalog 递归找模型 id（形状不固定）。
func parseCatalog(raw []byte) []Model {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Model
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			id, _ := t["id"].(string)
			if id == "" {
				id, _ = t["model"].(string)
			}
			if id == "" {
				id, _ = t["name"].(string)
			}
			if id != "" && !seen[id] {
				seen[id] = true
				name, _ := t["displayName"].(string)
				if name == "" {
					name, _ = t["display_name"].(string)
				}
				if name == "" {
					name = id
				}
				out = append(out, Model{ID: id, Name: name})
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

/* ── 余额 ────────────────────────────────────────────────────── */

// Quota 额度快照。
type Quota struct {
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	PlanName  string  `json:"plan_name,omitempty"`
}

// FetchQuota 查额度（失败不致命，仅展示用）。
func FetchQuota(ctx context.Context, cred *Credential) (*Quota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GatewayBase+QuotaPath, nil)
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("额度回执解析失败")
	}
	if d, ok := doc["data"].(map[string]any); ok {
		doc = d
	}
	q := &Quota{}
	q.Remaining = floatOf(doc, "remaining", "balance", "available")
	q.Total = floatOf(doc, "total", "quota", "limit")
	q.PlanName, _ = doc["planName"].(string)
	if q.PlanName == "" {
		q.PlanName, _ = doc["plan_name"].(string)
	}
	return q, nil
}

/* ── 小工具 ──────────────────────────────────────────────────── */

func postJSON(ctx context.Context, u string, body []byte, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", AcceptLang)
	req.Header.Set("x-app-key", AppKey)
	req.Header.Set("x-app-version", AppVersion)
	req.Header.Set("x-client-id", ClientID)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("凭据无效或已过期（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

func jwtExpMs(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp * 1000
}

func firstNum(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func floatOf(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

/* ── 回调监听 ────────────────────────────────────────────────── */

// AwaitCallback 在给定监听器上等一次回调（带超时）。
func AwaitCallback(ctx context.Context, ln net.Listener, ttl time.Duration) (*Callback, error) {
	type result struct {
		cb  *Callback
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				ch <- result{nil, err}
				return
			}
			cb, ok := readCallbackConn(conn)
			if !ok {
				continue // 杂音请求（preconnect / favicon 探测）：继续等真回调
			}
			ch <- result{cb, nil}
			return
		}
	}()

	select {
	case r := <-ch:
		return r.cb, r.err
	case <-time.After(ttl):
		return nil, fmt.Errorf("等待授权回调超时")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readCallbackConn 读一个回调连接：回一页提示，并解析出回调内容。
// ok=false 表示只是杂音请求，调用方应继续等。
func readCallbackConn(conn net.Conn) (*Callback, bool) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 8192)
	n, _ := conn.Read(buf)
	line := string(buf[:n])
	path := "/"
	if i := strings.Index(line, " "); i > 0 {
		rest := line[i+1:]
		if j := strings.Index(rest, " "); j > 0 {
			path = rest[:j]
		}
	}
	u, perr := url.Parse(path)
	if perr != nil {
		return nil, false
	}
	cb := ParseCallback(u.Query())
	cb.RawQuery = u.RawQuery

	// 无论是不是杂音都先回一页：浏览器在等响应，不回它会一直转圈。
	body := "<html><meta charset=\"utf-8\"><body style=\"font-family:system-ui;padding:40px;text-align:center\">" +
		"<h2>授权完成</h2><p>可以关闭本页，回到管理面板查看结果。</p></body></html>"
	resp := "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
	_, _ = conn.Write([]byte(resp))

	return cb, cb.resolvesLogin()
}
