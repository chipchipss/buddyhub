// Package marvis 腾讯 Marvis（马维斯，操作系统级 AI 助手）通道。
//
// 协议事实来自 marvis2api（Zhengyuuuui/marvis2api，含完整逆向报告
// FINDINGS.md，实测证据链齐全）：
//
//	POST https://yybadaccess.3g.qq.com/v1/chat/completions
//	    **上游本身就是标准 OpenAI 协议**（SSE / tool_calls / 图片），网关直通
//
// ── 鉴权：一组 Ual-Access-* 头，无签名、无验证码 ──────────────────
//
//	Ual-Access-Openid / Ual-Access-Access-Token（mv_ token，硬凭据）/
//	Ual-Access-Login-Type / Ual-Access-Guid / Ual-Access-Requestid /
//	Ual-Access-MarvisExt（JSON：qimei36 / guid / uskey / nonce / reqScene /
//	                     taskType / clientVersion / clientPlatform / …）
//
// ── uskey 的实测性质（FINDINGS.md Phase 2/3，8/8 + 30/30 全 200）────
//
//	服务端只校验「**每请求唯一**」（新鲜度），**当前不校验内容**——
//	随机值亦可通过。因此本包在 Windows 上不需要 macOS 的 beacon dylib：
//	每请求生成 `CiDVDCER` 前缀 + 240 字节随机 base64（总长 992B，与真实
//	抓包同构）即可。
//
//	⚠️ 这是**有时效的窗口**：上游一旦加严 uskey 内容校验（参考实现风险 #1，
//	定级“高”），随机模式立即失效。届时的正确形状是 992B 的 beacon 指纹
//	（头部设备成分确定性 + 尾部随机）——参考实现的 provider 架构已为此预留。
//
// ── 服务端实际校验的三层（同 FINDINGS）────────────────────────────
//
//  1. access_token   硬凭据；失效/锁定 → 401 {4100403}
//
//  2. uskey 新鲜度    每请求唯一即可
//
//  3. 请求节奏        账号级自适应风控；密集突发 → 403 {4100404}，会自动冷却
//
//     ⚠️ 因此本通道**严禁压测**——风控按账号计，触发即冷却。
package marvis

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// BaseURL Marvis 推理端点（标准 OpenAI 协议）。
	BaseURL = "https://yybadaccess.3g.qq.com"
	// chatPath 对话路径。
	chatPath = "/v1/chat/completions"
)

// Credential 落进 extstore 的凭据形态（全部从已登录客户端抓包获取）。
type Credential struct {
	// AccessToken mv_ 开头的硬凭据（抓包 Ual-Access-Access-Token 头）。
	AccessToken string `json:"access_token"`
	// Openid 抓包 Ual-Access-Openid 头。
	OpenID string `json:"openid"`
	// LoginType 登录类型（抓包 Ual-Access-Login-Type 头，如 "6"）。
	LoginType string `json:"login_type,omitempty"`
	// DeviceGuid 设备 GUID（抓包 Ual-Access-Guid / X-Device-GUID 头；
	// 也是 MarvisExt 里的 qimei36）。
	DeviceGuid string `json:"device_guid"`
	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`
}

// clientFingerprint 客户端指纹常量（照抄参考实现 config.example.toml 的默认档）。
const (
	clientVersion     = "1.0.0.10371"
	clientPlatform    = "Mac"
	clientPlatformVer = "1.0.0.10634"
	osVersion         = "macOS-15.6.1-arm64-arm-64bit"
)

// genUskey 生成一个**每请求唯一**的 uskey。
//
// 实测（FINDINGS Phase 3）：服务端不校验内容，只要求每请求唯一；
// 真实形状是 `CiDVDCER` 前缀 + base64（头部设备成分确定性、尾部随机）。
// 随机版复刻同样的前缀与长度——过不了内容校验的那天再换成真指纹。
func genUskey() (string, error) {
	// 992B 总长 = "CiDVDCER "(9) + base64(983)。
	// 983 字节 base64 ≈ 737 字节原始数据（base64 每字符 3/4 字节）。
	tail := make([]byte, 737)
	if _, err := rand.Read(tail); err != nil {
		return "", err
	}
	enc := base64.StdEncoding.EncodeToString(tail)
	// 精确截到 983 字符（base64 编码 737B 会产出 984 字符，去 padding）
	enc = strings.TrimRight(enc, "=")
	if len(enc) > 983 {
		enc = enc[:983]
	}
	return "CiDVDCER " + enc, nil
}

// httpClient 共享客户端。
var httpClient = &http.Client{Timeout: 0, Transport: newTransport()}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 600 * time.Second, // 长回答
	}
}

// SetHTTPClient 替换包级 HTTP 客户端（nil 忽略）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		httpClient = c
	}
}

// UpstreamError 非 2xx 的上游回执。Status 供桥接分类（401 换号 / 403 风控冷却）。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("上游返回 %d: %s", e.Status, e.Body)
}

// buildHeaders 构造一次请求的完整鉴权头（uskey 每请求新生成）。
func buildHeaders(cred *Credential) (map[string]string, error) {
	uskey, err := genUskey()
	if err != nil {
		return nil, err
	}
	nonce := newNonce()
	rid := "resp_" + newNonce()
	ext := map[string]any{
		"qimei36":               cred.DeviceGuid,
		"osVersion":             osVersion,
		"guid":                  cred.DeviceGuid,
		"uskey":                 uskey,
		"nonce":                 nonce,
		"reqScene":              0,
		"taskType":              "user",
		"clientVersion":         clientVersion,
		"clientPlatform":        clientPlatform,
		"clientPlatformVersion": clientPlatformVer,
		"isIoa":                 false,
	}
	extJSON, _ := json.Marshal(ext)
	return map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		// 官方客户端是 openai python SDK 的形态（抓包 UA 原样）
		"User-Agent":                  "AsyncOpenAI/Python 2.32.0",
		"X-Stainless-Lang":            "python",
		"X-Stainless-Package-Version": "2.32.0",
		"X-Stainless-OS":              "MacOS",
		"X-Stainless-Arch":            "arm64",
		"X-Stainless-Runtime":         "CPython",
		"X-Stainless-Runtime-Version": "3.11.9",
		"X-Stainless-Async":           "async:asyncio",
		"X-Stainless-Raw-Response":    "true",
		"x-stainless-retry-count":     "0",
		"x-stainless-read-timeout":    "600.0",
		"Accept-Encoding":             "gzip, deflate, br, zstd",
		"Authorization":               "Bearer placeholder", // 占位：真实鉴权走 Ual-Access-*
		"Ual-Access-Openid":           cred.OpenID,
		"Ual-Access-Access-Token":     cred.AccessToken,
		"Ual-Access-Login-Type":       cred.LoginType,
		"Ual-Access-Guid":             cred.DeviceGuid,
		"X-Device-GUID":               cred.DeviceGuid,
		"Ual-Access-MarvisExt":        string(extJSON),
		"Marvis-AgentTag":             "main",
		"Marvis-Scene":                "agent_chat",
		"Marvis-Protocol-Version":     "1000",
		"Marvis-Conversation-ID":      "conv_" + newNonce(),
		"Marvis-Response-ID":          rid,
		"Ual-Access-Requestid":        rid,
	}, nil
}

// Chat 发一次对话，返回上游原始响应（SSE 或 JSON 由 body.stream 决定）。
// 返回的 resp 由调用方 Close。
func Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	if cred == nil || cred.AccessToken == "" || cred.OpenID == "" {
		return nil, fmt.Errorf("Marvis 账号缺少 access_token / openid，无法对话")
	}
	headers, err := buildHeaders(cred)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("对话请求失败：%w", err)
	}
	return resp, nil
}

// Models 静态模型清单（Marvis 不暴露模型注册表——云端返回空；
// 清单来自参考实现协议实测）。
func Models() []Model {
	return []Model{
		{Name: "main-auto", Desc: "Marvis 主对话（服务端路由）"},
		{Name: "default", Desc: "Marvis 默认档"},
		{Name: "ark_deepseek-v4-flash-ga-260731", Desc: "DeepSeek V4 Flash（火山 ARK）"},
		{Name: "hy3", Desc: "腾讯混元 Hy3"},
		{Name: "deepseek-v4-pro-external", Desc: "DeepSeek V4 Pro (external)"},
	}
}

// Model 静态模型条目。
type Model struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

// 上游错误码语义（FINDINGS 实测）。
const (
	// errTokenInvalid 401 {4100403} = token 失效/锁定。
	errTokenInvalid = "4100403"
	// errRateLimited 403 {4100404} = 账号级风控（密集突发），会自动冷却。
	errRateLimited = "4100404"
)

// Classify 按上游错误体归类（供桥接决定换号还是冷却）。
func Classify(status int, body string) string {
	switch {
	case status == 401 && contains(body, errTokenInvalid):
		return "token"
	case status == 403 && contains(body, errRateLimited):
		return "rate"
	case status == 401:
		return "token"
	default:
		return "other"
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
