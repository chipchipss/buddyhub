package server

import (
	"net/http"
	"strings"
)

// dispatch.go 外部平台对话桥接的统一入口。
//
// 这 14 个块原样来自 chatCompletions（纯抽取，行为不变）——抽出的唯一目的是
// 让调用方能**重入**：某条通道额度耗尽时可以拿同族的另一个前缀再走一遍
// （见 handler.go 的重试环）。抽取本身不该改变任何语义，所以每段代码原样搬运。
//
// 返回值语义（与桥接的 bool 约定一一对应）：
//   - matched+served ⇒ 桥接已写响应（成功、503、以及埋在桥接内部的 400 全算），
//     调用方直接 return，**不可重试**
//   - matched: true ⇒ 有桥接认这个前缀但没服务成（**什么都没写**），调用方可
//     写 503，或在判定为额度耗尽时换候选再试
//   - 两者皆 false ⇒ 没有前缀匹配 → 落到腾讯池（裸名路径）
//
// ── 为什么 lastErr 由桥接自己返回，而不是写在 Handler 字段上 ──────────────
// 早先每条桥接把「本次失败原因」写进挂在 *Handler 单例上的 last<Provider>Err
// 字段，dispatch 再读回来拼 503 文案。那是个**跨请求共享的字段**：两个并发
// 请求同时写同一条通道时，B 的失败原因会盖掉 A 的（A 的 503 文案里出现 B 的
// 上游错误），并且每次读写都是一次数据竞争（`go test -race` 实测复现，
// 见 lasterr_race_test.go 的并发回归用例）。
//
// 现在每个桥接返回自己的 bridgeOutcome（含本请求的 lastErr），dispatch 只做
// 前缀识别 + 出站体重写 + 原样转发。请求级局部量天然满足两条原语义：
//   - 「本请求从零开始记」：每次调用新建 outcome，不存在上一次的残留；
//   - 「上一位用户的失败原因不挂进这一次」：A 与 B 各拿各的 outcome，
//     互不可见。
type bridgeOutcome struct {
	matched bool
	served  bool
	code    string
	detail  string
	lastErr string // 该平台**本次**失败原因，供 isExhausted 判定是否该跨平台降级
}

// bridgeServed 桥接已写响应（成功、503、以及埋在桥接内部的 400/404 全算），
// 调用方直接 return，**不可重试**。
func bridgeServed() bridgeOutcome {
	return bridgeOutcome{matched: true, served: true}
}

// bridgeServedWithReason 同上，另把本次失败原因带回来。
//
// 「模型不存在 / 未配置账号」这类**提前 return true** 的路径也走它：虽然
// dispatch 在 served=true 时不读 lastErr，但原因一旦丢掉就没处可查——
// 迁移前这些路径同样会写 Handler 上的共享 Err 字段，这里保持等价。
func bridgeServedWithReason(reason string) bridgeOutcome {
	return bridgeOutcome{matched: true, served: true, lastErr: reason}
}

// bridgeMiss 桥接认了这个前缀但没服务成（**什么都没写**）：调用方可写 503，
// 或在判定为额度耗尽时换候选再试。
//
// noAccountMsg / label / code 是该平台的固定口径，随桥接一起搬迁到这里；
// lastErr 是本次请求级的失败原因（空 = 一次上游都没碰过）。
func bridgeMiss(code, noAccountMsg, label, lastErr string) bridgeOutcome {
	return bridgeOutcome{
		matched: true,
		code:    code,
		detail:  bridgeDetail(noAccountMsg, label, lastErr),
		lastErr: lastErr,
	}
}

// tryBridge 按前缀把请求交给对应桥接。bareModel 是**带前缀**的模型名
// （前缀只剥 cn:/global: 这种 realm，平台前缀由各块自己剥）。
func (h *Handler) tryBridge(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bridgeOutcome {
	// Qoder 直连通道（qoder: 前缀模型）：独立于腾讯池，走 extstore 里的
	// Qoder 账号（api2-v2 新版协议纯 Bearer）。找不到可用账号时回 404 提示，
	// 不静默回落腾讯池——模型名就是路由协议，回落会把语义搞乱。
	if isQoderModel(bareModel) {
		qm := strings.TrimPrefix(bareModel, qoderModelPrefix)
		bodyQM := body
		if qm != bareModel {
			bodyQM = rewriteModel(body, qm)
		}
		return h.qoderChatStream(w, r, bodyQM, qm)
	}

	// GitHub Copilot 直连通道（copilot: 前缀模型）：独立于腾讯池，走 extstore 里的
	// Copilot 账号（设备流登录 → Copilot token 自动续期）。上游即 OpenAI 协议，
	// 网关只做鉴权与透传。找不到可用账号时回 503 提示，不静默回落腾讯池。
	if isCopilotModel(bareModel) {
		cm := strings.TrimPrefix(bareModel, copilotModelPrefix)
		bodyCM := body
		if cm != bareModel {
			bodyCM = rewriteModel(body, cm)
		}
		return h.copilotChatStream(w, r, bodyCM, cm)
	}

	// Cline 直连通道（cline: 前缀模型）：独立于腾讯池，走 extstore 里的 Cline
	// 账号（WorkOS 设备授权）。前缀后面**保留**上游的计费池前缀
	// （cline-free/ / cline-pass/ / cline-cloud/）——那是上游的计费通道选择器。
	if isClineModel(bareModel) {
		cm := strings.TrimPrefix(bareModel, clineModelPrefix)
		bodyCM := body
		if cm != bareModel {
			bodyCM = rewriteModel(body, cm)
		}
		return h.clineChatStream(w, r, bodyCM, cm)
	}

	// AutoClaw 直连通道（autoclaw: 前缀模型）：走 extstore 里的 AutoClaw 账号。
	// 账号分属国内/国际两个地区（两套域名），地区随凭据走。
	if isAutoClawModel(bareModel) {
		am := strings.TrimPrefix(bareModel, autoclawModelPrefix)
		bodyAM := body
		if am != bareModel {
			bodyAM = rewriteModel(body, am)
		}
		return h.autoclawChatStream(w, r, bodyAM, am)
	}

	// QClaw 直连通道（qclaw: 前缀模型）：走 extstore 里的 QClaw 账号
	// （微信扫码登录；对话用建出来的 sk key）。
	if isQClawModel(bareModel) {
		qm := strings.TrimPrefix(bareModel, qclawModelPrefix)
		bodyQM := body
		if qm != bareModel {
			bodyQM = rewriteModel(body, qm)
		}
		return h.qclawChatStream(w, r, bodyQM, qm)
	}

	// Trae 直连通道（trae: 前缀模型）：走 extstore 里的 Trae 账号（本机回调授权）。
	// 上游是 SOLO 信封：出站体白名单重建、响应是自定义事件流，两侧都要转换。
	if isTraeModel(bareModel) {
		tm := strings.TrimPrefix(bareModel, traeModelPrefix)
		bodyTM := body
		if tm != bareModel {
			bodyTM = rewriteModel(body, tm)
		}
		return h.traeChatStream(w, r, bodyTM, tm)
	}

	// 小浣熊直连通道（raccoon: 前缀模型）：上游本身就是 OpenAI 兼容，
	// body 原样透传、SSE 标准 chunk，只有响应帧里的 model 要回写。
	if isRaccoonModel(bareModel) {
		rm := strings.TrimPrefix(bareModel, raccoonModelPrefix)
		bodyRM := body
		if rm != bareModel {
			bodyRM = rewriteModel(body, rm)
		}
		return h.raccoonChatStream(w, r, bodyRM, rm)
	}

	// CodeArts 直连通道（codearts: 前缀模型）：签名口径与积分/签到那条链路
	// 不同（对话不签 host），见 extprovider/codearts/chat.go 模块头。
	if isCodeArtsModel(bareModel) {
		cm := strings.TrimPrefix(bareModel, codeartsModelPrefix)
		bodyCM := body
		if cm != bareModel {
			bodyCM = rewriteModel(body, cm)
		}
		return h.codeartsChatStream(w, r, bodyCM, cm)
	}

	// Accio 直连通道（accio: 前缀模型）：上游是 ADK（Gemini 风格）信封，
	// 出站体与响应流两侧都要完整翻译。
	if isAccioModel(bareModel) {
		am := strings.TrimPrefix(bareModel, accioModelPrefix)
		bodyAM := body
		if am != bareModel {
			bodyAM = rewriteModel(body, am)
		}
		return h.accioChatStream(w, r, bodyAM, am)
	}

	// TraeWork 直连通道（traework: 前缀模型）：上游是会话式协议
	// （建会话 → 开事件流 → 发消息），事件体形状不稳定，递归收集。
	if isTraeWorkModel(bareModel) {
		twm := strings.TrimPrefix(bareModel, traeworkModelPrefix)
		bodyTW := body
		if twm != bareModel {
			bodyTW = rewriteModel(body, twm)
		}
		return h.traeworkChatStream(w, r, bodyTW, twm)
	}

	// Marvis 直连通道（marvis: 前缀模型）：上游是标准 OpenAI 协议直通。
	// ⚠️ 上游按账号做自适应风控——本通道严禁压测。
	if isMarvisModel(bareModel) {
		mm := strings.TrimPrefix(bareModel, marvisModelPrefix)
		bodyMM := body
		if mm != bareModel {
			bodyMM = rewriteModel(body, mm)
		}
		return h.marvisChatStream(w, r, bodyMM, mm)
	}

	// ima 直连通道（ima: 前缀模型）：会话式协议（先 init_session 再问答），
	// 自定义 SSE，事件名全大写——两侧都要转换，见 ima_bridge.go。
	// 桥接自己剥 ima: 前缀（模型清单查询需要完整名字）。
	if isIMAModel(bareModel) {
		return h.imaBridgeChatStream(w, r, body, bareModel)
	}

	// ZCode 通道（zcode: 前缀模型）：Anthropic Messages 兼容端点（z.ai /
	// bigmodel 双站点，zcode:cn/ 子前缀选 bigmodel），翻译复用 zai 桥。
	if isZcodeModel(bareModel) {
		bodyZC := body
		if _, m := zcodeSiteFor(strings.TrimPrefix(bareModel, zcodeModelPrefix)); m != "" && m != bareModel {
			bodyZC = rewriteModel(body, m)
		}
		return h.ZcodeChatStream(w, r, bodyZC, strings.TrimPrefix(bareModel, zcodeModelPrefix))
	}

	// Cursor 通道（cursor: 前缀模型）：对话已接线（RunSSE/BidiAppend +
	// OpenAI chunk 转换，见 cursor_bridge.go）。**experimental**：假流端到端
	// 验证过但未连过真实上游，真连步骤见 README 的 Cursor KNOWN-LIMITATIONS。
	if isCursorModel(bareModel) {
		bodyCU := body
		m := strings.TrimPrefix(bareModel, cursorModelPrefix)
		if m != bareModel {
			bodyCU = rewriteModel(body, m)
		}
		return h.CursorChatStream(w, r, bodyCU, m)
	}

	// Codex 订阅池直连（codex: 前缀）：本机 ~/.codex* 凭据 + Responses API。
	if isCodexModel(bareModel) {
		cm := strings.TrimPrefix(bareModel, codexModelPrefix)
		bodyCM := body
		if cm != bareModel {
			bodyCM = rewriteModel(body, cm)
		}
		return h.codexChatStream(w, r, bodyCM, cm)
	}

	// 免费 key 池（free:<provider>/<model>）：groq/zp/l7/or 四上游。
	// 这条桥接不参与 last*Err 迁移（它没有平台级失败原因字段，且每条路径
	// 都已自行写响应），故仍返回 bool，由这里补成 outcome。
	if isFreeModel(bareModel) {
		if h.keyPoolChatStream(w, r, body, bareModel) {
			return bridgeServed()
		}
		return bridgeOutcome{matched: true, code: "free_pool_unavailable", detail: "免费池不可用"}
	}

	// Loomy 模型网关直连（loomy: 前缀）：讯飞模型上游 OpenAI 兼容，
	// 登录 session 即 Bearer。凭据走外部池 loomy-cli + session 文件。
	if isLoomyModel(bareModel) {
		return h.LoomyChatStream(w, r, body, bareModel)
	}

	// Z.AI / 智谱 GLM API Key 通道（zai: 前缀）：Anthropic 兼容端点转发，
	// OpenAI 双向翻译；凭据 config schedule.zai（x-api-key，免验证码）。
	if strings.HasPrefix(bareModel, zaiModelPrefix) {
		return h.ZaiChatStream(w, r, body, bareModel)
	}

	// 没有任何桥接认这个前缀 → 交给调用方（腾讯池路径）。
	return bridgeOutcome{}
}

// bridgeDetail 把「这条桥接认了这个前缀、但没服务成」写成一句符合事实的话。
//
// 每条桥接进入时 lastErr 都是**请求级**空串起步，所以这个字段能分辨两件事：
//   - 空 = 一次上游都没碰过，池里确实没有可用账号 → 这时才说「没有可用账号」
//   - 非空 = 账号全试过了且全失败 → 「没有可用账号」是句假话。实测就是这么误事的：
//     copilot 账号在池里、另一个模型同一分钟回 200，界面却写着「没有可用的
//     GitHub Copilot 账号」，用户于是去反复重登一个本来登录成功的账号。
func bridgeDetail(noAccountMsg, label, lastErr string) string {
	if lastErr == "" {
		return noAccountMsg
	}
	return label + " 通道本次不可用（池内账号均已尝试并失败）：" + lastErr
}
