// Package server: cursor_bridge.go — Cursor 通道（magpie 二批，对话已接线）。
//
// 模型名带 "cursor:" 前缀时走本路径。
//
// 协议事实（magpie-plugins packages/cursor/index.mjs，HEAD f573cfd）：
//   - 上游对话是 Connect bidi-stream；buddyhub 走 fork 自己的 HTTP/1.1 降级
//     路径（AgentService/RunSSE 下行 + BidiService/BidiAppend 逐条上行，
//     同一 x-request-id 绑会话），见 internal/extprovider/cursor/run.go；
//   - 对话历史存在客户端：每条 AI SDK 消息按 sha256 命名成 blob，服务端
//     按需索要原文（runbuild.go + run.go 的 kvMessage）；
//   - 调用方的工具经 CallDynamicTool 反向回调（run.go 的 execMessage）；
//   - 鉴权 Bearer access token；Cursor 的 refresh token **不能**换新 access
//     token（fork L264-267），到期只能重登——账号池按"耗尽即弃"建模，
//     不套 authcore 的 refresh-lead。
//
// 模型名口径（fork request() L821-826 的兜底分支）：目录里没有的名字原样
// 送出（"auto" 换 "default"），maxMode=false、无参数。**effort/fast 变体
// 选择需要 Cursor 的模型目录**（fork 的 pick/named/variants 那一整套），
// 本桥未接目录——如实记入 KNOWN-LIMITATIONS。
//
// 外壳语义沿用 0ed4e1c：账号解析 → 凭据校验 → lapsed 标注 → unary 连通性
// 探测 → 对话；无可用账号仍返回 false（dispatch 给 no_cursor_account）。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/cursor"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// cursorModelPrefix Cursor 路由前缀。
const cursorModelPrefix = "cursor:"

// isCursorModel 判断裸模型名是否请求 Cursor 通道。
func isCursorModel(bare string) bool { return strings.HasPrefix(bare, cursorModelPrefix) }

// cursor unary 路径（fork L207-208：RPC 全限定名）。
const (
	cursorGetMePath       = "aiserver.v1.DashboardService/GetMe"
	cursorGetPlanInfoPath = "aiserver.v1.DashboardService/GetPlanInfo"
)

// maxModeRequired 上游说"这个模型只在 Max Mode 提供"（fork L2193-2198：
// 拒的是名字不是账号，重发一次带 max_mode 即可）。
func maxModeRequired(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "max mode required") || strings.Contains(low, "max_mode required")
}

// CursorChatStream Cursor 通道入口。返回 false = 无可用账号。
func (h *Handler) CursorChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	h.lastCursorErr = ""
	if h.cfg.ExtAccounts == nil {
		return false
	}
	model := strings.TrimPrefix(bareModel, cursorModelPrefix)
	runModel := model // fork request() 兜底：目录外的名字原样送
	if runModel == "auto" {
		runModel = "default"
	}

	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCursor && !a.Disabled && a.SignInState != "expired" {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		h.lastCursorErr = "没有可用的 Cursor 账号（面板-添加账号-外部平台-Cursor）"
		return false
	}

	// 对话构造（纯函数，一次即可，不随换号重做）。
	conv, tools, _, err := cursor.BuildConversation(body)
	if err != nil || conv == nil || len(conv.Msgs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "Cursor：请求体里没有可用的对话消息")
		return true
	}
	// 会话稳定 id（同 prompt_cache_key + 同首条 user → 同 id：Cursor 按它把
	// 请求路由到缓存了这份 prompt 的机器）。无键时空串，每次 Run 新建。
	convID := cursor.ConversationID(body)
	wantStream := h.clientWantsStream(body)

	lastErr := ""
	for _, a := range accounts {
		var cred cursor.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if strings.TrimSpace(cred.AccessToken) == "" {
			lastErr = "凭据里没有 access_token"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// "耗尽即弃"（fork L264-267）：到期标 lapsed，不套 refresh-lead。
		if cred.Lapsed(time.Now()) {
			lastErr = "Cursor 登录已过期（refresh 不可用），请重新授权"
			_ = h.extManager.MarkSignInExpired(a.Provider, a.ID, lastErr)
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		// unary 连通性探测（fork whoIs L205-214）：验凭据 + 拿身份。
		// 网络故障保留登录态换号（与 zcode 桥同口径）。
		info, perr := cursorProbe(r, cred.AccessToken)
		if perr != nil {
			lastErr = perr.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if info.email == "" {
			lastErr = "上游未返回账号身份（凭据可能已失效）"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		cli := cursor.New()
		run := func(maxMode bool) (*cursor.ChatStream, cursor.Event, bool) {
			return h.cursorOpenRun(r, cli, cred.AccessToken, conv, tools, runModel, convID, maxMode)
		}
		stream, first, ok := run(false)
		if !ok {
			lastErr = firstErrText(first)
			if lastErr == "" {
				lastErr = "cursor: 建流失败"
			}
			// Max Mode 拒名字：换模式重发一次（fork 同款，仍算同一个账号）。
			if maxModeRequired(lastErr) {
				log.Printf("cursor-bridge: acct=%s 上游要求 Max Mode，重发 model=%s", a.ID, runModel)
				if s2, f2, ok2 := run(true); ok2 {
					stream, first, ok = s2, f2, true
				} else {
					lastErr = firstErrText(f2)
				}
			}
			if !ok {
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("cursor-bridge: acct=%s 建流失败: %s", a.ID, qzRedact(lastErr))
				continue
			}
		}
		h.noteChat(a.Provider, a.ID, nil)
		h.cursorPump(w, stream, first, model, a, wantStream)
		return true
	}
	h.lastCursorErr = lastErr
	return false
}

// cursorOpenRun 发起一次 Run 并**先读一帧**（fork runOnce 的 head-value
// 语义：首帧是错误就不算建流成功，调用方才能换号/换模式重试）。
// 返回 (流, 首帧, 成功)。失败时首帧带错误内容供分类。
func (h *Handler) cursorOpenRun(r *http.Request, cli *cursor.Client, token string,
	conv *cursor.Conversation, tools []cursor.Tool, model, convID string, maxMode bool) (*cursor.ChatStream, cursor.Event, bool) {
	stream, err := cli.ChatRun(r.Context(), cursor.ChatRequest{
		Token:   token,
		Model:   model,
		Conv:    conv,
		Tools:   tools,
		ConvID:  convID,
		MaxMode: maxMode,
	})
	if err != nil {
		return nil, cursor.Event{Kind: cursor.EventError, Err: err}, false
	}
	select {
	case <-r.Context().Done():
		stream.Close()
		return nil, cursor.Event{Kind: cursor.EventError, Err: r.Context().Err()}, false
	case ev, ok := <-stream.Events:
		if !ok {
			stream.Close()
			return nil, cursor.Event{Kind: cursor.EventError, Err: fmt.Errorf("cursor: 流在首帧前结束")}, false
		}
		if ev.Kind == cursor.EventError {
			stream.Close()
			return nil, ev, false
		}
		return stream, ev, true
	}
}

func firstErrText(ev cursor.Event) string {
	if ev.Err != nil {
		return ev.Err.Error()
	}
	return ""
}

// cursorPump 协议事件 → OpenAI chat.completion.chunk（流式）或聚合 JSON
// （非流式客户端）。first 是已读出但没发出去的首帧，从这里接着发。
func (h *Handler) cursorPump(w http.ResponseWriter, stream *cursor.ChatStream, first cursor.Event,
	model string, a *extstore.ExtAccount, wantStream bool) {
	defer stream.Close()

	events := stream.Events
	if !wantStream {
		agg := cursorAggregate(events, first, model)
		if e, isErr := agg["error"]; isErr && e != nil {
			h.noteChat(a.Provider, a.ID, errOf(fmt.Sprint(e)))
		}
		raw, _ := json.Marshal(agg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	w.WriteHeader(http.StatusOK)

	st := &cursorChunker{w: w, flusher: flusher, id: newChunkID(), created: time.Now().Unix(), model: model}
	// role 首帧（OpenAI 流式惯例）。
	st.delta(map[string]any{"role": "assistant"}, nil)

	// 把预读的首帧也走同一循环：用 once 包一层。
	queue := []cursor.Event{first}
	consume := func(ev cursor.Event) bool { // 返回 false = 已结束
		switch ev.Kind {
		case cursor.EventReasoning:
			if ev.Reasoning != "" {
				st.wroteAny = true
				// 与 qoder/zai 泵同款口径：思考走非标准 reasoning_content，
				// 不混进正文（Codex/Claude 客户端都读这个字段）。
				st.delta(map[string]any{"reasoning_content": ev.Reasoning}, nil)
			}
		case cursor.EventText:
			if ev.Text != "" {
				st.wroteAny = true
				st.delta(map[string]any{"content": ev.Text}, nil)
			}
		case cursor.EventTool:
			st.wroteAny = true
			st.delta(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": ev.Tool.Index,
					"id":    ev.Tool.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.Tool.Name,
						"arguments": ev.Tool.Args,
					},
				}},
			}, nil)
		case cursor.EventError:
			msg := "cursor: 上游流内错误"
			if ev.Err != nil {
				msg = ev.Err.Error()
			}
			h.noteChat(a.Provider, a.ID, errOf(msg))
			// 已成流的响应改不了状态码：写 error 帧（ima 同款）。
			raw, _ := json.Marshal(map[string]any{
				"error": map[string]any{"message": msg, "type": "upstream_error", "code": ev.Code},
			})
			st.frame(raw)
			st.done()
			return false
		case cursor.EventDone:
			finish := ev.Stop
			if finish == "" {
				finish = "stop"
			}
			if ev.Usage != nil {
				st.finishWithUsage(finish, ev.Usage)
			} else {
				st.delta(nil, &finish)
			}
			st.done()
			return false
		}
		return true
	}
	for _, ev := range queue {
		if !consume(ev) {
			return
		}
	}
	for ev := range events {
		if !consume(ev) {
			return
		}
	}
	// 通道关了却没给终局（协议层保证不会走到，兜底如实收尾）。
	h.noteChat(a.Provider, a.ID, errOf("cursor: 流意外结束"))
	st.done()
}

// cursorChunker OpenAI chunk 写入器。
type cursorChunker struct {
	w        io.Writer
	flusher  http.Flusher
	id       string
	created  int64
	model    string
	wroteAny bool
}

func (s *cursorChunker) delta(delta map[string]any, finish *string) {
	choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}
	if len(delta) > 0 {
		choice["delta"] = delta
	}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	raw, _ := json.Marshal(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created,
		"model": s.model, "choices": []any{choice},
	})
	s.frame(raw)
}

// finishWithUsage 终帧带 usage（惯例：最后一帧 choices 为空、usage 齐全）。
func (s *cursorChunker) finishWithUsage(finish string, u *cursor.Usage) {
	usage := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.CachedTokens > 0 || u.CacheWriteTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{
			"cached_tokens":      u.CachedTokens,
			"cache_write_tokens": u.CacheWriteTokens,
		}
	}
	if u.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	raw, _ := json.Marshal(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		"usage":   usage,
	})
	s.frame(raw)
}

func (s *cursorChunker) frame(raw []byte) {
	_, _ = s.w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *cursorChunker) done() {
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// cursorAggregate 非流式客户端的聚合回复（first 是预读首帧，一并计入）。
func cursorAggregate(events <-chan cursor.Event, first cursor.Event, model string) map[string]any {
	var content, reasoning strings.Builder
	type call struct{ id, name, args string }
	var calls []call
	usage := map[string]any{}
	stop := "stop"
	errText := ""

	consume := func(ev cursor.Event) {
		switch ev.Kind {
		case cursor.EventReasoning:
			reasoning.WriteString(ev.Reasoning)
		case cursor.EventText:
			content.WriteString(ev.Text)
		case cursor.EventTool:
			calls = append(calls, call{ev.Tool.ID, ev.Tool.Name, ev.Tool.Args})
		case cursor.EventError:
			if ev.Err != nil {
				errText = ev.Err.Error()
			} else {
				errText = "cursor: 上游错误"
			}
		case cursor.EventDone:
			if ev.Stop != "" {
				stop = ev.Stop
			}
			if ev.Usage != nil {
				usage = map[string]any{
					"prompt_tokens":     ev.Usage.PromptTokens,
					"completion_tokens": ev.Usage.CompletionTokens,
					"total_tokens":      ev.Usage.TotalTokens,
				}
			}
		}
	}
	consume(first)
	for ev := range events {
		consume(ev)
	}
	if errText != "" {
		return map[string]any{"error": map[string]any{"message": errText, "type": "upstream_error"}}
	}
	msg := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		msg["content"] = content.String()
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		list := make([]any, 0, len(calls))
		for _, c := range calls {
			list = append(list, map[string]any{
				"id": c.id, "type": "function",
				"function": map[string]any{"name": c.name, "arguments": c.args},
			})
		}
		msg["tool_calls"] = list
	}
	out := map[string]any{
		"id":      newChunkID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": stop}},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return out
}

// cursorProbeResult unary 探测结果。
type cursorProbeResult struct {
	email string
	plan  string
}

// cursorProbe 并发拉 GetMe + GetPlanInfo（fork whoIs：allSettled——单个
// 失败不拖垮另一个）。每路 15s 超时（fork unary AbortSignal 同值）。
//
// 走 http.DefaultClient 而不是自建 client：协议层（run.go）用的就是
// 默认客户端，本仓所有 ext 桥的测试都靠替换 http.DefaultClient 把生产
// 域名重写到假上游（zcode_bridge_test 同款）。自建 client 会绕过那层
// 注入——**测试就会打真实 api2.cursor.sh**（实测确实发生过：假上游测试
// 拿到大约 1 秒的真实网络往返与一个空 email 的 200）。
func cursorProbe(r *http.Request, token string) (cursorProbeResult, error) {
	type res struct {
		raw []byte
		err error
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ch := make(chan res, 2)
	for _, p := range []string{cursorGetMePath, cursorGetPlanInfoPath} {
		go func(path string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				cursor.API+"/"+path, strings.NewReader("{}"))
			if err != nil {
				ch <- res{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Connect-Protocol-Version", "1")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("x-cursor-client-type", "cli")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				ch <- res{err: err}
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				ch <- res{err: fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)}
				return
			}
			ch <- res{raw: raw}
		}(p)
	}
	out := cursorProbeResult{}
	for i := 0; i < 2; i++ {
		got := <-ch
		if got.err != nil {
			continue
		}
		var doc struct {
			Email    string `json:"email"`
			PlanInfo struct {
				PlanName string `json:"planName"`
			} `json:"planInfo"`
		}
		if json.Unmarshal(got.raw, &doc) == nil {
			if e := strings.TrimSpace(doc.Email); e != "" {
				out.email = e
			}
			if p := strings.TrimSpace(doc.PlanInfo.PlanName); p != "" {
				out.plan = p
			}
		}
	}
	return out, nil
}
