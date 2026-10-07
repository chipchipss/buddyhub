package zai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMergeBalancesMergesSameModelWindows(t *testing.T) {
	// 同模型两个窗口（日窗 + 一次性）：必须相加而不是覆盖，
	// 否则额度显示与耗尽判定都会被后一个窗口冲掉。
	bals := []any{
		map[string]any{"show_name": "GLM-5.3", "total_units": float64(100), "used_units": float64(30), "remaining_units": float64(70), "expires_at": float64(1000)},
		map[string]any{"show_name": "GLM-5.3", "total_units": float64(50), "used_units": float64(0), "remaining_units": float64(50), "expires_at": float64(2000)},
		map[string]any{"show_name": "GLM-5.3-Flash", "total_units": float64(10), "used_units": float64(10), "remaining_units": float64(0)},
	}
	got := mergeBalances(bals)
	if len(got) != 2 {
		t.Fatalf("应归并为 2 个模型，got %d", len(got))
	}
	m := got["GLM-5.3"]
	if m.Total != 150 || m.Used != 30 || m.Remaining != 120 {
		t.Fatalf("同模型窗口应相加，got %+v", m)
	}
	if m.ExpiresAt.Unix() != 2000 {
		t.Fatalf("到期时间应取较晚者，got %v", m.ExpiresAt)
	}
}

func TestBonusActive(t *testing.T) {
	now := time.Now()
	plan := map[string]any{
		"entitlements": []any{
			map[string]any{"period": "daily"},
			map[string]any{
				"period":       "one_time",
				"effective_at": float64(now.Add(-time.Hour).Unix()),
				"ends_at":      float64(now.Add(time.Hour).Unix()),
			},
		},
	}
	if !bonusActive(plan, now) {
		t.Fatal("生效中的一次性赠送应判为有效")
	}

	expired := map[string]any{
		"entitlements": []any{
			map[string]any{
				"period":       "one_time",
				"effective_at": float64(now.Add(-2 * time.Hour).Unix()),
				"ends_at":      float64(now.Add(-time.Hour).Unix()),
			},
		},
	}
	if bonusActive(expired, now) {
		t.Fatal("已过期的一次性赠送不应判为有效")
	}
	if bonusActive(map[string]any{}, now) {
		t.Fatal("无 entitlements 应为 false")
	}
}

func TestApplyQuotaStatus(t *testing.T) {
	now := time.Now()

	// 全部窗口归零且无赠送 → 额度用完
	a := &Account{Status: StatusActive}
	applyQuotaStatus(a, map[string]QuotaEntry{"GLM-5.3": {Remaining: 0}}, nil, now)
	if a.Status != StatusExhausted {
		t.Fatalf("窗口归零应判额度用完，got %s", a.Status)
	}

	// 有赠送池 → 不判耗尽（balance 不含赠送额度，单看日窗会误杀）
	b := &Account{Status: StatusActive}
	bonusPlan := map[string]any{"entitlements": []any{
		map[string]any{"period": "one_time", "effective_at": float64(now.Add(-time.Hour).Unix()), "ends_at": float64(now.Add(time.Hour).Unix())},
	}}
	applyQuotaStatus(b, map[string]QuotaEntry{"GLM-5.3": {Remaining: 0}}, []any{bonusPlan}, now)
	if b.Status != StatusActive {
		t.Fatalf("有赠送池时不应判耗尽，got %s", b.Status)
	}

	// 额度恢复 → 从 EXHAUSTED 回 ACTIVE
	c := &Account{Status: StatusExhausted}
	applyQuotaStatus(c, map[string]QuotaEntry{"GLM-5.3": {Remaining: 42}}, nil, now)
	if c.Status != StatusActive {
		t.Fatalf("额度恢复应回 ACTIVE，got %s", c.Status)
	}

	// 冷却中的账号不因额度数字提前解除冷却
	d := &Account{Status: StatusCooling, CoolingUntil: now.Add(time.Hour)}
	applyQuotaStatus(d, map[string]QuotaEntry{"GLM-5.3": {Remaining: 42}}, nil, now)
	if d.Status != StatusCooling {
		t.Fatalf("冷却期不应被额度查询提前解除，got %s", d.Status)
	}

	// 风控禁用绝不因额度数字复活
	e := &Account{Status: StatusDisabled}
	applyQuotaStatus(e, map[string]QuotaEntry{"GLM-5.3": {Remaining: 999}}, nil, now)
	if e.Status != StatusDisabled {
		t.Fatalf("风控禁用不应因额度恢复而复活，got %s", e.Status)
	}
}

func TestFetchQuotaWritesSnapshot(t *testing.T) {
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing/current":
			_, _ = w.Write([]byte(`{"data":{"plans":[{"name":"Coding Plan","expires_at":4102444800}]}}`))
		case "/billing/balance":
			_, _ = w.Write([]byte(`{"data":{"balances":[
				{"show_name":"GLM-5.3","total_units":100,"used_units":20,"remaining_units":80},
				{"show_name":"GLM-5.3-Flash","total_units":50,"used_units":10,"remaining_units":40}]}}`))
		case "/usage":
			_, _ = w.Write([]byte(`{"data":{"requests":7}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer billing.Close()
	SetEndpoints("", "", "", billing.URL)

	st, err := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc := NewAccount("主号", "header."+b64(`{"sub":"u1"}`)+".sig")
	if err := st.Add(acc); err != nil {
		t.Fatal(err)
	}
	client := NewClient(NewPool(st, 2), nil, nil)

	res := client.FetchQuota(context.Background(), acc.ID)
	if res.Error != "" {
		t.Fatalf("额度查询失败: %s", res.Error)
	}

	got := st.Get(acc.ID)
	if len(got.Quota) != 2 {
		t.Fatalf("应写入 2 个模型额度，got %d", len(got.Quota))
	}
	if q := got.Quota["GLM-5.3"]; q.Remaining != 80 || q.Total != 100 {
		t.Fatalf("额度数字不符: %+v", q)
	}
	if got.PlanName != "Coding Plan" {
		t.Fatalf("方案名应写回，got %q", got.PlanName)
	}
	if got.Status != StatusActive {
		t.Fatalf("有额度时应为 active，got %s", got.Status)
	}
	if got.QuotaAt.IsZero() {
		t.Fatal("应记录查询时刻")
	}
}

func TestFetchQuotaDoesNotInvalidateOnBillingAuthFailure(t *testing.T) {
	// billing 族是无验证码/无身份块的裸接口，上游 WAF 对 Go 客户端 TLS 指纹敏感，
	// 会随机 401/403——同一凭证换 HTTP 库实为 200。凭证是否真死只由 messages 通道
	// 判定，故后台额度轮询**绝不因 billing 401 把账号打成 INVALID**，否则会凭空下线整池。
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer billing.Close()
	SetEndpoints("", "", "", billing.URL)

	st, _ := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	acc := NewAccount("被WAF误杀号", "header."+b64(`{"sub":"u2"}`)+".sig")
	_ = st.Add(acc)
	client := NewClient(NewPool(st, 2), nil, nil)

	res := client.FetchQuota(context.Background(), acc.ID)
	if res.Error == "" {
		t.Fatal("billing 401 应回额度不可用错误（供面板显示）")
	}
	if got := st.Get(acc.ID).Status; got == StatusInvalid {
		t.Fatalf("billing 401 不得把账号判为凭证失效，got %s", got)
	}
}

func TestFetchQuotaKeepsAccountOnCaptchaRejection(t *testing.T) {
	// 403 + captcha 文案 = 人机校验，不是凭证失效——不能把账号错杀
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code":3007,"msg":"captcha verify required"}`))
	}))
	defer billing.Close()
	SetEndpoints("", "", "", billing.URL)

	st, _ := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	acc := NewAccount("待验证号", "header."+b64(`{"sub":"u3"}`)+".sig")
	_ = st.Add(acc)
	client := NewClient(NewPool(st, 2), nil, nil)

	client.FetchQuota(context.Background(), acc.ID)
	if got := st.Get(acc.ID).Status; got == StatusInvalid {
		t.Fatal("验证码挑战不应把账号判为失效")
	}
}

func TestBillingHeadersRequireDeviceMid(t *testing.T) {
	a := NewAccount("号", "header."+b64(`{"sub":"u4"}`)+".sig")
	h := billingHeaders(a)
	for _, k := range []string{"X-Device-Mid", "X-Platform", "X-Os-Category", "X-Title", "X-ZCode-App-Version"} {
		if h[k] == "" {
			t.Fatalf("billing 头缺 %s（缺失会被上游判 3001）", k)
		}
	}
	if h["Authorization"] == "" {
		t.Fatal("JWT 账号应带 Bearer")
	}

	// API Key 账号：走 x-api-key
	keyAcc := NewAccount("key号", "sk-abcdefghijkl")
	h2 := billingHeaders(keyAcc)
	if h2["x-api-key"] == "" || h2["Authorization"] != "" {
		t.Fatal("API Key 账号应用 x-api-key")
	}
}

func TestUnixTimeHandlesMillis(t *testing.T) {
	if got := unixTime(1700000000).Unix(); got != 1700000000 {
		t.Fatalf("秒级时间戳解析错误: %d", got)
	}
	if got := unixTime(1700000000000).Unix(); got != 1700000000 {
		t.Fatalf("毫秒级时间戳应归一为秒: %d", got)
	}
}

func TestNumAt(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"a": 42}`), &v)
	m := v.(map[string]any)
	if got := numAt(m["a"]); got != 42 {
		t.Fatalf("numAt = %d, want 42", got)
	}
	if got := numAt(nil); got != 0 {
		t.Fatalf("nil 应返回 0，got %d", got)
	}
}
