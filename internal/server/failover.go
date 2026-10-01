package server

// failover.go 跨平台额度降级的编排。
//
// 用户要的是「只要有任何一个平台的任何一个号有额度，我都可以放心使用」。
// 实现分两半：本文件负责**何时换、换成谁、换完怎么让人看出来**；
// 「同族有哪些候选」由 modelfamily.go 算，「这次失败是不是额度耗尽」
// 由 exhaust.go 判。
//
// ── 两个刻意的保守 ──────────────────────────────────────────────────
//  1. **只在额度耗尽时换**（`isExhausted`）。5xx / 超时 / 参数错换过去大概率
//     还是同样的错，而且会拿到**语义不同**的模型——那种"看起来成功了"的降级
//     比直接报错更难排查。
//  2. **逐候选重查 `platformAllowed`**。准入门只在请求开头对原始模型名查过
//     一次（handler.go:890），不重查的话，一个只授权了 raccoon 的 API key
//     会在降级后打到 zai 上——那是越权。多 Key 体系关闭时恒通过，与现状一致。
import (
	"log"
	"net/http"
	"strings"
)

// failoverToFamily 额度耗尽时，按剩余额度依次尝试同族的其它平台模型。
//
// 成功返回 true（响应已写，调用方直接 return）；一个都没成就返回 false，
// 调用方继续写**原来那条 503** —— 客户端看到的错误码与原因不因降级而改变。
func (h *Handler) failoverToFamily(w http.ResponseWriter, r *http.Request, body []byte, from string) bool {
	alts := h.familyAlts(from)
	if len(alts) == 0 {
		return false
	}
	for _, alt := range alts {
		// 响应头要在写响应**之前**设，所以这里先设、失败再清。
		w.Header().Set("X-Served-By", PlatformOf(alt))
		w.Header().Set("X-Failover-From", PlatformOf(from))

		if !platformAllowed(r, h.loadLive(), alt) {
			log.Printf("panel: 降级候选 %s 未被该 API Key 授权，跳过", alt)
			continue
		}
		if out := h.tryBridge(w, r, body, alt); out.served {
			h.noteFailover(from, alt, out.lastErr)
			return true
		}
	}
	// 一个都没成：把上一轮设的头摘掉，否则接下来那条 503 会带着
	// `X-Served-By` 谎称自己来自某个平台。
	w.Header().Del("X-Served-By")
	w.Header().Del("X-Failover-From")
	return false
}

// noteFailover 记一次跨平台降级。
//
// 回包 body 的 `model` **保持客户端请求的名字**（刻意不告知：严格校验 model
// 的客户端拿到另一个名字会直接报错），所以可追溯性只能落在日志与响应头上。
// 独立成行而不并进流水表——桥接路径本来就**不进**那张表（见 handler.go
// newChatStat 的位置），硬塞进去会改变整张表的量级与语义。
func (h *Handler) noteFailover(from, to, reason string) {
	log.Printf("panel: 额度耗尽降级 %s → %s（%s）", from, to, firstLine(reason))
}

// firstLine 只取原因串的第一行：上游 body 常带换行与大量 JSON，
// 整段塞进日志会把这一行冲垮。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}
