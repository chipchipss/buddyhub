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
import (
	"bytes"
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
// **三个模型头少一个都不行**（model-id / model-name / x-model-id），上游把模型名
// 也当请求头用——漏了不会 404，只会被判成"没这个模型"。
func (p ChatHeaderProfile) headers(model string) map[string]string {
	h := map[string]string{
		"Content-Type":   "application/json",
		"Accept":         "text/event-stream",
		"client_version": "Vscode_" + p.PluginVersion,
		"Agent-Type":     "ChatAgent",
		"X-Language":     p.Language,
		"plugin-name":    p.PluginName,
		"plugin-version": p.PluginVersion,
		"model-id":       model,
		"model-name":     model,
		"x-model-id":     model,
	}
	if p.IsConfidential {
		h["is_confidential"] = "true"
	} else {
		h["is_confidential"] = "false"
	}
	if p.SessionID != "" {
		h["User-Session-Id"] = p.SessionID
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
// `model` 是**上游模型名**（前缀剥掉后的裸名）；`benefit` 给福利模型注入
// `maas_type: benefit`（普通模型带了反而会被判成"没领福利"）。
func BuildChatRequest(cred *Credential, model string, payload []byte, stream bool, benefit bool) (string, map[string]string, []byte, error) {
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
	if benefit {
		doc["maas_type"] = "benefit"
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return "", nil, nil, fmt.Errorf("请求体序列化失败：%w", err)
	}

	endpoint := SnapEngineURL + ChatPath
	profile := DefaultChatProfile()
	signed, err := signChat(cred, http.MethodPost, endpoint, body, profile.headers(model))
	if err != nil {
		return "", nil, nil, err
	}
	return endpoint, signed, body, nil
}

// Chat 发一次对话，返回上游原始响应（SSE 或 JSON，由 body 的 stream 决定）。
// 返回的 resp 由调用方 Close。
func (c *Client) Chat(ctx context.Context, cred *Credential, model string, payload []byte, stream, benefit bool) (*http.Response, error) {
	endpoint, headers, body, err := BuildChatRequest(cred, model, payload, stream, benefit)
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

// ListModels 拉模型目录（签名 GET，同样不签 host）。
func (c *Client) ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	endpoint := SnapEngineURL + ModelsPath
	headers := map[string]string{"Accept": "application/json"}
	signed, err := signChat(cred, http.MethodGet, endpoint, nil, headers)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range signed {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("模型目录请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncate(raw, 200))
	}
	return parseModels(raw), nil
}

// Model 目录条目。
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

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
