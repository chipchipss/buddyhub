// Cursor 对话流客户端：HTTP/1.1 降级路径（fork index.mjs L1700-1799
// openH1 + L1803-1887 runOnce + L1891-2049 decode，MIT）。
//
// 为什么走这条：Cursor 的主路径是 HTTP/2 bidi（AgentService/Run），但 fork
// 自己就实现了无 HTTP/2 时的降级——**RunSSE 下行 + BidiAppend 逐条上行**，
// 全 HTTP/1.1 over api2。Go 标准库的 http2 客户端做 bidi 流写起来远不如
// 这条路直，且 cursor-agent 在代理/网络受限环境本来就走这条，上游支持是
// 一等的。x-request-id 把两条通道绑成同一个 bidi 会话。
//
// 帧协议（见 pb.go）：上行每条消息包成 BidiAppend 请求体的 field 1
// （**hex 字符串**，不是裸字节）；下行是标准 Connect 帧流。
package cursor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 协议路径（fork L42-44、L1758、L1774）。
const (
	pathRunSSE       = "/agent.v1.AgentService/RunSSE"
	pathBidiAppend   = "/aiserver.v1.BidiService/BidiAppend"
	allowedAgentTool = "mcp_tool_call,get_mcp_tools_tool_call"
)

// H1 时序（fork L1700：appendWait 60s，另按体积每 64KB 加 1s）。
const (
	appendWaitBase = 60 * time.Second
	appendWaitPer  = 64 * 1024
	heartbeatEvery = 5 * time.Second
)

// ClientVersionCursor 发给上游的客户端版本头（fork VERSION_FALLBACK L46：
// 本机没有 cursor-agent 安装时说的版本）。buddyhub 不探测本机 CLI——
// 如实声明这个差异：真实上游若校验版本与工具行为绑定，可能要改这里。
const ClientVersionCursor = "2026.09.23-86fc751"

// EventKind 事件类别。
type EventKind int

// 事件类别（fork decode 的 yield 形态：{text}/{reasoning}/{tool}/终局）。
const (
	EventText EventKind = iota
	EventReasoning
	EventTool
	EventDone
	EventError
)

// ToolCall 模型发起的一次调用方工具调用。
type ToolCall struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Args  string `json:"args"`
}

// Usage 本轮 token 用量（fork usageOf/finish 口径）。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
}

// Event 流上的一个事件。
type Event struct {
	Kind      EventKind
	Text      string
	Reasoning string
	Tool      ToolCall
	Usage     *Usage // Kind=EventDone 时有值
	Stop      string // stop | tool_calls
	Err       error  // Kind=EventError 时有值
	Status    int    // Kind=EventError 时的建议 HTTP 状态
	Code      string // Kind=EventError 时的 OpenAI 错误码
}

// ChatRequest 一次对话 Run 的输入。
type ChatRequest struct {
	Token   string        // access token（Bearer）
	Base    string        // agent API（fork agentURL 选的 host；空 = API）
	Model   string        // Cursor 内部模型名
	Conv    *Conversation // 构造层产物
	Tools   []Tool
	ConvID  string // 会话稳定 id（空则每次新建）
	MaxMode bool
	Params  []ModelParam
}

// ChatStream 一次 Run 的事件流。Close 幂等。
type ChatStream struct {
	Events <-chan Event

	closeOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	sender    *appendSender
}

// Close 结束 Run：停心跳、放掉上行。
func (s *ChatStream) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.sender.close()
	})
}

// ChatRun 发起一次对话（fork runOnce 的 H1 形态）。
// 返回的流在**首事件之前**不会返回错误：建流失败也会变成一个 EventError
// 事件，调用方按同一条路径处理（与 fork 的 head-value 语义一致——
// 它把首个错误从 iterator 里先读出来再返回）。
func (c *Client) ChatRun(ctx context.Context, req ChatRequest) (*ChatStream, error) {
	if strings.TrimSpace(req.Token) == "" {
		return nil, errors.New("cursor: 缺 access token")
	}
	if req.Conv == nil || len(req.Conv.Msgs) == 0 {
		return nil, errors.New("cursor: 对话为空")
	}
	base := req.Base
	if base == "" {
		base = c.agentBase()
	}

	// 会话绑定键：RunSSE 与 BidiAppend 都带同一个 x-request-id（fork L1718）。
	id := newUUID()
	parts := BuildRun(req.Conv, req.Tools, req.Model, req.ConvID, req.MaxMode, req.Params)

	runCtx, cancel := context.WithCancel(ctx)
	evs := make(chan Event, 32)
	sender := newAppendSender(c, runCtx, base, req.Token, id, cancel)
	stream := &ChatStream{Events: evs, cancel: cancel, done: make(chan struct{}), sender: sender}

	go c.pumpRun(runCtx, cancel, base, req, id, parts, evs, sender, stream.done)
	return stream, nil
}

// agentBase Run/BidiAppend 的上游 host。复用 APIBase（测试注入同一台假
// 服务器；生产为空回落 api2）。fork 的 agentURL 会查 ServerConfigService
// 按团队选区域，buddyhub 暂不做这一步——区域限制由错误分类如实透出。
func (c *Client) agentBase() string {
	if c.APIBase != "" {
		return c.APIBase
	}
	return API
}

// runHeaders 上游要求的客户端标识头（fork runOnce L1815-1825）。
// 关键两条：x-cursor-client-type=cli（否则 Cursor 会加它自己的提示词）、
// x-ghost-mode=true（隐私模式，不留存训练）。
func runHeaders(token, id string) map[string]string {
	return map[string]string{
		"authorization":                "Bearer " + token,
		"connect-protocol-version":     "1",
		"x-cursor-client-version":      ClientVersionCursor,
		"x-cursor-client-type":         "cli",
		"x-ghost-mode":                 "true",
		"x-request-id":                 id,
		"x-cursor-agent-allowed-tools": allowedAgentTool,
	}
}

// pumpRun 建流 + 读流 + 心跳（runOnce 的 H1 全路径）。
func (c *Client) pumpRun(ctx context.Context, cancel context.CancelFunc, base string, req ChatRequest,
	id string, parts *RunParts, evs chan Event, sender *appendSender, done <-chan struct{}) {
	defer close(evs)
	defer cancel()

	h := runHeaders(req.Token, id)
	body := EncodeFrame(PB().Str(1, id).Raw(), 0) // RunSSE 的入会消息：field 1 = 请求 id
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+pathRunSSE, bytes.NewReader(body))
	if err != nil {
		emitEvent(evs, Event{Kind: EventError, Err: err, Status: http.StatusBadGateway, Code: "upstream_error"})
		return
	}
	for k, v := range h {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("content-type", "application/connect+proto")
	httpReq.Header.Set("accept-encoding", "identity")
	httpReq.Header.Set("x-cursor-streaming", "true")

	resp, err := c.do(httpReq)
	if err != nil {
		emitEvent(evs, Event{Kind: EventError, Err: err, Status: http.StatusBadGateway, Code: "upstream_error"})
		return
	}
	defer resp.Body.Close()

	// 首帧前先发出 run_request（fork：send(first) 走 append #0）。
	if err := sender.send(parts.First); err != nil {
		emitEvent(evs, Event{Kind: EventError, Err: err, Status: http.StatusBadGateway, Code: "upstream_error"})
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		e := cursorFailure(resp.StatusCode, raw)
		emitEvent(evs, Event{Kind: EventError, Err: errors.New(e.message), Status: e.status, Code: e.code})
		return
	}

	// 心跳：客户端还在（fork L1834，5s 一次 field 7 空消息）。与解码应答
	// 同走 appendSender 的 wire 串行锁：上一条完成才发下一条（fork L1729
	// chain 语义），不会乱序上线。
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		for {
			select {
			case <-hbDone:
				return
			case <-ctx.Done():
				return
			case <-hb.C:
				_ = sender.send(PB().BytesField(7, nil).Raw())
			}
		}
	}()

	d := &runDecoder{
		evs:    evs,
		sender: sender,
		blobs:  parts.Blobs,
		tools:  req.Tools,
		// usage 缺失时的兜底估算（fork conv.estimate）。
		estimate: req.Conv.Estimate,
	}
	err = d.readStream(ctx, resp.Body)
	// append 失败票优先（fork end() 语义：失败即终局 Run）：fail() 已取消
	// runCtx，readStream 会带 Canceled 返回，这里把真实病因放成事件。
	if ferr := sender.failure(); ferr != nil && !d.settled {
		d.settled = true
		emitEvent(evs, Event{Kind: EventError, Err: ferr, Status: http.StatusBadGateway, Code: "upstream_error"})
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		// 读流失败但已经给过终局事件时不重复报。
		if !d.settled {
			d.settled = true
			emitEvent(evs, Event{Kind: EventError, Err: err, Status: http.StatusBadGateway, Code: "upstream_error"})
		}
	}
	if !d.settled {
		// 流自然结束却没说终局（fork 的 EOF 处理）。
		emitEvent(evs, Event{Kind: EventError, Err: errors.New("cursor: 流在终局前结束"),
			Status: http.StatusBadGateway, Code: "upstream_error"})
	}
	_ = done
}

func emitEvent(evs chan Event, e Event) {
	select {
	case evs <- e:
	case <-time.After(5 * time.Second):
	}
}

// ── 上行：BidiAppend 串行队列（fork L1721-1771 的 chain 语义）─────────

// appendSender 逐条上行；序号从 0 起、严格串行（乱序上游会拒）。
// 串行化是 fork chain 语义的移植（L1729：上一条**完成**才发下一条）：
// wire 锁覆盖"领号 + HTTP 全程"，序号即上线顺序；心跳与解码应答同走此队。
// 失败语义同 fork（L1766-1770）：非取消的 append 失败记票并终局整个 Run，
// 调用方吞错只是"已记票"的表象——pumpRun 读完后优先放票成 EventError。
type appendSender struct {
	c      *Client
	ctx    context.Context
	base   string
	token  string
	reqID  string
	cancel context.CancelFunc

	mu     sync.Mutex
	seq    int
	closed bool
	defs   map[string]string // execID → 工具定义缓存（未使用，留扩展）

	wire  sync.Mutex // 全程串行锁（领号到 HTTP 完成）
	fOnce sync.Once
	fErr  error
}

func newAppendSender(c *Client, ctx context.Context, base, token, reqID string, cancel context.CancelFunc) *appendSender {
	return &appendSender{c: c, ctx: ctx, base: base, token: token, reqID: reqID, cancel: cancel, defs: map[string]string{}}
}

func (a *appendSender) close() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
}

// failure 取 append 失败票（无票 = nil）。
func (a *appendSender) failure() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fErr
}

// send 一条消息（msg 是**裸消息体**，不带 5 字节帧头——fork 在 Duplex 的
// write 里 subarray(5) 剥掉头再 hex 化，L1727）。
func (a *appendSender) send(msg []byte) error {
	a.wire.Lock()
	defer a.wire.Unlock()

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	n := a.seq
	a.seq++
	a.mu.Unlock()

	inner := PB().Str(1, a.reqID).Raw()
	payload := PB().Str(1, hex.EncodeToString(msg)).BytesField(2, inner).Varint(3, uint64(n)).Raw()

	// 超时随体积放宽（fork L1755：60s + 每 64KB 加 1s）。
	wait := appendWaitBase + time.Duration((len(payload)+appendWaitPer-1)/appendWaitPer)*time.Second
	cctx, cancel := context.WithTimeout(a.ctx, wait)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, a.base+pathBidiAppend, bytes.NewReader(payload))
	if err != nil {
		return a.fail(fmt.Errorf("BidiAppend: %w", err))
	}
	for k, v := range runHeaders(a.token, a.reqID) {
		req.Header.Set(k, v)
	}
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("x-cursor-streaming", "true")

	resp, err := a.c.do(req)
	if err != nil {
		if a.ctx.Err() != nil {
			return nil // 已被取消（Close/客户端断连）：静默
		}
		return a.fail(fmt.Errorf("BidiAppend: %w", err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return a.fail(fmt.Errorf("BidiAppend HTTP %d", resp.StatusCode))
	}
	return nil
}

// fail 记票一次并终局 Run（fork 的 end() 语义：end 帧让读流者收到错误）。
func (a *appendSender) fail(err error) error {
	a.fOnce.Do(func() {
		a.mu.Lock()
		a.fErr = err
		a.mu.Unlock()
		a.cancel()
	})
	return err
}

// ── 下行：帧读取 + 消息解码（fork frames + decode）────────────────────

// readFrames 从 reader 里按 Connect 帧切，带 gzip 解压（fork L1018-1027）。
func readFrames(r io.Reader, each func(data []byte, isEnd bool) error) error {
	br := bufio.NewReader(r)
	for {
		head := make([]byte, 5)
		if _, err := io.ReadFull(br, head); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // 流结束
			}
			return err
		}
		flags := head[0]
		n := int(uint32(head[1])<<24 | uint32(head[2])<<16 | uint32(head[3])<<8 | uint32(head[4]))
		if n < 0 || n > MaxFrame {
			return ErrMalformedStream
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(br, data); err != nil {
			return err
		}
		if flags&1 != 0 {
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(gz)
			gz.Close()
			if err != nil {
				return err
			}
			data = raw
		}
		if err := each(data, flags&2 != 0); err != nil {
			return err
		}
	}
}

// runDecoder 把服务端消息翻成事件（fork decode L1891-2049）。
type runDecoder struct {
	evs    chan Event
	sender *appendSender
	blobs  map[string][]byte
	tools  []Tool
	// estimate 兜底 prompt token（fork conv.estimate）。
	estimate int

	// 统计与终局判定（fork 的 said/wrote/calls/listed/ended/counted）。
	said    int
	wrote   int
	calls   int
	listed  int
	ended   bool
	counted bool
	settled bool
	usage   Usage
}

// readStream 读满一条流，产出事件；settled 后不再产出。
func (d *runDecoder) readStream(ctx context.Context, r io.Reader) error {
	stop := false
	err := readFrames(r, func(data []byte, isEnd bool) error {
		if stop {
			return nil
		}
		if isEnd {
			d.finishFrame(data)
			stop = true
			return nil
		}
		d.serverMessage(data)
		if d.settled {
			stop = true
		}
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// finishFrame 处理 Connect 的 end 帧：带错误就报错误，否则按终局收尾
// （fork L1946-1952）。
func (d *runDecoder) finishFrame(data []byte) {
	e := cursorFailure(200, data)
	if e.status < 200 || e.status > 299 {
		d.emit(Event{Kind: EventError, Err: errors.New(e.message), Status: e.status, Code: e.code})
		d.settled = true
		return
	}
	if d.wrote == 0 && d.calls == 0 {
		// 什么都没写也什么都没调：空回复（客户端会重试，不是完成回合）。
		d.emit(Event{Kind: EventError,
			Err:    errors.New("cursor: 上游回了空回复（" + endFrameNote(data) + "）"),
			Status: http.StatusBadGateway, Code: "upstream_error"})
		d.settled = true
		return
	}
	d.emitDone()
}

// serverMessage 一条 AgentServerMessage（fork L1954-2049）。
func (d *runDecoder) serverMessage(data []byte) {
	for _, m := range Fields(data) {
		switch m.Num {
		case 1: // interaction_update
			d.interactionUpdate(m.Data)
		case 2: // exec_server_message：服务端要客户端跑东西
			d.execMessage(m.Data)
		case 4: // kv_server_message：索要 blob 或让客户端存东西
			d.kvMessage(m.Data)
		}
		if d.settled {
			return
		}
	}
}

// interactionUpdate 文本/思考/回合结束/调用计数（fork L1956-2001）。
func (d *runDecoder) interactionUpdate(data []byte) {
	for _, u := range Fields(data) {
		uf := Fields(u.Data)
		switch u.Num {
		case 1: // 正文
			t := Str(uf, 1)
			if t != "" {
				d.said += len([]rune(t))
				d.wrote += len([]rune(t))
				d.emit(Event{Kind: EventText, Text: t})
			}
		case 4: // 思考
			t := Str(uf, 1)
			if t != "" {
				d.said += len([]rune(t))
				d.emit(Event{Kind: EventReasoning, Reasoning: t})
			}
		case 14: // 回合结束，带用量
			d.usage = usageOf(uf)
			d.ended = true
			if d.calls == 0 {
				if d.wrote == 0 {
					d.emit(Event{Kind: EventError, Err: errors.New("cursor: 上游回了空回复"),
						Status: http.StatusBadGateway, Code: "upstream_error"})
				} else {
					d.emitDone()
				}
				d.settled = true
				return
			}
		case 27: // 本步会发几个工具调用（在调用之前送达）
			d.listed = int(Num(uf, 1))
			if d.listed > 0 && d.calls >= d.listed {
				d.emitDone()
				return
			}
		}
	}
}

// execMessage 应答服务端的执行请求（fork exec L2078-2119）。
// 三类：11 = 调用方工具（转成事件给客户端）；36 = 要工具清单；
// 10 = 要上下文；其余（Cursor 自己的 shell/读文件/编辑）一律拒绝。
func (d *runDecoder) execMessage(data []byte) {
	es := Fields(data)
	id := Num(es, 1)
	execID := Str(es, 15)
	answer := func(num int, res *Builder) {
		inner := PB().Varint(1, id).Str(15, execID).BytesField(num, res.Raw())
		_ = d.sender.send(PB().BytesField(2, inner.Raw()).Raw())
		// answer 自带收尾（fork L2082-2085 的 closeExec）：应答与关闭是两条消息。
		_ = d.sender.send(PB().BytesField(5, PB().Sub(1, PB().Varint(1, id)).Raw()).Raw())
	}

	for _, e := range es {
		switch e.Num {
		case 11: // mcp_args：调用方的工具被调了
			a := Fields(e.Data)
			args := map[string]any{}
			for _, kv := range a {
				if kv.Num != 2 {
					continue
				}
				ef := Fields(kv.Data)
				vf := Find(ef, 2)
				if vf == nil {
					args[Str(ef, 1)] = nil
					continue
				}
				v, err := AnyValue(vf.Data)
				if err != nil {
					continue
				}
				args[Str(ef, 1)] = v
			}
			name := Str(a, 5)
			if name == "" {
				name = strings.TrimPrefix(Str(a, 1), "magpie-")
			}
			// OpenAI 模型的 call id 与其 item 的 id 差一个换行，客户端不会
			// 把它当 id：换行外送时换成 __（fork L2099-2101）。
			cid := strings.ReplaceAll(Str(a, 3), "\n", "__")
			if cid == "" {
				buf := make([]byte, 12)
				_, _ = rand.Read(buf)
				cid = "call_" + hex.EncodeToString(buf)
			}
			// 工具参数还原：args 里是 {namespace,toolName,arguments}。
			toolName := name
			inner, _ := args["arguments"].(map[string]any)
			if tn, ok := args["toolName"].(string); ok && tn != "" {
				toolName = tn
			}
			argJSON := "{}"
			if inner != nil {
				if raw, err := json.Marshal(inner); err == nil {
					argJSON = string(raw)
				}
			} else if args["arguments"] != nil {
				if raw, err := json.Marshal(args["arguments"]); err == nil {
					argJSON = string(raw)
				}
			}
			d.said += len(argJSON) + len(toolName)
			d.emit(Event{Kind: EventTool, Tool: ToolCall{Index: d.calls, ID: cid, Name: toolName, Args: argJSON}})
			d.calls++
			// 调用方跑完工具后 Run 已关（fork #676 注释）：这里**不**应答，
			// 结果随下一次 Run 的对话历史回去。
			if d.listed > 0 && d.calls >= d.listed {
				d.emitDone()
			}
			return
		case 36: // mcp_state_exec_args：要工具清单
			srv := PB().Str(1, mcpNamespace).Str(2, mcpNamespace).Str(7, "connected")
			for i := range d.tools {
				srv.Sub(5, ToolDef(d.tools[i]))
			}
			answer(36, PB().Sub(1, PB().Sub(1, srv)))
			return
		case 10: // request_context_args：要上下文
			answer(10, PB().Sub(1, PB().Sub(1, PB().BytesField(4, EnvPB().Raw()))))
			return
		}
	}
	// 其余（Cursor 自己的 shell/读文件/编辑）：不跑，明说不可用（fork L2116-2119）。
	kind := uint64(0)
	for _, e := range es {
		if e.Num != 1 && e.Num != 15 {
			kind = uint64(e.Num)
			break
		}
	}
	_ = kind
	refused := PB().Varint(1, id).Str(2, "not available")
	_ = d.sender.send(PB().BytesField(5, PB().BytesField(2, refused.Raw()).Raw()).Raw())
}

// kvMessage blob 索要/存储应答（fork L2016-2030）。
func (d *runDecoder) kvMessage(data []byte) {
	kv := Fields(data)
	id := Num(kv, 1)
	for _, k := range kv {
		switch k.Num {
		case 2: // 索要某摘要的原文
			wf := Find(Fields(k.Data), 1)
			if wf == nil {
				continue
			}
			want := hex.EncodeToString(wf.Data)
			b, ok := d.blobs[want]
			var res *Builder
			if ok {
				res = PB().BytesField(1, b)
			} else {
				res = PB().BytesField(2, PB().Str(1, "blob not found").Raw())
			}
			inner := PB().Varint(1, id).BytesField(2, res.Raw())
			_ = d.sender.send(PB().BytesField(3, inner.Raw()).Raw())
		case 3: // 服务端自己的回合记录：回执即可
			inner := PB().Varint(1, id).BytesField(3, nil)
			_ = d.sender.send(PB().BytesField(3, inner.Raw()).Raw())
		}
	}
}

// emitDone 终局（fork finish L1911-1930）：用量缺失时按字符数与估算兜底，
// 保证面板与计费侧拿得到数。
func (d *runDecoder) emitDone() {
	u := d.usage
	if !d.counted && u.CompletionTokens == 0 {
		u.CompletionTokens = int64((d.said + 3) / 4)
	}
	if !d.counted && u.PromptTokens == 0 && u.CachedTokens == 0 && u.CacheWriteTokens == 0 {
		u.PromptTokens = int64(d.estimate)
	}
	prompt := u.PromptTokens + u.CachedTokens + u.CacheWriteTokens
	u.TotalTokens = prompt + u.CompletionTokens
	u.PromptTokens = prompt
	stop := "stop"
	if d.calls > 0 {
		stop = "tool_calls"
	}
	d.settled = true
	d.emit(Event{Kind: EventDone, Stop: stop, Usage: &u})
}

func (d *runDecoder) emit(e Event) { emitEvent(d.evs, e) }

// usageOf TurnEndedUpdate → Usage（fork usageOf L1316-1319）。
// 口径：input_tokens 是**整个 prompt**（含缓存读写），fork 把缓存项减出去
// 当未缓存 input；这里直接按字段存，prompt 在 emitDone 里加回。
func usageOf(uf []Field) Usage {
	inp, out, cr, cw, re := Num(uf, 1), Num(uf, 2), Num(uf, 3), Num(uf, 4), Num(uf, 5)
	in := inp
	if cr+cw > in {
		in = 0
	} else {
		in -= cr + cw
	}
	return Usage{
		PromptTokens:     int64(in),
		CompletionTokens: int64(out),
		CachedTokens:     int64(cr),
		CacheWriteTokens: int64(cw),
		ReasoningTokens:  int64(re),
	}
}

// endFrameNote end 帧的可读摘要（fork endFrame L2052-2058）。
func endFrameNote(data []byte) string {
	t := strings.TrimSpace(string(data))
	if t == "" || t == "{}" {
		return "空的 end 帧"
	}
	if len(t) > 300 {
		t = t[:300] + "…"
	}
	return "end 帧: " + t
}

// ── 错误分类（fork failure/statusOf L1036-1077）───────────────────────

// classified 分类后的错误。
type classified struct {
	status  int
	message string
	code    string
}

// cursorFailure 把 Cursor 的 Connect 错误翻成状态码 + 人话（fork 逐条）。
// 形态：{"code","message","details":[{"debug":{"details":{title,detail}}]}，
// 或同样的东西挂在 "error" 下。
func cursorFailure(status int, text []byte) classified {
	var e map[string]any
	if json.Unmarshal(text, &e) != nil {
		return classified{status: status, message: strings.TrimSpace(string(text)), code: ""}
	}
	obj := e
	if inner, ok := e["error"].(map[string]any); ok {
		obj = inner
	}
	msg, _ := obj["message"].(string)
	if details, ok := obj["details"].([]any); ok {
		for _, draw := range details {
			d, ok := draw.(map[string]any)
			if !ok {
				continue
			}
			dbg, _ := d["debug"].(map[string]any)
			dd, _ := dbg["details"].(map[string]any)
			title, _ := dd["title"].(string)
			detail, _ := dd["detail"].(string)
			t := strings.TrimSpace(title + ": " + detail)
			if t != ":" && t != "" {
				msg = strings.Trim(t, ": ")
			}
		}
	}
	code, _ := obj["code"].(string)
	if msg == "" || msg == "Error" {
		msg = code
	}
	if code == "" && status >= 200 && status < 300 {
		// 流正常结束。
		return classified{status: status, message: msg, code: ""}
	}
	return connectStatus(status, code, msg)
}

// connectStatus 按 code 与文案定状态（fork statusOf L1060-1077）。
func connectStatus(status int, code, msg string) classified {
	low := strings.ToLower(msg)
	out := func(s int, m string, c string) classified { return classified{s, m, c} }
	if strings.Contains(low, "region") {
		return out(http.StatusForbidden, msg+" — Cursor 只在该团队的部分区域提供服务并拒绝了本次请求；重新登录不改变这一点", "permission_denied")
	}
	switch {
	case code == "permission_denied":
		return out(http.StatusForbidden, msg, code)
	case code == "unauthenticated" || status == http.StatusUnauthorized || strings.Contains(low, "expired"):
		return out(http.StatusUnauthorized, msg+" — 请重新登录 Cursor", "unauthorized")
	case code == "resource_exhausted" || strings.Contains(low, "quota") ||
		strings.Contains(low, "rate limit") || strings.Contains(low, "usage limit"):
		return out(http.StatusTooManyRequests, "usage limit reached: "+msg, "rate_limit_exceeded")
	case strings.Contains(low, "too long") || strings.Contains(low, "context length") ||
		strings.Contains(low, "too many tokens"):
		return out(http.StatusBadRequest, "input is too long for the model's context: "+msg, "context_length_exceeded")
	case code == "invalid_argument":
		return out(http.StatusBadRequest, msg, code)
	case code == "unavailable":
		return out(http.StatusServiceUnavailable, msg, code)
	case code != "":
		return out(http.StatusBadGateway, msg, code)
	}
	return out(status, msg, code)
}

// connectCodeForHTTP 无自带错误时的 Connect 码（fork connectCode L1704-1706）。
func connectCodeForHTTP(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_argument"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusTooManyRequests:
		return "resource_exhausted"
	}
	return "unavailable"
}
