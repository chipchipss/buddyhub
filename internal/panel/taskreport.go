// taskreport.go 排程台账的面板读面：每一轮任务「谁成了、谁为什么没成、下一次
// 什么时候跑」。调度器只管执行，把结论交给这里统一渲染成一份快照。
package panel

import (
	"net/http"
)

// taskReport GET /panel/api/task_report —— 最近几轮台账 + 待补跑链 + 下一槽位。
func (p *Panel) taskReport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "调度器未启用")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Scheduler.Report())
}
