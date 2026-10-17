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
	"github.com/chipchipss/buddyhub/internal/usage"
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

// CursorChatStream Cursor 通道入口。
//
// 返回的 outcome：served=已写响应（成功 / 桥接内部的 400）；matched 且未 served
// = 什么都没写，调用方按 code/detail/lastErr 报错，**不回落腾讯池**。
//
// lastErr 是**本请求**的失败原因（请求级局部量，不是 Handler 字段）：
// 并发请求各拿各的，A 的 503 文案里不会出现 B 的上游错误。
func (h *Handler) CursorChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bridgeOutcome {
	if h.cfg.ExtAccounts == nil {
		return bridgeMiss("no_cursor_account",
			"没有可用的 Cursor 账号（面板-添加账号-外部平台-Cursor 浏览器登录）", "Cursor", "")
	}
	model := strings.TrimPrefix(bareModel, cursorModelPrefix)
	// 目录解析前的原始请求名：目录里有它 → 解析成变体 id（effort/fast/maxMode
	// 都由目录决定）；目录里没有/没拿到目录 → 原样送（fork request() 兜底）。

	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCursor && !a.Disabled && a.SignInState != "expired" {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return bridgeMiss("no_cursor_account",
			"没有可用的 Cursor 账号（面板-添加账号-外部平台-Cursor 浏览器登录）", "Cursor",
			"没有可用的 Cursor 账号（面板-添加账号-外部平台-Cursor 浏览器登录）")
	}

	// 对话构造（纯函数，一次即可，不随换号重做）。第三返回值是解析后的
	// 原始请求 map（取 reasoning_effort / service_tier 给模型目录用）。
	conv, tools, chat, err := cursor.BuildConversation(body)
	if err != nil || conv == nil || len(conv.Msgs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "Cursor：请求体里没有可用的对话消息")
		return bridgeServed()
	}
	// 会话稳定 id（同 prompt_cache_key + 同首条 user → 同 id：Cursor 按它把
	// 请求路由到缓存了这份 prompt 的机器）。无键时空串，每次 Run 新建。
	convID := cursor.ConversationID(body)
	wantStream := h.clientWantsStream(body)

	// 模型目录 + effort/fast 变体（fork answerOf L2162-2174）：
	//   - effort = 请求的 reasoning_effort（Codex 侧的推理档）
	//   - fast   = service_tier 为 priority/fast 时开
	// 目录**按账号 token 解析**（fork 的 usable 就是按 token 缓存的清单），
	// 拉不到时走兜底（名字原样送、auto→default）——与 fork 的
	// `try { raw = await usable(tok) } catch {}` 同款语义，不阻塞对话。
	effort, _ := chat["reasoning_effort"].(string)
	fast := false
	if tier, _ := chat["service_tier"].(string); tier == "priority" || tier == "fast" {
		fast = true
	}

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
		// 目录按账号解析（fork usable 就是按 token 缓存）：拉不到时
		// variant=nil → 走兜底（名字原样送），不阻塞对话。
		v := h.cursorVariant(r, cli, cred.AccessToken, model, effort, fast)
		// 目录解析出的目标已含 maxMode；这里只在上游**仍然**拒 Max Mode 时
		// 强制重发一次（fork 的 maxOnly.has(id) 那一路）。
		run := func(forceMax bool) (*cursor.ChatStream, cursor.Event, bool) {
			return h.cursorOpenRun(r, cli, cred.AccessToken, conv, tools, v, convID, forceMax)
		}
		stream, first, ok := run(false)
		if !ok {
			lastErr = firstErrText(first)
			if lastErr == "" {
				lastErr = "cursor: 建流失败"
			}
			// Max Mode 拒名字：换模式重发一次（fork 同款，仍算同一个账号），
			// 并记下该 id 以后直接带 Max Mode（fork L2198-2199 的
			// maxMode=true + maxOnly.add(id)——不记的话每个请求都要先撞一次拒）。
			if maxModeRequired(lastErr) && !v.MaxMode {
				log.Printf("cursor-bridge: acct=%s 上游要求 Max Mode，重发 model=%s", a.ID, model)
				h.cursorMarkNeedsMax(model)
				if v.ID != model {
					h.cursorMarkNeedsMax(v.ID)
				}
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
		h.cursorPump(r, w, stream, first, model, a, wantStream)
		return bridgeServed()
	}
	return bridgeMiss("no_cursor_account",
		"没有可用的 Cursor 账号（面板-添加账号-外部平台-Cursor 浏览器登录）", "Cursor", lastErr)
}

// cursorVariant 用该账号的模型目录把请求模型名解析成目标变体
// （fork answerOf L2162-2174 的 usable+request 两步）。
// 目录拉不到时走兜底（名字原样送、auto→default）——不阻塞对话。
func (h *Handler) cursorVariant(r *http.Request, cli *cursor.Client, token, model, effort string, fast bool) cursor.Variant {
	cat, err := h.cursorCatalogs.Get(r.Context(), cli, token)
	if err != nil {
		log.Printf("cursor-bridge: 模型目录拉取失败（走兜底原样送）: %s", err)
		return cursorFallbackVariant(model)
	}
	v := cat.Request(model, effort, fast)
	// 该 id 曾被上游拒过 "Max Mode Required"：以后直接带 Max Mode，
	// 不再每次先撞一次拒（fork maxOnly Set L2145 + L2174）。
	if h.cursorNeedsMax(model) || h.cursorNeedsMax(v.ID) {
		v.MaxMode = true
	}
	log.Printf("cursor-bridge: model=%s effort=%q fast=%v → id=%s maxMode=%v params=%d",
		model, effort, fast, v.ID, v.MaxMode, len(v.Params))
	return v
}

// cursorFallbackVariant 目录缺失时的兜底目标（fork request() 第三分支）。
func cursorFallbackVariant(model string) cursor.Variant {
	run := model
	if run == "auto" {
		run = "default"
	}
	return cursor.Variant{ID: run}
}

// cursorNeedsMax 该 id 是否已被上游拒过 "Max Mode Required"（fork maxOnly.has）。
func (h *Handler) cursorNeedsMax(id string) bool {
	if id == "" {
		return false
	}
	_, ok := h.cursorMaxOnly.Load(id)
	return ok
}

// cursorModelEntries /v1/models 的 cursor: 条目（模型清单与桥接解析同源：
// 都走目录的 request() 路径，列得出的名字就发得出对应变体）。
// 目录不可用时返回 nil —— 整段跳过，不编造名单。
func (h *Handler) cursorModelEntries() []map[string]any {
	models := h.cursorCatalog()
	if len(models) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		entry := map[string]any{
			"id":             cursorModelPrefix + m.ID,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "cursor",
			"context_length": m.Context,
		}
		if m.Name != "" && m.Name != m.ID {
			entry["description"] = m.Name
		}
		if len(m.Efforts) > 0 {
			// 按档位序输出（不给前端留 map 的随机序）。
			efforts := make([]string, 0, len(m.Efforts))
			for _, l := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
				if _, ok := m.Efforts[l]; ok {
					efforts = append(efforts, l)
				}
			}
			entry["reasoning_supported_efforts"] = efforts
		}
		if m.Fast {
			entry["fast_supported"] = true
		}
		out = append(out, entry)
	}
	return out
}

// cursorCatalog 列某账号可用的模型（fork usable 的对外面）。供
// /v1/models 列 cursor: 名单用：**任何失败都返回 nil**——模型清单
// 是增强信息，拉不到目录不该让整个 /v1/models 报错或卡住（与
// copilot/cline/autoclaw catalog 同款降级）。缓存走 handler 级
// cursorCatalogs，ModelsKeep（10 分钟）内不重复打上游。
//
// 冷启动这一次会真打上游（picker 30s / usable 8s 各自有超时，这里再加
// 一层 35s 上限兜底）：与其他 provider 的"实时拉目录 + 10 分钟缓存"
// 同一口径，不为此另立异步预取。
func (h *Handler) cursorCatalog() []cursor.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	var tok string
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PCursor || a.Disabled || a.SignInState == "expired" {
			continue
		}
		var cred cursor.Credential
		if json.Unmarshal(a.Cred, &cred) != nil || strings.TrimSpace(cred.AccessToken) == "" {
			continue
		}
		if cred.Lapsed(time.Now()) {
			continue
		}
		tok = cred.AccessToken
		break
	}
	if tok == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cat, err := h.cursorCatalogs.Get(ctx, cursor.New(), tok)
	if err != nil {
		log.Printf("cursor: 模型目录不可用（清单跳过 cursor: 条目）: %v", err)
		return nil
	}
	return cat.Models()
}

// cursorMarkNeedsMax 记下该 id 只能跑 Max Mode（fork maxOnly.add）。
func (h *Handler) cursorMarkNeedsMax(id string) {
	if id != "" {
		h.cursorMaxOnly.Store(id, struct{}{})
	}
}

// cursorOpenRun 发起一次 Run 并**先读一帧**（fork runOnce 的 head-value
// 语义：首帧是错误就不算建流成功，调用方才能换号/换模式重试）。
// 返回 (流, 首帧, 成功)。失败时首帧带错误内容供分类。
// v 是模型目录解析出的目标（id/params/maxMode）。
func (h *Handler) cursorOpenRun(r *http.Request, cli *cursor.Client, token string,
	conv *cursor.Conversation, tools []cursor.Tool, v cursor.Variant, convID string, forceMax bool) (*cursor.ChatStream, cursor.Event, bool) {
	req := cursor.ChatRequest{
		Token:   token,
		Model:   v.ID,
		Conv:    conv,
		Tools:   tools,
		ConvID:  convID,
		Params:  v.Params,
		MaxMode: v.MaxMode || forceMax,
	}
	stream, err := cli.ChatRun(r.Context(), req)
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
func (h *Handler) cursorPump(r *http.Request, w http.ResponseWriter, stream *cursor.ChatStream, first cursor.Event,
	model string, a *extstore.ExtAccount, wantStream bool) {
	defer stream.Close()

	events := stream.Events
	if !wantStream {
		agg := cursorAggregate(events, first, model)
		if e, isErr := agg["error"]; isErr && e != nil {
			h.noteChat(a.Provider, a.ID, errOf(fmt.Sprint(e)))
		}
		h.cursorRecordUsage(r, a, model, aggUsage(agg))
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
				// 与 qoder/zai 泵同款口径：思考走非标准 reasoning_content，
				// 不混进正文（Codex/Claude 客户端都读这个字段）。
				st.delta(map[string]any{"reasoning_content": ev.Reasoning}, nil)
			}
		case cursor.EventText:
			if ev.Text != "" {
				st.delta(map[string]any{"content": ev.Text}, nil)
			}
		case cursor.EventTool:
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
			h.cursorRecordUsage(r, a, model, ev.Usage)
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
	w       io.Writer
	flusher http.Flusher
	id      string
	created int64
	model   string
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

// cursorRecordUsage 终局记账（与 qoderRecordUsage 同模板、同口径差异声明）：
//  1. per-Key 小时桶先记（keyID 取调用方 API Key 摘要，空则不记；ext 通道
//     的失败尝试走 noteChat 账号级退避，不进 per-key 错误列）；
//  2. 账号维度桶后记（realm=ext，uid=provider/id，与腾讯池同源）。
//
// fire-and-forget：记账失败不影响已完成的响应。usage=nil（无终局用量）时
// per-Key 仍记一次调用、账号桶不记 token——与 qoder 桥同款语义。
func (h *Handler) cursorRecordUsage(r *http.Request, a *extstore.ExtAccount, model string, u *cursor.Usage) {
	if h.cfg.Usage == nil {
		return
	}
	keyID := h.callerKeyID(r)
	var d usage.Delta
	if u != nil {
		d = usage.Delta{
			PromptTokens:     u.PromptTokens,
			HasPromptTokens:  u.PromptTokens > 0,
			CompletionTokens: u.CompletionTokens,
			HasCompletion:    u.CompletionTokens > 0,
			TotalTokens:      u.TotalTokens,
			HasTotal:         u.TotalTokens > 0,
		}
	}
	if keyID != "" {
		h.cfg.Usage.AddKeyUsage(keyID, d, true, time.Now())
	}
	if u == nil {
		return
	}
	h.cfg.Usage.Add(time.Now(), "ext", string(a.Provider)+"/"+a.ID, model, d, true)
}

// aggUsage 从聚合响应里取 usage 段（cursorAggregate 无终局用量时不带）。
func aggUsage(agg map[string]any) *cursor.Usage {
	raw, ok := agg["usage"].(map[string]any)
	if !ok {
		return nil
	}
	num := func(k string) int64 {
		switch v := raw[k].(type) {
		case int64: // cursorAggregate 直接构造的 map（未经 JSON 往返）
			return v
		case float64: // 防御：若调用方给的是解码后的 map
			return int64(v)
		}
		return 0
	}
	return &cursor.Usage{
		PromptTokens:     num("prompt_tokens"),
		CompletionTokens: num("completion_tokens"),
		TotalTokens:      num("total_tokens"),
	}
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
