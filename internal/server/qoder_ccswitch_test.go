package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/qoder"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

/* ── 阶段 4：CC-Switch 兼容端点（fork ccSwitchUsage 契约）── */

// newCcSwitchHandler 带 extstore 账号表的 handler（余额管理器留空——
// 无观测时账号 quotaStatus 为 not_queried，走 not_queried 口径）。
func newCcSwitchHandler(t *testing.T, creds ...string) *Handler {
	t.Helper()
	mgr := extstore.NewManager(t.TempDir())
	for i, c := range creds {
		id := "acct" + string(rune('0'+i))
		if err := mgr.Add(extstore.PQoder, id, id, json.RawMessage(c)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	return NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
}

func postCCSwitch(t *testing.T, h *Handler, path, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// 契约字段齐全（fork buildCcSwitchUsage 返回形状）。

func TestCcSwitchUsageContractFields(t *testing.T) {
	h := newCcSwitchHandler(t, `{"access_token":"t1"}`)
	w := postCCSwitch(t, h, "/api/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	for _, k := range []string{"isValid", "balance", "remaining", "total", "used", "unit", "planName", "extra"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("missing field %q in %s", k, w.Body.String())
		}
	}
	if body["isValid"] != true {
		t.Fatalf("isValid = %v", body["isValid"])
	}
	if body["unit"] != "credits" {
		t.Fatalf("unit = %v", body["unit"])
	}
}

// /user/balance 追加 is_active（fork includeActiveField 分支）。

func TestCcSwitchBalanceHasIsActive(t *testing.T) {
	h := newCcSwitchHandler(t, `{"access_token":"t1"}`)
	w := postCCSwitch(t, h, "/user/balance", "")
	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("json: %v", w.Body.String())
	}
	v, ok := body["is_active"]
	if !ok {
		t.Fatal("/user/balance must carry is_active")
	}
	if v != true {
		t.Fatalf("is_active = %v", v)
	}
	// /api/usage 不带该字段。
	w2 := postCCSwitch(t, h, "/api/usage", "")
	var body2 map[string]any
	json.Unmarshal(w2.Body.Bytes(), &body2)
	if _, ok := body2["is_active"]; ok {
		t.Fatal("/api/usage must NOT carry is_active")
	}
}

// 无账号时返回 No active Qoder accounts（fork L94-104）。

func TestCcSwitchNoAccounts(t *testing.T) {
	h := newCcSwitchHandler(t) // 零账号
	w := postCCSwitch(t, h, "/api/usage", "")
	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("json: %v", w.Body.String())
	}
	if body["isValid"] != false || body["error"] != "No active Qoder accounts" {
		t.Fatalf("body = %v", body)
	}
	if body["invalidMessage"] != "No active Qoder accounts are available." {
		t.Fatalf("invalidMessage = %v", body["invalidMessage"])
	}
}

// 有分配上限时走本地账本口径（fork L113-127 分支：不查上游，
// 总数=分配上限、已用=账本、剩余=差额）。

func TestCcSwitchAllocationLedgerView(t *testing.T) {
	h := newCcSwitchHandler(t, `{"access_token":"t1"}`, `{"access_token":"t2"}`)
	h.qoderLedger.SetAllocation(qoder.AccountKey("qoder", "acct0"), 100)
	h.qoderLedger.SetAllocation(qoder.AccountKey("qoder", "acct1"), 50)
	h.qoderLedger.RecordCredits(qoder.AccountKey("qoder", "acct0"), 30)

	w := postCCSwitch(t, h, "/api/usage", "")
	var body ccSwitchUsageBody
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("json: %v", w.Body.String())
	}
	if !body.IsValid {
		t.Fatalf("isValid false: %s", w.Body.String())
	}
	if body.Total != 150 || body.Used != 30 || body.Remaining != 120 {
		t.Fatalf("total/used/remaining = %v/%v/%v, want 150/30/120", body.Total, body.Used, body.Remaining)
	}
	if body.Balance != 120 {
		t.Fatalf("balance = %v", body.Balance)
	}
	if body.PlanName != "Qoder allocated credits" {
		t.Fatalf("planName = %q", body.PlanName)
	}
	// extra 带账号数与活跃账号。
	if !strings.Contains(body.Extra, "2 accounts") {
		t.Fatalf("extra = %q", body.Extra)
	}
	if !strings.Contains(body.Extra, "active:") {
		t.Fatalf("extra missing active: %q", body.Extra)
	}
}

// 无分配上限、无余额观测 → not_queried 口径：total/used 0，isValid 仍 true
// （fork 的 not_queried 行不进汇总但不失败）。

func TestCcSwitchNotQueriedFallback(t *testing.T) {
	h := newCcSwitchHandler(t, `{"access_token":"t1"}`)
	w := postCCSwitch(t, h, "/api/usage", "")
	var body ccSwitchUsageBody
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("json: %v", w.Body.String())
	}
	if !body.IsValid {
		t.Fatalf("not_queried must stay valid: %s", w.Body.String())
	}
	if body.Total != 0 || body.Remaining != 0 {
		t.Fatalf("total/remaining = %v/%v, want 0/0", body.Total, body.Remaining)
	}
	if body.PlanName != "Qoder all accounts" {
		t.Fatalf("planName = %q", body.PlanName)
	}
}

// 鉴权：错误 Bearer 被 withAuth 挡 401（fork 401 invalid api key 同语义）。

func TestCcSwitchAuthRejects(t *testing.T) {
	h := newCcSwitchHandler(t, `{"access_token":"t1"}`)
	h.cfg.APIKey = "right-key"
	w := postCCSwitch(t, h, "/api/usage", "wrong-key")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
