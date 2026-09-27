// Package qoder 阿里 Qoder 每日积分（campaign claim）。
//
// 协议对齐 Jet-Hub qoder*.ts（逆向自官方客户端 app.asar，0.3.4）：
//
//	登录    PKCE 设备码轮询（authBase/device/selectAccounts → openApiBase/deviceToken/poll）
//	活动    GET  {openApiBase}/sash/api/v1/me/campaigns
//	领取    POST {openApiBase}/sash/api/v1/me/campaigns/{id}/claim（请求体必须空串）
//	用量    GET  {openApiBase}/sash/api/v2/me/usage
//
// 关键协议事实（Jet-Hub 抓包+消融实验实证）：
//   - /sash/ 端点必须带 Cosy-ClientType: 10（桌面 app 身份），用 5（CLI）恒拿不到可领活动
//   - 还必须带 Cosy-MachineToken + Cosy-MachineType 成对头 —— 值来自本机
//     Qoder 客户端的 %APPDATA%\Qoder\SharedClientCache\cache\machine_token.json
//     （{token, type}）。缺任一头服务端不下发 CLAIM_BENEFIT 活动。
//     纯插件登录的账号没有该文件 → 照常发请求（只是拿不到可领项），保守降级。
//   - claim 幂等判据是响应体 replayed:true（不是 HTTP 状态码）
//   - 「今天已领」的判据：存在 CLAIM_BENEFIT 且 claimStatus=CLAIMED 的活动，
//     且当前无可领项。「列表为空」不等于「已领」（可能未到 10:00 刷新点）
package qoder

import (
	"bytes"
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
	"time"
)

const (
	AuthBase        = "https://qoder.com"
	OpenAPIBase     = "https://openapi.qoder.sh"
	ClientID        = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb" // prod（国际版）
	SashClientType  = "10"
	RequestTimeout  = 30 * time.Second
	campaignsPath   = "/sash/api/v1/me/campaigns"
	usagePath       = "/sash/api/v2/me/usage"
	deviceSelect    = "/device/selectAccounts"
	deviceTokenPoll = "/api/v1/deviceToken/poll"
)

// Credential Qoder 凭据。security_oauth_token 与 access_token 双写同值
// （服务端取用顺序 security_oauth_token ?? access_token，双写兼容两种路径）。
// machine_id 必须持久化：续期请求体需要它，且参与服务端设备绑定。
type Credential struct {
	SecurityOAuthToken string `json:"security_oauth_token"`
	AccessToken        string `json:"access_token"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	ExpireTime         int64  `json:"expire_time,omitempty"` // 毫秒
	RefreshExpireTime  int64  `json:"refresh_token_expire_time,omitempty"`
	MachineID          string `json:"machine_id"`
	UID                string `json:"uid,omitempty"`
	Nickname           string `json:"nickname,omitempty"`
}

// Bearer 取请求用 token。
func (c *Credential) Bearer() string {
	if c.SecurityOAuthToken != "" {
		return c.SecurityOAuthToken
	}
	return c.AccessToken
}

// Client Qoder 积分客户端。
type Client struct {
	HTTP *http.Client
}

// New 创建客户端。
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: RequestTimeout}}
}

// MachineIdentity 设备身份（Cosy-MachineToken / Cosy-MachineType 成对）。
type MachineIdentity struct {
	Token string
	Type  string
}

// FindMachineIdentity 从本机 Qoder 客户端读 machine_token.json。
// 找不到返回 nil（调用方照常发请求，只是服务端不下发可领活动）。
//
// 实测该文件即使很旧 token 依然有效，故不做时效校验。
// 候选路径覆盖国际版/国内版两个客户端。
func FindMachineIdentity() *MachineIdentity {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil
	}
	candidates := []string{
		filepath.Join(appData, "Qoder", "SharedClientCache", "cache", "machine_token.json"),
		filepath.Join(appData, "Qoder CN", "SharedClientCache", "cache", "machine_token.json"),
		filepath.Join(appData, "com.qoder.app.stable", "SharedClientCache", "cache", "machine_token.json"),
		filepath.Join(appData, "com.qodercn.app.stable", "SharedClientCache", "cache", "machine_token.json"),
	}
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var parsed struct {
			Token string `json:"token"`
			Type  string `json:"type"`
		}
		if json.Unmarshal(raw, &parsed) != nil || parsed.Token == "" || parsed.Type == "" {
			continue
		}
		return &MachineIdentity{Token: parsed.Token, Type: parsed.Type}
	}
	return nil
}

// sashHeaders /sash/ 端点公共请求头。
// Cosy-ClientType=10 与 MachineToken/MachineType 成对头都是必要条件
// （缺一服务端不下发 CLAIM_BENEFIT 活动），machine 头缺失时保守降级照发。
func (c *Client) sashHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.Bearer())
	req.Header.Set("Cosy-ClientType", SashClientType)
	req.Header.Set("User-Agent", "Qoder")
	if m := FindMachineIdentity(); m != nil {
		req.Header.Set("Cosy-MachineToken", m.Token)
		req.Header.Set("Cosy-MachineType", m.Type)
	}
}

func (c *Client) doSash(ctx context.Context, method, path string, cred *Credential, body string) ([]byte, error) {
	var reader io.Reader
	if body != "" || method == http.MethodPost {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, OpenAPIBase+path, reader)
	if err != nil {
		return nil, err
	}
	c.sashHeaders(req, cred)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	return raw, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// Campaign 活动条目。
type Campaign struct {
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`  // CLAIM_BENEFIT | VIEW_DETAILS
	ClaimStatus string `json:"claimStatus"` // CLAIMABLE | CLAIMED
	Amount      int    `json:"amount"`
	Benefit     struct {
		Amount int `json:"amount"`
	} `json:"benefit"`
}

type campaignsResp struct {
	ShowCampaign bool       `json:"showCampaign"`
	Campaigns    []Campaign `json:"campaigns"`
}

// CheckinStatus 签到状态。
type CheckinStatus struct {
	TodayCheckedIn bool
	ClaimableID    string // 可领取活动的 campaignId（空 = 当前无可领）
	DailyCredit    int
	ActionRequired bool // 账号未开通（需去官方客户端登录）
}

// GetCheckinStatus 查询活动列表并解析签到状态。
//
// 判据（抓包实证）：
//   - todayCheckedIn = 存在 CLAIM_BENEFIT+CLAIMED 且当前无可领项
//   - 「列表为空 / 仅 VIEW_DETAILS」一律判未领（可能是未到刷新点/头不完整/无活动）
func (c *Client) GetCheckinStatus(ctx context.Context, cred *Credential) (*CheckinStatus, error) {
	raw, err := c.doSash(ctx, http.MethodGet, campaignsPath, cred, "")
	if err != nil {
		return nil, err
	}
	var parsed campaignsResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("活动响应解析失败: %w", err)
	}
	st := &CheckinStatus{}
	var claimedBenefit, claimable int
	for _, cp := range parsed.Campaigns {
		if cp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if cp.ClaimStatus == "CLAIMED" {
			claimedBenefit++
			continue
		}
		if cp.ClaimStatus == "CLAIMABLE" {
			claimable++
			st.ClaimableID = cp.CampaignKey
			st.DailyCredit = cp.Benefit.Amount
			if st.DailyCredit == 0 {
				st.DailyCredit = cp.Amount
			}
		}
	}
	st.TodayCheckedIn = claimedBenefit > 0 && claimable == 0
	return st, nil
}

// ClaimResult 领取结果。
type ClaimResult struct {
	Kind    string // "claimed" | "already-claimed" | "failed"
	Credit  int
	Message string
}

// ClaimDaily 领取每日积分。
//
// 幂等判据是响应体 replayed:true（重复领取同样 HTTP 200），不是状态码。
// 请求体必须是空串（抓包实测 content-length: 0）。
func (c *Client) ClaimDaily(ctx context.Context, cred *Credential) (*ClaimResult, error) {
	st, err := c.GetCheckinStatus(ctx, cred)
	if err != nil {
		return nil, err
	}
	if st.TodayCheckedIn {
		return &ClaimResult{Kind: "already-claimed", Message: "今天已领取"}, nil
	}
	if st.ClaimableID == "" {
		return &ClaimResult{Kind: "failed", Message: "当前没有可领取的活动（可能未到每日 10:00 刷新点，或账号未开通/缺设备头）"}, nil
	}
	claimPath := fmt.Sprintf("%s/%s/claim", campaignsPath, st.ClaimableID)
	raw, err := c.doSash(ctx, http.MethodPost, claimPath, cred, "")
	if err != nil {
		return nil, err
	}
	var result struct {
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
		Amount   int    `json:"amount"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("领取响应解析失败: %w", err)
	}
	if result.Replayed {
		return &ClaimResult{Kind: "already-claimed", Message: "今天已领取"}, nil
	}
	if result.Status != "" && result.Status != "CLAIMED" {
		return &ClaimResult{Kind: "failed", Message: fmt.Sprintf("领取未成功（status=%s）", result.Status)}, nil
	}
	return &ClaimResult{Kind: "claimed", Credit: result.Amount}, nil
}

// Usage 用量信息。
type Usage struct {
	Raw json.RawMessage
}

// FetchUsage 查询用量（原样透出，面板展示用）。
func (c *Client) FetchUsage(ctx context.Context, cred *Credential) (json.RawMessage, error) {
	return c.doSash(ctx, http.MethodGet, usagePath, cred, "")
}

// ── PKCE 设备码登录 ──

const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// DeviceSession 一次设备登录会话。
type DeviceSession struct {
	Verifier  string
	Challenge string
	Nonce     string
	MachineID string
}

// NewDeviceSession 生成设备登录会话（PKCE + nonce + machineId）。
func NewDeviceSession(machineID string) (*DeviceSession, error) {
	length := 43 + cryptoRandInt(87)
	vb := make([]byte, length)
	for i := range vb {
		n, err := rand.Int(rand.Reader, bigLen)
		if err != nil {
			return nil, err
		}
		vb[i] = pkceAlphabet[n.Int64()]
	}
	verifier := string(vb)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	nonce, err := randomUUID()
	if err != nil {
		return nil, err
	}
	if machineID == "" {
		machineID, err = randomUUID()
		if err != nil {
			return nil, err
		}
	}
	return &DeviceSession{Verifier: verifier, Challenge: challenge, Nonce: nonce, MachineID: machineID}, nil
}

// BuildAuthUrl 构造浏览器授权 URL。
func (s *DeviceSession) BuildAuthUrl() string {
	return fmt.Sprintf("%s%s?challenge=%s&challenge_method=S256&nonce=%s&machine_id=%s&client_id=%s",
		AuthBase, deviceSelect, s.Challenge, s.Nonce, s.MachineID, ClientID)
}

// Poll 轮询一次取 token（挂 openApiBase，不是 authBase —— 写错 host 永远 401）。
// 404 = 无待授权会话（继续轮询）；200 = 拿到 token。
func (c *Client) Poll(ctx context.Context, s *DeviceSession) (*Credential, error) {
	url := fmt.Sprintf("%s%s?nonce=%s&verifier=%s&challenge_method=S256",
		OpenAPIBase, deviceTokenPoll, s.Nonce, s.Verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 尚未授权
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poll HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		AccessToken        string `json:"access_token"`
		SecurityOAuthToken string `json:"security_oauth_token"`
		RefreshToken       string `json:"refresh_token"`
		ExpireTime         int64  `json:"expire_time"`
		RefreshExpireTime  int64  `json:"refresh_token_expire_time"`
		UserID             string `json:"user_id"`
		UserName           string `json:"user_name"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("poll 响应解析失败: %w", err)
	}
	token := parsed.AccessToken
	if token == "" {
		return nil, nil // 未完成
	}
	cred := &Credential{
		AccessToken:        token,
		SecurityOAuthToken: token, // 双写同值
		RefreshToken:       parsed.RefreshToken,
		ExpireTime:         parsed.ExpireTime,
		RefreshExpireTime:  parsed.RefreshExpireTime,
		MachineID:          s.MachineID,
		UID:                parsed.UserID,
		Nickname:           parsed.UserName,
	}
	return cred, nil
}
