// Package autoclaw AutoClaw（智谱 autoglm）通道：手机短信登录 + OpenAI 兼容直连。
//
// 协议事实来自公开实现（aimod-cc/agent2api 的 autoclaw provider，MIT）
// 与**本机实测核对**（域名可达性、路径存在性）。
//
// ── 两个地区 ─────────────────────────────────────────────────
//
// 同一套客户端代码的两个构建（编译期常量 isOversea），协议与客户端指纹
// （appId/appKey）**逐字相同**，只有站点不同：
//
//	          userapi（账号 / 积分 / 签到 / 刷新）              LLM 代理
//	国内  https://autoglm-acceleration-api.zhipuai.cn  …/autoclaw-proxy/proxy/autoclaw
//	国际  https://autoglm-api.autoglm.ai               …/autoclaw-proxy/proxy/autoclaw
//
// 地区是**凭据的属性**（一个账号只属于一个站点），随凭据持久化。
//
// ── 登录 ─────────────────────────────────────────────────────
//
// 国内版走**手机短信**（全自动，与 Loomy 同形）：
//
//	POST {userapi}/userapi/v1/agent-send-code  {"phone","source_id":"autoclaw","device_id"}
//	POST {userapi}/userapi/v1/agent-login/     {"phone","code","platform":"web","source_id","device_id"}
//
// 国际版**没有短信**（上游只在国际版关了短信入口），主登录是 Zai/Google OAuth，
// 且被阿里云风控验证码挡着；本包不实现那条链路，国际版账号需从桌面端导入或
// 手工填凭据。
//
// ── 三处「踩空后表现很像没权限」的口径 ───────────────────────
//
//  1. **`X-Version` 是模型目录的版本门控**：不带它时上游只下发 3–4 条模型
//     （缺 glm-5.3-flash 等），看起来像「账号没这些模型」。带上 1.18.5 才是全量。
//  2. **userapi 域要 `X-Harness-Type: zcode`，chat 域不能带**：上游对
//     /chat/completions 上的这个值区别对待（403 pay-view / 406）。两条链路
//     刻意不一致，别「统一」掉。
//  3. **签名 `X-Auth-Sign = MD5("{appId}&{ts}&{appKey}")`，ts 是秒**（不是毫秒）。
//     签名错时上游回 code 400002，需降级到 `agent-refresh`。
//
// appId/appKey 是**客户端指纹**而非我们的密钥（内嵌在官方客户端里，两地相同），
// 所以硬编码在这里是正确的——做成可配置只会让签名对不上。
package autoclaw

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Region 地区。
type Region string

const (
	RegionCN   Region = "cn"
	RegionIntl Region = "intl"
)

// ParseRegion 解析地区串（空/未知一律回落国内版）。
func ParseRegion(s string) Region {
	if strings.EqualFold(strings.TrimSpace(s), string(RegionIntl)) {
		return RegionIntl
	}
	return RegionCN
}

// Label 展示名。
func (r Region) Label() string {
	if r == RegionIntl {
		return "国际版"
	}
	return "国内版"
}

// UserAPI 账号/积分/刷新域。
func (r Region) UserAPI() string {
	if r == RegionIntl {
		return "https://autoglm-api.autoglm.ai"
	}
	return "https://autoglm-acceleration-api.zhipuai.cn"
}

// ChatBase LLM 代理域（OpenAI 兼容）。
func (r Region) ChatBase() string { return r.UserAPI() + "/autoclaw-proxy/proxy/autoclaw" }

// SupportsSMS 是否支持手机短信登录（只有国内版）。
func (r Region) SupportsSMS() bool { return r != RegionIntl }

const (
	// AuthAppID / AuthAppKey 桌面端客户端内嵌的**公开**指纹（两地逐字相同）。
	AuthAppID  = "100003"
	AuthAppKey = "38d2391985e2369a5fb8227d8e6cd5e5"

	// ClientVersion 申报的客户端版本。**它是模型目录的版本门控**：
	// 不带时上游只下发 3–4 条模型，看起来像账号没有这些模型。
	// 值跟着真实客户端走；过时的症状是目录回落成旧清单而不是报错。
	ClientVersion = "1.18.5"

	SourceID = "autoclaw"

	sendCodePath = "/userapi/v1/agent-send-code"
	loginPath    = "/userapi/v1/agent-login/"
	refreshPath  = "/userapi/v1/refresh"
	// refreshFallbackPath 签名校验失败（code 400002）时的降级路径。
	refreshFallbackPath = "/userapi/v1/agent-refresh"
)

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	// Region 账号所属站点（凭据属性，随凭据持久化）。
	Region Region `json:"region,omitempty"`
	// Token 访问令牌。
	Token string `json:"token"`
	// RefreshToken 服务端每次刷新会**轮换**它——并发刷新会互相作废，
	// 调用方需单飞。
	RefreshToken string `json:"refresh_token,omitempty"`
	// DeviceID 设备标识（登录时生成，刷新与登录沿用同一个）。
	DeviceID  string `json:"device_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // 毫秒
	// PhoneTail 手机号掩码（展示用，如 138****0000）。
	PhoneTail string `json:"phone_tail,omitempty"`
}

// NeedsRefresh 是否临近过期（提前 5 分钟）。
//
// 与源实现一致：**没有过期时间就不刷新**——没有依据就说「临期」会让每个请求
// 都去打一次刷新接口。
func (c *Credential) NeedsRefresh() bool {
	if c.Token == "" || c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(5*time.Minute).UnixMilli() >= c.ExpiresAt
}

// CanRefresh 是否具备刷新条件。
func (c *Credential) CanRefresh() bool { return c.RefreshToken != "" }

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

/* ── 客户端指纹头 ────────────────────────────────────────────── */

// userAPIHeaders userapi 域的请求头（刷新 / 目录 / 余额 / 签到）。
//
// 与 brandHeaders 的**唯一差别**是这里带 `X-Harness-Type: zcode`——上游对
// /chat/completions 上的这个值区别对待（403/406），两条链路刻意不一致。
func userAPIHeaders(token string) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := md5.Sum([]byte(AuthAppID + "&" + ts + "&" + AuthAppKey))
	h := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "*/*",
		"X-Version":        ClientVersion,
		"X-Product":        "autoclaw",
		"X-Client-Type":    "pc",
		"X-Harness-Type":   "zcode",
		"X-Tm":             "win",
		"X-Lang":           "zh-CN",
		"X-Channel":        "official",
		"X-Auth-Appid":     AuthAppID,
		"X-Auth-TimeStamp": ts,
		"X-Auth-Sign":      hex.EncodeToString(sum[:]),
		"X-Trace-Id":       newRequestID(),
	}
	if token != "" {
		h["authorization"] = "Bearer " + token
	}
	return h
}

// chatHeaders chat 域的请求头（**不带** X-Harness-Type）。
func chatHeaders(token, model string) map[string]string {
	return map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "*/*",
		"X-Product":       "autoclaw",
		"X-Client-Type":   "pc",
		"X-Tm":            "win",
		"X-Version":       ClientVersion,
		"X-Lang":          "zh-CN",
		"X-Channel":       "official",
		"x_trace_id":      "autoclaw-desktop",
		"X-Authorization": "Bearer " + token,
		"X-Request-Id":    newRequestID(),
		"X-Request-Model": model,
	}
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newDeviceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

/* ── 手机短信登录（国内版） ──────────────────────────────────── */

// SendCode 发送短信验证码，返回本次登录要沿用的 device_id。
//
// 上游把设备与登录会话绑定，所以 send_code 与 login 必须用同一个 device_id。
func SendCode(ctx context.Context, region Region, phone string) (string, error) {
	if !region.SupportsSMS() {
		return "", fmt.Errorf("国际版不支持短信登录（上游已关闭该入口），请从桌面端导入或手工填写凭据")
	}
	deviceID := newDeviceID()
	body, _ := json.Marshal(map[string]string{
		"phone": phone, "source_id": SourceID, "device_id": deviceID,
	})
	raw, err := postJSON(ctx, region.UserAPI()+sendCodePath, body, userAPIHeaders(""))
	if err != nil {
		return "", err
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Result bool `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", fmt.Errorf("验证码回执解析失败")
	}
	if doc.Code != 0 {
		return "", fmt.Errorf("%s", describeLoginError(doc.Code, doc.Msg))
	}
	if !doc.Data.Result {
		return "", fmt.Errorf("验证码发送失败，请稍后重试")
	}
	return deviceID, nil
}

// LoginWithCode 用短信验证码登录，返回凭据。
func LoginWithCode(ctx context.Context, region Region, phone, code, deviceID string) (*Credential, error) {
	if !region.SupportsSMS() {
		return nil, fmt.Errorf("国际版不支持短信登录")
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return nil, fmt.Errorf("请填写 6 位数字验证码")
	}
	if deviceID == "" {
		deviceID = newDeviceID()
	}
	body, _ := json.Marshal(map[string]string{
		"phone": phone, "code": code, "platform": "web",
		"source_id": SourceID, "device_id": deviceID,
	})
	raw, err := postJSON(ctx, region.UserAPI()+loginPath, body, userAPIHeaders(""))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token        string `json:"token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			UserID       string `json:"user_id"`
			ExpiresAt    any    `json:"expires_at"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("登录回执解析失败")
	}
	if doc.Code != 0 {
		return nil, fmt.Errorf("%s", describeLoginError(doc.Code, doc.Msg))
	}
	token := firstNonEmpty(doc.Data.Token, doc.Data.AccessToken)
	if token == "" {
		return nil, fmt.Errorf("登录回执不含 token")
	}
	return &Credential{
		Region:       region,
		Token:        token,
		RefreshToken: doc.Data.RefreshToken,
		DeviceID:     deviceID,
		UserID:       doc.Data.UserID,
		ExpiresAt:    parseExpiresAt(doc.Data.ExpiresAt),
		PhoneTail:    MaskPhone(phone),
	}, nil
}

// MaskPhone 手机号掩码（展示用）。
func MaskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

// describeLoginError 把上游业务码翻成人话（源实现的对照表）。
func describeLoginError(code int, upstreamMsg string) string {
	base := map[int]string{
		1001: "手机号格式不正确",
		1002: "验证码错误或已过期",
		1003: "验证码发送过于频繁，请稍后再试",
		1004: "该手机号今日验证码次数已达上限",
		429:  "请求过于频繁，请稍后再试",
	}[code]
	if base == "" {
		base = fmt.Sprintf("登录失败（上游 code %d）", code)
	}
	if upstreamMsg != "" {
		base += "：" + upstreamMsg
	}
	return base
}

/* ── 续期 ────────────────────────────────────────────────────── */

// Refresh 续期访问令牌。
//
// ⚠️ 服务端每次刷新会**轮换 refresh_token**：并发刷新会互相作废，调用方需单飞。
//
// 签名校验失败（code 400002）时降级到 agent-refresh —— 这是上游对签名头的
// 兜底路径，源实现同样这么做。
func Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if !cred.CanRefresh() {
		return nil, fmt.Errorf("缺少 refresh_token，需重新登录")
	}
	region := ParseRegion(string(cred.Region))
	payload := map[string]string{"refresh_token": cred.RefreshToken, "source_id": SourceID}
	if cred.DeviceID != "" {
		payload["device_id"] = cred.DeviceID
	}
	body, _ := json.Marshal(payload)

	raw, err := postJSON(ctx, region.UserAPI()+refreshPath, body, userAPIHeaders(cred.Token))
	if err != nil {
		return nil, err
	}
	var probe struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(raw, &probe)
	if probe.Code == 400002 {
		// 签名校验失败 → 降级路径
		raw, err = postJSON(ctx, region.UserAPI()+refreshFallbackPath, body, userAPIHeaders(cred.Token))
		if err != nil {
			return nil, err
		}
	}

	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token        string `json:"token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			UserID       string `json:"user_id"`
			ExpiresAt    any    `json:"expires_at"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("续期回执解析失败")
	}
	if doc.Code != 0 {
		return nil, fmt.Errorf("续期被拒（code %d）：%s", doc.Code, doc.Msg)
	}
	token := firstNonEmpty(doc.Data.Token, doc.Data.AccessToken)
	if token == "" {
		return nil, fmt.Errorf("续期回执不含 token")
	}
	out := *cred
	out.Token = token
	if doc.Data.RefreshToken != "" {
		out.RefreshToken = doc.Data.RefreshToken
	}
	if exp := parseExpiresAt(doc.Data.ExpiresAt); exp != 0 {
		out.ExpiresAt = exp
	}
	if out.UserID == "" {
		out.UserID = doc.Data.UserID
	}
	return &out, nil
}

/* ── 模型目录 ────────────────────────────────────────────────── */

// Model 模型条目。
type Model struct {
	ID   string
	Name string
}

// ListModels 拉取模型目录（`GET {chatBase}-model-config`）。
//
// ⚠️ 必须带 `X-Version`：它是**版本门控**，不带时上游只下发 3–4 条模型
// （缺 glm-5.3-flash 等），看起来像账号没有这些模型。
func ListModels(ctx context.Context, region Region, token string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, region.ChatBase()+"-model-config", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range userAPIHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型目录失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	// 顶层直接是 {"models":[...]}，**没有 {code,data} 信封**
	var doc struct {
		Models []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			ModelID  string `json:"model_id"`
			ModelKey string `json:"model_key"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("模型目录解析失败：%w", err)
	}
	out := make([]Model, 0, len(doc.Models))
	for _, m := range doc.Models {
		id := firstNonEmpty(m.ID, m.ModelID, m.ModelKey)
		if id == "" {
			continue
		}
		name := firstNonEmpty(m.Name, id)
		out = append(out, Model{ID: id, Name: name})
	}
	return out, nil
}

/* ── 对话 ────────────────────────────────────────────────────── */

// Chat 发起对话（OpenAI 协议，SSE 为裸 chunk 帧，调用方直接透传）。
func Chat(ctx context.Context, cred *Credential, model string, body []byte) (*http.Response, error) {
	region := ParseRegion(string(cred.Region))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, region.ChatBase(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range chatHeaders(cred.Token, model) {
		req.Header.Set(k, v)
	}
	return httpClient.Do(req)
}

/* ── 小工具 ──────────────────────────────────────────────────── */

func postJSON(ctx context.Context, u string, body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
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

// parseExpiresAt 过期时间两种形态都认（毫秒数 / ISO8601 字符串）。
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
