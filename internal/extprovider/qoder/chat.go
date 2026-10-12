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
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
)

const (
	// InferBase 新版推理端点（Qoder CLI 现行，性能优于老版 COSY 协议）。
	InferBase      = "https://api2-v2.qoder.sh"
	chatPath       = "/model/v1/chat/completions"
	refreshPathDT  = "/api/v1/deviceToken/refresh" // drt- 前缀（设备码登录签发）
	refreshPathJRT = "/api/v1/jobToken/refresh"    // jrt- 前缀（PAT/service account 兑换签发）
	chatUserAgent  = "qoder/1.1.16"
)

// chatTimeout 已废弃（历史上掐断过长 SSE 流）；保留名字仅为兼容参考，
// chat 走 streamClient（无总超时），短调用继续用 HTTP 客户端的 30s。
const chatTimeout = 300 * time.Second

// streamTransport chat 流式专用传输：ResponseHeaderTimeout 充当上游响应头超时
// （QODER_STREAM_TIMEOUT_MS，默认 600s），正文读取不限时——空闲由弹性泵的
// QODER_STALL_TIMEOUT_MS 兜底。fork 的 fetch 计时器同样只在等响应头期间生效。
func newStreamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = time.Duration(envInt("QODER_STREAM_TIMEOUT_MS", 600000, 1000)) * time.Millisecond
	return t
}

// streamClient chat SSE 专用客户端（无总超时）。
var streamClient = &http.Client{Transport: newStreamTransport()}

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

// SignInExpiredError Qoder 明确拒绝 refresh token 导致登录失效。
// 仅此错误设置 authcore.SignInGone{Expired:true}；一般网络/5xx 不得固化 lapsed。
type SignInExpiredError struct {
	Reason string
}

func (e *SignInExpiredError) Error() string { return e.Reason }

func (e *SignInExpiredError) SignInGone() *authcore.SignInGone {
	return &authcore.SignInGone{Reason: e.Reason, Expired: true}
}

// UpstreamError 非 2xx 响应：携带状态码与截断 body，弹性层据此分类。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// ChatStream 发起 Qoder chat（恒流式），返回可读 SSE 流与状态码。
// 调用方负责关闭返回的 ReadCloser。非 2xx 返回 *UpstreamError（Body 已读出）。
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

	client := c.HTTP
	// 共享 30s 总超时客户端会掐断长 SSE 流：chat 一律走无总超时的 streamClient
	//（响应头超时由其 Transport 负责），除非测试显式注入了别的客户端。
	if client == httpClient {
		client = streamClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	return resp.Body, resp.StatusCode, nil
}

// RefreshResult 刷新结果。
type RefreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

// Refresh 保留旧 API：按 refresh token 前缀刷新；调用方只拿普通 error，
// 生命周期敏感的桥接应使用 RefreshForSite（显式区分 SignInGone）。
func (c *Client) Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	return c.RefreshForSite(ctx, SiteGlobal, cred)
}

// RefreshForSite 按 site/凭据模式刷新，并给桥接明确的失效语义。
//
// 来源 magpie packages/qoder/index.mjs L314-335/L1220-1246/L1376-1385：
//   - jrt jobToken refresh：401/403 → SignInExpiredError（expired=true）；其他
//     status/网络错误仅普通 error，不固化 lapsed；成功必须返回 token+refresh_token 对；
//   - CN deviceChat：drt deviceToken refresh，device token 本身即 chat token；
//     401/403 同样 SignInExpiredError；普通/全局账号若 device refresh 仅供账号页，
//     失败不应标 chat 掉号（本函数对非 deviceChat 的 drt refresh 不会标 SignInGone）；
//   - refresh_token 一次性：成功必须返回新 pair，缺任一立即报错，绝不沿用已用旧 token；
//   - SpentCache 缓存旧 refresh token 已刷结果，防并发双刷新把一次性 token 撞废。
func (c *Client) RefreshForSite(ctx context.Context, site Site, cred *Credential) (*Credential, error) {
	if cred == nil {
		return nil, fmt.Errorf("nil Qoder credential")
	}
	// 站点判定：凭据显式 site_id 优先，但保留调用方传入的端点（测试/代理会覆盖
	// OpenAPI host；只修正 ID/名称/deviceChat，不重建常量站点导致端点漂回生产值）。
	if strings.TrimSpace(cred.SiteID) != "" && strings.TrimSpace(cred.SiteID) != strings.TrimSpace(site.ID) {
		resolved := SiteByID(cred.SiteID)
		site.ID, site.Name, site.DeviceChat = resolved.ID, resolved.Name, resolved.DeviceChat
	}
	if strings.TrimSpace(site.ID) == "" {
		site.ID = SiteGlobal.ID
	}
	rt := strings.TrimSpace(cred.RefreshToken)
	if rt == "" {
		return nil, &authcore.SignInGone{Reason: site.Name + ": the sign-in lapsed; sign in again", Expired: false}
	}
	cache := c.spentCache(site)
	if cached, ok := cache.Get(rt); ok {
		if cached.Err != "" {
			return nil, fmt.Errorf("cached refresh failure: %s", cached.Err)
		}
		var fresh Credential
		if err := json.Unmarshal(cached.Cred, &fresh); err != nil {
			return nil, fmt.Errorf("cached refresh credential decode: %w", err)
		}
		return &fresh, nil
	}
	isDevice := cred.DeviceChat || strings.HasPrefix(rt, "drt-")
	path := refreshPathJRT
	if isDevice {
		path = refreshPathDT
	}
	raw, status, err := c.postJSON(ctx, site.OpenAPI+path, map[string]string{"refresh_token": rt})
	if err != nil {
		return nil, err
	}
	fresh, expired, err := credentialAfterRefresh(site, cred, isDevice, status, raw)
	if err != nil {
		return nil, err
	}
	if expired {
		return nil, &SignInExpiredError{Reason: fmt.Sprintf("%s sign-in has expired (refresh HTTP %d)", site.Name, status)}
	}
	encoded, _ := json.Marshal(fresh)
	cache.Put(rt, authcore.SpentEntry{Cred: encoded})
	return fresh, nil
}

func firstSiteID(ids ...string) string {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			return id
		}
	}
	return SiteGlobal.ID
}

// credentialAfterRefresh is the protocol boundary used by production and tests.
// Only job refresh 401/403, or CN deviceChat device-refresh 401/403, is expired.
func credentialAfterRefresh(site Site, cred *Credential, deviceRefresh bool, status int, raw []byte) (*Credential, bool, error) {
	if status != http.StatusOK {
		expired := (status == http.StatusUnauthorized || status == http.StatusForbidden) && (!deviceRefresh || cred.DeviceChat)
		if expired {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("HTTP %d: %s", status, truncate(raw, 200))
	}
	var parsed struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		ExpiresIn    any    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false, fmt.Errorf("refresh response parse: %w", err)
	}
	newToken := strings.TrimSpace(parsed.Token)
	if newToken == "" {
		newToken = strings.TrimSpace(parsed.DeviceToken)
	}
	if newToken == "" || strings.TrimSpace(parsed.RefreshToken) == "" {
		return nil, false, fmt.Errorf("refresh success response missing rotated token pair")
	}
	out := *cred
	out.AccessToken, out.SecurityOAuthToken, out.RefreshToken = newToken, newToken, strings.TrimSpace(parsed.RefreshToken)
	out.ExpireTime = timeMillisAt(map[string]any{"expires_at": parsed.ExpiresAt, "expires_in": parsed.ExpiresIn}, "expires_at", "expires_in")
	if deviceRefresh {
		out.DeviceToken, out.DeviceRefreshToken = newToken, out.RefreshToken
	}
	out.SiteID = site.ID
	return &out, false, nil
}

func (c *Client) postJSON(ctx context.Context, target string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", chatUserAgent)
	client := c.HTTP
	if client == nil {
		client = httpClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, err
}

// spentRefreshCaches 按 site 隔离一次性 refresh-token 结果缓存。
var spentRefreshCaches = struct {
	sync.Mutex
	m map[string]*authcore.SpentCache
}{m: map[string]*authcore.SpentCache{}}

func (c *Client) spentCache(site Site) *authcore.SpentCache {
	id := site.ID
	if id == "" {
		id = "global"
	}
	if c.SpentCaches != nil {
		if cache := c.SpentCaches[id]; cache != nil {
			return cache
		}
	}
	spentRefreshCaches.Lock()
	defer spentRefreshCaches.Unlock()
	if spentRefreshCaches.m[id] == nil {
		spentRefreshCaches.m[id] = authcore.NewSpentCache(authcore.DefaultLeadPolicy().SpentTTL)
	}
	return spentRefreshCaches.m[id]
}

// refreshFlights singleflight 同一已耗用 refresh token 的并发刷新：第二个请求
// 等首个结果，绝不对一次性 token 再打一次上游（magpie spent+lock 语义）。
var refreshFlights = struct {
	sync.Mutex
	m map[string]*refreshFlight
}{m: map[string]*refreshFlight{}}

type refreshFlight struct {
	done chan struct{}
	cred *Credential
	err  error
}

func (c *Client) refreshSingleflight(ctx context.Context, site Site, cred *Credential) (*Credential, error) {
	key := site.ID + "\x00" + cred.RefreshToken
	refreshFlights.Lock()
	if f := refreshFlights.m[key]; f != nil {
		refreshFlights.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			return f.cred, f.err
		}
	}
	f := &refreshFlight{done: make(chan struct{})}
	refreshFlights.m[key] = f
	refreshFlights.Unlock()
	f.cred, f.err = c.refreshUncached(ctx, site, cred)
	refreshFlights.Lock()
	delete(refreshFlights.m, key)
	close(f.done)
	refreshFlights.Unlock()
	return f.cred, f.err
}

func (c *Client) refreshUncached(ctx context.Context, site Site, cred *Credential) (*Credential, error) {
	rt := strings.TrimSpace(cred.RefreshToken)
	if cached, ok := c.spentCache(site).Get(rt); ok {
		if cached.Err != "" {
			return nil, fmt.Errorf("cached refresh failure: %s", cached.Err)
		}
		var got Credential
		if err := json.Unmarshal(cached.Cred, &got); err != nil {
			return nil, err
		}
		return &got, nil
	}
	if cred.DeviceChat {
		return c.refreshDeviceChat(ctx, site, cred)
	}
	fresh, err := c.RefreshForSite(ctx, site, cred)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(fresh)
	c.spentCache(site).Put(rt, authcore.SpentEntry{Cred: encoded})
	return fresh, nil
}

// refreshDeviceChat refreshes CN's device-token chat pair. Its 401/403 marks
// sign-in expired; transient/network errors never do (magpie refreshDevice L1220-1246).
func (c *Client) refreshDeviceChat(ctx context.Context, site Site, cred *Credential) (*Credential, error) {
	rt := firstSiteID(cred.DeviceRefreshToken, cred.RefreshToken)
	if strings.TrimSpace(rt) == "" {
		return nil, &authcore.SignInGone{Reason: site.Name + ": sign-in lapsed; device refresh token missing", Expired: false}
	}
	raw, status, err := c.postJSON(ctx, site.OpenAPI+refreshPathDT, map[string]string{"refresh_token": rt})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, &SignInExpiredError{Reason: fmt.Sprintf("%s sign-in expired (device refresh HTTP %d)", site.Name, status)}
		}
		return nil, fmt.Errorf("device refresh HTTP %d: %s", status, truncate(raw, 200))
	}
	var parsed struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		ExpiresIn    any    `json:"expires_in"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, fmt.Errorf("device refresh response parse failed")
	}
	token := strings.TrimSpace(parsed.Token)
	if token == "" {
		token = strings.TrimSpace(parsed.DeviceToken)
	}
	refresh := strings.TrimSpace(parsed.RefreshToken)
	if token == "" || refresh == "" {
		return nil, fmt.Errorf("device refresh successful response missing rotated token pair")
	}
	out := *cred
	out.AccessToken, out.SecurityOAuthToken = token, token
	out.RefreshToken, out.DeviceToken, out.DeviceRefreshToken = refresh, token, refresh
	out.ExpireTime = timeMillisAt(map[string]any{"expires_at": parsed.ExpiresAt, "expires_in": parsed.ExpiresIn}, "expires_at", "expires_in")
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
