package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extstore"
)

// 这两条链路的完整协议行为在 internal/extprovider/autoclaw 里用 mock 上游覆盖。
// 这里只测面板这一层的**参数校验**与入库接线——它们不需要网络，也不该打网络。

func TestAutoClawSendCodeRejectsEmptyPhone(t *testing.T) {
	p := loginTestPanel(t)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/autoclaw/send_code",
		strings.NewReader(`{"phone":"  "}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空手机号应 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "手机号") {
		t.Fatalf("错误信息不明确: %s", rec.Body.String())
	}
}

func TestAutoClawLoginRejectsEmptyPhone(t *testing.T) {
	p := loginTestPanel(t)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/autoclaw/login",
		strings.NewReader(`{"phone":"","code":"123456"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空手机号应 400，得到 %d", rec.Code)
	}
}

func TestAutoClawLoginRejectsBadCodeLength(t *testing.T) {
	p := loginTestPanel(t)
	// 非 6 位验证码本地就拦掉，不打上游
	req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/autoclaw/login",
		strings.NewReader(`{"phone":"13800000000","code":"12","device_id":"d"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非 6 位验证码应 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "6 位") {
		t.Fatalf("错误信息不明确: %s", rec.Body.String())
	}
}

func TestAutoClawBadJSON(t *testing.T) {
	p := loginTestPanel(t)
	for _, path := range []string{"send_code", "login"} {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/autoclaw/"+path,
			strings.NewReader("{not json"))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 坏 JSON 应 400，得到 %d", path, rec.Code)
		}
	}
}

// 手工添加白名单必须认 autoclaw（与短信登录走同一条入库路径）。
func TestAutoClawManualAddAllowed(t *testing.T) {
	p := loginTestPanel(t)
	body := `{"provider":"autoclaw","id":"u-1","cred":{"token":"t","region":"cn"}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/ext/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("autoclaw 手工添加应被接受，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := p.extManager().Find(extstore.PAutoClaw, "u-1"); got == nil {
		t.Fatal("账号未入库")
	}
}
