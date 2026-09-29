// Package panel: accounts_dir.go — 统一账号目录（大一统账号池的数据面）。
//
// GET /panel/api/accounts/dir 返回全部平台的账号清单，平台分组：
//
//	{ "platforms": [
//	  {"id":"workbuddy","name":"腾讯 WorkBuddy","accounts":[{id,label,status,quota...}]},
//	  {"id":"loomy",...}, {"id":"zai",...}, {"id":"qoder",...}, ...
//	]}
//
// 账号来源（每平台一个 Agent 视图，全部只读汇总，不在这里做写操作）：
//   - workbuddy: cfg.Pool.List()（账号池/state.json——对话上游真账号）
//   - loomy-cli: extstore（密码/短信登录落库）
//   - lobsterai/raccoon/qoder/codearts: extstore
//   - zai: config schedule.zai（API Key 池，key 掩码展示）
//   - codex: keypool.ListCodexAccounts()（本机 ~/.codex）
//   - free: config free_pool（有 key 的上游列出）
//
// 前端据此渲染「全部 + 各平台子目录」导航与分组表格；每平台条目
// 「管理 →」按钮跳转到对应管理页（platforms→ext / loomy→loomy / workbuddy→accounts）。
package panel

import (
	"net/http"
	"strings"

	"github.com/chipchipss/buddyhub/internal/extstore"
)

// dirAccount 单账号目录条目（跨平台统一形状）。
type dirAccount struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Status   string `json:"status"`    // healthy/cooling/off/unknown
	Quota    string `json:"quota"`     // "1234/5000" 形态展示（平台尽力而为）
	Detail   string `json:"detail"`    // 平台自由文本（冷却原因/余额 note）
	ManageTo string `json:"manage_to"` // 前端跳转 hash（#accounts/#ext/#loomy/#apikeys）
}

// dirPlatform 单平台分组。
type dirPlatform struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	APIModel string       `json:"api_model"` // zai: / loomy: 等调用前缀；"" = 该平台非对话上游
	ManageTo string       `json:"manage_to"` // 该平台管理页 hash（组头「管理 →」跳转）
	Accounts []dirAccount `json:"accounts"`
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// accountsDir 统一账号目录。
func (p *Panel) accountsDir(w http.ResponseWriter, r *http.Request) {
	var platforms []dirPlatform

	// ── workbuddy（腾讯池）──
	wb := dirPlatform{ID: "workbuddy", Name: "腾讯 WorkBuddy", APIModel: "cn:", Accounts: []dirAccount{}}
	if p.cfg.Pool != nil {
		for _, s := range p.cfg.Pool.List() {
			st, detail := "healthy", ""
			switch {
			case s.Disabled:
				st, detail = "off", "已禁用"
			case s.CoolRemaining > 0:
				st, detail = "cooling", "冷却中"
			}
			quota := ""
			if s.CreditsTotal > 0 {
				quota = itoa(int(s.Credits)) + "/" + itoa(int(s.CreditsTotal))
			} else if s.Credits > 0 {
				quota = itoa(int(s.Credits))
			}
			wb.Accounts = append(wb.Accounts, dirAccount{
				ID: s.UID, Label: s.Nickname, Status: st, Quota: quota, Detail: detail,
				ManageTo: "#accounts",
			})
		}
	}
	platforms = append(platforms, wb)

	// ── 外部平台（extstore 全部）──
	extGroups := map[string]dirPlatform{
		extstore.PLoomyCLI:  {ID: "loomy", Name: "Loomy（讯飞）", APIModel: "loomy:", ManageTo: "#loomy", Accounts: []dirAccount{}},
		extstore.PLogsterAI: {ID: "lobsterai", Name: "LobsterAI（有道）", APIModel: "", ManageTo: "#ext", Accounts: []dirAccount{}},
		extstore.PRaccoon:   {ID: "raccoon", Name: "小浣熊（商汤）", APIModel: "", ManageTo: "#ext", Accounts: []dirAccount{}},
		extstore.PQoder:     {ID: "qoder", Name: "Qoder（阿里）", APIModel: "qoder:", ManageTo: "#ext", Accounts: []dirAccount{}},
		extstore.PCodeArts:  {ID: "codearts", Name: "CodeArts（华为云）", APIModel: "", ManageTo: "#ext", Accounts: []dirAccount{}},
	}
	if p.extManager() != nil {
		for _, a := range p.extManager().List() {
			g, ok := extGroups[a.Provider]
			if !ok {
				continue
			}
			st := "healthy"
			if a.Disabled {
				st = "off"
			}
			g.Accounts = append(g.Accounts, dirAccount{
				ID: a.ID, Label: a.Label, Status: st, ManageTo: g.ManageTo,
			})
			extGroups[a.Provider] = g
		}
	}
	platforms = append(platforms,
		extGroups[extstore.PLoomyCLI], extGroups[extstore.PQoder],
		extGroups[extstore.PLogsterAI], extGroups[extstore.PRaccoon], extGroups[extstore.PCodeArts])

	// ── zai（API Key 池）──
	zai := dirPlatform{ID: "zai", Name: "Z.AI 智谱 GLM", APIModel: "zai:", ManageTo: "#config", Accounts: []dirAccount{}}
	if len(p.cfg.ZaiKeys) > 0 {
		for i, k := range p.cfg.ZaiKeys {
			zai.Accounts = append(zai.Accounts, dirAccount{
				ID: "zai-" + itoa(i+1), Label: "API Key " + itoa(i+1), Status: "healthy",
				Detail: maskKey(k), ManageTo: "#config",
			})
		}
	}
	platforms = append(platforms, zai)

	// ── codex（本机订阅）──
	codex := dirPlatform{ID: "codex", Name: "Codex（ChatGPT 订阅）", APIModel: "codex:", ManageTo: "#config", Accounts: []dirAccount{}}
	if p.cfg.CodexCount != nil {
		if n := p.cfg.CodexCount(); n > 0 {
			codex.Accounts = append(codex.Accounts, dirAccount{
				ID: "codex-local", Label: "本机凭据", Status: "healthy",
				Detail: itoa(n) + " 个登录", ManageTo: "#config",
			})
		}
	}
	platforms = append(platforms, codex)

	// ── free 池 ──
	free := dirPlatform{ID: "free", Name: "免费 Key 池", APIModel: "free:", ManageTo: "#config", Accounts: []dirAccount{}}
	if p.cfg.FreeKeysDesc != nil {
		for _, d := range p.cfg.FreeKeysDesc() {
			free.Accounts = append(free.Accounts, dirAccount{
				ID: "free-" + d.Provider, Label: d.Provider, Status: "healthy",
				Detail: d.Masked, ManageTo: "#config",
			})
		}
	}
	platforms = append(platforms, free)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "platforms": platforms})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
