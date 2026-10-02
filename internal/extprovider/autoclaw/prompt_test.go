package autoclaw

import (
	"encoding/json"
	"strings"
	"testing"
)

// 上游 2026-09-22 起的 system 提示词闸门（prompt.rs 实测表）：
// system 必须以 OpenClaw 身份句开头且带 ## Tooling 段，否则 406 空响应体。
// 这正是「真 token 仍然恒 406」的根因——客户端发的通用提示词命中"中性句"分支。
func TestNormalizePromptPrependsIdentity(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"system","content":"You are a helpful assistant."},
		{"role":"user","content":"hi"}]}`
	out := NormalizePrompt([]byte(body))

	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("出站体不是合法 JSON: %v", err)
	}
	if len(doc.Messages) != 2 {
		t.Fatalf("消息数 = %d", len(doc.Messages))
	}
	sys := doc.Messages[0].Content
	if !strings.HasPrefix(sys, IdentityLine) {
		t.Errorf("system 未以身份句开头: %q", sys[:min(60, len(sys))])
	}
	if !strings.Contains(sys, "## Tooling") {
		t.Error("缺 ## Tooling 段（只有身份句会 403 pay-view）")
	}
	// 客户端提示词必须逐字保留在后面（用户明确要求保留）
	if !strings.Contains(sys, "You are a helpful assistant.") {
		t.Errorf("客户端提示词被丢了: %q", sys)
	}
	// user 消息不能被动
	if doc.Messages[1].Content != "hi" {
		t.Errorf("user 消息被改了: %q", doc.Messages[1].Content)
	}
}

// 完全没有 system → 406。要在最前面**插**一条。
func TestNormalizePromptInsertsSystemWhenMissing(t *testing.T) {
	out := NormalizePrompt([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(doc.Messages) != 2 || doc.Messages[0].Role != "system" {
		t.Fatalf("应在最前插入 system: %+v", doc.Messages)
	}
	if !strings.HasPrefix(doc.Messages[0].Content, IdentityLine) {
		t.Errorf("插入的 system 缺身份句: %q", doc.Messages[0].Content)
	}
}

// 幂等：官方客户端的请求本身就带身份句，重复前置会把提示词写两遍
// （降级/同家重试路径上必然发生）。
func TestNormalizePromptIsIdempotent(t *testing.T) {
	once := NormalizePrompt([]byte(`{"messages":[{"role":"system","content":"You are a helpful assistant."}]}`))
	twice := NormalizePrompt(once)
	if strings.Count(string(twice), IdentityLine) != 1 {
		t.Errorf("身份句出现了一次以上（不幂等）: %d", strings.Count(string(twice), IdentityLine))
	}
}

// 外来身份句改写：字面匹配、忽略大小写、长的先命中。
func TestNormalizePromptRewritesForeignIdentities(t *testing.T) {
	cases := []struct{ in, notWant string }{
		{"You are ZCode, an interactive coding agent", "ZCode"},
		{"You are Claude Code, Anthropic's official CLI tool for Claude.", "Claude Code"},
		{"you are codex", "Codex"}, // 大小写不敏感
		{"You are an AI agent powered by DeepSeek Harness.", "DeepSeek Harness"},
	}
	for _, c := range cases {
		body := `{"messages":[{"role":"system","content":` + quote(c.in) + `}]}`
		out := string(NormalizePrompt([]byte(body)))
		if strings.Contains(out, c.notWant) {
			t.Errorf("外来身份句没被改写干净（仍含 %q）: %s", c.notWant, out)
		}
	}
}

// 形态怪异时不失败（panic=abort 纪律：尽力产出能发出去的 body）。
func TestNormalizePromptToleratesWeirdShapes(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"messages":null}`,
		`{"messages":"not-an-array"}`,
		`{"messages":[{"role":"system","content":null}]}`,
		`not-json`,
	} {
		if out := NormalizePrompt([]byte(body)); out == nil {
			t.Errorf("%s: 返回 nil", body)
		}
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
