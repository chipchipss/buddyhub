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
//	POST {userapi}/userapi/v1/agent-login       {"phone","code","platform":"web","source_id","device_id"}
//
// ⚠️ 登录里的 `code` 必须是 **JSON 数字**（`123456`），传字符串会得到
// `400001 请求数据有问题` —— 一个与验证码无关的参数错误，见 LoginWithCode 注释。
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
	// loginPath **不要**带结尾斜杠：`/agent-login/` 会被上游 307 到
	// `/agent-login`（实测多绕一圈才拿到 400001），直接写最终路径。
	loginPath   = "/userapi/v1/agent-login"
	refreshPath = "/userapi/v1/refresh"
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

	// 沙箱 relay（1.18.x 官方对话路径）：EnsureSandbox 成功后写回，下轮命中缓存。
	// omitempty：旧凭据没这些字段也能正常反序列化（零值触发 EnsureSandbox 申请）。
	SandboxID           string `json:"sandbox_id,omitempty"`
	SandboxEndpoint     string `json:"sandbox_endpoint,omitempty"`
	SandboxEndTimestamp int64  `json:"sandbox_end_timestamp,omitempty"`
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

// stripBearer 去掉 `Bearer ` 前缀。
//
// 上游登录回执里的 `token` 自带这个前缀，手工粘贴凭据的用户也常把
// `Bearer xxx` 整段粘进来——而 header 构造时还要再加一次，拼成
// `Bearer Bearer xxx` 就会被上游判成 Invalid token。在构造头的地方统一剥，
// 比要求每个调用方都记住「别带前缀」可靠。
func stripBearer(v string) string {
	v = strings.TrimSpace(v)
	for _, p := range []string{"bearer ", "Bearer ", "BEARER "} {
		if strings.HasPrefix(v, p) {
			return strings.TrimSpace(v[len(p):])
		}
	}
	return v
}

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
		h["authorization"] = "Bearer " + stripBearer(token)
	}
	return h
}

// chatHeaders chat 域的请求头（**不带** X-Harness-Type）。
func chatHeaders(token, model string) map[string]string {
	token = stripBearer(token)
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
	phone, err := NormalizePhone(phone)
	if err != nil {
		return "", err
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
	phone, err := NormalizePhone(phone)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return nil, fmt.Errorf("请填写 6 位数字验证码")
	}
	codeNum, aerr := strconv.Atoi(code)
	if aerr != nil {
		return nil, fmt.Errorf("请填写 6 位数字验证码")
	}
	if deviceID == "" {
		deviceID = newDeviceID()
	}
	// ⚠️ `code` 必须是 **JSON 数字**，不能是字符串。
	//
	// 传字符串上游回 `400001 请求数据有问题`——一个与验证码完全无关的参数错误，
	// 会把排查方向整个带偏（看着像「请求体形状不对」，实际只是类型错）。实测
	// （同一手机号同一时刻，2026-10-01）：
	//
	//	code 字符串 "123456" → 400001 请求数据有问题
	//	code 数字   123456   → 630201 验证码已过期 / 630202 验证码错误
	//
	// 也就是说：拿到 400001 说明**请求根本没被受理**，只有拿到 630xxx 才是
	// 真的在讨论验证码。所以这里用 map[string]any 而不是 map[string]string。
	body, _ := json.Marshal(map[string]any{
		"phone": phone, "code": codeNum, "platform": "web",
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
	// 上游同时给 `token` 与 `access_token`，**权威的是 access_token**（源实现
	// 也只读它）：`token` 那份带着 `Bearer ` 前缀，拿去请求目录/对话接口会得到
	// `401 Invalid token`（header 又前缀一次，变成 `Bearer Bearer eyJ…`）。
	token := stripBearer(firstNonEmpty(doc.Data.AccessToken, doc.Data.Token))
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

// NormalizePhone 规范化中国大陆手机号。
//
// 用户从各种地方复制来的号码常带空格、横线、`+86` / `86` 前缀——**原样透传
// 上游会判「手机号格式不正确」**（发码可能容忍，登录那步就拒）。这里统一剥干净
// 并校验 11 位（1 开头、第二位 2-9、全数字），不合规直接本地拦下、不打上游。
func NormalizePhone(raw string) (string, error) {
	var b strings.Builder
	for _, ch := range raw {
		// 空白与横线一律剥掉；ch < 0x20 覆盖 tab/换行/回车等控制字符
		if ch == ' ' || ch == '-' || ch < 0x20 {
			continue
		}
		b.WriteRune(ch)
	}
	digits := b.String()
	digits = strings.TrimPrefix(digits, "+86")
	if rest := strings.TrimPrefix(digits, "86"); rest != digits && len(rest) == 11 {
		digits = rest
	}
	if len(digits) != 11 || digits[0] != '1' || digits[1] < '2' || digits[1] > '9' {
		return "", fmt.Errorf("请填写 11 位中国大陆手机号")
	}
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return "", fmt.Errorf("请填写 11 位中国大陆手机号")
		}
	}
	return digits, nil
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
		// 630xxx 一族才是「在讨论验证码」：说明请求已被受理。
		630201: "验证码已过期，请重新发送验证码",
		630202: "验证码不正确，请检查后重试",
		630101: "获取验证码过于频繁，请稍后再试",
		// 400001 是**参数错误**，不是验证码错误 —— 实测 code 传字符串（而非
		// JSON 数字）时就是它。所以这条文案要指向「请求没被受理」，别让用户
		// 去反复重发验证码。
		400001: "请求未被上游受理（上游 code 400001 参数错误）",
		400002: "请求签名校验失败（本机时钟可能有偏差），请校准系统时间后重试",
		400000: "登录态已失效，请重新登录",
		410000: "登录态已失效，请重新登录",
		// 631002 原文是「当前版本已停止服务，请前往官网下载最新版」——不是版本
		// 问题，而是**缺少风控验证**（换任何 X-Version 都是这个码）。直译会把
		// 用户引向「升级客户端」这个错误方向。
		631002: "上游要求过风控验证，本次登录无法完成；请改用「手动填写凭据」",
		630014: "风控验证未通过，请稍后重试；若持续失败请改用「手动填写凭据」",
		// 旧的兜底码（保留：上游偶发改码表时仍能给出可读提示）
		1001: "手机号格式不正确",
		1002: "验证码错误或已过期",
		1003: "验证码发送过于频繁，请稍后再试",
		1004: "该手机号今日验证码次数已达上限",
		429:  "请求过于频繁，请稍后再试",
	}[code]
	if base == "" {
		base = fmt.Sprintf("登录失败（上游 code %d）", code)
	}
	if upstreamMsg != "" && code != 400001 {
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

/* ── 余额（钱包）────────────────────────────────────────────── */

// Wallet 钱包余额快照。
type Wallet struct {
	// TotalBalance 总积分（reward + daily 等聚合）。
	TotalBalance int64
	// Reward 奖励积分（签到 / 活动领取的）。
	Reward int64
	// Daily 每日活跃额度（当日有效，次日清零）。
	Daily int64
}

// FetchBalance 查询账号钱包余额。
//
// 端点 `GET {userapi}/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw`，
// 响应 data.total_balance 为聚合总额，data.wallets[] 按 public_wallet_type 分类。
//
// ⚠️ 实测（2026-10-03）：对 assetmgr 域直接用存量 token 的签名头会回
// `code 410000 用户未登录`，**先走一次 Refresh 换新 token 再查就通**——
// 调用方（extstore.ViewOne）已在临期时刷新，但 assetmgr 似乎还要求 token
// 是「新鲜签出」的；故这里 410000 时自动刷新一次重试。
func FetchBalance(ctx context.Context, cred *Credential) (*Wallet, error) {
	region := ParseRegion(string(cred.Region))
	fetch := func(token string) (json.RawMessage, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			region.UserAPI()+"/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw", nil)
		if err != nil {
			return nil, 0, err
		}
		for k, v := range userAPIHeaders(token) {
			req.Header.Set(k, v)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return raw, resp.StatusCode, nil
	}

	raw, status, err := fetch(cred.Token)
	if err != nil {
		return nil, fmt.Errorf("钱包查询失败：%w", err)
	}
	var probe struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	_ = json.Unmarshal(raw, &probe)
	// 410000 用户未登录：token 对 assetmgr 不新鲜 → 刷新一次重试（单次，不递归）。
	if probe.Code == 410000 && cred.CanRefresh() {
		if fresh, rerr := Refresh(ctx, cred); rerr == nil {
			*cred = *fresh
			if raw, status, err = fetch(cred.Token); err == nil {
				_ = json.Unmarshal(raw, &probe)
			}
		}
	}
	if status != http.StatusOK || len(probe.Data) == 0 {
		return nil, fmt.Errorf("钱包 HTTP %d：%s", status, truncate(string(raw), 160))
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			TotalBalance json.RawMessage `json:"total_balance"`
			Wallets      []struct {
				PublicWalletType string `json:"public_wallet_type"`
				Balance          int64  `json:"balance"`
			} `json:"wallets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("钱包回执解析失败：%w", err)
	}
	if doc.Code != 0 {
		return nil, fmt.Errorf("钱包查询被拒（code %d）：%s", doc.Code, doc.Msg)
	}
	w := &Wallet{}
	_ = json.Unmarshal(doc.Data.TotalBalance, &w.TotalBalance)
	for _, s := range doc.Data.Wallets {
		switch s.PublicWalletType {
		case "reward":
			w.Reward = s.Balance
		case "daily":
			w.Daily = s.Balance
		}
	}
	return w, nil
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
//
// 两条路径：
//  1. **沙箱 relay**（1.18.x 官方客户端路径）：`{userapi 域}/autoclaw-cloud/
//     proxy/{sandboxID}/v1/chat/completions`，走 chatHeaders 签名头。
//  2. **直连代理**（旧路径）：`{userapi 域}/autoclaw-proxy/proxy/autoclaw`。
//
// 策略：先走直连，若上游回 **406**（权限层被拒，JWT power=0 + 模型校验后的
// 闸门）则自动回落到沙箱 relay 再试一次；沙箱也失败才把直连的 406 响应交回
// 调用方（保留原始 body，由 bridge 做归因文案）。
func Chat(ctx context.Context, cred *Credential, model string, body []byte) (*http.Response, error) {
	region := ParseRegion(string(cred.Region))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, region.ChatBase(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range chatHeaders(cred.Token, model) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotAcceptable {
		resp.Body.Close()
		if relay, rerr := ChatViaSandbox(ctx, cred, body, model); rerr == nil && relay != nil {
			if relay.StatusCode >= 200 && relay.StatusCode < 300 {
				return relay, nil
			}
			relay.Body.Close()
		}
	}
	return resp, nil
}

// ChatViaSandbox 走沙箱 relay 的 OpenAI 端点（1.18.x 官方客户端真实路径）。
//
// relay 基址：`{userapi 域}/autoclaw-cloud/proxy/{sandboxID}/v1/chat/completions`，
// 走 chatHeaders（X-Authorization）签名头。上游回 11003 = 该账号无沙箱对话权限
// （与直连 406 同根：power=0），由调用方归因，不再反复重试。
func ChatViaSandbox(ctx context.Context, cred *Credential, body []byte, model string) (*http.Response, error) {
	_, relayBase, err := EnsureSandbox(ctx, cred)
	if err != nil {
		return nil, err
	}
	b, err := ensureModelInBody(body, model)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		relayBase+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	for k, v := range chatHeaders(cred.Token, model) {
		req.Header.Set(k, v)
	}
	// 沙箱 relay 端点认标准 `Authorization` 头（chatHeaders 里是 `X-Authorization`，
	// 实测只带 X- 前缀那版回 11002 "authorization token is required"）——两个都带。
	// 上游 11003「invalid authorization token」= 该 token 无沙箱对话权限（与直连
	// 406 同根：JWT power=0），交给调用方归因，不再反复重试。
	if tok := stripBearer(cred.Token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("沙箱 relay 对话失败：%w", err)
	}
	return resp, nil
}

// StripRoutePrefix 剥路由前缀（zaicoding_ 先于 zai_）。
//
// 导出给桥接：桥接从 `autoclaw:<routeId>` 剥掉 `autoclaw:` 后，
// 还要把 `<routeId>` 拆成 (routeId, bodyModelId) 两个标识。
func StripRoutePrefix(routeID string) string {
	for _, prefix := range []string{"zaicoding_", "zai_"} {
		if rest, ok := strings.CutPrefix(routeID, prefix); ok {
			return rest
		}
	}
	return routeID
}

// bodyModelField 读 body 里的 model 字段（诊断用）。
func bodyModelField(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
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

// Sandbox 沙箱信息（字段与 /agentdr/v2/assistant/sandbox/list 对齐）。
type Sandbox struct {
	SandboxID       string `json:"sandbox_id"`
	SandboxName     string `json:"sandbox_name"`
	SandboxStatus   string `json:"sandbox_status"`
	RuntimeStatus   string `json:"runtime_status"`
	SandboxEndpoint string `json:"sandbox_endpoint"`
	EndTimestamp    int64  `json:"end_timestamp"`
}

// sandboxBaseHost 沙箱 relay 实际服务的主机（按账号 region 归一）。
//
// 上游 sandbox/list 下发的 endpoint 常写国际域（autoglm-api.zhipuai.cn），但 CN
// 账号的沙箱实例挂在 CN 域（autoglm-acceleration-api.zhipuai.cn）——直接拿
// endpoint 域名拼 relay 路径会 404（实测 2026-10-03）。
func sandboxBaseHost(region Region) string {
	if region == RegionIntl {
		return "https://autoglm-api.autoglm.ai"
	}
	return "https://autoglm-acceleration-api.zhipuai.cn"
}

// RelayProxyBase 拼「{host}/autoclaw-cloud/proxy/{sandboxID}」形式的 relay 基址。
// hostOverride 非空时替代 endpoint 里下发的域名（见 sandboxBaseHost 注释）。
func RelayProxyBase(endpoint, sandboxID, hostOverride string) string {
	host := hostOverride
	if host == "" {
		host = endpoint
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i] // 只取 scheme://host 部分
		}
	}
	_ = endpoint // 上游 endpoint 里的 /autoclaw-cloud 路径段与 region 域对齐，直接拼固定后缀
	return host + "/autoclaw-cloud/proxy/" + strings.TrimSpace(sandboxID)
}

// EnsureSandbox 确保账号有沙箱，返回 (sandboxID, relayProxyBase)。
// 缓存命中（Credential 里已有沙箱）直接用；否则 list → 空则 apply → 重 list。
func EnsureSandbox(ctx context.Context, cred *Credential) (string, string, error) {
	now := time.Now().Unix()
	region := ParseRegion(string(cred.Region))
	host := sandboxBaseHost(region)
	if cred.SandboxID != "" {
		cred.SandboxEndpoint = RelayProxyBase(cred.SandboxEndpoint, cred.SandboxID, host)
		return cred.SandboxID, cred.SandboxEndpoint, nil
	}
	list, err := listSandboxes(ctx, region, cred.Token)
	if err != nil {
		return "", "", err
	}
	if len(list) == 0 {
		if err := applySandbox(ctx, region, cred.Token); err != nil {
			return "", "", err
		}
		list, err = listSandboxes(ctx, region, cred.Token)
		if err != nil {
			return "", "", err
		}
	}
	if len(list) == 0 {
		return "", "", fmt.Errorf("autoclaw 无可用沙箱（apply 后仍为空）")
	}
	pick := list[0]
	for _, s := range list {
		expired := s.EndTimestamp != 0 && s.EndTimestamp <= now
		if !expired && (pick.SandboxID == "" || (pick.EndTimestamp != 0 && pick.EndTimestamp <= now)) {
			pick = s
		}
	}
	cred.SandboxID = pick.SandboxID
	cred.SandboxEndpoint = RelayProxyBase(pick.SandboxEndpoint, pick.SandboxID, host)
	cred.SandboxEndTimestamp = pick.EndTimestamp
	if cred.SandboxID == "" {
		return "", "", fmt.Errorf("autoclaw 沙箱回执缺 sandbox_id")
	}
	return cred.SandboxID, cred.SandboxEndpoint, nil
}

// listSandboxes 列出账号沙箱（userapi 域，签名头 + 数据信封）。
func listSandboxes(ctx context.Context, region Region, token string) ([]Sandbox, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		region.UserAPI()+"/agentdr/v2/assistant/sandbox/list", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range userAPIHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取沙箱列表失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("沙箱列表 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	data := env.Data
	if len(data) == 0 {
		data = raw
	}
	var list []Sandbox
	if json.Unmarshal(data, &list) == nil {
		return list, nil
	}
	var wrapped struct {
		SandboxList []Sandbox `json:"sandbox_list"`
	}
	if json.Unmarshal(data, &wrapped) == nil {
		return wrapped.SandboxList, nil
	}
	return nil, fmt.Errorf("沙箱列表解析失败")
}

// applySandbox 申请沙箱（sandbox_name 用官方默认 "default"）。
func applySandbox(ctx context.Context, region Region, token string) error {
	body, _ := json.Marshal(map[string]string{"sandbox_name": "default"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		region.UserAPI()+"/agentdr/v2/assistant/sandbox/apply", bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range userAPIHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("申请沙箱 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// ensureModelInBody 缺 model 字段时补上（调用方 bridge 已写好则原样保留）。
func ensureModelInBody(body []byte, model string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, nil
	}
	if _, ok := doc["model"]; !ok && model != "" {
		doc["model"] = model
	}
	return json.Marshal(doc)
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
