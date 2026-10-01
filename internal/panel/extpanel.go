package panel

// extpanel.go 外部积分账号（lobsterai/raccoon/qoder/codearts）面板接线：
// 账号 CRUD、单/全量签到、余额视图、全局一键签到。凭据持久化由 extstore
// 负责（data/ext-accounts.json，与 state 文件同目录）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/loomy"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// extPath 外部账号持久化文件路径（state 文件同目录）。
func extPath(stateFile string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return "ext-accounts.json"
	}
	return filepath.Join(dir, "ext-accounts.json")
}

// extMu/extMgr 全局单例（面板常驻进程，Manager 进程内唯一）。
var (
	extOnce sync.Once
	extMgr  *extstore.Manager
)

// extManager 惰性初始化外部账号管理器。
func (p *Panel) extManager() *extstore.Manager {
	extOnce.Do(func() {
		path := extPath(p.cfg.StateFile)
		extMgr = extstore.NewManager(filepath.Dir(path))
	})
	return extMgr
}

// extAccounts GET /panel/api/ext/accounts —— 全部外部账号视图（含余额，逐账号实时查）。
func (p *Panel) extAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accounts": p.extManager().StatusAll(r.Context()),
	})
}

// extAccountAdd POST /panel/api/ext/accounts —— 手工添加账号。
// body: {provider, id, label, cred(raw json object)}
func (p *Panel) extAccountAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string          `json:"provider"`
		ID       string          `json:"id"`
		Label    string          `json:"label"`
		Cred     json.RawMessage `json:"cred"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	// 只接受「账号落在 extstore」的平台（注册表说了算，见 platforms.go）
	if !platformUsesExtstore(body.Provider) {
		writeErr(w, http.StatusBadRequest, "该平台不走外部账号表: "+body.Provider)
		return
	}
	if body.ID == "" || len(body.Cred) == 0 {
		writeErr(w, http.StatusBadRequest, "id 与 cred 必填")
		return
	}
	if body.Label == "" {
		body.Label = body.ID
	}
	if err := p.extManager().Add(body.Provider, body.ID, body.Label, body.Cred); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 外部账号已添加 %s/%s", body.Provider, body.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// extAccountRemove POST /panel/api/ext/accounts/{provider}/{id}/remove
func (p *Panel) extAccountRemove(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	if err := p.extManager().Remove(provider, id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("panel: 外部账号已删除 %s/%s", provider, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// extAccountToggle POST /panel/api/ext/accounts/{provider}/{id}/toggle —— 启用/停用
func (p *Panel) extAccountToggle(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	if err := p.extManager().SetDisabled(provider, id, body.Disabled); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": body.Disabled})
}

// RunExtCheckinAll 调度器 ExtHook 入口：遍历全部外部账号签到（同步执行，
// 账号数有界 + 串行 45s 超时/个，最坏情况分钟级；调度器在独立 goroutine 调用）。
func (p *Panel) RunExtCheckinAll() {
	results := p.extManager().CheckinAll(context.Background())
	claimed, failed := 0, 0
	for _, res := range results {
		log.Printf("ext-checkin %s/%s → %s %s", res.Provider, res.ID, res.Kind, res.Message)
		switch res.Kind {
		case "claimed":
			claimed++
		case "failed":
			failed++
		}
	}
	log.Printf("ext-checkin 完成: 成功 %d · 失败 %d · 共 %d 账号", claimed, failed, len(results))
}

// RunLoomyDailyCheckin 调度器 ExtHook 入口：触发 Loomy 每日赠送额度（幂等）。
// 与面板 loomyCheckin 同管线：FindLoomySession → CheckinDailyQuota；无本地
// 登录态/已处理/失败均只记日志，不影响同 hook 的外部账号签到。
func (p *Panel) RunLoomyDailyCheckin() {
	client := getLoomyClient()
	session, err := upstream.FindLoomySession()
	if err != nil || session == nil {
		log.Printf("loomy-checkin 跳过: 未检测到本地 Loomy 客户端登录态 (auth-session.json): %v", err)
		return
	}
	res, err := client.CheckinDailyQuota(session.Session)
	if err != nil {
		log.Printf("loomy-checkin 失败: %v", err)
		return
	}
	if res.AlreadyProcessed {
		log.Printf("loomy-checkin 已处理: %s", res.Message)
	} else {
		log.Printf("loomy-checkin 成功: %s", res.Message)
	}
}

// extCheckinOne POST /panel/api/ext/accounts/{provider}/{id}/checkin —— 单账号签到
func (p *Panel) extCheckinOne(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	a := p.extManager().Find(provider, id)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	res := p.extManager().CheckinOne(r.Context(), a)
	log.Printf("panel: 外部签到 %s/%s → %s %s", provider, id, res.Kind, res.Message)
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.Kind != "failed", "result": res})
}

// extCheckinAll POST /panel/api/ext/checkin_all —— 全部账号签到（全局一键签到主体）。
// 账号间串行（外部接口限速优先于并发）；结果逐条返回。
func (p *Panel) extCheckinAll(w http.ResponseWriter, r *http.Request) {
	results := p.extManager().CheckinAll(r.Context())
	for _, res := range results {
		log.Printf("panel: 外部签到 %s/%s → %s %s", res.Provider, res.ID, res.Kind, res.Message)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// ensureExtStoreDir 保证持久化目录存在（首次启动即建，避免首次添加账号时才失败）。
func ensureExtStoreDir(stateFile string) {
	if dir := filepath.Dir(extPath(stateFile)); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
}

// ExtList server 包读取外部账号的导出面（Qoder 桥接用）。
func (p *Panel) ExtList() []*extstore.ExtAccount { return p.extManager().ExtList() }

// ExtManagerReplaceCred 暴露凭据回写（server 桥接经 main 闭包调用）。
func (p *Panel) ExtManagerReplaceCred(provider, id string, cred json.RawMessage) {
	p.extManager().ReplaceCred(provider, id, cred)
}

// ExtManagerNoteChatResult 暴露对话结果上报（驱动外部账号冷却/退避，
// server 桥接经 main 闭包调用）。
func (p *Panel) ExtManagerNoteChatResult(provider, id string, err error) {
	p.extManager().NoteChatResult(provider, id, err)
}

// ExtManagerNoteBalance 暴露余额观测（5 分钟余额刷新喂给候选排序）。
func (p *Panel) ExtManagerNoteBalance(provider, id string, balance float64, ok bool) {
	p.extManager().NoteBalance(provider, id, balance, ok)
}

// ExtManagerCachedBalance 暴露免网络的余额读（候选按剩余额度排序用）。
func (p *Panel) ExtManagerCachedBalance(provider string) (float64, bool) {
	return p.extManager().CachedBalance(provider)
}

// ---------------------------------------------------------------------------
// Loomy 账号服务登录（密码 / 短信）：新增平台账号的自动路径。
// 登录成功 = session 拿到 + 设备身份持久化 + 外部账号落库，全自动。
// ---------------------------------------------------------------------------

// loomySmsMsgID 短信验证码 msgid 缓存（phone → msgid，进程内）。
var (
	loomySmsMu    sync.Mutex
	loomySmsMsgID = map[string]string{}
)

// extLoginLoomyPassword POST /panel/api/ext/loomy/login_password
// body: {phone, password}
func (p *Panel) extLoginLoomyPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	if body.Phone == "" || body.Password == "" {
		writeErr(w, http.StatusBadRequest, "phone 与 password 必填")
		return
	}
	cli := loomy.NewClient()
	res, err := cli.LoginByPassword(body.Phone, body.Password, nil)
	if err != nil {
		log.Printf("panel: Loomy 密码登录失败 %s***: %v", body.Phone[:3], err)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	p.saveLoomyLoginWithPassword(w, r, res, cli, body.Password)
}

// extLoginLoomySendSMS POST /panel/api/ext/loomy/send_sms
// body: {phone} → {msgid}
func (p *Panel) extLoginLoomySendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Phone == "" {
		writeErr(w, http.StatusBadRequest, "phone 必填")
		return
	}
	cli := loomy.NewClient()
	msgID, err := cli.SendSMSCode(body.Phone, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	loomySmsMu.Lock()
	loomySmsMsgID[body.Phone] = msgID
	loomySmsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msgid": msgID})
}

// extLoginLoomySMS POST /panel/api/ext/loomy/login_sms
// body: {phone, code, msgid?}——msgid 可空:进程内缓存按 phone 回填 send_sms 时返回的 msgid。
func (p *Panel) extLoginLoomySMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
		MsgID string `json:"msgid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.Phone == "" || body.Code == "" {
		writeErr(w, http.StatusBadRequest, "phone/code 必填")
		return
	}
	if body.MsgID == "" {
		loomySmsMu.Lock()
		body.MsgID = loomySmsMsgID[body.Phone]
		loomySmsMu.Unlock()
		if body.MsgID == "" {
			writeErr(w, http.StatusBadRequest, "msgid 缺失:请先「发验证码」（服务端按手机号缓存 msgid）")
			return
		}
	}
	cli := loomy.NewClient()
	res, err := cli.LoginBySMS(body.Phone, body.Code, body.MsgID, nil)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	p.saveLoomyLogin(w, r, res, cli)
}

// saveLoomyLogin 登录成功公共尾：落库为 loomy-cli 外部账号 + 同步写 data/loomy-session.json。
// 双写原因：外部池（ext-accounts.json）承担签到/用量展示；而 Loomy 页的任务进度、
// 每日签到、双积分池查询走 upstream.FindLoomySession()，其第 0 优先级来源是
// data/loomy-session.json（本机无桌面客户端时唯一的凭据来源）。缺任一份，对应页面即"什么都没有"。
func (p *Panel) saveLoomyLogin(w http.ResponseWriter, r *http.Request, res *loomy.LoginResult, cli *loomy.Client) {
	p.saveLoomyLoginWithPassword(w, r, res, cli, "")
}

// saveLoomyLoginWithPassword 同 saveLoomyLogin，另把密码加密落盘（无人值守续期用）。
// plainPassword 为空 = 不存密（短信登录/仅 session 导入），该账号过期需手动重登。
func (p *Panel) saveLoomyLoginWithPassword(w http.ResponseWriter, r *http.Request, res *loomy.LoginResult, cli *loomy.Client, plainPassword string) {
	cred := map[string]any{
		"session":  res.Session,
		"userid":   res.UserID,
		"phone":    res.Phone,
		"login_at": time.Now().Unix(),
	}
	if plainPassword != "" {
		if stored, err := upstream.ProtectPassword(plainPassword); err == nil && stored != "" {
			cred["password"] = stored
		} else {
			log.Printf("panel: Loomy 密码存盘失败（该账号将无法自动续期）: %v", err)
		}
	}
	raw, _ := json.Marshal(cred)
	id := res.UserID
	if id == "" {
		id = "phone-" + res.Phone
	}
	if err := p.extManager().Add(extstore.PLoomyCLI, id, res.Phone, raw); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 同步 Loomy 页凭据（FindLoomySession 第 0 优先级读取此文件）
	if err := upstream.SaveLoomySession(&upstream.LoomySession{
		Session: res.Session,
		UserID:  res.UserID,
		Phone:   res.Phone,
	}); err != nil {
		log.Printf("panel: Loomy 会话文件写入失败（外部池已保存，任务页可能不可用）: %v", err)
	}
	log.Printf("panel: Loomy 账号已登录并保存 (%s)", res.Phone)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "userid": res.UserID})
}

// extBalanceLastLog 上一次打印的余额刷新摘要（去重用）。
//
// 这个任务跑在 5 分钟的 ticker 上——每次都打一行会在几小时内淹掉日志，
// 而成功/失败计数不变时那些行没有新信息。
var extBalanceLastLog string

// RunExtBalanceRefresh 调度器余额刷新入口：把外部平台的余额也刷新一遍。
//
// 此前只有腾讯池每 5 分钟刷一次（scheduler.RunBalanceRefreshNow 只遍历
// cfg.Pool），外部平台的余额是**打开面板时才按需拉**（StatusAll）——
// 于是面板上显示的一直是上一次打开时的旧数。挂到同一个 ticker 上，
// 不新增调度任务，也不改变语义（只刷新观测量，不签到、不续期）。
func (p *Panel) RunExtBalanceRefresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	views := p.extManager().StatusAll(ctx)
	ok, bad := 0, 0
	for _, v := range views {
		if v == nil {
			continue
		}
		// 这一记**是本函数此前唯一的缺口**：ViewOne 拿到了余额却直接丢掉，
		// 导致候选排序没有任何额度依据（ViewOne 每次 30s 网络，绝不能在请求
		// 路径上重跑）。缓存下来，排序侧走 CachedBalance 免网络读。
		p.extManager().NoteBalance(v.Provider, v.ID, v.Balance, v.BalanceOK)
		if v.BalanceOK {
			ok++
		} else {
			bad++
		}
	}
	// 只在计数**变化**时打：成功每 5 分钟一条毫无信息量，失败重复打也只会
	// 让真正要看的那条被淹没。
	if summary := fmt.Sprintf("成功 %d · 失败 %d · 共 %d 账号", ok, bad, len(views)); summary != extBalanceLastLog {
		extBalanceLastLog = summary
		log.Printf("ext-balance 完成: %s", summary)
	}
}
