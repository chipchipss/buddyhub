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

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
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
	for _, a := range accounts {
		var cred qoder.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// 到期前自动续期；续期失败继续尝试下一个账号。
		if cred.NeedsRefresh() {
			if fresh, err := cli.Refresh(r.Context(), &cred); err == nil {
				if raw, merr := json.Marshal(fresh); merr == nil {
					mgr.ReplaceCred(a.Provider, a.ID, raw)
					cred = *fresh
				}
			} else {
				log.Printf("qoder-bridge: %s 续期失败: %v", a.ID, err)
				// 此前这里**只 continue 不设 lastErr**：整轮失败时
				// h.lastQoderErr 是空串，503 文案里看不到真实原因。
				lastErr = "续期失败: " + err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
		}

		// 建流（带弹性：排队原位重试、首 token 超时独立预算、换号前原账号先扛）。
		rc, status, envErr, fatal, ferr := h.qoderResilientStream(r, cli, &cred, a, body, res)
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
		if h.clientWantsStream(body) {
			h.proxyQoderResilientSSE(w, r, rc, res, a.ID, body)
		} else {
			h.proxyQoderAggregate(w, rc)
		}
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastQoderErr = lastErr
	return false // 全部 Qoder 账号不可用
}

// qoderResilientStream 在单个账号内做阶段 1 弹性建流：
//   - 排队（403/429 body isQueued/10605）→ 原位重试，退避 QODER_QUEUE_*（默认 15 次 5s→60s），
//     等待期间向客户端发 SSE 注释心跳（QODER_KEEPALIVE_MS）——此时还没写过任何正文，安全；
//   - 首 token 超时（504/408 + 文案）→ 按 FirstTokenFallback 策略独立预算重试；
//   - 其余错误原样上抛给账号轮换。
//
// 返回 fatal=true 表示已直接向客户端写出终局错误（首信封错误）；
// rc 非 nil 表示建流成功（调用方负责关闭并继续透传）。
func (h *Handler) qoderResilientStream(r *http.Request, cli *qoder.Client, cred *qoder.Credential,
	a *extstore.ExtAccount, body []byte, res qoder.ResilienceConfig) (
	rc io.ReadCloser, status int, envErr *qoder.EnvelopeError, fatal bool, ferr error,
) {
	return h.qoderResilientStreamCtx(r.Context(), cli, cred, a, body, res)
}

// qoderResilientStreamCtx 弹性建流的 context 直通版：续跑请求复用主请求的
// context——客户端断开时续跑同步取消，不留孤儿上游流。
func (h *Handler) qoderResilientStreamCtx(ctx context.Context, cli *qoder.Client, cred *qoder.Credential,
	a *extstore.ExtAccount, body []byte, res qoder.ResilienceConfig) (
	rc io.ReadCloser, status int, envErr *qoder.EnvelopeError, fatal bool, ferr error,
) {
	ensureKeepalive := func() {
		// 建流阶段排队等待的心跳由调用方 ResponseWriter 处理不安全（头未发），
		// 这里只做空操作占位：真实心跳在 proxyQoderResilientSSE 的流内空闲检测里。
	}
	_ = ensureKeepalive
	for attempt := 1; attempt <= res.QueueMaxAttempts; attempt++ {
		meta := qoder.ChatMeta{} // 每次重试重新生成 request_id，防上游 Duplicate 判定
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
func (h *Handler) proxyQoderResilientSSE(w http.ResponseWriter, r *http.Request, rc io.ReadCloser, res qoder.ResilienceConfig, acctID string, origBody []byte) {
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
			return true
		default:
			tracker.Track(payload)
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
			cur, _, envErr, fatal, ferr := h.qoderResilientStreamCtx(ctx, cli, &cred, a, outBody, res)
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
}

// ExtSetManager 注入外部账号管理器（main 装配时调用；nil = Qoder 桥接禁用）。
func (h *Handler) ExtSetManager(m ExtManager) { h.extManager = m }

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
