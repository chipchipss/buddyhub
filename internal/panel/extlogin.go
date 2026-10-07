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
//	Trae               本机回调   网关临时监听回环端口 → 浏览器授权后自动跳回 → 无需人工操作
//	Accio              本机回调   同上（PKCE + loopback）
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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/accio"
	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extprovider/qclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extprovider/trae"
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
	// id 会话 ID（putExtLoginSession 登记时回填）。后台自驱轮询只认这个 ID，
	// 醒来第一件事是拿它回表核对会话还在不在。
	id string
	// settled 已经落成账号。前端轮询与后台自驱是同一次登录的两个驱动者，
	// 谁先拿到令牌谁落库，另一个必须**不再重复入池**。
	settled bool
	// pollMu 按会话串行化「问上游」这个动作。两个驱动者同时 Poll 时，各家
	// DeviceFlow 的节流状态字段是裸写的，且令牌换取会撞车。
	// **绝不可在持有 extLoginMu 时取它。**
	pollMu    sync.Mutex
	provider  string
	createdAt time.Time
	// ttl 会话存活窗口；0 = 用默认 extLoginTTL。设备码流程按上游有效期放宽，
	// 见 window()。
	ttl time.Duration
	// retryIn 上游/本地节流要求的下次轮询等待秒数，随 poll 回执带给前端。
	retryIn int
	// errStreak 连续轮询失败次数：单次网络抖动继续轮询，连续失败才判定终态
	// ——否则用户看到的是永远转圈，永远不知道错在哪。
	errStreak int
	// lastNote 上一次轮询报出的中间态。只记状态**变化**，否则每 2–5 秒一条
	// 日志会把整个日志文件淹掉（pending 是常态，本来就不记）。
	lastNote string
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
	// trae：本机回环监听 + 回调结果通道
	traeCtx  *trae.LoginContext
	traeLn   net.Listener
	traeDone chan *trae.Callback
	traeErr  chan error
	// accio：同款本机回调
	accioCtx  *accio.LoginContext
	accioLn   net.Listener
	accioDone chan *accio.Callback
	accioErr  chan error
}

var (
	extLoginMu       sync.Mutex
	extLoginSessions = map[string]*extLoginSession{}
)

// extLoginSettledTTL 已入池会话的回执保留时长。
const extLoginSettledTTL = 10 * time.Minute

// extLoginSettled 记「这个会话已经落成账号了」的回执，键为会话 ID。
//
// 为什么需要：后台自驱轮询可能比前端先一步拿到令牌并入池，会话随即被丢弃；
// 前端下一次 poll 就会撞上「登录会话不存在或已过期，请重新发起」——账号明明
// 已经在池子里，界面却报失败。留一条短期回执，让晚到的 poll 也能拿到
// done:true + 账号信息。
var extLoginSettled = map[string]settledLogin{}

type settledLogin struct {
	provider string
	account  map[string]any
	// fail 终态失败原因。后台先一步问出「授权被拒 / 设备码过期」并丢弃会话时，
	// 前端晚到的 poll 要拿到这句原因，而不是「会话不存在，请重新发起」。
	fail string
	at   time.Time
}

// rememberOutcome 记一次登录终态（入池回执或失败原因），顺带清掉过期的。
func rememberOutcome(id, provider string, account map[string]any, fail string) {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	now := time.Now()
	for k, v := range extLoginSettled {
		if now.Sub(v.at) > extLoginSettledTTL {
			delete(extLoginSettled, k)
		}
	}
	extLoginSettled[id] = settledLogin{provider: provider, account: account, fail: fail, at: now}
}

// settledResult 取该会话的入池回执；没有或已过期返回 nil。
func settledResult(id, provider string) map[string]any {
	return lookupOutcome(id, provider).account
}

// settledFailure 取该会话的终态失败原因；没有则空串。
func settledFailure(id, provider string) string {
	return lookupOutcome(id, provider).fail
}

func lookupOutcome(id, provider string) settledLogin {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	v, ok := extLoginSettled[id]
	if !ok || time.Since(v.at) > extLoginSettledTTL || v.provider != provider {
		return settledLogin{}
	}
	return v
}

func reapExtLoginSessions() {
	now := time.Now()
	for k, s := range extLoginSessions {
		if now.Sub(s.createdAt) > s.window() {
			delete(extLoginSessions, k)
		}
	}
}

// window 这次登录会话允许存活的时长。
//
// 默认 extLoginTTL，但设备码类流程按**上游自己的有效期**放宽：GitHub 给 15 分钟，
// 而面板只留 10 分钟——用户在第 11 分钟点完授权回来，设备码已被我们丢弃，
// 浏览器显示「已连接」而池子里没有账号，正是这类投诉的成因。
func (s *extLoginSession) window() time.Duration {
	if s.ttl > 0 {
		return s.ttl
	}
	return extLoginTTL
}

// deviceLoginWindow 把上游下发的 expires_in（秒）折成会话时长，夹在
// [extLoginTTL, 20 分钟] 内：不短于默认档，也不让会话无限期挂着占内存。
func deviceLoginWindow(expiresIn int) time.Duration {
	if expiresIn <= 0 {
		return extLoginTTL
	}
	w := time.Duration(expiresIn) * time.Second
	if w < extLoginTTL {
		return extLoginTTL
	}
	if w > 20*time.Minute {
		return 20 * time.Minute
	}
	return w
}

// setRetryIn / takeRetryIn 记录上游要求的下次轮询等待秒数（锁内改，轮询会重叠）。
func (s *extLoginSession) setRetryIn(sec int) {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	s.retryIn = sec
}

func (s *extLoginSession) takeRetryIn() int {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	n := s.retryIn
	s.retryIn = 0
	return n
}

// markNote 记一次中间态并返回「值不值得打这行日志」。
//
// 必须锁内做：前端 poll 与后台自驱共用同一个 lastNote，两处裸写是数据竞争
// （go test -race 直接报），而且会互相把对方的状态变化吞掉，日志少一半。
func (s *extLoginSession) markNote(note string) bool {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	if note == "pending" || note == s.lastNote {
		return false
	}
	s.lastNote = note
	return true
}

func getExtLoginSession(id string) *extLoginSession {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	s := extLoginSessions[id]
	if s == nil {
		return nil
	}
	if time.Since(s.createdAt) > s.window() {
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
	s := extLoginSessions[id]
	delete(extLoginSessions, id)
	extLoginMu.Unlock()
	// Trae 的回环监听要随手关掉，否则会话过期后端口一直占着
	if s != nil {
		if s.traeLn != nil {
			_ = s.traeLn.Close()
		}
		if s.accioLn != nil {
			_ = s.accioLn.Close()
		}
	}
}

func putExtLoginSession(s *extLoginSession) string {
	id := newSessionID()
	extLoginMu.Lock()
	reapExtLoginSessions()
	s.id = id
	extLoginSessions[id] = s
	extLoginMu.Unlock()
	return id
}

// startFail 记一行日志再回错误。
//
// 发起阶段的失败（代理不可达、上游拒绝签发设备码、回环端口占用）如果只回 HTTP
// 给前端，**日志里什么都看不到**——事后完全无法定位「用户点了没反应」是卡在哪
// 一步。这类失败本来就低频，逐条留痕的代价可以忽略。
func startFail(w http.ResponseWriter, provider string, status int, msg string) {
	log.Printf("panel: %s 发起登录失败：%s", provider, msg)
	writeErr(w, status, msg)
}

// extLoginStart POST /panel/api/ext/{provider}/login/start —— 发起登录。
func (p *Panel) extLoginStart(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	// 每个平台的发起都留一行：用户报「点了没反应」时，先要能确认这一步到底有没有到。
	log.Printf("panel: %s 发起登录", provider)

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
			startFail(w, provider, http.StatusInternalServerError, "生成设备会话失败："+err.Error())
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
			startFail(w, provider, http.StatusBadGateway, err.Error())
			return
		}
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), flow: flow,
			ttl: deviceLoginWindow(flow.ExpiresIn),
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
		// 后端自己把这轮问完：用户授权后不回面板（关抽屉 / 切页面 / 直接忘了）
		// 也照样入池。见 driveDeviceLogin。
		p.driveDeviceLogin(id)

	case extstore.PCline:
		flow, err := cline.StartDeviceFlow(ctx)
		if err != nil {
			startFail(w, provider, http.StatusBadGateway, err.Error())
			return
		}
		id := putExtLoginSession(&extLoginSession{
			provider: provider, createdAt: time.Now(), clineFlow: flow,
			ttl: deviceLoginWindow(flow.ExpiresIn),
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
		p.driveDeviceLogin(id)

	case extstore.PQClaw:
		flow, err := qclaw.StartLogin(ctx, "")
		if err != nil {
			startFail(w, provider, http.StatusBadGateway, err.Error())
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

	case extstore.PTrae:
		lctx, ln, err := trae.NewLogin(ctx)
		if err != nil {
			startFail(w, provider, http.StatusInternalServerError, err.Error())
			return
		}
		sess := &extLoginSession{
			provider: provider, createdAt: time.Now(),
			traeCtx: lctx, traeLn: ln,
			traeDone: make(chan *trae.Callback, 1),
			traeErr:  make(chan error, 1),
		}
		// 后台等回调：浏览器授权后会跳到本机这个端口
		go func() {
			cb, aerr := trae.AwaitCallback(context.Background(), ln, trae.LoginTTL)
			if aerr != nil {
				sess.traeErr <- aerr
				return
			}
			sess.traeDone <- cb
		}()
		id := putExtLoginSession(sess)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"mode":       "callback",
			"session":    id,
			"auth_url":   lctx.AuthURL,
			"expires_in": int(trae.LoginTTL.Seconds()),
			"hint":       "在浏览器打开链接完成授权，本页会自动接住回调（浏览器需与本机在同一台机器）",
		})

	case extstore.PAccio:
		lctx, ln, err := accio.NewLogin(accio.RegionCN)
		if err != nil {
			startFail(w, provider, http.StatusInternalServerError, err.Error())
			return
		}
		sess := &extLoginSession{
			provider: provider, createdAt: time.Now(),
			accioCtx: lctx, accioLn: ln,
			accioDone: make(chan *accio.Callback, 1),
			accioErr:  make(chan error, 1),
		}
		go func() {
			cb, aerr := accio.AwaitCallback(context.Background(), ln, extLoginTTL)
			if aerr != nil {
				sess.accioErr <- aerr
				return
			}
			sess.accioDone <- cb
		}()
		id := putExtLoginSession(sess)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"mode":       "callback",
			"session":    id,
			"auth_url":   lctx.AuthURL,
			"expires_in": int(extLoginTTL.Seconds()),
			"hint":       "在浏览器打开链接完成授权，本页会自动接住回调（浏览器需与本机在同一台机器）",
		})

	default:
		startFail(w, provider, http.StatusBadRequest, "该平台暂不支持登录入池："+provider)
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
		// 后台自驱可能已经先一步问出终态并丢弃了会话。那时该把**那句结果**给
		// 前端——入池回执，或者失败原因。反过来把「用户拒绝授权」讲成「会话
		// 不存在请重新发起」，是把已知事实藏起来让用户重跑一遍。
		if acct := settledResult(body.Session, provider); acct != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": true, "account": acct})
			return
		}
		if fail := settledFailure(body.Session, provider); fail != "" {
			writeErr(w, http.StatusBadRequest, fail)
			return
		}
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
	case extstore.PCopilot, extstore.PCline:
		// 与后台自驱共用同一个入口，两条驱动路径不会走成两套判据
		cred, done, note = pollDeviceCode(ctx, sess)
	case extstore.PQClaw:
		cred, done, note = completeQClaw(ctx, sess, code)
	case extstore.PTrae:
		cred, done, note = pollTraeCallback(ctx, sess)
	case extstore.PAccio:
		cred, done, note = pollAccioCallback(ctx, sess)
	default:
		writeErr(w, http.StatusBadRequest, "该平台暂不支持登录入池："+provider)
		return
	}

	if !done {
		// pending 是常态（前端每 2–5 秒轮一次），只在状态**变化**时记一行——
		// 「已扫码待确认」「上游瞬时错误」这类中间态是排障时仅有的线索，
		// 而每次都记会把日志淹掉。
		if sess.markNote(note) {
			log.Printf("panel: %s 登录进行中：%s", provider, note)
		}
		resp := map[string]any{"ok": true, "done": false, "status": note}
		if n := sess.takeRetryIn(); n > 0 {
			resp["retry_in"] = n
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if cred == nil {
		// 终态失败：这是「授权完了却没入池」最该看到的一条日志。
		log.Printf("panel: %s 登录失败（会话 %s）：%s", provider, shortID(body.Session), note)
		rememberOutcome(body.Session, provider, nil, note)
		dropExtLoginSession(body.Session)
		writeErr(w, http.StatusBadRequest, note)
		return
	}

	// 落库：与手工添加同一条路径，入池后立即可用。
	acct, err := p.settleLogin(sess, cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dropExtLoginSession(body.Session)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": true, "account": acct})
}

// settleLogin 把一次成功登录落成账号，**幂等认领**：同一次登录有两个驱动者
// （前端轮询 + 后台自驱），谁先拿到令牌谁落库，另一个拿到同一份回执。
// 重复入池会把凭据写成两份、面板出现两个同名账号。
func (p *Panel) settleLogin(s *extLoginSession, cred *extLoginCred) (map[string]any, error) {
	extLoginMu.Lock()
	if s.settled {
		extLoginMu.Unlock()
		if acct := settledResult(s.id, s.provider); acct != nil {
			return acct, nil
		}
		return map[string]any{"id": cred.id, "label": cred.label, "provider": s.provider}, nil
	}
	s.settled = true
	extLoginMu.Unlock()

	raw, err := json.Marshal(cred.cred)
	if err != nil {
		return nil, err
	}
	label := cred.label
	if label == "" {
		label = cred.id
	}
	if err := p.extManager().Add(s.provider, cred.id, label, raw); err != nil {
		extLoginMu.Lock()
		s.settled = false // 没落成了，另一个驱动者还有机会
		extLoginMu.Unlock()
		return nil, err
	}
	acct := map[string]any{
		"id": cred.id, "label": label, "provider": s.provider, "note": cred.note,
	}
	rememberOutcome(s.id, s.provider, acct, "")
	log.Printf("panel: %s 账号已通过登录入池 %s", s.provider, cred.id)
	return acct, nil
}

// pollDeviceCode 设备码类流程的一次轮询（copilot / cline）。
// 前端 poll 与后台自驱共用这一份判据，避免两条路径走成两套语义。
//
// 按会话串行：两个驱动者可能同时问上游，而各家 DeviceFlow 的 Poll 不承诺
// 并发安全（节流状态字段是裸写的）。同时问还会让令牌换取撞车。
func pollDeviceCode(ctx context.Context, s *extLoginSession) (cred *extLoginCred, done bool, note string) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	switch s.provider {
	case extstore.PCopilot:
		return pollCopilotCode(ctx, s)
	case extstore.PCline:
		return pollClineCode(ctx, s)
	}
	return nil, false, "pending"
}

// driveDeviceLogin 让**后端**替用户把设备码流程问到底。
//
// 为什么必须后端自驱：入池原来只由前端 poll 驱动，而前端一停（关掉添加账号
// 抽屉会调 stopExtAddTimers、切走页面、甚至只是用户授权完没回来）就没人再问
// 上游。设备码在 GitHub 那边照样被接受——浏览器显示「Congratulations, you're
// all set!」，回来面板却说「账号不存在」。这条差距不是重试间隔能修的：只要
// 驱动者在浏览器里，登录成功就仍然是「看用户回不回来」。
//
// 只做设备码两家用（copilot / cline）：其余平台的完成动作各有各的一次性
// 凭据（QClaw 要用户贴回 code、Trae/Accio 走本机回环回调），后台代问会把
// 那些流程搞成竞态。
func (p *Panel) driveDeviceLogin(id string) {
	go func() {
		for {
			// 先睡再问：刚发起时上游必然回 authorization_pending，
			// 立刻问一次只是白加一发请求。
			time.Sleep(deviceDriveWait(id))

			sess := getExtLoginSession(id)
			if sess == nil {
				return // 前端已入池 / 已终态 / 会话过期
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cred, done, note := pollDeviceCode(ctx, sess)
			cancel()

			if done {
				if cred == nil {
					log.Printf("panel: %s 登录失败（会话 %s，后台轮询）：%s", sess.provider, shortID(id), note)
					// 留回执：前端下一次 poll 会撞上会话已消失，那时该看到
					// 「用户拒绝授权」这句原因，而不是「请重新发起」。
					rememberOutcome(id, sess.provider, nil, note)
					dropExtLoginSession(id)
					return
				}
				if _, err := p.settleLogin(sess, cred); err != nil {
					log.Printf("panel: %s 后台入池失败：%v", sess.provider, err)
					rememberOutcome(id, sess.provider, nil, "入池失败："+err.Error())
				}
				dropExtLoginSession(id)
				return
			}
			if sess.markNote(note) {
				log.Printf("panel: %s 登录进行中（后台轮询）：%s", sess.provider, note)
			}
		}
	}()
}

// deviceDriveWait 后台下一轮询问的间隔：上游 interval 与本地节流取大者。
// 会话已经没了就退到最小值——调用方醒来会立刻回表核对并退出。
//
// 字段必须在**一次持锁**里直接读：改调用任何自带 extLoginMu 的方法都是锁内
// 取锁，自驱循环当场死锁。
func deviceDriveWait(id string) time.Duration {
	extLoginMu.Lock()
	defer extLoginMu.Unlock()
	s := extLoginSessions[id]
	if s == nil {
		return time.Second
	}
	sec := s.retryIn
	switch s.provider {
	case extstore.PCopilot:
		if s.flow != nil && s.flow.Interval > 0 && sec < s.flow.Interval {
			sec = s.flow.Interval
		}
	case extstore.PCline:
		if s.clineFlow != nil && s.clineFlow.Interval > 0 && sec < s.clineFlow.Interval {
			sec = s.clineFlow.Interval
		}
	}
	d := time.Duration(sec) * time.Second
	if d < time.Second {
		d = time.Second
	}
	return d
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
	got, status, err := qoder.New().Poll(ctx, sess.device)
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
		// 带上上游原始状态：第一次轮询必然留一行日志，用来区分
		// 「上游说还没授权」和「我们压根没在轮询」。
		return nil, false, "pending（上游 " + status + "）"
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
		// 节流提示带回去：前端固定 2s 一轮会把上游问烦（一直回 slow_down，永远换
		// 不到 token），这里告诉它下次至少等多久。
		if res.RetryIn > 0 {
			sess.setRetryIn(res.RetryIn)
		}
		// 把上游原始状态带进 note：lastNote 只在**变化**时记日志，因此第一次
		// 轮询必然留一行——用户报「授权完了却一直显示等待授权」时，先要能区分
		// 「上游说还没授权」和「我们压根没在轮询」。
		if res.Status == "throttled" {
			return nil, false, "pending（本地节流，这一轮没问上游）"
		}
		return nil, false, "pending（上游 " + res.Status + "）"
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

// pollTraeCallback 检查本机回调是否已到达（不阻塞轮询）。
func pollTraeCallback(ctx context.Context, sess *extLoginSession) (cred *extLoginCred, done bool, note string) {
	select {
	case cb := <-sess.traeDone:
		c, err := trae.Complete(ctx, sess.traeCtx, cb)
		if err != nil {
			// 把原始 query 一起记下来：上游改回调形状时，只有原文能看出
			// 解析器漏了哪个键（解析器认不出的东西全在这里）。
			log.Printf("panel: trae 换证失败：%v（回调原文 %q）", err, head(cb.RawQuery, 300))
			return nil, true, "Trae 授权失败：" + err.Error()
		}
		id := c.UID
		if id == "" {
			id = "trae-" + head(newSessionID(), 8)
		}
		label := "Trae"
		if c.Nickname != "" {
			label = "Trae " + c.Nickname
		}
		return &extLoginCred{id: id, label: label, note: "Trae SOLO", cred: *c}, true, ""
	case err := <-sess.traeErr:
		return nil, true, "等待授权回调失败：" + err.Error()
	default:
		return nil, false, "等待浏览器授权…"
	}
}

// pollAccioCallback 检查 Accio 的本机回调是否已到达。
func pollAccioCallback(ctx context.Context, sess *extLoginSession) (cred *extLoginCred, done bool, note string) {
	select {
	case cb := <-sess.accioDone:
		c, err := accio.Complete(ctx, sess.accioCtx, cb)
		if err != nil {
			return nil, true, "Accio 授权失败：" + err.Error()
		}
		id := c.UserID
		if id == "" {
			id = c.Email
		}
		if id == "" {
			id = "accio-" + head(newSessionID(), 8)
		}
		label := "Accio"
		if c.Email != "" {
			label = "Accio " + c.Email
		} else if c.Name != "" {
			label = "Accio " + c.Name
		}
		return &extLoginCred{id: id, label: label, note: "Accio 浏览器授权", cred: *c}, true, ""
	case err := <-sess.accioErr:
		return nil, true, "等待授权回调失败：" + err.Error()
	default:
		return nil, false, "等待浏览器授权…"
	}
}
