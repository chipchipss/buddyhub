package server

// responses_test.go — Responses API 转换层测试：
//   - Responses → Chat 请求转换（input 字符串/数组、instructions、tools、tool_choice、reasoning）
//   - Chat → Responses 响应转换（content/reasoning/tool_calls/usage）
//   - SSE 聚合器（delta 拼接、tool_calls 归并、finish_reason 缺失拒绝）
//   - 路由注册（/v1/responses 与 /responses 都挂上鉴权）
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesToChatRequestStringInput(t *testing.T) {
	respReq := map[string]any{
		"model":             "deepseek-v4.1-flash",
		"input":             "你好",
		"temperature":       0.5,
		"max_output_tokens": float64(1024),
	}
	chat, err := responsesToChatRequest(respReq, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "你好" {
		t.Errorf("message = %v, want user/你好", m0)
	}
	if chat["temperature"] != 0.5 {
		t.Errorf("temperature = %v", chat["temperature"])
	}
	if chat["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v（max_output_tokens 应映射为 max_tokens）", chat["max_tokens"])
	}
}

func TestResponsesToChatRequestAssistantMerge(t *testing.T) {
	// DSH 拆分的 reasoning + message + function_call 三项应合回一条 assistant。
	respReq := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"role": "user", "content": "查天气"},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "需要调用工具"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": ""},
			}},
			map[string]any{"type": "function_call", "call_id": "call_abc", "name": "get_weather", "arguments": `{"city":"北京"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_abc", "output": `{"temp":25}`},
		},
	}
	chat, err := responsesToChatRequest(respReq, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	msgs, _ := chat["messages"].([]any)
	// 期望: user, assistant(带 reasoning_content + tool_calls), tool
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3（user + 合并 assistant + tool）: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msgs[1].role = %v", asst["role"])
	}
	if rc, _ := asst["reasoning_content"].(string); rc != "需要调用工具" {
		t.Errorf("reasoning_content = %q, want 需要调用工具", rc)
	}
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_abc" {
		t.Errorf("tool message = %v", toolMsg)
	}
}

func TestResponsesToChatRequestToolsAndChoice(t *testing.T) {
	respReq := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "function", "name": "f1", "description": "d1", "parameters": map[string]any{}},
			map[string]any{"type": "web_search"}, // 非 function 工具应被忽略
		},
		"tool_choice": "auto",
		"reasoning":   map[string]any{"effort": "high"},
	}
	chat, err := responsesToChatRequest(respReq, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1（web_search 被过滤）", len(tools))
	}
	if chat["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", chat["tool_choice"])
	}
	if chat["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", chat["reasoning_effort"])
	}
}

func TestChatCompletionToResponsesFull(t *testing.T) {
	chat := map[string]any{
		"id":      "chatcmpl-abc",
		"created": float64(1753600000),
		"model":   "deepseek-v4.1-flash",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":              "assistant",
				"content":           "今天晴",
				"reasoning_content": "推理过程",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(15),
			"prompt_tokens_details":     map[string]any{"cached_tokens": float64(3)},
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(2)},
		},
	}
	raw, _ := json.Marshal(chat)
	out, err := chatCompletionToResponses(raw, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	var resp map[string]any
	if json.Unmarshal(out, &resp) != nil {
		t.Fatalf("output not json")
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("envelope = %v/%v", resp["object"], resp["status"])
	}
	if !strings.HasPrefix(resp["id"].(string), "resp_") {
		t.Errorf("id = %v", resp["id"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 2 { // reasoning + message
		t.Fatalf("output = %d items, want 2", len(output))
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("output[0] = %v, want reasoning", output[0])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Errorf("output[1] = %v, want message", output[1])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
		t.Errorf("usage = %v", usage)
	}
	if cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"]; cached != float64(3) {
		t.Errorf("cached_tokens = %v", cached)
	}
}

func TestAggregateSSEBytes(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-x","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"想一下"}}]}`,
		`data: {"choices":[{"delta":{"content":"答案"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	out, err := aggregateSSEBytes([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "答案" {
		t.Errorf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "想一下" {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Errorf("arguments = %v（分片参数应合并）", fn["arguments"])
	}
	if resp["usage"] == nil {
		t.Errorf("usage 应透传")
	}
}

func TestAggregateSSEBytesNoFinishReason(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"half"}}]}` + "\n"
	if _, err := aggregateSSEBytes([]byte(sse)); err == nil {
		t.Fatal("无 finish_reason 应报错（拒绝伪造成完整回复）")
	}
}

func TestResponsesRoutesRegistered(t *testing.T) {
	h := NewHandler(Config{})
	for _, path := range []string{"/v1/responses", "/responses"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// 空配置（无 api_key）时 withAuth 放行，请求进入转换层后因 body 为空被 400 拒绝。
		// 404 = 路由未注册；401 = 鉴权异常（空 key 应放行）；5xx = 管线崩溃。
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s 路由未注册", path)
		}
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s 空 api_key 应放行，got 401", path)
		}
		if rec.Code >= 500 {
			t.Errorf("%s 管线异常: %d", path, rec.Code)
		}
	}
}
