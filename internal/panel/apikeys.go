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

// apiKeysPayload 是配置文件里一行的形状（含明文），只在本文件内部用来读配置。
type apiKeysPayload struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Platforms []string `json:"platforms,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// apiKeyRow 是**列给前端的**一行：没有明文。完整密钥只在生成那一次出现（清单 37），
// 之后列表只给掩码；删除认 id。id 是服务端从明文算出的稳定摘要，所以旧 Key 不必
// 迁移、配置里也不多出任何字段。
type apiKeyRow struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Platforms []string `json:"platforms,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	Masked    string   `json:"masked"`
}

// keyID —— 一把 Key 的稳定标识（sha256 前 12 位）。不可从掩码反推，也不必入库：
// 列表与删除都靠它，前端因此永远拿不到明文（掩码函数复用 accounts_dir.go 的 maskKey）。
// 算法收编到 livecfg.APIKeyEntry.ID()——用量统计的 per-Key 计数键（清单 38）也用它，
// 两处必须是同一个函数的同一算法，面板行 id 才查得到对应的那条统计。
func keyID(key string) string {
	return livecfg.APIKeyEntry{Key: key}.ID()
}

// getAPIKeys GET /panel/api/apikeys —— 列出全部 Key（掩码，不回明文）。
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
	rows := make([]apiKeyRow, 0, len(parsed.APIKeys))
	for _, k := range parsed.APIKeys {
		rows = append(rows, apiKeyRow{
			ID: keyID(k.Key), Name: k.Name, Platforms: k.Platforms,
			Note: k.Note, CreatedAt: k.CreatedAt, Masked: maskKey(k.Key),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": rows})
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
	// 平台白名单校验：**从注册表取**（platforms.go）——新增平台不必再改这里，
	// 也不会出现「注册表里有、白名单里没有」的静默拒绝。
	valid := map[string]bool{"*": true}
	for _, id := range platformIDs() {
		valid[id] = true
	}
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
	log.Printf("panel: API Key 已生成 %s（%s，平台 %v）——完整密钥只在这一次响应里出现", maskKey(entry.Key), entry.Name, entry.Platforms)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": entry})
}

// deleteAPIKeys POST /panel/api/apikeys/delete —— 删除指定 Key。
// body: {id}（列表给的 id）；仍接受 {key} 以兼容旧调用方。
func (p *Panel) deleteAPIKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.ID == "" && body.Key == "") {
		writeErr(w, http.StatusBadRequest, "id 必填")
		return
	}
	removed := ""
	if err := p.mutateAPIKeys(func(keys []livecfg.APIKeyEntry) []livecfg.APIKeyEntry {
		out := keys[:0]
		for _, k := range keys {
			if (body.ID != "" && keyID(k.Key) == body.ID) || (body.Key != "" && k.Key == body.Key) {
				removed = maskKey(k.Key)
				continue
			}
			out = append(out, k)
		}
		return out
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if removed == "" {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	// 日志里只出现掩码：完整密钥进日志等于把它抄进另一份明文文件。
	log.Printf("panel: API Key 已删除 %s", removed)
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
