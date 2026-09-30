package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OAuth CLI 免密登录（协议事实：zcode2api app/oauth.py + settings.py）。
//
// 三段式：
//
//  1. init   POST {zcode.z.ai}/api/v1/oauth/cli/init   Bearer <poll_token>
//     → flow_id + authorize_url（服务端可能下发自己的 poll_token，须改用）
//  2. 用户在浏览器完成登录
//  3. poll   GET  {zcode.z.ai}/api/v1/oauth/cli/poll/{flow_id}  Bearer <poll_token>
//     → access_token（即 Plan 通道的 JWT）
//
// 随后可选地把 access_token 兑换成 API Key（回退通道）：
//
//	POST {api.z.ai}/api/auth/z/login          {"token": <access_token>} → 业务 token
//	GET  /api/biz/customer/getCustomerInfo    → 机构 / 项目
//	GET  /api/biz/v1/organization/{o}/projects/{p}/api_keys → 取或建 zcode-api-key
//	GET  .../api_keys/copy/{key}              → secretKey
//
// 最终 Key 形态为 `<apiKey>.<secretKey>`。
//
// 注意：init/poll 刻意只带 Authorization 与 Content-Type —— 官方 CLI 就是这么发的，
// 额外伪装头会让上游对 OAuth 会话产生异常的设备绑定。
const (
	oauthInitPath   = "/oauth/cli/init"
	oauthPollPath   = "/oauth/cli/poll/"
	apiKeyName      = "zcode-api-key"
	oauthPollPeriod = 3 * time.Second
)

// oauthAPIBase / exchangeOrigin 由端点变量派生（测试可注入 mock 上游）。
func oauthAPIBase() string   { return PlanOrigin + "/api/v1" }
func exchangeOrigin() string { return FallbackOrigin }

// OAuthFlow 一次进行中的授权流程。
type OAuthFlow struct {
	FlowID       string
	AuthorizeURL string
	pollToken    string
}

// StartOAuth 发起 OAuth 流程，返回授权链接。
func StartOAuth(ctx context.Context) (*OAuthFlow, error) {
	pollToken := strings.ReplaceAll(UUID(), "-", "") + strings.ReplaceAll(UUID(), "-", "")
	body, _ := json.Marshal(map[string]string{"provider": "zai"})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthAPIBase()+oauthInitPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "") // 抑制 Go 默认 UA：官方 CLI 只发这两个头

	resp, err := zaiHTTP().Do(req)
	if err != nil {
		return nil, fmt.Errorf("OAuth init 请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OAuth init 被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}

	var doc struct {
		Code int `json:"code"`
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
			PollToken    string `json:"poll_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("OAuth init 回执解析失败：%w", err)
	}
	if doc.Code != 0 {
		return nil, fmt.Errorf("OAuth init 被上游拒绝（code=%d）", doc.Code)
	}
	if doc.Data.FlowID == "" || doc.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("OAuth 流程数据不完整")
	}
	if t := strings.TrimSpace(doc.Data.PollToken); t != "" {
		pollToken = t // 服务端下发的 poll_token 必须沿用
	}
	return &OAuthFlow{FlowID: doc.Data.FlowID, AuthorizeURL: doc.Data.AuthorizeURL, pollToken: pollToken}, nil
}

// OAuthPollResult 一次轮询的结果。
type OAuthPollResult struct {
	Done        bool
	AccessToken string
	Email       string
}

// Poll 轮询授权状态。未完成时 Done=false（调用方按 oauthPollPeriod 重试）。
func (f *OAuthFlow) Poll(ctx context.Context) (*OAuthPollResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthAPIBase()+oauthPollPath+f.FlowID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.pollToken)
	req.Header.Set("User-Agent", "")

	resp, err := zaiHTTP().Do(req)
	if err != nil {
		return nil, fmt.Errorf("OAuth poll 请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		// poll 的 4xx 承载 code=3004（会话过期）——交给上层提示重新发起
		return nil, fmt.Errorf("OAuth poll 失败（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}

	var doc struct {
		Data struct {
			Status      string `json:"status"`
			AccessToken string `json:"access_token"`
			Token       string `json:"token"`
			Email       string `json:"email"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("OAuth poll 回执解析失败：%w", err)
	}
	tok := doc.Data.AccessToken
	if tok == "" {
		tok = doc.Data.Token
	}
	if tok == "" {
		return &OAuthPollResult{Done: false}, nil
	}
	return &OAuthPollResult{Done: true, AccessToken: tok, Email: doc.Data.Email}, nil
}

// ExchangeAPIKey 把 OAuth access_token 兑换成 API Key（回退通道用）。
//
// 链路：登录换业务 token → 取默认机构/项目 → 取或建 `zcode-api-key` → 取 secretKey。
// 任一步失败都只影响回退通道，不影响 Plan 通道（调用方可忽略该错误）。
func ExchangeAPIKey(ctx context.Context, accessToken string) (string, error) {
	// 1) access_token → 业务 token
	var login struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			AccessToken2 string `json:"accessToken"`
		} `json:"data"`
	}
	if err := postJSON(ctx, exchangeOrigin()+"/api/auth/z/login", map[string]string{"token": accessToken}, "", &login); err != nil {
		return "", fmt.Errorf("兑换业务凭证失败：%w", err)
	}
	bizToken := login.Data.AccessToken
	if bizToken == "" {
		bizToken = login.Data.AccessToken2
	}
	if bizToken == "" {
		return "", fmt.Errorf("兑换回执不含业务凭证")
	}

	// 2) 取机构与项目
	var info struct {
		Data struct {
			Organizations []struct {
				OrganizationID   string `json:"organizationId"`
				OrganizationName string `json:"organizationName"`
				Projects         []struct {
					ProjectID   string `json:"projectId"`
					ProjectName string `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if err := getJSON(ctx, exchangeOrigin()+"/api/biz/customer/getCustomerInfo", bizToken, &info); err != nil {
		return "", fmt.Errorf("获取机构信息失败：%w", err)
	}
	if len(info.Data.Organizations) == 0 {
		return "", fmt.Errorf("账号下没有可用机构")
	}
	org := info.Data.Organizations[0]
	for _, o := range info.Data.Organizations {
		if strings.Contains(o.OrganizationName, "默认机构") {
			org = o
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", fmt.Errorf("机构下没有可用项目")
	}
	proj := org.Projects[0]
	for _, p := range org.Projects {
		if strings.Contains(p.ProjectName, "默认项目") {
			proj = p
			break
		}
	}

	// 3) 取或建 API Key
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys",
		exchangeOrigin(), org.OrganizationID, proj.ProjectID)
	var keys struct {
		Data []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if err := getJSON(ctx, keysURL, bizToken, &keys); err != nil {
		return "", fmt.Errorf("获取 API Key 列表失败：%w", err)
	}
	apiKey := ""
	for _, k := range keys.Data {
		if k.Name == apiKeyName {
			apiKey = k.APIKey
			break
		}
	}
	if apiKey == "" {
		var created struct {
			Data struct {
				APIKey string `json:"apiKey"`
			} `json:"data"`
		}
		if err := postJSON(ctx, keysURL, map[string]string{"name": apiKeyName}, bizToken, &created); err != nil {
			return "", fmt.Errorf("创建 API Key 失败：%w", err)
		}
		apiKey = created.Data.APIKey
	}
	if apiKey == "" {
		return "", fmt.Errorf("未能取得 API Key")
	}

	// 4) 取 secretKey，拼成 <apiKey>.<secretKey>
	var copyResp struct {
		Data struct {
			SecretKey string `json:"secretKey"`
		} `json:"data"`
	}
	if err := getJSON(ctx, keysURL+"/copy/"+apiKey, bizToken, &copyResp); err != nil {
		return "", fmt.Errorf("解密 Secret Key 失败：%w", err)
	}
	if copyResp.Data.SecretKey == "" {
		return "", fmt.Errorf("Secret Key 为空")
	}
	return apiKey + "." + copyResp.Data.SecretKey, nil
}

// ---- HTTP 小工具 ----

func postJSON(ctx context.Context, url string, payload any, bearer string, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doJSON(req, out)
}

func getJSON(ctx context.Context, url, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doJSON(req, out)
}

func doJSON(req *http.Request, out any) error {
	resp, err := zaiHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 160))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
