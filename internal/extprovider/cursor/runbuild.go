// Cursor 对话请求的构造层（fork index.mjs L1079-1091、L1146-1308、
// L1545-1591，MIT）：OpenAI chat 请求 → AI SDK message JSON blob 序列 +
// AgentRunRequest 的 protobuf 首帧。
//
// 协议要点（fork L18-27 的文件头说明）：
//   - Cursor 把对话历史存在**客户端**：每条消息是一段 AI SDK message 的
//     JSON，以 sha256 命名（blob）；Run 的 state 里只带摘要，服务端按需
//     向客户端索要原文（kv_server_message，见 run.go 的应答分支）。
//   - 调用方的工具是 MCP 工具，模型通过 CallDynamicTool 调用；服务端把
//     每次调用交回客户端执行，本层负责把 tools 编进 state + 提示词目录。
//   - system 提示词要带工具目录（catalog）：Cursor 只通过 GetDynamicTools
//     /CallDynamicTool 看得到 MCP 工具，不给目录模型会说"我没这个工具"
//     （fork L1133-1139）。
package cursor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
)

// 协议常量（fork L44-51）。
const (
	callDynamicTool = "CallDynamicTool"
	noResult        = "Tool use was interrupted and did not produce a result."
	mcpNamespace    = "magpie"
)

// aiMsg 一条 AI SDK message（fork add() 的对象形态）。
// 用 struct 而非 map 保证 JSON 字段顺序确定——blob 以 sha256 命名，
// 字节序不定会让同一内容产生不同 id。
type aiMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// aiPart AI SDK 的 content part（text / reasoning / tool-call / image）。
type aiPart struct {
	Type       string       `json:"type"`
	Text       string       `json:"text,omitempty"`
	ToolCallID string       `json:"toolCallId,omitempty"`
	ToolName   string       `json:"toolName,omitempty"`
	Args       any          `json:"args,omitempty"`
	MimeType   string       `json:"mimeType,omitempty"`
	Image      *aiImageByte `json:"image,omitempty"`
}

// aiImageByte fork 的 {__type:"Uint8Array", hex} 形态（L1233）。
type aiImageByte struct {
	Type string `json:"__type"`
	Hex  string `json:"hex"`
}

// aiToolResult tool 消息里的一条结果（fork result() L1151-1164）。
type aiToolResult struct {
	Type               string          `json:"type"`
	ToolCallID         string          `json:"toolCallId"`
	ToolName           string          `json:"toolName"`
	Result             json.RawMessage `json:"result"`
	ExperimentalResult []aiPart        `json:"experimental_content"`
	IsError            bool            `json:"isError,omitempty"`
}

// Tool 一个调用方可用的工具（fork toolsOf 的元素）。
type Tool struct {
	Name        string
	Description string
	Schema      any    // 解析后的 JSON（进 pbValue）
	SchemaText  string // 原样 JSON 文本（进 toolDef 字段 6 与 catalog）
}

// Conversation 一次对话的客户端侧表示（fork conversation 的返回）。
type Conversation struct {
	Msgs     [][]byte // AI SDK message 的 JSON 逐条
	Last     string   // 最后一条 user 的首段文本（buildRun 的 user.content）
	Estimate int      // 粗估 prompt token（size/4），usage 缺失时兜底
}

// ── 基础转换 ───────────────────────────────────────────────────────────

// textOf 取消息 content 的纯文本（fork L1081-1082）。
func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			if p, ok := raw.(map[string]any); ok {
				if t, _ := p["type"].(string); t == "text" {
					if s, _ := p["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// reasoningOf 取思考文本（fork L1086：reasoning_content 或 reasoning）。
func reasoningOf(m map[string]any) string {
	for _, k := range []string{"reasoning_content", "reasoning"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// callID 还原 Cursor 原始 call id（fork L1149：call_ 前缀的把 __ 换回换行）。
func callID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return strings.Replace(id, "__fc_", "\nfc_", 1)
	}
	return id
}

// parseArgs 工具参数 JSON 解析（fork parseArgs L1109-1117：失败给空对象）。
func parseArgs(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) != nil {
		return map[string]any{}
	}
	return v
}

// joinSplitCalls 把拆开的两个 assistant 合并（fork L1088-1107）：
// Anthropic 把连续 assistant 当一个回合，一次回合的并行调用可能分两条发，
// 只在第二条应答——不合并的话第一条的调用会被标记中断、真实结果被丢。
func joinSplitCalls(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if len(out) > 0 {
			prev := out[len(out)-1]
			role, _ := m["role"].(string)
			pRole, _ := prev["role"].(string)
			pCalls, _ := prev["tool_calls"].([]any)
			if role == "assistant" && pRole == "assistant" && len(pCalls) > 0 {
				content := strings.Join(nonEmpty(textOf(prev["content"]), textOf(m["content"])), "\n\n")
				reasoning := strings.Join(nonEmpty(reasoningOf(prev), reasoningOf(m)), "\n\n")
				calls, _ := m["tool_calls"].([]any)
				merged := map[string]any{}
				for k, v := range prev {
					merged[k] = v
				}
				merged["content"] = content
				merged["reasoning_content"] = reasoning
				merged["tool_calls"] = append(append([]any{}, pCalls...), calls...)
				out[len(out)-1] = merged
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// ── 工具目录（fork catalog L1140-1145）────────────────────────────────

// catalog 给模型的调用方工具清单。Cursor 只通过 CallDynamicTool 用这些
// 工具，每项写成一次完整调用；有的模型（GLM-5.3）会裸调工具名导致落空，
// 所以清单直接给"该发的调用长什么样"。
func catalog(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<dynamic_tool_catalog>\nThe MCP namespace \"magpie\" has the tools below. They are reached only through `" + callDynamicTool + "`, never as tools of their own. Each entry is the exact call to make; its arguments must match the schema given. The schemas are complete, so there is no need to call `GetDynamicTools` first.\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "<call>%s({\"namespace\":\"magpie\",\"toolName\":\"%s\",\"arguments\":{...}})\n%s\narguments schema: %s\n</call>\n",
			callDynamicTool, t.Name, t.Description, t.SchemaText)
	}
	b.WriteString("</dynamic_tool_catalog>")
	return b.String()
}

// toolsOf 从请求体挑出模型可调用方工具（fork L1121-1131）：
// tool_choice="none" 一个不给；指定单个只给那一个。
func toolsOf(body map[string]any) []Tool {
	var only string
	switch c := body["tool_choice"].(type) {
	case string:
		if c == "none" {
			return nil
		}
	case map[string]any:
		if fn, ok := c["function"].(map[string]any); ok {
			only, _ = fn["name"].(string)
		}
	}
	raw, _ := body["tools"].([]any)
	var out []Tool
	for _, item := range raw {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if ty, _ := t["type"].(string); ty != "function" {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if only != "" && name != only {
			continue
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		st, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		desc, _ := fn["description"].(string)
		out = append(out, Tool{Name: name, Description: desc, Schema: schema, SchemaText: string(st)})
	}
	return out
}

// ── 对话构造（fork conversation L1171-1246）───────────────────────────

// BuildConversation 把 OpenAI 请求体转成 AI SDK message 序列。
// 返回 (对话, 可调用工具, 解析后的请求体, 错误)。
// 口径逐条对齐 fork：system/developer 合并 + 工具目录；tool 结果按
// call id 归位、未应答的调用补 noResult 应答（API 要求每个 call 都有结果）；
// assistant 的 reasoning 单列 part（放进文本会改变它的角色）；
// 工具调用一律包成 CallDynamicTool 的 args 形态。
func BuildConversation(body []byte) (*Conversation, []Tool, map[string]any, error) {
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, nil, nil, err
	}
	tools := toolsOf(chat)

	var out [][]byte
	add := func(m aiMsg) {
		b, err := json.Marshal(m)
		if err != nil {
			return
		}
		out = append(out, b)
	}
	size := 0

	// system 提示词 + 工具目录。
	var system []string
	msgsRaw, _ := chat["messages"].([]any)
	for _, raw := range msgsRaw {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := m["role"].(string); r == "system" || r == "developer" {
			if t := textOf(m["content"]); t != "" {
				system = append(system, t)
			}
		}
	}
	sys := strings.Join(system, "\n\n") + catalog(tools)
	size += len(sys)
	if sys != "" {
		add(aiMsg{Role: "system", Content: sys})
	}

	last := "."
	var pending []string
	var results []aiToolResult
	answer := func() {
		for _, id := range pending {
			results = append(results, mkResult(id, noResult, true))
		}
		pending = nil
		if len(results) > 0 {
			add(aiMsg{Role: "tool", Content: results})
			results = nil
		}
	}

	for _, raw := range joinSplitCalls(toAnyMaps(msgsRaw)) {
		role, _ := raw["role"].(string)
		switch role {
		case "system", "developer":
			continue
		case "tool":
			id, _ := raw["tool_call_id"].(string)
			idx := indexOf(pending, id)
			if idx < 0 {
				continue
			}
			pending = append(pending[:idx], pending[idx+1:]...)
			text := textOf(raw["content"])
			size += len(text)
			results = append(results, mkResult(id, text, false))
		case "assistant":
			answer()
			var content []aiPart
			reasoning := reasoningOf(raw)
			if reasoning != "" {
				content = append(content, aiPart{Type: "reasoning", Text: reasoning})
			}
			text := textOf(raw["content"])
			if text != "" {
				content = append(content, aiPart{Type: "text", Text: text})
			}
			size += len(reasoning) + len(text)
			calls, _ := raw["tool_calls"].([]any)
			for _, craw := range calls {
				c, ok := craw.(map[string]any)
				if !ok {
					continue
				}
				if ty, hasType := c["type"].(string); hasType && ty != "" && ty != "function" {
					continue
				}
				fn, _ := c["function"].(map[string]any)
				argStr, _ := fn["arguments"].(string)
				name, _ := fn["name"].(string)
				size += len(argStr) + len(name)
				id, _ := c["id"].(string)
				content = append(content, aiPart{
					Type:       "tool-call",
					ToolCallID: callID(id),
					ToolName:   callDynamicTool,
					Args: map[string]any{
						"namespace": mcpNamespace,
						"toolName":  name,
						"arguments": parseArgs(argStr),
					},
				})
				pending = append(pending, id)
			}
			if len(content) > 0 {
				add(aiMsg{Role: "assistant", Content: content})
			}
		default: // user
			var content []aiPart
			var first string
			switch c := raw["content"].(type) {
			case string:
				if c != "" {
					content = append(content, aiPart{Type: "text", Text: c})
					size += len(c)
					first = c
				}
			case []any:
				for _, praw := range c {
					p, ok := praw.(map[string]any)
					if !ok {
						continue
					}
					switch t, _ := p["type"].(string); t {
					case "text":
						if s, _ := p["text"].(string); s != "" {
							content = append(content, aiPart{Type: "text", Text: s})
							size += len(s)
							if first == "" {
								first = s
							}
						}
					case "image_url":
						u := imageURLString(p["image_url"])
						if mime, data, ok := splitDataURL(u); ok {
							content = append(content, aiPart{
								Type:     "image",
								MimeType: mime,
								Image:    &aiImageByte{Type: "Uint8Array", Hex: hexOf(data)},
							})
						} else if u != "" {
							content = append(content, aiPart{Type: "text", Text: fmt.Sprintf("[attachment image: %s]", u)})
						}
					case "file":
						name := "file"
						if f, ok := p["file"].(map[string]any); ok {
							if n, _ := f["filename"].(string); n != "" {
								name = n
							}
						}
						content = append(content, aiPart{Type: "text", Text: fmt.Sprintf("[attachment %s]", name)})
					}
				}
			}
			answer()
			if first != "" {
				last = first
			}
			if len(content) > 0 {
				add(aiMsg{Role: "user", Content: content})
			}
		}
	}
	answer()
	for _, t := range tools {
		size += len(t.Name) + len(t.Description) + len(t.SchemaText)
	}

	return &Conversation{
		Msgs:     out,
		Last:     last,
		Estimate: size / 4,
	}, tools, chat, nil
}

// mkResult 一条 tool-result（fork result() L1151-1164：能 parse 就嵌 JSON，
// 不能就嵌字符串；experimental_content 恒为文本 part）。
func mkResult(id, text string, isErr bool) aiToolResult {
	var res json.RawMessage
	if json.Valid([]byte(text)) {
		res = json.RawMessage(text)
	} else {
		b, _ := json.Marshal(text)
		res = b
	}
	return aiToolResult{
		Type:               "tool-result",
		ToolCallID:         callID(id),
		ToolName:           callDynamicTool,
		Result:             res,
		ExperimentalResult: []aiPart{{Type: "text", Text: text}},
		IsError:            isErr,
	}
}

func toAnyMaps(xs []any) []map[string]any {
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

// imageURLString image_url 既可能是字符串也可能对象（fork L1231）。
func imageURLString(v any) string {
	switch u := v.(type) {
	case string:
		return u
	case map[string]any:
		s, _ := u["url"].(string)
		return s
	}
	return ""
}

// splitDataURL 解析 data:<mime>;base64,<payload>（fork L1232 的正则）。
func splitDataURL(u string) (mime string, data []byte, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", nil, false
	}
	rest := u[len("data:"):]
	i := strings.Index(rest, ";base64,")
	if i < 0 {
		return "", nil, false
	}
	mime = rest[:i]
	payload := rest[i+len(";base64,"):]
	// 容忍带参数的 mime（image/png;charset=...）：按第一个分号切。
	if j := strings.Index(mime, ";"); j >= 0 {
		mime = mime[:j]
	}
	raw, err := decodeBase64(payload)
	if err != nil {
		return "", nil, false
	}
	return mime, raw, true
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// decodeBase64 宽容解码（fork 的 Buffer.from(x,"base64")）：标准字母表优先，
// 失败再试 URL-safe / 无填充——客户端给的 data URL 三种都见得到。
func decodeBase64(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// ── blob 与 Run 首帧（fork buildRun L1552-1591）───────────────────────

// Blobs 消息序列的 sha256 表：hex(摘要) → JSON 原文（服务端按摘要索要）。
func Blobs(msgs [][]byte) map[string][]byte {
	out := make(map[string][]byte, len(msgs))
	for _, m := range msgs {
		sum := sha256.Sum256(m)
		out[hexOf(sum[:])] = m
	}
	return out
}

// sha256Of 内容摘要（blob 键的算法人口）。
func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// ToolDef 一个 McpToolDefinition（fork toolDef L1251-1256：
// str1 name / str2 description / bytes3 schema Value / str4 namespace /
// str5 name / str6 schemaText）。
func ToolDef(t Tool) *Builder {
	d := PB().Str(1, t.Name)
	if t.Description != "" {
		d.Str(2, t.Description)
	}
	v, err := Value(t.Schema)
	if err != nil {
		v = PB().Varint(1, 0) // null（fork 的兜底：不支持的形态按 null）
	}
	d.Sub(3, v)
	return d.Str(4, mcpNamespace).Str(5, t.Name).Str(6, t.SchemaText)
}

// EnvPB 客户端环境（fork env L1258：GOOS / tmpdir / "UTC"）。
func EnvPB() *Builder {
	goos := runtime.GOOS
	return PB().Str(1, goos).Str(2, os.TempDir()).Str(10, "UTC")
}

// RunParts buildRun 的产物（fork L1590）。
type RunParts struct {
	First []byte            // AgentClientMessage（bytes 1 = run_request）
	Blobs map[string][]byte // sha256 hex → message JSON
}

// BuildRun 组 Run 首帧（fork buildRun L1552-1591 的逐字移植）：
// state（消息 blob 摘要 + turn）/ resume_action（工具 + env）/
// ModelDetails / mcp 工具表 / conversation_id / RequestedModel /
// 字段 19=inline images。convID 为空时按 fork 用新 UUID 顶替
// （runOnce L1806 的 cid 兜底，调用方拿不到 session 键时每次新建）。
func BuildRun(conv *Conversation, tools []Tool, model, convID string, maxMode bool, params []ModelParam) *RunParts {
	if convID == "" {
		convID = newUUID()
	}
	msgs := conv.Msgs
	blobs := Blobs(msgs)
	put := func(b []byte) []byte {
		sum := sha256.Sum256(b)
		blobs[hexOf(sum[:])] = b
		return sum[:]
	}

	state := PB()
	for _, m := range msgs {
		state.BytesField(1, put(m))
	}
	mid := newUUID()
	user := PB().Str(1, conv.Last).Str(2, mid).Varint(4, 1).Raw() // mode: agent
	turn := PB().Sub(1, PB().BytesField(1, put(user))).Str(10, mid).Raw()
	state.BytesField(8, put(turn)).Varint(10, 1).Str(22, "cli")

	defs := make([]*Builder, 0, len(tools))
	for i := range tools {
		defs = append(defs, ToolDef(tools[i]))
	}
	rc := PB().Sub(4, EnvPB())
	mcp := PB()
	for _, d := range defs {
		rc.Sub(7, d)
		mcp.Sub(1, d)
	}
	action := PB().Sub(2, PB().Sub(2, rc))

	// ModelDetails: model_id(1), display_model_id(3), display_name(4), max_mode(7)
	details := PB().Str(1, model).Str(3, model).Str(4, model)
	requested := PB().Str(1, model)
	if maxMode {
		details.Varint(7, 1)
		requested.Varint(2, 1)
	}
	for _, p := range params {
		requested.Sub(3, PB().Str(1, p.ID).Str(2, p.Value))
	}

	rr := PB().
		Sub(1, state).
		Sub(2, action).
		Sub(3, details).
		Sub(4, mcp).
		Str(5, convID).
		Sub(9, requested).
		Varint(19, 1) // inline images
	return &RunParts{First: PB().Sub(1, rr).Raw(), Blobs: blobs}
}

// ModelParam 模型的可选项（fork params：{id,value}，面板 picker 用）。
type ModelParam struct {
	ID    string
	Value string
}

// ── conversation_id（fork conversationID L1285-1308）──────────────────

// ConversationID 同一对话每次请求都用同一个 id（Cursor 后端按它把请求
// 路由到缓存了这份 prompt 的机器；Grok on Cursor 按机器缓存）。
// 取法：客户端的 prompt_cache_key（Codex 的 thread id），否则会话名；
// 再混入首条 user 消息（后续每次请求都会重复它）——同一会话下并行跑的
// 子代理（Claude Code 的 Task agents 共享 session id）在 Cursor 侧是
// 不同对话，与它自己的客户端一致。
// 没有任何会话键时返回 ""（每次 Run 新 id：只按首条消息会把陌生人的
// 对话并到一个 id 下）。
func ConversationID(body []byte) string {
	var chat map[string]any
	if json.Unmarshal(body, &chat) != nil {
		return ""
	}
	key := ""
	if s, ok := chat["prompt_cache_key"].(string); ok {
		key = strings.TrimSpace(s)
	}
	if key == "" {
		return "" // buddyhub 侧无 magpie 的 session 头；只认 prompt_cache_key
	}
	msgs, _ := chat["messages"].([]any)
	var first any
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if r, _ := mm["role"].(string); r == "user" {
				first = m
				break
			}
		}
	}
	if first == nil && len(msgs) > 0 {
		first = msgs[0]
	}
	fb, _ := json.Marshal(first)
	sum := sha256.Sum256([]byte("cursor conversation\x00" + key + "\x00" + string(fb)))
	return uuidOfBytes(sum[:16])
}

// uuidOfBytes 前 16 字节造 v4 UUID（fork uuidOf L1276-1283）。
func uuidOfBytes(b []byte) string {
	out := make([]byte, 16)
	copy(out, b[:16])
	out[6] = (out[6] & 0x0f) | 0x40
	out[8] = (out[8] & 0x3f) | 0x80
	h := hex.EncodeToString(out)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// sortedKeysFor 供需要确定性输出的调用方排序用（本包内 map 遍历都排序）。
func sortedStringKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
