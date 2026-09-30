package zai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// claimMock 模拟 billing/preview + billing/claim + event/report。
type claimMock struct {
	server      *httptest.Server
	claimCodes  []int // 依次返回的业务码（0 = 成功）
	claimIdx    int32
	events      []string
	claimBodies []string
}

func newClaimMock(t *testing.T, codes ...int) *claimMock {
	m := &claimMock{claimCodes: codes}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/preview"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[
				{"plan_id":"p-low","name":"低优先","priority":1},
				{"plan_id":"p-high","name":"高优先","priority":9,"entitlements":[
					{"meter":"model_usage","unit_type":"token","show_name":"GLM-5.3","grant_units":1000000,"period":"one_time"}]}]}}`))
		case strings.HasSuffix(r.URL.Path, "/billing/claim"):
			raw, _ := readAllString(r)
			m.claimBodies = append(m.claimBodies, raw)
			i := int(atomic.AddInt32(&m.claimIdx, 1)) - 1
			code := 0
			if i < len(m.claimCodes) {
				code = m.claimCodes[i]
			}
			if code == 0 {
				_, _ = w.Write([]byte(`{"code":0,"data":{"plan":{"starts_at":1700000000,"ends_at":1700100000},"server_time":1700000001}}`))
				return
			}
			if code == 1005 {
				_, _ = w.Write([]byte(`{"code":1005,"msg":"quota full","data":{"plan":{"ends_at":1700200000}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":` + itoa(code) + `,"msg":"upstream"}`))
		case strings.HasSuffix(r.URL.Path, "/event/report"):
			raw, _ := readAllString(r)
			var body map[string]any
			_ = json.Unmarshal([]byte(raw), &body)
			if el, ok := body["element_name"].(string); ok {
				m.events = append(m.events, el)
			}
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func readAllString(r *http.Request) (string, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return string(buf), nil
		}
	}
}

func claimTestClient(t *testing.T, node, solver string) (*Client, *Store, *Account) {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc := NewAccount("领取号", "header."+b64(`{"sub":"u-claim"}`)+".sig")
	if err := st.Add(acc); err != nil {
		t.Fatal(err)
	}
	client := NewClient(NewPool(st, 2), nil, nil)
	if node != "" {
		client.Captcha = NewCaptchaManager(SolverConfig{Command: node, Script: solver, Timeout: 10 * time.Second},
			func() bool { return true })
	}
	return client, st, acc
}

func writeFakeSolver(t *testing.T) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过领取测试")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "solver.js")
	if err := os.WriteFile(p, []byte("console.log('VERIFY_PARAM='+'c'.repeat(260));"), 0o644); err != nil {
		t.Fatal(err)
	}
	return node, p
}

func TestPreviewPlansSortedByPriority(t *testing.T) {
	m := newClaimMock(t)
	SetEndpoints("", "", "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	client, _, acc := claimTestClient(t, "", "")
	plans, err := client.PreviewPlans(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("preview 失败: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("应返回 2 个套餐，got %d", len(plans))
	}
	if plans[0].ID != "p-high" {
		t.Fatalf("应按优先级降序，got %s", plans[0].ID)
	}
	if len(plans[0].Grants) != 1 || plans[0].Grants[0].Name != "GLM-5.3" {
		t.Fatalf("授权项解析错误: %+v", plans[0].Grants)
	}
	if plans[0].Grants[0].Units != 1000000 {
		t.Fatalf("授权额度解析错误: %v", plans[0].Grants[0].Units)
	}
}

func TestClaimAllReportsActivationAndClaims(t *testing.T) {
	node, solver := writeFakeSolver(t)
	m := newClaimMock(t, 0, 0)
	SetEndpoints("", "", "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	client, _, acc := claimTestClient(t, node, solver)
	out, err := client.ClaimAll(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应逐个领取 2 个套餐，got %d", len(out))
	}
	for _, o := range out {
		if !o.OK {
			t.Fatalf("套餐 %s 应领取成功: %s", o.PlanID, o.Message)
		}
	}
	// 成功载荷带套餐窗口与上游时钟
	if out[0].StartsAt == 0 || out[0].EndsAt == 0 || out[0].ServerTime == 0 {
		t.Fatalf("应解析窗口与 server_time: %+v", out[0])
	}

	// preview 前应有激活上报（app_launch + app_daily_active）
	joined := strings.Join(m.events, ",")
	if !strings.Contains(joined, "app_launch") || !strings.Contains(joined, "app_daily_active") {
		t.Fatalf("应上报两个激活事件，got %v", m.events)
	}

	// claim 请求体应是 {"plan_id": "..."}
	if len(m.claimBodies) == 0 || !strings.Contains(m.claimBodies[0], "plan_id") {
		t.Fatalf("claim 请求体错误: %v", m.claimBodies)
	}
}

func TestClaimRetriesOnCaptchaRejection(t *testing.T) {
	node, solver := writeFakeSolver(t)
	m := newClaimMock(t, 3007, 0) // 第一次验证码被拒，第二次成功
	SetEndpoints("", "", "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	client, _, acc := claimTestClient(t, node, solver)
	out, err := client.ClaimOne(context.Background(), acc.ID, "p-high", "高优先", nil)
	if err != nil {
		t.Fatalf("3007 后换码重试应成功: %v", err)
	}
	if !out.OK {
		t.Fatal("重试后应成功")
	}
	if int(atomic.LoadInt32(&m.claimIdx)) != 2 {
		t.Fatalf("应请求两次 claim，got %d", m.claimIdx)
	}
}

func TestClaimQuotaFullCarriesNextAt(t *testing.T) {
	node, solver := writeFakeSolver(t)
	m := newClaimMock(t, 1005)
	SetEndpoints("", "", "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	client, _, acc := claimTestClient(t, node, solver)
	out, err := client.ClaimAll(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if len(out) == 0 || out[0].Code != 1005 {
		t.Fatalf("应返回 1005，got %+v", out)
	}
	if out[0].NextAt == 0 {
		t.Fatal("1005 应带名额恢复时间")
	}
	if !strings.Contains(out[0].Message, "名额") {
		t.Fatalf("文案应说明名额用完，got %q", out[0].Message)
	}
}

func TestClaimRejectsNonJWTAcount(t *testing.T) {
	client, st, _ := claimTestClient(t, "", "")
	keyAcc := NewAccount("纯key", "sk-abcdefghijklmnop")
	if err := st.Add(keyAcc); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ClaimAll(context.Background(), keyAcc.ID); err == nil {
		t.Fatal("非 JWT 账号应拒绝领取")
	}
}

func TestClaimRequiresSolver(t *testing.T) {
	m := newClaimMock(t)
	SetEndpoints("", "", "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	client, _, acc := claimTestClient(t, "", "") // 无求解器
	if _, err := client.ClaimAll(context.Background(), acc.ID); err == nil {
		t.Fatal("无验证码求解器时应拒绝领取")
	} else if !strings.Contains(err.Error(), "验证码求解器") {
		t.Fatalf("错误文案应指向求解器配置，got %v", err)
	}
}

func TestClaimSkipsCoolingAccount(t *testing.T) {
	m := newClaimMock(t)
	SetEndpoints("", "", "", m.server.URL)
	SetOrigins(m.server.URL, m.server.URL)

	client, st, acc := claimTestClient(t, "", "")
	_ = st.Update(acc.ID, func(a *Account) { a.Cool(time.Hour) })
	if _, err := client.PreviewPlans(context.Background(), acc.ID); err == nil {
		t.Fatal("冷却中的账号不应打 billing（冷却期零上游流量的不变量）")
	}
}

func TestActivationEventBodyFields(t *testing.T) {
	p := newProfile()
	body := activationEventBody("app_launch", p, "u-1")
	want := []string{
		"event_id", "client_timezone", "client_language", "element_name", "event_region",
		"event_type", "event_text", "event_extra_detail", "user_id", "screen_resolution",
		"app_version", "device_os_category", "device_os_version", "device_mid", "mac_id",
		"marketing_params",
	}
	if len(body) != len(want) {
		t.Fatalf("事件体应为 %d 个字段，got %d", len(want), len(body))
	}
	for _, k := range want {
		if _, ok := body[k]; !ok {
			t.Fatalf("缺字段 %s", k)
		}
	}
	if body["device_mid"] != p.DeviceMid {
		t.Fatal("device_mid 应取账号指纹")
	}
}
