// Package trae Trae（字节 SOLO）通道：本机回调授权 + SOLO 信封对话。
//
// 协议事实来自公开实现（aimod-cc/agent2api 的 trae provider，MIT）。
//
// ── 登录：本机回调 ───────────────────────────────────────────
//
// 与其它通道不同，Trae 用**本机回环回调**：网关临时监听一个随机端口，
// 浏览器授权后跳回 `http://127.0.0.1:<port>/authorize`，回调里带授权码。
// 因此**浏览器必须与网关在同一台机器上**（面板通常就是本机访问，符合）。
//
//  1. 绑定回环端口，生成 PKCE 对 + 设备密钥对（RSA）
//  2. POST {api}/cloudide/api/v3/trae/GetLoginGuidance → 该去哪个登录页
//     （回的是 Result.LoginHost，**网页域** www.trae.cn，只用来开浏览器）
//  3. 浏览器打开 {web}/authorization?login_version=1&auth_from=solo&…
//  4. 回调带回 auth_code → POST {api}/trae/api/v3/oauth/ExchangeToken
//     注意换证走的是 **API 域**，不是上面那个网页域：往网页域 POST 拿到的是一坨
//     HTML（字节的 JS 挑战页），解析必然炸在「invalid character '<'」。
//
// ── 对话：SOLO 白名单重建 ────────────────────────────────────
//
// 上游契约极小，**多带字段不是被忽略、是被拒**（带 agent_type / thinking 一族
// 会得到流内 4023 或直接炸流）。所以出站请求体是**白名单重建**，只搬清单里的键。
//
// 四处「照 OpenAI 形状直发必定 4001」的 SOLO 变形：
//
//	① tools[].function.parameters 必须是 **JSON 字符串**（OpenAI 是对象）
//	② tool_choice 必须是**裸字符串**
//	③ assistant 的 tool_calls[].function 要改名 **function_call**
//	④ developer 角色上游不认，降成 system
//
// 响应侧：上游**不发 `[DONE]`**（结束帧是 `event: done`），业务错误藏在 200 的
// 流里（`event: error`），`token_usage` 是"欠着"的（挂在下一帧一起走）。
package trae

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// AgentBase SOLO 对话与模型目录域。
	AgentBase = "https://trae-api-cn.mchost.guru"
	// ChatPath 对话端点。
	ChatPath = "/api/agent/v3/llm_utils_chat"
	// ModelsPath 模型目录端点（POST，不是 GET）。
	ModelsPath = "/api/ide/v1/get_detail_param"

	// GuidanceURL 问上游"该去哪个登录页"。
	GuidanceURL = "https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance"
	// DefaultLoginHost 兜底登录域。
	DefaultLoginHost = "https://api.trae.cn"
	// ExchangePath 授权码换令牌。
	ExchangePath = "/trae/api/v3/oauth/ExchangeToken"
	// CallbackPath 本机回调路径。
	CallbackPath = "/authorize"

	// ClientIDSolo SOLO 谱系的 client_id（公开值）。
	ClientIDSolo = "en1oxy7wnw8j9n"

	IDEVersion     = "0.1.61"
	IDEVersionCode = "20260820"
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	UserAgent      = "Trae/0.1.61"
	PluginVersion  = "1.0.0"
	// 授权页按这几个参数判客户端形态（漏了直接 404）
	DeviceType = "windows"
	Env        = "prod"
	AppType    = "trae"

	// LoginTTL 一次登录会话的有效期。
	LoginTTL = 15 * time.Minute
)

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒
	UID          string `json:"uid,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	// DevicePrivatePEM 设备私钥（登录时生成，参与设备绑定，必须持久化）。
	DevicePrivatePEM string `json:"device_private_pem,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
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

/* ── PKCE 与设备密钥 ─────────────────────────────────────────── */

const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// pkcePair 生成 (verifier, challenge)。
func pkcePair() (string, string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	vb := make([]byte, len(b))
	for i, x := range b {
		vb[i] = pkceAlphabet[int(x)%len(pkceAlphabet)]
	}
	verifier := string(vb)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func uuidV4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// deviceKeyPair 生成设备密钥对（PEM）。私钥随凭据落盘——它参与设备绑定。
func deviceKeyPair() (publicPEM, privatePEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})), nil
}

/* ── 登录 ────────────────────────────────────────────────────── */

// LoginContext 一次进行中的登录。
type LoginContext struct {
	LoginTraceID     string
	CodeVerifier     string
	CodeChallenge    string
	DeviceID         string
	MachineID        string
	CallbackURL      string
	DevicePublicPEM  string
	DevicePrivatePEM string
	AuthURL          string
	LoginHost        string
	Port             int
}

// BuildAuthURL 组装授权链接。
//
// 参数顺序与编码方式都有讲究（官方页面对 `auth_callback_url` 做正则匹配，
// 刻意不编码）——按参考实现逐项对齐。
func buildAuthURL(loginHost string, c *LoginContext) string {
	if !strings.HasPrefix(loginHost, "http") {
		loginHost = "https://" + loginHost
	}
	q := []string{
		"login_version=1",
		"auth_from=solo",
		"login_channel=native_ide",
		"plugin_version=" + url.QueryEscape(PluginVersion),
		"auth_type=local",
		"client_id=" + ClientIDSolo,
		"redirect=0",
		"login_trace_id=" + url.QueryEscape(c.LoginTraceID),
		// 刻意不编码：官方页面对它做正则匹配
		"auth_callback_url=" + c.CallbackURL,
		"machine_id=" + url.QueryEscape(c.MachineID),
		"device_id=" + url.QueryEscape(c.DeviceID),
		"x_device_id=" + url.QueryEscape(c.DeviceID),
		"x_machine_id=" + url.QueryEscape(c.MachineID),
		"x_device_brand=" + url.QueryEscape(DeviceBrand),
		// 下面这几个漏了会让授权页直接 404 —— 官方页面按它们判客户端形态
		"x_device_type=" + url.QueryEscape(DeviceType),
		"x_os_version=" + url.QueryEscape(OSVersion),
		"x_env=" + url.QueryEscape(Env),
		"x_app_version=" + url.QueryEscape(IDEVersion),
		"x_app_type=" + url.QueryEscape(AppType),
		"code_challenge=" + url.QueryEscape(c.CodeChallenge),
		"code_challenge_method=S256",
		// solo 谱系要隐藏 SaaS 登录入口（非 solo 分支不带这个参数）
		"hide_saas_login=true",
	}
	return strings.TrimRight(loginHost, "/") + "/authorization?" + strings.Join(q, "&")
}

// NewLogin 绑定本机回环端口并生成登录上下文。
//
// 返回的 LoginContext 里带着**已经绑定成功的端口**——端口在绑定后才知道，
// 而回调地址要用它，所以必须先 bind 再拼 URL。
func NewLogin(ctx context.Context) (*LoginContext, net.Listener, error) {
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
	pub, priv, err := deviceKeyPair()
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	c := &LoginContext{
		LoginTraceID:     uuidV4(),
		CodeVerifier:     verifier,
		CodeChallenge:    challenge,
		DeviceID:         uuidV4(),
		MachineID:        uuidV4(),
		CallbackURL:      fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath),
		DevicePublicPEM:  pub,
		DevicePrivatePEM: priv,
		Port:             port,
	}
	c.LoginHost = requestLoginGuidance(ctx)
	c.AuthURL = buildAuthURL(c.LoginHost, c)
	return c, ln, nil
}

// requestLoginGuidance 问上游该去哪个登录页；全挂时兜到默认 host。
func requestLoginGuidance(ctx context.Context) string {
	body, _ := json.Marshal(map[string]string{"loginTraceID": "", "login_trace_id": ""})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GuidanceURL, bytes.NewReader(body))
	if err != nil {
		return DefaultLoginHost
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae/"+PluginVersion+" antigravity-cockpit-tools")
	resp, err := httpClient.Do(req)
	if err != nil {
		return DefaultLoginHost
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	// 真实回执是 PascalCase 的 Result.LoginHost（实测），
	// 但字段名随版本变过，几种写法都认一遍——认不出来就回落默认域，
	// 回落会让授权链接指向错的 host（打开 404）。
	var probe map[string]any
	if json.Unmarshal(raw, &probe) != nil {
		return DefaultLoginHost
	}
	if h := digLoginHost(probe); h != "" {
		return h
	}
	return DefaultLoginHost
}

// digLoginHost 递归找 login host（字段名有多种写法，层级也可能变）。
func digLoginHost(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"LoginHost", "login_host", "loginHost"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		for _, child := range t {
			if h := digLoginHost(child); h != "" {
				return h
			}
		}
	case []any:
		for _, child := range t {
			if h := digLoginHost(child); h != "" {
				return h
			}
		}
	}
	return ""
}

// Callback 本机回调里带回来的参数。
type Callback struct {
	AuthCode     string
	RefreshToken string
	LoginHost    string
	Error        string
	// RawQuery 原始 query（未解码）。只在排障时用：上游改回调形状时，
	// 只有原文能看出到底变了什么——解析器认不出的键全在这里。
	RawQuery string
}

// ParseCallback 解析回调 query。
//
// 键名有四五种写法（camelCase / snake / Pascal / 连字符），逐个探测——
// 上游不同版本发过不同形态。错误键**只看非空值**：`error=` 不算失败。
func ParseCallback(query url.Values) *Callback {
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(query.Get(k)); v != "" {
				return v
			}
		}
		return ""
	}

	// 错误分支优先，且一旦命中就不再读授权码（参考实现同样在 error 处直接 return）：
	// 读出来会让上层以为「拿到码了，只是顺带有个错误」。
	for _, k := range []string{"error", "error_code", "err", "errorCode", "error_description"} {
		if v := strings.TrimSpace(query.Get(k)); v != "" {
			return &Callback{Error: v}
		}
	}
	// 官方前端用它标记「这次不是要回调到本机」——此时回调里不会有授权码，
	// 继续等也是白等，直接给出可读的失败原因。
	if strings.TrimSpace(query.Get("isRedirect")) == "false" {
		return &Callback{Error: "isRedirect=false（上游未按本机回调方式跳转）"}
	}

	authCode := pick("authCode", "auth_code", "AuthCode", "authorization_code", "code")
	if authCode == "" {
		// 新版本把授权码包在 authCodeInfo 这个 **JSON 字符串**里再塞进 query，
		// 字段名还嵌套过几层。见 extractAuthCode。
		for _, k := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
			if raw := pick(k); raw != "" {
				if code := extractAuthCode(raw); code != "" {
					authCode = code
					break
				}
			}
		}
	}
	return &Callback{
		AuthCode:     authCode,
		RefreshToken: pick("refreshToken", "refresh_token", "RefreshToken", "refresh-token"),
		LoginHost:    pick("loginHost", "login_host", "LoginHost", "host", "consoleHost"),
	}
}

// extractAuthCode 从 authCodeInfo 的 JSON 串里挖出授权码。
//
// 字段名在版本间漂过，所以是**递归**找：嵌套对象/数组都要能挖到。
func extractAuthCode(raw string) string {
	var doc any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return ""
	}
	keys := []string{"authCode", "auth_code", "AuthCode", "AuthCodeToken", "code"}
	var walk func(v any) string
	walk = func(v any) string {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range keys {
				if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
			for _, nested := range t {
				if s := walk(nested); s != "" {
					return s
				}
			}
		case []any:
			for _, item := range t {
				if s := walk(item); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(doc)
}

// resolvesLogin 这次回调是否把登录推进到了可判定的状态。
//
// false 时监听器要**继续等下一个请求**：浏览器会先发 preconnect、favicon 探测
// 之类的杂音（它们也打到 /authorize，但 query 里什么都没有）。一次就收尾等于
// 把真回调挡在门外——实测表现是「授权页刚打开 2 秒就报『回调里没有授权码』」。
func (c *Callback) resolvesLogin() bool {
	return c.Error != "" || c.AuthCode != "" || c.RefreshToken != ""
}

// apiHostsFor 换证要依次试的 API 域候选（顺序即优先级）。
//
// 两把官方账号 API 源**钉在最前**，再接从登录 host 派生的候选——顺序是语义：
// `www.*` 那类 host 会回一整页 HTML（字节的 JS 挑战页），只能垫在后面当兜底。
// 实测两个官方域都返回正常 JSON（同一个 `Invalid client` 业务错误），
// 而 www.trae.cn 返回 HTML。
//
// 逐个试而不是只试一个：两个官方域互为备份，某个区域不可达时仍能走通。
func apiHostsFor(loginHost string) []string {
	out := []string{"https://api.trae.cn", "https://api.trae.com.cn"}
	host := strings.TrimSpace(loginHost)
	if host != "" {
		if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
			host = "https://" + host
		}
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			derived := ""
			switch {
			case strings.HasPrefix(u.Host, "www."):
				derived = u.Scheme + "://api." + strings.TrimPrefix(u.Host, "www.")
			case strings.HasPrefix(u.Host, "api."):
				derived = u.Scheme + "://" + u.Host
			}
			if derived != "" {
				out = append(out, derived)
			}
		}
	}
	// 去重保序
	seen := map[string]bool{}
	uniq := out[:0]
	for _, u := range out {
		if !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	return uniq
}

// apiHostFor 单个换证域（保留给只需要一个地址的调用方 / 测试）。
func apiHostFor(loginHost string) string {
	hosts := apiHostsFor(loginHost)
	if len(hosts) == 0 {
		return DefaultLoginHost
	}
	return hosts[0]
}

// Complete 用回调内容换凭据。
func Complete(ctx context.Context, c *LoginContext, cb *Callback) (*Credential, error) {
	if cb.Error != "" {
		return nil, fmt.Errorf("授权被拒：%s", cb.Error)
	}
	if cb.AuthCode == "" && cb.RefreshToken == "" {
		return nil, fmt.Errorf("回调里没有授权码也没有续期串")
	}

	body, _ := json.Marshal(map[string]any{
		"ClientID":     ClientIDSolo,
		"AuthCode":     cb.AuthCode,
		"CodeVerifier": c.CodeVerifier,
		"IDEVersion":   IDEVersion,
		"DeviceInfo":   deviceInfo(c),
	})

	// 依次试候选域：回调里的 login_host 是**网页域**，只能用来定位 API 域。
	var errs []string
	for _, host := range apiHostsFor(firstNonEmptyStr(cb.LoginHost, c.LoginHost)) {
		cred, err := exchangeAt(ctx, host, body, c)
		if err == nil {
			return cred, nil
		}
		errs = append(errs, host+"："+err.Error())
	}
	return nil, fmt.Errorf("换证失败（%s）", strings.Join(errs, "；"))
}

// exchangeAt 向单个域换证。
func exchangeAt(ctx context.Context, host string, body []byte, c *LoginContext) (*Credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+ExchangePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-cloudide-token", "")
	req.Header.Set("User-Agent", "Trae/"+PluginVersion+" antigravity-cockpit-tools")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	return parseTokenResponse(raw, c)
}

// deviceInfo 官方客户端形状。DevicePublicKey 为空会被判成设备绑定拒绝。
func deviceInfo(c *LoginContext) map[string]any {
	return map[string]any{
		"DeviceID":        c.DeviceID,
		"MachineID":       c.MachineID,
		"PlatformCode":    "SOLO_PC",
		"DeviceType":      "PC",
		"DeviceName":      "DESKTOP-CPASOLO",
		"DeviceModel":     DeviceBrand,
		"ClientVersion":   IDEVersion,
		"DevicePublicKey": c.DevicePublicPEM,
		"DeviceBrand":     "Microsoft",
		"DeviceCPU":       "",
		"OSInfo":          "windows",
		"OSVersion":       OSVersion,
	}
}

// describeDoc 回执的结构描述：路径 + 值类型/长度，**不含值本身**。
//
// 排障要用它——上游改字段名时只有结构能看出令牌挂在哪个键下（嵌套也要能展开）。
// 绝不能打原文：换证回执里就是令牌，截断也不安全。
func describeDoc(doc map[string]any) string {
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			if len(t) == 0 {
				out = append(out, prefix+":{}")
				return
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(prefix+"."+k, t[k])
			}
		case []any:
			out = append(out, fmt.Sprintf("%s:array[%d]", prefix, len(t)))
		case string:
			out = append(out, fmt.Sprintf("%s:string(%d)", prefix, len(t)))
		case nil:
			out = append(out, prefix+":null")
		case float64, bool, json.Number:
			out = append(out, fmt.Sprintf("%s:%v", prefix, t))
		default:
			out = append(out, fmt.Sprintf("%s:%T", prefix, v))
		}
	}
	for _, k := range keysOf(doc) {
		walk(k, doc[k])
	}
	if len(out) > 40 {
		out = append(out[:40], fmt.Sprintf("…共 %d 个字段", len(out)))
	}
	return "{" + strings.Join(out, ", ") + "}"
}

// keysOf 按字典序取 map 的键（结构描述要稳定，否则每次失败都不一样没法比对）。
func keysOf(doc map[string]any) []string {
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseTokenResponse 解析换证/续期回执。字段名有多种写法。
func parseTokenResponse(raw []byte, c *LoginContext) (*Credential, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("换证回执解析失败：%w", err)
	}
	// 可能包在信封里。**两个都要试**：这族 API 用的是 Tencent 风格的
	// Result 信封（同源的 GetLoginGuidance 就是 Result.LoginHost），
	// 只解 data 会得到「HTTP 200 却没有令牌」——看着像成功，实际一个字段都没读到。
	for _, k := range []string{"data", "Data", "result", "Result"} {
		if d, ok := doc[k].(map[string]any); ok {
			for n, v := range d {
				if _, exists := doc[n]; !exists {
					doc[n] = v
				}
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
	access := pick("access_token", "accessToken", "AccessToken", "token", "Token")
	refresh := pick("refresh_token", "refreshToken", "RefreshToken")
	if access == "" && refresh == "" {
		// 带上回执的**结构**而不是原文：上游改字段名时只有结构能看出令牌挂在
		// 哪个键下，而原文里可能就带着令牌本身（截断也不安全）。
		return nil, fmt.Errorf("换证成功但回执里没有任何令牌（回执结构 %s）", describeDoc(doc))
	}
	cred := &Credential{
		AccessToken:  access,
		RefreshToken: refresh,
		UID:          pick("uid", "user_id", "userId", "UserID"),
	}
	if c != nil {
		cred.DeviceID = c.DeviceID
		cred.MachineID = c.MachineID
		cred.DevicePrivatePEM = c.DevicePrivatePEM
	}
	if exp := firstNum(doc, "expires_at", "expiresAt", "ExpiresAt"); exp != 0 {
		// 秒 / 毫秒两种形态
		if exp < 1e12 {
			exp *= 1000
		}
		cred.ExpiresAt = exp
	}
	return cred, nil
}

/* ── 续期 ────────────────────────────────────────────────────── */

// Refresh 用 refresh_token 续期。
func Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if cred.RefreshToken == "" {
		return nil, fmt.Errorf("缺少 refresh_token，需重新登录")
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     ClientIDSolo,
		"RefreshToken": cred.RefreshToken,
		"IDEVersion":   IDEVersion,
		"DeviceInfo":   refreshDeviceInfo(cred),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DefaultLoginHost+ExchangePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae/"+PluginVersion+" antigravity-cockpit-tools")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("续期失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("续期被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	fresh, err := parseTokenResponse(raw, nil)
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
	if out.UID == "" {
		out.UID = fresh.UID
	}
	return &out, nil
}

func refreshDeviceInfo(cred *Credential) map[string]any {
	return map[string]any{
		"DeviceID":        cred.DeviceID,
		"MachineID":       cred.MachineID,
		"PlatformCode":    "SOLO_PC",
		"DeviceType":      "PC",
		"DeviceName":      "DESKTOP-CPASOLO",
		"DeviceModel":     DeviceBrand,
		"ClientVersion":   IDEVersion,
		"DevicePublicKey": "",
		"DeviceBrand":     "Microsoft",
		"DeviceCPU":       "",
		"OSInfo":          "windows",
		"OSVersion":       OSVersion,
	}
}

/* ── 请求头 ──────────────────────────────────────────────────── */

// applySoloHeaders 写 SOLO 通道的头。
//
// 三条容易漏的：同一个 token 要出现在**三个头**里；`X-Uid` / `X-Machine-Id` /
// `X-Device-Id` **为空就不发**（发空值会被当成另一个身份）；Accept 随流式变。
func applySoloHeaders(req *http.Request, cred *Credential, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("X-Cloudide-Token", cred.AccessToken)
	req.Header.Set("X-Ide-Token", cred.AccessToken)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	if cred.MachineID != "" {
		req.Header.Set("X-Machine-Id", cred.MachineID)
	}
	if cred.DeviceID != "" {
		req.Header.Set("X-Device-Id", cred.DeviceID)
	}
}

/* ── 模型目录 ────────────────────────────────────────────────── */

// Model 模型条目。
type Model struct {
	ID   string
	Name string
}

// ListModels 拉模型目录（POST，不是 GET）。
func ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	body, _ := json.Marshal(map[string]any{"Function": "llm_utils_chat"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AgentBase+ModelsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applySoloHeaders(req, cred, false)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型目录失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	return parseCatalog(raw), nil
}

// parseCatalog 从上游目录响应里取出模型清单（形状不固定，递归找 id）。
func parseCatalog(raw []byte) []Model {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Model
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			id, _ := t["id"].(string)
			if id == "" {
				id, _ = t["model_name"].(string)
			}
			if id == "" {
				id, _ = t["config_name"].(string)
			}
			if id != "" && !seen[id] {
				seen[id] = true
				name, _ := t["name"].(string)
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

/* ── 对话 ────────────────────────────────────────────────────── */

// Chat 发起对话（SOLO 信封）。
func Chat(ctx context.Context, cred *Credential, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AgentBase+ChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applySoloHeaders(req, cred, stream)
	return httpClient.Do(req)
}

/* ── 小工具 ──────────────────────────────────────────────────── */

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

// firstNonEmptyStr 取第一个非空串（回调与上下文里同一个字段可能只在一处有）。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
//
// 浏览器授权后跳到 `http://127.0.0.1:<port>/authorize?...`，这里接住它、
// 回一个「可以关掉本页」的页面，并把 query 交回调用方。
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
//
// 返回 ok=false 表示这只是一次杂音请求（query 里没有可判定的登录信息），
// 调用方应继续等下一个——见 Callback.resolvesLogin 的说明。
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

	// 无论这次是不是杂音都要先回一页：浏览器在等响应，不回它会一直转圈，
	// 还可能因此重试、把真正的回调挤到后面。
	body := "<html><meta charset=\"utf-8\"><body style=\"font-family:system-ui;padding:40px;text-align:center\">" +
		"<h2>授权完成</h2><p>可以关闭本页，回到管理面板查看结果。</p></body></html>"
	resp := "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
	_, _ = conn.Write([]byte(resp))

	return cb, cb.resolvesLogin()
}
