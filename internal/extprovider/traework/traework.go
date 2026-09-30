// Package traework TraeWork（字节 TRAE SOLO CN）通道：会话式对话。
//
// 协议事实来自公开实现（wicm84266964/Buddy2api 的 traework provider，MIT）。
//
// ── 与 Trae 的区别 ───────────────────────────────────────────
//
// 这是**另一个产品**：Trae 是 IDE 的 SOLO 通道（单次 llm_utils_chat），
// TraeWork 是工作台（**会话式**）。两者端点、协议、凭据都不通用。
//
// ── 一轮对话三步 ─────────────────────────────────────────────
//
//  1. POST {agent}/api/remote/v1/chat_sessions        → {data:{chat_session_id}}
//  2. GET  {agent}/api/remote/v1/chat_sessions/{sid}/events   ← SSE 事件流
//  3. POST {agent}/api/remote/v1/chat_sessions/{sid}/messages  → 发消息
//
// **第 2 步必须先开**：事件流是独立的长连接，消息发出后的回复全从那上面来；
// 先发消息再开流会丢掉开头的帧。
//
// ── 事件解析是**递归收集**，不是固定形状 ─────────────────────
//
// 上游的事件体没有稳定契约（嵌套层级随事件类型变），参考实现的做法是
// **递归遍历 payload**，按节点类型（text / markdown / output_text / answer 取正文，
// thinking / reasoning / thought / chain_of_thought 取思维链）收集，再做去重。
// 这里照搬该策略——写死某一条路径必然漏事件。
package traework

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// AgentAPI 会话与事件域。
	AgentAPI = "https://trae-api-cn.mchost.guru"
	// UGAPI 账号域（签到 / 用量）。
	UGAPI = "https://api.trae.cn"

	SessionsPath = "/api/remote/v1/chat_sessions"
	ModelsPath   = "/api/remote/v1/models"

	CheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	CheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"

	IDEVersion  = "0.1.56"
	SessionMode = "work"

	// AuthStorageKey 官方客户端 storage.json 里的登录态键（手工填凭据时对照）。
	AuthStorageKey = "iCubeAuthInfo://icube.cloudide"
)

// StaticModels 拉不到目录时的兜底（官方客户端观测值）。
var StaticModels = []string{
	"qwen-3.7-plus",
	"Doubao-Seed-2.1-Turbo",
	"DeepSeek-V4-Flash-Official",
	"qwen-3.5",
	"glm-5",
	"glm-5.1",
	"kimi-k2.5",
	"Doubao-Seed-2.0-Code",
}

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒
	UID          string `json:"uid,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

// NeedsRefresh 是否临近过期（提前 5 分钟）。无过期时间不刷新。
func (c *Credential) NeedsRefresh() bool {
	if c.AccessToken == "" || c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(5*time.Minute).UnixMilli() >= c.ExpiresAt
}

/* ── HTTP 客户端 ─────────────────────────────────────────────── */

var httpClient = &http.Client{Timeout: 0, Transport: newTransport()}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

// SetHTTPClient 允许宿主替换（测试注入 mock 上游）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		httpClient = c
	}
}

// SetProxy 给本通道单独设置代理；空串 = 跟随环境变量，再不行直连。
func SetProxy(rawURL string) error {
	tr := newTransport()
	if strings.TrimSpace(rawURL) != "" {
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("代理地址无效（%s）：%w", rawURL, err)
		}
		if u.Host == "" {
			return fmt.Errorf("代理地址缺少主机（%s）", rawURL)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	httpClient = &http.Client{Timeout: 0, Transport: tr}
	return nil
}

// authHeaders 官方客户端形态：Authorization 带 Cloud-IDE-JWT 前缀。
func authHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("User-Agent", "TRAE-SOLO-CN/"+IDEVersion)
	if cred.DeviceID != "" {
		req.Header.Set("x-device-id", cred.DeviceID)
	}
}

/* ── 一轮对话 ────────────────────────────────────────────────── */

// Turn 一轮对话的结果。
type Turn struct {
	Text      string
	Reasoning string
	Usage     json.RawMessage
}

// RunTurn 走完「建会话 → 开事件流 → 发消息 → 收结果」。
//
// onDelta 每收到一段正文就被调一次（流式转发用）；传 nil 表示只要最终结果。
func RunTurn(ctx context.Context, cred *Credential, prompt string, onDelta func(text string)) (*Turn, error) {
	// 1. 建会话
	sid, err := createSession(ctx, cred)
	if err != nil {
		return nil, err
	}

	// 2. 先开事件流（必须先于发消息，否则丢开头的帧）
	type evResult struct {
		turn *Turn
		err  error
	}
	evCh := make(chan evResult, 1)
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	go func() {
		turn, err := readEvents(streamCtx, cred, sid, onDelta)
		evCh <- evResult{turn, err}
	}()

	// 3. 发消息（给事件流一点建立时间，与参考实现一致）
	select {
	case <-time.After(350 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := sendMessage(ctx, cred, sid, prompt); err != nil {
		return nil, err
	}

	// 4. 等结果
	select {
	case r := <-evCh:
		return r.turn, r.err
	case <-time.After(120 * time.Second):
		return nil, fmt.Errorf("等待上游回复超时")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func createSession(ctx context.Context, cred *Credential) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"mode": SessionMode, "auto_create_project": true, "origin": "web",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AgentAPI+SessionsPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	authHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("建会话失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("建会话 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			SessionID string `json:"chat_session_id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", fmt.Errorf("建会话回执解析失败")
	}
	if doc.Code != 0 {
		return "", fmt.Errorf("建会话被拒（code %d）：%s", doc.Code, doc.Msg)
	}
	if doc.Data.SessionID == "" {
		return "", fmt.Errorf("建会话回执缺少 chat_session_id")
	}
	return doc.Data.SessionID, nil
}

func sendMessage(ctx context.Context, cred *Credential, sid, prompt string) error {
	content, _ := json.Marshal([]any{
		map[string]any{"type": "text", "data": map[string]any{"content": prompt}},
	})
	body, _ := json.Marshal(map[string]any{
		"chat_session_id": sid,
		"content":         json.RawMessage(content),
		"model":           "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		AgentAPI+SessionsPath+"/"+sid+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	authHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("发送消息 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if json.Unmarshal(raw, &doc) == nil && doc.Code != 0 {
		return fmt.Errorf("发送消息被拒（code %d）：%s", doc.Code, doc.Msg)
	}
	return nil
}

// readEvents 读事件流直到 `done`。
func readEvents(ctx context.Context, cred *Credential, sid string, onDelta func(string)) (*Turn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		AgentAPI+SessionsPath+"/"+sid+"/events", nil)
	if err != nil {
		return nil, err
	}
	authHeaders(req, cred)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("打开事件流失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("事件流 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}

	turn := &Turn{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	eventName := "message"
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if eventName == "" {
				eventName = "message"
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		// 递归收集（上游事件形状不稳定，写死路径必漏）
		var text, reasoning []string
		collect(payload, &text, &reasoning)
		if s := dedupe(text, seen); s != "" {
			turn.Text += s
			if onDelta != nil {
				onDelta(s)
			}
		}
		if r := dedupe(reasoning, seen); r != "" {
			turn.Reasoning += r
		}
		if u := parseUsage(payload); len(u) > 0 {
			turn.Usage = u
		}
		if eventName == "done" {
			return turn, nil
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return turn, fmt.Errorf("事件流中断：%w", err)
	}
	// 流断了但已经有内容：当作完成（上游不发 done 时也会这样）
	if turn.Text != "" || turn.Reasoning != "" {
		return turn, nil
	}
	return turn, fmt.Errorf("事件流结束但没有收到任何内容")
}

/* ── 事件解析 ────────────────────────────────────────────────── */

// 跳过的事件名（心跳/状态/元数据，不含正文）。
var skipEvents = map[string]bool{
	"heartbeat": true, "status_changed": true, "platform_timing": true,
	"timing_events": true, "token_usage": true, "model_config": true,
	"project_name_message": true, "session_title_message": true,
	"session_icon_message": true, "metadata": true,
}

var skipNodeTypes = map[string]bool{"status": true, "heartbeat": true, "metadata": true}

var reasoningNodeTypes = map[string]bool{
	"thinking": true, "reasoning": true, "thought": true, "chain_of_thought": true,
}

var textNodeTypes = map[string]bool{
	"text": true, "markdown": true, "output_text": true, "answer": true,
}

// recurseKeys 递归时只走这几个键——全走会把工具参数、配置里的字符串也当正文。
var recurseKeys = []string{"messages", "content", "data", "plan_item", "payload"}

// collect 递归收集正文与思维链。
func collect(v any, text, reasoning *[]string) {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			collect(item, text, reasoning)
		}
	case map[string]any:
		nt, _ := t["type"].(string)
		nt = strings.ToLower(strings.TrimSpace(nt))
		if skipNodeTypes[nt] {
			return
		}
		if s, ok := t["text"].(string); ok && s != "" {
			if reasoningNodeTypes[nt] {
				*reasoning = append(*reasoning, s)
			} else if textNodeTypes[nt] || nt == "" {
				*text = append(*text, s)
			}
		}
		if s, ok := t["content"].(string); ok && s != "" && nt == "" {
			*text = append(*text, s)
		}
		for _, k := range recurseKeys {
			if child, ok := t[k]; ok {
				collect(child, text, reasoning)
			}
		}
	}
}

// dedupe 拼接去重（上游会把同一段正文在多个事件里重复发）。
func dedupe(parts []string, seen map[string]bool) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		b.WriteString(p)
	}
	return b.String()
}

func parseUsage(payload map[string]any) json.RawMessage {
	for _, k := range []string{"token_usage", "usage", "usageMetadata"} {
		v, ok := payload[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) == "" {
				continue
			}
			var parsed map[string]any
			if json.Unmarshal([]byte(t), &parsed) != nil {
				continue
			}
			raw, _ := json.Marshal(normalizeUsage(parsed))
			return raw
		case map[string]any:
			raw, _ := json.Marshal(normalizeUsage(t))
			return raw
		}
	}
	return nil
}

// normalizeUsage 上游的 token 统计字段名不一，归一到 OpenAI 口径。
func normalizeUsage(m map[string]any) map[string]any {
	num := func(keys ...string) (float64, bool) {
		for _, k := range keys {
			switch v := m[k].(type) {
			case float64:
				return v, true
			case string:
				var f float64
				if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
					return f, true
				}
			}
		}
		return 0, false
	}
	out := map[string]any{}
	p, okP := num("prompt_tokens", "promptTokens", "input_tokens")
	c, okC := num("completion_tokens", "completionTokens", "output_tokens")
	t, okT := num("total_tokens", "totalTokens")
	if !okP && !okC && !okT {
		return nil
	}
	if okP {
		out["prompt_tokens"] = p
	}
	if okC {
		out["completion_tokens"] = c
	}
	if okT {
		out["total_tokens"] = t
	} else {
		out["total_tokens"] = p + c
	}
	return out
}

/* ── 模型目录 ────────────────────────────────────────────────── */

// Model 模型条目。
type Model struct {
	ID   string
	Name string
}

// ListModels 拉模型目录；失败时回落到静态名单。
func ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, AgentAPI+ModelsPath, nil)
	if err != nil {
		return nil, err
	}
	authHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return staticModels(), fmt.Errorf("拉取模型目录失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return staticModels(), fmt.Errorf("模型目录 HTTP %d", resp.StatusCode)
	}
	mods := parseCatalog(raw)
	if len(mods) == 0 {
		return staticModels(), nil
	}
	return mods, nil
}

func staticModels() []Model {
	out := make([]Model, 0, len(StaticModels))
	for _, id := range StaticModels {
		out = append(out, Model{ID: id, Name: id})
	}
	return out
}

func parseCatalog(raw []byte) []Model {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Model
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			id, _ := t["id"].(string)
			if id == "" {
				id, _ = t["model"].(string)
			}
			if id == "" {
				id, _ = t["name"].(string)
			}
			if id != "" && !seen[id] {
				seen[id] = true
				name, _ := t["display_name"].(string)
				if name == "" {
					name, _ = t["displayName"].(string)
				}
				if name == "" {
					name = id
				}
				out = append(out, Model{ID: id, Name: name})
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

/* ── 签到 ────────────────────────────────────────────────────── */

// CheckinResult 签到结果。
type CheckinResult struct {
	Kind    string // claimed | already-claimed | inactive | failed
	Credit  float64
	Message string
}

// CheckinDaily 领取每日签到积分。
func CheckinDaily(ctx context.Context, cred *Credential) *CheckinResult {
	status, err := checkinStatus(ctx, cred)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: err.Error()}
	}
	if status.Claimed {
		return &CheckinResult{Kind: "already-claimed", Message: "今日已签到"}
	}
	if !status.Claimable {
		msg := status.Message
		if msg == "" {
			msg = "当前无可领签到积分"
		}
		return &CheckinResult{Kind: "inactive", Message: msg}
	}

	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, UGAPI+CheckinClaimPath, bytes.NewReader(body))
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: err.Error()}
	}
	authHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		return &CheckinResult{Kind: "failed", Message: fmt.Sprintf("领取 HTTP %d", resp.StatusCode)}
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if d, ok := doc["data"].(map[string]any); ok {
		doc = d
	}
	credit := floatOf(doc, "credits", "credit", "points", "amount")
	return &CheckinResult{Kind: "claimed", Credit: credit, Message: "签到成功"}
}

type checkinState struct {
	Claimed   bool
	Claimable bool
	Message   string
}

func checkinStatus(ctx context.Context, cred *Credential) (*checkinState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UGAPI+CheckinStatusPath, nil)
	if err != nil {
		return nil, err
	}
	authHeaders(req, cred)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("签到状态 HTTP %d", resp.StatusCode)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("签到状态解析失败")
	}
	if d, ok := doc["data"].(map[string]any); ok {
		doc = d
	}
	st := &checkinState{}
	st.Claimed = boolOf(doc, "claimed", "is_claimed", "checked_in", "today_claimed")
	st.Claimable = boolOf(doc, "claimable", "can_claim", "available")
	if !st.Claimable && !st.Claimed {
		// 没给明确布尔时，有可领数量就算可领
		st.Claimable = floatOf(doc, "credits", "amount") > 0
	}
	st.Message = strOf(doc, "message", "msg")
	return st, nil
}

/* ── 小工具 ──────────────────────────────────────────────────── */

func boolOf(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}

func floatOf(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case string:
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
				return f
			}
		}
	}
	return 0
}

func strOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
