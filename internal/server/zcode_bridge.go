// Package server: zcode_bridge.go — ZCode / z.ai 通道（magpie 二批，阶段②）。
//
// 模型名带 "zcode:" 前缀时走本路径。协议事实（magpie-plugins
// packages/zcode/index.mjs，HEAD f573cfd）：
//   - 上游是 **Anthropic Messages 兼容端点**（index.mjs L29-31：zai/bigmodel
//     两站 base + Start Plan base；本阶段只接 coding-key 模式的两站）：
//     https://api.z.ai/api/anthropic/v1/messages 与
//     https://open.bigmodel.cn/api/anthropic/v1/messages；
//   - 翻译复用既有 upstream.ZaiTranslateIn / ZaiSSEConverter（zai_bridge.go
//     同一套，zcode 上游与 zai 上游是同一形状的 Anthropic 兼容协议）；
//   - 鉴权（fork L1545-51）：coding key 双头——x-api-key 与 Authorization:
//     Bearer 都设（兼容两种客户端风格）；Start Plan JWT 路径本阶段不接
//     （需 dress() 改写 body 与一整套 ZCode 客户端标识头，后续批次）；
//   - anthropic-version: 2023-06-01（与 zai 桥同值）；
//   - 错误分类（auxiliary 参考对齐 fork spentUp L247-255）：429 与
//     200-with-code 1113/1005 都归额度类；401 不标登录失效（zcode 的
//     coding key 无过期概念，expires=0）；
//   - 无排队信封：限速语义就是 429 本身，无 retry-after 可读。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extprovider/zcode"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// zcodeModelPrefix ZCode 路由前缀。
const zcodeModelPrefix = "zcode:"

// isZcodeModel 判断裸模型名是否请求 ZCode 通道。
func isZcodeModel(bare string) bool { return strings.HasPrefix(bare, zcodeModelPrefix) }

// zcodeSiteFor 按 bareModel 后缀选站点："zcode:cn/xxx" → bigmodel，
// 其余 → z.ai（fork siteOf：非 bigmodel 一律 zai）。
func zcodeSiteFor(bareModel string) (site string, model string) {
	if rest, ok := strings.CutPrefix(bareModel, "cn/"); ok {
		return "bigmodel", rest
	}
	return "zai", bareModel
}

// zcodeMessagesURL 站点 → Anthropic messages 端点（fork L29-30）。
func zcodeMessagesURL(site string) string {
	if site == "bigmodel" {
		return "https://open.bigmodel.cn/api/anthropic/v1/messages"
	}
	return "https://api.z.ai/api/anthropic/v1/messages"
}

// ZcodeChatStream ZCode 通道：OpenAI 请求 → Anthropic messages（复用 zai 翻译）
// → coding-key 双头直连 → 反译回 OpenAI。返回 false = 无可用账号。
func (h *Handler) ZcodeChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	h.lastZcodeErr = ""
	if h.cfg.ExtAccounts == nil {
		return false
	}
	site, model := zcodeSiteFor(bareModel)
	// 剥 zcode:/cn/ 前缀后重写 body 模型名（与其他桥接同款）。
	if m := strings.TrimPrefix(strings.TrimPrefix(bareModel, zcodeModelPrefix), "cn/"); m != bareModel {
		body = rewriteModel(body, m)
	}

	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PZCode && !a.Disabled && a.SignInState != "expired" {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		h.lastZcodeErr = "没有可用的 ZCode 账号（面板添加 zcode 凭据）"
		return false
	}

	anthBody, _, err := upstream.ZaiTranslateIn(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return true
	}

	lastErr := ""
	for _, a := range accounts {
		var cred zcode.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		key := cred.BearerToken()
		if key == "" {
			lastErr = "凭据里没有 api_key/access_token"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// 观察 3 收口（auxiliary 复核）：JWT-only 凭据会走错上游路径（fork 的
		// JWT 走 START_BASE + startHeaders + dress()，本阶段未接）——把"只接
		// coding key"的范围声明变成强制约束：JWT 形态直接跳过并提示。
		if cred.APIKey == "" && cred.AccessToken != "" {
			lastErr = "仅 JWT 凭据暂不支持对话（Start Plan 路径未接线），请配置 API Key"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.Lapsed(timeNow()) {
			lastErr = "Start Plan JWT 已过期（耗尽即弃），请重新获取"
			_ = h.extManager.MarkSignInExpired(a.Provider, a.ID, lastErr)
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		req, rerr := http.NewRequestWithContext(r.Context(), http.MethodPost, zcodeMessagesURL(site), strings.NewReader(string(anthBody)))
		if rerr != nil {
			lastErr = rerr.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// coding key 双头（fork L1548-50）：x-api-key 与 Authorization 都设。
		req.Header.Set("x-api-key", key)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("anthropic-version", upstream.AnthropicVersion)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		// magpie 私有头剥离（auxiliary 桥接注意点 3）：x-magpie-work-* 是
		// magpie 对自己插件的指示头，绝不外发上游。
		for _, hh := range []string{"x-magpie-work-dir", "x-magpie-work-git"} {
			req.Header.Del(hh)
		}

		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			lastErr = derr.Error() // 网络故障：保留登录态，换号
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			// 错误分类（fork spentUp L247-255）：429 或 200-body 1113/1005 = 额度类；
			// zcode 的 401 不标登录失效（coding key 永不过期）。
			if resp.StatusCode == http.StatusTooManyRequests || zcodeSpentUp(string(raw), resp.StatusCode) {
				lastErr = fmt.Sprintf("额度/限流（HTTP %d）", resp.StatusCode)
				// 二相性入账（Chief 口径 2）：额度耗尽事件记进积分账本（若该账号
				// 配了分配上限）——PRE 门槛据此把账号踢出轮转，二相性两条路
				// （HTTP 429 / 200-body code）都汇到这里，单一事实源。
				h.zcodeRecordSpent(a)
			} else {
				lastErr = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
			}
			log.Printf("zcode-bridge: acct=%s 上游 %d: %s", a.ID, resp.StatusCode, qzRedact(string(raw)))
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		ctype := resp.Header.Get("Content-Type")
		isSSE := strings.Contains(ctype, "text/event-stream")
		log.Printf("zcode-bridge: acct=%s site=%s model=%s ctype=%s", a.ID, site, model, ctype)
		if h.clientWantsStream(body) {
			if isSSE {
				// Chief 裁定①：流内 200 body 命中 1113/1005 同样入账 + noteChat 冷却
				//（扫描 SSE 事件不消费流——zai 泵本来就逐帧读，钩子在帧级别判）。
				h.proxyZcodeSSEWithSpentCheck(w, resp.Body, model, a)
			} else {
				h.proxyZaiJSONAsSSE(w, resp.Body, model)
			}
		} else if isSSE {
			// 非流式客户端收到 SSE：聚合前同样逐帧扫（聚合器本身读帧）。
			agg := zaiAggregateSSEWithSpentCheck(resp.Body, model, func() {
				h.zcodeRecordSpent(a)
				h.noteChat(a.Provider, a.ID, errors.New("额度/配额耗尽（流内 200-body 1113/1005）"))
			})
			writeJSON(w, http.StatusOK, agg)
			resp.Body.Close()
		} else {
			raw, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_read", rerr.Error())
				return true
			}
			// 二相性观察 1（auxiliary 复核）：非流式 200 响应的 body 里也可能
			// 藏着额度耗尽（code 1113/1005——fork spentUp 本来就在 200 body 上
			// 判）。入账后回 429 语义给客户端（不让"耗尽"被当成成功空回合），
			// 流式路径不读 body 的约束保留（Known-Limitations）。
			if zcodeSpentUp(string(raw), resp.StatusCode) {
				h.zcodeRecordSpent(a)
				writeOpenAIError(w, http.StatusTooManyRequests, "quota_exhausted", "ZCode 额度/配额已耗尽（上游 200 + 1113/1005）")
				log.Printf("zcode-bridge: acct=%s 200-body 额度耗尽（1113/1005），已入账", a.ID)
				return true
			}
			out, terr := upstream.ZaiTranslateOutJSON(raw, model)
			if terr != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", terr.Error())
				return true
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
		}
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastZcodeErr = lastErr
	return false
}

// zcodeSpentUp fork spentUp（index.mjs L247-255）：HTTP 429 恒为额度类；
// 200 body 顶层 code 或 error.code 为 1113/1005 亦然。
func zcodeSpentUp(body string, status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	var doc struct {
		Code  any `json:"code"`
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil {
		return false
	}
	code := fmt.Sprint(doc.Code)
	ecode := fmt.Sprint(doc.Error.Code)
	return code == "1113" || code == "1005" || ecode == "1113" || ecode == "1005"
}

// zcodeRecordSpent 额度耗尽事件入账（Chief 口径 2）：账本以"每次耗尽 +1 次计"
// 记录（zcode 上游不明示消耗 credits 数，二相性只暴露"耗尽"这一事实）。
// 有分配上限时 PRE 门槛据此拦截；无上限时仅作观测。复用 qoder.CreditLedger
// 的计数语义（"provider/id" 键与 qoder 共用一个账本实例）。
func (h *Handler) zcodeRecordSpent(a *extstore.ExtAccount) {
	if h.qoderLedger == nil {
		return
	}
	h.qoderLedger.RecordCredits(qoder.AccountKey(string(a.Provider), a.ID), 1)
}

// qzRedact 日志脱敏（复用 qoder 包的规则——同为 token 形态串）。
func qzRedact(s string) string { return qoder.RedactSensitive(s) }

// timeNow 可注入测试的时间源（保留扩展位；当前直接用 time.Now）。
func timeNow() time.Time { return time.Now() }

// truncateStr 桥接层截断。
func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
