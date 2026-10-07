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
type bridgeOutcome struct {
	matched bool
	served  bool
	code    string
	detail  string
	lastErr string // 该平台的最近失败原因，供 isExhausted 判定是否该跨平台降级
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
		if h.qoderChatStream(w, r, bodyQM, qm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 Qoder 账号（面板-外部积分账号中添加 qoder 凭据后重试）"
		if h.lastQoderErr != "" {
			detail += "；最近失败原因: " + h.lastQoderErr
		}
		return bridgeOutcome{matched: true, code: "no_qoder_account", detail: detail, lastErr: h.lastQoderErr}
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
		if h.copilotChatStream(w, r, bodyCM, cm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 GitHub Copilot 账号（面板-自动化-外部平台中添加后重试）"
		if h.lastCopilotErr != "" {
			detail += "；最近失败原因: " + h.lastCopilotErr
		}
		return bridgeOutcome{matched: true, code: "no_copilot_account", detail: detail, lastErr: h.lastCopilotErr}
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
		if h.clineChatStream(w, r, bodyCM, cm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 Cline 账号（面板-添加账号-外部平台-Cline 授权后重试）"
		if h.lastClineErr != "" {
			detail += "；最近失败原因: " + h.lastClineErr
		}
		return bridgeOutcome{matched: true, code: "no_cline_account", detail: detail, lastErr: h.lastClineErr}
	}

	// AutoClaw 直连通道（autoclaw: 前缀模型）：走 extstore 里的 AutoClaw 账号。
	// 账号分属国内/国际两个地区（两套域名），地区随凭据走。
	if isAutoClawModel(bareModel) {
		am := strings.TrimPrefix(bareModel, autoclawModelPrefix)
		bodyAM := body
		if am != bareModel {
			bodyAM = rewriteModel(body, am)
		}
		if h.autoclawChatStream(w, r, bodyAM, am) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 AutoClaw 账号（面板-添加账号-外部平台-AutoClaw 手机号登录后重试）"
		if h.lastAutoClawErr != "" {
			detail += "；最近失败原因: " + h.lastAutoClawErr
		}
		return bridgeOutcome{matched: true, code: "no_autoclaw_account", detail: detail, lastErr: h.lastAutoClawErr}
	}

	// QClaw 直连通道（qclaw: 前缀模型）：走 extstore 里的 QClaw 账号
	// （微信扫码登录；对话用建出来的 sk key）。
	if isQClawModel(bareModel) {
		qm := strings.TrimPrefix(bareModel, qclawModelPrefix)
		bodyQM := body
		if qm != bareModel {
			bodyQM = rewriteModel(body, qm)
		}
		if h.qclawChatStream(w, r, bodyQM, qm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 QClaw 账号（面板-添加账号-外部平台-QClaw 微信扫码后重试）"
		if h.lastQClawErr != "" {
			detail += "；最近失败原因: " + h.lastQClawErr
		}
		return bridgeOutcome{matched: true, code: "no_qclaw_account", detail: detail, lastErr: h.lastQClawErr}
	}

	// Trae 直连通道（trae: 前缀模型）：走 extstore 里的 Trae 账号（本机回调授权）。
	// 上游是 SOLO 信封：出站体白名单重建、响应是自定义事件流，两侧都要转换。
	if isTraeModel(bareModel) {
		tm := strings.TrimPrefix(bareModel, traeModelPrefix)
		bodyTM := body
		if tm != bareModel {
			bodyTM = rewriteModel(body, tm)
		}
		if h.traeChatStream(w, r, bodyTM, tm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 Trae 账号（面板-添加账号-外部平台-Trae 浏览器授权后重试）"
		if h.lastTraeErr != "" {
			detail += "；最近失败原因: " + h.lastTraeErr
		}
		return bridgeOutcome{matched: true, code: "no_trae_account", detail: detail, lastErr: h.lastTraeErr}
	}

	// 小浣熊直连通道（raccoon: 前缀模型）：上游本身就是 OpenAI 兼容，
	// body 原样透传、SSE 标准 chunk，只有响应帧里的 model 要回写。
	if isRaccoonModel(bareModel) {
		rm := strings.TrimPrefix(bareModel, raccoonModelPrefix)
		bodyRM := body
		if rm != bareModel {
			bodyRM = rewriteModel(body, rm)
		}
		if h.raccoonChatStream(w, r, bodyRM, rm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的小浣熊账号（面板-添加账号-外部平台-小浣熊 微信扫码后重试）"
		if h.lastRaccoonErr != "" {
			detail += "；最近失败原因: " + h.lastRaccoonErr
		}
		return bridgeOutcome{matched: true, code: "no_raccoon_account", detail: detail, lastErr: h.lastRaccoonErr}
	}

	// CodeArts 直连通道（codearts: 前缀模型）：签名口径与积分/签到那条链路
	// 不同（对话不签 host），见 extprovider/codearts/chat.go 模块头。
	if isCodeArtsModel(bareModel) {
		cm := strings.TrimPrefix(bareModel, codeartsModelPrefix)
		bodyCM := body
		if cm != bareModel {
			bodyCM = rewriteModel(body, cm)
		}
		if h.codeartsChatStream(w, r, bodyCM, cm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 CodeArts 账号（面板-添加账号-外部平台-CodeArts 填 AK/SK 后重试）"
		if h.lastCodeArtsErr != "" {
			detail += "；最近失败原因: " + h.lastCodeArtsErr
		}
		return bridgeOutcome{matched: true, code: "no_codearts_account", detail: detail, lastErr: h.lastCodeArtsErr}
	}

	// Accio 直连通道（accio: 前缀模型）：上游是 ADK（Gemini 风格）信封，
	// 出站体与响应流两侧都要完整翻译。
	if isAccioModel(bareModel) {
		am := strings.TrimPrefix(bareModel, accioModelPrefix)
		bodyAM := body
		if am != bareModel {
			bodyAM = rewriteModel(body, am)
		}
		if h.accioChatStream(w, r, bodyAM, am) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 Accio 账号（面板-添加账号-外部平台-Accio 浏览器授权后重试）"
		if h.lastAccioErr != "" {
			detail += "；最近失败原因: " + h.lastAccioErr
		}
		return bridgeOutcome{matched: true, code: "no_accio_account", detail: detail, lastErr: h.lastAccioErr}
	}

	// TraeWork 直连通道（traework: 前缀模型）：上游是会话式协议
	// （建会话 → 开事件流 → 发消息），事件体形状不稳定，递归收集。
	if isTraeWorkModel(bareModel) {
		twm := strings.TrimPrefix(bareModel, traeworkModelPrefix)
		bodyTW := body
		if twm != bareModel {
			bodyTW = rewriteModel(body, twm)
		}
		if h.traeworkChatStream(w, r, bodyTW, twm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 TraeWork 账号（面板-添加账号-外部平台-TraeWork 粘贴客户端凭据）"
		if h.lastTraeWorkErr != "" {
			detail += "；最近失败原因: " + h.lastTraeWorkErr
		}
		return bridgeOutcome{matched: true, code: "no_traework_account", detail: detail, lastErr: h.lastTraeWorkErr}
	}

	// Marvis 直连通道（marvis: 前缀模型）：上游是标准 OpenAI 协议直通。
	// ⚠️ 上游按账号做自适应风控——本通道严禁压测。
	if isMarvisModel(bareModel) {
		mm := strings.TrimPrefix(bareModel, marvisModelPrefix)
		bodyMM := body
		if mm != bareModel {
			bodyMM = rewriteModel(body, mm)
		}
		if h.marvisChatStream(w, r, bodyMM, mm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 Marvis 账号（需从已登录的 Marvis 客户端抓包获取凭据后，在面板手工添加）"
		if h.lastMarvisErr != "" {
			detail += "；最近失败原因: " + h.lastMarvisErr
		}
		return bridgeOutcome{matched: true, code: "no_marvis_account", detail: detail, lastErr: h.lastMarvisErr}
	}

	// ima 直连通道（ima: 前缀模型）：会话式协议（先 init_session 再问答），
	// 自定义 SSE，事件名全大写——两侧都要转换，见 ima_bridge.go。
	// 桥接自己剥 ima: 前缀（模型清单查询需要完整名字）。
	if isIMAModel(bareModel) {
		if h.imaBridgeChatStream(w, r, body, bareModel) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 ima 账号（面板-添加账号-外部平台-ima 粘贴 Cookie 后重试）"
		if h.lastIMAErr != "" {
			detail += "；最近失败原因: " + h.lastIMAErr
		}
		return bridgeOutcome{matched: true, code: "no_ima_account", detail: detail, lastErr: h.lastIMAErr}
	}

	// Codex 订阅池直连（codex: 前缀）：本机 ~/.codex* 凭据 + Responses API。
	if isCodexModel(bareModel) {
		cm := strings.TrimPrefix(bareModel, codexModelPrefix)
		bodyCM := body
		if cm != bareModel {
			bodyCM = rewriteModel(body, cm)
		}
		if h.codexChatStream(w, r, bodyCM, cm) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "本机未发现可用 Codex 登录（codex login 后重试）"
		if h.lastCodexErr != "" {
			detail += "；最近失败: " + h.lastCodexErr
		}
		return bridgeOutcome{matched: true, code: "no_codex_account", detail: detail, lastErr: h.lastCodexErr}
	}

	// 免费 key 池（free:<provider>/<model>）：groq/zp/l7/or 四上游。
	if isFreeModel(bareModel) {
		if h.keyPoolChatStream(w, r, body, bareModel) {
			return bridgeOutcome{matched: true, served: true}
		}
		return bridgeOutcome{matched: true, code: "free_pool_unavailable", detail: "免费池不可用", lastErr: ""}
	}

	// Loomy 模型网关直连（loomy: 前缀）：讯飞模型上游 OpenAI 兼容，
	// 登录 session 即 Bearer。凭据走外部池 loomy-cli + session 文件。
	if isLoomyModel(bareModel) {
		if h.LoomyChatStream(w, r, body, bareModel) {
			return bridgeOutcome{matched: true, served: true}
		}
		detail := "没有可用的 Loomy 账号（面板-添加账号-Loomy 登录后重试）"
		if h.lastLoomyErr != "" {
			detail += "；最近失败原因: " + h.lastLoomyErr
		}
		return bridgeOutcome{matched: true, code: "no_loomy_account", detail: detail, lastErr: h.lastLoomyErr}
	}

	// Z.AI / 智谱 GLM API Key 通道（zai: 前缀）：Anthropic 兼容端点转发，
	// OpenAI 双向翻译；凭据 config schedule.zai（x-api-key，免验证码）。
	if strings.HasPrefix(bareModel, zaiModelPrefix) {
		if h.ZaiChatStream(w, r, body, bareModel) {
			return bridgeOutcome{matched: true, served: true}
		}
		// 有具体失败原因时以它为主（可能是某模型上游抖动/额度用完的按模型短冷却，
		// 并非真的没有 Key）；只有确无凭据线索时才回落「未配置」提示。
		detail := "没有可用的 Z.AI / 智谱 通道（配置 schedule.zai 或面板添加账号后重试）"
		if h.lastZaiErr != "" {
			detail = "Z.AI 通道暂不可用：" + h.lastZaiErr
		}
		return bridgeOutcome{matched: true, code: "no_zai_key", detail: detail, lastErr: h.lastZaiErr}
	}

	// 没有任何桥接认这个前缀 → 交给调用方（腾讯池路径）。
	return bridgeOutcome{}
}
