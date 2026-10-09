// config_test.go 配置页接口的两条新约定：默认值与「需重启」清单由后端一处给出，
// 以及「立即重启」端点在没挂重启钩子时明确 501（面板据此不出现那颗按钮）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigGetCarriesDefaultsAndRestartFields(t *testing.T) {
	p := New(Config{
		Version: "test",
		LoadConfig: func() (any, error) {
			return map[string]any{"listen": ":7863"}, nil
		},
		ConfigDefaults: func() any {
			return map[string]any{"listen": ":7863", "upstream": map[string]any{"timeout_seconds": 120}}
		},
		RestartFields: func() []string { return []string{"listen", "upstream.timeout_seconds"} },
	})

	rec := httptest.NewRecorder()
	p.getConfig(rec, httptest.NewRequest(http.MethodGet, "/panel/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d 正文 %s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK            bool           `json:"ok"`
		Config        map[string]any `json:"config"`
		Defaults      map[string]any `json:"defaults"`
		RestartFields []string       `json:"restart_fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 前端要靠 defaults 做「恢复默认」、靠 restart_fields 在按下保存之前就说
	// 「其中几项要重启」；少任何一个，配置页就只能瞎猜或者干脆不说。
	if got.Defaults == nil {
		t.Fatalf("defaults 缺失：%s", rec.Body.String())
	}
	if len(got.RestartFields) != 2 {
		t.Fatalf("restart_fields %v，期望 2 项", got.RestartFields)
	}
}

// 没注入时也必须有形状：前端 dig 一个不存在的 defaults 会整页报错。
func TestConfigGetWithoutHooksIsWellFormed(t *testing.T) {
	p := New(Config{Version: "test", LoadConfig: func() (any, error) { return map[string]any{}, nil }})
	rec := httptest.NewRecorder()
	p.getConfig(rec, httptest.NewRequest(http.MethodGet, "/panel/api/config", nil))
	var got struct {
		Defaults      any      `json:"defaults"`
		RestartFields []string `json:"restart_fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RestartFields == nil {
		t.Fatalf("restart_fields 应是空数组而不是 null：%s", rec.Body.String())
	}
}

func TestRestartWithoutHookIs501(t *testing.T) {
	p := New(Config{Version: "test"})
	rec := httptest.NewRecorder()
	p.restartProcess(rec, httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 %d，期望 501（没挂重启钩子时不该装作能重启）", rec.Code)
	}
}

func TestRestartCallsHookAfterAck(t *testing.T) {
	done := make(chan struct{})
	p := New(Config{Version: "test", Restart: func() error { close(done); return nil }})

	rec := httptest.NewRecorder()
	p.restartProcess(rec, httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil))
	// 回执必须先写出去：进程马上就没了，这条连接必然断，客户端不能等结果。
	var ack map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || ack["restarting"] != true {
		t.Fatalf("回执不对：%d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("重启钩子没被调用")
	}
}
