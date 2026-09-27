// Package lobsterai 有道龙虾 LobsterAI 每日签到与积分余额。
//
// 协议对齐 Jet-Hub lobsterai-credits.ts（逆向自 lobsterai2api/sigin.py）：
//
//  0. 版本号（必填参数）：GET {clientVersionApi} → data.value.version
//  1. 查活动槽位  GET  /api/client-activities/slot?placement=…&clientVersion=…
//  2. 查活动上下文 GET  /api/client-activities/{activityCode}/context?configRevision=…
//  3. 签到        POST /api/client-activities/{activityCode}/actions/check_in
//
// 认证是纯 Bearer，无签名。幂等是客户端实现：请求带 idempotencyKey（UUID4），
// 签到前先查 context 的 state.claimedToday 与 actions 是否含 check_in（两步预检都要做）。
package lobsterai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// 端点与常量（对齐 Jet-Hub lobsterai-product.ts / lobsterai-credits.ts）。
const (
	APIBase          = "https://lobsterai-server.youdao.com"
	ClientVersionAPI = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"
	FallbackVersion  = "2026.9.4"
	UserAgent        = "LobsterAI/0.1.0"
	// ClientCapabilities 客户端能力声明（X-LobsterAI-Client-Capabilities 头）。
	ClientCapabilities = "kimi-k3-agentic-v1,thinking-level-control-v1"

	slotPlacement        = "desktop_sidebar"
	slotContainerVersion = "2"
	slotPlatform         = "win32"
	requestTimeout       = 30 * time.Second
)

// Credential 持久化的 LobsterAI 凭据。
//
// refresh 请求体必须带 firstKeyfrom / latestKeyfrom / uuid（登录时生成、
// 随凭据持久化，丢了续期失败只能重新登录）。
type Credential struct {
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	ExpiresAt     int64  `json:"expires_at,omitempty"` // 毫秒时间戳
	UID           string `json:"uid,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
	UUID          string `json:"uuid"`
	FirstKeyfrom  string `json:"first_key_from"`
	LatestKeyfrom string `json:"latest_key_from"`
}

// Client LobsterAI 签到/余额客户端。
type Client struct {
	HTTP *http.Client
}

// New 创建客户端。
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: requestTimeout}}
}

// envelope 统一信封 {code, msg, data}，code!==0 即失败；chat 端点例外（裸 SSE，本包不涉及）。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// getJSON 发一次 GET 并拆信封。
func (c *Client) getJSON(ctx context.Context, url, token string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuthHeaders(req, token)
	return c.do(req)
}

// postJSON 发一次 POST 并拆信封。
func (c *Client) postJSON(ctx context.Context, url, token string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.setAuthHeaders(req, token)
	return c.do(req)
}

func (c *Client) setAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("X-LobsterAI-Client-Capabilities", ClientCapabilities)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (c *Client) do(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("响应不是 JSON（HTTP %d）: %w", resp.StatusCode, err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("LobsterAI 错误: %s (code=%d)", env.Msg, env.Code)
	}
	return env.Data, nil
}

// FetchClientVersion 拉取当前客户端版本号（签到必带；拉不到用兜底值）。
func (c *Client) FetchClientVersion(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ClientVersionAPI, nil)
	if err != nil {
		return FallbackVersion
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return FallbackVersion
	}
	defer resp.Body.Close()
	var raw struct {
		Data struct {
			Value struct {
				Version string `json:"version"`
			} `json:"value"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&raw) != nil || raw.Data.Value.Version == "" {
		return FallbackVersion
	}
	return raw.Data.Value.Version
}

// activitySlot 槽位查询结果。
type activitySlot struct {
	SlotState      string `json:"slotState"`
	ActivityCode   string `json:"activityCode"`
	ConfigRevision int64  `json:"configRevision"`
}

// activityContext 上下文查询结果。
type activityContext struct {
	ClaimedToday bool     `json:"claimedToday"`
	Actions      []string `json:"actions"`
}

// CheckinResult 签到结果。
type CheckinResult struct {
	Kind    string // "claimed" | "already-claimed" | "inactive" | "failed"
	Credit  float64
	Message string
}

// CheckinDaily 完整三步签到流程。
//
// 判定顺序：槽位失败→failed；slotState!=available→inactive；上下文失败→failed；
// claimedToday→already-claimed；无 check_in 动作→inactive；请求失败→failed；成功→claimed。
func (c *Client) CheckinDaily(ctx context.Context, cred *Credential) *CheckinResult {
	version := c.FetchClientVersion(ctx)

	// 1) 槽位查询
	slotURL := fmt.Sprintf("%s/api/client-activities/slot?placement=%s&containerApiVersion=%s&platform=%s&clientVersion=%s",
		APIBase, slotPlacement, slotContainerVersion, slotPlatform, version)
	slotData, err := c.getJSON(ctx, slotURL, cred.AccessToken)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: "活动槽位查询失败: " + err.Error()}
	}
	var slot activitySlot
	if err := json.Unmarshal(slotData, &slot); err != nil {
		return &CheckinResult{Kind: "failed", Message: "槽位响应解析失败: " + err.Error()}
	}
	if slot.SlotState != "available" || slot.ActivityCode == "" {
		return &CheckinResult{Kind: "inactive", Message: fmt.Sprintf("无可用活动（slotState=%s）", slot.SlotState)}
	}

	// 2) 上下文查询
	ctxURL := fmt.Sprintf("%s/api/client-activities/%s/context?configRevision=%d",
		APIBase, slot.ActivityCode, slot.ConfigRevision)
	ctxData, err := c.getJSON(ctx, ctxURL, cred.AccessToken)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: "活动上下文查询失败: " + err.Error()}
	}
	var actx activityContext
	if err := json.Unmarshal(ctxData, &actx); err != nil {
		return &CheckinResult{Kind: "failed", Message: "上下文响应解析失败: " + err.Error()}
	}
	if actx.ClaimedToday {
		return &CheckinResult{Kind: "already-claimed", Message: "今天已签到"}
	}
	hasCheckin := false
	for _, a := range actx.Actions {
		if a == "check_in" {
			hasCheckin = true
			break
		}
	}
	if !hasCheckin {
		return &CheckinResult{Kind: "inactive", Message: "当前不可签到"}
	}

	// 3) 签到（客户端幂等键）
	claimURL := fmt.Sprintf("%s/api/client-activities/%s/actions/check_in", APIBase, slot.ActivityCode)
	claimBody := map[string]any{
		"configRevision": slot.ConfigRevision,
		"idempotencyKey": newUUID(),
		"payload":        map[string]any{},
	}
	claimData, err := c.postJSON(ctx, claimURL, cred.AccessToken, claimBody)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: err.Error()}
	}
	// 积分字段三级回退：creditsGranted → rewardCredits → credits
	var result struct {
		Result struct {
			CreditsGranted *float64 `json:"creditsGranted"`
			RewardCredits  *float64 `json:"rewardCredits"`
			Credits        *float64 `json:"credits"`
			Message        string   `json:"message"`
		} `json:"result"`
	}
	_ = json.Unmarshal(claimData, &result)
	credit := 0.0
	for _, p := range []*float64{result.Result.CreditsGranted, result.Result.RewardCredits, result.Result.Credits} {
		if p != nil {
			credit = *p
			break
		}
	}
	return &CheckinResult{Kind: "claimed", Credit: credit, Message: result.Result.Message}
}

// CreditPackage 积分包条目。
type CreditPackage struct {
	Name      string  `json:"name"`
	Remaining float64 `json:"remaining"`
	Active    bool    `json:"active"`
	ExpiredAt string  `json:"expired_at,omitempty"`
}

// CreditBalance 余额查询结果。
type CreditBalance struct {
	Total    float64         `json:"total"`
	Packages []CreditPackage `json:"packages"`
}

// FetchBalance 查询积分余额（profile-summary 而非 quota：quota 不含活动积分）。
func (c *Client) FetchBalance(ctx context.Context, cred *Credential) (*CreditBalance, error) {
	data, err := c.getJSON(ctx, APIBase+"/api/user/profile-summary", cred.AccessToken)
	if err != nil {
		return nil, err
	}
	var raw struct {
		TotalCreditsRemaining *float64 `json:"totalCreditsRemaining"`
		CreditItems           []struct {
			Type             string   `json:"type"`
			CreditsRemaining *float64 `json:"creditsRemaining"`
			ExpiresAt        string   `json:"expiresAt"`
		} `json:"creditItems"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("余额响应解析失败: %w", err)
	}
	bal := &CreditBalance{}
	if raw.TotalCreditsRemaining != nil {
		bal.Total = *raw.TotalCreditsRemaining
		if bal.Total < 0 {
			bal.Total = 0
		}
	}
	for _, item := range raw.CreditItems {
		name := item.Type
		if name == "" {
			name = "积分包"
		}
		pkg := CreditPackage{Name: name, Active: true, ExpiredAt: item.ExpiresAt}
		if item.CreditsRemaining != nil {
			pkg.Remaining = *item.CreditsRemaining
		}
		if item.ExpiresAt != "" {
			if t, err := time.Parse("2006-01-02 15:04:05", item.ExpiresAt); err == nil && time.Now().After(t) {
				pkg.Active = false
			}
		}
		bal.Packages = append(bal.Packages, pkg)
	}
	return bal, nil
}
