// Package panel: accounts_dir.go — 统一账号目录（大一统账号池的数据面）。
//
// GET /panel/api/accounts/dir 返回全部平台的账号清单。**平台清单来自注册表**
// （platforms.go）——这里只负责回答「每个平台的账号从哪来」：
//
//   - workbuddy  账号池 state.json（对话上游真账号）
//   - zai        Z.AI 账号池 + 旧版 API Key（掩码展示）
//   - codex      本机 ~/.codex 登录数
//   - free       配置里的免费 key（掩码展示）
//   - 其余        extstore（data/ext-accounts.json）
//
// 前端据此渲染「全部 + 各平台子目录」导航与分组表格；注册表里有的平台
// **一定**会出现在这里，不会因为漏改某个 map 而消失。
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
	ManageTo string `json:"manage_to"` // 前端跳转 hash（#accounts / #accounts?seg=zai|ext / #config）
}

// dirPlatform 单平台分组。
type dirPlatform struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	APIModel string       `json:"api_model"` // 调用前缀；"" = 该平台非对话上游
	Group    string       `json:"group"`     // gateway（有对话 API）/ points（只签到）
	Login    string       `json:"login"`     // 入池方式（前端据此给「去添加」入口）
	Checkin  bool         `json:"checkin"`
	Note     string       `json:"note,omitempty"`
	ManageTo string       `json:"manage_to"`
	Accounts []dirAccount `json:"accounts"`
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// manageToFor 该平台的管理页跳转目标。
//
// 前端视图注册表里只有 overview/accounts/usage/tasks/models/keys/config/logs
// 这些真实视图；Z.AI 与外部平台的管理界面是「账号」视图的分段
// （#accounts?seg=zai / #accounts?seg=ext），不是独立 hash——指向不存在的
// #ext 会被 navigate 回落到第一个视图，表现为「点了没反应」。
func manageToFor(pl Platform) string {
	switch {
	case pl.ID == "workbuddy":
		return "#accounts"
	case pl.ID == "zai":
		return "#accounts?seg=zai"
	case pl.Login == LoginNone || pl.Login == LoginConfig:
		return "#config" // 本机凭据 / 配置页填写
	default:
		return "#accounts?seg=ext"
	}
}

// accountsDir 统一账号目录。
func (p *Panel) accountsDir(w http.ResponseWriter, r *http.Request) {
	// extstore 一次 List，按 provider 分桶（避免每个平台各扫一遍）
	extByProvider := map[string][]dirAccount{}
	if p.extManager() != nil {
		for _, a := range p.extManager().List() {
			st := "healthy"
			if a.Disabled {
				st = "off"
			}
			extByProvider[a.Provider] = append(extByProvider[a.Provider], dirAccount{
				ID: a.ID, Label: a.Label, Status: st,
			})
		}
	}

	out := make([]dirPlatform, 0, len(platforms))
	for _, pl := range platforms {
		g := dirPlatform{
			ID: pl.ID, Name: pl.Name, APIModel: pl.Prefix, Group: pl.Group,
			Login: pl.Login, Checkin: pl.Checkin, Note: pl.Note,
			ManageTo: manageToFor(pl), Accounts: []dirAccount{},
		}
		switch pl.ID {
		case "workbuddy":
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
					g.Accounts = append(g.Accounts, dirAccount{
						ID: s.UID, Label: s.Nickname, Status: st, Quota: quota, Detail: detail,
						ManageTo: "#accounts",
					})
				}
			}

		case "zai":
			// 账号池（面板可增删改）+ 旧版 API Key（掩码）
			if p.cfg.Zai != nil {
				for _, s := range p.cfg.Zai.Snapshots() {
					st := "healthy"
					if !s.Enabled {
						st = "off"
					} else if s.CoolLeft > 0 {
						st = "cooling"
					}
					detail := s.Mode
					if s.PlanName != "" {
						detail = s.PlanName
					}
					if s.LastErr != "" && st != "healthy" {
						detail = s.LastErr
					}
					g.Accounts = append(g.Accounts, dirAccount{
						ID: s.ID, Label: s.Name, Status: st, Detail: detail, ManageTo: "#accounts?seg=zai",
					})
				}
			}
			for i, k := range p.cfg.ZaiKeys {
				g.Accounts = append(g.Accounts, dirAccount{
					ID: "zai-key-" + itoa(i+1), Label: "旧版 API Key " + itoa(i+1),
					Status: "healthy", Detail: maskKey(k), ManageTo: "#config",
				})
			}

		case "codex":
			if p.cfg.CodexCount != nil {
				if n := p.cfg.CodexCount(); n > 0 {
					g.Accounts = append(g.Accounts, dirAccount{
						ID: "codex-local", Label: "本机凭据", Status: "healthy",
						Detail: itoa(n) + " 个登录", ManageTo: "#config",
					})
				}
			}

		case "free":
			if p.cfg.FreeKeysDesc != nil {
				for _, d := range p.cfg.FreeKeysDesc() {
					g.Accounts = append(g.Accounts, dirAccount{
						ID: "free-" + d.Provider, Label: d.Provider, Status: "healthy",
						Detail: d.Masked, ManageTo: "#config",
					})
				}
			}

		default:
			// 其余平台的账号都在 extstore；loomy 的分组 id 是 loomy，落库 provider 是 loomy-cli
			provider := pl.ID
			if pl.ID == "loomy" {
				provider = extstore.PLoomyCLI
			}
			for _, a := range extByProvider[provider] {
				a.ManageTo = g.ManageTo
				g.Accounts = append(g.Accounts, a)
			}
		}
		out = append(out, g)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "platforms": out})
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
