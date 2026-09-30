package accio

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

/* ── 脚手架 ──────────────────────────────────────────────────── */

type rewriteTransport struct {
	target *httptest.Server
	reqs   *[]*http.Request
	bodies *[]string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.target.URL, "http://")
	if t.reqs != nil {
		*t.reqs = append(*t.reqs, req)
	}
	if t.bodies != nil {
		var raw []byte
		if req.Body != nil {
			raw, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		*t.bodies = append(*t.bodies, string(raw))
	}
	return http.DefaultTransport.RoundTrip(req)
}

type recorder struct {
	reqs   []*http.Request
	bodies []string
}

func withMock(t *testing.T, h http.HandlerFunc) *recorder {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	rec := &recorder{}
	old := httpClient
	SetHTTPClient(&http.Client{Transport: &rewriteTransport{target: srv, reqs: &rec.reqs, bodies: &rec.bodies}})
	t.Cleanup(func() { httpClient = old })
	return rec
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func build(t *testing.T, src string) map[string]any {
	t.Helper()
	raw, err := BuildUpstreamBody([]byte(src), "", "tok", "req-1", "dev-1")
	if err != nil {
		t.Fatalf("BuildUpstreamBody: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("出站体不是对象: %v", err)
	}
	return out
}

/* ── 三处形态差异（照 OpenAI 直发必失败） ───────────────────── */

// tools[].parameters_json 必须是 **JSON 字符串**（不是对象）。
func TestToolsParametersJSONIsString(t *testing.T) {
	out := build(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`)

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数量 = %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	params, ok := tool["parameters_json"].(string)
	if !ok {
		t.Fatalf("parameters_json 必须是字符串（上游 protobuf-JSON 形态），得到 %T", tool["parameters_json"])
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(params), &parsed) != nil || parsed["type"] != "object" {
		t.Fatalf("parameters_json 内容不对: %s", params)
	}
	if tool["name"] != "f" || tool["description"] != "d" {
		t.Fatalf("工具名/描述丢失: %v", tool)
	}
}

// tool_config 也必须是字符串。
func TestToolConfigIsString(t *testing.T) {
	out := build(t, `{"model":"m","messages":[]}`)
	cfg, ok := out["tool_config"].(string)
	if !ok {
		t.Fatalf("tool_config 必须是字符串，得到 %T", out["tool_config"])
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(cfg), &parsed) != nil {
		t.Fatalf("tool_config 不是合法 JSON: %s", cfg)
	}
	if _, ok := parsed["functionCallingConfig"]; !ok {
		t.Fatalf("tool_config 缺 functionCallingConfig: %s", cfg)
	}
}

// assistant 的 tool_calls 拆成 parts[].function_call，args_json 是字符串。
func TestAssistantToolCallsBecomeParts(t *testing.T) {
	out := build(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
		{"role":"tool","tool_call_id":"c1","name":"f","content":"result"}]}`)

	contents, _ := out["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents 数量 = %d，期望 3（user/model/tool）", len(contents))
	}
	modelTurn := contents[1].(map[string]any)
	if modelTurn["role"] != "model" {
		t.Fatalf("assistant 应映射成 model 角色，得到 %v", modelTurn["role"])
	}
	parts, _ := modelTurn["parts"].([]any)
	fc, ok := parts[0].(map[string]any)["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("缺 function_call part: %v", parts[0])
	}
	if fc["name"] != "f" || fc["id"] != "c1" {
		t.Fatalf("function_call 内容错误: %v", fc)
	}
	if _, ok := fc["args_json"].(string); !ok {
		t.Fatalf("args_json 必须是字符串，得到 %T", fc["args_json"])
	}

	// tool 结果包成 function_response，response_json 是字符串
	toolTurn := contents[2].(map[string]any)
	if toolTurn["role"] != "tool" {
		t.Fatalf("tool 角色错误: %v", toolTurn["role"])
	}
	tparts, _ := toolTurn["parts"].([]any)
	fr, ok := tparts[0].(map[string]any)["function_response"].(map[string]any)
	if !ok {
		t.Fatalf("缺 function_response part: %v", tparts[0])
	}
	respJSON, ok := fr["response_json"].(string)
	if !ok {
		t.Fatalf("response_json 必须是字符串，得到 %T", fr["response_json"])
	}
	var inner map[string]any
	if json.Unmarshal([]byte(respJSON), &inner) != nil {
		t.Fatalf("response_json 不是合法 JSON: %s", respJSON)
	}
	if inner["content"] != "result" {
		t.Fatalf("工具结果内容丢失: %v", inner)
	}
	if inner["is_error"] != false {
		t.Fatalf("is_error 应为 false: %v", inner)
	}
}

/* ── 其它结构映射 ───────────────────────────────────────────── */

// system_instruction 是**一个字符串**（不是对象），多条 system 拼接。
func TestSystemInstructionIsJoinedString(t *testing.T) {
	out := build(t, `{"model":"m","messages":[
		{"role":"system","content":"first"},
		{"role":"developer","content":"second"},
		{"role":"user","content":"hi"}]}`)

	si, ok := out["system_instruction"].(string)
	if !ok {
		t.Fatalf("system_instruction 必须是字符串，得到 %T", out["system_instruction"])
	}
	if !strings.Contains(si, "first") || !strings.Contains(si, "second") {
		t.Fatalf("多条 system 应拼接: %q", si)
	}
	contents, _ := out["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("system/developer 不该出现在 contents 里，得到 %d 条", len(contents))
	}
}

func TestEnvelopeCoreFields(t *testing.T) {
	out := build(t, `{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"hi"}],
		"temperature":0.7,"max_tokens":1024,"top_p":0.9}`)

	if out["tenant"] != Tenant || out["iai_tag"] != IaiTag {
		t.Fatalf("租户/标记错误: %v %v", out["tenant"], out["iai_tag"])
	}
	if out["token"] != "tok" || out["request_id"] != "req-1" {
		t.Fatalf("token/request_id 错误: %v %v", out["token"], out["request_id"])
	}
	if out["temperature"] != 0.7 || out["top_p"] != 0.9 {
		t.Fatalf("采样参数未透传: %v %v", out["temperature"], out["top_p"])
	}
	// OpenAI 的 max_tokens → ADK 的 max_output_tokens
	if out["max_output_tokens"] != float64(1024) {
		t.Fatalf("max_tokens 应映射成 max_output_tokens，得到 %v", out["max_output_tokens"])
	}
	if _, ok := out["max_tokens"]; ok {
		t.Fatal("不该同时带 max_tokens（上游只认 max_output_tokens）")
	}
}

func TestImageContentBecomesInlineData(t *testing.T) {
	out := build(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)

	contents, _ := out["contents"].([]any)
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts 数量 = %d，期望 2", len(parts))
	}
	inline, ok := parts[1].(map[string]any)["inline_data"].(map[string]any)
	if !ok {
		t.Fatalf("data URI 应转成 inline_data: %v", parts[1])
	}
	if inline["mime_type"] != "image/png" || inline["data"] != "AAAA" {
		t.Fatalf("inline_data 内容错误: %v", inline)
	}
}

func TestReasoningEffortSetsIncludeThoughts(t *testing.T) {
	out := build(t, `{"model":"m","messages":[],"reasoning_effort":"high"}`)
	if out["reasoning_effort"] != "high" || out["include_thoughts"] != true {
		t.Fatalf("思考档位应同时置 include_thoughts: %v %v", out["reasoning_effort"], out["include_thoughts"])
	}
	// 无意义档位等同不传
	out = build(t, `{"model":"m","messages":[],"reasoning_effort":"off"}`)
	if _, ok := out["reasoning_effort"]; ok {
		t.Fatal("off 不该出现在出站体")
	}
}

/* ── URL 签名 ────────────────────────────────────────────────── */

// sg_k 是 request_id 的 MD5（上游的请求签名参数）。
func TestGenerateContentURL(t *testing.T) {
	u := GenerateContentURL("https://phoenix-gw.alibaba.com/api/adk/llm", "req-abc")
	sum := md5.Sum([]byte("req-abc"))
	want := "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=" + hex.EncodeToString(sum[:])
	if u != want {
		t.Fatalf("URL = %s\n期望 %s", u, want)
	}
}

/* ── 响应侧 ──────────────────────────────────────────────────── */

func TestParseFrame(t *testing.T) {
	f := ParseFrame(`{"content":{"parts":[{"text":"hi"},{"text":"think","thought":true}]},"turnComplete":true,"usageMetadata":{"totalTokenCount":5}}`)
	if f == nil {
		t.Fatal("帧解析失败")
	}
	if !f.TurnComplete {
		t.Fatal("turnComplete 未识别")
	}
	if len(f.Parts) != 2 || f.Parts[0].Kind != "text" || f.Parts[1].Kind != "thought" {
		t.Fatalf("parts 解析错误: %+v", f.Parts)
	}
	if len(f.Usage) == 0 {
		t.Fatal("usageMetadata 丢失")
	}
	// 空 / [DONE] 返回 nil
	if ParseFrame("") != nil || ParseFrame("[DONE]") != nil {
		t.Fatal("空帧与 [DONE] 应返回 nil")
	}
	// snake_case 形态也要认
	f = ParseFrame(`{"turn_complete":true,"finish_reason":"STOP"}`)
	if !f.TurnComplete || f.FinishReason != "STOP" {
		t.Fatalf("snake_case 形态未识别: %+v", f)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"STOP": "stop", "MAX_TOKENS": "length", "TOOL_CALLS": "tool_calls",
		"SAFETY": "content_filter", "": "stop", "SOMETHING_NEW": "stop",
	}
	for in, want := range cases {
		if got := FinishReason(in); got != want {
			t.Errorf("FinishReason(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestTranslatorEmitsChunks(t *testing.T) {
	tr := NewTranslator("id-1", 1, "m")
	out := tr.Translate(ParseFrame(`{"content":{"parts":[{"text":"hello"}]}}`))
	if !strings.Contains(out, `"content":"hello"`) {
		t.Fatalf("文本片段未产出: %s", out)
	}
	// 思维链走 reasoning_content
	out = tr.Translate(ParseFrame(`{"content":{"parts":[{"text":"hmm","thought":true}]}}`))
	if !strings.Contains(out, `"reasoning_content":"hmm"`) {
		t.Fatalf("思维链未映射到 reasoning_content: %s", out)
	}
	// 结束帧 + [DONE]
	out = tr.Translate(ParseFrame(`{"turnComplete":true,"finishReason":"STOP"}`))
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("结束帧不对: %s", out)
	}
	if extra := tr.Finish(); extra != "" {
		t.Fatalf("已结束不该重复收尾: %s", extra)
	}
}

// 上游没发 turnComplete 也要补 [DONE]，否则客户端永远等最后一片。
func TestTranslatorFinishFallback(t *testing.T) {
	tr := NewTranslator("id-1", 1, "m")
	tr.Translate(ParseFrame(`{"content":{"parts":[{"text":"partial"}]}}`))
	out := tr.Finish()
	if !strings.Contains(out, "[DONE]") || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("收尾不对: %s", out)
	}
}

// 业务错误藏在 200 的流里。
func TestTranslatorErrorFrame(t *testing.T) {
	tr := NewTranslator("id-1", 1, "m")
	out := tr.Translate(ParseFrame(`{"errorCode":"402","errorMessage":"quota exceeded"}`))
	if !strings.Contains(out, "quota exceeded") {
		t.Fatalf("错误原文未带出: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("错误后也要补 [DONE]: %s", out)
	}
}

func TestAggregate(t *testing.T) {
	body := "data: {\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}\n\n" +
		"data: {\"content\":{\"parts\":[{\"text\":\" world\"}]}}\n\n" +
		"data: {\"content\":{\"parts\":[{\"text\":\"think\",\"thought\":true}]}}\n\n" +
		"data: {\"turnComplete\":true,\"finishReason\":\"STOP\",\"usageMetadata\":{\"totalTokenCount\":7}}\n\n" +
		"data: [DONE]\n\n"

	raw, err := Aggregate(strings.NewReader(body), "id-1", 1, "m")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var doc struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("聚合结果解析失败: %v", err)
	}
	if doc.Object != "chat.completion" || len(doc.Choices) != 1 {
		t.Fatalf("聚合形状不对: %s", raw)
	}
	if doc.Choices[0].Message.Content != "Hello world" {
		t.Fatalf("正文拼接错误: %q", doc.Choices[0].Message.Content)
	}
	if doc.Choices[0].Message.Reasoning != "think" {
		t.Fatalf("思维链丢失: %q", doc.Choices[0].Message.Reasoning)
	}
	if doc.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", doc.Choices[0].FinishReason)
	}
	if doc.Usage["totalTokenCount"] != float64(7) {
		t.Fatalf("usage 丢失: %v", doc.Usage)
	}
}

/* ── 登录 ────────────────────────────────────────────────────── */

func TestNewLoginBuildsAuthorizeURL(t *testing.T) {
	c, ln, err := NewLogin(RegionCN)
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	defer ln.Close()

	if c.Port <= 0 {
		t.Fatal("未绑定端口")
	}
	for _, want := range []string{
		"https://www.accio-ai.com/login", "return_url=", "state=",
		"code_challenge=", "code_challenge_method=S256", "client_id=" + ClientID,
	} {
		if !strings.Contains(c.AuthURL, want) {
			t.Errorf("授权链接缺少 %s：%s", want, c.AuthURL)
		}
	}
	// 回调地址要嵌进 return_url
	if !strings.Contains(c.AuthURL, url.QueryEscape(c.CallbackURL)) {
		t.Fatalf("return_url 未带回调地址: %s", c.AuthURL)
	}

	// 国际版走另一个站点
	c2, ln2, err := NewLogin(RegionGlobal)
	if err != nil {
		t.Fatalf("NewLogin(global): %v", err)
	}
	defer ln2.Close()
	if !strings.Contains(c2.AuthURL, "https://www.accio.com/login") {
		t.Fatalf("国际版站点不对: %s", c2.AuthURL)
	}
}

func TestParseCallbackVariants(t *testing.T) {
	for _, q := range []string{"code=A", "auth_code=B", "authCode=C"} {
		v, _ := url.ParseQuery(q)
		if cb := ParseCallback(v); cb.Code == "" {
			t.Errorf("%s 未解析出 code", q)
		}
	}
	v, _ := url.ParseQuery("error=access_denied")
	if cb := ParseCallback(v); cb.Error != "access_denied" {
		t.Error("错误参数未解析")
	}
}

func TestCompleteExchangesCode(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "userinfo") {
			writeJSON(w, map[string]any{"data": map[string]any{"id": "u-1", "email": "a@b.c"}})
			return
		}
		writeJSON(w, map[string]any{"accessToken": "at-1", "refreshToken": "rt-1", "expiresAt": float64(1893456000000)})
	})
	c := &LoginContext{Region: RegionCN, State: "st-1", CodeVerifier: "v", CallbackURL: "http://127.0.0.1:1/auth/callback-accio", DeviceID: "d-1"}
	cred, err := Complete(context.Background(), c, &Callback{Code: "code-1", State: "st-1"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if cred.AccessToken != "at-1" || cred.RefreshToken != "rt-1" {
		t.Fatalf("凭据解析错误: %+v", cred)
	}
	if cred.Region != RegionCN || cred.DeviceID != "d-1" {
		t.Fatalf("地区/设备未写入: %+v", cred)
	}
	if cred.UserID != "u-1" || cred.Email != "a@b.c" {
		t.Fatalf("用户信息未补齐: %+v", cred)
	}

	var body map[string]string
	_ = json.Unmarshal([]byte(rec.bodies[0]), &body)
	// redirect_uri 必须与授权时逐字相同
	if body["redirectUri"] != c.CallbackURL || body["codeVerifier"] != "v" || body["clientId"] != ClientID {
		t.Fatalf("换证请求体错误: %v", body)
	}
}

func TestCompleteRejectsStateMismatch(t *testing.T) {
	c := &LoginContext{State: "expected"}
	if _, err := Complete(context.Background(), c, &Callback{Code: "x", State: "other"}); err == nil {
		t.Fatal("state 不匹配应报错")
	}
	if _, err := Complete(context.Background(), c, &Callback{Error: "denied"}); err == nil {
		t.Fatal("带 error 的回调应报错")
	}
}

func TestAwaitCallbackReceivesRedirect(t *testing.T) {
	c, ln, err := NewLogin(RegionCN)
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	defer ln.Close()

	done := make(chan *Callback, 1)
	go func() {
		cb, _ := AwaitCallback(context.Background(), ln, 5*time.Second)
		done <- cb
	}()
	resp, err := http.Get(c.CallbackURL + "?code=C1&state=st")
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	resp.Body.Close()

	select {
	case cb := <-done:
		if cb == nil || cb.Code != "C1" {
			t.Fatalf("回调解析错误: %+v", cb)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待回调超时")
	}
}

/* ── 续期与代理 ──────────────────────────────────────────────── */

func TestRefresh(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"accessToken": "new", "refreshToken": "rt2"})
	})
	old := &Credential{AccessToken: "old", RefreshToken: "rt1", Region: RegionCN, DeviceID: "d"}
	fresh, err := Refresh(context.Background(), old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.AccessToken != "new" || fresh.RefreshToken != "rt2" {
		t.Fatalf("令牌未更新: %+v", fresh)
	}
	if fresh.DeviceID != "d" || fresh.Region != RegionCN {
		t.Fatalf("续期丢失了字段: %+v", fresh)
	}
	if old.AccessToken != "old" {
		t.Fatal("Refresh 不应修改入参")
	}
}

func TestRefreshWithoutToken(t *testing.T) {
	if _, err := Refresh(context.Background(), &Credential{}); err == nil {
		t.Fatal("缺 refresh_token 应报错")
	}
}

func TestSetProxy(t *testing.T) {
	prev := httpClient
	t.Cleanup(func() { httpClient = prev })
	for _, bad := range []string{"://nope", "127.0.0.1:2080"} {
		if err := SetProxy(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if err := SetProxy("http://127.0.0.1:2080"); err != nil {
		t.Fatalf("合法代理被拒: %v", err)
	}
	if httpClient.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("代理未生效")
	}
}
