package panel

// extpanel.go 外部积分账号（lobsterai/raccoon/qoder/codearts）面板接线：
// 账号 CRUD、单/全量签到、余额视图、全局一键签到。凭据持久化由 extstore
// 负责（data/ext-accounts.json，与 state 文件同目录）。

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/chipchipss/buddyhub/internal/extstore"
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
	switch body.Provider {
	case extstore.PLogsterAI, extstore.PRaccoon, extstore.PQoder, extstore.PCodeArts:
	default:
		writeErr(w, http.StatusBadRequest, "未知平台: "+body.Provider)
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
