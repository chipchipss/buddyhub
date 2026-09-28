// Package keypool API-key 型免费上游统一客户端。
//
// 移植自 dsh-xuediner-gateway 的 groq.ts / zhipu.ts / llm7.ts / openrouter.ts
// （四家均为 OpenAI 兼容端点，仅 base URL、key 来源、免费模型清单不同）。
// Codex 订阅池（Responses API）单独在 codex.go。
//
// 各家免费档要点（来源 DSH 插件 2026-09 实测）：
//   - Groq: gpt-oss-120b/20b、qwen3.8-27b 等；模型 id 必须全限定名
//     （"openai/gpt-oss-120b"），短名 404。
//   - 智谱: 仅 Flash 家族免费（glm-4.5/4.7-flash）；429 拥塞可重试。
//   - LLM7: 匿名/free-token 层只服务**别名**（default/fast/turbo）；
//     具名模型 402 余额不足。匿名限速 1/s 10/min 60/h 500k tokens/24h。
//   - OpenRouter: ":free" 后缀目录；需过滤非对话模型（lyria 音乐 /
//     content-safety 分类器 / image 类）。
package keypool

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

// Provider 上游标识（模型前缀同名小写）。
const (
	PGroq       = "groq"
	PZhipu      = "zp"
	PLLMS7      = "l7"
	POpenRouter = "or"
	PCodex      = "codex"
)

// upstreamDef 每家上游的静态配置。
type upstreamDef struct {
	Base   string
	Prefix string
	UA     string
}

var upstreams = map[string]upstreamDef{
	PGroq:       {Base: "https://api.groq.com/openai/v1", Prefix: "groq/", UA: "buddyhub/1.0"},
	PZhipu:      {Base: "https://open.bigmodel.cn/api/paas/v4", Prefix: "zp/", UA: "buddyhub/1.0"},
	PLLMS7:      {Base: "https://api.llm7.io/v1", Prefix: "l7/", UA: "buddyhub/1.0"},
	POpenRouter: {Base: "https://openrouter.ai/api/v1", Prefix: "or/", UA: "buddyhub/1.0"},
}

// Model 免费模型条目。
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"context_length"`
}

// Catalog 各家免费模型静态清单（动态目录失败时的兜底，来源 DSH 实测）。
func Catalog(provider string) []Model {
	switch provider {
	case PGroq:
		return []Model{
			{"openai/gpt-oss-120b", "GPT-OSS 120B (Groq)", 131072},
			{"openai/gpt-oss-20b", "GPT-OSS 20B (Groq)", 131072},
			{"qwen/qwen3.8-27b", "Qwen3.8 27B (Groq)", 131072},
			{"qwen/qwen3.6-27b", "Qwen3.6 27B (Groq)", 131072},
			{"groq/compound", "Compound (Groq)", 131072},
			{"groq/compound-mini", "Compound Mini (Groq)", 131072},
		}
	case PZhipu:
		return []Model{
			{"glm-4.7-flash", "GLM-4.7 Flash (智谱)", 200000},
			{"glm-4.5-flash", "GLM-4.5 Flash (智谱)", 131072},
			{"glm-4-flash", "GLM-4 Flash (智谱)", 131072},
		}
	case PLLMS7:
		// 免费层只服务选择器别名，具名模型 402。
		return []Model{
			{"default", "LLM7 Default (free)", 128000},
			{"fast", "LLM7 Fast (free)", 128000},
			{"turbo", "LLM7 Turbo (free)", 128000},
		}
	case POpenRouter:
		// 实测可用子集（全目录动态拉取失败时的兜底；1M 上下文的 Inkling 系为当前旗舰免费档）。
		return []Model{
			{"thinkingmachines/inkling:free", "Inkling (free)", 1048576},
			{"nvidia/nemotron-3-ultra-550b-a55b:free", "Nemotron 3 Ultra 550B (free)", 1000000},
			{"inclusionai/ling-3.0-flash-sante:free", "Ling 3.0 Flash Sante (free)", 262144},
			{"poolside/laguna-s-2.1:free", "Laguna S 2.1 (free)", 262144},
			{"google/gemma-4-31b-it:free", "Gemma 4 31B (free)", 262144},
		}
	}
	return nil
}

// IsChatCapable 过滤 OpenRouter 免费目录里的非对话模型
// （音乐生成 / 内容分类器 / 图像类，必失败只添延迟）。
func IsChatCapable(id string) bool {
	lower := strings.ToLower(id)
	return !strings.Contains(lower, "lyria") &&
		!strings.Contains(lower, "content-safety") &&
		!strings.Contains(lower, "image")
}

// Client 单一 HTTP 客户端供全部上游复用。
type Client struct {
	HTTP *http.Client
}

// New 创建客户端。
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// chat 通用 OpenAI 兼容流式请求：返回 SSE 流。调用方负责关闭。
func (c *Client) chat(ctx context.Context, provider, apiKey string, body []byte) (io.ReadCloser, int, error) {
	def, ok := upstreams[provider]
	if !ok {
		return nil, 0, fmt.Errorf("未知上游: %s", provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, def.Base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", def.UA)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// OpenRouter 推荐头（免费档排名与归因）。
	if provider == POpenRouter {
		req.Header.Set("HTTP-Referer", "http://127.0.0.1:7863")
		req.Header.Set("X-Title", "BuddyHub")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	return resp.Body, resp.StatusCode, nil
}

// truncate 有界错误文本。
func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// ChatStream 发起指定上游的流式对话。调用方负责关闭 ReadCloser。
func (c *Client) ChatStream(ctx context.Context, provider, apiKey string, incoming []byte) (io.ReadCloser, int, error) {
	var req map[string]any
	if err := json.Unmarshal(incoming, &req); err != nil {
		return nil, 0, fmt.Errorf("解析请求体失败: %w", err)
	}
	req["stream"] = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}
	return c.chat(ctx, provider, apiKey, body)
}
