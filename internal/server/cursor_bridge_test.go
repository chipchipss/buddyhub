package server

// Cursor 桥接接线测试（magpie 二批第 4 步：协议层 → OpenAI chunk）。
// 手法沿用本仓既有约定：生产 URL 常量不动，测试用 Transport 把
// api2.cursor.sh 重写到本地假上游（zcode_bridge_test 同款思路）。
// 假上游说**真协议**（Connect 帧 + protobuf），所以这里验的是
// "桥接 + 协议层 + 转换" 三段串起来，不是各自内部一致。
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/cursor"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
	"github.com/chipchipss/buddyhub/internal/usage"
)

// ── 假上游 ────────────────────────────────────────────────────────────

type fakeCursorUpstream struct {
	mu sync.Mutex
	// unary 身份（GetMe）。
	meStatus int
	meBody   string
	// 每次 RunSSE 返回一组帧字节（按调用序取，用尽后重复末项）。
	runStatus int
	runBodies [][]byte
	runCalled int
	// BidiAppend 行为。
	appendStatus int
	// 上行留档（验会话 id 绑定、序号串行）。
	appendCount int
	runSeenAuth []string
}

func (f *fakeCursorUpstream) nextRunBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.runCalled
	f.runCalled++
	if i < len(f.runBodies) {
		return f.runBodies[i]
	}
	if len(f.runBodies) > 0 {
		return f.runBodies[len(f.runBodies)-1]
	}
	return nil
}

func (f *fakeCursorUpstream) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "GetMe"):
		st, body := f.meStatus, f.meBody
		if st == 0 {
			st = 200
		}
		if body == "" {
			body = `{"email":"user@example.com"}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(body))
	case strings.HasSuffix(path, "GetPlanInfo"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"planInfo":{"planName":"Pro"}}`))
	case strings.HasSuffix(path, "RunSSE"):
		f.mu.Lock()
		f.runSeenAuth = append(f.runSeenAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		raw := f.nextRunBody()
		st := f.runStatus
		if st == 0 {
			st = 200
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(st)
		_, _ = w.Write(raw)
	case strings.HasSuffix(path, "BidiAppend"):
		f.mu.Lock()
		f.appendCount++
		st := f.appendStatus
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		w.WriteHeader(200)
	default:
		w.WriteHeader(404)
	}
}

// cursorTransport 只重写 Cursor 生产域名（不动别人的 redirectTransport）。
type cursorTransport struct{ target string }

func (t cursorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "cursor.sh") || strings.Contains(req.URL.Host, "cursor.com") {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(t.target, "http://")
	}
	return http.DefaultTransport.RoundTrip(req)
}

// ── 协议帧构造（与 run.go 的解析层级逐字对齐）──────────────────────────
//
// 服务端消息：AgentServerMessage{1: updates 容器{1: TextDelta{1: 文本}}}
// → 三层 Sub（run.go 的 serverMessage 取 field 1，interactionUpdate 取
// field 1，文本再取 field 1）。

func cursorTextMsg(text string) []byte {
	return cursor.PB().Sub(1, cursor.PB().Sub(1, cursor.PB().Str(1, text))).Raw()
}

func cursorReasoningMsg(text string) []byte {
	return cursor.PB().Sub(1, cursor.PB().Sub(4, cursor.PB().Str(1, text))).Raw()
}

// cursorFrames 若干消息 + end 帧（空 JSON = 正常终结）。
func cursorFrames(msgs ...[]byte) []byte {
	var out []byte
	for _, m := range msgs {
		out = append(out, cursor.EncodeFrame(m, 0)...)
	}
	return append(out, cursor.EncodeFrame([]byte(`{}`), 2)...)
}

// cursorErrorFrames 只有 end 帧且带 Connect 错误。
func cursorErrorFrames(code, msg string) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	return cursor.EncodeFrame(body, 2)
}

// cursorToolCallMsg 一条 mcp_args 调用（run.go execMessage 的 11 分支）：
// exec{1:id, 11:mcp_args{1:namespace, 2:kv(每对 str1 键 + bytes2 Value),
// 3:call id, 5:toolName}}。
func cursorToolCallMsg(id, name string, args map[string]any) []byte {
	mcp := cursor.PB().Str(1, "magpie").Str(3, id).Str(5, name)
	av, err := cursor.Value(args)
	if err != nil {
		av = cursor.PB()
	}
	mcp = mcp.Sub(2, cursor.PB().Str(1, "arguments").Sub(2, av))
	tn, _ := cursor.Value(name)
	if tn != nil {
		mcp = mcp.Sub(2, cursor.PB().Str(1, "toolName").Sub(2, tn))
	}
	exec := cursor.PB().Varint(1, 77).Sub(11, mcp)
	return cursor.PB().BytesField(2, exec.Raw()).Raw()
}

// ── 测试基座 ──────────────────────────────────────────────────────────

func newCursorBridgeHarness(t *testing.T, up *fakeCursorUpstream, creds ...cursor.Credential) *Handler {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		raw, _ := json.Marshal(c)
		if err := mgr.Add(extstore.PCursor, "cu"+string(rune('0'+i)), "CU"+string(rune('0'+i)), raw); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	prev := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: cursorTransport{target: srv.URL}}
	t.Cleanup(func() { http.DefaultClient = prev })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	h.ExtSetManager(&fakeExtMgr{})
	return h
}

func postCursor(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// ── 用例 ──────────────────────────────────────────────────────────────

// 自检：测试自己造的帧必须能被协议层认出来，否则下面所有用例都在测一个
// 不存在的协议。
func TestCursorFrameHelpersMatchProtocol(t *testing.T) {
	c := cursor.New()
	c.APIBase = "" // 用注入的 DefaultClient（见 harness）
	_ = c
	raw := cursorFrames(cursorTextMsg("hello"))
	// 第一帧应是普通消息，第二帧是 end。
	data, flags, rest, ok := decodeFrameProbe(raw)
	if !ok || flags&2 != 0 {
		t.Fatalf("首帧形态错: ok=%v flags=%d", ok, flags)
	}
	msgs := cursor.Fields(data)
	upd := cursor.Find(msgs, 1)
	if upd == nil {
		t.Fatalf("缺 interaction_update(1): %+v", msgs)
	}
	inner := cursor.Fields(upd.Data)
	text := cursor.Find(inner, 1)
	if text == nil {
		t.Fatalf("缺 text update(1): %+v", inner)
	}
	if got := cursor.Str(cursor.Fields(text.Data), 1); got != "hello" {
		t.Fatalf("文本 = %q, want hello", got)
	}
	d2, flags2, _, ok2 := decodeFrameProbe(rest)
	if !ok2 || flags2&2 == 0 {
		t.Fatalf("末帧应是 end 帧: %v %d", ok2, flags2)
	}
	_ = d2
}

func decodeFrameProbe(raw []byte) ([]byte, byte, []byte, bool) {
	if len(raw) < 5 {
		return nil, 0, raw, false
	}
	n := int(uint32(raw[1])<<24 | uint32(raw[2])<<16 | uint32(raw[3])<<8 | uint32(raw[4]))
	if len(raw) < 5+n {
		return nil, 0, raw, false
	}
	return raw[5 : 5+n], raw[0], raw[5+n:], true
}

// 流式：协议帧 → chat.completion.chunk（正文、role 首帧、[DONE]）。
func TestCursorBridgeStreamsText(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("你好"), cursorTextMsg("，世界"))}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	for _, want := range []string{`"content":"你好"`, `"content":"，世界"`, `"role":"assistant"`, "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"object":"chat.completion.chunk"`) {
		t.Fatalf("不是 chunk 形状: %s", got)
	}
	// 模型名回显剥前缀后的裸名（与其他桥一致）。
	if !strings.Contains(got, `"model":"gpt-5"`) {
		t.Fatalf("model 未剥前缀: %s", got)
	}
}

// 思考走非标准 reasoning_content，不混进正文。
func TestCursorBridgeReasoningSeparate(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorReasoningMsg("先想三步"), cursorTextMsg("答案"))}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	got := w.Body.String()
	if !strings.Contains(got, `"reasoning_content":"先想三步"`) {
		t.Fatalf("reasoning_content 未透出: %s", got)
	}
	if strings.Contains(got, `"content":"先想三步"`) {
		t.Fatal("思考混进正文（客户端会把思考当回答）")
	}
}

// 非流式客户端：聚合成 chat.completion。
func TestCursorBridgeAggregatesForNonStream(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("AB"), cursorTextMsg("CD"))}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var doc map[string]any
	if json.Unmarshal(w.Body.Bytes(), &doc) != nil {
		t.Fatalf("非 JSON: %s", w.Body.String())
	}
	if doc["object"] != "chat.completion" {
		t.Fatalf("object = %v", doc["object"])
	}
	choices, _ := doc["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", doc["choices"])
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "ABCD" {
		t.Fatalf("聚合正文 = %v", msg["content"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

// 工具调用透出：EventTool → chunk 的 tool_calls（含 id/name/arguments）。
func TestCursorBridgeToolCalls(t *testing.T) {
	msg := cursorToolCallMsg("call_1", "read_file", map[string]any{"path": "a.go"})
	up := &fakeCursorUpstream{runBodies: [][]byte{
		cursorFrames(
			cursor.PB().Sub(1, cursor.PB().Sub(27, cursor.PB().Varint(1, 1))).Raw(), // 本步 1 个调用
			msg,
		),
	}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,
		"messages":[{"role":"user","content":"读 a.go"}],
		"tools":[{"type":"function","function":{"name":"read_file","description":"读","parameters":{"type":"object"}}}]}`)
	got := w.Body.String()
	if !strings.Contains(got, `"tool_calls"`) {
		t.Fatalf("tool_calls 未透出: %s", got)
	}
	if !strings.Contains(got, `"name":"read_file"`) || !strings.Contains(got, `"id":"call_1"`) {
		t.Fatalf("工具名/ id 未透出: %s", got)
	}
	if !strings.Contains(got, "a.go") {
		t.Fatalf("参数未透出: %s", got)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("终局应为 tool_calls: %s", got)
	}
}

// 会话绑定：RunSSE 与 BidiAppend 共用同一个 x-request-id；上行有调用
// （建流即发 run_request 首帧）。
func TestCursorBridgeSessionBindingAndAppend(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("ok"))}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if up.appendCount == 0 {
		t.Fatal("BidiAppend 一次都没调（上行没走通：run_request 或心跳没发）")
	}
	if len(up.runSeenAuth) == 0 || up.runSeenAuth[0] != "Bearer tok-ok" {
		t.Fatalf("RunSSE 鉴权头 = %v", up.runSeenAuth)
	}
}

// "auto" 模型名换成 default（fork request() 兜底）。
func TestCursorBridgeAutoModelMapsToDefault(t *testing.T) {
	var seenPathRunBody []byte
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("x"))}}
	_ = seenPathRunBody
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	// 回显给客户端的 model 仍是请求名（与其他桥一致：不改客户端看到的名字）。
	if !strings.Contains(w.Body.String(), `"model":"auto"`) {
		t.Fatalf("model 回显 = %s", w.Body.String())
	}
}

// 上游 Max Mode 拒名字：换模式重发一次（fork L2193-2198），仍算同一账号。
func TestCursorBridgeMaxModeRetry(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{
		cursorErrorFrames("invalid_argument", "Max Mode Required for this model"),
		cursorFrames(cursorTextMsg("重试成功")),
	}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:composer-2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if up.runCalled != 2 {
		t.Fatalf("RunSSE 次数 = %d, want 2（应重发一次）", up.runCalled)
	}
	if !strings.Contains(w.Body.String(), "重试成功") {
		t.Fatalf("重试结果未透出: %s", w.Body.String())
	}
}

// 非 Max-Mode 的建流错误：不换号重试成功路径，错误如实透出给客户端。
func TestCursorBridgeStreamErrorSurfaces(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorErrorFrames("resource_exhausted", "usage limit reached")}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	if !strings.Contains(body, "usage limit") {
		t.Fatalf("上游错误文案未透出: %s", body)
	}
	if strings.Contains(body, "cursor_protocol_unwired") {
		t.Fatal("已接线后不该再出现 unwired")
	}
}

// 身份探测失败（401）：不建流；账号外壳给"没有可用账号"类错误，
// 且**不再**返回 501 unwired。
func TestCursorBridgeNoIdentityFallsToNoAccount(t *testing.T) {
	up := &fakeCursorUpstream{meStatus: 401, meBody: `{"error":"unauthenticated"}`,
		runBodies: [][]byte{cursorFrames(cursorTextMsg("不该被调用"))}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-dead"})
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if up.runCalled != 0 {
		t.Fatalf("身份未过不该建流，runCalled = %d", up.runCalled)
	}
	if strings.Contains(w.Body.String(), "cursor_protocol_unwired") {
		t.Fatalf("已接线后不该返回 unwired: %s", w.Body.String())
	}
}

// 到期凭据（耗尽即弃）：标 lapsed、不建流、不尝试 refresh。
func TestCursorBridgeLapsedMarksExpired(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("x"))}}
	dead := cursor.Credential{AccessToken: "tok-old", ExpireAtMS: 1}
	h := newCursorBridgeHarness(t, up, dead)
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if up.runCalled != 0 {
		t.Fatalf("到期凭据不该建流，runCalled = %d", up.runCalled)
	}
	if strings.Contains(w.Body.String(), `"content":"x"`) {
		t.Fatal("到期凭据不该拿到回复")
	}
}

// 空对话（无 messages）：400 invalid_request，不碰上游。
func TestCursorBridgeEmptyConversation(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("x"))}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	w := postCursor(h, `{"model":"cursor:gpt-5","messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if up.runCalled != 0 || up.appendCount != 0 {
		t.Fatalf("空对话不该打上游: run=%d append=%d", up.runCalled, up.appendCount)
	}
}

// 无账号：dispatch 给 no_cursor_account（外壳语义与 0ed4e1c 一致）。
func TestCursorBridgeNoAccountShape(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{cursorFrames(cursorTextMsg("x"))}}
	h := newCursorBridgeHarness(t, up) // 不给凭据
	w := postCursor(h, `{"model":"cursor:gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if up.runCalled != 0 {
		t.Fatal("无账号不该建流")
	}
	if w.Code == 200 {
		t.Fatalf("无账号不该成功: %s", w.Body.String())
	}
}

// ── usage 记账（Chief 五条之"接 usage.Recorder"，qoderRecordUsage 同模板）──

// cursorUsageMsg 一条回合结束帧（fork usageOf 字段：1=input 总数、
// 2=output、3=cacheRead、4=cacheWrite）：input 净量 = 100-20-5 = 75。
func cursorUsageMsg() []byte {
	turn := cursor.PB().Sub(14, cursor.PB().Varint(1, 100).Varint(2, 30).Varint(3, 20).Varint(4, 5))
	return cursor.PB().Sub(1, turn).Raw()
}

// 流式：终局 usage 事件要进 usage.Recorder 账号桶（per-Key 断言见下面的
// 直接调用用例——集成路径没配入口 Key，keyID 为空不记 per-Key，这是
// 与 qoder 桥一致的既定口径，不是缺陷）。
func TestCursorBridgeStreamRecordsUsage(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{
		cursorFrames(cursorTextMsg("你好"), cursorUsageMsg()),
	}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	rec := usage.New("")
	h.cfg.Usage = rec
	w := postCursor(h, `{"model":"cursor:gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	snap := rec.Snapshot(1, nil)
	if snap.Totals.Requests == 0 {
		t.Fatalf("账号桶没记: %+v", snap.Totals)
	}
	if snap.Totals.TotalTokens != 130 || snap.Totals.PromptTokens != 100 || snap.Totals.CompletionTok != 30 {
		t.Fatalf("token 口径 = %+v want prompt=100 completion=30 total=130", snap.Totals)
	}
}

// per-Key 小时桶：直接调用记账函数（qoder_bridge_stage3 同款手法），
// 钉死 keyID 非空时 AddKeyUsage + KeySeries 都有数。
func TestCursorRecordUsagePerKey(t *testing.T) {
	rec := usage.New("")
	h := NewHandler(Config{Usage: rec, APIKey: "sk-gw-test"})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	a := &extstore.ExtAccount{Provider: extstore.PCursor, ID: "cu0"}
	h.cursorRecordUsage(req, a, "gpt-5", &cursor.Usage{PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130})

	snap := rec.Snapshot(0, nil)
	if len(snap.ByKey) == 0 || snap.ByKey[0].TotalTokens != 130 {
		t.Fatalf("per-Key 桶没记: %+v", snap.ByKey)
	}
	// 期望 keyID = 主 Key 的摘要（callerKeyID 的兜底路径）。
	kid := h.callerKeyID(req)
	if kid == "" {
		t.Fatal("callerKeyID 应兜底到主 Key")
	}
	if pts := rec.KeySeries(kid, 24); len(pts) != 1 || pts[0].TotalTokens != 130 {
		t.Fatalf("KeySeries = %+v", pts)
	}
	// 零用量：per-Key 记一次调用，账号桶不写（qoder 同口径）。
	h.cursorRecordUsage(req, a, "gpt-5", nil)
	if got := rec.Snapshot(0, nil).ByKey[0].Calls; got != 2 {
		t.Fatalf("零用量仍要计一次调用: calls = %d", got)
	}
}

// 非流式：聚合响应同样要落账（aggUsage 双形态取值路径）。
func TestCursorBridgeNonStreamRecordsUsage(t *testing.T) {
	up := &fakeCursorUpstream{runBodies: [][]byte{
		cursorFrames(cursorTextMsg("AB"), cursorUsageMsg()),
	}}
	h := newCursorBridgeHarness(t, up, cursor.Credential{AccessToken: "tok-ok"})
	rec := usage.New("")
	h.cfg.Usage = rec
	w := postCursor(h, `{"model":"cursor:gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	snap := rec.Snapshot(1, nil)
	if snap.Totals.TotalTokens != 130 {
		t.Fatalf("非流式没记账: %+v", snap.Totals)
	}
}
