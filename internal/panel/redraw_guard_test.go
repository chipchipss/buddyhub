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
// 在子树整体重建后保持 800；#models→返回→#usage→返回→#overview；子状态段
// 切换 history.length 不增长（0），跨视图导航增长 1。
func TestRedrawGuardKept(t *testing.T) {
	kernel := readWebJS(t, "kernel.js")
	shell := readWebJS(t, "shell.js")

	for _, want := range []struct {
		what string
		want string
	}{
		{"effect 在换 DOM 前判交互护栏", "if (isEngaged()) { e.pending = out; pendingSwap.add(e); armFlush(); return out; }"},
		{"挂起队列排空即停表", "if (pendingSwap.size === 0) return;"},
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
		{"跨视图导航留历史", "writeHash('#' + id, true);"},
		{"子状态段不新增历史", "writeHash('#' + id, false);"},
		{"写地址栏的唯一出口用 pushState", "if (push && history.pushState) history.pushState(null, '', target);"},
		{"返回/前进有监听", "addEventListener('popstate'"},
		{"后台标签页停止轮询", "if (document.hidden) return;"},
	} {
		if !strings.Contains(shell, want.want) {
			t.Errorf("shell.js 缺少「%s」断言目标：%q", want.want, want.want)
		}
	}

	// setHashSeg 也必须走同一个出口，否则它就成了第二处 replaceState 漏网之鱼。
	if seg := sliceAround(shell, "export function setHashSeg", "}\n"); seg == "" || !strings.Contains(seg, "writeHash(") {
		t.Error("shell.js setHashSeg 未复用 writeHash 出口")
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
