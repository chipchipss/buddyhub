package contextbudget

// 测试口径全部对照 fork context_window.py 的行为钉死：
// 预算内逐字透传、必留集合、检索回填、tool 完整性、末尾微修、fail-open。
import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ── token 估算 ──────────────────────────────────────────────────────────

// 估算口径：非代码 ≈4 字符/token，代码 ≈3.2，每条 +4 开销（fork L41-85）。
func TestEstimateTokens(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": strings.Repeat("a", 40)},     // 40/4+4 = 14
		map[string]any{"role": "assistant", "content": strings.Repeat("b", 8)}, // 8/4+4 = 6
	}
	if got := EstimateTokens(msgs); got != 20 {
		t.Fatalf("EstimateTokens = %d, want 20", got)
	}
	// 空列表 / 畸形元素不炸。
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("nil = %d, want 0", got)
	}
	if got := EstimateTokens([]any{"not-a-map", 42}); got != 0 {
		t.Fatalf("畸形元素 = %d, want 0（跳过不计）", got)
	}
	// 代码密度触发 3.2 口径（同样的字符量，代码估算 token 更多）。
	code := map[string]any{"role": "user", "content": "func f() {\n\tx = 1;\n}\n" + strings.Repeat("y", 100)}
	text := map[string]any{"role": "user", "content": strings.Repeat("y", 100)}
	if msgTokens(code) <= msgTokens(text) {
		t.Fatalf("代码估算应比文本贵: code=%d text=%d", msgTokens(code), msgTokens(text))
	}
	// tool_calls 的 name/arguments 计入字符量。
	withTools := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"function": map[string]any{"name": strings.Repeat("n", 40), "arguments": strings.Repeat("a", 40)}},
	}}
	if msgTokens(withTools) != 80/4+4 {
		t.Fatalf("tool_calls token = %d, want %d", msgTokens(withTools), 80/4+4)
	}
}

// ── 关闭与透传 ──────────────────────────────────────────────────────────

// budget<=0 = 关闭：原样返回，一个字节不动（buddyhub 默认关，零回归）。
func TestWindowDisabledByDefault(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": strings.Repeat("x", 100000)}}
	got, meta := Window(msgs, 0)
	if len(got) != 1 || meta.Windowed {
		t.Fatalf("budget=0 应原样返回: %+v", meta)
	}
	// 返回同一切片（透传语义：调用方改不改都随它）。
	if &got[0] != &msgs[0] {
		t.Fatal("budget=0 应返回同一切片引用")
	}
}

// 预算内 → 逐字透传（fork L217-231）。
func TestWindowUnderBudgetPassthrough(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": strings.Repeat("u", 100)},
	}
	got, meta := Window(msgs, 100000)
	if meta.Windowed || meta.OriginalTokens != meta.WindowedTokens || meta.Dropped != 0 {
		t.Fatalf("预算内应 windowed=false: %+v", meta)
	}
	if len(got) != 2 {
		t.Fatalf("预算内应全留: %d", len(got))
	}
	// PruneBody 未触发裁剪时返回**原始字节**（不重新序列化）。
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	out, meta := PruneBody(body, 100000)
	if string(out) != string(body) || meta.Windowed {
		t.Fatalf("预算内 body 应逐字一致: %s", out)
	}
}

// ── 超预算裁剪 ──────────────────────────────────────────────────────────

// 核心场景：system + 长历史 + 当前问题。必留 system/末条 user/近期尾部，
// 旧的非相关历史被丢，命中关键词的旧消息被逐字拉回。
func TestWindowOverBudgetKeepsEssentials(t *testing.T) {
	// 每条 filler 约 100 字符 ≈ 29 token；追加多条无关历史把总量推过预算。
	fill := func(role, pad string) map[string]any {
		return map[string]any{"role": role, "content": strings.Repeat(pad, 100)}
	}
	oldIrrelevant := fill("user", "z")       // 与当前问题无关
	oldWithKeyword := fill("assistant", "q") // 稍后塞关键词
	oldWithKeyword["content"] = "main.go 里的 handleRequest 需要修复 error 处理"
	sys := map[string]any{"role": "system", "content": "you are a helpful assistant"}
	recent1 := fill("assistant", "r")
	recent2 := fill("user", "s")
	question := map[string]any{"role": "user", "content": "请检查 main.go 里 handleRequest 的报错，关键词命中测试 main.go handleRequest error"}
	var msgs []any
	msgs = append(msgs, sys, oldIrrelevant, oldWithKeyword, recent1, recent2)
	// filler 每条内容唯一（用索引区分），否则 DeepEqual 分不清是哪一条。
	for i := 0; i < 6; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "filler-" + string(rune('a'+i)) + " " + strings.Repeat("z", 100)})
	}
	msgs = append(msgs, question)

	if EstimateTokens(msgs) <= 300 {
		t.Fatalf("前置条件不成立：总量 %d 未超预算 300", EstimateTokens(msgs))
	}
	got, meta := Window(msgs, 300)
	if !meta.Windowed {
		t.Fatal("超预算必须 windowed=true")
	}
	// system 必留（第一条）。
	if roleOf(got[0]) != "system" {
		t.Fatalf("system 必须第一个保留: %v", got[0])
	}
	// 最后一条 user（当前问题）必留。
	if !reflect.DeepEqual(got[len(got)-1], msgs[len(msgs)-1]) {
		t.Fatal("最后一条 user 必须保留")
	}
	// 输出保持原顺序（绝不重排）——验证输出是原列表的子序列。
	pos := 0
	for _, g := range got {
		found := false
		for ; pos < len(msgs); pos++ {
			if reflect.DeepEqual(msgs[pos], g) {
				found = true
				pos++
				break
			}
		}
		if !found {
			t.Fatalf("输出不是原列表的子序列（顺序被重排或出现拼接消息）: %v", g)
		}
	}
	// 无关旧消息应被丢（除非它被关键词拉回——这里它不含关键词）。
	droppedOld := true
	for _, g := range got {
		if reflect.DeepEqual(g, oldIrrelevant) {
			droppedOld = false
		}
	}
	if !droppedOld {
		t.Fatal("无关旧消息应被丢弃")
	}
	// 命中关键词的旧消息应被逐字拉回（retrieved > 0 或仍在输出里）。
	found := false
	for _, g := range got {
		if reflect.DeepEqual(g, oldWithKeyword) {
			found = true
		}
	}
	if !found && meta.Retrieved == 0 {
		t.Fatal("命中关键词的旧消息应被检索回填")
	}
	// 裁剪后估算 token 不超预算。
	if meta.WindowedTokens > meta.Budget {
		t.Fatalf("裁剪后 %d 超预算 %d", meta.WindowedTokens, meta.Budget)
	}
}

// tool 完整性：assistant(tool_calls) 与 tool 响应永不拆散（fork L295-323）。
func TestWindowToolPairIntegrity(t *testing.T) {
	// 构造：system + 旧无关 + assistant(tool_calls) + tool 响应 + 近期 + 问题。
	// 裁剪时若拉回了 tool 响应，它的发起者必须一起回来；反之亦然。
	assistantCall := map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{
			"id": "c1", "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`},
		}},
	}
	toolResp := map[string]any{"role": "tool", "tool_call_id": "c1", "content": strings.Repeat("file content ", 50)}
	filler := map[string]any{"role": "user", "content": strings.Repeat("filler ", 50)}
	sys := map[string]any{"role": "system", "content": "sys"}
	question := map[string]any{"role": "user", "content": "read_file main.go 的结果是什么 main.go read_file"}
	msgs := []any{sys, filler, assistantCall, toolResp, filler, filler, question}

	got, meta := Window(msgs, 200)
	if !meta.Windowed {
		t.Fatal("应触发裁剪")
	}
	// 验证：输出里若含 tool 响应，必含它的 assistant 发起者。
	hasTool, hasCall := false, false
	for _, g := range got {
		if roleOf(g) == "tool" {
			hasTool = true
		}
		if reflect.DeepEqual(g, assistantCall) {
			hasCall = true
		}
	}
	if hasTool && !hasCall {
		t.Fatal("tool 响应被保留但发起者被拆散（违反 tool 完整性）")
	}
	if hasCall && !hasTool {
		t.Fatal("assistant(tool_calls) 被保留但 tool 响应被拆散")
	}
	// 输出是原子序列。
	if meta.WindowedTokens > meta.Budget {
		t.Fatalf("tool 完整性补回后超预算: %d > %d", meta.WindowedTokens, meta.Budget)
	}
}

// 末尾微修：即使必留集合本身就超预算，也要砍到预算内（fork L329-343）。
func TestWindowTrimsToFitHard(t *testing.T) {
	// 巨型 system + 巨型问题：两者加起来就超预算——必须砍到预算内。
	bigSys := map[string]any{"role": "system", "content": strings.Repeat("s", 20000)}
	bigQ := map[string]any{"role": "user", "content": strings.Repeat("q", 20000)}
	filler := map[string]any{"role": "user", "content": strings.Repeat("f", 100)}
	msgs := []any{bigSys, filler, filler, bigQ}

	got, meta := Window(msgs, 2000)
	if !meta.Windowed {
		t.Fatal("应触发裁剪")
	}
	// system 与最后一条 user 永不砍（它们是绝对必留），但中间的必须让位。
	if meta.WindowedTokens > meta.Budget && len(got) > 2 {
		t.Fatalf("微修后仍超预算且还有可砍的: tokens=%d budget=%d kept=%d",
			meta.WindowedTokens, meta.Budget, len(got))
	}
}

// ── fail-open ──────────────────────────────────────────────────────────

// PruneBody 对畸形输入绝不失败：解析失败 / 无 messages → 原样返回。
func TestPruneBodyFailOpen(t *testing.T) {
	// 非 JSON。
	body := []byte("not json at all")
	out, _ := PruneBody(body, 100)
	if string(out) != string(body) {
		t.Fatal("非 JSON 应原样返回")
	}
	// JSON 但无 messages。
	body2 := []byte(`{"model":"m","prompt":"x"}`)
	out2, _ := PruneBody(body2, 100)
	if string(out2) != string(body2) {
		t.Fatal("无 messages 应原样返回")
	}
	// messages 不是数组。
	body3 := []byte(`{"messages":"oops"}`)
	out3, _ := PruneBody(body3, 100)
	if string(out3) != string(body3) {
		t.Fatal("messages 非数组应原样返回")
	}
	// budget<=0 直接返回。
	body4 := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 100000) + `"}]}`)
	out4, meta := PruneBody(body4, 0)
	if string(out4) != string(body4) || meta.Windowed {
		t.Fatal("budget=0 应原样返回")
	}
	// 正常裁剪：只动 messages，其余字段（model/temperature）保留。
	body5 := []byte(`{"model":"glm-5.2","temperature":0.7,"messages":[` +
		`{"role":"user","content":"` + strings.Repeat("x", 100000) + `"},` +
		`{"role":"user","content":"` + strings.Repeat("y", 100000) + `"}]}`)
	out5, meta5 := PruneBody(body5, 500)
	if !meta5.Windowed {
		t.Fatal("应触发裁剪")
	}
	var obj map[string]any
	if json.Unmarshal(out5, &obj) != nil {
		t.Fatalf("裁剪后不是合法 JSON: %s", out5)
	}
	if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
		t.Fatalf("非 messages 字段被动过: %+v", obj)
	}
	if _, ok := obj["messages"].([]any); !ok {
		t.Fatal("messages 丢了呢")
	}
}

// 关键词提取口径（fork L135-142）：标识符形态、去停用词、去纯数字。
func TestKeywords(t *testing.T) {
	kws := keywords("请检查 main.go 里 handleRequest 的报错，还有 the and config.yaml 404")
	for _, want := range []string{"main.go", "handlerequest", "config.yaml"} {
		if _, ok := kws[want]; !ok {
			t.Fatalf("关键词缺 %q: %v", want, kws)
		}
	}
	for _, notWant := range []string{"the", "and", "404"} {
		if _, ok := kws[notWant]; ok {
			t.Fatalf("停用词/纯数字不该进关键词: %q", notWant)
		}
	}
	// 空文本 → 空集（无关键词时不做检索，只靠必留+尾部）。
	if len(keywords("")) != 0 {
		t.Fatal("空文本应无关键词")
	}
}

// 空消息列表：原样返回（fork L204-215 的空分支）。
func TestWindowEmptyMessages(t *testing.T) {
	got, meta := Window(nil, 100)
	if len(got) != 0 || meta.Windowed {
		t.Fatalf("空列表应原样返回: %+v", meta)
	}
	got2, meta2 := Window([]any{}, 100)
	if len(got2) != 0 || meta2.Windowed {
		t.Fatalf("空切片应原样返回: %+v", meta2)
	}
}
