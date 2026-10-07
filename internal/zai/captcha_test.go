package zai

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestRegionCandidates 验证区域尝试顺序：last-good → 配置区域 → 兜底，且去重。
func TestRegionCandidates(t *testing.T) {
	m := NewCaptchaManager(SolverConfig{Script: "x.js"}, func() bool { return true })

	if got := m.regionCandidates("cn"); !reflect.DeepEqual(got, []string{"cn", "sgp"}) {
		t.Fatalf("primary=cn → %v, want [cn sgp]", got)
	}
	if got := m.regionCandidates("sgp"); !reflect.DeepEqual(got, []string{"sgp", "cn"}) {
		t.Fatalf("primary=sgp → %v, want [sgp cn]", got)
	}

	m.setLastRegion("sgp")
	// last-good 优先：即使上游仍声明 cn，也应先试 sgp（cn 在该出口降级）
	if got := m.regionCandidates("cn"); !reflect.DeepEqual(got, []string{"sgp", "cn"}) {
		t.Fatalf("last=sgp,primary=cn → %v, want [sgp cn]", got)
	}
}

// TestSolveFallsBackToWorkingRegion 复现线上故障：cn 只吐降级 token（<200 字符、
// 无 securityToken），必须自动换到 sgp 拿到完整 token，并把 sgp 记为 last-good。
func TestSolveFallsBackToWorkingRegion(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过求解器回退测试")
	}
	dir := t.TempDir()
	solver := filepath.Join(dir, "solver.js")
	// argv: [node, solver.js, scene, region, prefix]
	stub := `const r = process.argv[3];
if (r === 'cn') { console.log('VERIFY_PARAM=' + 's'.repeat(76)); }
else { console.log('VERIFY_PARAM=' + 'p'.repeat(280)); }`
	if err := os.WriteFile(solver, []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewCaptchaManager(SolverConfig{Command: node, Script: solver, Timeout: 10 * time.Second},
		func() bool { return true })

	tok, err := m.solve(context.Background(), captchaCfg{scene: "11xygtvd", region: "cn", prefix: "no8xfe"})
	if err != nil {
		t.Fatalf("应回退到 sgp 成功求解: %v", err)
	}
	if tok.region != "sgp" {
		t.Fatalf("tok.region = %q, want sgp", tok.region)
	}
	if len(tok.param) < 200 {
		t.Fatalf("应拿到完整 token（>=200 字符），got %d", len(tok.param))
	}
	if m.lastRegion != "sgp" {
		t.Fatalf("last-good 区域应记为 sgp，got %q", m.lastRegion)
	}
}

// TestRiskControlWithoutSystemBlocksDoesNotBan 复现团灭场景：Plan 通道未配身份块
// （system_file）时上游判 3012。此时应给出可操作的配置错误、立即停止（不重复打
// 上游加剧风控）、且**不禁用账号**——否则首个请求就把整池永久踢出。
func TestRiskControlWithoutSystemBlocksDoesNotBan(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过风控守卫测试")
	}
	dir := t.TempDir()
	solver := filepath.Join(dir, "solver.js")
	if err := os.WriteFile(solver, []byte("console.log('VERIFY_PARAM='+'p'.repeat(260));"), 0o644); err != nil {
		t.Fatal(err)
	}

	up := newMockUpstream(t, mockStep{status: 405, body: `{"code":3012,"message":"unusual activity"}`})
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	jwt := NewAccount("主号", "header."+b64("{\"sub\":\"u1\"}")+".sig")
	client, st := newTestClient(t, jwt) // SystemBlocks = nil（未配 system_file）
	client.Captcha = NewCaptchaManager(SolverConfig{Command: node, Script: solver, Timeout: 10 * time.Second},
		func() bool { return true })

	_, err = client.Do(context.Background(), []byte(`{"model":"glm-5.3","messages":[]}`))
	if err == nil {
		t.Fatal("3012 应返回错误")
	}
	if !strings.Contains(err.Error(), "system_file") {
		t.Fatalf("错误应指向 system_file 缺失，got: %v", err)
	}
	if got := st.Get(jwt.ID).Status; got == StatusDisabled {
		t.Fatalf("缺 system_file 导致的 3012 不应禁用账号，got status=%s", got)
	}
	if up.count() != 1 {
		t.Fatalf("应立即停止重试，上游调用数=%d, want 1", up.count())
	}
}
