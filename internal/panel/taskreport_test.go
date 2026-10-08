// taskreport_test.go 台账接口：没挂调度器时明确 501（面板据此显示「排程未启用」），
// 挂了则一轮的结论、待补跑、下一槽位都要能拿到。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chipchipss/buddyhub/internal/scheduler"
)

func TestTaskReportWithoutSchedulerIs501(t *testing.T) {
	p := New(Config{Version: "test"})
	rec := httptest.NewRecorder()
	p.taskReport(rec, httptest.NewRequest(http.MethodGet, "/panel/api/task_report", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 %d，期望 501（调度器未挂载时不该空转返回空台账）", rec.Code)
	}
}

func TestTaskReportServesRoundLedger(t *testing.T) {
	sch := scheduler.New(scheduler.Config{})
	p := New(Config{Version: "test", Scheduler: sch})

	rec := httptest.NewRecorder()
	p.taskReport(rec, httptest.NewRequest(http.MethodGet, "/panel/api/task_report", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d 正文 %s", rec.Code, rec.Body.String())
	}
	var v scheduler.ReportView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("台账不是合法 JSON: %v %s", err, rec.Body.String())
	}
	// 尚未派发过：空台账是正常态，但下一槽位必须有（面板靠它显示「下一次 09:00」）。
	if len(v.Rounds) != 0 {
		t.Fatalf("新调度器不该有历史轮次: %+v", v.Rounds)
	}
	if v.NextFire == "" || len(v.NextKinds) == 0 {
		t.Fatalf("缺下一次排程信息: %+v", v)
	}
}

// 路由必须挂在鉴权后面：台账里带账号 uid 与失败原因，不是可以匿名读的东西。
func TestTaskReportRouteRequiresAuth(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/task_report", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读台账返回 %d，期望 401", rec.Code)
	}
}
