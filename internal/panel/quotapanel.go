package panel

// quotapanel.go 阶段 4/5 面板增强：Credits Used 列 + Key Name 列与过滤 +
// 配额弹窗数据源（Chief 派工令第 2 项）。
//
// 数据衔接（与阶段 4 账本的直连）：
//   - Credits Used 列：extstore 账号视图每行追加 credits_allocated/credits_used/
//     credits_remaining/exhausted——由 server 层的 qoder.CreditLedger 注入
//     （面板不能直接 import server，会成环；经 QuotaSource 接口解耦，
//     main 装配时把 handler 的账本快照函数递进来）；
//   - Key Name 列 + 过滤：usage 视图 by_key 行本来就只有 id（sha256 摘要），
//     这里补 key name 映射（livecfg 配置里就有），并支持 ?key_name= 过滤；
//   - 配额弹窗：GET /panel/api/quota 返回 CreditLedger.Snapshot 全量，
//     前端弹窗直接渲染。

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/chipchipss/buddyhub/internal/usage"
)

// QuotaSource 阶段 4 账本的读取面（main 装配注入；nil = 面板额度数据为空态）。
// 返回 qoder.QuotaSnapshot 切片的函数——面板不 import qoder 包，
// 用 any 切片避免包环（调用方保证元素带 json 标签）。
type QuotaSource func() []any

// ExtQuotaView 外部账号行的额度扩展字段（前端 Credits Used 列渲染用）。
type ExtQuotaView struct {
	AccountKey       string  `json:"account_key"`
	Provider         string  `json:"provider"`
	ID               string  `json:"id"`
	CreditsAllocated float64 `json:"credits_allocated,omitempty"`
	CreditsUsed      float64 `json:"credits_used,omitempty"`
	CreditsRemaining float64 `json:"credits_remaining,omitempty"` // -1 = 无上限
	Exhausted        bool    `json:"exhausted,omitempty"`
	Precise          bool    `json:"precise,omitempty"`
}

// quota GET /panel/api/quota —— 全部账号的额度快照（配额弹窗数据源）。
// QuotaSource 未注入（nil）时返回空列表 + available:false（前端显示空态，
// 不报错——裸用/测试场景没有账本）。
func (p *Panel) quota(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QuotaSource == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"available": false,
			"quotas":    []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"available":  true,
		"quotas":     p.cfg.QuotaSource(),
		"can_remove": p.cfg.QuotaRemove != nil,
	})
}

// quotaRemove POST /panel/api/quota/remove —— 移除账号分配上限
// （配额弹窗「移除分配」，fork quota modal 语义）。body: {account_key}。
func (p *Panel) quotaRemove(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QuotaRemove == nil {
		writeErr(w, http.StatusNotImplemented, "quota ledger not wired")
		return
	}
	var body struct {
		AccountKey string `json:"account_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.AccountKey) == "" {
		writeErr(w, http.StatusBadRequest, "account_key 必填")
		return
	}
	p.cfg.QuotaRemove(strings.TrimSpace(body.AccountKey))
	log.Printf("panel: 已移除账号分配上限 %s", body.AccountKey)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": body.AccountKey})
}

// keyNameIndex Key 摘要 id → 显示名的映射（livecfg 配置读取）。
// 读不出来时空映射（by_key 行照旧，只是没有名字列数据）。
func (p *Panel) keyNameIndex() map[string]string {
	out := map[string]string{}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		return out
	}
	raw, _ := json.Marshal(cfg)
	var parsed struct {
		APIKeys []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"api_keys"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return out
	}
	for _, k := range parsed.APIKeys {
		if k.Name != "" {
			out[keyID(k.Key)] = k.Name
		}
	}
	return out
}

// keyUsagePayload by_key 行的增强形状：原 KeyStat + name（Key Name 列）
// + 终身 token 累计（fork keyOwner 口径：这把 Key 一共消耗多少 token）。
type keyUsagePayload struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	Calls            int64  `json:"calls"`
	Errors           int64  `json:"errors"`
	Last             int64  `json:"last"`
	PromptTokens     int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
}

// usageKeyNames GET /panel/api/usage/keys —— 逐 Key 用量 + 名称映射。
// key_name 查询参数做过滤（前缀不区分大小写）：Chief 派工令"Key Name 列+过滤"。
// token 字段为终身累计（KeyStat 与 keyBuckets 同源），时间维度走 series 端点。
func (p *Panel) usageKeyNames(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("key_name")))
	snap := p.cfg.Usage.Snapshot(0, nil)
	names := p.keyNameIndex()
	rows := make([]keyUsagePayload, 0, len(snap.ByKey))
	for _, k := range snap.ByKey {
		name := names[k.ID]
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		rows = append(rows, keyUsagePayload{
			ID: k.ID, Name: name, Calls: k.Calls, Errors: k.Errors, Last: k.Last,
			PromptTokens: k.PromptTokens, CompletionTokens: k.CompletionTokens, TotalTokens: k.TotalTokens,
		})
	}
	// 调用量降序（与 usage.Snapshot 的 by_key 排序一致）。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Calls != rows[j].Calls {
			return rows[i].Calls > rows[j].Calls
		}
		return rows[i].ID < rows[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": rows, "names_mapped": len(names)})
}

// usageKeySeries GET /panel/api/usage/keys/series —— 单把 Key 的时序用量
// （fork usage-db 的 period 查询：按小时/日合并的读路径）。
// 参数：id（Key 摘要，必填）、hours（>0 窗口，<=0 全部历史，上限 60 天）。
func (p *Panel) usageKeySeries(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id 必填（Key 摘要）")
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "hours 必须是整数")
			return
		}
		hours = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"id":     id,
		"hours":  hours,
		"series": p.cfg.Usage.KeySeries(id, hours),
	})
}

var _ = usage.Delta{} // 保持 import（类型引用锚点）
