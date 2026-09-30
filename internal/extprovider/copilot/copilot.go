// Package copilot GitHub Copilot 通道（设备流登录 + OpenAI 兼容直连）。
//
// 协议事实（公开实现交叉验证：Alorse/copilot-to-api、ericc-ch/copilot-api、
// StarryKira/copilot2api-go）：
//
//  1. 设备流   POST https://github.com/login/device/code      → user_code + verification_uri
//     POST https://github.com/login/oauth/access_token → GitHub token
//     client_id `Iv1.b507a08c87ecfe98` 是 Copilot 扩展的**公开**应用标识（所有用户相同）。
//  2. 兑换     GET  https://api.github.com/copilot_internal/v2/token
//     authorization: token <github_token> → Copilot token（**约 25 分钟过期**）
//  3. 对话     POST https://api.githubcopilot.com/chat/completions（OpenAI 原生协议，
//     无需翻译）+ Copilot-Integration-Id: vscode-chat
//  4. 模型     GET  https://api.githubcopilot.com/models
//
// 与 Z.AI 通道的关键差异：上游就是 OpenAI 格式，网关只做鉴权与转发，不翻译协议。
package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 上游端点与客户端标识。
const (
	deviceCodeURL  = "https://github.com/login/device/code"
	accessTokenURL = "https://github.com/login/oauth/access_token"
	userURL        = "https://api.github.com/user"
	tokenURL       = "https://api.github.com/copilot_internal/v2/token"
	apiBase        = "https://api.githubcopilot.com"

	// ClientID Copilot 扩展的公开应用标识（硬编码在官方扩展里，非秘密）。
	ClientID = "Iv1.b507a08c87ecfe98"
	// Scope 只需 read:user —— 拿 GitHub token 换 Copilot token 用。
	Scope = "read:user"

	// 设备流头：官方扩展形态（GitHub 按此识别客户端）。
	editorVersion       = "Neovim/0.6.1"
	editorPluginVersion = "copilot.vim/1.16.0"
	deviceUA            = "GithubCopilot/1.155.0"

	// 对话头：vscode-chat 集成标识 + VS Code 形态（模型可用性最好）。
	integrationID  = "vscode-chat"
	chatEditorVer  = "vscode/1.99.3"
	chatPluginVer  = "copilot-chat/0.26.7"
	chatUA         = "GitHubCopilotChat/0.26.7"
	chatAPIVersion = "2025-04-01"
)

var httpClient = &http.Client{Timeout: 0, Transport: newTransport()}

// newTransport 本通道专用传输层（长连接池 + 宽松的响应头超时：Copilot 首字节可能较慢）。
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

// SetProxy 给本通道单独设置代理；空串 = 跟随环境变量 HTTPS_PROXY，再不行直连。
//
// 为什么是「本通道」而不是全局：github.com / api.githubcopilot.com 在部分网络下
// 直连不通（实测国内多数网络 100% 超时，走本地代理 2-3s 稳定成功），但把整条网关
// 的出口都绑到代理进程上太脆——代理一挂，腾讯/讯飞/智谱/阿里全部跟着不可用。
// 只让 GitHub 的流量走代理，其它上游保持直连。
//
// 代理配了不等于代理**开着**：用户关掉代理软件后，所有请求都会死在
// "proxyconnect ... actively refused" 上。所以配了代理的传输层外面再包一层
// 直连兜底（见 fallbackTransport）——代理挂了就直连，别让整条通道瘫掉。
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
		httpClient = &http.Client{Timeout: 0, Transport: &fallbackTransport{
			primary: tr,
			direct:  newTransport(), // Proxy 保持 ProxyFromEnvironment：兜底也要尊重环境变量
		}}
		return nil
	}
	httpClient = &http.Client{Timeout: 0, Transport: tr}
	return nil
}

// fallbackTransport 代理优先、直连兜底。
//
// 只在**代理自身连不上**时兜底（proxyconnect 失败）。代理连得上但上游返回错误、
// 或直连被墙导致的超时，都不在这里兜——那些是真实的上游结果，重试只会更慢。
type fallbackTransport struct {
	primary *http.Transport
	direct  *http.Transport
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.primary.RoundTrip(req)
	if err == nil || !isProxyConnError(err) {
		return resp, err
	}
	// 请求体可能已被 primary 消费掉，用 GetBody 复位。
	// 没有 body 的请求（GET、或空体 POST）本来就没有可复位的，直接重试；
	// 有 body 却拿不到 GetBody 的才放弃——那种重试会发出一个空体请求。
	retry := req.Clone(req.Context())
	if req.Body != nil {
		if req.GetBody == nil {
			return nil, err
		}
		body, berr := req.GetBody()
		if berr != nil {
			return nil, err
		}
		retry.Body = body
	}
	return t.direct.RoundTrip(retry)
}

// isProxyConnError 判断错误是不是「代理进程连不上」。
//
// Go 在连不上代理时会把 OpError.Op 置为 "proxyconnect"，错误文本也以它开头；
// 两者取其一即可命中（不同 Go 版本包装层次不完全一致）。
func isProxyConnError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "proxyconnect" {
		return true
	}
	return strings.Contains(err.Error(), "proxyconnect")
}

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	// GitHubToken 设备流取得的长期令牌（不会自动过期，除非用户撤销授权）。
	GitHubToken string `json:"github_token"`
	// CopilotToken 由 GitHubToken 兑换的短期令牌（约 25 分钟）。
	CopilotToken string `json:"copilot_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 秒
	Login        string `json:"login,omitempty"`      // GitHub 用户名（展示）
	Plan         string `json:"plan,omitempty"`       // 订阅类型（展示）
}

// NeedsRefresh Copilot token 是否临近过期（提前 3 分钟刷新，避免临界失败）。
func (c *Credential) NeedsRefresh() bool {
	if c.CopilotToken == "" || c.ExpiresAt == 0 {
		return true
	}
	return time.Now().Add(3*time.Minute).Unix() >= c.ExpiresAt
}

// DeviceFlow 一次进行中的设备授权。
type DeviceFlow struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	Interval        int // 轮询间隔（秒，GitHub 下发）
	ExpiresIn       int

	// badCodeStreak 连续收到 incorrect_device_code 的次数。
	// GitHub 在设备码刚下发后的一小段窗口内会回 incorrect_device_code（设备码尚未
	// 生效），随后才转为 authorization_pending。实测首次轮询命中该错误，故把它当作
	// 「暂未生效」而非终态；连续超过 badCodeTolerance 次才判定设备码真的无效。
	badCodeStreak int
}

// badCodeTolerance incorrect_device_code 的容忍次数（超过即判定设备码无效）。
const badCodeTolerance = 3

// StartDeviceFlow 申请设备码。
func StartDeviceFlow(ctx context.Context) (*DeviceFlow, error) {
	payload, _ := json.Marshal(map[string]string{"client_id": ClientID, "scope": Scope})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceCodeURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("editor-version", editorVersion)
	req.Header.Set("editor-plugin-version", editorPluginVersion)
	req.Header.Set("user-agent", deviceUA)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("申请设备码失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("申请设备码被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("设备码回执解析失败：%w", err)
	}
	if doc.Error != "" {
		return nil, fmt.Errorf("GitHub 拒绝：%s", doc.Error)
	}
	if doc.DeviceCode == "" || doc.UserCode == "" {
		return nil, fmt.Errorf("设备码回执不完整")
	}
	if doc.Interval <= 0 {
		doc.Interval = 5
	}
	return &DeviceFlow{
		DeviceCode: doc.DeviceCode, UserCode: doc.UserCode,
		VerificationURI: doc.VerificationURI, ExpiresIn: doc.ExpiresIn, Interval: doc.Interval,
	}, nil
}

// PollResult 一次轮询的结果。
type PollResult struct {
	Done  bool
	Cred  *Credential
	Error string // 面向用户的错误（授权被拒 / 设备码过期）
}

// Poll 轮询授权状态；未完成时 Done=false（调用方按 Interval 重试）。
func (f *DeviceFlow) Poll(ctx context.Context) (*PollResult, error) {
	payload, _ := json.Marshal(map[string]string{
		"client_id":   ClientID,
		"device_code": f.DeviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accessTokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("轮询失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var doc struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
		// ErrorDescription 让上游把话说清楚（如 incorrect_device_code 的具体原因），
		// 否则用户只看到一句笼统的 error code 无从排查。
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("轮询回执解析失败：%w", err)
	}

	switch doc.Error {
	case "":
		f.badCodeStreak = 0
		if doc.AccessToken == "" {
			return &PollResult{Done: false}, nil
		}
		cred, err := Exchange(ctx, doc.AccessToken)
		if err != nil {
			// 拿到了 GitHub token 却换不到 Copilot token（无订阅 / token 被撤销）——
			// 这是**终态**，必须把原因告诉用户。当成底层错误返回会让调用方
			// 误判为「还没授权」，用户看着它一直转圈直到超时。
			return &PollResult{Error: err.Error()}, nil
		}
		return &PollResult{Done: true, Cred: cred}, nil
	case "authorization_pending", "slow_down":
		f.badCodeStreak = 0
		return &PollResult{Done: false}, nil
	case "expired_token":
		return &PollResult{Error: "设备码已过期，请重新发起登录"}, nil
	case "access_denied":
		return &PollResult{Error: "授权被拒绝"}, nil
	case "incorrect_device_code":
		// 设备码刚下发时 GitHub 会短暂回这个错（尚未生效）——当作 pending 重试。
		f.badCodeStreak++
		if f.badCodeStreak <= badCodeTolerance {
			return &PollResult{Done: false}, nil
		}
		return &PollResult{Error: "设备码无效，请重新发起登录"}, nil
	default:
		msg := "GitHub 返回：" + doc.Error
		if doc.ErrorDescription != "" {
			msg += "（" + doc.ErrorDescription + "）"
		}
		return &PollResult{Error: msg}, nil
	}
}

// Exchange 用 GitHub token 兑换 Copilot token（并取用户名/订阅类型用于展示）。
func Exchange(ctx context.Context, githubToken string) (*Credential, error) {
	tok, exp, err := fetchCopilotToken(ctx, githubToken)
	if err != nil {
		return nil, err
	}
	cred := &Credential{GitHubToken: githubToken, CopilotToken: tok, ExpiresAt: exp}
	// 用户名与订阅类型是展示信息，失败不影响可用性
	if login, plan, err := fetchUser(ctx, githubToken); err == nil {
		cred.Login, cred.Plan = login, plan
	}
	return cred, nil
}

// Refresh 用已存的 GitHub token 续期 Copilot token。
func Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if strings.TrimSpace(cred.GitHubToken) == "" {
		return nil, fmt.Errorf("缺少 GitHub token，需重新登录")
	}
	tok, exp, err := fetchCopilotToken(ctx, cred.GitHubToken)
	if err != nil {
		return nil, err
	}
	out := *cred
	out.CopilotToken, out.ExpiresAt = tok, exp
	return &out, nil
}

func fetchCopilotToken(ctx context.Context, githubToken string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("authorization", "token "+githubToken)
	req.Header.Set("editor-version", editorVersion)
	req.Header.Set("editor-plugin-version", editorPluginVersion)
	req.Header.Set("user-agent", deviceUA)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("兑换 Copilot token 失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", 0, fmt.Errorf("GitHub token 无效或该账号未订阅 Copilot（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("兑换 Copilot token 被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", 0, fmt.Errorf("token 回执解析失败：%w", err)
	}
	if doc.Token == "" {
		return "", 0, fmt.Errorf("回执不含 Copilot token（该账号可能没有 Copilot 订阅）")
	}
	if doc.ExpiresAt == 0 {
		doc.ExpiresAt = time.Now().Add(25 * time.Minute).Unix()
	}
	return doc.Token, doc.ExpiresAt, nil
}

// fetchUser 取 GitHub 用户名与 Copilot 订阅类型（展示用；失败可忽略）。
func fetchUser(ctx context.Context, githubToken string) (login, plan string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("authorization", "token "+githubToken)
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("user-agent", deviceUA)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Login string `json:"login"`
		Plan  struct {
			Name string `json:"name"`
		} `json:"plan"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&doc); err != nil {
		return "", "", err
	}
	return doc.Login, doc.Plan.Name, nil
}

// Model 上游模型条目。
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Vendor  string `json:"vendor,omitempty"`
	Version string `json:"version,omitempty"`
}

// ListModels 拉取该账号可用的模型（上游已按订阅等级过滤）。
func ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	applyChatHeaders(req, cred)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("模型列表 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Data, nil
}

// Chat 发起对话（流式或非流式由 body 内的 stream 决定），返回原始响应体。
// 上游就是 OpenAI 格式：调用方直接透传即可，无需协议翻译。
func Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyChatHeaders(req, cred)
	return httpClient.Do(req)
}

func applyChatHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("authorization", "Bearer "+cred.CopilotToken)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("Copilot-Integration-Id", integrationID)
	req.Header.Set("editor-version", chatEditorVer)
	req.Header.Set("editor-plugin-version", chatPluginVer)
	req.Header.Set("x-github-api-version", chatAPIVersion)
	req.Header.Set("user-agent", chatUA)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FormatExpiry 展示用：把秒级时间戳格式化成剩余时长。
func FormatExpiry(exp int64) string {
	if exp == 0 {
		return "未知"
	}
	left := time.Until(time.Unix(exp, 0))
	if left <= 0 {
		return "已过期"
	}
	return strconv.Itoa(int(left.Minutes())) + " 分钟后过期"
}
