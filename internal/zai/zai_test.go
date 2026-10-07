package zai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClassifySignals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		hdr    http.Header
		want   FailureKind
	}{
		{"402 额度用完", 402, `{"error":"payment required"}`, nil, FailExhausted},
		{"400 quota 关键词", 400, `{"error":{"message":"Insufficient balance"}}`, nil, FailExhausted},
		{"中文额度不足", 400, `{"message":"余额不足"}`, nil, FailExhausted},
		{"429 限流", 429, `rate limited`, nil, FailRateLimited},
		{"405 真风控", 405, `{"code":3012}`, nil, FailRiskControl},
		{"400 带 3012 也算风控", 400, `{"code": 3012, "msg":"unusual activity"}`, nil, FailRiskControl},
		{"401 凭证失效", 401, `{"error":"unauthorized"}`, nil, FailInvalid},
		{"403 普通失效", 403, `{"error":"forbidden"}`, nil, FailInvalid},
		{"400 code 3007 验证码", 400, `{"code":3007,"msg":"captcha expired"}`, nil, FailCaptcha},
		{"403 验证码挑战不算失效", 403, `{"error":"captcha verify required"}`, nil, FailCaptcha},
		{"响应头透出验证码", 403, `{}`, http.Header{"X-Captcha-Challenge": []string{"1"}}, FailCaptcha},
		{"500 上游错误", 500, `boom`, nil, FailServerError},
		{"503 上游错误", 503, ``, nil, FailServerError},
		{"200 正常", 200, `{"content":[]}`, nil, FailNone},
		{"200 体里有 verify 字样不算验证码", 200, `{"text":"please verify this"}`, nil, FailNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, []byte(c.body), c.hdr); got != c.want {
				t.Fatalf("Classify = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	h := http.Header{"Retry-After": []string{"30"}}
	if got := RetryAfter(h, 5); got != 30 {
		t.Fatalf("RetryAfter = %d, want 30", got)
	}
	if got := RetryAfter(nil, 5); got != 5 {
		t.Fatalf("缺省应回落 fallback，got %d", got)
	}
	if got := RetryAfter(http.Header{"Retry-After": []string{"Wed, 21 Oct 2015 07:28:00 GMT"}}, 7); got != 7 {
		t.Fatalf("HTTP 日期形式应回落 fallback，got %d", got)
	}
	if got := RetryAfter(http.Header{"Retry-After": []string{"999999"}}, 7); got != 86400 {
		t.Fatalf("应封顶 86400，got %d", got)
	}
}

func TestAccountStateMachine(t *testing.T) {
	a := NewAccount("测试号", "aaa.bbb.ccc")
	if a.Mode != ModeJWT {
		t.Fatalf("三段点分应判为 JWT，got %s", a.Mode)
	}
	if !a.HasJWTPath() || !a.Selectable(time.Now()) {
		t.Fatal("新账号应可用")
	}

	a.Cool(50 * time.Millisecond)
	if a.Selectable(time.Now()) {
		t.Fatal("冷却期内不应可选中")
	}
	time.Sleep(60 * time.Millisecond)
	if !a.Selectable(time.Now()) {
		t.Fatal("冷却到期后应可选中（由 Reap 回迁状态）")
	}

	a.Exhaust(time.Minute)
	if a.Status != StatusExhausted || a.Selectable(time.Now()) {
		t.Fatal("额度用完应不可选中")
	}

	a.MarkOK()
	if a.Status != StatusActive || !a.Selectable(time.Now()) {
		t.Fatal("成功后应回 ACTIVE")
	}

	a.Invalidate("401")
	if a.Status != StatusInvalid || a.Selectable(time.Now()) {
		t.Fatal("失效账号不应可选中")
	}

	a.Status = StatusActive
	a.BanForRisk("3012")
	if a.Status != StatusDisabled || a.RiskStrikes != 1 {
		t.Fatal("风控应禁用并累计次数")
	}
	a.MarkOK()
	if a.Status != StatusDisabled {
		t.Fatal("风控禁用后 MarkOK 不得自动恢复")
	}
}

func TestKeyFallbackOnlyForJWTAccounts(t *testing.T) {
	// 纯 API Key 账号：主键不是"回退"，失效后不得继续被选中
	keyOnly := NewAccount("纯key", "sk-abcdefghijklmnop")
	if keyOnly.Mode != ModeAPIKey {
		t.Fatalf("非三段点分应判为 API Key，got %s", keyOnly.Mode)
	}
	if keyOnly.HasKeyFallback() {
		t.Fatal("纯 API Key 账号不应有回退通道")
	}

	// JWT 账号附带 Key：才算回退
	jwt := NewAccount("jwt号", "aaa.bbb.ccc")
	jwt.APIKey = "sk-abcdefghijklmnop"
	if !jwt.HasKeyFallback() {
		t.Fatal("JWT 账号带 Key 应有回退通道")
	}
	jwt.Status = StatusDisabled
	if jwt.HasKeyFallback() {
		t.Fatal("风控禁用的账号不得走回退通道")
	}
}

func TestStorePersistAndReap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zai-accounts.json")

	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAccount("号一", "aaa.bbb.ccc")
	a.APIKey = "sk-key-1234567890"
	if err := st.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(NewAccount("号二", "sk-second-key-abcdef")); err != nil {
		t.Fatal(err)
	}

	// 权限：含明文凭证，POSIX 下必须 0600（Windows 无 POSIX 权限位，跳过）
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("账号文件权限 = %o, want 600", perm)
		}
	}

	// 重载
	re, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	list := re.List()
	if len(list) != 2 {
		t.Fatalf("重载后账号数 = %d, want 2", len(list))
	}
	if list[0].Name != "号一" || list[0].APIKey == "" {
		t.Fatalf("重载内容不符: %+v", list[0])
	}
	if list[0].Fingerprint == nil || list[0].Fingerprint.DeviceMid == "" {
		t.Fatal("重载后应补齐设备档案")
	}

	// 冷却到期回迁
	_ = re.Update(a.ID, func(x *Account) { x.Cool(20 * time.Millisecond) })
	if n := re.Reap(time.Now()); n != 0 {
		t.Fatalf("未到期不应回迁，n=%d", n)
	}
	time.Sleep(30 * time.Millisecond)
	if n := re.Reap(time.Now()); n != 1 {
		t.Fatalf("到期应回迁 1 个，n=%d", n)
	}
	if got := re.Get(a.ID).Status; got != StatusActive {
		t.Fatalf("回迁后状态 = %s, want active", got)
	}

	if err := re.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if len(re.List()) != 1 {
		t.Fatal("删除后应只剩 1 个")
	}
}

func TestPoolSelection(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "a.json"))
	_ = st.Add(NewAccount("一", "aaa.bbb.1"))
	_ = st.Add(NewAccount("二", "aaa.bbb.2"))
	_ = st.Add(NewAccount("三", "aaa.bbb.3"))

	p := NewPool(st, 1)

	// 轮转：连续取应依次覆盖不同账号
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		a, err := p.Pick(false, nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[a.Name] = true
		p.Release(a.ID)
	}
	if len(seen) != 3 {
		t.Fatalf("轮转应覆盖 3 个账号，实际 %d", len(seen))
	}

	// 单账号并发上限 1：占住后不得再选到同一个
	a1, err := p.Pick(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := p.Pick(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID == a2.ID {
		t.Fatal("满并发的账号应被跳过")
	}
	p.Release(a1.ID)
	p.Release(a2.ID)

	// 全部禁用 → 无可用
	_ = st.Update(a1.ID, func(x *Account) { x.Status = StatusDisabled; x.Enabled = false })
	_ = st.Update(a2.ID, func(x *Account) { x.Status = StatusDisabled; x.Enabled = false })
	_ = st.Update(a1.ID, func(x *Account) {})
	for _, acc := range st.List() {
		_ = st.Update(acc.ID, func(x *Account) { x.Enabled = false })
	}
	if _, err := p.Pick(false, nil); err != ErrNoAccount {
		t.Fatalf("全禁用应返回 ErrNoAccount，got %v", err)
	}
}

func TestPoolPrefersPlanChannel(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "a.json"))
	keyOnly := NewAccount("纯key", "sk-abcdefghijklmnop")
	_ = st.Add(keyOnly)
	jwtAcc := NewAccount("jwt号", "aaa.bbb.ccc")
	_ = st.Add(jwtAcc)

	p := NewPool(st, 2)
	a, err := p.Pick(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != jwtAcc.ID {
		t.Fatalf("wantPlan=true 应优先 JWT 账号，got %s", a.Name)
	}
}

// TestPickUsablePredicateExcludesPlanOnly 锁定「按可用性过滤选号」：Plan 结构性不可用
// 时，usable 过滤器必须把「只能走 Plan」的 JWT 号挡在候选之外，只留下真正能建流的
// API Key 号——否则多个纯 Plan 号会吃掉尝试预算，把可用号挤到选不到（线上间歇
// 「无可用账号」的根因）。
func TestPickUsablePredicateExcludesPlanOnly(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "a.json"))
	// 三个纯 Plan 号排在前面（无回退 Key）
	for _, n := range []string{"p1", "p2", "p3"} {
		_ = st.Add(NewAccount(n, "header."+b64(`{"sub":"`+n+`"}`)+".sig"))
	}
	// 唯一一个此刻可用的 API Key 号排在最后
	key := NewAccount("freekey", "sk-open.abcdefgh")
	_ = st.Add(key)

	p := NewPool(st, 2)
	planUsable := false
	usable := func(a *Account) bool {
		if a.Mode == ModeAPIKey || a.HasKeyFallback() {
			return true
		}
		return planUsable && a.HasJWTPath()
	}
	// 反复选号（覆盖游标旋转），每次都必须拿到 API Key 号，绝不能拿到纯 Plan 号
	for i := 0; i < 8; i++ {
		a, err := p.Pick(false, usable)
		if err != nil {
			t.Fatalf("第 %d 次应能选到可用号，got err=%v", i, err)
		}
		if a.ID != key.ID {
			t.Fatalf("第 %d 次应选到 API Key 号，got %s(mode=%s)", i, a.Name, a.Mode)
		}
		p.Release(a.ID)
	}

	// 对照组：不带过滤器时，纯 Plan 号仍会被选中（证明过滤器确实在起作用）
	if a, err := p.Pick(false, nil); err != nil || a.Mode == ModeAPIKey {
		t.Fatalf("无过滤器应可能选到非 API Key 号，got id=%v err=%v", a, err)
	}
}

func TestBuildRequestPlanVsFallback(t *testing.T) {
	// 用真实形态的 JWT（payload 可解出 sub），metadata.user_id 才有时可断言
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-7"}`))
	jwtAcc := NewAccount("jwt号", "header."+payload+".sig")
	jwtAcc.APIKey = "sk-fallback-key-123"

	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)

	url, h, out := BuildRequest(jwtAcc, body, "PARAM123", "cn", false, nil)
	if url != PlanMessagesURL {
		t.Fatalf("JWT 应走 Plan 端点，got %s", url)
	}
	if h["Authorization"] != "Bearer "+jwtAcc.JWT {
		t.Fatalf("Plan 通道鉴权头不对: %v", h["Authorization"])
	}
	if h["X-Aliyun-Captcha-Verify-Param"] != "PARAM123" || h["X-Aliyun-Captcha-Verify-Region"] != "cn" {
		t.Fatal("Plan 通道必须带验证码头")
	}
	for _, k := range []string{"X-Device-Mid", "X-Platform", "X-Os-Category", "X-Os-Version", "X-Title", "X-ZCode-Agent"} {
		if h[k] == "" {
			t.Fatalf("Plan 通道缺身份头 %s", k)
		}
	}
	for _, k := range []string{"x-request-id", "x-zcode-session-type", "x-zcode-trace-id"} {
		if h[k] == "" {
			t.Fatalf("Plan 通道缺追踪头 %s", k)
		}
	}
	// 误发这两个头会触发上游 3012
	if h["x-query-id"] != "" || h["x-session-id"] != "" {
		t.Fatal("Plan 通道不得发 x-query-id / x-session-id")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	meta, _ := m["metadata"].(map[string]any)
	if meta["user_id"] != "user-7" {
		t.Fatalf("Plan 通道应注入 metadata.user_id，got %v", m["metadata"])
	}

	// 强制回退：走 API Key 端点、最小头集、无验证码头
	url2, h2, _ := BuildRequest(jwtAcc, body, "PARAM123", "cn", true, nil)
	if url2 != FallbackMessagesURL {
		t.Fatalf("回退应走 api.z.ai，got %s", url2)
	}
	if h2["x-api-key"] != "sk-fallback-key-123" {
		t.Fatal("回退通道应用 x-api-key")
	}
	if h2["X-Aliyun-Captcha-Verify-Param"] != "" {
		t.Fatal("回退通道不应带验证码头")
	}
	if h2["X-Device-Mid"] != "" {
		t.Fatal("回退通道不应带桌面身份头（最小头集）")
	}
}

func TestJWTUserID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-42","exp":9999999999}`))
	tok := "header." + payload + ".sig"
	if got := jwtUserID(tok); got != "user-42" {
		t.Fatalf("jwtUserID = %q, want user-42", got)
	}
	if got := jwtUserID("not-a-jwt"); got != "" {
		t.Fatalf("非 JWT 应返回空串，got %q", got)
	}
	if got := jwtUserID("a." + base64.RawURLEncoding.EncodeToString([]byte(`{bad`)) + ".c"); got != "" {
		t.Fatal("坏 payload 应返回空串")
	}
}

func TestTransformPlanBody(t *testing.T) {
	a := NewAccount("号", "aaa."+base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1"}`))+".ccc")
	body := []byte(`{"model":"glm-5.3","system":"原始 system","messages":[{"role":"user","content":"hi"}]}`)
	blocks := []SystemBlock{{Type: "text", Text: "你是 ZCode", CacheControl: map[string]any{"type": "ephemeral"}}}

	out := transformPlanBody(body, a, blocks)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}

	sys, _ := m["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("身份块应前置且保留原 system，got %d 块", len(sys))
	}
	if first, _ := sys[0].(map[string]any); first["text"] != "你是 ZCode" {
		t.Fatal("身份块应在最前")
	}
	if second, _ := sys[1].(map[string]any); second["text"] != "原始 system" {
		t.Fatal("客户端原 system 应保留在后")
	}

	msgs, _ := m["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	blk, _ := content[0].(map[string]any)
	if blk["cache_control"] == nil {
		t.Fatal("最后一条非 system 消息应打 cache_control")
	}

	meta, _ := m["metadata"].(map[string]any)
	if meta["user_id"] != "u1" {
		t.Fatalf("metadata.user_id = %v, want u1", meta["user_id"])
	}

	// 畸形体：原样返回，不放大
	bad := []byte(`{not json`)
	if string(transformPlanBody(bad, a, blocks)) != string(bad) {
		t.Fatal("畸形体应原样透传")
	}
}

func TestCaptchaDisabledWithoutSolver(t *testing.T) {
	m := NewCaptchaManager(SolverConfig{}, func() bool { return true })
	if m.Enabled() {
		t.Fatal("未配置脚本时求解器应视为不可用")
	}
	if _, _, err := m.Get(context.Background()); err == nil {
		t.Fatal("未配置求解器应返回错误（调用方据此降级到 API Key 通道）")
	}
}

func TestCaptchaSolverContract(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过外部求解器契约测试")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-solver.js")
	// 契约：<command> <script> <scene> <region> <prefix> → stdout 打 VERIFY_PARAM=
	fake := "console.log('VERIFY_PARAM=' + 'x'.repeat(260));"
	if err := os.WriteFile(script, []byte(fake), 0o644); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过契约测试")
	}

	m := NewCaptchaManager(SolverConfig{Command: node, Script: script, Timeout: 10 * time.Second}, func() bool { return true })
	param, region, err := m.Get(context.Background())
	if err != nil {
		t.Fatalf("契约调用应成功: %v", err)
	}
	if len(param) < 200 {
		t.Fatalf("应拿到完整 param，got %d 字符", len(param))
	}
	if region == "" {
		t.Fatal("应带 region")
	}
}

func TestCaptchaRejectsShortParam(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "short.js")
	// 降级结果（缺 securityToken，长度 < 200）：必须判失败，不能拿去用
	_ = os.WriteFile(script, []byte("console.log('VERIFY_PARAM=' + 'y'.repeat(80));"), 0o644)

	m := NewCaptchaManager(SolverConfig{Command: node, Script: script, Timeout: 8 * time.Second}, func() bool { return true })
	if _, _, err := m.Get(context.Background()); err == nil {
		t.Fatal("过短的降级结果必须判失败")
	}
}

func TestMask(t *testing.T) {
	if got := Mask("short"); got != "••••" {
		t.Fatalf("短串应全掩码，got %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz"
	got := Mask(long)
	if strings.Contains(got, "hijklmnop") {
		t.Fatalf("中段应被掩掉，got %q", got)
	}
	if !strings.HasPrefix(got, "abcdef") || !strings.HasSuffix(got, "wxyz") {
		t.Fatalf("应保留首尾，got %q", got)
	}
}
