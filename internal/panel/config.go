// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"io"
	"log"
	"net/http"
	"time"
)

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": cfg,
		// 默认值与「哪些字段改了要重启」都由 main 注入：配置页要能在按下保存之前
		// 就报出「3 项改动，其中 1 项需重启」，也能逐项「恢复默认」。
		"defaults":       defaultsOf(p),
		"restart_fields": restartFieldsOf(p),
	})
}

func defaultsOf(p *Panel) any {
	if p.cfg.ConfigDefaults == nil {
		return nil
	}
	return p.cfg.ConfigDefaults()
}

func restartFieldsOf(p *Panel) []string {
	if p.cfg.RestartFields == nil {
		return []string{}
	}
	out := p.cfg.RestartFields()
	if out == nil {
		return []string{}
	}
	return out
}

// restartProcess 重启网关进程，让装配期定死的配置项生效。
//
// 先把回执写出去再动手：进程马上就没了，这条连接必然断，客户端不能等结果。
func (p *Panel) restartProcess(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Restart == nil {
		writeErr(w, http.StatusNotImplemented, "restart api not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true})
	log.Printf("panel: 收到重启指令（来自配置页），进程即将退出")
	go func() {
		time.Sleep(150 * time.Millisecond) // 让上面那个回执落进 socket
		if err := p.cfg.Restart(); err != nil {
			log.Printf("panel: 重启失败：%v（配置已保存，需手工重启进程）", err)
		}
	}()
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
