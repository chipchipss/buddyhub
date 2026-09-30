// index.go 面板静态资源与安全响应头。
//
// 前端是**原生 ES 模块**应用，随二进制内嵌（go:embed），无构建步骤：
//
//	web/index.html          壳层（唯一的 <script type="module"> 入口）
//	web/css/*.css           设计令牌 / 玻璃组件 / 视图样式
//	web/js/*.js             内核 / 壳层 / 视图 / 抽屉（真模块，import 关系即依赖）
//
// 服务方式：/panel/ 出页面，/panel/<相对路径> 出文件（按扩展名定 Content-Type）。
// 模块化换来的是清晰的依赖边界与可维护性；代价是若干次本地请求——对局域网面板
// 而言可忽略，且浏览器会缓存。
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed web
var webFS embed.FS

// csp 内容安全策略（严格版，无需 unsafe-inline 的脚本）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（ES 模块同样受此约束）；页面无内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：少数元素带 style="..." 属性（进度宽度、图表尺寸），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标 / 内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'           禁止注入 <base> 改写相对路径
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

// assetTypes 静态资源的 Content-Type 白名单（不在表内一律 404，避免意外暴露）。
var assetTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".json": "application/json; charset=utf-8",
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "panel assets missing", http.StatusInternalServerError)
		return
	}
	setSecurityHeaders(w)
	writeAsset(w, r, "text/html; charset=utf-8", data)
}

// asset 输出前端模块/样式（同源资源，供 CSP script-src/style-src 'self' 加载）。
// 路径经 path.Clean 归一后仅允许 web/ 下的白名单扩展名，杜绝目录穿越。
func (p *Panel) asset(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/panel/")
	clean := path.Clean("/" + rel) // 归一：吃掉 ../ 与重复斜杠
	if clean == "/" || strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(path.Ext(clean))
	ctype, ok := assetTypes[ext]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(webFS, "web"+clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setSecurityHeaders(w)
	writeAsset(w, r, ctype, data)
}

// writeAsset 输出内嵌静态资源，带**内容 ETag**。
//
// 为什么需要：面板前端是 go:embed 进二进制的，升级二进制后文件名不变
// （ext-add.js 还是 ext-add.js）。只给 `Cache-Control: no-cache` 而没有校验器时，
// 浏览器**没有依据判断内容变没变**，升级后可能继续跑旧 JS——表现是「新功能
// 点了没反应」或「已修好的 bug 还在」，而且从服务端完全看不出问题。
//
// 有了 ETag：浏览器每次带 If-None-Match 回来，内容没变回 304（省流量），
// 变了回 200 + 新内容。升级即时生效。
func writeAsset(w http.ResponseWriter, r *http.Request, ctype string, data []byte) {
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
