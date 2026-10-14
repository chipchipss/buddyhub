package cursor

// runbuild.go 测试：对话构造口径、blob 摘要确定性、Run 首帧字段号。
// 这些是"按 fork 语义逐字移植"的机器可验部分——不联网也能钉死。
import (
	"encoding/json"
	"strings"
	"testing"
)

// 最小请求：一条 user。验证 system 缺席时不造假 system 消息。
func TestBuildConversationSimpleUser(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello there"}]}`)
	conv, tools, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Msgs) != 1 {
		t.Fatalf("msgs = %d, want 1（无 system 不造 system）", len(conv.Msgs))
	}
	if conv.Last != "hello there" {
		t.Fatalf("last = %q", conv.Last)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %d, want 0", len(tools))
	}
	var m aiMsg
	if json.Unmarshal(conv.Msgs[0], &m) != nil {
		t.Fatalf("msg 不是合法 JSON: %s", conv.Msgs[0])
	}
	if m.Role != "user" {
		t.Fatalf("role = %q", m.Role)
	}
}

// system/developer 合并进同一条 system，并追加工具目录。
func TestBuildConversationSystemAndCatalog(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"你是助手"},
		{"role":"developer","content":"遵守规范"},
		{"role":"user","content":"读 main.go"}
	],"tools":[{"type":"function","function":{"name":"read_file","description":"读文件","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`)
	conv, tools, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("tools = %+v", tools)
	}
	var sys aiMsg
	if json.Unmarshal(conv.Msgs[0], &sys) != nil {
		t.Fatal("system 消息解析失败")
	}
	content, _ := sys.Content.(string)
	if !strings.Contains(content, "你是助手") || !strings.Contains(content, "遵守规范") {
		t.Fatalf("system 未合并: %q", content)
	}
	if strings.Index(content, "你是助手") > strings.Index(content, "遵守规范") {
		t.Fatal("system 合并顺序变了（应保序）")
	}
	// 工具目录：模型只能经 CallDynamicTool 看到 MCP 工具，不给目录它会说
	// "没这个工具"（fork L1133-1139）。
	if !strings.Contains(content, "<dynamic_tool_catalog>") || !strings.Contains(content, "read_file") {
		t.Fatalf("工具目录缺失: %q", content)
	}
	if conv.Last != "读 main.go" {
		t.Fatalf("last = %q", conv.Last)
	}
}

// assistant 的工具调用必须包成 CallDynamicTool 形态，args 带
// namespace/toolName/arguments（fork L1215）。
func TestBuildConversationToolCallShape(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"读文件"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}
	]}`)
	conv, _, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	var m aiMsg
	if json.Unmarshal(conv.Msgs[1], &m) != nil {
		t.Fatal("assistant 消息解析失败")
	}
	if m.Role != "assistant" {
		t.Fatalf("role = %q", m.Role)
	}
	raw, _ := json.Marshal(m.Content)
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		t.Fatalf("content 不是数组: %s", raw)
	}
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1（空 content 不占位）", len(parts))
	}
	p := parts[0]
	if p["type"] != "tool-call" || p["toolName"] != callDynamicTool {
		t.Fatalf("工具调用形态错: %v", p)
	}
	args, _ := p["args"].(map[string]any)
	if args["namespace"] != mcpNamespace || args["toolName"] != "read_file" {
		t.Fatalf("args 错: %v", args)
	}
	inner, _ := args["arguments"].(map[string]any)
	if inner["path"] != "a.go" {
		t.Fatalf("arguments 未解析: %v", args["arguments"])
	}
}

// 未应答的工具调用补 noResult 应答（API 要求每个 call 都有结果，
// fork L1184-1189）；有结果的按 id 归位且不重复补。
func TestBuildConversationToolResults(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"t1","arguments":"{}"}},
			{"id":"call_b","type":"function","function":{"name":"t2","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call_a","content":"ok"}
	]}`)
	conv, _, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	// 末条应是 tool 消息，含两条结果：call_a 的真结果 + call_b 的补应答。
	var m aiMsg
	if json.Unmarshal(conv.Msgs[len(conv.Msgs)-1], &m) != nil {
		t.Fatal("末条解析失败")
	}
	if m.Role != "tool" {
		t.Fatalf("末条 role = %q, want tool", m.Role)
	}
	raw, _ := json.Marshal(m.Content)
	var results []map[string]any
	if json.Unmarshal(raw, &results) != nil {
		t.Fatalf("tool content 不是数组: %s", raw)
	}
	if len(results) != 2 {
		t.Fatalf("结果数 = %d, want 2", len(results))
	}
	byID := map[string]map[string]any{}
	for _, r := range results {
		byID[r["toolCallId"].(string)] = r
	}
	if got := byID["call_a"]; got == nil || got["isError"] == true {
		t.Fatalf("call_a 应答错: %v", got)
	}
	gotB := byID["call_b"]
	if gotB == nil || gotB["isError"] != true {
		t.Fatalf("未应答的 call_b 应补 isError=true: %v", gotB)
	}
	exp, _ := gotB["experimental_content"].([]any)
	if len(exp) != 1 || exp[0].(map[string]any)["text"] != noResult {
		t.Fatalf("补应答文案错: %v", gotB["experimental_content"])
	}
}

// reasoning 单独成 part（放进文本会改变角色，fork L1206-1209）。
func TestBuildConversationReasoningPart(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"x"},
		{"role":"assistant","content":"答案是 42","reasoning_content":"先想三步"}
	]}`)
	conv, _, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	var m aiMsg
	json.Unmarshal(conv.Msgs[1], &m)
	raw, _ := json.Marshal(m.Content)
	if !strings.Contains(string(raw), `"type":"reasoning"`) || !strings.Contains(string(raw), `"type":"text"`) {
		t.Fatalf("reasoning/text 未分开: %s", raw)
	}
	var parts []map[string]any
	json.Unmarshal(raw, &parts)
	if parts[0]["type"] != "reasoning" || parts[1]["type"] != "text" {
		t.Fatalf("part 顺序错（reasoning 应在前）: %v", parts)
	}
	if parts[0]["text"] != "先想三步" || parts[1]["text"] != "答案是 42" {
		t.Fatalf("part 内容错: %v", parts)
	}
}

// blob 摘要确定性：同一内容两次构造必须得到同一个 sha256（AI SDK message
// 用 struct 保字段序，不用 map——Go map JSON 序列化按 key 排序也稳，
// 但显式 struct 更直白）。且 Blobs 的 key 就是内容摘要。
func TestBlobsDeterministic(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":[{"type":"text","text":"读 main.go"}]}
	]}`)
	c1, _, _, _ := BuildConversation(body)
	c2, _, _, _ := BuildConversation(body)
	if len(c1.Msgs) != len(c2.Msgs) {
		t.Fatal("两次构造消息数不同")
	}
	b1, b2 := Blobs(c1.Msgs), Blobs(c2.Msgs)
	if len(b1) != len(c1.Msgs) {
		t.Fatalf("blob 表大小 = %d, want %d", len(b1), len(c1.Msgs))
	}
	k1 := sortedStringKeys(b1)
	k2 := sortedStringKeys(b2)
	for i := range k1 {
		if k1[i] != k2[i] {
			t.Fatalf("摘要不稳定: %s vs %s", k1[i], k2[i])
		}
		// key 必须就是 value 的 sha256（服务端按摘要索要，对不上就是丢文）。
		if hexOf(sha256Of(b1[k1[i]])) != k1[i] {
			t.Fatalf("key 不是内容摘要: %s", k1[i])
		}
	}
}

// 拆开的并行调用合并（fork joinSplitCalls：不合并第一条的调用会被标记
// 中断、真实结果被丢）。
func TestJoinSplitCallsMerges(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "go"},
		{"role": "assistant", "content": "第一步", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "a", "arguments": "{}"}},
		}},
		{"role": "assistant", "content": "第二步", "tool_calls": []any{
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "b", "arguments": "{}"}},
		}},
	}
	out := joinSplitCalls(msgs)
	if len(out) != 2 {
		t.Fatalf("应合并为 2 条, got %d", len(out))
	}
	calls, _ := out[1]["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("合并后调用数 = %d, want 2", len(calls))
	}
	content, _ := out[1]["content"].(string)
	if !strings.Contains(content, "第一步") || !strings.Contains(content, "第二步") {
		t.Fatalf("合并丢文本: %q", content)
	}
}

// tool_choice 过滤（fork toolsOf L1121-1126）：none 不给，指定只给那一个。
func TestToolsOfChoiceFiltering(t *testing.T) {
	mk := func(choice string) []Tool {
		body := `{"tools":[
			{"type":"function","function":{"name":"a","description":"A","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"b","description":"B","parameters":{"type":"object"}}}
		]`
		if choice != "" {
			body += `,"tool_choice":` + choice
		}
		body += `}`
		var chat map[string]any
		json.Unmarshal([]byte(body), &chat)
		return toolsOf(chat)
	}
	if got := mk(""); len(got) != 2 {
		t.Fatalf("无 tool_choice = %d, want 2", len(got))
	}
	if got := mk(`"none"`); len(got) != 0 {
		t.Fatalf(`"none" = %d, want 0`, len(got))
	}
	if got := mk(`{"type":"function","function":{"name":"a"}}`); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("指定单个 = %+v", got)
	}
}

// Run 首帧的字段号与嵌套（fork buildRun L1552-1590）：
// AgentClientMessage{1:run_request{1:state,2:action,3:details,4:mcp,
// 5:conversation_id,9:requested,19:1}}；state{1:blob摘要repeated, 8:turn,
// 10:1, 22:"cli"}。解一遍确认结构没漂。
func TestBuildRunStructure(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"最后一个问题"}
	]}`)
	conv, tools, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	parts := BuildRun(conv, tools, "gpt-5", "conv-fixed", false, nil)
	if parts.First == nil {
		t.Fatal("首帧为空")
	}
	// AgentClientMessage field 1 → run_request。
	outer := Fields(parts.First)
	rr := Find(outer, 1)
	if rr == nil {
		t.Fatalf("首帧缺 field 1: %+v", outer)
	}
	run := Fields(rr.Data)
	if Str(run, 5) != "conv-fixed" {
		t.Fatalf("conversation_id = %q", Str(run, 5))
	}
	if Num(run, 19) != 1 {
		t.Fatal("inline images 标志位(19)缺失")
	}
	for _, num := range []int{1, 2, 3, 4, 9} {
		if Find(run, num) == nil {
			t.Fatalf("run_request 缺 field %d", num)
		}
	}

	// state：每条消息一个 field 1（32 字节摘要）+ turn 在 field 8。
	state := Fields(Find(run, 1).Data)
	var digests int
	for _, f := range state {
		if f.Num == 1 && len(f.Data) == 32 {
			digests++
		}
	}
	if digests != len(conv.Msgs) {
		t.Fatalf("state 摘要数 = %d, want %d", digests, len(conv.Msgs))
	}
	if Str(state, 22) != "cli" {
		t.Fatalf("state.client_name = %q", Str(state, 22))
	}

	// details/requested 的 model 名三处一致（fork str(1)=str(3)=str(4)）。
	details := Fields(Find(run, 3).Data)
	if Str(details, 1) != "gpt-5" || Str(details, 3) != "gpt-5" || Str(details, 4) != "gpt-5" {
		t.Fatalf("ModelDetails 错: %v", details)
	}
	if Find(details, 7) != nil {
		t.Fatal("maxMode=false 不该有 field 7")
	}
	req := Fields(Find(run, 9).Data)
	if Str(req, 1) != "gpt-5" {
		t.Fatalf("RequestedModel 错")
	}

	// last user 文本进 turn 的 user.content（服务端采样要看的那条）。
	if conv.Last != "最后一个问题" {
		t.Fatalf("last = %q", conv.Last)
	}
	// blob 表覆盖所有消息 + user 与 turn 两个附加 blob
	// （fork 的 put() 对它们同样入表，服务端可能索要）。
	if len(parts.Blobs) != len(conv.Msgs)+2 {
		t.Fatalf("blob 表 = %d, want %d", len(parts.Blobs), len(conv.Msgs)+2)
	}
}

// maxMode 与参数开关进 pb（Cursor 对只在 Max Mode 提供的模型会拒
// "Max Mode Required"，fork L1577-1581）。
func TestBuildRunMaxModeAndParams(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	conv, tools, _, _ := BuildConversation(body)
	parts := BuildRun(conv, tools, "composer-2", "", true, []ModelParam{{ID: "speed", Value: "fast"}})
	run := Fields(Find(Fields(parts.First), 1).Data)
	details := Fields(Find(run, 3).Data)
	if Num(details, 7) != 1 {
		t.Fatal("max_mode(7) 未设")
	}
	req := Fields(Find(run, 9).Data)
	if Num(req, 2) != 1 {
		t.Fatal("RequestedModel.max_mode(2) 未设")
	}
	p := Find(req, 3)
	if p == nil {
		t.Fatal("参数(3) 未进")
	}
	pf := Fields(p.Data)
	if Str(pf, 1) != "speed" || Str(pf, 2) != "fast" {
		t.Fatalf("参数形态错: %v", pf)
	}
	// convID 空 → 自造 UUID（fork runOnce L1806 兜底）。
	if len(Str(run, 5)) != 36 || !strings.Contains(Str(run, 5), "-") {
		t.Fatalf("兜底 conversation_id 不是 UUID 形态: %q", Str(run, 5))
	}
}

// conversation_id 稳定性：同 prompt_cache_key + 同首条 user → 同 id
// （Cursor 按它把请求路由到缓存了这份 prompt 的机器）。
func TestConversationIDSStable(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"thread-1","messages":[
		{"role":"user","content":"first"}]}`)
	a := ConversationID(body)
	b := ConversationID(body)
	if a != b || len(a) != 36 {
		t.Fatalf("同输入 id 不稳: %q vs %q", a, b)
	}
	// 换 key → 换 id。
	other := ConversationID([]byte(`{"prompt_cache_key":"thread-2","messages":[{"role":"user","content":"first"}]}`))
	if other == a {
		t.Fatal("不同 prompt_cache_key 应得不同 id")
	}
	// 无 key → ""（每次 Run 新 id；只按首条消息会把陌生人的对话并到一个 id）。
	if got := ConversationID([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); got != "" {
		t.Fatalf("无会话键应为空串, got %q", got)
	}
	// 畸形输入不 panic。
	if got := ConversationID([]byte("not json")); got != "" {
		t.Fatalf("非 JSON 应空串")
	}
}

// data URL 图片进 image part（fork L1230-1234 的 __type Uint8Array/hex）；
// 非 data URL 降级为文本附件说明，不静默丢。
func TestBuildConversationImageParts(t *testing.T) {
	// 1x1 PNG 的 base64。
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
		{"type":"image_url","image_url":"https://x.example/a.png"},
		{"type":"file","file":{"filename":"spec.md"}}
	]}]}`)
	conv, _, _, err := BuildConversation(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(conv.Msgs[0])
	if !strings.Contains(raw, `"type":"image"`) || !strings.Contains(raw, "image/png") {
		t.Fatalf("data URL 图片未转 image part: %s", raw)
	}
	if !strings.Contains(raw, `"__type":"Uint8Array"`) || !strings.Contains(raw, "89504e470d0a1a0a") {
		t.Fatalf("图片负载未按 {__type,hex} 形态: %s", raw)
	}
	if !strings.Contains(raw, "[attachment image: https://x.example/a.png]") {
		t.Fatalf("远程图片应降级为文本说明: %s", raw)
	}
	if !strings.Contains(raw, "[attachment spec.md]") {
		t.Fatalf("file part 应降级为附件名: %s", raw)
	}
	if conv.Last != "看图" {
		t.Fatalf("last 应取首段文本: %q", conv.Last)
	}
}

// toolDef 的六字段（fork L1251-1256）。
func TestToolDefFields(t *testing.T) {
	tools := toolsOf(map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "description": "读文件",
			"parameters": map[string]any{"type": "object"},
		}},
	}})
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	raw := ToolDef(tools[0]).Raw()
	fs := Fields(raw)
	if Str(fs, 1) != "read_file" || Str(fs, 2) != "读文件" {
		t.Fatalf("name/description 错: %v", fs)
	}
	if Str(fs, 4) != mcpNamespace || Str(fs, 5) != "read_file" {
		t.Fatalf("namespace/name 重复位错: %v", fs)
	}
	if Str(fs, 6) == "" {
		t.Fatal("schemaText(6) 缺失")
	}
	schema := Find(fs, 3)
	if schema == nil {
		t.Fatal("schema(3) 缺失")
	}
	v, err := AnyValue(schema.Data)
	if err != nil {
		t.Fatalf("schema 解码: %v", err)
	}
	m, _ := v.(map[string]any)
	if m["type"] != "object" {
		t.Fatalf("schema 内容错: %v", v)
	}
}
