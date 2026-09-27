package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

var (
	defaultLoomyClient *upstream.LoomyClient
	loomyClientOnce    sync.Once
)

func getLoomyClient() *upstream.LoomyClient {
	loomyClientOnce.Do(func() {
		defaultLoomyClient = upstream.NewLoomyClient("")
	})
	return defaultLoomyClient
}

// loomyStatus 获取本地 Loomy 任务进度与账号状态。
func (p *Panel) loomyStatus(w http.ResponseWriter, r *http.Request) {
	client := getLoomyClient()
	session, err := upstream.FindLoomySession()
	if err != nil || session == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"has_account": false,
			"message":     "未检测到本地 Loomy 客户端登录态",
			"status": upstream.LoomyStatus{
				HasAccount: false,
				Earned:     0,
				Total:      10000,
				Tasks:      map[string]bool{},
			},
		})
		return
	}

	st := client.QueryStatus(session)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"has_account": true,
		"status":      st,
	})
}

// loomyCompleteAll 一键完成全部 Loomy 新手任务并持久化同步本地缓存。
func (p *Panel) loomyCompleteAll(w http.ResponseWriter, r *http.Request) {
	client := getLoomyClient()
	session, err := upstream.FindLoomySession()
	if err != nil || session == nil {
		writeErr(w, http.StatusBadRequest, "未检测到本地 Loomy 客户端登录态 (auth-session.json)")
		return
	}

	completed, st, err := client.CompleteAll(session)
	if err != nil {
		log.Printf("panel: Loomy 任务完成失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "执行失败: "+err.Error())
		return
	}

	log.Printf("panel: Loomy 任务一键完成: 新完成 %d 项, 当前总积分 %d/%d", len(completed), st.Earned, st.Total)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"completed": completed,
		"status":    st,
	})
}

// loomySave 保存或验证 Loomy 账号会话。
func (p *Panel) loomySave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		UserID  string `json:"userid"`
		Phone   string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	sessionToken := strings.TrimSpace(body.Session)
	if sessionToken == "" {
		writeErr(w, http.StatusBadRequest, "session token 不能为空")
		return
	}
	client := getLoomyClient()
	_, earned, total, err := client.GetTasks(sessionToken)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "验证 Loomy token 失败: "+err.Error())
		return
	}
	sess := &upstream.LoomySession{
		Session: sessionToken,
		UserID:  body.UserID,
		Phone:   body.Phone,
	}
	if err := upstream.SaveLoomySession(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	st := client.QueryStatus(sess)
	log.Printf("panel: Loomy 账号已保存 (当前积分 %d/%d)", earned, total)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": st,
	})
}

// loomyCheckin 触发 Loomy 每日赠送额度（幂等，重复点击返回已初始化状态）。
func (p *Panel) loomyCheckin(w http.ResponseWriter, r *http.Request) {
	client := getLoomyClient()
	session, err := upstream.FindLoomySession()
	if err != nil || session == nil {
		writeErr(w, http.StatusBadRequest, "未检测到本地 Loomy 客户端登录态 (auth-session.json)")
		return
	}
	res, err := client.CheckinDailyQuota(session.Session)
	if err != nil {
		log.Printf("panel: Loomy 每日签到失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "签到失败: "+err.Error())
		return
	}
	if res.AlreadyProcessed {
		log.Printf("panel: Loomy 每日签到: %s", res.Message)
	} else {
		log.Printf("panel: Loomy 每日签到: %s", res.Message)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"checkin": res,
	})
}

// loomyCredits 查询 Loomy 双积分池明细（永久 + 每日赠送，只读）。
func (p *Panel) loomyCredits(w http.ResponseWriter, r *http.Request) {
	client := getLoomyClient()
	session, err := upstream.FindLoomySession()
	if err != nil || session == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"has_account": false,
			"message":     "未检测到本地 Loomy 客户端登录态",
		})
		return
	}
	detail, err := client.GetCreditDetail(session.Session)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"has_account": true,
			"error":       err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"has_account": true,
		"credits":     detail,
	})
}
