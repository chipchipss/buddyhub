package server

// exhaust.go 判定「这条失败是否该换平台」。
//
// ── 为什么不能直接用 upstream.Classify ──────────────────────────────
// `upstream.Classify`（client.go:447）是为**腾讯池的号级惩罚**设计的，它的
// 429 语义是「限流，软冷却不换号」：只有 429 + 业务码 14018 才判耗尽，
// 其余裸 429（哪怕 body 写着 quota exhausted）恒返 ErrSoftRate——
// 且这条语义被 `client_test.go:83-97` 钉住了，**不能改**。
//
// 跨平台降级要的是另一个问题的答案：「这个平台现在还用得了吗」。用不上就换，
// 与「要不要罚这个号」无关。所以这里用**耗尽优先**的判据（与
// `internal/zai/classify.go:80` 同源：`402 || exhaustKeywords`），
// 外加 429 —— 用户口径里 429 也属「额度类错误」，且当前平台被限流时
// 换一家正是最直接的解法。
//
// 这是仓库里**有意的第三套枚举**：`upstream.Classify`（号级惩罚）、
// `zai.Classify`（Z.AI 账号状态机）、本文件（跨平台降级）。三者语义不同，
// 不要「统一」掉。
import (
	"net/http"
	"regexp"
	"strings"
)

// exhaustKeywords 与 zai/classify.go:48 同表（那份是私有且带 http.Header 签名，
// 拿过来用会让 internal/server 依赖 internal/zai，故在此复制并注明来源）。
var exhaustKeywords = []string{
	"quota", "insufficient", "balance", "exhaust",
	"额度", "余额不足", "积分不足",
}

// statusRe 从桥接的失败文案里取 HTTP 状态码。
//
// 11 条桥接把 status 与 body 拼进了一个字符串，三种形状：
//
//	"上游 HTTP 429：<json前200字节>"   ← trae/accio/autoclaw/cline/copilot/qclaw/codearts
//	"上游返回 429: …"                  ← raccoon（UpstreamError.Error）
//	"HTTP 429: …"                      ← qoder/loomy
//
// traework 既无状态码也无 body → 匹配不到 → isExhausted 返回 false（保守）。
var statusRe = regexp.MustCompile(`(?:HTTP\s|返回\s|上游\s)(\d{3})`)

// modelRateRe 6004 —— 腾讯的「该模型在此账号被限流」，换一个平台正好解决。
//
// 匹配 `"code":6004` 而不是 `6004`：JSON 里它是**数字**（写成 `"6004"`
// 就永远匹配不上——这个坑第一版就踩了）；而裸 `6004` 又太宽（模型 id 里
// 出现这四位也会误判）。
var modelRateRe = regexp.MustCompile(`"code":\s*6004`)

// isExhausted 该失败是否意味着「这个平台现在用不了，该换下一个」。
//
// 返回 false 的情形同样重要：
//   - 取不出状态码（traework 那类包装文案）→ 判不出，**不降级**
//   - 5xx / 超时 / 模型不存在 / 400 参数错误 → 换平台大概率没用，且换过去
//     可能拿到语义不同的模型，宁可把真实错误透给客户端
func isExhausted(msg string) bool {
	if msg == "" {
		return false
	}
	m := statusRe.FindStringSubmatch(msg)
	if m == nil {
		return false // 取不出状态码 → 保守不降级
	}
	switch m[1] {
	case "402", "429":
		return true
	}
	lower := strings.ToLower(msg)
	for _, k := range exhaustKeywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return modelRateRe.MatchString(msg)
}

// exhaustedForTerminal 腾讯池**轮转到底**时的降级判据。
//
// 与 isExhausted 的差别：这里手里有 HTTP 状态码与 OpenAI 错误码，不必去文案里抠。
//
// ── 为什么**不**放行 `no_healthy_account` ──────────────────────────
// 它是**本地调度状态**（号全在冷却 / 熔断 / 禁用），不是上游告诉我们的额度信号。
// 用户定的口径是「只在额度耗尽时降级」——而这个 code 有太多别的成因：
// 池被手工停用、全局熔断、压根没配账号。对它降级等于把配置问题换个平台掩盖掉，
// 而且它会顺带触发一次**模型目录索引构建**（要拉 10 个平台的目录，
// 冷缓存单个 15–20s）——在"请求已经失败"的那一刻再等这么久是最糟的时机。
//
// 三种会放行的：
//   - `rate_limit_exceeded`（429）：上游明确说限流了，换一家不在这个窗口里
//   - 429 / 402 作为 HTTP 状态传进来（同上，客户端口径里的额度类错误）
//   - 其余交给 isExhausted 按上游原文里的状态码与关键词判
func exhaustedForTerminal(status int, code, msg string) bool {
	if code == "rate_limit_exceeded" {
		return true
	}
	if status == http.StatusTooManyRequests || status == http.StatusPaymentRequired {
		return true
	}
	return isExhausted(msg)
}
