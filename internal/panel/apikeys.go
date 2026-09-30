// apikeys.go 面板多 API Key 管理：列表 / 生成 / 删除 / 平台授权调整。
//
// 与配置页整表单（config.go）分离：Key 生成是原子操作，不该要求前端把
// 整个 config.json 表单读改写。此处直接改写 config.json 的 api_keys 数组
// 并触发 SaveConfig 同款热应用（live.Store）——但落盘走 saveConfig 闭包
// 同一路径，保证格式/校验/原子写一致。
//
// Key 形态：bh-<32hex>（buddyhub 前缀便于日志识别）。Platforms 空 = 全平台。
package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/livecfg"
)

// apiKeysPayload 单 Key 的请求/响应形状。
type apiKeysPayload struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Platforms []string `json:"platforms,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// getAPIKeys GET /panel/api/apikeys —— 列出全部 Key（脱敏?不脱敏:面板本就有主 Key 权限）。
func (p *Panel) getAPIKeys(w http.ResponseWriter, r *http.Request) {
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	raw, _ := json.Marshal(cfg)
	var parsed struct {
		APIKeys []apiKeysPayload `json:"api_keys"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if parsed.APIKeys == nil {
		parsed.APIKeys = []apiKeysPayload{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": parsed.APIKeys})
}

// postAPIKeys POST /panel/api/apikeys —— 生成一把新 Key。
// body: {name, platforms?, note?}（platforms 空 = 全平台）。key 服务端生成。
func (p *Panel) postAPIKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string   `json:"name"`
		Platforms []string `json:"platforms"`
		Note      string   `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name 必填")
		return
	}
	// 平台白名单校验（前端下拉来源；写死避免任意串进配置）
	valid := map[string]bool{"*": true, "workbuddy": true, "loomy": true, "qoder": true, "codex": true, "free": true, "zai": true, "copilot": true, "cline": true}
	for _, plat := range body.Platforms {
		if !valid[strings.TrimSpace(plat)] {
			writeErr(w, http.StatusBadRequest, "未知平台: "+plat)
			return
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	entry := livecfg.APIKeyEntry{
		Key:       "bh-" + hex.EncodeToString(buf),
		Name:      strings.TrimSpace(body.Name),
		Platforms: body.Platforms,
		Note:      body.Note,
		CreatedAt: time.Now(),
	}
	if err := p.mutateAPIKeys(func(keys []livecfg.APIKeyEntry) []livecfg.APIKeyEntry {
		return append(keys, entry)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: API Key 已生成 %s（%s，平台 %v）", entry.Key, entry.Name, entry.Platforms)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": entry})
}

// deleteAPIKeys POST /panel/api/apikeys/delete —— 删除指定 Key。
// body: {key}
func (p *Panel) deleteAPIKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		writeErr(w, http.StatusBadRequest, "key 必填")
		return
	}
	removed := false
	if err := p.mutateAPIKeys(func(keys []livecfg.APIKeyEntry) []livecfg.APIKeyEntry {
		out := keys[:0]
		for _, k := range keys {
			if k.Key == body.Key {
				removed = true
				continue
			}
			out = append(out, k)
		}
		return out
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	log.Printf("panel: API Key 已删除 %s", body.Key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mutateAPIKeys 读当前 config → 修改 api_keys → 走 SaveConfig 落盘+热应用。
// SaveConfig 是 main 注入的同一闭包（校验+原子写+live.Store），格式与配置页一致。
func (p *Panel) mutateAPIKeys(mutate func([]livecfg.APIKeyEntry) []livecfg.APIKeyEntry) error {
	if p.cfg.LoadConfig == nil || p.cfg.SaveConfig == nil {
		return errors.New("config api not available")
	}
	cfgAny, err := p.cfg.LoadConfig()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cfgAny)
	if err != nil {
		return err
	}
	var cfgMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfgMap); err != nil {
		return err
	}
	var keys []livecfg.APIKeyEntry
	if v, ok := cfgMap["api_keys"]; ok {
		if err := json.Unmarshal(v, &keys); err != nil {
			return err
		}
	}
	keys = mutate(keys)
	buf, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	if keys == nil {
		buf = []byte("[]")
	}
	cfgMap["api_keys"] = buf
	updated, err := json.Marshal(cfgMap)
	if err != nil {
		return err
	}
	_, err = p.cfg.SaveConfig(updated)
	return err
}
