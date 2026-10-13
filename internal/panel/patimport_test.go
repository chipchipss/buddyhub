package panel

// PAT 批量导入测试（fork qoder-farm inject_qoder_pat.py 语义）：
// 幂等 upsert、pt- 前缀校验、坏行不阻断好行、落库凭据形状。
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extstore"
)

// 基本路径：合法数组全量入库，凭据落成 {"pat":"pt-..."}，id/label = email。
func TestImportQoderPATHappy(t *testing.T) {
	p := newQuotaTestPanel(t, nil, "")
	body := `{"accounts":[
		{"email":"a@x.com","pat":"pt-aaaaaaaaaaaaaaaaaaaa"},
		{"email":"b@x.com","pat":"pt-bbbbbbbbbbbbbbbbbbbb"}
	]}`
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/import/qoder-pat",
		strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK       bool     `json:"ok"`
		Total    int      `json:"total"`
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("json: %s", w.Body.String())
	}
	if !resp.OK || resp.Total != 2 || resp.Imported != 2 || resp.Skipped != 0 {
		t.Fatalf("resp = %+v", resp)
	}

	// 落库核验：provider=qoder，id=email，label=email，cred.pat 正确。
	for _, want := range []struct{ email, pat string }{
		{"a@x.com", "pt-aaaaaaaaaaaaaaaaaaaa"},
		{"b@x.com", "pt-bbbbbbbbbbbbbbbbbbbb"},
	} {
		a := p.extManager().Find("qoder", want.email)
		if a == nil {
			t.Fatalf("账号 %s 未入库", want.email)
		}
		if a.Provider != extstore.PQoder || a.Label != want.email {
			t.Fatalf("%s 落库形态错: %+v", want.email, a)
		}
		var cred map[string]string
		if json.Unmarshal(a.Cred, &cred) != nil || cred["pat"] != want.pat {
			t.Fatalf("%s 凭据错: %s", want.email, a.Cred)
		}
	}
}

// 幂等（fork SELECT-then-UPSERT）：同 email 重复导入是更新，不报错、
// 不产生第二条账号，PAT 换成新的。
func TestImportQoderPATIdempotent(t *testing.T) {
	p := newQuotaTestPanel(t, nil, "")
	post := func(email, pat string) {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/import/qoder-pat",
			strings.NewReader(`{"accounts":[{"email":"`+email+`","pat":"`+pat+`"}]}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
		}
	}
	post("dup@x.com", "pt-old1111111111111111")
	post("dup@x.com", "pt-new2222222222222222")

	accounts := p.extManager().List()
	n := 0
	for _, a := range accounts {
		if a.ID == "dup@x.com" {
			n++
			var cred map[string]string
			json.Unmarshal(a.Cred, &cred)
			if cred["pat"] != "pt-new2222222222222222" {
				t.Fatalf("PAT 未更新: %s", a.Cred)
			}
		}
	}
	if n != 1 {
		t.Fatalf("同 email 应有且只有 1 条账号，实为 %d（幂等被破坏）", n)
	}
}

// 坏行不阻断好行（fork 逐条 try/except）：PAT 不带 pt-、缺 email、
// email 带怪字符三条各记一个 error，好行照常入库。
func TestImportQoderPATAutoSkipsBadRows(t *testing.T) {
	p := newQuotaTestPanel(t, nil, "")
	body := `{"accounts":[
		{"email":"bad1@x.com","pat":"sk-xxx"},
		{"email":"","pat":"pt-cccccccccccccccccccc"},
		{"email":"bad id/with slash@x.com","pat":"pt-dddddddddddddddddddd"},
		{"email":"good@x.com","pat":"pt-eeeeeeeeeeeeeeeeeeee"}
	]}`
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/import/qoder-pat",
		strings.NewReader(body)))
	var resp struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Imported != 1 || resp.Skipped != 3 {
		t.Fatalf("imported/skipped = %d/%d, want 1/3", resp.Imported, resp.Skipped)
	}
	if len(resp.Errors) != 3 {
		t.Fatalf("errors = %v, want 3 条（坏行逐条记）", resp.Errors)
	}
	if p.extManager().Find("qoder", "good@x.com") == nil {
		t.Fatal("好行应照常入库")
	}
	if p.extManager().Find("qoder", "bad1@x.com") != nil {
		t.Fatal("非 pt- 前缀不该入库")
	}
}

// 入参防御：空数组 400；非 JSON 400（与 cockpit 导入同口径）。
func TestImportQoderPATValidation(t *testing.T) {
	p := newQuotaTestPanel(t, nil, "")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/import/qoder-pat",
		strings.NewReader(`{"accounts":[]}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空数组 code = %d, want 400", w.Code)
	}
	w2 := httptest.NewRecorder()
	p.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/panel/api/import/qoder-pat",
		strings.NewReader("not json")))
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("非 JSON code = %d, want 400", w2.Code)
	}
}
