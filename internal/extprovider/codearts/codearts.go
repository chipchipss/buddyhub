// Package codearts 华为云 CodeArts 每日签到积分。
//
// 协议对齐 Jet-Hub codearts-credits.ts（逆向自本机码道 IDE，非猜测）：
//
//	账户/套餐  GET  {snapEngineUrl}/snap-manager/v1/statistics/plugin
//	活动列表   GET  {snapEngineUrl}/v1/ops/delivery?channel=IDE
//	领取       POST {snapEngineUrl}/v1/ops/claim   { campaignId, channel: 'IDE' }
//	领取确认   POST {snapEngineUrl}/v1/ops/confirm { campaignId }
//
// snapEngineUrl = https://snap-access.cn-north-4.myhuaweicloud.com
//
// 认证：SDK-HMAC-SHA256 签名（AK/SK/securityToken），不是 Cookie。
// 关键陷阱（Jet-Hub 实测）：Agent-Type / X-Language 头必须在**签名之后**追加，
// 一旦进入 canonical request 服务端回 401 verify ak sk signature fail。
//
// 幂等：靠活动列表的 claimable / status 预检（CLAIMED/CONFIRMED/CONSUMED = 已领）。
// 领取后若响应带 id（benefit.id !== null），必须补一次 confirm，漏掉积分停在
// 「待确认」不入账。
package codearts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	SnapEngineURL   = "https://snap-access.cn-north-4.myhuaweicloud.com"
	packageInfoPath = "/snap-manager/v1/statistics/plugin"
	opsDeliveryPath = "/v1/ops/delivery"
	opsClaimPath    = "/v1/ops/claim"
	opsConfirmPath  = "/v1/ops/confirm"
	opsChannel      = "IDE"
	dailyLoginType  = "USER_LOGIN"
	requestTimeout  = 30 * time.Second
	// chatRequestTimeout 对话的**首字节**上限（不是总时长）：对齐仓库内其它
	// extprovider 的 120s。流已开始后不受它约束，腰斩与否交给调用方 ctx。
	chatRequestTimeout = 120 * time.Second
)

// claimedStatuses 活动 status 中表示已领取/已确认/已核销的取值
// （IDE 的 ActivityWelfarePane.render 在这三种状态下禁用领取按钮）。
var claimedStatuses = map[string]bool{"CLAIMED": true, "CONFIRMED": true, "CONSUMED": true}

// Credential CodeArts 临时凭据（IAM AK/SK + securityToken）。
type Credential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	UserID          string `json:"user_id,omitempty"`
	UserName        string `json:"user_name,omitempty"`
}

// Client CodeArts 积分客户端。
type Client struct {
	HTTP     *http.Client // 短请求（签到/余额/领取确认）：总时长 30s
	ChatHTTP *http.Client // 对话：无总时长，首字节由 ResponseHeaderTimeout 管
}

// httpClient 短请求包级客户端（默认值；SetHTTPClient 可整体替换）。
//
// 新建一个 Client 就换一次 http.Client 会让**连接池失效**（每条请求都重建
// TCP/TLS），且测试没有注入点——与 raccoon/qoder/trae 同一套做法。
var httpClient = &http.Client{Timeout: requestTimeout}

// chatHTTP 对话包级客户端。
//
// **必须 Timeout: 0**：http.Client.Timeout 是整个请求生命周期的上限，包含读
// 响应体。对话返回的是 SSE 流（codearts_bridge 直接透传/聚合），CodeArts 上
// 的长回答——慢模型、长上下文、思维链——会在 30s 处被 transport 硬切，长流
// 全部腰斩。首字节延迟由 Transport.ResponseHeaderTimeout 单独管（值对齐仓库
// 其它 extprovider 的 120s），流中空闲则由调用方的 ctx 兜底。
//
// 与自签短请求共用同一个连接池目标（同域 SnapEngineURL），但刻意保留两个
// 独立客户端：短请求该被 30s 硬切，对话不该——一套超时策略满足不了两种请求。
var chatHTTP = defaultChatClient()

// defaultChatClient 构造对话用的默认客户端（出厂配置）。
//
// 拆成函数而不是内联在 var 里：测试要断言出厂值（Timeout:0 + 首字节上限），
// 而包级 chatHTTP 会被 SetHTTPClient 注入顶掉——直接断言全局变量等于让测试
// 结果取决于别的用例先跑过什么。
func defaultChatClient() *http.Client {
	return &http.Client{Timeout: 0, Transport: defaultChatTransport()}
}

// defaultChatTransport 对话用的默认 transport：唯一一份连接池/超时配置。
// SetChatHTTPClient 在注入缺首字节上限的 transport 时，构造的是同一个函数，
// 保证「对话永不被总时长腰斩」与「首字节有界」这两条契约只有一处定义。
func defaultChatTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: chatRequestTimeout,
	}
}

// New 创建客户端（共享包级 HTTP 客户端与连接池）。
func New() *Client {
	return &Client{HTTP: httpClient, ChatHTTP: chatHTTP}
}

// SetHTTPClient 替换短请求的包级 HTTP 客户端（测试注入 mock 上游；nil 忽略）。
//
// 对**对话**同样生效（测试只有一处注入点，不该为长流再开一个）：
// SetHTTPClient(x) 把 x 的 Transport 搬到 ChatHTTP，测试注入的 mock transport
// 因此覆盖签到与对话两条链路——否则注入只覆盖签到，对话仍打真网。
//
// 刻意**不搬** x.Timeout：对话客户端恒为 Timeout: 0。搬过来的话，任何带
// Timeout 的注入（比如 30s）都会悄悄把 P1-5 修掉的长流腰斩又放回来，而且
// 调用方看不出区别。要给对话单独配总时长请用 SetChatHTTPClient。
//
// Transport 的兜底与 SetChatHTTPClient 同一哲学（omp 终审备注 A）：注入的
// transport 缺首字节上限就 Clone 后补上（nil 则换默认对话 transport）——
// Timeout:0 的对话流首字节必须由 ResponseHeaderTimeout 单独兜底，两条注入
// 路径的契约不能一条有界一条无界。
func SetHTTPClient(c *http.Client) {
	if c == nil {
		return
	}
	httpClient = c
	chatHTTP = normalizeChatClient(&http.Client{
		Timeout:       0, // 恒为 0，不搬 c.Timeout（见 SetHTTPClient 注释）
		Transport:     c.Transport,
		CheckRedirect: c.CheckRedirect,
		Jar:           c.Jar,
	})
}

// SetChatHTTPClient 单独替换对话客户端（宿主想给对话配不同 transport 时用）；
// 传 nil 还原出厂默认（defaultChatClient）——与 SetHTTPClient 的 nil 语义对齐，
// 避免「还原」这条最常用的路径反而退化成无首字节上限的裸客户端。
//
// 两处兜底，都是为了让「对话永不被总时长腰斩」这个契约不会在注入时被悄悄
// 破坏（调用方看不出区别，只会表现为长流偶发断掉）：
//   - c.Timeout 一律归 0（与 SetHTTPClient 同款处理，理由见其注释）；
//   - c.Transport 为 nil 时补 http.DefaultTransport，且**缺失
//     ResponseHeaderTimeout 的 transport 补上 chatRequestTimeout**。
//
// 为什么必须补首字节上限：Timeout: 0 意味着「永不被总时长腰斩」，那首字节
// 就必须由 ResponseHeaderTimeout 单独兜底，否则上游只连上不吐头时对话流
// 会无限挂等（既不报错也不结束，goroutine 和连接一起泄漏）。
func SetChatHTTPClient(c *http.Client) {
	if c == nil {
		chatHTTP = defaultChatClient()
		return
	}
	chatHTTP = normalizeChatClient(c)
}

// normalizeChatClient 把注入的对话 client 归一到两条硬契约上：总时长恒 0、
// 首字节有界。SetHTTPClient 与 SetChatHTTPClient 共用，保证两条注入路径的
// 契约对称（omp 终审备注 A）。
func normalizeChatClient(c *http.Client) *http.Client {
	c.Timeout = 0
	// 自定义 transport 且缺首字节上限时，克隆一份再补——**绝不能就地改**：
	// http.DefaultTransport 是全局共享的，在它身上设 ResponseHeaderTimeout
	// 会影响这个进程里所有用它的请求（别的 provider、别的包）。
	switch tr := c.Transport.(type) {
	case nil:
		c.Transport = defaultChatTransport()
	case *http.Transport:
		if tr.ResponseHeaderTimeout == 0 {
			clone := tr.Clone()
			clone.ResponseHeaderTimeout = chatRequestTimeout
			c.Transport = clone
		}
	}
	return c
}

// signRequestHuawei SDK-HMAC-SHA256 签名（对齐 Jet-Hub sign.ts 与华为 Rust 参考实现）。
// 返回需合并进请求的头映射。
func signRequestHuawei(cred *Credential, method, urlStr, body string) (map[string]string, error) {
	// 解析 URL 的 path/query/host（不引入 net/url 的 query 重排序问题）
	host, uri, query, err := splitURL(urlStr)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	dateStamp := time.Now().UTC().Format("20060102T150405Z")
	payloadHash := sha256Hex(body)

	headers := map[string]string{
		"host":                 host,
		"x-sdk-date":           dateStamp,
		"x-sdk-content-sha256": payloadHash,
	}
	// x-security-token 只在有值时参与签名与发送。
	// 华为云永久 AK/SK 的请求**不带**该头；带上空值会让服务端算出的规范请求
	// 与客户端不一致（401 verify ak sk signature fail）。临时 STS 凭据才需要它。
	//
	// 这对账号池很关键：STS security_token 几小时就过期，放进池里等于每几小时
	// 重填一次；永久 AK/SK 才撑得起无人值守。
	if cred.SecurityToken != "" {
		headers["x-security-token"] = cred.SecurityToken
	}
	if method != http.MethodGet {
		headers["content-type"] = "application/json"
	}

	// canonical request：头名排序 → k:v 换行拼接 → method\nuri\nquery\nheaders\n\nsigned\npayloadHash
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var headerLines strings.Builder
	for i, k := range names {
		if i > 0 {
			headerLines.WriteByte('\n')
		}
		headerLines.WriteString(k)
		headerLines.WriteByte(':')
		headerLines.WriteString(headers[k])
	}
	canonical := strings.Join([]string{
		method, uri, query, headerLines.String(), "", strings.Join(names, ";"), payloadHash,
	}, "\n")
	canonicalHash := sha256Hex(canonical)
	stringToSign := "SDK-HMAC-SHA256\n" + dateStamp + "\n" + canonicalHash
	mac := hmac.New(sha256.New, []byte(cred.SecretAccessKey))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	headers["Authorization"] = fmt.Sprintf("SDK-HMAC-SHA256 Access=%s,SignedHeaders=%s,Signature=%s",
		cred.AccessKeyID, strings.Join(names, ";"), signature)
	return headers, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// splitURL 极简 URL 拆解（host / path / rawquery）。
func splitURL(urlStr string) (host, path, query string, err error) {
	rest, ok := strings.CutPrefix(urlStr, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(urlStr, "http://")
		if !ok {
			return "", "", "", fmt.Errorf("仅支持 http(s) URL")
		}
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest, "/", "", nil
	}
	host = rest[:slash]
	restPath := rest[slash:]
	qmark := strings.Index(restPath, "?")
	if qmark >= 0 {
		path = restPath[:qmark]
		query = restPath[qmark+1:]
	} else {
		path = restPath
	}
	return host, path, query, nil
}

// signedRequest 发一次签名请求。
// snapExtraHeaders（Agent-Type/X-Language）在签名后追加，绝不参与签名计算。
func (c *Client) signedRequest(ctx context.Context, method, url string, cred *Credential, body string) (json.RawMessage, error) {
	signed, err := signRequestHuawei(cred, method, url, body)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range signed {
		req.Header.Set(k, v)
	}
	// 签名后追加（不参与签名）：见包注释的 401 陷阱。
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")

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
		// 保留服务端原因：APIG.0301 等 error_code 处置方式完全不同，不能压成 HTTP 401。
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

// AccountInfo 账户/套餐信息。
type AccountInfo struct {
	IsCreditPackage bool // 积分计费账户（活动参与前提）
	IsTokenPackage  bool // 旧 Token 计费账户
	PackageName     string
	TotalCredits    float64 // 积分余额（非积分账户为 0）
}

// FetchAccountInfo 查询账户信息（含积分账户检测与余额）。
func (c *Client) FetchAccountInfo(ctx context.Context, cred *Credential) (*AccountInfo, error) {
	raw, err := c.signedRequest(ctx, http.MethodGet, SnapEngineURL+packageInfoPath, cred, "")
	if err != nil {
		return nil, err
	}
	var data struct {
		Package struct {
			IsCreditPackage bool   `json:"is_credit_package"`
			IsTokenPackage  bool   `json:"is_token_package"`
			SpecCode        string `json:"spec_code"`
			SpecName        string `json:"spec_name"`
			SpecDisplayName string `json:"spec_display_name"`
		} `json:"package"`
		Metrics []struct {
			Name                string  `json:"name"`
			PackageCreditAmount float64 `json:"package_credit_amount"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("账户信息解析失败: %w", err)
	}
	info := &AccountInfo{
		IsCreditPackage: data.Package.IsCreditPackage,
		IsTokenPackage:  data.Package.IsTokenPackage,
		PackageName:     data.Package.SpecDisplayName,
	}
	if info.PackageName == "" {
		info.PackageName = data.Package.SpecName
	}
	// 总额取 usageTotalPackageCredit（分类是构成明细，相加会重复计算）。
	for _, m := range data.Metrics {
		if m.Name == "usageTotalPackageCredit" {
			info.TotalCredits = m.PackageCreditAmount
			break
		}
	}
	return info, nil
}

// OpsActivity 活动条目。
type OpsActivity struct {
	CampaignID string // ⚠️ 服务端下发的是数字（实测 1），解析时兼容数字/字符串
	Type       string
	Title      string
	Claimable  bool
	Status     string
	Amount     float64 // 字段名是 benefitAmount（实测 1000），不是 amount
}

// FetchActivities 查询活动列表（channel=IDE）。
func (c *Client) FetchActivities(ctx context.Context, cred *Credential) ([]OpsActivity, error) {
	raw, err := c.signedRequest(ctx, http.MethodGet, SnapEngineURL+opsDeliveryPath+"?channel=IDE", cred, "")
	if err != nil {
		return nil, err
	}
	var data struct {
		Activities []struct {
			ID            any      `json:"id"`
			CampaignID    any      `json:"campaignId"`
			Type          string   `json:"type"`
			Title         string   `json:"title"`
			Claimable     bool     `json:"claimable"`
			Status        *string  `json:"status"`
			BenefitAmount *float64 `json:"benefitAmount"`
		} `json:"activities"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("活动列表解析失败: %w", err)
	}
	out := make([]OpsActivity, 0, len(data.Activities))
	for _, a := range data.Activities {
		id := idToString(a.CampaignID)
		if id == "" {
			id = idToString(a.ID)
		}
		status := ""
		if a.Status != nil {
			status = *a.Status
		}
		amount := 0.0
		if a.BenefitAmount != nil {
			amount = *a.BenefitAmount
		}
		out = append(out, OpsActivity{
			CampaignID: id,
			Type:       a.Type,
			Title:      a.Title,
			Claimable:  a.Claimable,
			Status:     status,
			Amount:     amount,
		})
	}
	return out, nil
}

// idToString 标识符字段统一转字符串（兼容数字与字符串两种形态）。
func idToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	}
	return ""
}

// noEntitlement 上游「这个账号没有这个功能权益」的话术（个人版走不到积分活动）。
// 与瞬时 5xx 分别是两件事：重试不会让权益长出来，只会白打上游。
func noEntitlement(msg string) bool {
	for _, s := range []string{"TM.00001005", "未获得此功能的权限", "开启席位"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// CheckinResult 签到结果。
type CheckinResult struct {
	Kind    string // "claimed" | "already-claimed" | "inactive" | "failed"
	Credit  float64
	Message string
}

// CheckinDaily 完整签到流程：账户检测 → 活动列表 → claim →（需要时）confirm。
func (c *Client) CheckinDaily(ctx context.Context, cred *Credential) *CheckinResult {
	info, err := c.FetchAccountInfo(ctx, cred)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: "账户信息查询失败: " + err.Error()}
	}
	if !info.IsCreditPackage {
		msg := "非积分计费账户，不在积分活动范围"
		if info.IsTokenPackage {
			msg = "Token 计费账户，不在积分活动范围"
		}
		return &CheckinResult{Kind: "inactive", Message: msg}
	}

	activities, err := c.FetchActivities(ctx, cred)
	if err != nil {
		// 「未获得此功能的权限 / 请企业管理员开启席位」(TM.00001005) 不是瞬时故障：
		// 这个 AK/SK 所在账号本就没有积分活动权益（个人版走的就是这条路）。报 failed
		// 会让它每天进重试链、对同一个不会变的结论白打三轮，所以如实归到「不在活动范围」。
		if noEntitlement(err.Error()) {
			return &CheckinResult{Kind: "inactive", Message: "账号无积分活动权益（个人版 / 未开通席位）: " + err.Error()}
		}
		return &CheckinResult{Kind: "failed", Message: "活动列表查询失败: " + err.Error()}
	}
	var activity *OpsActivity
	for i := range activities {
		if activities[i].Type == dailyLoginType {
			activity = &activities[i]
			break
		}
	}
	if activity == nil {
		return &CheckinResult{Kind: "inactive", Message: "未找到每日签到活动"}
	}
	if !activity.Claimable {
		if claimedStatuses[activity.Status] {
			return &CheckinResult{Kind: "already-claimed", Message: "今天已领取"}
		}
		return &CheckinResult{Kind: "inactive", Message: fmt.Sprintf("当前不可领取（status=%s）", activity.Status)}
	}
	if activity.CampaignID == "" {
		return &CheckinResult{Kind: "failed", Message: "活动缺少 campaignId，无法领取"}
	}

	claimBody := fmt.Sprintf(`{"campaignId":%s,"channel":"IDE"}`, jsonQuote(activity.CampaignID))
	claimRaw, err := c.signedRequest(ctx, http.MethodPost, SnapEngineURL+opsClaimPath, cred, claimBody)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: err.Error()}
	}

	// confirm：IDE 判据是 benefit.id !== null；confirm 失败不把整体判失败
	//（积分已进待确认态，报 failed 会让用户重复点击）。
	var claimData struct {
		ID *json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(claimRaw, &claimData)
	if claimData.ID != nil && string(*claimData.ID) != "null" {
		confirmBody := fmt.Sprintf(`{"campaignId":%s}`, jsonQuote(activity.CampaignID))
		_, _ = c.signedRequest(ctx, http.MethodPost, SnapEngineURL+opsConfirmPath, cred, confirmBody)
	}

	// 积分多级回退：领取响应 → 活动条目；都没有如实记 0。
	var creditFields struct {
		BenefitAmount *float64 `json:"benefitAmount"`
		Credit        *float64 `json:"credit"`
		Credits       *float64 `json:"credits"`
		CreditAmount  *float64 `json:"creditAmount"`
		Amount        *float64 `json:"amount"`
	}
	_ = json.Unmarshal(claimRaw, &creditFields)
	credit := activity.Amount
	for _, p := range []*float64{creditFields.BenefitAmount, creditFields.Credit, creditFields.Credits, creditFields.CreditAmount, creditFields.Amount} {
		if p != nil && *p > 0 {
			credit = *p
			break
		}
	}
	return &CheckinResult{Kind: "claimed", Credit: credit}
}

// jsonQuote 字符串转 JSON 字面量（含引号）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
