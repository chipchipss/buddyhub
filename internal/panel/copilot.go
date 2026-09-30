package panel

// copilot.go GitHub Copilot 设备流登录面板接线。
//
// 设备流是两段式交互，无法在一次 HTTP 往返里完成，故用内存会话：
//
//	POST /panel/api/ext/copilot/start → {session, user_code, verification_uri, interval, expires_in}
//	POST /panel/api/ext/copilot/poll  → {done} | {done:true, account:{...}} | {error}
//
// 前端拿到 user_code 后引导用户去 github.com/login/device 输入，然后按 interval 轮询。
// 授权成功后凭据直接落进 extstore（provider=copilot），即刻可用于 copilot: 前缀模型。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// copilotSessions 进行中的设备流（进程内；面板重启即失效，符合预期）。
var (
	copilotMu       sync.Mutex
	copilotSessions = map[string]*copilotSession{}
)

type copilotSession struct {
	flow      *copilot.DeviceFlow
	createdAt time.Time
}

// copilotSessionTTL 设备码有效期上限（GitHub 下发通常 900s，这里留冗余）。
const copilotSessionTTL = 20 * time.Minute

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// reapCopilotSessions 清理过期会话（惰性，在 start 时调用）。
func reapCopilotSessions() {
	cutoff := time.Now().Add(-copilotSessionTTL)
	for k, s := range copilotSessions {
		if s.createdAt.Before(cutoff) {
			delete(copilotSessions, k)
		}
	}
}

// extCopilotStart POST /panel/api/ext/copilot/start —— 申请设备码。
func (p *Panel) extCopilotStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	flow, err := copilot.StartDeviceFlow(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	id := newSessionID()
	copilotMu.Lock()
	reapCopilotSessions()
	copilotSessions[id] = &copilotSession{flow: flow, createdAt: time.Now()}
	copilotMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"session":          id,
		"user_code":        flow.UserCode,
		"verification_uri": flow.VerificationURI,
		"interval":         flow.Interval,
		"expires_in":       flow.ExpiresIn,
	})
}

// extCopilotPoll POST /panel/api/ext/copilot/poll —— 轮询授权状态。
// body: {session}
func (p *Panel) extCopilotPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	copilotMu.Lock()
	sess := copilotSessions[body.Session]
	copilotMu.Unlock()
	if sess == nil {
		writeErr(w, http.StatusNotFound, "登录会话不存在或已过期，请重新发起")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res, err := sess.flow.Poll(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if res.Error != "" {
		copilotMu.Lock()
		delete(copilotSessions, body.Session)
		copilotMu.Unlock()
		writeErr(w, http.StatusBadRequest, res.Error)
		return
	}
	if !res.Done {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": false})
		return
	}

	// 授权成功：落库（账号 ID 用 GitHub 用户名，取不到则回退随机串）。
	cred := res.Cred
	raw, err := json.Marshal(cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := cred.Login
	if id == "" {
		id = "copilot-" + newSessionID()[:8]
	}
	label := cred.Login
	if label == "" {
		label = "GitHub Copilot"
	}
	if cred.Plan != "" {
		label += "（" + cred.Plan + "）"
	}
	if err := p.extManager().Add(extstore.PCopilot, id, label, raw); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	copilotMu.Lock()
	delete(copilotSessions, body.Session)
	copilotMu.Unlock()

	log.Printf("panel: Copilot 账号已接入 %s（订阅 %s）", id, cred.Plan)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"done":    true,
		"account": map[string]any{"id": id, "label": label, "plan": cred.Plan},
	})
}
