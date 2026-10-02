package codearts

// chat.go 华为云 CodeArts 对话通道。
//
// 端点 `POST https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions`
// —— **与积分/签到同一个 SnapEngineURL**，只是多了 api/v2 路径。
//
// ── 为什么对话要另写一套签名 ─────────────────────────────────────
// 参考实现（agent2api）在同一处明确记着两套口径**刻意不同**：
//
//	balance.rs:313 「签名口径与区域 API 不同：带 Host、不带 X-Domain-Id」
//	chat.rs:140    signer::sign(…, include_host = **false**)
//
// 对话这套（`signer_vectors.json` 的 `reference-vector`，注释写明"对照官方测试
// 向量"）签进去的是：
//
//	content-type;plugin-name;x-domain-id;x-language;x-sdk-date;x-security-token
//
// —— **没有 host、没有 x-sdk-content-sha256**，只签调用方给的头 + 两个固定的。
// 而区域 API 那套（signRequestHuawei）恒签 host。照抄任意一套到另一条链路，
// 得到的都是稳定 401 `verify ak sk signature fail`，且**因为没有账号无法端到端
// 验证**，只能靠官方向量的黄金测试兜底（见 signChatGoldenTest）。
//
// ── 两条模型通道（agent / benefit）──────────────────────────────
// 上游模型分两条计费通道，签名头集合**不同**（实测 + 参考项目实证）：
//
//   - agent 通道（老套餐模型，如 openpangu-2.0-pro / GLM-5.2）：签名头不含
//     model-id / model-name / x-model-id 三头，模型名只走 body。
//   - benefit 通道（免费额度模型，如 deepseek-v4-flash-0731）：body 带
//     maas_type=benefit，且三模型头**必须参与签名**——缺了报 "model is not
//     registered"；agent 模型带三头反而报 "unsupported model"。
//
// 通路由模型名决定（IsBenefitModel：兜底目录里的 benefit 模型 + 未知新模型
// 保守按 benefit），见 BuildChatRequest 的 ChatOptions。
//
// 模型目录：`/v1/model/builtin` 与 AgentCenter 的 useragents GET 对本凭据都
// 401（永久 AK/SK 拉不到），ListModels 因此走兜底名单 FallbackModels。
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// ChatPath 对话端点（相对 base）。
	ChatPath = "/api/v2/chat/completions"
	// ModelsPath 模型目录（签名 GET）。
	ModelsPath = "/v1/model/builtin"

	// 客户端指纹默认值，照参考实现的 config 默认档。
	DefaultPluginName    = "snap_vscode"
	DefaultPluginVersion = "26.9.101"
	DefaultLanguage      = "en-us"
)

// ChatHeaderProfile 一次对话请求的客户端指纹头（全部参与签名）。
type ChatHeaderProfile struct {
	PluginName     string
	PluginVersion  string
	Language       string
	IsConfidential bool
	// SessionID 会话并发心跳用；空即不带。
	SessionID string
}

// ChatOptions 一次对话的通道选择（见包注释「两条通道」）。
//
// Benefit=true 时出站体注入 maas_type=benefit，且三个模型头（model-id /
// model-name / x-model-id）必须**参与签名**——上游缺这三个头就报
// "model is not registered"。Agent 通道的模型名走普通路径即可，签名头
// 集合与 Benefit 不同（Benefit 头在签名前就要注入）。
type ChatOptions struct {
	// Benefit 走免费额度通道（签到领取的模型族）。
	Benefit bool
	// 模型名透传给上游 model-id/model-name/x-model-id 三头。
	Model string
}

// DefaultChatProfile 默认指纹档（与参考实现 HeaderProfile::default 一致）。
func DefaultChatProfile() ChatHeaderProfile {
	return ChatHeaderProfile{
		PluginName:    DefaultPluginName,
		PluginVersion: DefaultPluginVersion,
		Language:      DefaultLanguage,
	}
}

// headers 构造参与签名的客户端指纹头。
//
// 三个模型头（model-id / model-name / x-model-id）**是否参与签名**由
// benefit 决定：
//   - benefit 通道（免费模型）：三个模型头**必须**进签名——上游缺它们就报
//     "model is not registered"（参考项目 BENEFIT_MODELS 实测）。
//   - agent 通道（老套餐模型）：模型名只走 body，不进签名；强行带上这三个
//     头反而会被判 "unsupported model"（参考项目两条通道严格区分的原因）。
func (p ChatHeaderProfile) headers(model string, benefit bool) map[string]string {
	h := map[string]string{
		"Content-Type":   "application/json",
		"Accept":         "text/event-stream",
		"client_version": "Vscode_" + p.PluginVersion,
		"Agent-Type":     "ChatAgent",
		"X-Language":     p.Language,
		"plugin-name":    p.PluginName,
		"plugin-version": p.PluginVersion,
	}
	if p.IsConfidential {
		h["is_confidential"] = "true"
	} else {
		h["is_confidential"] = "false"
	}
	if p.SessionID != "" {
		h["User-Session-Id"] = p.SessionID
	}
	if benefit {
		// benefit 通道：模型三头进签名 + maas_type 走 body（BuildChatRequest 注入）。
		h["model-id"] = model
		h["model-name"] = model
		h["x-model-id"] = model
	}
	return h
}

// signChat 对话链路的签名（对齐官方测试向量，见模块头）。
//
// 输入的 in 是**已经成型的客户端头**，本函数补 x-sdk-date / x-security-token
// 后交给纯函数 signChatCanonical。日期与 body 单独一层，是为了能用官方向量
// 做黄金测试——固定日期才算得出与向量相同的签名。
func signChat(cred *Credential, method, urlStr string, body []byte, in map[string]string) (map[string]string, error) {
	if cred == nil || cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return nil, fmt.Errorf("CodeArts 账号缺少 AK/SK，无法签名")
	}
	_, uri, query, err := splitURL(urlStr)
	if err != nil {
		return nil, err
	}
	dateStamp := time.Now().UTC().Format("20060102T150405Z")
	h := make(map[string]string, len(in)+2)
	for k, v := range in {
		h[k] = v
	}
	h["x-sdk-date"] = dateStamp
	if cred.SecurityToken != "" {
		h["x-security-token"] = cred.SecurityToken
	}

	_, signedNames, sig := signChatCanonical(method, uri, query, h, body,
		dateStamp, cred.AccessKeyID, cred.SecretAccessKey)

	out := make(map[string]string, len(h)+1)
	for k, v := range h {
		out[k] = v
	}
	out["Authorization"] = "SDK-HMAC-SHA256 Access=" + cred.AccessKeyID +
		",SignedHeaders=" + signedNames + ",Signature=" + sig
	return out, nil
}

// signChatCanonical 纯签名。返回规范请求串、参与签名的头名（小写排序、`;` 连接）
// 与 HMAC 签名。
//
// 规则逐字对齐 `signer_vectors.json` 的 `reference-vector`
// （该条注释为「signer_test.go 对照官方测试向量」）：
//   - path 末尾补 `/`，query 原样（向量里为空）
//   - 头名**全部小写**后按字典序排，`k:v` 换行拼接
//   - **不把 host 与 x-sdk-content-sha256 签进去**（include_host=false）——
//     这是与区域 API 那套（signRequestHuawei 恒签 host）的唯一结构差异
//   - 末行是 body 的 sha256，但它不作为一个 header 出现
func signChatCanonical(method, uri, query string, headers map[string]string,
	body []byte, dateStamp, accessKey, secretKey string) (canonical, signedNames, signature string) {

	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	lower := make(map[string]string, len(headers))
	for k, v := range headers {
		lower[strings.ToLower(k)] = v
	}
	names := make([]string, 0, len(lower))
	for k := range lower {
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
		headerLines.WriteString(lower[k])
	}
	signedNames = strings.Join(names, ";")
	payloadHash := sha256Hex(string(body))
	canonical = strings.Join([]string{
		method, uri, query, headerLines.String(), "",
		signedNames, payloadHash,
	}, "\n")

	stringToSign := "SDK-HMAC-SHA256\n" + dateStamp + "\n" + sha256Hex(canonical)
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(stringToSign))
	signature = hex.EncodeToString(mac.Sum(nil))
	_ = accessKey
	return canonical, signedNames, signature
}

// BuildChatRequest 组装一次出站对话请求（端点 + 头 + 体）。
//
// `model` 是**上游模型名**（前缀剥掉后的裸名）。opts.Benefit 决定通道：
// 免费模型注入 `maas_type: benefit` 并把模型三头纳入签名（见 headers 注释）；
// 普通模型一律按 agent 通道签（带 benefit 头反而 "unsupported model"）。
func BuildChatRequest(cred *Credential, model string, payload []byte, stream bool, opts ChatOptions) (string, map[string]string, []byte, error) {
	if model == "" {
		return "", nil, nil, fmt.Errorf("CodeArts 模型名为空")
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return "", nil, nil, fmt.Errorf("请求体不是合法 JSON：%w", err)
	}
	doc["model"] = model
	doc["stream"] = stream
	if stream {
		// 不要求就不给 usage（上游要被点名才会带）
		doc["stream_options"] = map[string]any{"include_usage": true}
	}
	if opts.Benefit {
		doc["maas_type"] = "benefit"
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return "", nil, nil, fmt.Errorf("请求体序列化失败：%w", err)
	}

	endpoint := SnapEngineURL + ChatPath
	profile := DefaultChatProfile()
	signed, err := signChat(cred, http.MethodPost, endpoint, body, profile.headers(model, opts.Benefit))
	if err != nil {
		return "", nil, nil, err
	}
	return endpoint, signed, body, nil
}

// Chat 发一次对话，返回上游原始响应（SSE 或 JSON，由 body 的 stream 决定）。
// 返回的 resp 由调用方 Close。
func (c *Client) Chat(ctx context.Context, cred *Credential, model string, payload []byte, stream bool, opts ChatOptions) (*http.Response, error) {
	endpoint, headers, body, err := BuildChatRequest(cred, model, payload, stream, opts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("对话请求失败：%w", err)
	}
	return resp, nil
}

// Model 目录条目。
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Benefit bool   `json:"benefit"` // 免费额度模型（走 benefit 签名通道）
}

// FallbackModels 兜底模型目录（云端同步失败时，/v1/models 至少能列出这些）。
// 与参考项目 FALLBACK_AGENT_MODELS + BENEFIT_MODELS 一致。
var FallbackModels = []Model{
	{ID: "openpangu-2.0-pro", Name: "openpangu-2.0-pro"},
	{ID: "openpangu-2.0-flash", Name: "openpangu-2.0-flash"},
	{ID: "GLM-5.2", Name: "GLM-5.2"},
	{ID: "deepseek-v4-flash-0731", Name: "deepseek-v4-flash-0731", Benefit: true},
	{ID: "deepseek-v4-pro-0813", Name: "deepseek-v4-pro-0813", Benefit: true},
	{ID: "glm-5.3-flash", Name: "glm-5.3-flash", Benefit: true},
}

// IsBenefitModel 该裸模型名是否走 benefit 通道（免费额度模型）。
// 未知模型名默认按 benefit 处理——参考项目同款判据：agent 通道模型会显式
// 出现在 AgentCenter 目录里，没见过的名字（多为新增免费模型）一律按
// benefit 头签名，否则上游报 "model is not registered"。
func IsBenefitModel(bare string) bool {
	for _, m := range FallbackModels {
		if m.ID == bare {
			return m.Benefit
		}
	}
	// 兜底名单里没有 = 未验证过的新模型，按 benefit 处理（参考项目同款保守判据）。
	return true
}

// ListModels 拉模型目录。
//
// 实测（本机永久 AK/SK）：`/v1/model/builtin` 与 AgentCenter 的 useragents
// GET 端点都 401——签名口径与 chat 不同，且服务端对这两个 GET 端点的校验更严，
// 永久 AK/SK 拉不到。因此目录改走**兜底名单**（FallbackModels），保证
// /v1/models 与 benefit 路由判断永远有可用集合；等上游 GET 端点对该凭据放行
// 后，可在下面加回云端同步（parseModels 已备好）。
func (c *Client) ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	_ = ctx
	_ = cred
	return FallbackModels, nil
}

// parseModels 保留：上游端点恢复后可直接复用。
func parseModels(raw []byte) []Model {
	var doc struct {
		Models []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			ModelID string `json:"model_id"`
		} `json:"models"`
		// 有的版本直接平铺在根上
		Data struct {
			Models []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				ModelID string `json:"model_id"`
			} `json:"models"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	list := doc.Models
	if len(list) == 0 {
		list = doc.Data.Models
	}
	out := make([]Model, 0, len(list))
	seen := map[string]bool{}
	for _, m := range list {
		id := m.ID
		if id == "" {
			id = m.ModelID
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := m.Name
		if name == "" {
			name = id
		}
		out = append(out, Model{ID: id, Name: name})
	}
	return out
}
