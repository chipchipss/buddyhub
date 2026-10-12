package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/usage"
)

/* ── 阶段 4/5：面板额度/Key Name 增强 ────────────────────── */

type fakeQuotaRow struct {
	AccountKey string  `json:"account_key"`
	Allocated  float64 `json:"allocated"`
	Used       float64 `json:"used"`
	Remaining  float64 `json:"remaining"`
	Exhausted  bool    `json:"exhausted"`
	Precise    bool    `json:"precise"`
}

func newQuotaTestPanel(t *testing.T, quotaRows []any, keyCfg string) *Panel {
	t.Helper()
	dir := t.TempDir()
	cfgFp := filepath.Join(dir, "config.json")
	if keyCfg != "" {
		if err := os.WriteFile(cfgFp, []byte(keyCfg), 0o644); err != nil {
			t.Fatalf("write cfg: %v", err)
		}
	}
	return New(Config{
		StateFile:  filepath.Join(dir, "state.json"),
		ConfigPath: cfgFp,
		LoadConfig: func() (any, error) {
			raw, err := os.ReadFile(cfgFp)
			if err != nil {
				if os.IsNotExist(err) {
					return map[string]any{}, nil
				}
				return nil, err
			}
			var doc map[string]any
			return doc, json.Unmarshal(raw, &doc)
		},
		QuotaSource: func() []any { return quotaRows },
		Usage:       usage.New(""),
	})
}

func TestQuotaEndpoint(t *testing.T) {
	p := newQuotaTestPanel(t, []any{
		fakeQuotaRow{AccountKey: "qoder/a", Allocated: 100, Used: 40, Remaining: 60, Precise: true},
	}, "")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panel/api/quota", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		OK        bool            `json:"ok"`
		Available bool            `json:"available"`
		Quotas    []fakeQuotaRow  `json:"quotas"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("json: %s", w.Body.String())
	}
	if !body.OK || !body.Available || len(body.Quotas) != 1 {
		t.Fatalf("body = %+v", body)
	}
	if body.Quotas[0].AccountKey != "qoder/a" || body.Quotas[0].Used != 40 {
		t.Fatalf("row = %+v", body.Quotas[0])
	}
}

func TestQuotaEndpointNilSource(t *testing.T) {
	p := newQuotaTestPanel(t, nil, "")
	p.cfg.QuotaSource = nil
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panel/api/quota", nil))
	var body struct {
		OK        bool `json:"ok"`
		Available bool `json:"available"`
		Quotas    []any `json:"quotas"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if !body.OK || body.Available || len(body.Quotas) != 0 {
		t.Fatalf("nil source must be empty-not-error: %+v", body)
	}
}

func TestUsageKeyNamesAndFilter(t *testing.T) {
	// 两把 Key：带名字的 + 匿名的。
	cfg := `{"api_keys":[
		{"key":"sk-alpha-key","name":"团队A"},
		{"key":"sk-beta-key","name":"测试B"}
	]}`
	p := newQuotaTestPanel(t, nil, cfg)
	// 记两把 Key 的用量（id 与 keyID() 同源）。
	idA := keyID("sk-alpha-key")
	idB := keyID("sk-beta-key")
	p.cfg.Usage.AddKey(idA, true, time.Now())
	p.cfg.Usage.AddKey(idB, false, time.Now())

	// 无过滤：全部行 + 名称映射。
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panel/api/usage/keys", nil))
	var body struct {
		OK   bool             `json:"ok"`
		Keys []keyUsagePayload `json:"keys"`
		NamesMapped int        `json:"names_mapped"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("json: %s", w.Body.String())
	}
	if !body.OK || len(body.Keys) != 2 || body.NamesMapped != 2 {
		t.Fatalf("body = %+v", body)
	}
	byID := map[string]string{}
	for _, k := range body.Keys {
		byID[k.ID] = k.Name
	}
	if byID[idA] != "团队A" || byID[idB] != "测试B" {
		t.Fatalf("names = %v", byID)
	}

	// 过滤：key_name=团队 只出 A。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/panel/api/usage/keys?key_name="+strings.ToLower("团队"), nil)
	p.ServeHTTP(w2, req2)
	var body2 struct {
		Keys []keyUsagePayload `json:"keys"`
	}
	json.Unmarshal(w2.Body.Bytes(), &body2)
	if len(body2.Keys) != 1 || body2.Keys[0].Name != "团队A" {
		t.Fatalf("filtered keys = %+v", body2.Keys)
	}

	// 过滤：无命中出空列表（不是错误）。
	w3 := httptest.NewRecorder()
	p.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/panel/api/usage/keys?key_name=nobody", nil))
	var body3 struct {
		Keys []keyUsagePayload `json:"keys"`
	}
	json.Unmarshal(w3.Body.Bytes(), &body3)
	if len(body3.Keys) != 0 {
		t.Fatalf("no-hit must be empty: %+v", body3.Keys)
	}
}
