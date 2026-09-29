// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）。源文件按模块拆分：
//
//	assets/css/*.css  设计令牌 / 组件 / 视图样式
//	assets/js/*.js    按功能域拆分的前端模块（经典脚本共享全局词法作用域）
//
// 服务时按显式顺序（cssOrder / jsOrder）拼接为两个入口：
//   - /panel/app.css  <link> 引入
//   - /panel/app.js   <script src> 引入（严格 CSP 下 script-src 'self' 可用）
//
// 拼接而非多标签的好处：请求数最少、执行顺序由一处显式定义，测试与运行时共用
// 同一顺序不会漂移。拼接结果进程内缓存：静态资源，生命周期内不变。
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed assets
var assetsFS embed.FS

// cssOrder 样式拼接顺序（base → 组件 → 视图，后加载可覆盖先加载）。
var cssOrder = []string{"base.css", "components.css", "views.css"}

// jsOrder 脚本拼接顺序：核心工具 → 格式化 → QR → 各功能域 → 启动引导。
// 99-main.js 必须最后（顶层 start() 依赖此前定义的全部函数）。
var jsOrder = []string{
	"10-core.js",
	"15-format.js",
	"20-qr.js",
	"30-accounts.js",
	"35-models.js",
	"40-usage.js",
	"45-packages.js",
	"50-tasks.js",
	"55-loomy.js",
	"60-ext.js",
	"65-apikeys.js",
	"70-config.js",
	"75-logs.js",
	"80-addaccount.js",
	"99-main.js",
}

// concatDir 按顺序读取 dir 下文件拼接（UTF-8 文本；各自以换行收尾保证拼接后语法独立）。
func concatDir(dir string, names []string) []byte {
	var b strings.Builder
	for _, n := range names {
		data, err := assetsFS.ReadFile(dir + n)
		if err != nil {
			continue // embed 集合内文件缺失属编程错误，测试会兜住；此处不炸服务
		}
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// csp 内容安全策略（严格版，无需 unsafe-inline 的脚本）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	p.assetsOnce.Do(p.buildAssets)
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(p.assetJS)
}

// appStyle 输出样式（同源样式表，供 CSP style-src 'self' 加载）。
func (p *Panel) appStyle(w http.ResponseWriter, r *http.Request) {
	p.assetsOnce.Do(p.buildAssets)
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(p.assetCSS)
}

func (p *Panel) buildAssets() {
	p.assetJS = concatDir("assets/js/", jsOrder)
	p.assetCSS = concatDir("assets/css/", cssOrder)
}
