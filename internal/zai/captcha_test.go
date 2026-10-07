package zai

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// TestRoutesToAPIKeyWhenPlanUnavailable 复现线上诉求：Plan 通道结构性不可用
// （此例 system_file 身份块未配）时，请求必须让位给可用的 API Key 号，而不是被
// 「只能走 Plan」的号触发的风控/团灭挡在门外；且被跳过的纯 Plan 号状态不动、
// 不应向 Plan 端打上游。
func TestRoutesToAPIKeyWhenPlanUnavailable(t *testing.T) {
	up := newMockUpstream(t, mockStep{status: 200, body: `{"type":"message","content":[{"type":"text","text":"hi"}]}`})
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	st, err := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 纯 Plan 号（JWT、无回退 Key）：缺 system_file 时它唯一的路就是坏的
	jwt := NewAccount("plan号", "header."+b64(`{"sub":"u1"}`)+".sig")
	// 免费 API Key 号（单点分隔 → ModeAPIKey）：应被路由过去
	freekey := NewAccount("freekey", "sk-open.abcdefgh")
	if err := st.Add(jwt); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(freekey); err != nil {
		t.Fatal(err)
	}

	// 求解器「已启用」但 SystemBlocks 为空 → planUsable=false（我们不会真的调它）
	cap := NewCaptchaManager(SolverConfig{Script: "x.js"}, func() bool { return true })
	client := NewClient(NewPool(st, 2), cap, nil)

	res, err := client.Do(context.Background(), []byte(`{"model":"glm-4-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("应经 API Key 通道成功，got err=%v", err)
	}
	if res.UsedPlan {
		t.Fatal("缺 system_file 时不应走 Plan 通道")
	}
	if res.Account.ID != freekey.ID {
		t.Fatalf("应由 API Key 号服务，got %s", res.Account.Name)
	}
	if up.count() != 1 {
		t.Fatalf("只应向可用通道打一次，上游调用=%d", up.count())
	}
	if got := st.Get(jwt.ID).Status; got == StatusDisabled || got == StatusInvalid {
		t.Fatalf("被跳过的纯 Plan 号不应被改状态，got %s", got)
	}
}

// TestDoNeverStarvesAPIKeyBehindPlanOnlyPool 是线上间歇「无可用账号」的回归锁：
// 池里有多个「只能走 Plan」的 JWT 号排在真正的 API Key 号前面。Plan 结构性不可用
// 时，每次 Do 都必须稳定落到 API Key 号成功——不能因为纯 Plan 号吃掉有限的尝试
// 预算而间歇失败。反复调用以覆盖选号游标的旋转。
func TestDoNeverStarvesAPIKeyBehindPlanOnlyPool(t *testing.T) {
	up := newMockUpstream(t, mockStep{status: 200, body: `{"content":[{"type":"text","text":"hi"}]}`})
	SetEndpoints(up.server.URL+"/plan", up.server.URL+"/fallback", "", "")

	// 三个纯 Plan 号（无回退 Key）先入池，API Key 号最后
	accounts := []*Account{
		NewAccount("plan1", "header."+b64(`{"sub":"p1"}`)+".sig"),
		NewAccount("plan2", "header."+b64(`{"sub":"p2"}`)+".sig"),
		NewAccount("plan3", "header."+b64(`{"sub":"p3"}`)+".sig"),
		NewAccount("freekey", "sk-open.abcdefgh"),
	}
	client, st := newTestClient(t, accounts...)
	keyID := accounts[3].ID

	for i := 0; i < 12; i++ {
		res, err := client.Do(context.Background(), []byte(`{"model":"glm-4-flash","messages":[]}`))
		if err != nil {
			t.Fatalf("第 %d 次应稳定经 API Key 号成功，got err=%v", i, err)
		}
		if res.Account.ID != keyID {
			t.Fatalf("第 %d 次应由 API Key 号服务，got %s", i, res.Account.Name)
		}
		res.Resp.Body.Close()
	}
	// 纯 Plan 号状态不应被动过（没被拿去打 Plan，也没被冷却/失效）
	for _, name := range []string{"plan1", "plan2", "plan3"} {
		for _, a := range st.List() {
			if a.Name == name && (a.Status == StatusDisabled || a.Status == StatusInvalid) {
				t.Fatalf("纯 Plan 号 %s 不应被改状态，got %s", name, a.Status)
			}
		}
	}
}
