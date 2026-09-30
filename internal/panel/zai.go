// zai.go 面板的 Z.AI / ZCode 账号管理接口。
//
// 账号池是 zai: 通道的唯一真相源（config 的 zai_keys 只在池空时导入一次）。
// 这里提供：列表（含状态/额度/指纹）、增删、启停、换发指纹、验证码池状态。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/chipchipss/buddyhub/internal/zai"
)

// zaiAccounts GET /panel/api/zai/accounts —— 账号池快照 + 验证码池状态。
func (p *Panel) zaiAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"accounts":   []any{},
			"message":    "尚未配置 Z.AI 账号：可用「添加账号」粘贴 JWT 或 API Key，或在 config 的 schedule.zai.zai_keys 填写",
		})
		return
	}
	out := map[string]any{
		"configured": true,
		"accounts":   p.cfg.Zai.Snapshots(),
		"stats":      p.cfg.Zai.Stats(),
	}
	if p.cfg.ZaiCaptcha != nil {
		out["captcha"] = map[string]any{
			"enabled":    p.cfg.ZaiCaptcha.Enabled(),
			"pool_size":  p.cfg.ZaiCaptcha.PoolSize(),
			"last_error": p.cfg.ZaiCaptcha.LastError(),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// zaiAccountAdd POST /panel/api/zai/accounts —— 新增账号。
// 体：{"name":"主号","secret":"<三段 JWT 或 API Key>","provider":"zai|bigmodel"}
func (p *Panel) zaiAccountAdd(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	var req struct {
		Name     string `json:"name"`
		Secret   string `json:"secret"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	secret := strings.TrimSpace(req.Secret)
	if secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "凭据不能为空"})
		return
	}
	a := zai.NewAccount(req.Name, secret)
	if strings.TrimSpace(req.Provider) == zai.ProviderBigModel {
		a.Provider = zai.ProviderBigModel
		a.Mode = zai.ModeAPIKey
		a.JWT = ""
		a.APIKey = secret
	}
	if err := p.cfg.Zai.Store().Add(a); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"id":      a.ID,
		"mode":    a.Mode,
		"message": "账号已入池",
	})
}

// zaiAccountRemove POST /panel/api/zai/accounts/{id}/remove
func (p *Panel) zaiAccountRemove(w http.ResponseWriter, r *http.Request) {
	st, id := p.zaiStore(w, r)
	if st == nil {
		return
	}
	if err := st.Remove(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// zaiAccountToggle POST /panel/api/zai/accounts/{id}/toggle —— 启停（禁用账号人工恢复也走这里）。
func (p *Panel) zaiAccountToggle(w http.ResponseWriter, r *http.Request) {
	st, id := p.zaiStore(w, r)
	if st == nil {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	cur := st.Get(id)
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	next := !cur.Enabled
	if req.Enabled != nil {
		next = *req.Enabled
	}
	if err := st.SetEnabled(id, next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": next})
}

// zaiAccountRotate POST /panel/api/zai/accounts/{id}/rotate —— 换发设备指纹。
func (p *Panel) zaiAccountRotate(w http.ResponseWriter, r *http.Request) {
	st, id := p.zaiStore(w, r)
	if st == nil {
		return
	}
	if err := st.RotateFingerprint(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已换发设备指纹"})
}

// zaiStore 取账号池存储与路径参数 id；未启用时写出 501 并返回 nil。
func (p *Panel) zaiStore(w http.ResponseWriter, r *http.Request) (*zai.Store, string) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return nil, ""
	}
	return p.cfg.Zai.Store(), r.PathValue("id")
}
