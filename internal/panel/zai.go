// zai.go 面板的 Z.AI / ZCode 账号管理接口。
//
// 账号池是 zai: 通道的唯一真相源（config 的 zai_keys 只在池空时导入一次）。
// 这里提供：列表（含状态/额度/指纹）、增删、启停、换发指纹、验证码池状态。
package panel

import (
	"encoding/json"
	"fmt"
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

// zaiAccountQuota POST /panel/api/zai/accounts/{id}/quota —— 刷新单账号额度。
//
// 顺带做状态联动：额度归零 → exhausted；额度恢复 → 回 active。
// 上游 WAF 对 billing 族连续查询敏感，面板侧不做自动轮询（后台循环负责）。
func (p *Panel) zaiAccountQuota(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	id := r.PathValue("id")
	res := p.cfg.Zai.FetchQuota(r.Context(), id)
	if res.Error != "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": res.Error})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

// zaiQuotaAll POST /panel/api/zai/quota_all —— 刷新全部 JWT 账号额度。
func (p *Panel) zaiQuotaAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	var ids []string
	for _, a := range p.cfg.Zai.Snapshots() {
		if a.Mode == zai.ModeJWT && a.Enabled {
			ids = append(ids, a.ID)
		}
	}
	ok, fail := p.cfg.Zai.RefreshQuotas(r.Context(), ids)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "success": ok, "failed": fail})
}

// zaiStore 取账号池存储与路径参数 id；未启用时写出 501 并返回 nil。
func (p *Panel) zaiStore(w http.ResponseWriter, r *http.Request) (*zai.Store, string) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return nil, ""
	}
	return p.cfg.Zai.Store(), r.PathValue("id")
}

// zaiPlans GET /panel/api/zai/accounts/{id}/plans —— 预览可领取套餐。
func (p *Panel) zaiPlans(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	plans, err := p.cfg.Zai.PreviewPlans(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plans": plans})
}

// zaiClaimAll POST /panel/api/zai/accounts/{id}/claim —— 领取全部可领套餐。
//
// 领取需验证码（服务端求解）；未配置求解器时返回明确提示。
// 上游 WAF 对 billing 族敏感，面板侧不做自动重试，失败文案直接回给用户。
func (p *Panel) zaiClaimAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	id := r.PathValue("id")
	outcomes, err := p.cfg.Zai.ClaimAll(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	okN := 0
	for _, o := range outcomes {
		if o.OK {
			okN++
		}
	}
	msg := "没有可领取的套餐"
	if len(outcomes) > 0 {
		msg = fmt.Sprintf("领取完成：成功 %d / 共 %d", okN, len(outcomes))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "success": okN, "outcomes": outcomes, "message": msg})
}
