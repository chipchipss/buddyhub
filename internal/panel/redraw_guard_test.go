package panel

import (
	"io/fs"
	"strings"
	"testing"
)

// TestRedrawGuardKept 前端重绘护栏与返回键必须留在源码里。
//
// 为什么需要：这两处都属于「删掉照样编译、照样跑、Go 测试全绿，用户却每天
// 踩」的那类不变量——
//   - effect 换 DOM 前若不判 isEngaged()，5 秒轮询就会在用户滚动/输入/选中/
//     开抽屉时重建整棵子树（滚动跳、悬停闪、选区丢、下拉关）；
//   - navigate() 若退回 history.replaceState，跨视图导航不留历史，浏览器返回键
//     直接退出面板。
// 所以这里断言的是**调用点**而不是关键字：护栏必须挂在这次替换那条分支上，
// pushState 必须用在跨视图导航上。改坏任一条，本测试变红。
//
// 行为级证据（非本测试职责，2026-10-09 真实浏览器实测记录）：焦点在输入框时
// 信号变化后节点仍是旧产物、失焦后落地新值；文字选区跨轮询存活；scrollTop
// 在子树整体重建后保持 800；#accounts?tab=zai→返回→#home 能退回上一页；页内
// 芯片筛选/子页签切换 history.length 不增长（0），跨页与换分段各增长 1。
func TestRedrawGuardKept(t *testing.T) {
	kernel := readWebJS(t, "kernel.js")
	shell := readWebJS(t, "shell.js")

	for _, want := range []struct {
		what string
		want string
	}{
		{"effect 在换 DOM 前判交互护栏", "if (isEngaged()) { e.pending = out; pendingSwap.add(e); armFlush(); return out; }"},
		{"挂起队列排空即停表", "if (pendingSwap.size === 0) { stopFlush(); return; }"},
		{"停表是真的 clearInterval", "if (flushTimer) { clearInterval(flushTimer); flushTimer = 0; }"},
		{"换 DOM 时保持滚动位置", "const snap = captureScroll(e.node)"},
		{"换 DOM 时保持焦点与选区", "const fsnap = captureFocus(e.node)"},
		{"护栏含抽屉未关判据", "if (drawerState !== 'closed') return true;"},
		{"护栏含输入控件聚焦判据", "/^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName)"},
		{"护栏含文字选区判据", "!sel.isCollapsed"},
		{"挂起队列有补落地轮询", "for (const e of [...pendingSwap])"},
		{"滚动本身计入护栏", "lastScrollAt = typeof performance"},
	} {
		if !strings.Contains(kernel, want.want) {
			t.Errorf("kernel.js 缺少「%s」断言目标：%q", want.what, want.want)
		}
	}

	for _, want := range []struct {
		what string
		want string
	}{
		{"跨页/跨分段导航才留历史", "const push = cur.page !== page"},
		{"导航写地址栏走统一出口", "writeHash(hashFor(page,"},
		{"写地址栏的唯一出口用 pushState", "if (push && history.pushState) history.pushState(null, '', target);"},
		{"返回/前进有监听", "addEventListener('popstate'"},
		{"后台标签页停止轮询", "if (document.hidden) return;"},
	} {
		if !strings.Contains(shell, want.want) {
			t.Errorf("shell.js 缺少「%s」断言目标：%q", want.what, want.want)
		}
	}

	// 页内子状态（?tab=）必须 replaceState：切筛选条件不该往历史里堆几十条，
	// 否则返回键要按几十下才退得出一页。
	for _, fn := range []string{"export function setHashTab", "export function patchHash"} {
		seg := sliceAround(shell, fn, "}\n")
		if seg == "" {
			t.Errorf("shell.js 找不到 %s（页内子状态的写入口）", fn)
			continue
		}
		if !strings.Contains(seg, "writeHash(") || !strings.Contains(seg, ", false)") {
			t.Errorf("shell.js %s 未以 replace 方式复用 writeHash 出口", fn)
		}
	}
}

func TestInPlaceRepaintKeepsMountedTree(t *testing.T) {
	kernel := readWebJS(t, "kernel.js")
	shell := readWebJS(t, "shell.js")
	accounts := readWebJS(t, "views/accounts.js")

	// 1) 内核：视图返回同一个节点就不许再换（换会把已上屏的子树搬进未上屏的新包装）
	if !strings.Contains(kernel, "if (out === e.node) return out;") {
		t.Error(`kernel.js 缺少「视图复用同一棵树时跳过替换」：if (out === e.node) return out;`)
	}
	// 2) 路由：同一路由的重跑必须复用包装层
	if !strings.Contains(shell, "if (key === routeKey && routeCache && body === routeBody && routeCache.isConnected) return routeCache;") {
		t.Error("shell.js 的 renderRoute() 不再复用已上屏的包装层（轮询会换整棵树）")
	}
	// 3) 账号页：骨架复用的那一句
	if !strings.Contains(accounts, "if (rootEl && rootEl.isConnected) { paint(); return rootEl; }") {
		t.Error("views/accounts.js 不再复用骨架（rootEl），数据轮询会换掉整页并吃掉用户交互")
	}
	// 4) 反模式：paint 拿 isConnected 当闸门——挂起期间它必然为假，于是每次交互都画进
	//    一棵没上屏的树里（2026-10-09 实测的「点什么都没反应」）。不许复活。
	if strings.Contains(accounts, "if (!listHost || !listHost.isConnected) return;") {
		t.Error("views/accounts.js 里 paint() 又用 isConnected 拒绝重画了（这正是交互被吃掉的原因）")
	}
	// 5) 首帧必须在返回前画好：护栏什么时候落地不由视图决定，不能等微任务
	if !strings.Contains(accounts, "if (rootEl && rootEl.isConnected) { paint(); return rootEl; }") ||
		!strings.Contains(accounts, "\n    paint();\n    queueMicrotask(deepLink);") {
		t.Error("views/accounts.js 首帧没有在 return 前同步画好（挂起的新树落地时会是空白）")
	}
	// 6) null 不许直接交给 replaceChildren：真实 DOM 会渲染出字面 "null"
	if strings.Contains(kernel, "footer ? h('footer', null, footer) : null,") {
		t.Error("kernel.js 的 openDrawer 又把 null 交给 replaceChildren（会渲染出 \"null\" 文本）")
	}
	if !strings.Contains(kernel, "...(footer ? [h('footer', null, footer)] : []),") {
		t.Error("kernel.js 的 openDrawer 缺少「无 footer 就不产出子节点」的写法")
	}
	// 搜索框是这一页唯一不该被重绘碰的节点：它一旦进入重绘范围，焦点就会丢
	if strings.Contains(accounts, "listHost.replaceChildren") {
		t.Error("views/accounts.js 还在整块替换 listHost（搜索框会被一起重建，焦点丢失）")
	}
}

func readWebJS(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(webFS, "web/js/"+name)
	if err != nil {
		t.Fatalf("读取 web/js/%s: %v", name, err)
	}
	return string(data)
}

// sliceAround 取 from 起、到首个 until 之前的片段（找不到返回空串）。
func sliceAround(s, from, until string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	rest := s[i:]
	j := strings.Index(rest[len(from):], until)
	if j < 0 {
		return rest
	}
	return rest[:len(from)+j]
}
