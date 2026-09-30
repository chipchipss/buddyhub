package panel

// extlogin.go 外部平台的「登录并入池」流程（区别于手工粘贴凭据）。
//
// 三个平台在协议层早就实现了登录，但一直没接到面板，用户只能去客户端本地
// 文件里手抄 token——这就是「没有实现加入账号池」。这里把它们统一接上：
//
//	小浣熊（raccoon）  微信扫码   生成 code → 面板出二维码 → 轮询扫码结果
//	Qoder（qoder）     设备授权   PKCE 授权 URL → 浏览器完成 → 轮询取 token
//	Copilot            设备码     申请设备码 → GitHub 输入 → 轮询兑换 token
//	Cline              设备码     WorkOS 设备码 → 浏览器确认 → 轮询（再登记换令牌）
//	QClaw              扫码回填   面板出微信二维码 → 扫码授权 → 用户把回调里的 code 贴回来
//
// 都是两段式交互，无法在一次 HTTP 往返里完成，故用内存会话：
//
//	POST /panel/api/ext/{provider}/login/start → {mode, session, ...}
//	POST /panel/api/ext/{provider}/login/poll  → {done:false,status} | {done:true,account}
//
// 登录成功后凭据直接落进 extstore，与手工添加走同一条入库路径。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extprovider/qclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// extLoginTTL 一次登录会话的有效期（扫码/授权超过即作废）。
const extLoginTTL = 10 * time.Minute

// maxLoginErrStreak 连续轮询失败多少次才判定登录失败（单次抖动不算）。
const maxLoginErrStreak = 5

// newSessionID 生成登录会话标识（16 字节随机 hex）。
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

type extLoginSession struct {
	provider  string
	createdAt time.Time
	// errStreak 连续轮询失败次数：单次网络抖动继续轮询，连续失败才判定终态
	// ——否则用户看到的是永远转圈，永远不知道错在哪。
	errStreak int
	// raccoon
	qrCode string
	// qoder
	device *qoder.DeviceSession
	// copilot
	flow *copilot.DeviceFlow
	// cline
	clineFlow *cline.DeviceFlow
	// qclaw
	qclawFlow *qclaw.LoginFlow
}

var (
	extLoginMu       sync.Mutex
	extLoginSessions = map[string]*extLoginSession{}
)

func reapExtLoginSessions() {
	cutoff := time.Now().Add(-extLoginTTL)
	for k, s := range extLoginSessions {
		if s.createdAt.Before(cutoff) {
			delete(extLoginSessions, k)
		}
	}
}

func getExtLoginSession(id string) *extLoginSession {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	s := extLoginSessions[id]
	if s == nil {
		return nil
	}
	if time.Since(s.createdAt) > extLoginTTL {
		delete(extLoginSessions, id)
		return nil
	}
	return s
}

// bumpErrStreak / resetErrStreak 在锁内改计数：轮询可能重叠（前端 2s 一次、
// 单次请求最长 30s），直接在会话结构上自增是数据竞争。
func (s *extLoginSession) bumpErrStreak() int {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	s.errStreak++
	return s.errStreak
}

func (s *extLoginSession) resetErrStreak() {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	s.errStreak = 0
}

func dropExtLoginSession(id string) {
	extLoginMu.Lock()
	delete(extLoginSessions, id)
	extLoginMu.Unlock()
}

func putExtLoginSession(s *extLoginSession) string {
	id := newSessionID()
	extLoginMu.Lock()
	reapExtLoginSessions()
	extLoginSessions[id] = s
	extLoginMu.Unlock()
	return id
}

// extLoginStart POST /panel/api/ext/{provider}/login/start —— 发起登录。
func (p *Panel) extLoginStart(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	switch provider {
	case extstore.PRaccoon:
		code := raccoon.GenerateQrCode()
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), qrCode: code,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"mode":       "qr",
			"session":    id,
			"qr_url":     raccoon.BuildQrImageUrl(code),
			"expires_in": int(extLoginTTL.Seconds()),
			"hint":       "用微信扫一扫，在手机上确认登录",
		})

	case extstore.PQoder:
		// machineID 留空由协议层生成并写进凭据——它参与服务端设备绑定，
		// 必须持久化，否则续期会失败。
		sess, err := qoder.NewDeviceSession("")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "生成设备会话失败："+err.Error())
			return
		}
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), device: sess,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"mode":       "device",
			"session":    id,
			"auth_url":   sess.BuildAuthUrl(),
			"expires_in": int(extLoginTTL.Seconds()),
			"hint":       "在浏览器打开链接并完成登录授权",
		})

	case extstore.PCopilot:
		flow, err := copilot.StartDeviceFlow(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), flow: flow,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"mode":             "code",
			"session":          id,
			"user_code":        flow.UserCode,
			"verification_uri": flow.VerificationURI,
			"interval":         flow.Interval,
			"expires_in":       flow.ExpiresIn,
			"hint":             "在 GitHub 页面输入设备码并授权",
		})

	case extstore.PCline:
		flow, err := cline.StartDeviceFlow(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), clineFlow: flow,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"mode":             "code",
			"session":          id,
			"user_code":        flow.UserCode,
			"verification_uri": flow.VerificationURI,
			"complete_uri":     flow.CompleteURI,
			"interval":         flow.Interval,
			"expires_in":       flow.ExpiresIn,
			"hint":             "在浏览器打开链接、输入设备码并确认",
		})

	case extstore.PQClaw:
		flow, err := qclaw.StartLogin(ctx, "")
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), qclawFlow: flow,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"mode":       "paste",
			"session":    id,
			"auth_url":   flow.URL,
			"expires_in": int(extLoginTTL.Seconds()),
			"hint":       "用微信扫码授权，然后把跳转后页面地址里的 code 贴回来",
		})

	default:
		writeErr(w, http.StatusBadRequest, "该平台暂不支持登录入池："+provider)
	}
}

// extLoginPoll POST /panel/api/ext/{provider}/login/poll —— 轮询登录状态。
// body: {session}
func (p *Panel) extLoginPoll(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	var body struct {
		Session string `json:"session"`
		// Code QClaw 专用：用户在微信授权页拿到的 code（或整条回调 URL）。
		// 微信把 code 回给腾讯自己的域名，网关截不到，只能让用户贴回来。
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	code := body.Code
	sess := getExtLoginSession(body.Session)
	if sess == nil || sess.provider != provider {
		writeErr(w, http.StatusNotFound, "登录会话不存在或已过期，请重新发起")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var (
		cred *extLoginCred
		done bool
		note string
	)
	switch provider {
	case extstore.PRaccoon:
		cred, done, note = pollRaccoonQr(ctx, sess)
	case extstore.PQoder:
		cred, done, note = pollQoderDevice(ctx, sess)
	case extstore.PCopilot:
		cred, done, note = pollCopilotCode(ctx, sess)
	case extstore.PCline:
		cred, done, note = pollClineCode(ctx, sess)
	case extstore.PQClaw:
		cred, done, note = completeQClaw(ctx, sess, code)
	default:
		writeErr(w, http.StatusBadRequest, "该平台暂不支持登录入池："+provider)
		return
	}

	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": false, "status": note})
		return
	}
	if cred == nil {
		dropExtLoginSession(body.Session)
		writeErr(w, http.StatusBadRequest, note)
		return
	}

	// 落库：与手工添加同一条路径，入池后立即可用。
	raw, err := json.Marshal(cred.cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := cred.label
	if label == "" {
		label = cred.id
	}
	if err := p.extManager().Add(provider, cred.id, label, raw); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dropExtLoginSession(body.Session)

	log.Printf("panel: %s 账号已通过登录入池 %s", provider, cred.id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"done": true,
		"account": map[string]any{
			"id": cred.id, "label": label, "provider": provider, "note": cred.note,
		},
	})
}

// extLoginCred 一次成功登录的产物（凭据 + 账号标识）。
type extLoginCred struct {
	id    string
	label string
	note  string
	cred  any
}

// head 安全取前 n 个字符（短串原样返回，不越界）。
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

/* ── 各平台的轮询实现 ────────────────────────────────────────── */

// pollRaccoonQr 轮询一次微信扫码登录。
func pollRaccoonQr(ctx context.Context, sess *extLoginSession) (cred *extLoginCred, done bool, note string) {
	res := raccoon.New().PollQrLogin(ctx, sess.qrCode)
	switch res.Status {
	case "success":
		exp := res.ExpiresAt
		if exp == 0 {
			exp = raccoon.DecodeJWTExpMs(res.AccessToken)
		}
		uid := raccoon.DecodeJWTUserID(res.AccessToken)
		if uid == "" {
			uid = "raccoon-" + raccoon.TokenDigest(res.AccessToken)
		}
		return &extLoginCred{
			id:    uid,
			label: "小浣熊 " + uid,
			note:  "微信扫码登录",
			cred: raccoon.Credential{
				AccessToken:  res.AccessToken,
				RefreshToken: res.RefreshToken,
				ExpiresAt:    exp,
				UserID:       uid,
			},
		}, true, ""
	case "canceled":
		return nil, true, "扫码已取消，请重新发起登录"
	default: // pending / logging
		return nil, false, res.Status
	}
}

// pollQoderDevice 轮询一次设备授权（nil 凭据 = 尚未授权）。
func pollQoderDevice(ctx context.Context, sess *extLoginSession) (cred *extLoginCred, done bool, note string) {
	got, err := qoder.New().Poll(ctx, sess.device)
	if err != nil {
		n := sess.bumpErrStreak()
		log.Printf("panel: qoder 设备轮询出错（第 %d 次）: %v", n, err)
		if n >= maxLoginErrStreak {
			return nil, true, "Qoder 授权轮询连续失败：" + err.Error()
		}
		return nil, false, "轮询出错，重试中…（" + err.Error() + "）"
	}
	sess.resetErrStreak()
	if got == nil {
		return nil, false, "pending"
	}
	uid := got.UID
	if uid == "" {
		uid = "qoder-" + head(sess.device.MachineID, 8)
	}
	label := "Qoder " + uid
	if got.Nickname != "" {
		label = "Qoder " + got.Nickname
	}
	return &extLoginCred{id: uid, label: label, note: "设备授权登录", cred: *got}, true, ""
}

// pollCopilotCode 轮询一次设备码授权。
func pollCopilotCode(ctx context.Context, sess *extLoginSession) (cred *extLoginCred, done bool, note string) {
	res, err := sess.flow.Poll(ctx)
	if err != nil {
		n := sess.bumpErrStreak()
		log.Printf("panel: copilot 设备码轮询出错（第 %d 次）: %v", n, err)
		if n >= maxLoginErrStreak {
			return nil, true, "GitHub 设备码轮询连续失败：" + err.Error()
		}
		return nil, false, "轮询出错，重试中…（" + err.Error() + "）"
	}
	sess.resetErrStreak()
	if res.Error != "" {
		return nil, true, res.Error
	}
	if !res.Done {
		return nil, false, "pending"
	}
	c := res.Cred
	id := c.Login
	if id == "" {
		id = "copilot-" + head(newSessionID(), 8)
	}
	label := c.Login
	if label == "" {
		label = "GitHub Copilot"
	}
	if c.Plan != "" {
		label += "（" + c.Plan + "）"
	}
	return &extLoginCred{id: id, label: label, note: "GitHub Copilot " + c.Plan, cred: *c}, true, ""
}

// pollClineCode 轮询一次 Cline 的 WorkOS 设备码授权。
//
// 授权成功后上游还会做一次「登记」把 WorkOS 令牌换成 Cline 会话令牌
// （协议层 Poll 内完成）——省掉它拿到的凭据发请求会被拒。
func pollClineCode(ctx context.Context, sess *extLoginSession) (cred *extLoginCred, done bool, note string) {
	res, err := sess.clineFlow.Poll(ctx)
	if err != nil {
		n := sess.bumpErrStreak()
		log.Printf("panel: cline 设备码轮询出错（第 %d 次）: %v", n, err)
		if n >= maxLoginErrStreak {
			return nil, true, "Cline 设备码轮询连续失败：" + err.Error()
		}
		return nil, false, "轮询出错，重试中…（" + err.Error() + "）"
	}
	sess.resetErrStreak()
	if res.Error != "" {
		return nil, true, res.Error
	}
	if !res.Done {
		return nil, false, "pending"
	}
	c := res.Cred
	// 账号 ID 优先用邮箱（稳定且可读），其次上游 userId
	id := firstNonEmptyStr(c.Email, c.AccountID)
	if id == "" {
		id = "cline-" + head(newSessionID(), 8)
	}
	label := "Cline"
	if c.Email != "" {
		label = "Cline " + c.Email
	} else if c.Name != "" {
		label = "Cline " + c.Name
	}
	return &extLoginCred{id: id, label: label, note: "Cline 设备授权", cred: *c}, true, ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// completeQClaw 用用户贴回来的 code 完成微信登录。
//
// 微信把授权码回给腾讯自己的回调域名（security.guanjia.qq.com），网关截不到，
// 所以这一步只能由用户把 code（或整条回调 URL）贴回来——ParseCallback 两种都认。
func completeQClaw(ctx context.Context, sess *extLoginSession, raw string) (cred *extLoginCred, done bool, note string) {
	code, state := qclaw.ParseCallback(raw)
	if code == "" {
		return nil, false, "等待贴回微信授权 code"
	}
	// 回调里的 state 与本次会话不一致说明贴错了（或贴的是上一次的）
	if state != "" && sess.qclawFlow.State != "" && state != sess.qclawFlow.State {
		return nil, true, "code 与本次登录不匹配（可能是上一次的），请重新扫码"
	}
	c, err := qclaw.CompleteLogin(ctx, sess.qclawFlow.GUID, code, sess.qclawFlow.State)
	if err != nil {
		return nil, true, "微信登录失败：" + err.Error()
	}
	id := c.UID
	if id == "" {
		id = "qclaw-" + head(newSessionID(), 8)
	}
	label := "QClaw"
	if c.Nickname != "" {
		label = "QClaw " + c.Nickname
	}
	return &extLoginCred{id: id, label: label, note: "微信扫码登录", cred: *c}, true, ""
}
