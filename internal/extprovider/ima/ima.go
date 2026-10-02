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
	// UserID IMA-UID 的值（展示用）。
	UserID string `json:"user_id,omitempty"`
	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`
	// LastRefresh 最近一次续期成功的时刻（秒）；由续期循环维护。
	LastRefresh int64 `json:"last_refresh,omitempty"`
}

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
func InitSession(ctx context.Context, question string, cred *Credential) (string, error) {
	name := question
	if len([]rune(name)) > 50 {
		name = string([]rune(name)[:50])
	}
	if strings.TrimSpace(name) == "" {
		name = "新对话"
	}
	data, err := postJSON(ctx, initSessionPath, map[string]any{
		"env_info":   map[string]any{"interact_type": 2, "robot_type": 10000},
		"name":       name,
		"msgs_limit": 20,
	}, cred)
	if err != nil {
		return "", err
	}
	var doc struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.SessionID == "" {
		return "", fmt.Errorf("init_session 未返回 session_id")
	}
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

// Text 提取事件里的文本。
//
// data: 的 JSON 里文本字段名漂过多个版本（Text/text/Content/content/Delta/
// delta/Msg/msg/reply/Reply/answer/Answer），逐个探测；JSON 损坏时尽力用
// tryRepairJSON 兜底（ima2api 同款手法——上游偶发截断的 JSON 也能挖出正文）。
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
	for _, k := range []string{"Text", "text", "Content", "content", "Delta", "delta",
		"Msg", "msg", "reply", "Reply", "answer", "Answer"} {
		if v, ok := doc[k].(string); ok && v != "" {
			return v
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
