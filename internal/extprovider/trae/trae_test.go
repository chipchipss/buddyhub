package trae

import (
	"context"
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

func prep(t *testing.T, src string, model string) map[string]any {
	t.Helper()
	raw, err := PrepareBody([]byte(src), model)
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("出站体不是对象: %v", err)
	}
	return out
}

/* ── SOLO 四处变形（照 OpenAI 直发必 4001） ─────────────────── */

func TestPayloadToolsParametersBecomeJSONString(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`, "")

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数量 = %d", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 必须是 JSON 字符串（OpenAI 是对象，直发上游 4001），得到 %T", fn["parameters"])
	}
	// 字符串内容必须仍是合法 JSON 且保留结构
	var parsed map[string]any
	if json.Unmarshal([]byte(params), &parsed) != nil || parsed["type"] != "object" {
		t.Fatalf("parameters 字符串内容不对: %s", params)
	}
}

func TestPayloadToolChoiceBecomesBareString(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"required"}`, "required"},
		{`{"type":"function","function":{"name":"do_thing"}}`, "do_thing"},
		{`{"type":"function","name":"direct"}`, "direct"},
		{`{"type":"function"}`, "auto"}, // 没名字兜 auto
		{`"auto"`, "auto"},
	}
	for _, c := range cases {
		out := prep(t, `{"model":"m","messages":[],"tool_choice":`+c.in+`,"tools":[{"type":"function","function":{"name":"x"}}]}`, "")
		got, present := out["tool_choice"]
		if !present {
			t.Fatalf("%s: tool_choice 被丢了", c.in)
		}
		if got != c.want {
			t.Errorf("%s → %v，期望 %v（上游要裸字符串）", c.in, got, c.want)
		}
	}
}

// tool_choice=none 时必须**连 tools 一起删**（只删 choice 上游仍会尝试调工具）。
func TestPayloadToolChoiceNoneSuppressesTools(t *testing.T) {
	for _, in := range []string{`"none"`, `{"type":"none"}`} {
		out := prep(t, `{"model":"m","messages":[],"tool_choice":`+in+`,"tools":[{"type":"function","function":{"name":"x"}}]}`, "")
		if _, ok := out["tool_choice"]; ok {
			t.Errorf("%s: tool_choice 应被删除", in)
		}
		if _, ok := out["tools"]; ok {
			t.Errorf("%s: tools 也必须一起删（否则上游仍会调工具）", in)
		}
	}
}

func TestPayloadAssistantToolCallsRenamed(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"r"}]}`, "")

	msgs, _ := out["messages"].([]any)
	asst := msgs[0].(map[string]any)
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量 = %d", len(calls))
	}
	call := calls[0].(map[string]any)
	if _, ok := call["function"]; ok {
		t.Error("function 必须改名为 function_call（上游只认后者）")
	}
	if _, ok := call["function_call"]; !ok {
		t.Error("缺 function_call")
	}
}

func TestPayloadDeveloperRoleBecomesSystem(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[{"role":"developer","content":"be nice"},{"role":"user","content":"hi"}]}`, "")
	msgs, _ := out["messages"].([]any)
	role := msgs[0].(map[string]any)["role"]
	if role != "system" {
		t.Fatalf("developer 应降成 system（上游不认，实测静默空流），得到 %v", role)
	}
}

// 没有名字的 tool_call 上游不认；全被剔空的 assistant 占位消息要置 null。
func TestPayloadDropsNamelessToolCalls(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"arguments":"{}"}}]},
		{"role":"user","content":"hi"}]}`, "")
	msgs, _ := out["messages"].([]any)
	if msgs[0] != nil {
		t.Fatalf("无名字调用的 assistant 应置 null，得到 %v", msgs[0])
	}
}

/* ── 白名单与默认值 ─────────────────────────────────────────── */

// 多带字段**不是被忽略、是被拒**——白名单之外的键必须一个不留。
func TestPayloadDropsNonWhitelistedKeys(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[],"agent_type":"x","device_id":"y","ide_version":"z",
		"thinking":{"type":"enabled"},"stream_options":{"include_usage":true},"response_format":{"type":"json_object"}}`, "")

	for _, k := range []string{"agent_type", "device_id", "ide_version", "thinking", "stream_options", "response_format"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s 不在白名单里，必须丢弃（上游会拒）", k)
		}
	}
	// 白名单内的必须都在
	for _, k := range []string{"messages", "function", "stream", "config_name", "model", "max_tokens"} {
		if _, ok := out[k]; !ok {
			t.Errorf("缺少白名单键 %s", k)
		}
	}
}

func TestPayloadDefaultsAndSampling(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[],"temperature":0.5,"seed":7,"stop":"END","n":"2"}`, "")
	if out["function"] != soloFunction {
		t.Fatalf("function = %v，期望 %s", out["function"], soloFunction)
	}
	if out["stream"] != true {
		t.Fatal("出站必须强制 stream:true（上游只支持流式）")
	}
	if out["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("缺省 max_tokens = %v，期望 %d", out["max_tokens"], defaultMaxTokens)
	}
	if out["temperature"] != 0.5 || out["seed"] != float64(7) {
		t.Fatalf("采样参数未透传: %v %v", out["temperature"], out["seed"])
	}
	// n 是字符串不是数值 → 不搬
	if _, ok := out["n"]; ok {
		t.Error("非数值的采样参数不该搬（上游只认数值）")
	}
	if out["stop"] != "END" {
		t.Fatalf("stop 字符串应透传，得到 %v", out["stop"])
	}
}

func TestPayloadStopOnlyStringOrArray(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[],"stop":{"bad":true}}`, "")
	if _, ok := out["stop"]; ok {
		t.Fatal("stop 非字符串/数组时不该搬")
	}
	out = prep(t, `{"model":"m","messages":[],"stop":["A","B"]}`, "")
	if _, ok := out["stop"].([]any); !ok {
		t.Fatalf("stop 数组应透传，得到 %T", out["stop"])
	}
}

func TestPayloadReasoningEffortFiltered(t *testing.T) {
	for _, v := range []string{"", "auto", "none", "off"} {
		out := prep(t, `{"model":"m","messages":[],"reasoning_effort":"`+v+`"}`, "")
		if _, ok := out["reasoning_effort"]; ok {
			t.Errorf("%q 等同不传，不该出现在出站体", v)
		}
	}
	out := prep(t, `{"model":"m","messages":[],"reasoning_effort":"high"}`, "")
	if out["reasoning_effort"] != "high" {
		t.Fatalf("有效档位应透传，得到 %v", out["reasoning_effort"])
	}
}

func TestPayloadModelResolution(t *testing.T) {
	// resolvedModel 优先于请求体里的 model
	out := prep(t, `{"model":"request-body","messages":[]}`, "resolved-name")
	if out["model"] != "resolved-name" || out["config_name"] != "resolved-name" {
		t.Fatalf("model/config_name = %v/%v", out["model"], out["config_name"])
	}
	// 展示后缀要剥掉（上游不认 -solo/-intl）
	out = prep(t, `{"model":"glm-5.2-solo","messages":[]}`, "")
	if out["model"] != "glm-5.2" {
		t.Fatalf("应剥掉 -solo 后缀，得到 %v", out["model"])
	}
	// 空模型兜底，而不是发空 config_name
	out = prep(t, `{"messages":[]}`, "")
	if out["config_name"] != DefaultConfigName {
		t.Fatalf("空模型应兜底 %s，得到 %v", DefaultConfigName, out["config_name"])
	}
}

// 客户端修剪历史留下的悬空 tool 结果：上游可能空流而不是报错，宁可先清掉。
func TestPayloadDropsOrphanToolResults(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"tool","tool_call_id":"ghost","content":"dangling"}]}`, "")
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("悬空 tool 结果应被丢掉，剩 %d 条", len(msgs))
	}
}

func TestPayloadContentStringBecomesBlocks(t *testing.T) {
	out := prep(t, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`, "")
	msgs, _ := out["messages"].([]any)
	content := msgs[0].(map[string]any)["content"]
	blocks, ok := content.([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content 应转成块数组，得到 %T", content)
	}
	b := blocks[0].(map[string]any)
	if b["type"] != "text" || b["text"] != "hello" {
		t.Fatalf("块内容不对: %v", b)
	}
}

/* ── 请求头 ──────────────────────────────────────────────────── */

// 同一个 token 要出现在三个头里；空身份头不能发（发空值会被当成另一个身份）。
func TestSoloHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://x/", nil)
	applySoloHeaders(req, &Credential{AccessToken: "tok", UID: "u1", MachineID: "m1", DeviceID: "d1"}, true)

	if got := req.Header.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
		t.Fatalf("Authorization = %q（要带 Cloud-IDE-JWT 前缀）", got)
	}
	if req.Header.Get("X-Cloudide-Token") != "tok" || req.Header.Get("X-Ide-Token") != "tok" {
		t.Fatal("token 必须同时出现在 X-Cloudide-Token 与 X-Ide-Token（裸串）")
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("流式 Accept = %q", req.Header.Get("Accept"))
	}
	if req.Header.Get("X-Uid") != "u1" || req.Header.Get("X-Machine-Id") != "m1" || req.Header.Get("X-Device-Id") != "d1" {
		t.Fatal("身份头缺失")
	}

	// 空身份：这几个头必须**完全不发**
	req2, _ := http.NewRequest("POST", "https://x/", nil)
	applySoloHeaders(req2, &Credential{AccessToken: "tok"}, false)
	for _, h := range []string{"X-Uid", "X-Machine-Id", "X-Device-Id"} {
		if _, ok := req2.Header[h]; ok {
			t.Errorf("%s 为空时不该发（上游会当成另一个身份）", h)
		}
	}
	if req2.Header.Get("Accept") != "application/json" {
		t.Fatalf("非流式 Accept = %q", req2.Header.Get("Accept"))
	}
}

/* ── 流翻译 ──────────────────────────────────────────────────── */

func TestParseSoloEvent(t *testing.T) {
	ev := ParseSoloEvent("output", `{"response":"hi","reasoning_content":"think"}`)
	if ev.Kind != "output" || ev.Response != "hi" || ev.Reason != "think" {
		t.Fatalf("output 解析错误: %+v", ev)
	}
	// 上游会发一条**不带 data 的 done**
	if ev := ParseSoloEvent("done", ""); ev.Kind != "done" {
		t.Fatalf("空 data 的 done 应识别为 done: %+v", ev)
	}
	if ev := ParseSoloEvent("token_usage", `{"prompt_tokens":1}`); ev.Kind != "token_usage" {
		t.Fatalf("token_usage 解析错误: %+v", ev)
	}
	ev = ParseSoloEvent("error", `{"code":"4001","message":"bad model"}`)
	if ev.Kind != "error" || ev.Code != "4001" || ev.Message != "bad model" {
		t.Fatalf("error 解析错误: %+v", ev)
	}
}

func TestTranslatorEmitsChunksAndDone(t *testing.T) {
	tr := NewSoloTranslator("id-1", 123, "glm-5.2")

	frames := tr.Translate(ParseSoloEvent("output", `{"response":"hello"}`))
	if len(frames) != 1 || !strings.Contains(string(frames[0]), `"content":"hello"`) {
		t.Fatalf("output 应产出一帧 chunk: %s", frames)
	}
	if !strings.HasPrefix(string(frames[0]), "data: ") {
		t.Fatalf("帧格式不对: %s", frames[0])
	}

	// 上游不发 [DONE]，由 done 事件补
	frames = tr.Translate(ParseSoloEvent("done", `{"finish_reason":"stop"}`))
	if len(frames) != 2 {
		t.Fatalf("done 应产出 finish 帧 + [DONE]，得到 %d 帧", len(frames))
	}
	if !strings.Contains(string(frames[0]), `"finish_reason":"stop"`) {
		t.Fatalf("finish 帧不对: %s", frames[0])
	}
	if string(frames[1]) != "data: [DONE]\n\n" {
		t.Fatalf("[DONE] 帧不对: %q", frames[1])
	}
	// 已经 done 过，收尾不该再补
	if extra := tr.Finish(); len(extra) != 0 {
		t.Fatalf("已 done 不该重复收尾: %s", extra)
	}
}

// 上游中途断掉（没有 done）也必须补 [DONE]，否则客户端永远等最后一片。
func TestTranslatorFinishWithoutDone(t *testing.T) {
	tr := NewSoloTranslator("id-1", 123, "m")
	tr.Translate(ParseSoloEvent("output", `{"response":"partial"}`))
	frames := tr.Finish()
	if len(frames) != 2 {
		t.Fatalf("无 done 收尾应补 finish 帧 + [DONE]，得到 %d 帧", len(frames))
	}
	if string(frames[1]) != "data: [DONE]\n\n" {
		t.Fatalf("缺 [DONE]: %q", frames[1])
	}
}

// token_usage 是"欠着"的：读到它不发帧，挂在下一帧（finish 帧）一起走。
func TestTranslatorDefersUsage(t *testing.T) {
	tr := NewSoloTranslator("id-1", 123, "m")
	if frames := tr.Translate(ParseSoloEvent("token_usage", `{"total_tokens":42}`)); len(frames) != 0 {
		t.Fatalf("token_usage 不该立刻发帧，得到 %s", frames)
	}
	frames := tr.Translate(ParseSoloEvent("done", `{"finish_reason":"stop"}`))
	if len(frames) == 0 || !strings.Contains(string(frames[0]), `"usage"`) {
		t.Fatalf("usage 应挂在 finish 帧上，得到 %s", frames)
	}
}

func TestTranslatorErrorEvent(t *testing.T) {
	tr := NewSoloTranslator("id-1", 123, "m")
	frames := tr.Translate(ParseSoloEvent("error", `{"code":"4023","message":"model is unknown"}`))
	if len(frames) != 2 {
		t.Fatalf("error 应产出错误帧 + [DONE]，得到 %d 帧", len(frames))
	}
	if !strings.Contains(string(frames[0]), "model is unknown") {
		t.Fatalf("错误帧未带出上游原文: %s", frames[0])
	}
	if string(frames[1]) != "data: [DONE]\n\n" {
		t.Fatalf("错误后也要补 [DONE]: %q", frames[1])
	}
}

func TestScanSoloSSE(t *testing.T) {
	body := "event: output\ndata: {\"response\":\"a\"}\n\n" +
		"event: token_usage\ndata: {\"total_tokens\":1}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	var kinds []string
	if err := ScanSoloSSE(strings.NewReader(body), func(e, d string) { kinds = append(kinds, e) }); err != nil {
		t.Fatalf("ScanSoloSSE: %v", err)
	}
	want := []string{"output", "token_usage", "done"}
	if len(kinds) != len(want) {
		t.Fatalf("事件数 = %d（%v），期望 %d", len(kinds), kinds, len(want))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("第 %d 个事件 = %q，期望 %q", i, kinds[i], want[i])
		}
	}
}

func TestAggregateSolo(t *testing.T) {
	body := "event: output\ndata: {\"response\":\"Hello\"}\n\n" +
		"event: output\ndata: {\"response\":\" world\"}\n\n" +
		"event: output\ndata: {\"reasoning_content\":\"thinking\"}\n\n" +
		"event: token_usage\ndata: {\"total_tokens\":7}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	raw, err := AggregateSolo(strings.NewReader(body), "id-1", 1, "m")
	if err != nil {
		t.Fatalf("AggregateSolo: %v", err)
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
	if doc.Choices[0].Message.Reasoning != "thinking" {
		t.Fatalf("思维链丢失: %q", doc.Choices[0].Message.Reasoning)
	}
	if doc.Usage["total_tokens"] != float64(7) {
		t.Fatalf("usage 丢失: %v", doc.Usage)
	}
}

/* ── 登录 ────────────────────────────────────────────────────── */

func TestBuildAuthURL(t *testing.T) {
	c := &LoginContext{
		LoginTraceID: "trace-1", DeviceID: "dev-1", MachineID: "mac-1",
		CallbackURL: "http://127.0.0.1:54321/authorize",
	}
	u := buildAuthURL("https://api.trae.cn", c)
	for _, want := range []string{
		"login_version=1", "auth_from=solo", "login_channel=native_ide",
		"auth_type=local", "client_id=" + ClientIDSolo, "redirect=0",
		"login_trace_id=trace-1", "device_id=dev-1", "machine_id=mac-1",
		"x_device_brand=" + DeviceBrand,
	} {
		if !strings.Contains(u, want) {
			t.Errorf("授权链接缺少 %s：%s", want, u)
		}
	}
	// auth_callback_url **刻意不编码**：官方页面对它做正则匹配
	if !strings.Contains(u, "auth_callback_url=http://127.0.0.1:54321/authorize") {
		t.Fatalf("auth_callback_url 不该被编码: %s", u)
	}
}

func TestNewLoginBindsLoopback(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login_host": "https://api.trae.cn"})
	})
	c, ln, err := NewLogin(context.Background())
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	defer ln.Close()

	if c.Port <= 0 {
		t.Fatal("未绑定到端口")
	}
	if !strings.Contains(c.CallbackURL, "127.0.0.1:") || !strings.HasSuffix(c.CallbackURL, CallbackPath) {
		t.Fatalf("回调地址不对: %s", c.CallbackURL)
	}
	if c.CodeVerifier == "" || c.CodeChallenge == "" {
		t.Fatal("缺 PKCE 对")
	}
	// 设备公钥必须随登录一起生成（空值会被判设备绑定拒绝）
	if !strings.Contains(c.DevicePublicPEM, "BEGIN PUBLIC KEY") {
		t.Fatalf("设备公钥缺失: %q", c.DevicePublicPEM[:min(40, len(c.DevicePublicPEM))])
	}
	if !strings.Contains(c.DevicePrivatePEM, "BEGIN PRIVATE KEY") {
		t.Fatal("设备私钥缺失（要随凭据落盘）")
	}
	if len(rec.reqs) != 1 {
		t.Fatalf("应问一次登录引导，实际 %d 次", len(rec.reqs))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestParseCallbackFieldVariants(t *testing.T) {
	cases := []struct {
		q    string
		code string
		rt   string
	}{
		{"auth_code=A", "A", ""},
		{"authCode=B", "B", ""},
		{"AuthCode=C", "C", ""},
		{"code=D", "D", ""},
		{"refresh_token=R", "", "R"},
		{"refreshToken=S", "", "S"},
	}
	for _, c := range cases {
		v, _ := url.ParseQuery(c.q)
		cb := ParseCallback(v)
		if cb.AuthCode != c.code || cb.RefreshToken != c.rt {
			t.Errorf("%s → code=%q rt=%q，期望 %q/%q", c.q, cb.AuthCode, cb.RefreshToken, c.code, c.rt)
		}
	}
	v, _ := url.ParseQuery("error=access_denied")
	if cb := ParseCallback(v); cb.Error != "access_denied" {
		t.Errorf("错误参数未解析: %+v", cb)
	}
}

// 本机回调监听真的能收住浏览器那一次跳转。
func TestAwaitCallbackReceivesRedirect(t *testing.T) {
	c, ln, err := NewLogin(context.Background())
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	defer ln.Close()

	done := make(chan *Callback, 1)
	errCh := make(chan error, 1)
	go func() {
		cb, err := AwaitCallback(context.Background(), ln, 5*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		done <- cb
	}()

	// 模拟浏览器跳回来
	resp, err := http.Get(c.CallbackURL + "?auth_code=CODE-1&login_host=https://api.trae.cn")
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("回调页状态码 = %d（浏览器要看到成功页）", resp.StatusCode)
	}

	select {
	case cb := <-done:
		if cb.AuthCode != "CODE-1" || cb.LoginHost != "https://api.trae.cn" {
			t.Fatalf("回调解析错误: %+v", cb)
		}
	case err := <-errCh:
		t.Fatalf("AwaitCallback: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待回调超时")
	}
}

/* ── 换证与续期 ──────────────────────────────────────────────── */

func TestCompleteExchangesCode(t *testing.T) {
	rec := withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"access_token": "at-1", "refresh_token": "rt-1",
			"expires_at": float64(1893456000), "uid": "u-1",
		})
	})
	c := &LoginContext{
		CodeVerifier: "verifier", DeviceID: "d", MachineID: "m",
		DevicePublicPEM: "PUB", LoginHost: "https://api.trae.cn",
	}
	cred, err := Complete(context.Background(), c, &Callback{AuthCode: "code-1"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if cred.AccessToken != "at-1" || cred.RefreshToken != "rt-1" || cred.UID != "u-1" {
		t.Fatalf("凭据解析错误: %+v", cred)
	}
	// 秒级 expires_at 要换算成毫秒
	if cred.ExpiresAt != 1893456000000 {
		t.Fatalf("expires_at = %d，期望毫秒", cred.ExpiresAt)
	}
	// 设备私钥与身份必须随凭据走（参与设备绑定）
	if cred.DeviceID != "d" || cred.MachineID != "m" {
		t.Fatalf("设备身份丢失: %+v", cred)
	}

	var body map[string]any
	_ = json.Unmarshal([]byte(rec.bodies[0]), &body)
	if body["ClientID"] != ClientIDSolo || body["AuthCode"] != "code-1" || body["CodeVerifier"] != "verifier" {
		t.Fatalf("换证请求体错误: %v", body)
	}
	di, _ := body["DeviceInfo"].(map[string]any)
	if di["DevicePublicKey"] != "PUB" {
		t.Fatalf("DeviceInfo.DevicePublicKey 为空会被判设备绑定拒绝: %v", di)
	}
}

func TestCompleteRejectsError(t *testing.T) {
	c := &LoginContext{}
	if _, err := Complete(context.Background(), c, &Callback{Error: "access_denied"}); err == nil {
		t.Fatal("回调带 error 时应报错")
	}
	if _, err := Complete(context.Background(), c, &Callback{}); err == nil {
		t.Fatal("回调既无 code 也无 refresh_token 时应报错")
	}
}

func TestRefresh(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"accessToken": "new-at", "refreshToken": "new-rt", "expiresAt": float64(1893456000000)})
	})
	old := &Credential{AccessToken: "old", RefreshToken: "rt", DeviceID: "d", MachineID: "m", UID: "u"}
	fresh, err := Refresh(context.Background(), old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.AccessToken != "new-at" || fresh.RefreshToken != "new-rt" {
		t.Fatalf("令牌未更新: %+v", fresh)
	}
	if fresh.DeviceID != "d" || fresh.UID != "u" {
		t.Fatalf("续期丢失了设备身份: %+v", fresh)
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

/* ── 模型与代理 ──────────────────────────────────────────────── */

func TestListModels(t *testing.T) {
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("模型目录是 POST，得到 %s", r.Method)
		}
		writeJSON(w, map[string]any{"data": map[string]any{"models": []map[string]any{
			{"id": "glm-5.2", "name": "GLM-5.2"},
			{"model_name": "deepseek-v4"},
		}}})
	})
	mods, err := ListModels(context.Background(), &Credential{AccessToken: "t"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(mods) != 2 || mods[0].ID != "glm-5.2" {
		t.Fatalf("模型解析错误: %+v", mods)
	}
	if mods[1].ID != "deepseek-v4" || mods[1].Name != "deepseek-v4" {
		t.Fatalf("model_name 形态解析错误: %+v", mods[1])
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

/* ── 换证域：网页域 → API 域 ───────────────────────────────────── */

// 实测踩的坑：GetLoginGuidance 回的 LoginHost 是 www.trae.cn（网页域），
// 直接拿它换证会 POST 到网页，拿回一坨 HTML（字节的 JS 挑战页），
// 表现是「换证回执解析失败：invalid character '<'」。换证必须走 API 域。
func TestExchangeHostIsAPIHostNotWebHost(t *testing.T) {
	cases := map[string]string{
		"www.trae.cn":         "https://api.trae.cn",
		"https://www.trae.cn": "https://api.trae.cn",
		"api.trae.cn":         "https://api.trae.cn",
		"www.trae.ai":         "https://api.trae.ai",
		"":                    DefaultLoginHost,
		"trae.cn":             DefaultLoginHost, // 既非 www 也非 api：不猜
		"://bad":              DefaultLoginHost,
	}
	for in, want := range cases {
		if got := apiHostFor(in); got != want {
			t.Errorf("apiHostFor(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 换证请求实际打到的 host 必须是 API 域，不能是回调带回来的网页域。
func TestCompletePostsToAPIHost(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt"}`))
	}))
	t.Cleanup(srv.Close)

	// 自建传输层：先记下原始 host，再改写到 mock
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	SetHTTPClient(&http.Client{Transport: hostCapture{target: srv, host: &gotHost}})

	ctx := &LoginContext{LoginHost: "www.trae.cn", DeviceID: "d", MachineID: "m"}
	cred, err := Complete(context.Background(), ctx, &Callback{AuthCode: "code", LoginHost: "www.trae.cn"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if cred.AccessToken != "at" {
		t.Fatalf("令牌未解析: %+v", cred)
	}
	if gotHost != "api.trae.cn" {
		t.Fatalf("换证打到了 %q，期望 api.trae.cn（网页域会返回 HTML）", gotHost)
	}
}

// hostCapture 记录请求原始 host 后改写到 httptest 服务器。
type hostCapture struct {
	target *httptest.Server
	host   *string
}

func (h hostCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	*h.host = req.URL.Host
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(h.target.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}
