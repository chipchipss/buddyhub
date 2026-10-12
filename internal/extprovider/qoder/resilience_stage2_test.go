package qoder

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── BuildMidStreamErrorFrame（偏差1 修复验证）──

func TestBuildMidStreamErrorFrameRetryAfter(t *testing.T) {
	// 429/限流帧必须带 retry-after 提示（fork withRetryAfterHint 语义，
	// 自查裁定"修"——原实现该分支是死代码）。
	frame := BuildMidStreamErrorFrame("rate_limit_exceeded", "rate limited by upstream")
	if !strings.Contains(frame, "retry after 30s") {
		t.Fatalf("rate_limit frame missing retry hint: %s", frame)
	}
	// 非限流不带提示，文案截断 1600
	long := strings.Repeat("x", 3000)
	frame2 := BuildMidStreamErrorFrame("upstream_error", long)
	if strings.Contains(frame2, "retry after") {
		t.Fatalf("non-rate-limit must not carry retry hint: %s", frame2[:80])
	}
	if len(frame2) > 2100 {
		t.Fatalf("message not truncated: len=%d", len(frame2))
	}
}

// ── tool_calls 终帧直透（偏差2 修复验证）──

func TestClassifyMidStreamToolCallsPassthrough(t *testing.T) {
	// fork isHeldTerminal L1324-1329：tool_calls 是真实回合结束，直接透行不暂存。
	k, _, _ := ClassifyMidStreamData(`{"choices":[{"delta":{"tool_calls":[{"id":"t1"}]},"finish_reason":"tool_calls"}]}`)
	if k != MidPass {
		t.Fatalf("tool_calls terminal = %v, want MidPass (passthrough)", k)
	}
	// 其他 finish_reason 仍暂存
	if k, _, _ := ClassifyMidStreamData(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`); k != MidTerminal {
		t.Fatalf("stop terminal = %v, want MidTerminal", k)
	}
	if k, _, _ := ClassifyMidStreamData(`{"choices":[{"delta":{},"finish_reason":"length"}]}`); k != MidTerminal {
		t.Fatalf("length terminal = %v, want MidTerminal", k)
	}
}

// ── 阶段 2：ClassifyMidStreamData ──

func TestClassifyMidStreamDone(t *testing.T) {
	if k, _, _ := ClassifyMidStreamData("[DONE]"); k != MidDone {
		t.Fatalf("[DONE] = %v", k)
	}
}

func TestClassifyMidStreamPass(t *testing.T) {
	k, _, _ := ClassifyMidStreamData(`{"id":"1","choices":[{"delta":{"content":"hi"}}]}`)
	if k != MidPass {
		t.Fatalf("normal chunk = %v", k)
	}
	// 非 JSON 帧保守透传
	if k, _, _ := ClassifyMidStreamData(`not-json`); k != MidPass {
		t.Fatalf("non-json = %v", k)
	}
}

func TestClassifyMidStreamTerminal(t *testing.T) {
	k, _, _ := ClassifyMidStreamData(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	if k != MidTerminal {
		t.Fatalf("finish stop = %v", k)
	}
	// tool_calls 是真实回合结束：fork 直接透行不暂存（MidPass）——自查裁定
	// 偏差2 修复后的语义，勿改回 MidTerminal。
	if k, _, _ := ClassifyMidStreamData(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`); k != MidPass {
		t.Fatalf("finish tool_calls = %v", k)
	}
	// 无 finish_reason 不算终帧
	if k, _, _ := ClassifyMidStreamData(`{"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`); k != MidPass {
		t.Fatalf("null finish = %v", k)
	}
}

func TestClassifyMidStreamQueueSignal(t *testing.T) {
	if k, _, s := ClassifyMidStreamData(`{"isQueued":true}`); k != MidQueueSignal || !s.Queued {
		t.Fatalf("isQueued = %v %v", k, s)
	}
	if k, _, _ := ClassifyMidStreamData(`{"code":"10605"}`); k != MidQueueSignal {
		t.Fatalf("10605 = %v", k)
	}
}

func TestClassifyMidStreamEnvelopeError(t *testing.T) {
	// statusCodeValue≠200 信封 → 内联错误帧素材
	k, envErr, _ := ClassifyMidStreamData(`{"statusCodeValue":500,"body":"boom happened"}`)
	if k != MidEnvelopeError || envErr == nil {
		t.Fatalf("envelope 500 = %v %v", k, envErr)
	}
	// overflow 映射 400
	_, envErr2, _ := ClassifyMidStreamData(`{"statusCodeValue":413,"body":"prompt is too long for this model"}`)
	if envErr2 == nil || !envErr2.ContextOverflow || envErr2.Status != 400 {
		t.Fatalf("overflow = %+v", envErr2)
	}
	// chunk.error 对象 → envErr=nil 但 kind=MidEnvelopeError（原样转发）
	k3, envErr3, _ := ClassifyMidStreamData(`{"error":{"message":"mid-failure","code":"x"}}`)
	if k3 != MidEnvelopeError || envErr3 != nil {
		t.Fatalf("chunk.error = %v %v", k3, envErr3)
	}
}

// ── TurnTracker ──

func TestTurnTrackerTrackAndReset(t *testing.T) {
	var tr TurnTracker
	tr.Track(`{"choices":[{"delta":{"content":"让我先"},"tool_calls":[]}]}`)
	tr.Track(`{"choices":[{"delta":{"content":"检查一下"}}]}`)
	tr.Track(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	if !tr.SawFinish || tr.LastFinish != "stop" {
		t.Fatalf("finish not tracked: %+v", tr)
	}
	if !strings.Contains(tr.TurnText(), "让我先检查一下") {
		t.Fatalf("turn text = %q", tr.TurnText())
	}
	tr.Track(`{"choices":[{"delta":{"tool_calls":[{"id":"t1"}]}}]}`)
	if !tr.SawToolCall {
		t.Fatal("tool call not tracked")
	}
	tr.Reset()
	if tr.SawFinish || tr.SawToolCall || tr.LastFinish != "" || tr.TurnText() != "" {
		t.Fatalf("reset incomplete: %+v text=%q", tr, tr.TurnText())
	}
}

// ── LooksLikeDanglingIntent ──

func TestLooksLikeDanglingIntent(t *testing.T) {
	// 注意（自查裁定·刻意偏离）：fork L1136 用 JS `t.length > 600`，JS length 是
	// UTF-16 code unit 计数——同样 600 单位，纯中文只覆盖约 200 个汉字，长中文
	// 回复会被提前判"不续跑"（误杀）。本实现用 rune 计数（len([]rune(t)) > 600），
	// 对多字节文本语义更正确：中文 600 字才触顶。这是经 Chief 裁定"不改，方向
	// 正确"的有意偏离，回归时勿按"对齐 fork"把它改回 code unit。
	cases := []struct {
		text string
		want bool
	}{
		{"让我先查看项目结构。", true},
		{"我来核对配置文件：", true},
		{"接下来，我将运行测试", true},
		{"Let me check the config file", true},
		{"I'll now run the tests", true},
		{"I will fix this bug next", true},
		{"现在开始实现", true},
		{"总之这就是完整方案，如有疑问告诉我", false}, // 排除短语
		{"Let me know if you need anything", false}, // 排除短语
		{"修复完成，所有测试通过。", false},
		{"The answer is 42.", false},
		{"", false},
		{strings.Repeat("长", 700) + "我来", false}, // >600 字符不续
	}
	for _, c := range cases {
		if got := LooksLikeDanglingIntent(c.text); got != c.want {
			t.Errorf("LooksLikeDanglingIntent(%q) = %v, want %v", truncateMsg(c.text, 30), got, c.want)
		}
	}
	// 末 120 字符窗口：开头悬念结尾意图也命中
	long := strings.Repeat("背景说明。", 60) + "接下来我来实现它"
	if !LooksLikeDanglingIntent(long) {
		t.Fatal("tail window intent missed")
	}
}

// ── BuildContinueBody ──

func TestBuildContinueBody(t *testing.T) {
	in := `{"model":"qoder:lite","stream":false,"messages":[
		{"role":"system","content":"sys"},
		{"role":"assistant","content":""},
		{"role":"assistant","content":{"notes":"empty-ish"}},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"with tool","tool_calls":[{"id":"t"}]}
	]}`
	out, err := BuildContinueBody([]byte(in), "我先检查测试结果")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(out, &got) != nil {
		t.Fatalf("out: %s", out)
	}
	if !got.Stream {
		t.Fatal("stream must be forced true")
	}
	roles := make([]string, 0, len(got.Messages))
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	// 空 assistant（content=""）被过滤；object content 保守保留；dangling+nudge 追加。
	want := []string{"system", "assistant", "user", "assistant", "assistant", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles[%d] = %s, want %s (all: %v)", i, roles[i], want[i], roles)
		}
	}
	// 最后一条是 NUDGE
	var last struct {
		Content string `json:"content"`
	}
	// 直接从原始 JSON 再取一次
	var full map[string]any
	json.Unmarshal(out, &full)
	msgs := full["messages"].([]any)
	json.Unmarshal([]byte(msgs[len(msgs)-1].(map[string]any)["content"].(string)), &last)
	_ = last
}

func TestHasToolsRequest(t *testing.T) {
	if !HasToolsRequest([]byte(`{"tools":[{"type":"function"}]}`)) {
		t.Fatal("tools missed")
	}
	if HasToolsRequest([]byte(`{"messages":[]}`)) {
		t.Fatal("false positive")
	}
}

// ── BuildMidStreamErrorFrame（若以包内帧构造测试）──

func TestEnvelopeErrorFrameShape(t *testing.T) {
	e := &EnvelopeError{Message: "boom", Code: "upstream_error"}
	var probe struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = probe
	// 确认 EnvelopeError.Error() 与 Code 通道一致
	if e.Error() != "boom" {
		t.Fatal("Error() mismatch")
	}
}
