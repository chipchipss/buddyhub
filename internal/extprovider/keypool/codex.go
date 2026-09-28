// Package keypool: codex.go — ChatGPT 订阅号池（Codex Responses API 直连）。
//
// 移植自 dsh-xuediner-gateway src/codex.ts（2026-09-20 实测协议）：
//
//	端点:  POST https://chatgpt.com/backend-api/codex/responses
//	认证:  Bearer <access_token> + chatgpt-account-id 头（JWT exp 本地判过期）
//	格式:  Responses（input items / instructions / reasoning.effort），非 chat.completions
//	凭据:  本机 Codex CLI 的 auth.json（~/.codex 或 ~/.codex-pool-b 等多目录）
//
// 账号轮转：过期账号直接排除；401 触发冷却记号（进程内）。
package keypool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CodexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	codexHeaderTO     = 30 * time.Second
)

// CodexAccount 一个可入池的 Codex 账号（来自 auth.json）。
type CodexAccount struct {
	Key         string `json:"key"`  // 池内稳定键（目录名）
	Home        string `json:"home"` // auth.json 所在目录
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	ExpiresAt   int64  `json:"expires_at"` // 秒；0 = 未知
	Plan        string `json:"plan,omitempty"`
	Email       string `json:"email,omitempty"`
}

// codexHomes 可扫描的 CODEX_HOME 候选（账号 A = 官方默认，B+ = 多号池扩展目录）。
func codexHomes() []struct {
	Key  string
	Home string
} {
	home, _ := os.UserHomeDir()
	return []struct {
		Key  string
		Home string
	}{
		{"a", filepath.Join(home, ".codex")},
		{"b", filepath.Join(home, ".codex-pool-b")},
		{"c", filepath.Join(home, ".codex-pool-c")},
	}
}

// ListCodexAccounts 扫描本机全部 auth.json，返回 token 仍有效的账号。
func ListCodexAccounts() []*CodexAccount {
	var out []*CodexAccount
	now := time.Now().Unix()
	for _, cand := range codexHomes() {
		file := filepath.Join(cand.Home, "auth.json")
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var j struct {
			AuthMode string `json:"auth_mode"`
			Tokens   struct {
				AccessToken  string `json:"access_token"`
				AccountID    string `json:"account_id"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
			} `json:"tokens"`
		}
		if json.Unmarshal(raw, &j) != nil || j.Tokens.AccessToken == "" || j.Tokens.AccountID == "" {
			continue
		}
		exp := jwtExpMS(j.Tokens.AccessToken) / 1000
		// 过期 token 直接排除：上游必 401，不值得烧冷却名额。
		if exp > 0 && exp <= now {
			continue
		}
		out = append(out, &CodexAccount{
			Key:         cand.Key,
			Home:        cand.Home,
			AccessToken: j.Tokens.AccessToken,
			AccountID:   j.Tokens.AccountID,
			ExpiresAt:   exp,
		})
	}
	return out
}

// jwtExpMS 解析 JWT payload 的 exp（毫秒）；解析失败返回 0。
func jwtExpMS(jwt string) int64 {
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

// codexHeaders 单次请求头（account id 参与鉴权，缺了会被拒）。
func codexHeaders(acct *CodexAccount) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+acct.AccessToken)
	h.Set("chatgpt-account-id", acct.AccountID)
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("originator", "codex_cli_rs")
	h.Set("User-Agent", "codex_cli_rs/0.55.0")
	return h
}

// toResponsesInput 把 OpenAI chat messages 转成 Responses input items。
// tool 结果 → function_call_output；assistant tool_calls → function_call 逐项；
// 普通文本轮 → message（assistant 用 output_text，其余 input_text）。
func toResponsesInput(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, mAny := range messages {
		m, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		content := m["content"]
		if role == "tool" {
			toolCallID, _ := m["tool_call_id"].(string)
			outStr, isStr := content.(string)
			if !isStr {
				b, _ := json.Marshal(content)
				outStr = string(b)
			}
			out = append(out, map[string]any{
				"type": "function_call_output", "call_id": toolCallID, "output": outStr,
			})
			continue
		}
		if role == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					tc, ok2 := tcAny.(map[string]any)
					if !ok2 {
						continue
					}
					id, _ := tc["id"].(string)
					fn, _ := tc["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					out = append(out, map[string]any{
						"type": "function_call", "call_id": id, "name": name,
						"arguments": args,
					})
				}
				if s, ok := content.(string); ok && s != "" {
					out = append(out, map[string]any{"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": s}}})
				}
				continue
			}
		}
		text, isStr := content.(string)
		if !isStr {
			b, _ := json.Marshal(content)
			text = string(b)
		}
		partType := "input_text"
		outRole := role
		if role == "assistant" {
			partType = "output_text"
			outRole = "assistant"
		} else {
			outRole = "user"
		}
		out = append(out, map[string]any{"type": "message", "role": outRole,
			"content": []any{map[string]any{"type": partType, "text": text}}})
	}
	return out
}

// CodexChatStream 发起 Codex Responses 流式请求（恒 stream=true）。
// incoming 为 OpenAI chat.completions 形态的客户端请求体（模型名已剥前缀）。
func (c *Client) CodexChatStream(ctx context.Context, acct *CodexAccount, incoming []byte, system string) (io.ReadCloser, int, error) {
	var req map[string]any
	if err := json.Unmarshal(incoming, &req); err != nil {
		return nil, 0, fmt.Errorf("解析请求体失败: %w", err)
	}
	messages, _ := req["messages"].([]any)
	body := map[string]any{
		"model":        req["model"],
		"instructions": system,
		"input":        toResponsesInput(messages),
		"stream":       true,
		"store":        false,
	}
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		body["tools"] = tools
	}
	effort := "medium"
	if re, ok := req["reasoning_effort"].(string); ok && re != "" {
		effort = re
	}
	body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, CodexResponsesURL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, 0, err
	}
	for k, vs := range codexHeaders(acct) {
		for _, v := range vs {
			req2.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req2)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 200))
	}
	return resp.Body, resp.StatusCode, nil
}
