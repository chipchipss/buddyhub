// Package raccoon 商汤小浣熊 Raccoon Work：余额 + 一次性登录奖励 + 微信扫码登录。
//
// 协议对齐 Jet-Hub raccoon*.ts（逆向自官方桌面端 app.asar）：
//
//	API 基址  https://xiaohuanxiong.com
//	认证前缀  /api/web/auth/v1
//	积分前缀  /api/web/points/v1
//	桌面前缀  /api/web/desktop/v1
//
// 关键事实（来源 Jet-Hub 注释，实测结论）：
//   - 每日 300 积分服务端按日自动发放（daily_grant），无签到端点，不可实现为签到按钮
//   - 桌面端登录奖励 3000 分是一次性幂等端点（POST desktop/v1/login/points/grant，
//     已领过返回 granted:false），需要 X-Client-Platform: desktop-windows 头
//   - 微信扫码登录：code 由客户端本地随机生成（任意自造 code 均被接受），
//     用户扫码后轮询 login_with_qrcode_code 取 token —— 完全绕开官方
//     office-raccoon:// 自定义协议回调
//   - access_token 是 JWT，exp 本地解码即过期时间
package raccoon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	APIBase        = "https://xiaohuanxiong.com"
	AuthPrefix     = "/api/web/auth/v1"
	PointsPrefix   = "/api/web/points/v1"
	DesktopPrefix  = "/api/web/desktop/v1"
	ClientPlatform = "desktop-windows"
	ClientVersion  = "v1.0.35"
	RequestTimeout = 60 * time.Second

	// PhoneCipherSecret 手机号 AES-128-CFB 加密密钥（客户端硬编码公开常量，
	// 仅防明文出现在日志/代理，不是安全边界）。
	PhoneCipherSecret = "senseraccoon2023"
	LoginRewardPoints = 3000
	LoginRewardEvent  = "桌面端登录奖励"
)

// Credential Raccoon 凭据（JWT access_token + refresh_token 轮换）。
type Credential struct {
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	ExpiresAt      int64  `json:"expires_at,omitempty"` // 毫秒
	OfficeIdentity string `json:"office_identity,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	Nickname       string `json:"nickname,omitempty"`
}

// Client Raccoon 客户端。
type Client struct {
	HTTP *http.Client
}

// New 创建客户端。
func New() *Client {
	return &Client{HTTP: httpClient}
}

// httpClient 共享 HTTP 客户端（SetHTTPClient 可替换，测试注入 mock 上游）。
var httpClient = &http.Client{Timeout: RequestTimeout}

// SetHTTPClient 替换包级 HTTP 客户端（nil = 忽略）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		httpClient = c
	}
}

// envelope 业务信封：code===0 成功；失败可能带 HTTP 400/401 也可能 HTTP 200 + 非 0 code。
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Details string          `json:"details"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) headers(req *http.Request, cred *Credential) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if cred != nil {
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		req.Header.Set("X-Org-Code", cred.OfficeIdentity) // 个人账号为空串，但总是发送
	}
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("X-Client-Platform", ClientPlatform)
	req.Header.Set("X-Client-Version", ClientVersion)
}

func (c *Client) do(ctx context.Context, method, url string, cred *Credential, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	c.headers(req, cred)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应不是 JSON（HTTP %d）", resp.StatusCode)
	}
	code := env.Code
	if code == 0 && resp.StatusCode >= 400 {
		code = resp.StatusCode
	}
	if code != 0 {
		msg := strings.TrimSpace(strings.Join([]string{env.Message, env.Details}, ": "))
		if msg == "" {
			msg = fmt.Sprintf("code=%d", code)
		}
		return nil, fmt.Errorf("Raccoon 错误: %s", msg)
	}
	return env.Data, nil
}

// DecodeJWTExpMs 从 JWT 本地解码 exp（毫秒）；解析失败返回 0。
func DecodeJWTExpMs(jwt string) int64 {
	parts := strings.Split(jwt, ".")
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

// GenerateQrCode 生成扫码 code（32 位小写 hex，任意自造值均被服务端接受）。
func GenerateQrCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// DecodeJWTUserID 从 JWT 载荷里取稳定的用户标识（扫码登录回执不含用户信息，
// 账号 ID 只能从 token 里拿）。
//
// 按常见 claim 名依次尝试——不同签发版本字段名不一，逐个探测比写死一个更稳。
// 全部落空时返回空串，调用方自行兜底（例如用 token 摘要），**不要**用随机值：
// 账号 ID 是 extstore 的幂等键，每次都变会让同一账号反复入池。
func DecodeJWTUserID(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, k := range []string{"sub", "uid", "user_id", "userId", "userid", "id", "account_id", "accountId"} {
		switch v := claims[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			// JSON 数字统一解成 float64；整数 id 不带小数点输出
			if v == float64(int64(v)) {
				return strconv.FormatInt(int64(v), 10)
			}
		}
	}
	return ""
}

// TokenDigest 凭据摘要（8 位十六进制），用于 JWT 里取不到用户标识时兜底当账号 ID。
// 同一 token 稳定映射到同一 ID，因此重复登录同一账号仍幂等。
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}

// BuildQrImageUrl 二维码承载的微信登录页 URL。
func BuildQrImageUrl(code string) string {
	return fmt.Sprintf("%s/login/mp?code=%s&appname=%s", APIBase, code, "商汤小浣熊官网")
}

// QrPollResult 扫码轮询结果。任何异常都降级为 pending（轮询 2s 一次，偶发失败不中断）。
type QrPollResult struct {
	Status       string // pending | logging | canceled | success
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// PollQrLogin 轮询一次扫码登录状态。
func (c *Client) PollQrLogin(ctx context.Context, code string) *QrPollResult {
	data, err := c.do(ctx, http.MethodPost, APIBase+AuthPrefix+"/login_with_qrcode_code", nil, map[string]any{"qrcode_code": code})
	if err != nil {
		return &QrPollResult{Status: "pending"}
	}
	var raw struct {
		Status       string `json:"status"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return &QrPollResult{Status: "pending"}
	}
	switch raw.Status {
	case "canceled":
		return &QrPollResult{Status: "canceled"}
	case "logging":
		return &QrPollResult{Status: "logging"}
	case "success":
		if raw.AccessToken == "" {
			return &QrPollResult{Status: "pending"} // 缺 token 的 success 视为未完成
		}
		return &QrPollResult{
			Status:       "success",
			AccessToken:  raw.AccessToken,
			RefreshToken: raw.RefreshToken,
			ExpiresAt:    DecodeJWTExpMs(raw.AccessToken),
		}
	}
	return &QrPollResult{Status: "pending"}
}

// CheckinResult 登录奖励领取结果（语义上是 onboarding 一次性，不是每日签到）。
type CheckinResult struct {
	Kind    string // "claimed" | "already-claimed" | "failed"
	Credit  int
	Message string
}

// ClaimLoginReward 领取桌面端登录奖励（一次性幂等，已领过返回 already-claimed）。
func (c *Client) ClaimLoginReward(ctx context.Context, cred *Credential) *CheckinResult {
	data, err := c.do(ctx, http.MethodPost, APIBase+DesktopPrefix+"/login/points/grant", cred, nil)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: err.Error()}
	}
	var raw struct {
		Granted bool `json:"granted"`
		Popup   struct {
			Points int `json:"points"`
		} `json:"popup"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return &CheckinResult{Kind: "failed", Message: "领取响应解析失败"}
	}
	if !raw.Granted {
		return &CheckinResult{Kind: "already-claimed", Message: "该账号已领取过桌面端登录奖励（每号一次）"}
	}
	points := raw.Popup.Points
	if points <= 0 {
		points = LoginRewardPoints
	}
	return &CheckinResult{Kind: "claimed", Credit: points}
}

// CreditPackage 积分池条目（各池分开：注册礼包/每日/会员/充值有效期规则不同）。
type CreditPackage struct {
	Name      string  `json:"name"`
	Remaining float64 `json:"remaining"`
}

// CreditBalance 余额。
type CreditBalance struct {
	Total    float64         `json:"total"`
	Packages []CreditPackage `json:"packages"`
}

// FetchBalance 查询积分余额（GET points/v1/balance 只读）。
func (c *Client) FetchBalance(ctx context.Context, cred *Credential) (*CreditBalance, error) {
	data, err := c.do(ctx, http.MethodGet, APIBase+PointsPrefix+"/balance", cred, nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		AvailablePoints *float64 `json:"available_points"`
		RewardPoints    *float64 `json:"reward_points"`
		DailyPoints     *float64 `json:"daily_points"`
		TopupPoints     *float64 `json:"topup_points"`
		MonthlyPoints   *float64 `json:"monthly_points"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("余额响应解析失败: %w", err)
	}
	if raw.AvailablePoints == nil {
		return nil, fmt.Errorf("余额响应缺少 available_points")
	}
	bal := &CreditBalance{Total: *raw.AvailablePoints}
	if raw.RewardPoints != nil {
		bal.Packages = append(bal.Packages, CreditPackage{"奖励积分", *raw.RewardPoints})
	}
	if raw.DailyPoints != nil {
		bal.Packages = append(bal.Packages, CreditPackage{"每日积分", *raw.DailyPoints})
	}
	if raw.MonthlyPoints != nil && *raw.MonthlyPoints > 0 {
		bal.Packages = append(bal.Packages, CreditPackage{"会员积分", *raw.MonthlyPoints})
	}
	if raw.TopupPoints != nil {
		bal.Packages = append(bal.Packages, CreditPackage{"充值积分", *raw.TopupPoints})
	}
	if len(bal.Packages) == 0 {
		bal.Packages = []CreditPackage{{"可用积分", bal.Total}}
	}
	return bal, nil
}

// Refresh 续期 access_token（refresh_token 轮换）。
//
// 路径是 `/refresh`，**不是** `/refresh_token` —— 实测后者恒 404（网关日志里
// 连续 40 小时 `续期失败: 响应不是 JSON（HTTP 404）`，账号 token 一到期就再也
// 续不上）。前者对假 token 回 `400 param payload iss invalid`，说明路由与
// 字段名 `refresh_token` 都是对的。
func (c *Client) Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if cred.RefreshToken == "" {
		return nil, fmt.Errorf("无 refresh_token，需重新登录")
	}
	data, err := c.do(ctx, http.MethodPost, APIBase+AuthPrefix+"/refresh", nil, map[string]any{
		"refresh_token": cred.RefreshToken,
	})
	if err != nil {
		return nil, err
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("续期响应解析失败: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("续期响应缺少 access_token")
	}
	out := *cred
	out.AccessToken = raw.AccessToken
	if raw.RefreshToken != "" {
		out.RefreshToken = raw.RefreshToken
	}
	if exp := DecodeJWTExpMs(raw.AccessToken); exp > 0 {
		out.ExpiresAt = exp
	}
	return &out, nil
}

// IsExpired 凭据是否已过期（提前 5 分钟窗口，对齐官方 scheduleAuth.js 的 300s）。
func (cred *Credential) IsExpired() bool {
	if cred.ExpiresAt <= 0 {
		return false
	}
	return time.Now().UnixMilli() >= cred.ExpiresAt-5*60*1000
}
