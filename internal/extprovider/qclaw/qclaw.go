// Package qclaw QClaw（腾讯）通道：微信扫码登录 + OpenAI 兼容对话。
//
// 协议事实来自公开实现（wicm84266964/Buddy2api 的 qclaw provider，MIT）与
// **本机实测核对**（域名与路径可达性）。
//
// ── 两条链路，两个域 ─────────────────────────────────────────
//
//	JPRX 业务域   POST https://jprx.m.qq.com/data/{cmd}/forward   登录 / 建 key / 模型列表
//	AIZone 对话域 POST https://mmgrcalltoken.3g.qq.com/aizone/v1/chat/completions
//
// 登录是**微信扫码**（OAuth 授权码），与腾讯其它产品同一套：
//
//	4050 wx_login_state  {guid}                     → {state}
//	  浏览器打开 open.weixin.qq.com/connect/qrconnect?appid=…&state=…
//	4026 wx_login        {guid, code, state}        → {token(JWT), user_info, openclaw_channel_token}
//	4055 create_api_key  {}                         → {key: "sk-…"}
//
// 对话用**建出来的 sk key** 走 Bearer，不是 JWT。
//
// ── 四处「踩空后表现很像没权限」的口径 ───────────────────────
//
//  1. **JPrx-Ctx 是 MD5 拼接**：`rnd=<32位a-z0-9>; date=<秒>; gid=<gid>; sg=md5(body+KEY+rnd+date+gid)`。
//     注意是**先拼 body**，且 date 是秒。签名错时上游回 ret != 0。
//  2. **对话必须带 `X-Conversation-Request-ID`**：不带时上游直接 400
//     （实测口径，桌面代理注入的那几个 HMAC 头在公开 aizone 域上并不需要）。
//  3. **响应要解两层信封**：`{ret, data:{resp:{common:{code}, data:{…}}}}` ——
//     `ret` 与 `common.code` 任一非 0 都是失败，成功的数据在最里层。
//  4. **`X-New-Token` 响应头会轮换 JWT**：拿到了必须回写，否则下次登录态过期。
package qclaw

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// JPRXGateway 业务域（登录 / 建 key / 模型列表）。
	JPRXGateway = "https://jprx.m.qq.com"
	// AIZoneBase 对话域（OpenAI 兼容）。
	AIZoneBase = "https://mmgrcalltoken.3g.qq.com/aizone/v1"

	// WXAppID 微信开放平台应用标识（公开值，非秘密）。
	WXAppID = "wx9d11056dd75b7240"
	// WXQRConnect 微信扫码授权页。
	WXQRConnect = "https://open.weixin.qq.com/connect/qrconnect"
	// WXLoginRedirect 授权后跳转地址（官方客户端用；用户手动复制 code 时也认）。
	WXLoginRedirect = "https://security.guanjia.qq.com/login"

	// JPrxSignatureKey JPRX 签名盐（内嵌在官方客户端里，非秘密）。
	JPrxSignatureKey = "7fcd3045-3171-482b-9be4-0430bf8553b5"

	WebVersion    = "1.4.0"
	ClientVersion = "0.2.36.629"

	// 业务命令号。
	CmdWXLoginState   = "4050"
	CmdWXLogin        = "4026"
	CmdUserInfo       = "4027"
	CmdCreateAPIKey   = "4055"
	CmdRefreshChannel = "4058"
	CmdTodayTokens    = "4075"
	CmdModelList      = "4320"
	CmdTimeSync       = "4629"

	jprxRndChars = "abcdefghijklmnopqrstuvwxyz0123456789"
	jprxRndLen   = 32
)

// StaticModels 上游 4320 拿不到时的兜底名单（官方客户端观测值）。
var StaticModels = []string{
	"default",
	"pool-hy3-preview",
	"pool-deepseek-v4-pro",
	"pool-deepseek-v4-flash",
	"pool-glm-5.2",
	"pool-glm-5.2-night",
	"pool-glm-5.1",
	"pool-kimi-k2.7-code-highspeed",
	"pool-kimi-k2.6",
	"pool-minimax-m3",
	"pool-minimax-m2.7",
}

// Credential 落进 extstore 的凭据形态。
type Credential struct {
	// APIKey 建出来的 sk- key，对话用（Bearer）。
	APIKey string `json:"access_token"`
	// JWT 微信登录拿到的会话令牌（JPRX 业务调用用，且会轮换）。
	JWT string `json:"refresh_token,omitempty"`
	// GUID 设备标识（签名的一部分，登录后不变）。
	GUID string `json:"guid,omitempty"`
	// UID 上游用户 id。
	UID string `json:"uid,omitempty"`
	// Nickname 微信昵称（展示用）。
	Nickname string `json:"nickname,omitempty"`
	// ChannelToken 渠道令牌（建 key 时用）。
	ChannelToken string `json:"channel_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒
}

// NeedsRefresh JWT 是否临近过期（提前 5 分钟）。无过期时间不刷新。
func (c *Credential) NeedsRefresh() bool {
	if c.JWT == "" || c.ExpiresAt == 0 {
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

/* ── 签名 ────────────────────────────────────────────────────── */

// JPrxCtx 生成 JPrx-Ctx 头：`rnd=…; date=…; gid=…; sg=md5(body+KEY+rnd+date+gid)`。
//
// 注意顺序：**先 body**，再盐、rnd、秒级时间戳、gid。签名错时上游回 ret != 0。
func JPrxCtx(body, gid string) string {
	if gid == "" {
		gid = "1"
	}
	rnd := randomRnd()
	date := strconv.FormatInt(time.Now().Unix(), 10)
	sum := md5.Sum([]byte(body + JPrxSignatureKey + rnd + date + gid))
	return fmt.Sprintf("rnd=%s; date=%s; gid=%s; sg=%s", rnd, date, gid, hex.EncodeToString(sum[:]))
}

func randomRnd() string {
	b := make([]byte, jprxRndLen)
	_, _ = rand.Read(b)
	out := make([]byte, jprxRndLen)
	for i, v := range b {
		out[i] = jprxRndChars[int(v)%len(jprxRndChars)]
	}
	return string(out)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

/* ── JPRX 业务调用 ───────────────────────────────────────────── */

// jprxResult 一次业务调用的结果（已解两层信封）。
type jprxResult struct {
	Data     map[string]any
	NewToken string // 响应头 X-New-Token（轮换后的 JWT）
}

// postCmd 发一条 JPRX 业务命令。cred 为 nil 时用匿名身份（登录前用）。
func postCmd(ctx context.Context, cred *Credential, cmd string, extra map[string]any) (*jprxResult, error) {
	payload := map[string]any{"web_version": WebVersion, "web_env": "release"}
	for k, v := range extra {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	gid, uid, jwt := "1", "1", ""
	if cred != nil {
		if cred.GUID != "" {
			gid = cred.GUID
		}
		if cred.UID != "" {
			uid = cred.UID
		}
		jwt = cred.JWT
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		JPRXGateway+"/data/"+cmd+"/forward", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Version", "1")
	req.Header.Set("X-Token", jwt)
	req.Header.Set("X-Guid", gid)
	req.Header.Set("X-Account", uid)
	req.Header.Set("X-Session", "")
	req.Header.Set("JPrx-Ctx", JPrxCtx(string(body), gid))
	if gid != "1" {
		req.Header.Set("X-Qclaw-DeviceToken", gid)
	}
	if jwt != "" {
		req.Header.Set("X-OpenClaw-Token", jwt)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("上游 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("回执不是 JSON（HTTP %d）", resp.StatusCode)
	}
	data, err := unwrapJPRX(doc)
	if err != nil {
		return nil, err
	}
	return &jprxResult{Data: data, NewToken: resp.Header.Get("X-New-Token")}, nil
}

// unwrapJPRX 解两层信封：`{ret, msg, data:{resp:{common:{code,message}, data:{…}}}}`。
//
// `ret` 与 `common.code` 任一非 0 都是失败；成功的数据在最里层。
func unwrapJPRX(doc map[string]any) (map[string]any, error) {
	if ret, ok := numOf(doc["ret"]); ok && ret != 0 {
		msg := firstStr(doc, "msg", "message")
		return nil, fmt.Errorf("jprx ret=%d %s", ret, msg)
	}
	data, _ := doc["data"].(map[string]any)
	if data == nil {
		data = doc
	}
	resp, _ := data["resp"].(map[string]any)
	if resp == nil {
		resp, _ = doc["resp"].(map[string]any)
	}
	if resp == nil {
		return data, nil
	}
	if common, ok := resp["common"].(map[string]any); ok {
		if code, ok := numOf(common["code"]); ok && code != 0 {
			return nil, fmt.Errorf("jprx code=%d %s", code, firstStr(common, "message", "msg"))
		}
	}
	if inner, ok := resp["data"].(map[string]any); ok {
		return inner, nil
	}
	return resp, nil
}

/* ── 微信扫码登录 ────────────────────────────────────────────── */

// LoginFlow 一次进行中的微信扫码登录。
type LoginFlow struct {
	State string
	GUID  string
	URL   string
}

// StartLogin 取微信授权链接（JPRX 4050 拿 state）。
func StartLogin(ctx context.Context, guid string) (*LoginFlow, error) {
	if guid == "" {
		guid = newUUID()
	}
	res, err := postCmd(ctx, &Credential{GUID: guid}, CmdWXLoginState, map[string]any{"guid": guid})
	if err != nil {
		return nil, err
	}
	state := str(res.Data, "state")
	if state == "" {
		return nil, fmt.Errorf("4050 未返回 state")
	}
	redirect := url.QueryEscape(WXLoginRedirect)
	return &LoginFlow{
		State: state,
		GUID:  guid,
		URL: fmt.Sprintf("%s?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_login&state=%s#wechat_redirect",
			WXQRConnect, WXAppID, redirect, url.QueryEscape(state)),
	}, nil
}

// ParseCallback 从用户粘回来的内容里取出 code/state。
// 接受完整回调 URL，也接受裸 code。
func ParseCallback(raw string) (code, state string) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", ""
	}
	if !strings.Contains(text, "://") && !strings.Contains(text, "code=") {
		return text, ""
	}
	u, err := url.Parse(text)
	if err != nil {
		return text, ""
	}
	q := u.Query()
	code = q.Get("code")
	state = q.Get("state")
	if code == "" {
		code = text
	}
	return code, state
}

// CompleteLogin 用授权码换凭据：4026 拿 JWT → 4055 建对话用的 sk key。
func CompleteLogin(ctx context.Context, guid, code, state string) (*Credential, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("请填写微信授权返回的 code")
	}
	if guid == "" {
		guid = newUUID()
	}
	anon := &Credential{GUID: guid}

	res, err := postCmd(ctx, anon, CmdWXLogin, map[string]any{
		"guid": guid, "code": code, "state": state,
	})
	if err != nil {
		return nil, err
	}
	jwt := str(res.Data, "token")
	channelToken := str(res.Data, "openclaw_channel_token")
	user, _ := res.Data["user_info"].(map[string]any)
	uid := firstStr(user, "userId", "user_id")
	nickname := str(user, "nickname")
	if jwt == "" {
		return nil, fmt.Errorf("4026 未返回 token")
	}

	// 对话用的是**建出来的 sk key**，不是 JWT
	session := &Credential{GUID: guid, UID: uid, JWT: jwt, ChannelToken: channelToken}
	keyRes, err := postCmd(ctx, session, CmdCreateAPIKey, nil)
	if err != nil {
		return nil, fmt.Errorf("建 API Key 失败：%w", err)
	}
	apiKey := firstStr(keyRes.Data, "key", "api_key")
	if apiKey == "" {
		return nil, fmt.Errorf("4055 未返回 sk key（登录成功但没拿到对话凭据）")
	}
	if keyRes.NewToken != "" {
		jwt = keyRes.NewToken
	}

	cred := &Credential{
		APIKey: apiKey, JWT: jwt, GUID: guid, UID: uid,
		Nickname: nickname, ChannelToken: channelToken,
	}
	if exp := jwtExpMs(jwt); exp != 0 {
		cred.ExpiresAt = exp
	}
	return cred, nil
}

// jwtExpMs 从 JWT 载荷取 exp（毫秒）；解析失败返回 0。
func jwtExpMs(token string) int64 {
	parts := strings.Split(token, ".")
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

/* ── 模型目录 ────────────────────────────────────────────────── */

// Model 模型条目。
type Model struct {
	ID          string
	Name        string
	Description string
}

// ListModels 拉模型列表（JPRX 4320）；失败时回落到静态名单。
func ListModels(ctx context.Context, cred *Credential) ([]Model, error) {
	res, err := postCmd(ctx, cred, CmdModelList, nil)
	if err != nil {
		return staticModelList(), err
	}
	rows, _ := res.Data["model_status_list"].([]any)
	if rows == nil {
		rows, _ = res.Data["models"].([]any)
	}
	seen := map[string]bool{}
	var out []Model
	for _, row := range rows {
		var id, name, desc string
		switch v := row.(type) {
		case string:
			id, name = v, v
		case map[string]any:
			id = firstStr(v, "id", "model_id")
			name = firstStr(v, "name", "display_id")
			desc = str(v, "description")
		default:
			continue
		}
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		out = append(out, Model{ID: id, Name: name, Description: desc})
	}
	if len(out) == 0 {
		return staticModelList(), nil
	}
	return out, nil
}

func staticModelList() []Model {
	out := make([]Model, 0, len(StaticModels))
	for _, id := range StaticModels {
		out = append(out, Model{ID: id, Name: id})
	}
	return out
}

/* ── 对话（AIZone，OpenAI 兼容） ─────────────────────────────── */

// Chat 发起对话，返回原始响应（上游即 OpenAI 格式，调用方直接透传）。
func Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AIZoneBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyChatHeaders(req, cred)
	return httpClient.Do(req)
}

func applyChatHeaders(req *http.Request, cred *Credential) {
	gid, uid := cred.GUID, cred.UID
	if gid == "" {
		gid = "1"
	}
	if uid == "" {
		uid = "1"
	}
	req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QClaw/"+ClientVersion)
	// 不带这个头上游直接 400（实测口径）
	req.Header.Set("X-Conversation-Request-ID", newUUID())
	req.Header.Set("X-Conversation-ID", newUUID())
	req.Header.Set("X-Conversation-Message-ID", newUUID())
	req.Header.Set("X-QClaw-Version", ClientVersion)
	req.Header.Set("X-Trigger", "webchat")
	req.Header.Set("X-Guid", gid)
	req.Header.Set("X-Account", uid)
	if cred.JWT != "" {
		req.Header.Set("X-OpenClaw-Token", cred.JWT)
	}
}

/* ── 小工具 ──────────────────────────────────────────────────── */

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := str(m, k); v != "" {
			return v
		}
	}
	return ""
}

// numOf JSON 数字在 any 里是 float64；整数也接受。
func numOf(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
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

// ReissueKey 用已有的 JWT 重新建一把对话用的 sk key。
//
// 场景：key 被上游失效（401）而 JWT 还有效。业务域建 key 的响应头
// `X-New-Token` 会轮换 JWT，所以刷新也走单飞（并发刷新会互相作废）。
func ReissueKey(ctx context.Context, cred *Credential) (*Credential, error) {
	if cred.JWT == "" {
		return nil, fmt.Errorf("缺少登录令牌，需重新扫码登录")
	}
	res, err := postCmd(ctx, cred, CmdCreateAPIKey, nil)
	if err != nil {
		return nil, err
	}
	key := firstStr(res.Data, "key", "api_key")
	if key == "" {
		return nil, fmt.Errorf("重新建 key 未返回 sk key")
	}
	out := *cred
	out.APIKey = key
	if res.NewToken != "" {
		out.JWT = res.NewToken
		if exp := jwtExpMs(out.JWT); exp != 0 {
			out.ExpiresAt = exp
		}
	}
	return &out, nil
}
