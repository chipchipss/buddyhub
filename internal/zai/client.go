package zai

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
)

// BuildRequest 组装上游请求：返回 (目标 URL, 请求头, 请求体)。
//
// 两条通道的头集刻意不同（协议事实，误发会触发上游 3012）：
//
//	Plan（JWT）  全套桌面身份头 + 追踪头 + 验证码头；体走 coding-plan 变换
//	回退（API Key）最小头集 + anthropic-version；体不做身份块注入
//
// forceFallback=true 强制走回退通道：Plan 通道瞬态失败（429 耗尽）时账号状态
// 仍是 ACTIVE，不能靠状态推导路由，必须显式指定。
//
// systemBlocks 为 Plan 通道要求的身份块（官方客户端会前置这段 system，缺失
// 会被上游拒为 3012）。为空则跳过——调用方应从配置读取，见 SolverConfig 同级的
// 说明：本包不内嵌该内容。
func BuildRequest(a *Account, body []byte, verifyParam, verifyRegion string, forceFallback bool, systemBlocks []SystemBlock) (string, map[string]string, []byte) {
	headers := map[string]string{"content-type": "application/json"}
	var url string

	plan := a.HasJWTPath() && !forceFallback
	switch {
	case a.Provider == ProviderBigModel:
		// 智谱开放平台：同形 anthropic 端点，只有 API Key，无身份头与验证码
		url = BigModelMessagesURL
		headers["x-api-key"] = a.APIKey
		headers["anthropic-version"] = AnthropicVersion
		headers["User-Agent"] = "ZCode/" + ClientAppVersion
	case plan:
		url = PlanMessagesURL
		headers["Authorization"] = "Bearer " + a.JWT
		headers["anthropic-version"] = AnthropicVersion
		applyIdentity(headers, a)
		applyTrace(headers)
		if verifyParam != "" {
			headers["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		}
		if verifyRegion != "" {
			headers["X-Aliyun-Captcha-Verify-Region"] = verifyRegion
		}
		body = transformPlanBody(body, a, systemBlocks)
	case a.APIKey != "":
		url = FallbackMessagesURL
		headers["x-api-key"] = a.APIKey
		headers["anthropic-version"] = AnthropicVersion
		headers["User-Agent"] = "ZCode/" + ClientAppVersion
		headers["X-ZCode-App-Version"] = ClientAppVersion
		headers["X-ZCode-Agent"] = "glm"
		headers["HTTP-Referer"] = Referer
	default:
		url = FallbackMessagesURL // 无凭证：交给上游判 401
		headers["anthropic-version"] = AnthropicVersion
	}
	return url, headers, body
}

// applyIdentity 写全套桌面身份头（字段顺序对齐官方客户端的 pio 头集合）。
func applyIdentity(h map[string]string, a *Account) {
	a.EnsureProfile()
	p := a.Fingerprint
	h["HTTP-Referer"] = Referer
	h["User-Agent"] = "ZCode/" + ClientAppVersion
	h["X-ZCode-App-Version"] = ClientAppVersion
	h["X-Title"] = "Z Code@electron"
	h["X-ZCode-Agent"] = "glm"
	h["X-Platform"] = p.PlatformFull()
	h["X-Release-Channel"] = "stable"
	h["X-Client-Language"] = p.Language
	h["X-Client-Timezone"] = p.Timezone
	h["X-Os-Category"] = p.OSCategory()
	h["X-Os-Version"] = p.OSVersion
	h["X-Device-Mid"] = p.DeviceMid
}

// applyTrace 写追踪头。
//
// 关键：Plan 通道（官方客户端语义的 start-plan）**只发这三个头**。
// 误发 x-query-id / x-session-id 会被上游判为 3012「unusual activity」。
func applyTrace(h map[string]string) {
	h["x-request-id"] = UUID()
	h["x-zcode-session-type"] = "main"
	h["x-zcode-trace-id"] = UUID()
}

// SystemBlock 身份块（type=text + 可选 cache_control）。
type SystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

// transformPlanBody 对 Plan 通道请求体做三项变换（对畸形输入保持 no-op）：
//
//  1. system 前置身份块（缺失会被上游拒为 3012）
//  2. 最后一条非 system 消息的最后一个 content block 追加 cache_control
//  3. metadata.user_id 注入（从 JWT payload 实时解出）
func transformPlanBody(body []byte, a *Account, blocks []SystemBlock) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body // 畸形体原样透传，不在这里放大
	}

	if len(blocks) > 0 {
		m["system"] = mergeSystem(m["system"], blocks)
	}
	finalizeCacheControl(m)

	if uid := jwtUserID(a.JWT); uid != "" {
		meta, _ := m["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["user_id"] = uid
		m["metadata"] = meta
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// mergeSystem 把身份块前置到客户端 system 之前（保持客户端原有块在后）。
func mergeSystem(existing any, blocks []SystemBlock) []any {
	out := make([]any, 0, len(blocks)+4)
	for _, b := range blocks {
		blk := map[string]any{"type": "text", "text": b.Text}
		if b.CacheControl != nil {
			blk["cache_control"] = b.CacheControl
		}
		out = append(out, blk)
	}
	switch v := existing.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
	case []any:
		out = append(out, v...)
	}
	return out
}

// finalizeCacheControl 给最后一条非 system 消息的最后一个 block 打 ephemeral 缓存标记。
// Anthropic 对低于缓存门槛的请求静默忽略该字段，故无条件追加是安全的。
func finalizeCacheControl(m map[string]any) {
	msgs, _ := m["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, _ := msgs[i].(map[string]any)
		if msg == nil || msg["role"] == "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case []any:
			if len(content) == 0 {
				return
			}
			if blk, ok := content[len(content)-1].(map[string]any); ok {
				if _, exists := blk["cache_control"]; !exists {
					blk["cache_control"] = map[string]any{"type": "ephemeral"}
				}
			}
		case string:
			if strings.TrimSpace(content) == "" {
				return
			}
			msg["content"] = []any{map[string]any{
				"type": "text", "text": content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		}
		return
	}
}

// jwtUserID 从 JWT payload 解 user_id（sub / user_id），失败返回空串。
// 每次实时解：token 刷新后自动跟随。
func jwtUserID(token string) string {
	if strings.Count(token, ".") != 2 {
		return ""
	}
	part := strings.Split(token, ".")[1]
	if pad := len(part) % 4; pad != 0 {
		part += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part, "="))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(part)
		if err != nil {
			return ""
		}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	for _, k := range []string{"user_id", "sub"} {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// LoadSystemBlocks 读取身份块文件（JSON 数组）。文件缺失/非法时返回 nil。
//
// 该内容取自官方客户端（会随版本变化），本包不内嵌，由 `zai.system_file` 指定。
func LoadSystemBlocks(path string) []SystemBlock {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		logf("zai: 读取身份块文件失败（%s）：%v", path, err)
		return nil
	}
	var blocks []SystemBlock
	if json.Unmarshal(raw, &blocks) != nil {
		logf("zai: 身份块文件不是 JSON 数组（%s）", path)
		return nil
	}
	return blocks
}
