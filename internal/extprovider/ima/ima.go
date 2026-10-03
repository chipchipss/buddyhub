// Package ima 腾讯 ima.copilot（AI 知识管家）通道：Cookie 登录 + 会话式问答。
//
// 协议事实来自 ima2api（aishen1 fork，Node 实现）与本包对齐移植：
//
//	POST https://ima.qq.com/cgi-bin/session_logic/init_session   建会话
//	    {"env_info":{"inter_type":2,"robot_type":10000},"name":…,"msgs_limit":20}
//	    → data.session_id
//	POST https://ima.qq.com/cgi-bin/assistant/qa                 问答（SSE）
//	    {"session_id","robot_type":10000,"question","question_type":2,
//	     "client_id":<uuid>,"model_info":{"model_type":N,"model_id":"official_N"}}
//
// 鉴权（两个头，Cookie 从浏览器 F12 抄一次即可）：
//
//	x-ima-cookie: <完整 cookie，含 IMA-TOKEN / IMA-UID>
//	x-ima-bkn:    <java 风格 djb2 哈希(IMA-TOKEN) & 0x7fffffff>
//
// ── 三处与其它通道不同（照抄会错）──────────────────────────────
//  1. **会话制**：每次问答前要先 init_session 拿 session_id（20 条消息上限，
//     超限回 code=51）；会话按「账号+对话」缓存 30 分钟。
//  2. **业务错误藏在 200 的 SSE 流里**（code=41 登录过期 / 600001 被踢 /
//     5|51 会话无效），不能只看 HTTP 状态码。
//  3. **model_info 是 (type, id) 二元组**，不是模型名字符串——清单见 models.go。
package ima

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// BaseHost ima Web 端基址。
	BaseHost = "https://ima.qq.com"

	// initSessionPath 建会话。
	initSessionPath = "/cgi-bin/session_logic/init_session"
	// qaPath 问答（SSE）。
	qaPath = "/cgi-bin/assistant/qa"
)

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	// Cookie 完整的 x-ima-cookie 值（浏览器 F12 复制，须含 IMA-TOKEN 与 IMA-UID）。
	Cookie string `json:"cookie"`
	// RefreshToken IMA-REFRESH-TOKEN 的值；Cookie 里没有时留空（则不自动续期）。
	// 手工添加的 cookie 通常自带该字段，故自动续期对绝大多数账号开箱可用。
	RefreshToken string `json:"refresh_token,omitempty"`
	// UserID IMA-UID 的值（展示用）。
	UserID string `json:"user_id,omitempty"`
	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`
	// LastRefresh 最近一次续期成功的时刻（秒）；由续期循环维护。
	LastRefresh int64 `json:"last_refresh,omitempty"`
	// TokenValidTime 上游下发的 token 有效秒数（缺省 7200）；续期后用于判断下次何时到期。
	TokenValidTime int64 `json:"token_valid_time,omitempty"`
}

// refreshPath 续期端点（换新 IMA-TOKEN）。
const refreshPath = "/auth_login/refresh"

// tokenTTL 缺省有效期（秒）。上游 client 口径是 7200。
const tokenTTL = 7200

// refreshSkew 提前多久续期（秒）。留 10 分钟余量，避免请求正好压在到期边界上。
const refreshSkew = 600

// NeedsRefresh 是否该续期。
//
// **没有 refresh_token 就不续**——此时只能人工重抓 cookie，续期无从谈起（也不该
// 让每个请求都去打一次注定失败的续期接口）。有 refresh_token 时按
// 「上次续期 + 有效期 - 余量」判断；从未续期过（LastRefresh=0）且拿不到有效秒数
// 时保守视为**不需要**——刚添加的 cookie 通常还有效，避免每次首请求都续期。
func (c *Credential) NeedsRefresh() bool {
	rt := strings.TrimSpace(c.RefreshToken)
	if rt == "" {
		rt = cookieField(c.Cookie, "IMA-REFRESH-TOKEN")
	}
	if rt == "" {
		return false // 无续期凭据：只能人工重抓
	}
	if c.LastRefresh == 0 {
		return false // 刚添加，还没到过期点
	}
	valid := c.TokenValidTime
	if valid <= 0 {
		valid = tokenTTL
	}
	expireAt := c.LastRefresh + valid - refreshSkew
	return time.Now().Unix() >= expireAt
}

// Refresh 换新 IMA-TOKEN，回填 Cookie 里的 IMA-TOKEN 字段。
//
// 协议事实来自 ima2api 的 refreshAccount：`POST /auth_login/refresh`
// 体 `{refresh_token, user_id, registration_id}`；回执 `{code:0, token,
// token_valid_time}`。注意 bkn 必须用**旧** token 算（请求头里的
// x-ima-cookie 仍是旧的），换来的新 token 只写回 cookie 供后续请求使用。
func (c *Credential) Refresh(ctx context.Context) (*Credential, error) {
	rt := strings.TrimSpace(c.RefreshToken)
	if rt == "" {
		rt = cookieField(c.Cookie, "IMA-REFRESH-TOKEN")
	}
	if rt == "" {
		return nil, fmt.Errorf("cookie 里没有 IMA-REFRESH-TOKEN，需重新抓取 cookie")
	}
	uid := strings.TrimSpace(c.UserID)
	if uid == "" {
		uid = cookieField(c.Cookie, "IMA-UID")
	}
	payload, _ := json.Marshal(map[string]string{
		"refresh_token":  rt,
		"user_id":        uid,
		"registration_id": "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseHost+refreshPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers(c) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("续期请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &IMAError{Code: -1, Message: "续期被拒（HTTP " + fmt.Sprint(resp.StatusCode) + "）：refresh_token 可能已失效，需重新抓取 cookie", Auth: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("续期 HTTP %d：%s", resp.StatusCode, truncate(raw, 160))
	}
	var doc struct {
		Code           int    `json:"code"`
		Msg            string `json:"msg"`
		Token          string `json:"token"`
		TokenValidTime int64  `json:"token_valid_time"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("续期回执解析失败：%w", err)
	}
	if doc.Code != 0 {
		return nil, &IMAError{Code: doc.Code, Message: doc.Msg, Auth: true}
	}
	if doc.Token == "" {
		return nil, fmt.Errorf("续期回执不含 token")
	}
	out := *c
	out.Cookie = replaceCookieToken(c.Cookie, doc.Token)
	out.LastRefresh = time.Now().Unix()
	if doc.TokenValidTime > 0 {
		out.TokenValidTime = doc.TokenValidTime
	}
	out.RefreshToken = rt
	out.UserID = uid
	return &out, nil
}

// replaceCookieToken 把 cookie 里的 IMA-TOKEN 换掉（没有则追加）。
func replaceCookieToken(cookie, newToken string) string {
	if strings.Contains(cookie, "IMA-TOKEN=") {
		return cookieRegexToken.ReplaceAllString(cookie, "IMA-TOKEN="+newToken)
	}
	return cookie + "; IMA-TOKEN=" + newToken
}

// cookieRegexToken 匹配 cookie 里 IMA-TOKEN 的整个值（惰性到分号或结尾）。
var cookieRegexToken = regexp.MustCompile(`IMA-TOKEN=[^;]*`)

// bkn 计算 x-ima-bkn：djb2 变体哈希（与 ima2api 的 calcBkn 逐字对齐）。
//
//	h = 5381; for each ch: h += (h << 5) + ch; return h & 0x7fffffff
func bkn(token string) string {
	h := int64(5381)
	for _, ch := range token {
		h += (h << 5) + int64(ch)
	}
	return fmt.Sprintf("%d", h&0x7fffffff)
}

// cookieField 从 cookie 串里取某个键的值。
func cookieField(cookie, key string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.Index(part, "="); eq > 0 && part[:eq] == key {
			return part[eq+1:]
		}
	}
	return ""
}

// headers 请求头（对齐 ima2api 的 headersFor）。
func headers(cred *Credential) map[string]string {
	token := cookieField(cred.Cookie, "IMA-TOKEN")
	return map[string]string{
		"from_browser_ima": "1",
		"x-ima-cookie":     cred.Cookie,
		"x-ima-bkn":        bkn(token),
		"referer":          BaseHost,
		"origin":           BaseHost,
		"User-Agent":       "okhttp/4.12.0",
		"Content-Type":     "application/json; charset=utf-8",
	}
}

// httpClient 共享客户端（SetHTTPClient 可替换，测试注入 mock）。
var httpClient = &http.Client{Timeout: 0, Transport: newTransport()}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
	}
}

// SetHTTPClient 替换包级 HTTP 客户端（nil 忽略）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		httpClient = c
	}
}

// postJSON 发一次 JSON POST 并解析 {code, msg, data} 信封。
// code != 0 返回 IMAError（业务错误，HTTP 仍 200）。
//
// 诊断：把原始响应体（脱敏前 240 字节）挂在 error 上——「init_session 未返回
// session_id」曾让人对着一个没有原因的提示猜了 N 轮。上游实际回 code=0 + 有
// session_id，问题出在 Go 侧（见 InitSession 注释），原始报文一摆出来立判。
func postJSON(ctx context.Context, path string, body any, cred *Credential) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseHost+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers(cred) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &IMAError{Code: -1, Message: "登录态失效（HTTP " + fmt.Sprint(resp.StatusCode) + "）", Auth: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(raw, 160))
	}
	var doc struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("响应解析失败：%w", err)
	}
	if doc.Code != 0 {
		return nil, &IMAError{Code: doc.Code, Message: doc.Msg, Auth: isAuthCode(doc.Code)}
	}
	return doc.Data, nil
}

// postJSONTop 发一次 JSON POST，返回**完整**响应体（不解包 `data` 信封）。
//
// init_session 的 `session_id` 在**顶层**（`{"code":0,"session_id":…}`），不像
// 其它接口藏在 `data` 里——用 postJSON 解包会把 session_id 整个丢掉，导致
// 「init_session 未返回 session_id」却 code=0 的怪象（cookie 其实没问题，换 cookie
// 也没用，因为病因在解包）。code != 0 仍返回 IMAError。
func postJSONTop(ctx context.Context, path string, body any, cred *Credential) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseHost+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers(cred) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &IMAError{Code: -1, Message: "登录态失效（HTTP " + fmt.Sprint(resp.StatusCode) + "）", Auth: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(raw, 160))
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("响应解析失败：%w", err)
	}
	if doc.Code != 0 {
		return nil, &IMAError{Code: doc.Code, Message: doc.Msg, Auth: isAuthCode(doc.Code)}
	}
	return raw, nil
}

// IMAError ima 的业务错误（HTTP 200 但 code != 0）。
type IMAError struct {
	Code    int
	Message string
	Auth    bool // true = 凭据失效，该换账号
}

func (e *IMAError) Error() string {
	return fmt.Sprintf("ima code=%d：%s", e.Code, e.Message)
}

// isAuthCode 上游明确说「该重新登录了」的码（ima2api 的 isAuthError）。
func isAuthCode(code int) bool {
	switch code {
	case 41, 600001, 5, 51:
		return true
	}
	return false
}

// InitSession 建会话，返回 session_id。
//
// msgs_limit=20 是 ima 的硬上限（超过回 code=51）。
//
// 响应里的 session_id 在**顶层**（`{"code":0,"session_id":…}`），不在 `data`
// 信封里——故走 postJSONTop 取完整响应体，再解析顶层字段。用 postJSON（解包
// data）会丢掉 session_id，表现为「code=0 却没返回 session_id」。
//
// 解析同时认顶层与 `data` 两种形状（真实上游回顶层；部分历史版本/测试 mock
// 可能塞在 data 信封里），命中即取，避免上游偶尔改包结构就整条链路挂死。
func InitSession(ctx context.Context, question string, cred *Credential) (string, error) {
	name := question
	if len([]rune(name)) > 50 {
		name = string([]rune(name)[:50])
	}
	if strings.TrimSpace(name) == "" {
		name = "新对话"
	}
	raw, err := postJSONTop(ctx, initSessionPath, map[string]any{
		"env_info":   map[string]any{"interact_type": 2, "robot_type": 10000},
		"name":       name,
		"msgs_limit": 20,
	}, cred)
	if err != nil {
		return "", err
	}
	// 顶层优先；取不到再看 data 信封。
	var doc struct {
		SessionID string          `json:"session_id"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("init_session 解析失败：%w（raw=%s）", err, truncate(raw, 120))
	}
	if doc.SessionID != "" {
		return doc.SessionID, nil
	}
	var inner struct {
		SessionID string `json:"session_id"`
	}
	if len(doc.Data) > 0 {
		_ = json.Unmarshal(doc.Data, &inner)
	}
	if inner.SessionID != "" {
		return inner.SessionID, nil
	}
	return "", fmt.Errorf("init_session 未返回 session_id（raw=%s）", truncate(raw, 120))
	return doc.SessionID, nil
}

// qaEvent 一次 SSE 推送。
//
// ima 的 SSE 是 `event: <名>\ndata: <json>` 形态；事件名**全大写**：
//
//	正文流：TEXT_DELTA 等（文本字段名漂过多个版本，见 eventText）
//	结束哨兵：COMPLETED / CLOSE / INNER_EXCEPTION / ERROR / FAILED
type QAEvent struct {
	Event string // 事件名（全大写）
	Data  string // data: 行的原文（可能不是合法 JSON——有 tryRepair 兜底）
}

// controlEvents 结束哨兵（ima2api 的 CONTROL_EVENTS 同表）。
var controlEvents = map[string]bool{
	"COMPLETED": true, "CLOSE": true, "INNER_EXCEPTION": true, "ERROR": true, "FAILED": true,
}

// IsControl 该事件是否结束流。
func (e *QAEvent) IsControl() bool { return controlEvents[e.Event] }

// IsError 该事件是否表示失败（INNER_EXCEPTION/ERROR/FAILED → 建新会话重试）。
func (e *QAEvent) IsError() bool {
	return e.Event == "INNER_EXCEPTION" || e.Event == "ERROR" || e.Event == "FAILED"
}

// Text 提取事件里的正文文本。
//
// ima 的正文**不在事件顶层**：QA 流的正文事件是 STRUCTURED_BLOCK，data 形如
// `{"Type":"blockMessage","Data":{"text_message":{"Text":"…"}}}`——要钻进
// `Data` 嵌套才拿得到 Text。旧实现只在顶层找 Text/text/Content 等字段，
// 对 blockMessage 恒空（表现是 200 但 content 是 ""，看着像"回答为空"）。
//
// 策略：顶层 + `Data` 一层 + `Data` 里的 `text_message`（兼容其它块类型的
// 直接 Text 字段）逐个探测；命中即返回。loading / 空块 / 建议问等非正文事件
// 自然落空（提取不到就跳过，见桥接侧 controlEvents 过滤）。
func (e *QAEvent) Text() string {
	raw := strings.TrimSpace(e.Data)
	if raw == "" {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		doc = tryRepairJSON(raw)
	}
	if doc == nil {
		return ""
	}
	// 候选对象：顶层、Data、Data.text_message（ima blockMessage 的正文落点）。
	candidates := []map[string]any{doc}
	if dataObj, ok := doc["Data"].(map[string]any); ok {
		candidates = append(candidates, dataObj)
		if tm, ok := dataObj["text_message"].(map[string]any); ok {
			candidates = append(candidates, tm)
		}
	}
	// 文本字段名漂过多个版本，逐个探测。
	for _, cand := range candidates {
		for _, k := range []string{"Text", "text", "Content", "content", "Delta", "delta",
			"Msg", "msg", "reply", "Reply", "answer", "Answer"} {
			if v, ok := cand[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// tryRepairJSON 尽力从损坏的 JSON 里挖文本（ima2api 的同名兜底）。
// 保守起见只做一种修复：剥掉尾部残缺的 `,"xxx"` 片段再试一次。
func tryRepairJSON(raw string) map[string]any {
	if i := strings.LastIndex(raw, `,"`); i > 0 {
		var doc map[string]any
		if json.Unmarshal([]byte(raw[:i]+"}"), &doc) == nil {
			return doc
		}
	}
	return nil
}

// ScanSSE 逐块解析 ima 的 SSE 流（按空行分块，块内 event:/data: 行）。
//
// 与标准 SSE 的差别：ima 的 data: 行**不保证**是合法 JSON（偶发截断），
// 由 qaEvent.eventText 的 tryRepair 兜底；这里只负责切块与字段提取。
func ScanSSE(r io.Reader, fn func(QAEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event string
	var dataLines []string
	flush := func() bool {
		if event == "" && len(dataLines) == 0 {
			return true
		}
		ev := QAEvent{Event: event, Data: strings.Join(dataLines, "\n")}
		event, dataLines = "", nil
		return fn(ev)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line[len("data:"):], " "))
		case line == "data":
			dataLines = append(dataLines, "")
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

// AskStream 一次完整问答：建/复用会话 → 发问 → 返回上游 SSE 流。
//
// 会话满（INNER_EXCEPTION）由**调用方**处理重试——参考实现的做法是重建会话
// 再问一次；网关侧的首轮封装在 server 桥里做（那里拿得到原始问题）。
func AskStream(ctx context.Context, convID, accountID, question string, m Model, cred *Credential) (*http.Response, error) {
	sessionID := CachedSession(convID, accountID)
	if sessionID == "" {
		var err error
		sessionID, err = InitSession(ctx, question, cred)
		if err != nil {
			return nil, err
		}
		PutSession(convID, accountID, sessionID)
	}
	return QAStream(ctx, sessionID, question, m, cred)
}

// QAStream 发一次问答，返回 SSE 流（调用方负责 Close）。
func QAStream(ctx context.Context, sessionID, question string, m Model, cred *Credential) (*http.Response, error) {
	payload, _ := json.Marshal(map[string]any{
		"session_id":    sessionID,
		"robot_type":    10000,
		"question":      question,
		"question_type": 2,
		"command_info":  map[string]any{"question_info": map[string]any{}},
		"client_id":     newUUID(),
		"model_info":    map[string]any{"model_type": m.Type, "model_id": m.ID},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseHost+qaPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers(cred) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("问答请求失败：%w", err)
	}
	return resp, nil
}

// newUUID RFC 4122 v4。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst, b[:4])
	dst[8], dst[13], dst[18], dst[23] = '-', '-', '-', '-'
	hex.Encode(dst[9:], b[4:6])
	hex.Encode(dst[14:], b[6:8])
	hex.Encode(dst[19:], b[8:10])
	hex.Encode(dst[24:], b[10:16])
	return string(dst)
}

// sessionCache 会话缓存（账号+对话 → session_id，30 分钟 TTL）。
var (
	sessMu    sync.Mutex
	sessCache = map[string]sessEntry{}
)

type sessEntry struct {
	id string
	at time.Time
}

// CachedSession 取缓存的会话（无则空串）。
func CachedSession(convID, accountID string) string {
	sessMu.Lock()
	defer sessMu.Unlock()
	now := time.Now()
	for k, e := range sessCache {
		if now.Sub(e.at) > 30*time.Minute {
			delete(sessCache, k)
		}
	}
	if e, ok := sessCache[accountID+"::"+convID]; ok {
		e.at = now
		sessCache[accountID+"::"+convID] = e
		return e.id
	}
	return ""
}

// PutSession 写入会话缓存。
func PutSession(convID, accountID, sessionID string) {
	sessMu.Lock()
	defer sessMu.Unlock()
	sessCache[accountID+"::"+convID] = sessEntry{id: sessionID, at: time.Now()}
}

// DropSession 丢弃会话缓存（会话满/失效时重建用）。
func DropSession(convID, accountID string) {
	sessMu.Lock()
	defer sessMu.Unlock()
	delete(sessCache, accountID+"::"+convID)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
