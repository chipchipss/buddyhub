// Package qoder: chat.go — Qoder 新版协议 chat 转发（api2-v2.qoder.sh，纯 Bearer）
// 与 token 刷新（deviceToken/jobToken/refresh 按 drt-/jrt- 前缀分流）。
//
// 协议对齐 QoderGateway bridge.py/tokens.py（逆向 qoderclicn@1.1.16 实测）：
//
//	chat:  POST https://api2-v2.qoder.sh/model/v1/chat/completions
//	       OpenAI 原生格式透传（messages/tools 原样），仅需
//	       Authorization: Bearer <security_oauth_token (dt-)>
//	       headers: X-Request-ID / X-Session-ID / UA qoder/1.1.16
//	       body.metadata.context = {request_id, request_set_id, session_id,
//	                                task_id: "common", client_type: "qodercli"}
//	       响应为标准 OpenAI chat.completion.chunk SSE（raw_usage 带 token 统计）
//	refresh: POST openapi.qoder.sh/api/v1/deviceToken/refresh（drt-）
//	         POST openapi.qoder.sh/api/v1/jobToken/refresh（jrt-）
//	         body {"refresh_token": rt}；响应含 token/refresh_token/expires_at
package qoder

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// InferBase 新版推理端点（Qoder CLI 现行，性能优于老版 COSY 协议）。
	InferBase      = "https://api2-v2.qoder.sh"
	chatPath       = "/model/v1/chat/completions"
	refreshPathDT  = "/api/v1/deviceToken/refresh" // drt- 前缀（设备码登录签发）
	refreshPathJRT = "/api/v1/jobToken/refresh"    // jrt- 前缀（PAT/service account 兑换签发）
	chatUserAgent  = "qoder/1.1.16"
	chatTimeout    = 300 * time.Second
)

// ChatMeta Qoder 请求上下文（每次对话生成一次，轮转内共享）。
type ChatMeta struct {
	RequestID  string
	SessionID  string
	RequestSet string
}

// BuildChatBody 构造新版协议 body：OpenAI 原生格式透传 + metadata.context。
// incoming 是客户端原始请求体（model/messages/tools/temperature 等全保留）。
func BuildChatBody(incoming []byte, meta ChatMeta) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(incoming, &req); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	if meta.RequestID == "" {
		meta.RequestID = newRequestID()
	}
	if meta.SessionID == "" {
		meta.SessionID = newRequestID()
	}
	if meta.RequestSet == "" {
		meta.RequestSet = meta.RequestID
	}
	req["stream"] = true
	req["stream_options"] = map[string]any{"include_usage": true}
	req["metadata"] = map[string]any{
		"context": map[string]any{
			"request_id":     meta.RequestID,
			"request_set_id": meta.RequestSet,
			"session_id":     meta.SessionID,
			"task_id":        "common",
			"client_type":    "qodercli",
		},
	}
	return json.Marshal(req)
}

// newRequestID UUIDv4（无连字符需求，标准格式即可）。
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = crandReadQ(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ChatStream 发起 Qoder chat（恒流式），返回可读 SSE 流与状态码。
// 调用方负责关闭返回的 ReadCloser。
func (c *Client) ChatStream(ctx context.Context, cred *Credential, body []byte, meta ChatMeta) (io.ReadCloser, int, error) {
	if meta.RequestID == "" {
		meta.RequestID = newRequestID()
	}
	if meta.SessionID == "" {
		meta.SessionID = newRequestID()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, InferBase+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.Bearer())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", chatUserAgent)
	req.Header.Set("X-Request-ID", meta.RequestID)
	req.Header.Set("X-Session-ID", meta.SessionID)

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

// RefreshResult 刷新结果。
type RefreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

// Refresh 刷新凭据：按 refresh_token 前缀自动选择端点（drt- → deviceToken，
// jrt- → jobToken），回写新的 dt-/drt- 对。
func (c *Client) Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	rt := strings.TrimSpace(cred.RefreshToken)
	if rt == "" {
		return nil, fmt.Errorf("无 refresh_token，需重新设备码登录")
	}
	path := refreshPathDT
	if strings.HasPrefix(rt, "jrt-") {
		path = refreshPathJRT
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, OpenAPIBase+path, strings.NewReader(`{"refresh_token":"`+rt+`"}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", chatUserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var parsed struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("刷新响应解析失败: %w", err)
	}
	newTok := strings.TrimSpace(parsed.Token)
	if newTok == "" {
		newTok = strings.TrimSpace(parsed.DeviceToken)
	}
	if newTok == "" {
		return nil, fmt.Errorf("刷新响应缺少 token")
	}
	out := *cred
	out.AccessToken = newTok
	out.SecurityOAuthToken = newTok // 双写同值（与设备码登录口径一致）
	if parsed.RefreshToken != "" {
		out.RefreshToken = parsed.RefreshToken
	}
	return &out, nil
}

// NeedsRefresh 凭据是否临近过期（提前 24h：dt- 有效期约 30 天，宽松续期窗口）。
// 无过期信息（expires_at 缺失）时按 false 处理——不误刷新，失效时靠 401 触发。
func (cred *Credential) NeedsRefresh() bool {
	if cred.ExpireTime <= 0 {
		return false
	}
	return time.Now().UnixMilli() >= cred.ExpireTime-24*60*60*1000
}
