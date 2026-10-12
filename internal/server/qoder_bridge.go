// Package server: qoder_bridge.go — Qoder 模型直连路由。
//
// 模型名带 "qoder:" 前缀（如 qoder:lite、qoder:qfmodel）时走本路径：
// 从外部账号表（extstore）取一个启用的 qoder 账号 → 自动续期（NeedsRefresh
// 或 401 触发）→ ChatStream（api2-v2 纯 Bearer 新版协议）→ SSE 透传/聚合。
// 与腾讯池完全独立：Qoder 凭据失败只在本路径内轮换下一个 Qoder 账号。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/dingtalk"
	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/session"
	"github.com/chipchipss/buddyhub/internal/upstream"
	"github.com/chipchipss/buddyhub/internal/usage"
)

// QoderChatStream 直接向客户端透传 Qoder 的 OpenAI 兼容 SSE。
// 返回 false = 未找到可用账号（调用方报错，不回落腾讯池）。
func (h *Handler) qoderChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	// 先清：那是**上一次请求**的事实，不该挂进本次的失败提示里。
	h.lastQoderErr = ""
	if h.cfg.ExtAccounts == nil {
		return false
	}
	mgr := h.extManager
	cli := qoder.New()
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PQoder && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	res := qoder.LoadResilienceConfig()
	// 阶段 3：客户端会话键 → 上游 session_id 的稳定映射（fork sessionManager
	// 语义：账号不是对话边界，无客户端键时 ephemeral 隔离，绝不账号级共享）。
	sessKey := session.ExtractKey(body)
	// 阶段 4：PRE 额度门（fork apiKeyPolicy PRE-request 语义）——每个账号在
	// 撞上游前先判额度；QuotaUnknown = fail-open 放行（配额基础设施不可用
	// 绝不能挡正常流量）。
	tried := map[string]bool{}
	for _, a := range accounts {
		var cred qoder.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// 到期前自动续期；续期失败继续尝试下一个账号。
		if cred.NeedsRefresh() {
			fresh, rerr := cli.RefreshForSite(r.Context(), qoder.SiteByID(cred.SiteID), &cred)
			var gone *authcore.SignInGone
			var expired *qoder.SignInExpiredError
			switch {
			case rerr == nil:
				if raw, merr := json.Marshal(fresh); merr == nil {
					mgr.ReplaceCred(a.Provider, a.ID, raw)
					cred = *fresh
					_ = mgr.MarkSignInActive(a.Provider, a.ID) // renewed → 清 lapsed
				}
			case errors.As(rerr, &gone) && gone.Expired, errors.As(rerr, &expired):
				// Qoder 明确拒绝刷新（401/403）：登录态已失效，标记 lapsed 并跳过。
				note := "Qoder 登录已失效（refresh 被拒），请重新设备授权登录"
				_ = mgr.MarkSignInExpired(a.Provider, a.ID, note)
				h.noteChat(a.Provider, a.ID, errors.New(note))
				lastErr = note
				tried[a.ID] = true
				continue
			default:
				log.Printf("qoder-bridge: %s 续期失败: %v", a.ID, rerr)
				// 网络错误/5xx：**保留登录态**（绝不固化禁用），走普通失败路径换号。
				lastErr = "续期失败: " + rerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				tried[a.ID] = true
				continue
			}
		}

		// 阶段 4 PRE 额度门：分配上限内的已用额度（精确账本优先、余额差估算
		// 兜底）达到上限即跳过该号（fork exhaustedConnectionIds 语义）。
		// 估算数据缺失（QuotaUnknown）一律放行——fail-open。
		if gate := h.qoderQuotaGate(a); gate == qoder.QuotaExhausted {
			log.Printf("qoder-bridge: acct=%s 额度已耗尽（分配上限内），跳过", a.ID)
			lastErr = "账号额度已耗尽（分配上限内）"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			tried[a.ID] = true
			// 阶段 5：耗尽告警（fork notifyApiKeyQuotaExhausted——活跃态只报一次）。
			if h.qoderLedger != nil {
				key := qoder.AccountKey(string(a.Provider), a.ID)
				h.qoderAlertQuotaExhausted(a, h.qoderLedger.UsedCredits(key), h.qoderLedger.AllocationLimit(key), 0)
			}
			continue
		}
		tried[a.ID] = true

		// 建流（带弹性：排队原位重试、首 token 超时独立预算、换号前原账号先扛）。
		rc, status, envErr, fatal, ferr := h.qoderResilientStreamCtx(r.Context(), cli, &cred, a, body, res, sessKey)
		if fatal {
			// 终局错误（首信封 provider/overflow 等）：直接写 OpenAI 错误给客户端，
			// 不再换号——这类错误换号也无法解决（如上下文超限）。
			if envErr != nil {
				writeOpenAIError(w, envErr.Status, envErr.Code, envErr.Message)
				log.Printf("qoder-bridge: acct=%s 首信封终局错误 code=%s: %s", a.ID, envErr.Code, envErr.Message)
			}
			return true
		}
		if ferr != nil {
			lastErr = ferr.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			// 日志脱敏：body 可能夹带 token/授权回显，只保留状态码和清理后的文案。
			log.Printf("qoder-bridge: %s chat 失败 (HTTP %d): %s", a.ID, status, qoder.RedactSensitive(ferr.Error()))
			continue // 换下一个 Qoder 账号
		}

		// 成功建流：按客户端是否要流式决定透传或聚合（resilient 泵内做首帧判定）。
		log.Printf("qoder-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.qoderRecordCall() // 阶段 5：闲时告警的事实源（成功调用即记）
		if h.clientWantsStream(body) {
			// 阶段 4 POST 记账（流式）：泵尾把上游 usage 帧累加进 bridgeUsage，
			// 结束后一次性入账（fork recordKeyQuotaUsage fire-and-forget 语义）。
			h.bridgeUsageReset(a.ID)
			h.proxyQoderResilientSSE(w, r, rc, res, a.ID, body, sessKey)
			h.qoderRecordUsage(a, sessKey, bareModel, h.bridgeUsageTake(a.ID))
		} else {
			agg, err := upstream.Aggregate(rc)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
				h.noteChat(a.Provider, a.ID, errOf("聚合失败: "+err.Error()))
				return true
			}
			// 阶段 4 POST 记账：非流式一次性拿全量 usage。
			if u, ok := qoder.ParseChatUsage(agg); ok {
				h.qoderRecordUsage(a, sessKey, bareModel, u)
			}
			writeJSON(w, http.StatusOK, agg)
		}
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastQoderErr = lastErr
	return false // 全部 Qoder 账号不可用
}

// qoderResilientStream *http.Request 签名保留兼容（主路径委托 Ctx 版）。
func (h *Handler) qoderResilientStream(r *http.Request, cli *qoder.Client, cred *qoder.Credential,
	a *extstore.ExtAccount, body []byte, res qoder.ResilienceConfig) (
	rc io.ReadCloser, status int, envErr *qoder.EnvelopeError, fatal bool, ferr error,
) {
	return h.qoderResilientStreamCtx(r.Context(), cli, cred, a, body, res, "")
}

// qoderResilientStreamCtx 弹性建流的 context 直通版：续跑请求复用主请求的
// context——客户端断开时续跑同步取消，不留孤儿上游流。
// sessKey 是客户端会话键（session.ExtractKey 结果，空=无客户端键）：
// 阶段 3 用它派生稳定的上游 session_id（fork 隔离语义，见 qoder/session.go）。
func (h *Handler) qoderResilientStreamCtx(ctx context.Context, cli *qoder.Client, cred *qoder.Credential,
	a *extstore.ExtAccount, body []byte, res qoder.ResilienceConfig, sessKey string) (
	rc io.ReadCloser, status int, envErr *qoder.EnvelopeError, fatal bool, ferr error,
) {
	ensureKeepalive := func() {
		// 建流阶段排队等待的心跳由调用方 ResponseWriter 处理不安全（头未发），
		// 这里只做空操作占位：真实心跳在 proxyQoderResilientSSE 的流内空闲检测里。
	}
	_ = ensureKeepalive
	// 阶段 3 会话隔离：会话键 → 稳定上游 session_id。无客户端键时 SessionIDFor
	// 返回一次性 UUID（fork ephemeral 语义——隔离优先，绝不账号级共享）。
	meta := qoder.ChatMeta{SessionID: h.qoderSessionID(sessKey)}
	for attempt := 1; attempt <= res.QueueMaxAttempts; attempt++ {
		meta.RequestID = ""  // 每次重试重新生成，防上游 Duplicate 判定
		meta.RequestSet = "" // 同上；session_id 保持稳定（会话隔离语义）
		outBody, err := qoder.BuildChatBody(body, meta)
		if err != nil {
			return nil, 0, nil, false, err
		}
		cur, st, err := cli.ChatStream(ctx, cred, outBody, meta)
		if err == nil {
			// HTTP 2xx：做首帧判定——首个 data 帧可能是排队/错误信封。
			cur, envErr2, signal, retryable := qoder.ClassifyFirstEnvelope(cur, res)
			if envErr2 != nil {
				return nil, st, envErr2, true, nil
			}
			if retryable {
				// 首帧即排队/超时标记：按其预算继续原位重试。
				if !h.qoderWait(ctx, res, attempt, signal.Reason, signal.IsTimeout) {
					return nil, st, nil, false, errors.New("客户端断开，放弃重试")
				}
				continue
			}
			return cur, st, nil, false, nil
		}
		ue, ok := err.(*qoder.UpstreamError)
		if !ok {
			// 网络层错误：少量原位重试（复用排队预算的前 3 次），其余换号。
			if attempt <= 3 && ctx.Err() == nil {
				if !h.qoderWait(ctx, res, attempt, "network: "+err.Error(), false) {
					return nil, 0, nil, false, err
				}
				continue
			}
			return nil, 0, nil, false, err
		}
		info := qoder.ClassifyUpstreamError(ue.Status, ue.Body)
		if !info.Queued {
			return nil, ue.Status, nil, false, ue
		}
		if info.IsTimeout && res.FirstTokenFallback == qoder.FallbackOff {
			return nil, ue.Status, nil, false, ue
		}
		// 排队/超时：原位等待后重试（排队识别优先于普通失败——fork 语义）。
		if !h.qoderWait(ctx, res, attempt, info.Reason, info.IsTimeout) {
			return nil, ue.Status, nil, false, ue
		}
	}
	return nil, 0, nil, false, errors.New("qoder 排队重试次数耗尽")
}

// qoderWait 弹性等待：日志 + 退避 sleep（带抖动）。返回 false = 客户端已断开。
// isTimeout=true 时按首 token 超时策略取延迟，否则按排队退避。
func (h *Handler) qoderWait(ctx context.Context, res qoder.ResilienceConfig, attempt int, reason string, isTimeout bool) bool {
	var d time.Duration
	if isTimeout {
		d = res.TimeoutDelayFor(attempt)
	} else {
		d = res.QueueDelay(attempt)
	}
	log.Printf("qoder-bridge: 第 %d 次等待重试（%s），%.1fs 后重试", attempt, reason, d.Seconds())
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// proxyQoderResilientSSE 流式兜底泵（阶段 2，语义对齐 fork wrapQoderSSE）：
//   - 全程逐帧分类（ClassifyMidStreamData）：正文透传、排队帧发注释心跳、
//     错误信封发内联 error 帧+[DONE]、[DONE] 哨兵终结；
//   - 五态终止状态机（clean_done/premature_done/upstream_error/finish_without_done/
//     premature_eof），EOF 分类对齐 fork classifyEndOfStream；
//   - 终帧（finish_reason）暂存不转发（heldFinish），确认无续跑才释放——
//     自动续跑前置，防客户端在续跑请求间隙结束回合并 abort；
//   - 自动续跑：全 AND 触发（fork L1554-1563），续跑请求重新生成 request_id
//     防上游 Duplicate 判定，计数走 QODER_AUTO_CONTINUE_MAX（默认 1，0=关）；
//   - 审阅意见回链（Chief 三条）：①abort/写失败后不发错误帧、不泄终帧——
//     所有写路径先探写错误，断开即静默收流；②held 释放四个显式测试用例
//     （无续跑顺序/有续跑哨兵复用/abort 不释放/信封错误置空顺序）；
//     ③"只有正常 EOF 可当缺 [DONE]"——transport error 即使见过 finish_reason
//     也算失败，held 帧不得把截断答案变成成功回合（反例测试钉死）。
func (h *Handler) proxyQoderResilientSSE(w http.ResponseWriter, r *http.Request, rc io.ReadCloser, res qoder.ResilienceConfig, acctID string, origBody []byte, sessKey string) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}

	var (
		term          qoder.StreamTermination
		heldFinish    string // 暂存的终帧原文（含 "data: " 前缀，不含行尾换行）
		doneEmitted   bool
		clientGone    bool // 客户端断开标志：置位后一切写路径静默、不发错误帧
		tracker       qoder.TurnTracker
		continuations int
		hasTools      = qoder.HasToolsRequest(origBody)
	)

	// writeLine 唯一写出口：写失败即置 clientGone（审阅①：断开后静默，
	// 不发错误帧、不泄终帧——fork 无此闸，Go 侧 ResponseWriter 语义要求）。
	writeLine := func(s string) bool {
		if clientGone {
			return false
		}
		if _, err := w.Write([]byte(s)); err != nil {
			clientGone = true
			return false
		}
		return true
	}

	// emitInlineError 内联错误帧 + [DONE]（fork L1417-1430：头已上线后唯一的
	// 诚实信号，绝不补 finish_reason=stop——否则客户端记成功回合带污染历史重试）。
	// 帧构造统一走 qoder.BuildMidStreamErrorFrame（偏差1 修复：rate_limit 的
	// retry-after 提示分支从此生效，与 fork withRetryAfterHint 语义对齐）。
	emitInlineError := func(message, code string) {
		if doneEmitted || clientGone {
			return
		}
		raw := qoder.BuildMidStreamErrorFrame(code, qoder.RedactSensitive(message))
		if !writeLine("data: " + raw + "\n\ndata: [DONE]\n\n") {
			return
		}
		flusher.Flush()
		doneEmitted = true
		term = qoder.TermUpstreamError
	}

	// emitDone 正常收尾：此刻确认无续跑才释放暂存终帧（fork L1558-1566）。
	emitDone := func() {
		if doneEmitted || clientGone {
			return
		}
		if heldFinish != "" {
			if !writeLine(heldFinish + "\n") {
				return
			}
			heldFinish = ""
		}
		if !writeLine("data: [DONE]\n\n") {
			return
		}
		flusher.Flush()
		doneEmitted = true
	}

	// linePump 处理一行 SSE。返回 false = 上游流应立即终止。
	linePump := func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return true
		}
		if !strings.HasPrefix(trimmed, "data:") {
			return writeLine(line + "\n") // 注释/事件行原样透传
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		kind, envErr, _ := qoder.ClassifyMidStreamData(payload)
		switch kind {
		case qoder.MidDone:
			// fork handleDoneSignal：无 finish_reason 即 [DONE] = premature_done
			//（截断答案不算成功回合）。
			if !tracker.SawFinish {
				term = qoder.TermPrematureDone
				emitInlineError("upstream sent [DONE] before a terminal chunk", "stream_disconnected")
			} else {
				term = qoder.TermClean
				emitDone() // 释放暂存终帧 + [DONE]
			}
			return false
		case qoder.MidQueueSignal:
			// 防御包裹的排队帧：发注释心跳，不透传帧本身。
			return writeLine(": queued\n\n")
		case qoder.MidEnvelopeError:
			heldFinish = "" // fork：错误帧前丢暂存终帧
			if envErr != nil {
				emitInlineError(envErr.Message, envErr.Code)
			} else {
				// chunk.error 对象（fork L1437-1444）：错误帧原样转发后终结。
				if writeLine("data: " + payload + "\n\n") {
					flusher.Flush()
				}
				doneEmitted = true
				term = qoder.TermUpstreamError
			}
			return false
		case qoder.MidTerminal:
			heldFinish = line // 暂存不转发，续跑判定后统一释放
			tracker.Track(payload)
			h.bridgeUsageAddFrame(acctID, payload) // 阶段 4：末帧可能带 usage
			return true
		default:
			tracker.Track(payload)
			h.bridgeUsageAddFrame(acctID, payload) // 阶段 4：usage 帧累加
			return writeLine(line + "\n")
		}
	}

	// pump 读完一条上游流：返回 true = 正常耗尽（EOF 且未发 done）。
	pump := func(reader io.Reader) bool {
		buf := make([]byte, 32*1024)
		var pending []byte
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				for {
					idx := indexByteSlice(pending, '\n')
					if idx < 0 {
						break
					}
					line := string(pending[:idx])
					pending = pending[idx+1:]
					if !linePump(line) || doneEmitted || clientGone {
						return false
					}
				}
				flusher.Flush()
			}
			if err != nil {
				if err == io.EOF {
					if len(pending) > 0 && !linePump(string(pending)) {
						return false
					}
					return !doneEmitted && !clientGone
				}
				// transport error（审阅铁律③）：即使见过 finish_reason 也算失败，
				// held 帧不释放——否则截断答案变成功回合。
				term = qoder.TermUpstreamError
				emitInlineError("upstream stream failed before a terminal chunk", "stream_disconnected")
				return false
			}
		}
	}

	// classifyEOF EOF 终态分类（fork classifyEndOfStream）。
	classifyEOF := func() {
		if doneEmitted || term != "" {
			return
		}
		if tracker.SawFinish {
			term = qoder.TermFinishNoDone
		} else {
			term = qoder.TermPrematureEOF
		}
	}

	// continueFetch 续跑请求（fork continueFetch 语义）：每个候选账号构造
	// BuildContinueBody（追加悬挂文本+NUDGE、过滤空 assistant、流式恒开），
	// 走 qoderResilientStreamCtx 复用主请求 context（断开同步取消），
	// ChatMeta 空值触发 request_id 重生成防 Duplicate；fatal/失败换下一账号。
	continueFetch := func(ctx context.Context) (io.ReadCloser, bool) {
		if h.cfg.ExtAccounts == nil {
			return nil, false
		}
		cli := qoder.New()
		for _, a := range h.cfg.ExtAccounts() {
			if a.Provider != extstore.PQoder || a.Disabled {
				continue
			}
			var cred qoder.Credential
			if json.Unmarshal(a.Cred, &cred) != nil {
				continue
			}
			outBody, err := qoder.BuildContinueBody(origBody, tracker.TurnText())
			if err != nil {
				continue
			}
			cur, _, envErr, fatal, ferr := h.qoderResilientStreamCtx(ctx, cli, &cred, a, outBody, res, sessKey)
			if fatal || envErr != nil || ferr != nil || cur == nil {
				if cur != nil {
					cur.Close()
				}
				continue
			}
			return cur, true
		}
		return nil, false
	}

	// ── 主循环 ──
	if !pump(rc) {
		return
	}
	classifyEOF()

	// 自动续跑全 AND 条件（fork L1554-1563 原文语义）。
	for term != qoder.TermUpstreamError && !doneEmitted && !clientGone &&
		res.AutoContinueMax > 0 && continuations < res.AutoContinueMax &&
		hasTools && !tracker.SawToolCall && tracker.LastFinish != "tool_calls" &&
		qoder.LooksLikeDanglingIntent(tracker.TurnText()) {

		continuations++
		heldFinish = "" // 回合没结束：丢暂存终帧，否则客户端中途 abort 我们
		log.Printf("qoder-bridge: acct=%s auto-continue %d/%d (finish=%s, dangling intent)",
			acctID, continuations, res.AutoContinueMax, tracker.LastFinish)

		next, ok := continueFetch(r.Context())
		if !ok {
			term = qoder.TermUpstreamError
			emitInlineError("auto-continue request failed", "stream_disconnected")
			break
		}
		tracker.Reset() // fork：六项状态清零后再泵续跑流
		term = ""
		more := pump(next)
		next.Close()
		if !more {
			return
		}
		classifyEOF()
	}

	// premature_eof 兜底（fork）：EOF 且无任何终止标志。
	if !doneEmitted && !clientGone && term == qoder.TermPrematureEOF {
		emitInlineError("upstream stream closed before a terminal chunk", "stream_disconnected")
	}
	// 无续跑将跟随：释放终帧收尾。
	if !doneEmitted {
		emitDone()
	}
}

// indexByteSlice 返回 b 中首个 sep 的下标，没有则 -1。
func indexByteSlice(b []byte, sep byte) int {
	for i := range b {
		if b[i] == sep {
			return i
		}
	}
	return -1
}

// clientWantsStream 从请求体读 stream 字段。
func (h *Handler) clientWantsStream(body []byte) bool {
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Stream
}

// proxyQoderSSE 逐帧透传 Qoder SSE（帧格式本就是标准 OpenAI chat.completion.chunk）。
func (h *Handler) proxyQoderSSE(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// proxyQoderAggregate 聚合 Qoder SSE 为标准 chat.completion 非流式响应。
func (h *Handler) proxyQoderAggregate(w http.ResponseWriter, rc io.ReadCloser) {
	defer rc.Close()
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ExtManager Qoder 桥接需要的最小外部账号能力（main 注入实现）。
type ExtManager interface {
	ReplaceCred(provider, id string, cred json.RawMessage)
	// NoteChatResult 上报一次对话结果，驱动外部账号的冷却/退避
	//（extstore.SelectChatOrder 据此把失败的号退到队尾）。
	NoteChatResult(provider, id string, err error)
	// NoteBalance 上报一次余额观测（5 分钟余额刷新喂进缓存，供候选排序）。
	NoteBalance(provider, id string, balance float64, ok bool)
	// CachedBalance 免网络读该平台的最高余额。第二返回值=是否测得过。
	CachedBalance(provider string) (float64, bool)
	// MarkSignInExpired 标记 provider 明确确认凭据失效（refresh 401/403）。
	MarkSignInExpired(provider, id, note string) error
	// MarkSignInActive 清除 lapsed 标记（refresh 成功后）。
	MarkSignInActive(provider, id string) error
}

// qoderSessionID 阶段 3 会话隔离：客户端会话键 → 稳定上游 session_id。
// 键非空：存储内稳定映射（fork runtimeSessionStore——TTL 内同键同值，
// prompt cache 连续）；键为空：一次性 UUID（fork ephemeral 分支——两个
// 独立首回合客户端不共用账号级服务端状态，完整历史每次请求都发）。
func (h *Handler) qoderSessionID(sessKey string) string {
	if h.qoderSessions == nil {
		if sessKey == "" {
			return qoder.StableSessionHash("qoder-session", "")
		}
		return qoder.StableSessionHash("qoder-session", sessKey)
	}
	return h.qoderSessions.SessionIDFor(sessKey)
}

// ExtSetManager 注入外部账号管理器（main 装配时调用；nil = Qoder 桥接禁用）。
func (h *Handler) ExtSetManager(m ExtManager) { h.extManager = m }

// ── 阶段 4：额度门与记账 ──

// ── 阶段 4：额度门与记账 ──

// qoderAlertCfg 当前告警配置（阶段 5）：默认关闭；main 装配或面板可注入。
// 用值类型 + 无锁读写——配置变更频率极低（面板改一次），竞态无害
// （最坏晚一个请求周期生效）。
func (h *Handler) qoderAlertCfg() dingtalk.Settings {
	if h.cfg.DingTalk == nil {
		return dingtalk.Settings{}
	}
	return dingtalk.NormalizeSettings(*h.cfg.DingTalk)
}

// qoderRecordCall 记一次成功的 Qoder 调用（阶段 5 闲时告警的事实源）。
// 在成功建流并开始透传时调；nil 安全。
func (h *Handler) qoderRecordCall() {
	if h.qoderIdle == nil {
		return
	}
	h.qoderIdle.RecordCall(time.Now())
}

// qoderAlertQuotaExhausted 账号分配额度耗尽告警（阶段 5，fork
// notifyApiKeyQuotaExhausted）：PRE 门判 exhausted 时调；同一账号耗尽态
// 持续期间只报一次（activeExhaustedKeys），恢复解禁后才可再报。
// fire-and-forget：发送失败只回滚状态记日志。
func (h *Handler) qoderAlertQuotaExhausted(a *extstore.ExtAccount, used, limit, remaining float64) {
	set := h.qoderAlertCfg()
	if !set.Enabled || set.DingTalkWebhook == "" {
		return
	}
	keyID := qoder.AccountKey(string(a.Provider), a.ID)
	ev := h.qoderKeyQuota.EvaluateExhausted(set, keyID, time.Now())
	if !ev.ShouldAlert {
		return
	}
	name := a.Label
	if name == "" {
		name = a.ID
	}
	text := dingtalk.BuildExhaustedText(dingtalk.QuotaDetails{
		KeyName: name, Provider: string(a.Provider), Used: used, Limit: limit, Remaining: remaining,
	}, set.Locale, time.Now())
	h.qoderKeyQuota.MarkExhaustedAlerted(keyID, time.Now())
	if err := h.dingTalk.SendWithRetry(set, text, time.Now(), 3); err != nil {
		h.qoderKeyQuota.RollbackExhausted(keyID) // 发送失败回滚：下次可重试
		log.Printf("qoder-bridge: acct=%s 额度耗尽告警发送失败: %v", a.ID, err)
		return
	}
	log.Printf("qoder-bridge: acct=%s 额度耗尽告警已发送", a.ID)
}

// qoderAlertQuotaThreshold 使用率阈值告警（阶段 5，fork
// notifyApiKeyQuotaThresholdExceeded）：记账后调，过阈值+过冷却才报。
func (h *Handler) qoderAlertQuotaThreshold(a *extstore.ExtAccount, used, limit float64) {
	set := h.qoderAlertCfg()
	if !set.Enabled || !set.QuotaUsageThresholdEnable || set.DingTalkWebhook == "" {
		return
	}
	keyID := qoder.AccountKey(string(a.Provider), a.ID)
	ev := h.qoderKeyQuota.EvaluateThreshold(set, keyID, used, limit, time.Now())
	if !ev.ShouldAlert {
		return
	}
	name := a.Label
	if name == "" {
		name = a.ID
	}
	text := dingtalk.BuildThresholdText(dingtalk.QuotaDetails{
		KeyName: name, Provider: string(a.Provider), Used: used, Limit: limit,
		Remaining: limit - used,
	}, ev.ThresholdPct, ev.UsagePercent, set.Locale, time.Now())
	h.qoderKeyQuota.MarkThresholdAlerted(keyID, time.Now())
	if err := h.dingTalk.SendWithRetry(set, text, time.Now(), 3); err != nil {
		log.Printf("qoder-bridge: acct=%s 阈值告警发送失败: %v", a.ID, err)
	}
}

// qoderQuotaGate 路由前额度门（fork apiKeyPolicy PRE-request 语义）：
// 读账本分配上限内的已用量（精确优先、余额差估算兜底），判定账号是否还有
// 额度。无账本/无上限 → QuotaOK；两条估算路都无数据 → QuotaUnknown
// （调用方 fail-open 放行——配额基础设施不可用不挡正常流量）。
func (h *Handler) qoderQuotaGate(a *extstore.ExtAccount) qoder.QuotaGateResult {
	if h.qoderLedger == nil {
		return qoder.QuotaOK
	}
	key := qoder.AccountKey(string(a.Provider), a.ID)
	// 余额差估算：余额缓存观测折算（无观测传 -1 = 不可用）。
	est := -1.0
	if h.extManager != nil {
		if bal, ok := h.extManager.CachedBalance(string(a.Provider)); ok {
			// CachedBalance 是平台最高余额（粗口径）：只作为"有观测"的信号，
			// 精确差估算需要 per-account baseline，当前上游不给，故传 0 表示
			// "观测存在但折算为 0 消耗"——不拦路，由精确记账兜底。
			_ = bal
			est = 0
		}
	}
	return h.qoderLedger.Gate(key, est)
}

// qoderRecordUsage POST 记账（fork recordKeyQuotaUsage 语义）：
//  1. token 统计进 usage.Recorder（面板用量视图，与腾讯池同源）；
//  2. 积分账本精确记账（RecordCredits）——buddyhub 侧上游不明示 credit
//     消耗，token 不折算积分（汇率未知），此处仅在有显式 credits 时记；
//     当前 Qoder 上游无该字段，故只走 usage.Recorder，账本由 SetAllocation
//     配置的上限 + 未来上游 credits 字段驱动。
//
// fire-and-forget：记账失败只打日志，不影响已完成的响应。
func (h *Handler) qoderRecordUsage(a *extstore.ExtAccount, sessKey, model string, u qoder.UsageFromChatCompletion) {
	if u.TotalTokens <= 0 && u.PromptTokens <= 0 && u.CompletionTokens <= 0 {
		return
	}
	if h.cfg.Usage != nil {
		h.cfg.Usage.Add(time.Now(), "ext", string(a.Provider)+"/"+a.ID, model, usage.Delta{
			PromptTokens:     u.PromptTokens,
			HasPromptTokens:  u.PromptTokens > 0,
			CompletionTokens: u.CompletionTokens,
			HasCompletion:    u.CompletionTokens > 0,
			TotalTokens:      u.TotalTokens,
			HasTotal:         u.TotalTokens > 0,
		}, true)
	}
	// 阶段 5：使用率阈值告警（记账后判，fork notifyApiKeyQuotaThresholdExceeded）。
	if h.qoderLedger != nil {
		key := qoder.AccountKey(string(a.Provider), a.ID)
		if limit := h.qoderLedger.AllocationLimit(key); limit > 0 {
			h.qoderAlertQuotaThreshold(a, h.qoderLedger.UsedCredits(key), limit)
		}
	}
	log.Printf("qoder-bridge: acct=%s 记账 prompt=%d completion=%d total=%d",
		a.ID, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
}

// bridgeUsageReset 清空账号本轮流式 usage 累计（建流后调用）。
func (h *Handler) bridgeUsageReset(acctID string) {
	if h.bridgeUsage == nil {
		return
	}
	h.bridgeUsage.Reset(qoder.AccountKey(string(extstore.PQoder), acctID))
}

// bridgeUsageTake 取走并清空账号本轮流式 usage 累计（流结束后入账）。
func (h *Handler) bridgeUsageTake(acctID string) qoder.UsageFromChatCompletion {
	if h.bridgeUsage == nil {
		return qoder.UsageFromChatCompletion{}
	}
	return h.bridgeUsage.Take(qoder.AccountKey(string(extstore.PQoder), acctID))
}

// bridgeUsageAddFrame 泵内调用：把一个 SSE data 帧的 usage 累加进账号本轮。
func (h *Handler) bridgeUsageAddFrame(acctID, data string) {
	if h.bridgeUsage == nil {
		return
	}
	h.bridgeUsage.AddFrame(qoder.AccountKey(string(extstore.PQoder), acctID), data)
}

// QuotaSnapshots 全部账号的额度快照（面板 /panel/api/quota 数据源）。
// 面板不 import qoder 包，经此方法 + QuotaSource 闭包解耦；nil 账本返回空。
func (h *Handler) QuotaSnapshots() []qoder.QuotaSnapshot {
	if h.qoderLedger == nil {
		return nil
	}
	return h.qoderLedger.Snapshot(nil)
}

// RemoveQuotaAllocation 移除账号分配上限（面板配额弹窗「移除分配」按钮）。
// fork quota modal 同语义：SetAllocation(key, 0) 即删分配；移除后该账号
// 回到无上限（PRE 额度门放行），记账继续累计。nil 账本安全空操作。
func (h *Handler) RemoveQuotaAllocation(accountKey string) {
	if accountKey == "" {
		return
	}
	h.qoderLedger.SetAllocation(accountKey, 0)
	log.Printf("qoder-bridge: 面板移除账号分配上限 %s", accountKey)
}

// noteChat 上报一次对话结果。
//
// 统一从这里走：桥接里散落 `if h.extManager != nil` 会漏掉（漏一次 = 那个号
// 永远不进冷却，每次都白白撞），而裸用/测试场景没有管理器时静默忽略即可。
func (h *Handler) noteChat(provider, id string, err error) {
	if h.extManager == nil {
		return
	}
	h.extManager.NoteChatResult(provider, id, err)
}

// extBalance 读该平台的缓存余额。没有管理器（裸用/测试）时返回 (0, false)——
// 排序侧把"没测得过"当 0 处理，退化成稳定的原序，不影响正确性。
func (h *Handler) extBalance(provider string) (float64, bool) {
	if h.extManager == nil {
		return 0, false
	}
	return h.extManager.CachedBalance(provider)
}

// errOf 把失败原因串转成 error。
//
// 桥接内部一直用字符串（最终要拼进 `h.lastXxxErr` 文案给用户看），而上报接口
// 要 error——两者的形状差只在这一个转换点收口，不用改每个桥接的变量类型。
func errOf(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// qoderModelPrefix Qoder 路由前缀。
const qoderModelPrefix = "qoder:"

// isQoderModel 判断裸模型名是否请求 Qoder 通道。
func isQoderModel(bare string) bool { return strings.HasPrefix(bare, qoderModelPrefix) }
