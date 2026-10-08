// renew.go 任务/签到路径的非交互凭据续期能力表。
//
// 对话路径早就「撞到期就续」（server/*_bridge.go 每个通道各自续），但签到/任务
// 路径不是：它直接拿落库凭据打上游，凭据过期就表现为一轮 failed 台账 + 三条重试
// 链，报的还是上游那句 401——用户看不出「其实是要重登了」，网关也白挨三次重试。
// 所以动手前先过一遍续期，并把结论分成四类：
//   - renewNone：没到续期点（或该平台凭据长期有效）；
//   - renewDone：已换新并回写账号表；
//   - renewManual：只能人工重新授权（缺 refresh_token / 扫码 / 设备码 / 重抓 cookie）
//     ——上游不必再打，原因直接写进结论；
//   - renewFailed：试了没成（网络 / 上游 5xx），按瞬时处理，原动作照做。
//
// 每次调用最多续期一次，不做额外抑制：上游调用次数已由重试链封顶
// （每类任务每账号每天 3 次），续期跟着它走就不会失控。
package extstore

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strings"

	"github.com/chipchipss/buddyhub/internal/extprovider/accio"
	"github.com/chipchipss/buddyhub/internal/extprovider/autoclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/copilot"
	"github.com/chipchipss/buddyhub/internal/extprovider/ima"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extprovider/trae"
)

type renewStatus int

const (
	renewNone renewStatus = iota
	renewDone
	renewManual
	renewFailed
)

// manualRenewHint 上游错误里代表「这条凭据救不回来，只能重登」的措辞。
// 各 provider 的 Refresh 已经把它们写成话术（缺 refresh_token / 需重新抓取
// cookie），这里只做归一判定，不再造一遍文案。
func manualRenewHint(err error) bool {
	if err == nil {
		return false
	}
	return NeedsReloginText(err.Error())
}

// reloginPhrases 「只能重登」的措辞表。每个 provider 的终态错误都落在这几句里
// （"缺少 refresh_token，需重新登录" / "cookie 里没有 IMA-REFRESH-TOKEN，需重新
// 抓取 cookie" / "请重新扫码登录"）。
var reloginPhrases = []string{"需重新登录", "需重新抓取", "请重新扫码"}

// NeedsReloginText 导出给面板：台账分诊（failed → 要不要进重试链）与这里的续期
// 判定**必须同一份口径**——各自维护一份列表早晚会漂，漂了就会出现「续期知道救不
// 回来、重试链却照补三轮」这种看不见的矛盾。
func NeedsReloginText(msg string) bool {
	for _, s := range reloginPhrases {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// renewer 一个平台的续期实现：解凭据 → 判到期 → 续 → 回写。
type renewer func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string)

// writeBack 续期成功后回写账号表并同步入参快照（调用方随后就用新凭据打上游）。
func writeBack(m *Manager, a *ExtAccount, fresh any) (renewStatus, string) {
	raw, err := json.Marshal(fresh)
	if err != nil {
		return renewFailed, "续期结果序列化失败: " + err.Error()
	}
	m.replaceCred(a.Provider, a.ID, raw)
	a.Cred = raw
	return renewDone, "已自动续期"
}

var renewers = map[string]renewer{
	PRaccoon: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred raccoon.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		if !cred.IsExpired() {
			return renewNone, ""
		}
		fresh, err := raccoon.New().Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PAccio: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred accio.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		if !cred.NeedsRefresh() {
			return renewNone, ""
		}
		fresh, err := accio.Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PTrae: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred trae.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		if !cred.NeedsRefresh() {
			return renewNone, ""
		}
		fresh, err := trae.Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PCline: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred cline.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		if !cred.NeedsRefresh() {
			return renewNone, ""
		}
		fresh, err := cline.Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PCopilot: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred copilot.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		if !cred.NeedsRefresh() {
			return renewNone, ""
		}
		fresh, err := copilot.Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PAutoClaw: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred autoclaw.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		// 无 refresh_token 的号不判人工：上游 token 可能仍有效（CanRefresh 口径），
		// 交给原动作去撞，撞死了再说。
		if !cred.NeedsRefresh() || !cred.CanRefresh() {
			return renewNone, ""
		}
		fresh, err := autoclaw.Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PQoder: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred qoder.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		if !cred.NeedsRefresh() {
			return renewNone, ""
		}
		fresh, err := qoder.New().Refresh(ctx, &cred)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
	PIMA: func(ctx context.Context, m *Manager, a *ExtAccount) (renewStatus, string) {
		var cred ima.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			return renewNone, ""
		}
		// ima 的 NeedsRefresh 无 refresh_token 时返回 false（只能人工重抓 cookie，
		// 那是账号视图的事）；这里到期即续。
		if !cred.NeedsRefresh() {
			return renewNone, ""
		}
		fresh, err := cred.Refresh(ctx)
		if err != nil {
			if manualRenewHint(err) {
				return renewManual, err.Error()
			}
			return renewFailed, err.Error()
		}
		return writeBack(m, a, fresh)
	},
}

// RenewableProviders 可非交互续期的平台（面板注册表一致性断言用）。
func RenewableProviders() []string {
	out := make([]string, 0, len(renewers))
	for id := range renewers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CanRenew 该平台是否有非交互续期路径（false = 到期只能人工重新授权）。
func CanRenew(provider string) bool { _, ok := renewers[provider]; return ok }

// renew 到期凭据自动换新；返回结论与原因。未注册续期器的平台一律 renewNone。
func (m *Manager) renew(ctx context.Context, a *ExtAccount) (renewStatus, string) {
	fn, ok := renewers[a.Provider]
	if !ok {
		return renewNone, ""
	}
	st, reason := fn(ctx, m, a)
	if st != renewNone {
		log.Printf("extstore: %s/%s 续期 %s: %s", a.Provider, a.ID, st, reason)
	}
	return st, reason
}

func (s renewStatus) String() string {
	switch s {
	case renewDone:
		return "done"
	case renewManual:
		return "manual"
	case renewFailed:
		return "failed"
	}
	return "none"
}
