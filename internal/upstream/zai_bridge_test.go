package upstream

import (
	"encoding/json"
	"testing"
)

// TestZaiTranslateInMaxTokens 锁定 max_tokens 翻译：客户端没给时必须落到上限（缺省=不限，
// 让模型自然 stop），而不是钳到 1 导致只出 1 token 的空正文；显式给了则夹进合法区间。
func TestZaiTranslateInMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"omitted_defaults_to_ceiling", `{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`, ZaiMaxTokensLimit},
		{"explicit_small_respected", `{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`, 256},
		{"explicit_oversize_clamped", `{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}],"max_tokens":999999}`, ZaiMaxTokensLimit},
		{"zero_treated_as_omitted", `{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`, ZaiMaxTokensLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			anth, _, err := ZaiTranslateIn([]byte(c.body))
			if err != nil {
				t.Fatalf("翻译失败: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(anth, &got); err != nil {
				t.Fatalf("反序列化 Anthropic 体失败: %v", err)
			}
			mt, ok := got["max_tokens"]
			if !ok {
				t.Fatal("Anthropic 体缺 max_tokens（该字段为必填）")
			}
			// JSON 数字反序列化为 float64
			if int(mt.(float64)) != c.want {
				t.Fatalf("max_tokens = %v, want %d", mt, c.want)
			}
		})
	}
}
