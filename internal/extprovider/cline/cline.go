// Package cline Cline（cline.bot）通道：WorkOS 设备授权 + OpenAI 兼容直连。
//
// 协议事实来自公开实现（aimod-cc/agent2api 的 cline provider，MIT）与**本机实测核对**：
//
//  1. 设备授权  POST https://api.workos.com/user_management/authorize/device
//     → device_code / user_code / verification_uri（实测 expires_in=300, interval=5）
//  2. 轮询      POST https://api.workos.com/user_management/authenticate
//     grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=…&client_id=…
//  3. **登记**  POST {apiBase}/auth/register  {"accessToken":…,"refreshToken":…}
//     ← WorkOS 令牌必须再换成 Cline 会话令牌，**这一步不能省**
//  4. 对话      POST {apiBase}/chat/completions（OpenAI 原生；stream 时是裸 chunk 帧）
//  5. 模型      GET  {apiBase}/ai/cline/recommended-models（**免鉴权**，实测 200）
//  6. 续期      POST {apiBase}/auth/refresh  {"refreshToken":…,"grantType":"refresh_token"}
//
// 三个容易踩空、且踩空后表现很像「账号没权限」的口径：
//
//   - **token 必须带 `workos:` 前缀**（`Authorization: Bearer workos:eyJ…`）。
//     而 /auth/refresh 返回的 accessToken **不带**前缀——写入前统一补齐。
//   - **必须带 `X-CLIENT-TYPE: cline-sdk`**：不带时免费池模型一律 403
//     （`only available via Cline product surfaces`），看起来像没订阅。
//   - **`grantType` 是 camelCase**（不是 OAuth 标准的 grant_type）。
//
// 计费池由模型名前缀选择，**前缀是网关侧的计费通道选择器**，出站前要剥掉：
//
//	cline-free/…   免费池（也有少数无前缀条目同属免费池）
//	cline-pass/…   ClinePass 订阅池
//	cline-cloud/…  云端池
package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// APIBase Cline 网关基址。注意路径里**只有一个 v1**：上游把 baseURL 当根拼，
	// 拼成 /api/v1/v1/... 会 404。
	APIBase = "https://api.cline.bot/api/v1"

	// WorkOSBase / WorkOSClientID 设备授权（WorkOS AuthKit）。client_id 是
	// Cline 官方客户端的**公开**标识，非秘密。
	WorkOSBase     = "https://api.workos.com"
	WorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

	// DeviceVerifyFallback 上游未回 verification_uri 时的兜底验证页。
	DeviceVerifyFallback = "https://authkit.cline.bot/device"

	// TokenPrefix 访问令牌前缀。发请求时必须带，续期接口返回时不带。
	TokenPrefix = "workos:"

	// ClientType 产品面标识。**缺了它免费池一律 403**。
	ClientType = "cline-sdk"

	// 计费池前缀（网关侧选择器，出站前剥掉）。
	FreePrefix  = "cline-free/"
	PassPrefix  = "cline-pass/"
	CloudPrefix = "cline-cloud/"

	deviceAuthorizePath = "/user_management/authorize/device"
	deviceAuthPath      = "/user_management/authenticate"
	registerPath        = "/auth/register"
	refreshPath         = "/auth/refresh"
	chatPath            = "/chat/completions"
	modelsPath          = "/ai/cline/recommended-models"
	userMePath          = "/users/me"
	userPlanPath        = "/users/me/plan"

	// defaultClientVersion 内置默认档（对齐官方客户端形态）。
	defaultClientVersion = "3.0.62"
)

// ClientVersion 申报的客户端版本（实测非必需，但更贴近官方、无代价），走
// X-CLIENT-VERSION 与 User-Agent。可由 config ext_versions 覆盖而无需重新编译；
// 空值回落内置默认。
var ClientVersion = defaultClientVersion

// SetClientVersion 应用版本覆盖（空值回落内置默认），返回生效值供启动日志打印。
func SetClientVersion(v string) string {
	if s := strings.TrimSpace(v); s != "" {
		ClientVersion = s
	} else {
		ClientVersion = defaultClientVersion
	}
	return ClientVersion
}

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	// AccessToken Cline 会话令牌（**含 `workos:` 前缀**）。
	AccessToken string `json:"access_token"`
	// RefreshToken 一次性轮换语义：并发续期可能互相作废，调用方需单飞。
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
}

// EnsurePrefix 补齐 `workos:` 前缀（续期/登记接口返回的令牌不带前缀）。
func EnsurePrefix(token string) string {
	token = strings.TrimSpace(token)
	if token == "" || strings.HasPrefix(token, TokenPrefix) {
		return token
	}
	return TokenPrefix + token
}

// NeedsRefresh 是否临近过期（提前 5 分钟）。
func (c *Credential) NeedsRefresh() bool {
	if c.AccessToken == "" || c.ExpiresAt == 0 {
		return true
	}
	return time.Now().Add(5*time.Minute).UnixMilli() >= c.ExpiresAt
}

// Pool 计费池。
type Pool int

const (
	PoolFree Pool = iota
	PoolPass
	PoolCloud
)

// PoolOf 由模型名判定所属计费池（无前缀的 id 归订阅池，与上游行为一致）。
func PoolOf(modelID string) Pool {
	switch {
	case strings.HasPrefix(modelID, FreePrefix):
		return PoolFree
	case strings.HasPrefix(modelID, CloudPrefix):
		return PoolCloud
	default:
		return PoolPass
	}
}

// BareModel 剥掉计费池前缀（出站前必须做——上游只认裸名）。
func BareModel(modelID string) string {
	for _, p := range []string{FreePrefix, PassPrefix, CloudPrefix} {
		if strings.HasPrefix(modelID, p) {
			return strings.TrimPrefix(modelID, p)
		}
	}
	return modelID
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
//
// 实测 api.cline.bot 与 api.workos.com 在国内可直连（各 ~4s / ~0.8s），
// 一般无需代理；但出口受限的网络可用它单独放行，不必把整条网关绑上去。
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

/* ── 设备授权登录 ────────────────────────────────────────────── */

// DeviceFlow 一次进行中的设备授权。
type DeviceFlow struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	CompleteURI     string
	Interval        int
	ExpiresIn       int
}

// StartDeviceFlow 申请设备码（WorkOS AuthKit）。
func StartDeviceFlow(ctx context.Context) (*DeviceFlow, error) {
	form := url.Values{"client_id": {WorkOSClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, WorkOSBase+deviceAuthorizePath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("申请设备码失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("申请设备码被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc struct {
		DeviceCode          string `json:"device_code"`
		UserCode            string `json:"user_code"`
		VerificationURI     string `json:"verification_uri"`
		VerificationURIComp string `json:"verification_uri_complete"`
		ExpiresIn           int    `json:"expires_in"`
		Interval            int    `json:"interval"`
		Error               string `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("设备码回执解析失败：%w", err)
	}
	if doc.Error != "" {
		return nil, fmt.Errorf("WorkOS 拒绝：%s", doc.Error)
	}
	if doc.DeviceCode == "" || doc.UserCode == "" {
		return nil, fmt.Errorf("设备码回执不完整")
	}
	if doc.Interval <= 0 {
		doc.Interval = 5
	}
	if doc.VerificationURI == "" {
		doc.VerificationURI = DeviceVerifyFallback
	}
	if doc.VerificationURIComp == "" {
		doc.VerificationURIComp = doc.VerificationURI + "?user_code=" + doc.UserCode
	}
	return &DeviceFlow{
		DeviceCode: doc.DeviceCode, UserCode: doc.UserCode,
		VerificationURI: doc.VerificationURI, CompleteURI: doc.VerificationURIComp,
		Interval: doc.Interval, ExpiresIn: doc.ExpiresIn,
	}, nil
}

// PollResult 一次轮询的结果。
type PollResult struct {
	Done  bool
	Cred  *Credential
	Error string // 面向用户的终态错误
}

// Poll 轮询一次授权；未完成时 Done=false。
//
// 拿到 WorkOS 令牌后会立刻做「登记」把它换成 Cline 会话令牌——省掉这一步
// 得到的凭据看起来正常，但发请求会被上游拒。
func (f *DeviceFlow) Poll(ctx context.Context) (*PollResult, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {f.DeviceCode},
		"client_id":   {WorkOSClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, WorkOSBase+deviceAuthPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("轮询失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))

	var doc struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &doc)

	switch doc.Error {
	case "":
		if doc.AccessToken == "" {
			return &PollResult{Done: false}, nil
		}
		cred, err := registerSession(ctx, doc.AccessToken, doc.RefreshToken)
		if err != nil {
			// 拿到 WorkOS 令牌却登记不了 —— 终态，把原因告诉用户，
			// 当成「还没授权」会让人对着不会发生的结果一直等。
			return &PollResult{Error: err.Error()}, nil
		}
		return &PollResult{Done: true, Cred: cred}, nil
	case "authorization_pending", "slow_down":
		return &PollResult{Done: false}, nil
	case "expired_token":
		return &PollResult{Error: "设备码已过期，请重新发起登录"}, nil
	case "access_denied":
		return &PollResult{Error: "授权被拒绝"}, nil
	default:
		msg := "WorkOS 返回：" + doc.Error
		if doc.ErrorDescription != "" {
			msg += "（" + doc.ErrorDescription + "）"
		}
		return &PollResult{Error: msg}, nil
	}
}

// registerSession 把 WorkOS 令牌换成 Cline 会话令牌（登录流程的第四步）。
func registerSession(ctx context.Context, accessToken, refreshToken string) (*Credential, error) {
	body, _ := json.Marshal(map[string]string{
		"accessToken":  accessToken,
		"refreshToken": refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+registerPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CLIENT-TYPE", ClientType)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("令牌登记失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("令牌登记被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
			UserInfo     struct {
				Email   string `json:"email"`
				Name    string `json:"name"`
				Subject string `json:"subject"`
				UserID  string `json:"clineUserId"`
			} `json:"userInfo"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("令牌登记回执解析失败：%w", err)
	}
	if env.Data.AccessToken == "" {
		return nil, fmt.Errorf("令牌登记回执不含 accessToken")
	}
	cred := &Credential{
		AccessToken:  EnsurePrefix(env.Data.AccessToken),
		RefreshToken: env.Data.RefreshToken,
		ExpiresAt:    parseExpiresAt(env.Data.ExpiresAt),
		Email:        env.Data.UserInfo.Email,
		Name:         env.Data.UserInfo.Name,
		AccountID:    firstNonEmpty(env.Data.UserInfo.UserID, env.Data.UserInfo.Subject),
	}
	if cred.RefreshToken == "" {
		cred.RefreshToken = refreshToken
	}
	return cred, nil
}

// Refresh 续期会话令牌。
//
// ⚠️ refresh_token 在服务端是**一次性轮换**语义：并发请求同时发现临期时各自打一次
// 续期，后到的那次会拿着已作废的 refresh_token → 401 → 用户被踢下线。
// 调用方需保证同一账号同一时刻只有一次续期在飞（单飞）。
func Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, fmt.Errorf("缺少 refresh_token，需重新登录")
	}
	// grantType 是 camelCase —— 不是 OAuth 标准的 grant_type
	body, _ := json.Marshal(map[string]string{
		"refreshToken": cred.RefreshToken,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+refreshPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("续期失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("refresh_token 已失效，需重新登录（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("续期被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("续期回执解析失败：%w", err)
	}
	if env.Data.AccessToken == "" {
		return nil, fmt.Errorf("续期回执不含 accessToken")
	}
	out := *cred
	out.AccessToken = EnsurePrefix(env.Data.AccessToken)
	if env.Data.RefreshToken != "" {
		out.RefreshToken = env.Data.RefreshToken
	}
	if exp := parseExpiresAt(env.Data.ExpiresAt); exp != 0 {
		out.ExpiresAt = exp
	}
	return &out, nil
}

// parseExpiresAt 解析过期时间：登记/续期接口回 **ISO8601 字符串**，
// 而桌面端 providers.json 里是**毫秒数**——同名不同型，两种都要认。
func parseExpiresAt(v any) int64 {
	switch t := v.(type) {
	case string:
		if t == "" {
			return 0
		}
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return ts.UnixMilli()
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n
		}
	case float64:
		return int64(t)
	}
	return 0
}

/* ── 模型目录 ────────────────────────────────────────────────── */

// Model 模型条目（含所属计费池）。
type Model struct {
	ID       string
	Name     string
	Pool     Pool
	PoolName string
}

// Catalog 一次目录拉取的结果（按池分组）。
type Catalog struct {
	Recommended []Model
	Free        []Model
	Pass        []Model
	Cloud       []Model
}

// All 全部模型（顺序：免费 → 订阅 → 云端 → 推荐）。
func (c *Catalog) All() []Model {
	var out []Model
	out = append(out, c.Free...)
	out = append(out, c.Pass...)
	out = append(out, c.Cloud...)
	out = append(out, c.Recommended...)
	return out
}

// ListModels 拉取上游模型目录（**免鉴权**，实测公开可读）。
//
// 归池以**响应里的分组**为准，不只看前缀：免费组里也有少数不带 `cline-free/`
// 前缀的条目（如 `stealth/…`），只看前缀会漏掉它们。
func ListModels(ctx context.Context) (*Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+modelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CLIENT-TYPE", ClientType)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型目录失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}

	type entry struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var doc struct {
		Recommended []entry `json:"recommended"`
		Free        []entry `json:"free"`
		Pass        []entry `json:"clinePass"`
		Cloud       []entry `json:"clineCloud"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("模型目录解析失败：%w", err)
	}

	conv := func(list []entry, pool Pool, poolName string) []Model {
		out := make([]Model, 0, len(list))
		for _, e := range list {
			if e.ID == "" {
				continue
			}
			name := e.Name
			if name == "" {
				name = e.ID
			}
			out = append(out, Model{ID: e.ID, Name: name, Pool: pool, PoolName: poolName})
		}
		return out
	}
	return &Catalog{
		Recommended: conv(doc.Recommended, PoolPass, "推荐"),
		Free:        conv(doc.Free, PoolFree, "免费"),
		Pass:        conv(doc.Pass, PoolPass, "订阅"),
		Cloud:       conv(doc.Cloud, PoolCloud, "云端"),
	}, nil
}

/* ── 对话 ────────────────────────────────────────────────────── */

// Chat 发起对话，返回原始响应（上游即 OpenAI 格式，调用方直接透传）。
//
// model 传**裸名**（已剥掉计费池前缀）。
func Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyChatHeaders(req, cred)
	return httpClient.Do(req)
}

func applyChatHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+EnsurePrefix(cred.AccessToken))
	// 产品面标识：缺了它免费池一律 403（`only available via Cline product surfaces`）
	req.Header.Set("X-CLIENT-TYPE", ClientType)
	req.Header.Set("User-Agent", "Cline/"+ClientVersion)
	req.Header.Set("X-CLIENT-VERSION", ClientVersion)
	// OpenRouter 那套来源标记，官方客户端会带
	req.Header.Set("HTTP-Referer", "https://cline.bot")
	req.Header.Set("X-Title", "Cline")
}

/* ── 余额 ────────────────────────────────────────────────────── */

// Balance 账户余额（credit）。
type Balance struct {
	Credits   float64 `json:"credits"` // 已换算（微 credit ÷ 1e6）
	PlanName  string  `json:"plan_name,omitempty"`
	RawMicro  float64 `json:"raw_microcredits,omitempty"`
	UserEmail string  `json:"user_email,omitempty"`
}

// FetchBalance 查余额。上游返回**微 credit**，除以 1e6 才是 credit。
func FetchBalance(ctx context.Context, cred *Credential) (*Balance, error) {
	me, err := getJSON(ctx, APIBase+userMePath, cred)
	if err != nil {
		return nil, err
	}
	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(me, &user)
	if user.ID == "" {
		return nil, fmt.Errorf("用户信息不含 id")
	}

	balRaw, err := getJSON(ctx, APIBase+"/users/"+url.PathEscape(user.ID)+"/balance", cred)
	if err != nil {
		return nil, err
	}
	var bal struct {
		Balance float64 `json:"balance"`
	}
	_ = json.Unmarshal(balRaw, &bal)

	out := &Balance{Credits: bal.Balance / 1e6, RawMicro: bal.Balance, UserEmail: user.Email}
	if planRaw, err := getJSON(ctx, APIBase+userPlanPath, cred); err == nil {
		var plan struct {
			Name string `json:"name"`
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(planRaw, &plan)
		out.PlanName = firstNonEmpty(plan.Name, plan.Plan)
	}
	return out, nil
}

func getJSON(ctx context.Context, u string, cred *Credential) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+EnsurePrefix(cred.AccessToken))
	req.Header.Set("X-CLIENT-TYPE", ClientType)
	req.Header.Set("User-Agent", "Cline/"+ClientVersion)
	req.Header.Set("X-CLIENT-VERSION", ClientVersion)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	// 管理接口同样包在信封里
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Data) > 0 {
		return env.Data, nil
	}
	return raw, nil
}

/* ── 小工具 ──────────────────────────────────────────────────── */

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
