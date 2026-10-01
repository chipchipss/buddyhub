package raccoon

// chat.go 小浣熊对话通道：LLM 网关直连 + 模型目录。
//
// 协议事实来自 agent2api 的 raccoon adapter（README 已列该仓库为参考来源）：
//
//	POST {llmBase}/chat/completions
//	    Content-Type: application/json
//	    Accept: */*                      ← 网关两种响应形态都可能回
//	    Authorization: Bearer <access_token>
//	    body = 客户端原始 OpenAI 请求，**原样透传**
//	GET  {llmBase}/model_catalog          → {code, data:{categories:[{models:[…]}]}}
//
// ── 三处与别的通道不同（照抄会错）─────────────────────────────
//
//  1. **只有 3 个头**：不像积分/签到那条链路要带 X-Client-Platform 等桌面指纹，
//     对话端点只要 Content-Type / Accept / Authorization 三个。
//  2. **body 一个字都不改**：上游本身就是 OpenAI 兼容，既不重建白名单，
//     也不改 system 消息（那是腾讯 workbuddy 的硬要求，小浣熊没有）。
//  3. **响应帧的 model 要回写**：上游会把它换成自己的内部名（见 RewriteModel）。
//
// 不能复用 raccoon.go 的 do() —— 那是 {code,data} 信封协议，装不了 SSE 流。
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// LLMBase 小浣熊 LLM 网关基址（末尾无斜杠）。
const LLMBase = "https://xiaohuanxiong.com/api/web/llm/v2"

// chatTimeout 对话是长回答场景，不设总超时（上游可能跑数分钟）；
// 连接与响应头超时由传输层负责。
const chatTimeout = 15 * time.Minute

// CatalogTTL 模型目录缓存有效期（与参考实现 10 分钟一致）。
// 导出给桥接做缓存判定。
const CatalogTTL = 10 * time.Minute

// Model 目录条目。**id 是上游的 `name`**（小浣熊目录用 name 当模型 id、
// displayName 当展示名）——不换这层键，/v1/models 会输出一批没有 id 的条目，
// 路由也只能退化成按展示名比，那是静默的功能缺失。
type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name"` // 展示名
	Description string `json:"description,omitempty"`
	Tags        string `json:"tags,omitempty"`
	ContextMax  int    `json:"context_max,omitempty"`
	OutputMax   int    `json:"output_max,omitempty"`
	ImageInput  bool   `json:"image_input,omitempty"`
	Thinking    bool   `json:"thinking,omitempty"`
	Credits     string `json:"credits,omitempty"`
}

// Chat 发一次对话，返回上游原始响应（流式/非流式由 body 决定，调用方自己分流）。
//
// 返回的 resp 由调用方负责 Close。401/429 也按**返回值**而不是错误表达，
// 好让桥接能分辨「该换号了」与「网络不通」。
func (c *Client) Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("小浣熊账号缺少 access_token，无法对话")
	}
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, LLMBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Bearer "+stripBearer(cred.AccessToken))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("对话请求失败：%w", err)
	}
	if resp.StatusCode >= 400 {
		// 错误体读出来给桥接展示（上游非 2xx 只透传状态码 + message）
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		resp.Body.Close()
		cancel()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: truncate(raw, 400)}
	}
	// 成功路径不 cancel：让调用方读完流再释放（cancel 会掐断流）
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// UpstreamError 非 2xx 的上游回执。Status 供桥接分类（401 刷新重试 / 429 限流）。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("上游返回 %d: %s", e.Status, e.Body)
}

// cancelBody 读完流再释放 context（否则非 2xx 路径的 cancel 会漏掉，
// 或者反过来：过早 cancel 会把正在推流的连接掐断）。
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// stripBearer 去掉 `Bearer ` 前缀（手工粘贴凭据常整段粘进来，
// header 构造时还要再加一次，拼成 Bearer Bearer 会被上游拒）。
func stripBearer(v string) string {
	for _, p := range []string{"Bearer ", "bearer ", "BEARER "} {
		if len(v) > len(p) && v[:len(p)] == p {
			return strings.TrimSpace(v[len(p):])
		}
	}
	return v
}

// RewriteModel 把上游 SSE 帧里的 model 回写成客户端请求的名字。
//
// 上游网关会把下发帧的 model 换成它自己的内部名（源实现
// `raccoon-sse-pipe.mjs` 的 `pipeSseWithModelRewrite` 专处理这件事），
// 不回写的话客户端与聚合层会看到一个它从没请求过的模型名。
//
// 返回原样字节（没变时不做任何解析开销）。
func RewriteModel(data []byte, want string) []byte {
	if want == "" || !bytes.Contains(data, []byte(`"model"`)) {
		return data
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &probe) != nil || probe.Model == want || probe.Model == "" {
		return data
	}
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return data
	}
	if obj, ok := doc.(map[string]any); ok {
		obj["model"] = want
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

/* ── 模型目录 ─────────────────────────────────────────────── */

// FallbackModels 静态兜底清单（5 条，逐字对齐参考实现的 FALLBACK_MODELS）。
//
// **不要自己编**：编出来的模型名会在 /v1/models 里显示一个上游根本不认的东西。
// `raccoon-chat-ml-5-5` 刻意不在此列之外的条目里，它是第 4 条（默认模型）。
//
// 进程启动即可用：网络不通、token 失效时 /v1/models 仍然有内容。
func FallbackModels() []Model {
	return []Model{
		{
			ID: "raccoon-8c4485", Name: "Raccoon-Work",
			Description: "适合复杂办公任务、代码开发、调试和图像识别等任务（目录默认旗舰，自称 GPT-5）",
			ContextMax:  1_000_000, OutputMax: 100_000, ImageInput: true, Credits: "x1 credits",
		},
		{
			ID: "raccoon-19b265", Name: "Raccoon-Work-260817-A",
			Description: "适合复杂办公任务、代码开发和综合分析等任务（A/B 变体 A，自称 GPT-5）",
			ContextMax:  1_000_000, OutputMax: 100_000, Credits: "x1 credits",
		},
		{
			ID: "raccoon-405a1c", Name: "Raccoon-Work-260817-B",
			Description: "适合通用对话、轻量代码和内容处理等任务（A/B 变体 B，自称 GPT-5）",
			ContextMax:  1_000_000, OutputMax: 100_000, Credits: "x1 credits",
		},
		{
			ID: "raccoon-chat-ml-5-5", Name: "Raccoon Chat (默认)",
			Description: "小浣熊默认对话模型（default-llm-config.json 预置）。",
			ContextMax:  180_000, OutputMax: 80_000, Thinking: true,
		},
		{
			ID: "sn-sensenova-6-8-flash-lite", Name: "SenseNova 6.8 Flash Lite",
			Description: "轻量模型，用于标题生成等小任务。",
			ContextMax:  256_000, OutputMax: 63_999, Thinking: true, Credits: "x0.5 credits",
		},
	}
}

// ListModels 拉远程模型目录（需要登录态，实测无 token 回 401）。
//
// 失败返回 error，调用方自行决定是用旧缓存还是静态兜底——**不要在这里吞掉**，
// 否则「token 过期」与「网络不通」在日志里长得一模一样。
func (c *Client) ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("缺少 access_token，无法拉取模型目录")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LLMBase+"/model_catalog", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Bearer "+stripBearer(cred.AccessToken))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("模型目录请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncate(raw, 200))
	}
	return parseCatalog(raw), nil
}

// parseCatalog 解析 `{code, data:{categories:[{models:[…]}]}}`。
// 根上没有 data 时把根当 data；平铺所有分类；同名首次出现者胜；空 name 丢弃。
func parseCatalog(raw []byte) []Model {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	cats, _ := data["categories"].([]any)
	out := make([]Model, 0, 16)
	seen := map[string]bool{}
	for _, c := range cats {
		cat, _ := c.(map[string]any)
		entries, _ := cat["models"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			// **上游用 name 当模型 id**，displayName 当展示名 —— 换过来才是
			// OpenAI 形状（id 是模型、name 是展示名）。
			id, _ := entry["name"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			display, _ := entry["displayName"].(string)
			if display == "" {
				display = id
			}
			m := Model{ID: id, Name: display}
			m.Description, _ = entry["description"].(string)
			m.Credits, _ = entry["credits"].(string)
			m.ContextMax = numOr(entry["contextWindow"])
			m.OutputMax = numOr(entry["maxTokens"])
			m.ImageInput, _ = entry["supportImage"].(bool)
			m.Thinking, _ = entry["supportThinking"].(bool)
			m.Tags = joinStrings(entry["tags"])
			out = append(out, m)
		}
	}
	return out
}

func numOr(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

func joinStrings(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, ok := a.(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "," + p
	}
	return out
}

// truncate 截断上游原文，只用于错误信息与日志。
func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
