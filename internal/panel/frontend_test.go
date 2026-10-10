package panel

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// copyModules 把内嵌的前端模块树复制到临时目录，并写入 package.json
// （{"type":"module"}）——Node 据此把 .js 当 ES 模块解析，从而能用原生
// 语法检查与动态 import 验证前端，而不引入任何构建步骤。
func copyModules(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	err := fs.WalkDir(webFS, "web/js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := fs.ReadFile(webFS, p)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "web/js/")))
		if merr := os.MkdirAll(filepath.Dir(out), 0o755); merr != nil {
			return merr
		}
		return os.WriteFile(out, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestModuleSyntax 每个前端模块都必须通过 JS 解析器语法校验。
//
// 为什么需要：模块是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次语法错误就让整个面板白屏，而所有 Go 测试依然全绿。无 node 时跳过。
func TestModuleSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	dir := copyModules(t)
	var checked int
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		checked++
		out, cerr := exec.Command(node, "--check", p).CombinedOutput()
		if cerr != nil {
			t.Fatalf("%s 语法错误:\n%s", filepath.Base(p), out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 8 {
		t.Fatalf("只检查到 %d 个模块，模块树疑似不完整", checked)
	}
}

// TestModuleTopLevelSmoke 顶层求值冒烟：用 DOM 桩真实 import 入口模块，
// 抓 TDZ / ReferenceError / 循环依赖一类的运行时错误——语法检查对此全盲。
// 无 node 时跳过。
func TestModuleTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	dir := copyModules(t)
	harness := `const inert = () => new Proxy(function () {}, {
  get(t, k) {
    if (k === Symbol.toPrimitive) return () => '';
    if (k === 'nodeType') return 1;
    if (k === 'value' || k === 'textContent') return '';
    if (k === 'children' || k === 'childNodes' || k === 'files') return [];
    if (k === 'classList' || k === 'style' || k === 'dataset') return inert();
    if (k === 'parentNode') return null;
    return inert();
  },
  set() { return true; },
  apply() { return inert(); },
  construct() { return inert(); },
  has() { return true; },
});
const doc = {
  createElement: () => inert(),
  createElementNS: () => inert(),
  createDocumentFragment: () => inert(),
  createComment: () => inert(),
  createTextNode: () => inert(),
  getElementById: () => inert(),
  querySelector: () => inert(),
  querySelectorAll: () => [],
  addEventListener() {}, removeEventListener() {},
  documentElement: inert(), head: inert(), body: inert(),
  cookie: '',
};
globalThis.document = doc;
globalThis.Node = class Node {};
globalThis.window = globalThis;
globalThis.location = { hash: process.env.SMOKE_HASH || '#overview', search: '' };
globalThis.history = { replaceState() {} };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
Object.defineProperty(globalThis, 'navigator', {
  value: { clipboard: { writeText: () => Promise.resolve() } },
  configurable: true, writable: true,
});
globalThis.matchMedia = () => ({ matches: false, addEventListener() {} });
globalThis.fetch = () => new Promise(() => {});
globalThis.requestAnimationFrame = () => 0;
globalThis.addEventListener = () => {};
globalThis.removeEventListener = () => {};
globalThis.isSecureContext = false;
await import('./boot.js');
console.log('SMOKE OK');
process.exit(0);
`
	hf := filepath.Join(dir, "smoke.mjs")
	if err := os.WriteFile(hf, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	// 新 IA（五个一级页 + 页内分段）与旧 hash（README/收藏夹深链）都要能起得来：
	// 入口模块顶层求值会 import 全部视图，路由解析错就整屏白。
	for _, hash := range []string{
		"#home", "#accounts", "#accounts?tab=zai", "#accounts?acct=workbuddy:abc",
		"#automation?seg=tasks", "#automation?seg=logs",
		"#gateway?seg=stats", "#gateway?seg=keys", "#gateway?seg=models",
		"#settings?seg=config",
		"#overview", "#usage", "#tasks", "#tasks?seg=loomy", "#models", "#keys", "#config", "#logs",
	} {
		cmd := exec.Command(node, hf)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("入口模块顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("冒烟 %s 未通过:\n%s", hash, out)
		}
	}
}

// domStub 是**真实** DOM 桩（不是 Proxy 黑洞）：够 h() 把树建出来，再把类名、
// 按钮文案、input 数量读回来。语法检查与顶层求值冒烟都看不见「画出来的树长
// 什么样」——只有回读结构才能断言界面真的对。渲染类测试共用它。
//
// 桩的关键取舍：className 是类名的唯一真源。kernel 的 applyProps 走
// el.className = v，SVG 走 setAttribute('class', v)，classList 走 add/remove/
// toggle——三条路必须汇到同一个字段，否则 findByClass 读到两套状态，测试全绿
// 而浏览器里的类名是错的。
const domStub = `class El {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.props = {};
    this.style = {}; this.dataset = {}; this.listeners = {}; this._text = '';
    this.isConnected = true; this._class = ''; this.scrollTop = 0; this.disabled = false; }
  get className() { return this._class; }
  set className(v) { this._class = String(v == null ? '' : v); this.props.class = this._class; }
  _cs() { return new Set(this._class.split(' ').filter(Boolean)); }
  get classList() { const self = this; return {
    add: (...c) => { const s = self._cs(); for (const x of c) s.add(x); self.className = [...s].join(' '); },
    remove: (...c) => { const s = self._cs(); for (const x of c) s.delete(x); self.className = [...s].join(' '); },
    toggle: (c, on) => { const s = self._cs(); const v = on === undefined ? !s.has(c) : !!on;
      v ? s.add(c) : s.delete(c); self.className = [...s].join(' '); return v; },
    contains: c => self._cs().has(c),
  }; }
  setAttribute(k, v) { if (k === 'class') { this.className = v; return; } this.props[k] = String(v);
    // disabled 在真实 DOM 是布尔属性：setAttribute('disabled','') 之后 el.disabled
    // 立刻为 true（按钮点不动）。桩不镜像这一条，「转圈时禁用」就永远测不出来。
    if (k === 'disabled') this.disabled = true;
    // value / checked 同理：真实 DOM 用属性设过初值后，读 el.value / el.checked
    // 就是那个值。h() 走的全是 setAttribute，桩不接这条，表单类界面里每个输入框
    // 都会读成空串——「改动计数」「恢复默认」这类逻辑就全在假数据上跑。
    if (k === 'value') this.value = String(v);
    if (k === 'checked') this.checked = true; }
  getAttribute(k) { return this.props[k] ?? null; }
  removeAttribute(k) { delete this.props[k]; if (k === 'disabled') this.disabled = false; }
  setAttributeNS(a, k, v) { this.setAttribute(k, v); }
  addEventListener(t, f) { (this.listeners[t] ||= []).push(f); }
  removeEventListener() {}
  // 真实 DOM：节点一旦离开文档，整棵子树的 isConnected 都是 false。抽屉换内容
  // （openAccountDetail 覆盖向导那一屏）就是这样把上一屏判成「已废弃」的——
  // 只标自己等于让桩里的废弃表单永远「还在」，视图复用判定测不出来。
  appendChild(c) { this.children.push(c); c.parentNode = this; setConn(c, this.isConnected); return c; }
  // 真实 DOM 的 append/replaceChildren 只接受 Node 或字符串：传 null 不会报错，
  // 而是渲染出一个字面的 "null" 文本节点（2026-10-09 浏览器实测踩过）。
  // 桩以前把 null 悄悄滤掉，于是这类缺陷在测试里永远是绿的——现在照浏览器画。
  _node(c) { return (c && c.tagName) ? c : globalThis.document.createTextNode(String(c)); }
  append(...cs) { for (const c of cs) this.appendChild(this._node(c)); }
  prepend(...cs) { for (const c of cs.map(x => this._node(x))) { this.children.unshift(c); c.parentNode = this; setConn(c, this.isConnected); } }
  before() {} after() {}
  remove() { setConn(this, false); const p = this.parentNode;
    if (p) { const i = p.children.indexOf(this); if (i >= 0) p.children.splice(i, 1); } }
  replaceWith(n) { const p = this.parentNode; if (!p) return;
    const i = p.children.indexOf(this); if (i >= 0) { p.children[i] = n; n.parentNode = p; setConn(n, p.isConnected); }
    setConn(this, false); }
  insertBefore(c) { return this.appendChild(c); }
  // 被换下去的节点要整棵断开（视图靠 isConnected 拒绝写进废弃容器，抽屉的 loader
  // 与向导的「宿主还在不在屏上」判定也靠它）；新上来的继承父节点的连通性。
  replaceChildren(...cs) { const next = cs.map(c => this._node(c));
    for (const old of this.children) if (!next.includes(old)) setConn(old, false);
    this.children = next; for (const c of this.children) { c.parentNode = this; setConn(c, this.isConnected); } }
  focus() {}
  scrollIntoView() {}   // 配置页左侧目录跳位要用；桩里点它等于什么都不做
  get firstChild() { return this.children[0] ?? null; }
  get lastChild() { return this.children[this.children.length - 1] ?? null; }
  get firstElementChild() { return this.children.find(c => !String(c.tagName).startsWith('#')) ?? null; }
  get childElementCount() { return this.children.filter(c => !String(c.tagName).startsWith('#')).length; }
  get textContent() { return this._text || this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get value() {
    // select 的 .value 在真实 DOM 里跟着选中项走（h() 只设 selected 属性，不设 value），
    // 不镜像这条的话下拉框读回来永远是空串，配置页会把每个下拉都当成「已改动」。
    if (this.tagName === 'SELECT') {
      const on = (this.children || []).find(c => c.props && c.props.selected !== undefined);
      return on ? String(on.props.value ?? on.textContent) : '';
    }
    return this._value ?? '';
  }
  set value(v) { this._value = v; }
  getBoundingClientRect() { return { top: 20, left: 20, right: 60, bottom: 40, width: 40, height: 20, x: 20, y: 20 }; }
  querySelector(sel) { return qsel(this, sel); }
  querySelectorAll(sel) { const out = []; qselAll(this, sel, out); return out; }
  contains(n) { for (const c of this.children) if (c === n || (c.contains && c.contains(n))) return true; return false; }
  closest() { return null; }
}
// 选择器只实现面板真正用到的形态：空格分隔的后代链，每段可叠加 .class / #id / tag
function qmatch(el, part) {
  const toks = String(part).match(/[.#]?[-\w]+/g) || [];
  for (const tok of toks) {
    if (tok === '*') continue;
    if (tok.startsWith('.')) {
      if (!String(el.className || (el.props && el.props.class) || '').split(' ').includes(tok.slice(1))) return false;
    } else if (tok.startsWith('#')) {
      if (!(el.props && el.props.id === tok.slice(1))) return false;
    } else if ((el.tagName || '').toLowerCase() !== tok.toLowerCase()) return false;
  }
  return true;
}
// 后代（不含自身）里所有命中 part 的节点，文档序
function qselAll(el, part, out) {
  for (const c of el.children || []) {
    if (qmatch(c, part)) out.push(c);
    qselAll(c, part, out);
  }
  return out;
}
function qsel(root, sel) {
  let cur = [root];
  for (const part of String(sel).trim().split(/\s+/)) {
    const next = [];
    for (const el of cur) qselAll(el, part, next);
    cur = next;
    if (!cur.length) return null;
  }
  return cur[0] || null;
}
globalThis.Node = El; globalThis.Element = El; globalThis.HTMLElement = El;
// 真实 DOM：节点离开文档，整棵子树的 isConnected 都是 false；重新挂回文档，
// 子树一起回来。视图（抽屉的 loader、向导的「宿主还在不在屏上」判定）全靠这条
// 拒绝往废弃容器里写——桩只标直接子节点的话，那些守卫在测试里永远为真，
// 「切走再切回来还是同一棵树」这类不变量就根本测不到。
function setConn(n, v) {
  if (!n || typeof n !== 'object') return;
  n.isConnected = v;
  for (const c of n.children || []) setConn(c, v);
}
// html/head/body 真的连起来：抽屉、菜单、toast 都 append 到 body，
// 而 loadTasks 这类「按选择器回找自己那一点」的写法必须能找到才测得准。
const htmlEl = new El('html'), headEl = new El('head'), bodyEl = new El('body');
htmlEl.append(headEl, bodyEl);
setConn(htmlEl, true);   // 真实 DOM：documentElement 一造出来就连着文档
globalThis.document = {
  createElement: t => new El(t), createElementNS: (ns, t) => new El(t),
  createDocumentFragment: () => new El('#fragment'),
  createComment: t => { const e = new El('#comment'); e._text = t; return e; },
  createTextNode: t => { const e = new El('#text'); e._text = t; return e; },
  getElementById: id => qsel(htmlEl, '#' + id),
  querySelector: sel => qsel(htmlEl, sel),
  querySelectorAll: sel => qselAll(htmlEl, sel, []),
  addEventListener() {}, removeEventListener() {},
  documentElement: htmlEl, head: headEl, body: bodyEl, cookie: '',
  activeElement: null, hidden: false,
};
globalThis.window = { addEventListener() {}, removeEventListener() {}, innerWidth: 1280, innerHeight: 800,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  open() {}, location: { hash: '#accounts', href: '' } };
globalThis.location = globalThis.window.location;
// pushState/replaceState 真的改地址栏：路由的深链、页内子状态都靠它，
// 桩里不动 hash 就等于把整条路由层测了个寂寞。
const setHash = t => { if (t != null) globalThis.location.hash = String(t); };
globalThis.history = { pushState: (a, b, t) => setHash(t), replaceState: (a, b, t) => setHash(t) };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
Object.defineProperty(globalThis, 'navigator',
  { value: { clipboard: { writeText: () => Promise.resolve() } }, configurable: true, writable: true });
globalThis.matchMedia = globalThis.window.matchMedia;
globalThis.addEventListener = () => {};
globalThis.removeEventListener = () => {};
globalThis.fetch = () => new Promise(() => {});
globalThis.requestAnimationFrame = () => 0;
globalThis.isSecureContext = false;

// 递归收集 input 数量与按钮文案，用于断言
function walk(el, out = { inputs: 0, buttons: [] }) {
  if (!el || typeof el !== 'object') return out;
  const tag = (el.tagName || '').toLowerCase();
  if (tag === 'input') out.inputs++;
  if (tag === 'button') out.buttons.push(el.textContent.trim());
  for (const c of el.children || []) walk(c, out);
  return out;
}
const has = (btns, kw) => btns.some(b => b.includes(kw));

function findButton(el, kw) {
  if (!el || typeof el !== 'object') return null;
  if ((el.tagName || '').toLowerCase() === 'button' && el.textContent.includes(kw)) return el;
  for (const c of el.children || []) { const r = findButton(c, kw); if (r) return r; }
  return null;
}
function findByClass(el, cls) {
  if (!el || typeof el !== 'object') return null;
  // applyProps 对普通元素写 className，对 SVG 走 setAttribute('class')
  const c = el.className || (el.props && el.props.class) || '';
  if (String(c).split(' ').includes(cls)) return el;
  for (const c2 of el.children || []) { const r = findByClass(c2, cls); if (r) return r; }
  return null;
}
// 收集某一类名的全部节点
function findAll(el, cls, out = []) {
  if (!el || typeof el !== 'object') return out;
  const c = el.className || (el.props && el.props.class) || '';
  if (String(c).split(' ').includes(cls)) out.push(el);
  for (const k of el.children || []) findAll(k, cls, out);
  return out;
}
// 触发一次绑定：h(..., {onclick}) 走 addEventListener，btn.onclick= 是属性赋值
const fire = (el, type, ev) => { const f = el['on' + type] || (el.listeners[type] || [])[0];
  if (typeof f !== 'function') throw new Error('没绑定 ' + type + ' 事件');
  return f(Object.assign({ currentTarget: el, target: el, preventDefault() {}, stopPropagation() {} }, ev || {})); };
`

// wizardBoot 是「添加账号向导」两个渲染测试共用的脚手架：真实注册表 + fetch 桩
// + 只走公开入口（openAddAccount 与真实点击）的导航助手。
//
// 平台清单直接序列化后端 platforms.go 那张表，测试里不手抄第二份——手抄一份就
// 等于「加平台漏改测试」，而漏改的测试比没有测试更危险（它照样绿）。
func wizardBoot(t *testing.T) string {
	t.Helper()
	reg, err := json.Marshal(platforms)
	if err != nil {
		t.Fatal(err)
	}
	return `const problems = [];
const bad = m => problems.push(m);
` + domStub + `
// 后端注册表是平台身份的唯一事实源（js/platforms.js 从 GET /panel/api/platforms 拿）。
const PLATFORMS = ` + string(reg) + `;

// fetch 桩：按 URL 片段给响应（DATA 由各场景自己填），并记下调用过哪些路径。
const CALLS = [];
const DATA = {};
globalThis.fetch = async (url, opts) => {
  const u = String(url);
  CALLS.push(u);
  if (u.includes('/api/platforms')) {
    return { ok: true, status: 200, json: async () => ({ ok: true, platforms: PLATFORMS }) };
  }
  for (const frag of Object.keys(DATA)) {
    if (u.includes(frag)) {
      const v = DATA[frag];
      return { ok: true, status: 200, json: async () => (typeof v === 'function' ? v(u, opts) : v) };
    }
  }
  return { ok: true, status: 200, json: async () => ({ ok: true }) };
};

const { openAddAccount } = await import('./addwizard.js');
const { credHelp, hasCredFields } = await import('./views/ext-add.js');

// 向导的界面全在那只抽屉里。测试只读界面、只点按钮，不去戳模块内部状态——
// 测的是用户看到的东西，不是实现。
const drawerEl = () => qsel(document.body, '.drawer');
const stage = () => findByClass(drawerEl(), 'wiz-stage');
const stepBtns = () => findAll(drawerEl(), 'wiz-step');
const hintText = () => ((findByClass(drawerEl(), 'wiz-hint') || {}).textContent) || '';
const wayRows = () => findAll(stage(), 'wiz-way');
const addRows = () => findAll(stage(), 'addrow');
const shape = () => walk(stage());
const words = () => walk(stage()).buttons;
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const rowWith = (rows, kw) => rows.find(r => r.textContent.includes(kw));
async function openWizard(provider) { await openAddAccount(provider); await tick(); }
async function click(el, label) {
  if (!el) { bad('点不到「' + label + '」'); return; }
  await fire(el, 'click');
  await tick();
}
// 向导第一步能列出来的平台（注册表的 login 字段说了算：''/config 不从这条路径入池，
// alias_of 是同一个平台的另一种落库身份，不单列）。
const addable = () => PLATFORMS.filter(p => p.login && p.login !== 'config' && !p.alias_of);
`
}

// TestAddWizardSteps 「添加账号」向导的分步界面必须真的能点完、每步画对。
//
// 为什么需要：向导是命令式建树的（切步骤只换 stage 内容、节点按 (平台,方式) 缓存），
// 语法检查与顶层求值冒烟都只证明「能 import」，不证明「点下去出的是什么」——
// 方式列表漏一条、切回来看不到自己填的东西、操作说明没跟着方式走，
// 都是 Go 侧全绿而用户第一天就撞的错。无 node 时跳过。
func TestAddWizardSteps(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := wizardBoot(t) + `
/* ── 1. 第一步：搜索 + 厂商分组，且不列「不从添加账号入池」的通道 ── */
await openWizard();
if (!stage()) bad('向导没挂出 .wiz-stage');
if (findAll(document.body, 'drawer').length !== 1) bad('抽屉挂了不止一只（清单 20：入口只有一个）');
if (stepBtns().length !== 3) bad('还没选平台时进度该是 3 步，实为 ' + stepBtns().length);
const shownTexts = addRows().map(r => r.textContent);
for (const p of PLATFORMS) {
  const shown = shownTexts.some(t => t.startsWith(p.name));
  const should = !!p.login && p.login !== 'config' && !p.alias_of;
  if (shown !== should) bad(p.id + '：向导清单出现=' + shown + '，按注册表规则应为 ' + should);
}
if (!shownTexts.some(t => t.includes('批量 JSON 导入'))) bad('批量 JSON 导入没进清单（老用户靠它整包入池）');

const heads = findAll(stage(), 'wiz-vendor');
if (!heads.length) bad('第一步没有厂商分组表头');
const headName = hd => ((hd.children[0] || {}).textContent) || '';
for (const hd of heads) {
  const v = headName(hd);
  const n = Number(((hd.children[1] || {}).textContent) || '-1');
  const want = addable().filter(p => (p.vendor || '其他') === v).length + (v === '工具' ? 1 : 0);
  if (n !== want) bad('厂商「' + v + '」表头计数 ' + n + '，清单里实际 ' + want);
}
for (const v of new Set(addable().map(p => p.vendor || '其他'))) {
  if (!heads.some(hd => headName(hd) === v)) bad('缺厂商分组表头：' + v);
}

const q = document.getElementById('add-q');
if (!q) bad('第一步没有搜索框（平台已上到二十个，一屏清单等于没有清单）');
else {
  q.value = '华为';
  fire(q, 'input');
  const only = addRows().map(r => r.textContent);
  if (only.length !== 1 || !only[0].includes('CodeArts')) bad('搜「华为」应只剩 CodeArts，实为 ' + only.join(' / '));
  q.value = '扫码';
  fire(q, 'input');
  if (!addRows().some(r => r.textContent.includes('QClaw'))) bad('按接入方式搜（扫码）搜不到 QClaw');
  q.value = 'zzzz-not-a-platform';
  fire(q, 'input');
  if (!findByClass(stage(), 'empty')) bad('搜不到时没有空状态');
  fire(q, 'keydown', { key: 'Escape' });
  if (q.value !== '') bad('Escape 没清空搜索框');
  if (addRows().length !== shownTexts.length) bad('Escape 后清单没收回全量');
}

/* ── 2. 逐平台：接入方式列表 → 点进去把表单画对 ──────────────── */
const LOGIN_NAME = { oauth: '浏览器授权登录', device: '浏览器授权登录', callback: '浏览器授权登录',
  qr: '扫码登录', code: '设备码授权', sms: '手机号验证码登录', paste: '扫码后粘贴回调链接' };
const LOGIN_BTN = { qr: '微信扫码登录', device: '浏览器授权登录', callback: '浏览器授权登录',
  code: '开始授权', sms: '发送验证码', paste: '微信扫码登录' };
const CREDS_NAME = '粘贴客户端凭据', JSON_NAME = '粘贴完整凭据 JSON';
const BESPOKE = ['workbuddy', 'loomy', 'zai'];

// 手填那条（逐字段或整包 JSON）——两种形态的字段数与说明都得对着。
function checkHand(p) {
  const g = shape();
  if (!g.inputs && !g.buttons.length) { bad(p.id + '：表单是空的（用户看到白板）'); return; }
  if (hasCredFields(p.id)) {
    const want = credHelp(p.id).fields.length + 1;
    if (g.inputs !== want) bad(p.id + '：凭据表单 ' + g.inputs + ' 个输入框，期望 ' + want + '（账号标识 + 凭据字段）');
    if (!g.buttons.includes('添加')) bad(p.id + '：凭据表单缺「添加」按钮：' + g.buttons.join('/'));
    const help = findByClass(stage(), 'wiz-help');
    if (!help) { bad(p.id + '：操作说明没放进这一步（清单 24）'); return; }
    if (!help.textContent.includes('这些值从哪里复制')) bad(p.id + '：说明的标题不说人话：' + help.textContent.slice(0, 24));
    const n = help.querySelectorAll('li').length;
    const wantSteps = credHelp(p.id).steps.length;
    if (n !== wantSteps) bad(p.id + '：操作说明 ' + n + ' 条，字段表里写的是 ' + wantSteps + ' 条');
    const fig = findByClass(help, 'wiz-fig');
    if (credHelp(p.id).fig && !fig) bad(p.id + '：该配的示意图没画出来');
    if (fig && !qsel(fig, 'svg')) bad(p.id + '：示意图不是现画的 SVG（CSP 下引不了外链图片）');
    return;
  }
  if (g.inputs !== 2) bad(p.id + '：整包 JSON 那条该有「账号 ID + 凭据 JSON」两个输入框，实为 ' + g.inputs);
  if (qsel(stage(), 'select')) bad(p.id + '：整包 JSON 那条又给了一个平台下拉（第一步已经选好平台）');
}

for (const p of addable()) {
  if (BESPOKE.includes(p.id)) continue;      // 下面单独走：它们的方式清单不是通用 kind 派生的
  await openWizard(p.id);
  const ways = wayRows();
  const hasLoginWay = !!LOGIN_NAME[p.login];
  if (!hasLoginWay) {
    // 只有一条路（逐字段/整包凭据）时不必再问第二步：进度 3 步，直接就是表单
    if (ways.length) bad(p.id + '：单一接入方式不该问第二步：' + ways.map(w => w.textContent).join(' / '));
    if (stepBtns().length !== 3) bad(p.id + '：单方式平台的进度应 3 步，实为 ' + stepBtns().length);
    checkHand(p);
    continue;
  }
  if (ways.length !== 2) {
    bad(p.id + '：接入方式应列 2 种，实为 ' + ways.length + '：' + ways.map(w => w.textContent).join(' / '));
    continue;
  }
  if (stepBtns().length !== 4) bad(p.id + '：两种方式的进度应 4 步，实为 ' + stepBtns().length);

  if (!ways[0].textContent.includes(LOGIN_NAME[p.login])) bad(p.id + '：推荐方式措辞不对：' + ways[0].textContent);
  if (!ways[0].textContent.includes('推荐')) bad(p.id + '：排在第一的方式没标「推荐」');
  const wantHand = hasCredFields(p.id) ? CREDS_NAME : JSON_NAME;
  if (!ways[1].textContent.includes(wantHand)) bad(p.id + '：手填那条措辞不对：' + ways[1].textContent);

  const wh = ((findAll(ways[0], 'wiz-way-hint')[0] || {}).textContent) || '';
  await click(ways[0], p.id + ' 的登录方式');
  if (hintText() !== wh) bad(p.id + '：完成接入那一步的说明没跟着方式走（期望「' + wh + '」，实为「' + hintText() + '」）');
  if (!words().some(t => t.includes(LOGIN_BTN[p.login]))) bad(p.id + '：登录表单缺「' + LOGIN_BTN[p.login] + '」按钮：' + words().join('/'));
  if (findByClass(stage(), 'wiz-help')) bad(p.id + '：扫码/授权这条不需要「值从哪里复制」的说明');
  await click(stepBtns()[1], p.id + ' 回到第二步');
  await click(wayRows()[1], p.id + ' 的手填方式');
  checkHand(p);
}

/* ── 3. 三个有专属交互的平台：方式清单是它们自己的真路径 ─────── */
await openWizard('loomy');
const LOOMY = ['自动检测本机客户端', '手机号 + 密码', '手机号 + 短信验证码', '手动粘贴 Session Token'];
const lw = wayRows();
if (lw.length !== 4) bad('Loomy 应给 4 种接入方式，实为 ' + lw.length);
LOOMY.forEach((n, i) => {
  if (!lw[i] || !lw[i].textContent.includes(n)) bad('Loomy 第 ' + (i + 1) + ' 种方式应为「' + n + '」：' + (lw[i] ? lw[i].textContent : '（没有这一条）'));
});
for (let i = 0; i < 4; i++) {
  if (i > 0) await click(stepBtns()[1], '回到 Loomy 第二步');
  await click(wayRows()[i], 'Loomy 第 ' + (i + 1) + ' 种方式');
  const g = shape(), w = g.buttons;
  if (i === 0 && !w.includes('开始检测')) bad('Loomy 本机检测那条没有「开始检测」：' + w.join('/'));
  if (i === 1 && (g.inputs !== 2 || !w.includes('登录并入池'))) bad('Loomy 密码那条要有手机号 + 密码两个输入框与「登录并入池」');
  if (i === 2 && (g.inputs !== 2 || !w.includes('发送验证码') || !w.includes('登录并入池'))) bad('Loomy 短信那条要有手机号 + 验证码与发码/入池两个按钮');
  if (i === 3 && (g.inputs !== 1 || !w.includes('验证并保存'))) bad('Loomy Token 那条应只有一个输入框与「验证并保存」：' + g.inputs + '/' + w.join('/'));
}

await openWizard('zai');
const zw = wayRows().map(r => r.textContent);
if (zw.length !== 2 || !zw[0].includes('OAuth 免密登录') || !zw[1].includes('粘贴 JWT 或 API Key')) bad('Z.AI 的两种方式不对：' + zw.join(' / '));
await click(wayRows()[0], 'Z.AI OAuth 免密登录');
if (!words().includes('OAuth 免密登录')) bad('Z.AI OAuth 那条没有 OAuth 按钮：' + words().join('/'));
if (shape().inputs !== 1) bad('Z.AI OAuth 那条只该有「账号名称」一个输入框，实为 ' + shape().inputs);
await click(stepBtns()[1], '回到 Z.AI 第二步');
await click(wayRows()[1], 'Z.AI 粘贴凭据');
if (shape().inputs !== 2 || !words().includes('入池')) bad('Z.AI 粘贴凭据那条要有名称 + 密钥两个输入框与「入池」按钮');

// 腾讯只有一条路：第二步不该再问，直接给授权表单
await openWizard('workbuddy');
if (wayRows().length) bad('腾讯只有一种接入方式，第二步不该再问');
if (stepBtns().length !== 3) bad('腾讯的进度步数不对：' + stepBtns().length);
if (!words().includes('获取授权链接')) bad('腾讯那一步没有「获取授权链接」：' + words().join('/'));
const beforeRealm = stage().textContent;
await click(findButton(stage(), '国际版'), '国际版 Global');
if (stage().textContent === beforeRealm) bad('切了 CN/Global 但这一屏没重画');
if (!stage().textContent.includes('试用额度')) bad('切到国际版没说清会自动领试用额度');

/* ── 4. 批量导入 + 「切走再切回来还是同一棵树」───────────────── */
await openWizard();
await click(rowWith(addRows(), '批量 JSON 导入'), '批量 JSON 导入');
const fileIn = qsel(stage(), 'input');
if (!fileIn || shape().inputs !== 1) bad('批量导入应只有一个文件输入框，实为 ' + shape().inputs);
if (fileIn && fileIn.props.accept !== '.json') bad('批量导入的文件框没限定 .json');

await openWizard('qoder');
await click(wayRows()[1], 'Qoder 粘贴客户端凭据');
const node1 = stage().firstElementChild;
const idIn = qsel(node1, 'input');
idIn.value = '主号';
await click(findButton(drawerEl(), '上一步'), '上一步');
await click(wayRows()[1], '再进 Qoder 粘贴凭据');
if (stage().firstElementChild !== node1) bad('切走再切回来重建了表单——填了一半的内容与进行中的授权都会丢');
if (qsel(node1, 'input').value !== '主号') bad('切回来看不到刚才填的账号标识');
if (!node1.isConnected) bad('缓存的表单节点重新上屏后没回到文档里');
if (!findButton(drawerEl(), '关闭')) bad('向导底部没有「关闭」');

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('STEPS OK');
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "wizard-steps.mjs", harness, "STEPS OK", "添加向导的分步渲染不符")
}

// TestAddWizardLogin 走完一条真实接入：点授权 → 自动检测 → 入池 → 自动查余额 →
// 直接落到那个账号的详情；以及切步骤、换屏都不能把进行中的登录轮询掐掉。
//
// 为什么需要：这一段全是异步回调 + 定时器，import 与静态渲染都碰不到。以前
// 「面板重建把轮询掐了」上线过一次，表现是用户在浏览器里授权完了却没入池；
// 而「加完号得自己回列表找」这种收尾没做，也只有真点一遍才看得见。无 node 时跳过。
func TestAddWizardLogin(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := wizardBoot(t) + `
/* ── 1. 腾讯 OAuth：点链接 → 自动检测 → 入池 → 查余额 → 落详情 ── */
DATA['/api/login/start'] = { ok: true, state: 'st-1', url: 'https://workbuddy.example/oauth/start?x=1' };
DATA['/api/login/poll'] = { ok: true, done: true, uid: 'u9', nickname: '主号' };
DATA['/api/overview'] = { ok: true, accounts: [{ uid: 'u9', nickname: '主号', credits: 100, credits_total: 200 }] };
DATA['/api/zai/accounts'] = { ok: true, accounts: [] };
DATA['/api/accounts/u9/balance'] = { ok: true, credits: 100 };
await openWizard('workbuddy');
await click(findButton(stage(), '获取授权链接'), '获取授权链接');
await tick(20);
const d1 = drawerEl();
const title = ((qsel(d1, 'h2') || {}).textContent) || '';
if (title !== '主号') bad('入池后没自动打开该账号详情：抽屉标题「' + title + '」，期望「主号」');
if (!findByClass(d1, 'detail-foot')) bad('详情抽屉没打开（缺 .detail-foot）');
if (!CALLS.some(c => c.includes('/api/accounts/u9/balance'))) bad('入池确认后没自动查一次余额（清单 23）');

/* ── 2. 池里没有对应行（本机 Loomy）：停在「加入池中」把回执说清楚 ── */
DATA['/api/loomy/status'] = { ok: true, has_account: true,
  status: { userid: 'x-1', phone_masked: '138****0000', earned: 3, total: 10 } };
await openWizard('loomy');
await click(wayRows()[0], '自动检测本机客户端');
await click(findButton(stage(), '开始检测'), '开始检测');
await tick(20);
if (!stage().textContent.includes('登录态已经可用')) bad('本机 Loomy 没落到「加入池中」回执：' + stage().textContent.slice(0, 40));
if (!words().includes('看任务进度')) bad('本机 Loomy 的回执没给去处（应能直接去看任务进度）');

/* ── 3. 扫码那条：二维码必须是真的 SVG 元素，不是标记字符串 ──────
   h() 把字符串当文本节点，传标记进去会把 "<svg ...>" 原样显示出来。 */
const { qrMatrix, qrSVG } = await import('./qr.js');
const qrM = qrMatrix('https://xiaohuanxiong.com/login/mp?code=abc');
const svg = qrSVG(qrM, 160);
if (typeof svg === 'string') bad('qrSVG 返回字符串——h() 会当成文本节点，二维码不显示');
else {
  if ((svg.tagName || '').toLowerCase() !== 'svg') bad('qrSVG 未返回 svg 元素');
  const paths = (svg.children || []).filter(c => (c.tagName || '').toLowerCase() === 'path');
  if (!paths.length) bad('qrSVG 没有深色模块 path');
  else if (!paths[0].props.d || paths[0].props.d.length < 20) bad('qrSVG 的 path d 为空');
  const wantVB = '0 0 ' + (qrM.length + 8) + ' ' + (qrM.length + 8);
  if (svg.props['viewBox'] !== wantVB) bad('qrSVG viewBox = ' + svg.props['viewBox'] + '，期望 ' + wantVB);
}
DATA['/api/ext/raccoon/login/start'] = { ok: true, mode: 'qr', session: 's1',
  qr_url: 'https://xiaohuanxiong.com/login/mp?code=abc', expires_in: 600 };
DATA['/api/ext/raccoon/login/poll'] = { ok: true, done: false, status: 'pending' };
await openWizard('raccoon');
await click(wayRows()[0], 'raccoon 扫码登录');
await click(findButton(stage(), '微信扫码登录'), '微信扫码登录');
await tick(20);
const qrbox = findByClass(stage(), 'qr');
if (!qrbox) bad('点扫码登录后没渲染出二维码');
else if ((qrbox.children[0] || {}).tagName !== 'SVG') bad('二维码不是 svg 元素');

/* ── 4. 切步骤 / 换屏都不能停掉进行中的授权轮询 ────────────────
   用户在浏览器里点授权要几十秒，期间会来回切向导的步骤。早先的实现把轮询
   挂在面板实例上，重建一次就掐死——表现是「授权完成了却没入池」。 */
let polls = 0;
DATA['/api/ext/qoder/login/start'] = { ok: true, mode: 'device', session: 'sess-A',
  auth_url: 'https://qoder.example/device/authorize?x=1', expires_in: 600 };
DATA['/api/ext/qoder/login/poll'] = () => { polls++; return { ok: true, done: true,
  account: { provider: 'qoder', id: 'q-9', label: 'Qoder 主号' } }; };
DATA['/api/ext/accounts'] = { ok: true, accounts: [{ provider: 'qoder', id: 'q-9', label: 'Qoder 主号' }] };
await openWizard('qoder');
await click(wayRows()[0], 'Qoder 浏览器授权登录');
await click(findButton(stage(), '浏览器授权登录'), '开始授权');
await tick(20);
if (!stage().textContent.includes('qoder.example/device/authorize')) bad('设备授权那条没把链接画出来：' + stage().textContent.slice(0, 40));
await click(findButton(drawerEl(), '上一步'), '上一步');
await click(wayRows()[0], '再进 Qoder 授权方式');
await tick(2400);
if (polls === 0) bad('切走再切回来把登录轮询停了——授权完成也不会入池');
const title4 = ((qsel(drawerEl(), 'h2') || {}).textContent) || '';
if (title4 !== 'Qoder 主号') bad('轮询收到账号后没落到详情：抽屉标题「' + title4 + '」');

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('LOGIN OK');
// 活动栏作业与登录轮询都带着无 ref 的定时器；先让 stdout 落盘再主动退出，
// 别让一次绿测试挂成超时。
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "wizard-login.mjs", harness, "LOGIN OK", "添加向导的登录收尾不符")
}

// TestTasksPageRender 「任务」页这一屏必须画对（清单 25 / 26 / 27 / 29）：
// 两条动作合一成「检查并执行」，确认框里说的是将要做什么而不是「确定吗」，
// 并发档位带着「同时跑」的说明，排程开口是时间线而不是空白台账，季节性文案消失。
//
// 为什么需要：这一页全是命令式建树 + 异步队列，语法检查与 import 冒烟都只证明
// 「能加载」；把两个按钮合并成一个时漏掉某条路径、确认框回退成通用文案、时间线
// 拿不到 next_fire 时整格空白，都是编译绿而用户第一天就看见的错。无 node 时跳过。
func TestTasksPageRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := wizardBoot(t) + `
const view = (await import('./views/tasks.js')).default;
// 平台名来自后端注册表（boot.js 在真实面板里负责先加载它），不加载就只能显示裸 id
const { loadPlatforms } = await import('./platforms.js');
await loadPlatforms();
const pageEl = () => document.body;
const allTxt = () => document.body.textContent;
const btnTxts = () => walk(document.body).buttons;
const hasTxt = s => allTxt().includes(s);

/* ── 1. 待办那一屏：一条动作、一个说明清楚的并发档 ────────────── */
DATA['/api/tasks/queue'] = { ok: true, started: false };
DATA['/api/loomy/status'] = { ok: true, has_account: false };
DATA['/api/task_report'] = { ok: true, day_done: 0, rounds: [], pending: [] };
let root = view.render();
document.body.append(root);
await tick();

if (!hasTxt('检查并执行')) bad('没有「检查并执行」（清单 25：扫描与执行合一）：' + btnTxts().join('/'));
for (const dead of ['扫描待办', '执行全部待办']) {
  if (btnTxts().some(b => b.includes(dead))) bad('旧的两条动作还留着按钮：' + dead);
}
if (hasTxt('开学季')) bad('页面上还有季节性文案「开学季」（清单 29）');
const tabBar = findAll(document.body, 'seg')[0];
const tabs = tabBar ? walk(tabBar).buttons : [];
if (tabs.join('/') !== '今日待办/排程台账') bad('任务页还按平台分 tab（清单 28），实为：' + tabs.join('/'));
if (!hasTxt('同时跑')) bad('并发档没标「同时跑」（清单 26：并发是实现词）');
if (hasTxt('并发')) bad('界面上还在用「并发」这个词：' + allTxt().slice(0, 60));
const concWrap = findAll(document.body, 'seg').find(s => s.textContent.includes('同时跑'));
const concOpts = concWrap ? walk(concWrap).buttons : [];
if (concOpts.join('/') !== '1/2/3') bad('并发档应只剩 1/2/3 三个数字，实为 ' + concOpts.join('/'));

/* ── 2. 点「检查并执行」：先扫、把清单给人看过、确认后才排队 ───── */
let scans = 0;
DATA['/api/tasks/scan_all'] = () => {
  scans++;
  return { ok: true,
    accounts: [
      { uid: 'u1', nickname: '甲', growth: [
        { task_code: 'chat_5', title: '对话 5 次', target: 5, current: 0 },
        { task_code: 'first_buddy', title: '首次对话' }] },
      { uid: 'u2', nickname: '乙', growth: [{ task_code: 'chat_5', title: '对话 5 次', target: 5, current: 2 }] },
    ],
    ext: [{ provider: 'raccoon', id: 'r1', label: '小浣熊' }] };
};
let runs = 0, runBody = '';
DATA['/api/tasks/run_queue'] = (u, o) => { runs++; runBody = String((o && o.body) || ''); return { ok: true, started: true, total: 4, seq: 7 }; };

root = view.render();
document.body.append(root);
// 「检查并执行」的处理器会停在 confirm 那一步等用户回话，所以不能用会 await
// 处理器的 click 助手（那是等一个永远不 resolve 的 promise）——直接触发，再等宏任务。
const goBtn = findButton(root, '检查并执行');
if (!goBtn) bad('点不到「检查并执行」按钮：' + btnTxts().join('/'));
else fire(goBtn, 'click');   // 不 await：处理器正等着确认框回话，await 会永远挂着
await tick(40);
if (scans !== 1) bad('点一次「检查并执行」应扫一次，实为 ' + scans);
const sheet = findByClass(document.body, 'sheet');
if (!sheet) bad('检查完没把清单交给用户确认（没有确认框）');
else {
  const msg = ((findByClass(sheet, 'hint') || {}).textContent) || '';
  if (!msg.includes('共 4 项待办')) bad('确认框没说清共几项：' + msg);
  if (!msg.includes('成长任务 3 项')) bad('确认框没按类型报数（成长）：' + msg);
  if (!msg.includes('外部平台签到/领奖 1 项')) bad('确认框没按类型报数（签到）：' + msg);
  if (!msg.includes('同时跑 1 个')) bad('确认框没说什么并发：' + msg);
  if (msg.includes('确定吗')) bad('确认框回退成了通用措辞：' + msg);

  /* 清单 28：待办清单的主轴是任务类型，不是平台 —— 确认之前就该按类型分组画出来 */
  const g1 = view.render();
  document.body.append(g1);
  const heads = findAll(g1, 'qgroup').map(gr => ((findByClass(gr, 'nm') || {}).textContent) || '');
  if (!heads.includes('成长任务')) bad('待办没按任务类型分组（缺「成长任务」那一组）：' + heads.join(' / '));
  if (!heads.includes('每日签到 / 领奖')) bad('待办没按任务类型分组（缺「每日签到」那一组）：' + heads.join(' / '));
  if (heads.some(hh => hh === '甲' || hh === '乙')) bad('还在按账号/平台分组：' + heads.join(' / '));
  const rowTxt = findAll(g1, 'qrow').map(r => r.textContent);
  if (!rowTxt.some(t => t.includes('甲'))) bad('行上没有账号名（分组换成类型后，账号只能靠标签认）：' + rowTxt[0]);
  if (!rowTxt.some(t => t.includes('小浣熊'))) bad('外部平台那行没带账号标签：' + rowTxt.join('/'));
  const chips = findAll(g1, 'chip').map(c => c.textContent);
  if (!chips.includes('全部平台')) bad('平台筛选没出现（有两个以上平台时该给筛选条）：' + chips.join('/'));
  // 只点筛选条上那些带 click 的 chip（行内的平台标签是同一形状，但没有处理器）
  const fChip = findAll(g1, 'chip').find(c => (c.listeners.click || []).length && c.textContent.includes('小浣熊'));
  if (!fChip) bad('平台筛选条里没有「小浣熊」这一档');
  else {
    fire(fChip, 'click');
    await tick();
    const g2 = view.render();
    document.body.append(g2);
    const h2 = findAll(g2, 'qgroup').map(gr => ((findByClass(gr, 'nm') || {}).textContent) || '');
    if (h2.includes('成长任务')) bad('筛到小浣熊后还留着成长任务那一组（平台筛选没生效）：' + h2.join('/'));
    if (!h2.includes('每日签到 / 领奖')) bad('筛到小浣熊后签到组没了：' + h2.join('/'));
  }

  await click(findButton(sheet, '排队执行'), '排队执行');
  await tick(30);
}
if (runs !== 1) bad('确认后没把队列发出去（run_queue 调用 ' + runs + ' 次）');
if (!/"concurrency":1/.test(runBody)) bad('执行请求没带并发档：' + runBody);
if (findAll(document.body, 'sheet').length > 1) bad('一次动作问了不止一遍（叠了第二个确认框）');
if (!hasTxt('券码')) bad('活动券码入口丢了');

/* ── 3. 排程那一屏：开口是时间线，没台账也不空白 ──────────────── */
const NEXT = Date.now() + 3600 * 1000;
DATA['/api/task_report'] = { ok: true, day_done: 3, next_fire: NEXT, next_kinds: ['签到', '成长任务'],
  rounds: [], pending: [{ task: 'chat_5', uids: ['u1'], tries: 1, next_at: '12:00' }] };
location.hash = '#tasks?tab=sched';
root = view.render();
document.body.append(root);
await click(findButton(root, '刷新'), '刷新台账');   // 台账数据是异步回来的，先让它落进信号
await tick(20);
root = view.render();
document.body.append(root);
const schedTxt = () => root.textContent;
for (const k of ['今天', '现在', '下一次']) if (!schedTxt().includes(k)) bad('排程时间线缺「' + k + '」那一格：' + schedTxt().slice(0, 80));
if (!schedTxt().includes('待补跑')) bad('待补跑（退避重试）的条目没画出来');
if (schedTxt().includes('暂无台账')) bad('有台账数据却还显示「暂无台账」');

DATA['/api/task_report'] = { ok: true, day_done: 0, rounds: [], pending: [] };
await click(findButton(root, '刷新'), '刷新空台账');
await tick(20);
location.hash = '#tasks?tab=sched';
root = view.render();
document.body.append(root);
if (!root.textContent.includes('未排程')) bad('排程没启用时「下一次」那一格什么都没写（清单 27 要的是不空白）：' + root.textContent.slice(0, 60));
if (root.textContent.includes('暂无台账') === false && !root.textContent.includes('今天')) bad('排程一屏开口不是时间线');

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('TASKS OK');
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "tasks-page.mjs", harness, "TASKS OK", "任务页的合一动作与时间线不符")
}

// TestConfigPageRender 配置页的六条界面约定（清单 30–35）必须真的成立：
// 目录与搜索在、开关带用途说明、时长是「数字 + 单位」、改动有状态（几项改动 /
// 几项要重启 / 字段左边的小点）、每项能恢复默认、高级项默认折叠。
//
// 为什么需要：这页是声明式字段表命令式建出来的，取值逻辑（基线 vs 未保存改动）
// 和显隐逻辑都在闭包里；编译与 import 都拦不住「搜索把没保存的输入重建没了」
// 「需重启清单没接上，保存栏永远说保存即生效」这类错。无 node 时跳过。
func TestConfigPageRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := wizardBoot(t) + `
const view = (await import('./views/config.js')).default;
const note = () => ((findByClass(document.body, 'note') || {}).textContent) || '';
const all = () => document.body.textContent;

const CONF = {
  listen: ':7863', api_key: '',
  schedule: { checkin_enabled: true, checkin_hours: [9, 21], balance_refresh_minutes: 5, growth_enabled: true },
  cooldown: { soft_rate: '600s' },
  pool: { max_in_flight: 3, breaker_threshold: 3, breaker_cooldown: '30m' },
  session_sticky: { ttl: '30m', enabled: true },
  upstream: { timeout_seconds: 120, user_agent: '' },
  prompt: { mode: 'custom' },
  features: { sanitize_blacklist_fingerprints: true },
};
const DEFS = {
  listen: ':7863', api_key: '',
  schedule: { checkin_enabled: true, checkin_hours: [8, 22], balance_refresh_minutes: 5, growth_enabled: true },
  cooldown: { soft_rate: '300s' },
  pool: { max_in_flight: 3, breaker_threshold: 5, breaker_cooldown: '20m' },
  session_sticky: { ttl: '20m', enabled: true },
  upstream: { timeout_seconds: 90, user_agent: '' },
  prompt: { mode: 'custom' },
  features: { sanitize_blacklist_fingerprints: true },
};
const RESTART = ['listen', 'upstream.timeout_seconds', 'session_sticky.ttl'];
let posts = 0;
DATA['/api/config'] = (u, o) => {
  if (o && o.method === 'POST') { posts++; return { ok: true, restart_required: ['listen'] }; }
  return { ok: true, path: 'D:/data/config.json', config: CONF, defaults: DEFS, restart_fields: RESTART };
};

// 只挂一个宿主：多棵渲染树并存在 body 里，按 body 查元素就会查到上一棵的旧节点，
// 断言读到的值不是刚操作的那个（2026-10-09 实测踩过：恢复默认「没生效」其实是查错树）。
const mount = document.createElement('div');
document.body.append(mount);
let root = null;
const paint = () => { root = view.render(); mount.replaceChildren(root); };

view.render();                 // 第一屏只会是「读取配置」：值都在后端
await tick(30);
paint();
await tick();

/* ── 1. 目录 + 搜索（清单 30）────────────────────────────────── */
if (!document.getElementById('cfg-q')) bad('没有搜索配置项的输入框（四十来项靠滚找不到）');
const navBtns = findAll(root, 'cfg-nav')[0] ? walk(findAll(root, 'cfg-nav')[0]).buttons : [];
if (navBtns.length < 3) bad('左侧目录没列出分组，实为 ' + navBtns.length + ' 个：' + navBtns.join('/'));

/* ── 2. 高级项默认折叠（清单 35）────────────────────────────── */
const advs = findAll(root, 'adv');
if (!advs.length) bad('没有折叠的高级项分组（熔断/降权这类该默认收起来）');
else if (advs.some(d => d.open)) bad('高级项默认就是展开的');
else if (!advs.some(d => d.textContent.includes('熔断'))) bad('熔断阈值没被归进高级项：' + advs.map(d => d.textContent.slice(0, 20)).join('/'));

/* ── 3. 开关带用途说明（清单 31）────────────────────────────── */
const rows = findAll(root, 'switch-row');
if (rows.length < 4) bad('开关行只有 ' + rows.length + ' 条');
const noTip = rows.filter(r => {
  const m = findAll(r, 'muted')[0];
  return !m || !((m.textContent) || '').trim();
});
if (noTip.length) bad('有无小字说明的开关：' + noTip.map(r => r.textContent.slice(0, 12)).join(' / '));
if (!rows[0].textContent.includes('要重启') && rows.some(r => r.textContent.includes('自动签到')) === false) bad('开关行措辞异常');

/* ── 4. 时长＝数字 + 单位（清单 32）；需重启的字段先标出来 ────── */
const ttlWrap = findAll(root, 'field').find(f => f.textContent.includes('会话粘性 TTL'));
if (!ttlWrap) bad('找不到会话粘性 TTL 这一项');
else {
  const g = walk(ttlWrap);
  const sel = qsel(ttlWrap, 'select');
  if (!sel) bad('时长项没有单位下拉（还是让人手写 30m）：' + g.buttons.join('/'));
  else {
    const opts = (sel.children || []).map(c => c.textContent);
    if (!opts.includes('分钟') || !opts.includes('小时')) bad('单位下拉缺档：' + opts.join('/'));
  }
  const num = (sel ? qsel(sel.parentNode, 'input') : null) || qsel(ttlWrap, 'input');
  if (num && num.value !== '30') bad('30m 没拆成数字 30 + 单位 m，实为「' + (num ? num.value : '?') + '」');
  if (!ttlWrap.textContent.includes('要重启')) bad('装配期定死的项没标「要重启」（清单 33 要说在保存之前）');
}

/* ── 5. 改动有状态：小点 + 保存栏报数（清单 33）──────────────── */
if (note() !== '没有改动') {
  const chs = findAll(root, 'field').filter(f => f.classList.contains('changed'))
    .map(f => ((findByClass(f, 'label') || {}).textContent || '').slice(0, 12)
      + '=' + JSON.stringify((qsel(f, 'input') || qsel(f, 'select') || {}).value));
  bad('初始状态保存栏不该报改动：' + note() + ' ｜ ' + chs.join(' | '));
}
const listenWrap = findAll(root, 'field').find(f => f.textContent.includes('监听地址'));
const listenIn = listenWrap && qsel(listenWrap, 'input');
if (!listenIn) bad('找不到监听地址输入框');
else {
  listenIn.value = ':8080';
  fire(listenIn, 'input');
  await tick();
  if (!note().includes('1 项改动')) bad('改一项后保存栏没报数：' + note());
  if (!note().includes('要重启')) bad('改的是需重启项，保存栏没说有几项要重启：' + note());
  if (!listenWrap.classList.contains('changed')) bad('改过的字段左边没有小点');
  listenIn.value = ':7863';               // 改回原值
  await fire(listenIn, 'input');
  await tick();
  if (note() !== '没有改动') bad('改回原值后保存栏该回到「没有改动」：' + note());
  if (listenWrap.classList.contains('changed')) bad('改回原值后小点该消失');
}

/* ── 6. 恢复默认（清单 34）：值来自后端 defaults ─────────────── */
const brkWrap = findAll(root, 'field').find(f => f.textContent.includes('连续失败熔断阈值'));
const brkIn = brkWrap && qsel(brkWrap, 'input');
if (!brkIn) bad('找不到熔断阈值输入框');
else {
  const rb = brkWrap ? findButton(brkWrap, '恢复默认') : null;
  if (!rb) bad('与默认值不同的项没给「恢复默认」按钮');
  else {
    brkIn.value = '9';
    fire(brkIn, 'input');
    await tick();
    const rb2 = findButton(findAll(root, 'field').find(f => f.textContent.includes('连续失败熔断阈值')), '恢复默认');
    if (!rb2) bad('改成非默认值后「恢复默认」不该消失');
    fire(rb2, 'click');
    await tick();
    const now = qsel(findAll(root, 'field').find(f => f.textContent.includes('连续失败熔断阈值')), 'input');
    if (now.value !== '5') bad('恢复默认没落到后端给的默认值 5，实为「' + now.value + '」');
  }
}

/* ── 7. 搜索就地显隐，不重建表单（没保存的输入不能被冲掉）─────── */
const before = listenIn.value;
const sq = document.getElementById('cfg-q');
sq.value = '熔断';
fire(sq, 'input');
await tick();
const hiddenCnt = findAll(root, 'field').filter(f => f.style.display === 'none').length;
if (hiddenCnt === 0) bad('搜索没把不匹配的项藏起来');
if (qsel(root, '#cfg-q') !== sq || listenIn.value !== before) bad('搜索重建了表单（没保存的输入会被冲掉）');
const openAdv = findAll(root, 'adv').filter(d => d.open);
if (!openAdv.some(d => d.textContent.includes('熔断'))) bad('搜到高级项时那一组该摊开，否则命中的东西藏在折叠里');

/* ── 8. 保存 → 立即重启（清单 33 的后半句）──────────────────── */
// 改的是需重启项（监听地址），保存后回执会说 listen 要重启——这时才该出现按钮。
listenIn.value = ':9999';
await fire(listenIn, 'input');
await tick();
// render() 返回的就是那只 form（qsel 只往后代找，不查自身），所以先看根。
const form = root.tagName === 'FORM' ? root : qsel(root, 'form');
if (!form) bad('配置页不是 form（保存按钮走的是提交语义）');
else { await fire(form, 'submit'); await tick(40); }
if (posts !== 1) bad('保存发了 ' + posts + ' 次请求（同一次点击发两遍就是双写）');
paint();
await tick();
if (!all().includes('立即重启')) bad('改了需重启的项并保存后，没给出「立即重启」');
const rb = findButton(root, '立即重启');
if (rb) {
  fire(rb, 'click');
  await tick(30);
  const sheet = findByClass(document.body, 'sheet');
  if (!sheet) bad('立即重启没先说清代价（没有确认框）');
  else {
    if (!sheet.textContent.includes('中断')) bad('确认框没说重启会断几秒：' + sheet.textContent.slice(0, 60));
    await click(findButton(sheet, '立即重启'), '确认重启');
    await tick(30);
    if (!CALLS.some(c => c.includes('/api/restart'))) bad('确认后没把重启请求发出去');
  }
}

/* ── 9. 改的都是热生效项时，别递「立即重启」───────────────────── */
const brk3Wrap = findAll(root, 'field').find(f => f.textContent.includes('连续失败熔断阈值'));
const brk3 = brk3Wrap && qsel(brk3Wrap, 'input');
if (!brk3) bad('第二轮找不到熔断阈值输入框');
else {
  brk3.value = '6';
  await fire(brk3, 'input');
  await tick();
  if (note().includes('要重启')) bad('熔断阈值是热生效项，保存栏不该说要重启：' + note());
  await fire(root.tagName === 'FORM' ? root : qsel(root, 'form'), 'submit');
  await tick(40);
  paint();
  await tick();
  if (findButton(root, '立即重启')) bad('改动全是热生效的，却给了「立即重启」——后端回执是全量清单，要跟本次改动求交集');
}

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('CONFIG OK');
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "config-page.mjs", harness, "CONFIG OK", "配置页的目录/搜索/改动状态不符")
}

// TestKeysPageRender API 密钥页的三条界面约定（清单 36 / 37 / 39）：
// 授权范围是「全平台 / 指定平台」单选 + 可搜索多选；生成后那张一次性卡片真的
// 把完整密钥、复制、现成示例一起交出去；列表里只剩掩码。
//
// 为什么需要：这一页改动同时涉及「明文只出现一次」这条安全约定——它一旦回退，
// 页面看起来完全正常，只有把鼠标移到列表上才发现所有密钥又摊在屏幕上了。
func TestKeysPageRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := wizardBoot(t) + `
const view = (await import('./views/keys.js')).default;
const FULL = 'bh-aa11bb22cc33dd44ee55ff66aa11bb22';
let posts = [];
DATA['/api/apikeys'] = (u, o) => {
  const body = o && o.body ? JSON.parse(o.body) : null;
  if (u.includes('/delete')) { posts.push(['delete', o.body]); return { ok: true }; }
  if (o && o.method === 'POST') { posts.push(['post', o.body]); return { ok: true, key: { key: FULL, name: body.name, platforms: body.platforms, note: body.note } }; }
  return { ok: true, keys: [{ id: 'abc123def456', name: '桌面端', masked: 'bh-aa1…bb22', platforms: ['qoder'] }] };
};
DATA['/api/platforms'] = { ok: true, platforms: PLATFORMS };
const { loadPlatforms } = await import('./platforms.js');
await loadPlatforms();

const mount = document.createElement('div');
document.body.append(mount);
let root = null;
const paint = () => { root = view.render(); mount.replaceChildren(root); };
const txt = () => root.textContent;
const btns = () => walk(root).buttons;

paint();
await tick(40);
paint();
await tick();

const scopeDetail = () => findAll(root, 'scope-detail')[0];
const chipsShown = () => { const d = scopeDetail(); return !!d && d.style.display !== 'none'; };

/* ── 1. 授权范围：先单选，勾了「指定平台」才出现清单（清单 36）── */
const seg = findAll(root, 'seg').map(s => walk(s).buttons.join('/'));
if (!seg.some(s => s === '全平台/指定平台')) bad('授权范围不是「全平台 / 指定平台」单选：' + seg.join(' | '));
if (chipsShown()) bad('默认「全平台」时就把平台清单摊开了（20 个按钮的老样子）');
if (btns().filter(b => b === '生成新 Key').length !== 1) bad('生成按钮应当只有一个：' + btns().join('/'));

const some = findAll(root, 'seg')[0].children.find(c => c.textContent.trim() === '指定平台');
await click(some, '指定平台');
await tick();
if (!chipsShown()) bad('选了「指定平台」却没把可搜索的清单摊开');
const nChips = findAll(root, 'chip').length;
const sq = qsel(root, 'input.search');
if (!sq) bad('平台清单没有搜索框');
else {
  sq.value = 'qoder';
  await fire(sq, 'input');
  await tick();
  const hit = findAll(root, 'chip').map(c => c.textContent);
  if (hit.length >= nChips) bad('搜索没缩短平台清单（' + nChips + ' → ' + hit.length + '）');
  const one = findAll(root, 'chip').find(c => c.textContent.includes('Qoder'));
  if (!one) bad('搜 qoder 没出现 Qoder 平台：' + hit.join('/'));
  await click(one, '勾上 Qoder');
  if (!txt().includes('已勾 1 个')) bad('勾选后没汇总说了勾了哪几个：' + txt().slice(-80));
}

/* ── 2. 一次性卡片（清单 37）────────────────────────────────── */
posts = [];
paint();                                   // 先生成这一屏的树，再往它自己的输入框里填
document.getElementById('nk-name').value = '我的桌面端';
await click(findButton(root, '生成新 Key'), '生成新 Key');
await tick(40);
if (posts.length !== 1 || posts[0][0] !== 'post') bad('点生成没发请求：' + JSON.stringify(posts));
const sentBody = JSON.stringify(posts[0][1]);
if (!sentBody.includes('qoder')) bad('指定平台勾选没进请求体：' + sentBody);
paint();
await tick();
if (!txt().includes(FULL)) bad('一次性卡片没把完整密钥给出来（用户拿不到就用不了）');
if (!txt().includes('只显示这一次')) bad('没说明这是只显示一次的（清单 37）');
if (!txt().includes('curl')) bad('卡片没有现成的 curl 示例');
if (!txt().includes('OpenAI Compatible')) bad('卡片没有客户端填写示例');
if (!txt().includes('7863/v1')) bad('示例里没有 Base URL');

/* ── 3. 列表只留掩码，删除认 id ─────────────────────────────── */
const done = findAll(root, 'onecard')[0];
const closeBtn = done ? findButton(done, '我已保存') : null;
if (!closeBtn) bad('一次性卡片没有「我已保存」收口');
else { await click(closeBtn, '我已保存'); paint(); await tick(); }
if (txt().includes(FULL)) bad('关掉卡片后完整密钥还留在页面上');
if (findAll(root, 'keyrow').length !== 1) bad('列表行数不对');
const rowTxt = findAll(root, 'keyrow')[0].textContent;
if (rowTxt.includes(FULL)) bad('列表行里还有明文密钥：' + rowTxt);
if (!rowTxt.includes('bh-aa1…bb22')) bad('列表没显示掩码：' + rowTxt);
if (findButton(findAll(root, 'keyrow')[0], '复制')) bad('列表里还能复制密钥（明文早已不再下发）');
posts = [];
const del = findButton(findAll(root, 'keyrow')[0], '删除');
fire(del, 'click');   // 不 await：处理器停在确认框上，await 会永远挂着
await tick(30);
const sheet = findByClass(document.body, 'sheet');
if (!sheet) bad('删除没有二次确认');
else {
  if (!sheet.textContent.includes('桌面端')) bad('确认框没说是哪一把：' + sheet.textContent.slice(0, 60));
  await click(findButton(sheet, '删除'), '确认删除');
  await tick(30);
  if (!posts.some(x => x[0] === 'delete' && String(x[1]).includes('abc123def456'))) bad('删除没按 id 发出去：' + JSON.stringify(posts));
}

/* ── 4. 前缀对照表折起来（清单 39）──────────────────────────── */
const det = findAll(root, 'adv')[0];
if (!det) bad('调用说明没有折叠的前缀对照表（清单 39）');
else {
  if (det.open) bad('对照表默认就该折叠');
  if (!det.textContent.includes('cn:') && !det.textContent.includes('前缀')) bad('折叠里没有对照内容：' + det.textContent.slice(0, 40));
}

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('KEYS OK');
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "keys-page.mjs", harness, "KEYS OK", "密钥页的范围单选/一次性卡片不符")

	// 单独一轮：平台注册表**晚到**时这一屏必须自己补出平台名（2026-10-10 用户实测）。
	// 上一轮预先 await 了 loadPlatforms()，恰好把这条竞态遮住——真实首屏是先画密钥、
	// 注册表还在路上，platName 只能回落成 id，界面上就漏出 qoder / codearts 这种实现词。
	late := wizardBoot(t) + `
const view = (await import('./views/keys.js')).default;
const { effect } = await import('./kernel.js');
const { platforms, loadPlatforms } = await import('./platforms.js');
DATA['/api/apikeys'] = { ok: true, keys: [{ id: 'k1', name: '桌面端', masked: 'bh-aa1…bb22', platforms: ['qoder', 'codearts'] }] };
DATA['/api/usage?hours=0'] = { ok: true, by_key: [] };
await loadPlatforms();

const mount = document.createElement('div');
document.body.append(mount);
// 只看那一行密钥右侧的平台标签——整页文本会撞上「前缀对照表」里的 qoder:，那是该出现的技术词。
const rowChips = () => {
  const row = findAll(mount, 'keyrow')[0];
  return row ? findAll(row, 'chip').map(c => c.textContent.trim()) : null;
};
const settle = async () => { for (let i = 0; i < 60; i++) await new Promise(r => setTimeout(r, 0)); };

// 把注册表清空再画：platName 在没有注册表时按设计回落成 id——这就是用户看到的
// 那一帧（上一轮预载把它遮住了）。随后让注册表到位，这一屏必须自己重画。
platforms.set([]);
effect(() => { const n = view.render(); mount.replaceChildren(n); return n; });
await settle();
const bare = rowChips();
if (!bare) bad('等不到密钥行（注册表清空后就不画了吗）');
else if (bare.join('/') !== 'qoder/codearts') bad('没构造出「注册表还没到」那一帧，标签实为 ' + bare.join('/'));
platforms.set(PLATFORMS);
await settle();
const named = rowChips();
const qname = (PLATFORMS.find(p => p.id === 'qoder') || {}).name;
const canme = (PLATFORMS.find(p => p.id === 'codearts') || {}).name;
if (!qname || !canme) bad('夹具里没有 qoder / codearts 的名字');
else if (!named || named.join('/') !== qname + '/' + canme) bad('注册表到位后标签没补成平台名，实为 ' + (named || []).join('/'));
if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('LATE REGISTRY OK');
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "keys-late-registry.mjs", late, "LATE REGISTRY OK", "密钥页在平台注册表晚到时没把 id 补成平台名")
}

// TestLogsPageRender 执行记录页（清单 40 / 41 / 42）必须画对：五路筛选合成
// （频道+级别+全文+账号+平台）、错误行走统一分级、长行折叠且展开态扛得过
// 增量追加与重建、行内账号可跳详情、往上翻后新行只计数并在点「↓ N 条新日志」
// 后才上屏、「自动滚动」开关不复存在。
//
// 为什么需要：日志框是跨渲染复用的命令式增量更新——语法检查与 import 冒烟都
// 看不见「第 N 次 tick 之后屏上是什么」；把展开态错记到 DOM 节点上、把锚点
// 追加算错一条、筛选少 AND 一路，都是编译绿而用户翻日志时才发现的错。无 node 时跳过。
func TestLogsPageRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := wizardBoot(t) + `
const view = (await import('./views/logs.js')).default;

/* ── fixture：条目形态照后端 ring.go（ts/ch/text，没有结构化账号列）── */
const A_UID = 'uid-alice-0123456789';           // 前 8 字 = uid-alic，池行写法
const mk = (min, ch, text) => ({ ts: '2026-10-08T10:' + String(min).padStart(2, '0') + ':00Z', ch, text });
let LOG = [
  mk(1, 'task', 'checkin 爱丽丝(uid-alic) 登录失败 error'),
  mk(2, 'task', 'checkin 爱丽丝(uid-alic) 进入冷却'),
  mk(3, 'task', 'checkin 爱丽丝(uid-alic) 成功 +50分'),
  mk(4, 'chat', 'chat uid=' + A_UID + ' model=glm 200'),
  mk(5, 'sys',  'panel 启动完成'),
  mk(6, 'sys',  'error: 上游返回体 ' + 'x'.repeat(300) + 'TAILMARK'),
  mk(7, 'task', 'qoder 签到 acct=q-9 成功'),
];
let EXTRA = 0;
DATA['/api/logs'] = () => ({ ok: true, entries: LOG });
DATA['/api/overview'] = () => ({ ok: true, accounts: [{ uid: A_UID, nickname: '爱丽丝', credits: 10, credits_total: 100 }] });
DATA['/api/zai/accounts'] = { ok: true, configured: true, accounts: [] };
DATA['/api/ext/accounts'] = () => ({ ok: true, accounts: [{ provider: 'qoder', id: 'q-9', label: 'Qoder 主号' }] });
DATA['/api/usage/series'] = { ok: true, uid: A_UID, days: 7, points: [] };

const mount = document.createElement('div');
document.body.append(mount);
let root = null;
const paint = () => { root = view.render(); mount.replaceChildren(root); };
const box = () => findByClass(mount, 'logbox');
const lines = () => findAll(box(), 'ln');
const lineWith = kw => lines().find(l => l.textContent.includes(kw));
const btns = () => walk(mount).buttons;
const segWith = kw => findAll(mount, 'seg').find(s => s.textContent.includes(kw));
const segBtn = (kw, label) => { const s = segWith(kw); return s ? s.children.find(c => c.textContent.trim() === label) : null; };
const chipWith = kw => findAll(mount, 'fchip').find(c => c.textContent.includes(kw));
const qin = () => findByClass(mount, 'search');
// 信号驱动的部分测试里只能手动补一帧：渲染 → 微任务 syncBox → 再渲染
const settle = async (n = 2) => { for (let i = 0; i < n; i++) { paint(); await tick(10); } };

paint();                          // 第一帧：拉日志 + 注册表 + 三个账号源
await tick(60);
await settle(3);

/* ── 1. 自动滚动开关已消失；行样式与级别同源（统一分级器）──────── */
if (btns().some(b => b.includes('自动滚动'))) bad('清单 42：「自动滚动」开关没删掉：' + btns().join('/'));
if (lines().length !== 7) bad('初始应有 7 行，实有 ' + lines().length);
const errLn = lineWith('登录失败');
if (!errLn) bad('看不到那条失败行');
// 本仓库令牌表刻意单色（tokens.css：状态靠「形」而非「色」，没有红色令牌可指认），
// 「标红」落成的就是这条统一分级 class——样式与筛选共用同一个 levelOf，测它。
else if (!String(errLn.className).split(' ').includes('strong')) bad('错误行没走统一分级的强调态（.strong）：' + errLn.className);
const warnLn = lineWith('进入冷却');
if (!warnLn || !String(warnLn.className).split(' ').includes('dim')) bad('冷却行没标成警示态（.dim）');

/* ── 2. 五路合成：频道 AND 级别 AND 全文 AND 账号 ──────────────── */
await click(segBtn('对话', '任务'), '频道=任务');          await settle();
if (lines().length !== 4) bad('频道=任务应筛出 4 行，实有 ' + lines().length);
await click(segBtn('只看错误', '只看错误'), '级别=错误');  await settle();
if (lines().length !== 1 || !lines()[0].textContent.includes('登录失败')) {
  bad('频道+级别合成后应只剩登录失败那行：' + lines().map(l => l.textContent.trim()).join(' / '));
}
qin().value = '登录'; fire(qin(), 'input');
await tick(10);                                             // oninput 自己排了增量同步
await settle();
if (lines().length !== 1) bad('叠加全文搜索后应仍是 1 行');
const ac = chipWith('爱丽丝');
if (!ac) bad('账号筛选没给「爱丽丝」（她的 uid 明明在行里）：' + findAll(mount, 'fchip').map(c => c.textContent).join('/'));
else { await click(ac, '账号=爱丽丝'); await settle(); }
if (lines().length !== 1) bad('四路合成后应剩 1 行，实有 ' + lines().length);
if (!root.textContent.includes('命中 1 / 7 条')) bad('结果计数不对：' + ((findByClass(root, 'muted') || {}).textContent || ''));

/* ── 3. 空状态点名是哪几路排除了全部 ──────────────────────────── */
qin().value = 'zzz没有这个词'; fire(qin(), 'input');
await tick(10);
const em = findByClass(box(), 'empty');
if (!em) bad('筛到零结果时没有空状态');
else {
  const et = em.textContent;
  for (const k of ['搜索', '只看错误', '频道', '爱丽丝']) {
    if (!et.includes(k)) bad('空状态没说清是哪一路排除了全部（缺「' + k + '」）：' + et.slice(0, 80));
  }
  const cb = findButton(em, '清除筛选');
  if (!cb) bad('空状态没给「清除筛选」出口');
  else { await click(cb, '清除筛选'); await settle(); }
}
if (lines().length !== 7) bad('清除筛选后应回到 7 行，实有 ' + lines().length);

/* ── 4. 平台 chip 只给出现过的，并能筛 ────────────────────────── */
const pc = chipWith('Qoder');
if (!pc) bad('平台筛选没给 Qoder（acct=q-9 明明出现过）：' + findAll(mount, 'fchip').map(c => c.textContent).join('/'));
else { await click(pc, '平台=Qoder'); await settle(); }
if (lines().length !== 1 || !lines()[0].textContent.includes('acct=q-9')) {
  bad('按平台筛选后应只剩 qoder 那行，实有 ' + lines().map(l => l.textContent.trim()).join(' / '));
}
await click(chipWith('Qoder'), '再点取消平台筛选'); await settle();
if (lines().length !== 7) bad('取消平台筛选后没回到 7 行');

/* ── 5. 长行折叠：展开后扛得过增量追加，也扛得过整体重建 ──────── */
const longLn = lineWith('上游返回体');
if (!longLn) bad('找不到那条超长行');
else {
  if (longLn.textContent.includes('TAILMARK')) bad('长行没默认折叠（240 字阈值形同虚设）');
  if (!longLn.textContent.includes('error:')) bad('折叠保留的该是行首：' + longLn.textContent.slice(0, 40));
  const xb = findButton(longLn, '展开');
  if (!xb) bad('折叠行上没有「展开」入口：' + walk(longLn).buttons.join('/'));
  else {
    await click(xb, '展开');
    if (!longLn.textContent.includes('TAILMARK')) bad('展开后没给出完整长行');
    if (!findButton(longLn, '收起')) bad('展开后没给「收起」');
  }
}
LOG.push(mk(8, 'sys', 'keepalive 心跳 ' + (++EXTRA)));       // 5 秒 tick：走追加路
view.tick(); await tick(20);
await settle();
if (!lineWith('上游返回体') || !lineWith('上游返回体').textContent.includes('TAILMARK')) {
  bad('展开态没扛过增量追加——折叠态必须记在条目指纹上而不是 DOM 节点上');
}
await click(segBtn('对话', '任务'), '频道=任务'); await settle();   // 强制整表重建
await click(segBtn('对话', '全部'), '频道=全部'); await settle();
const long2 = lineWith('上游返回体');
if (!long2 || !long2.textContent.includes('TAILMARK')) bad('展开态没扛过整体重建');

/* ── 6. 行内账号名可点 → 详情抽屉；认不出账号的行不给死目标 ──── */
const chatLn = lineWith('model=glm 200');
const aBtn = chatLn && findByClass(chatLn, 'ln-a');
if (!aBtn) bad('行内 uid 没变成可点的账号名');
else {
  await click(aBtn, '账号跳转');
  const dr = drawerEl();
  const dt = dr ? (((qsel(dr, 'h2') || {}).textContent) || '') : '';
  if (dt !== '爱丽丝') bad('点行内账号没打开「爱丽丝」的详情抽屉，抽屉标题：「' + dt + '」');
}
if (findByClass(lineWith('panel 启动完成'), 'ln-a')) bad('认不出账号的行也给了可点目标（死链接）');

/* ── 7. 往上翻：新行不上屏只计数；点「↓ N 条新日志」才补 ─────── */
const b = box();
b.scrollHeight = 600; b.clientHeight = 300;
b.scrollTop = 0; fire(b, 'scroll');
LOG.push(mk(9, 'sys', 'balance 查询 ' + (++EXTRA)));
view.tick(); await tick(20);
await settle(3);
// 此刻 LOG 有 9 条（第 5 节追加过一条），屏上该停在 8 行
if (lines().length !== 8) bad('往上翻时新行不该上屏（应还是 8 行），实有 ' + lines().length);
const bar = findByClass(mount, 'newlogbar');
if (!bar) bad('往上翻后没有「↓ N 条新日志」的追新条');
else if (!bar.textContent.includes('↓ 1 条新日志')) bad('追新条计数不对：' + bar.textContent);
await click(findButton(bar, '条新日志'), '追新');
if (lines().length !== 9) bad('点追新条没把欠着的行补上屏，实有 ' + lines().length);
if (b.scrollTop !== 600) bad('补行后没跳到最新（scrollTop=' + b.scrollTop + '）');
await settle();
if (findByClass(mount, 'newlogbar')) bad('补完后追新条没收起');

/* ── 8. 滚回底部：恢复贴底、清零，之后的新行直接上屏 ─────────── */
b.scrollTop = 0; fire(b, 'scroll');
LOG.push(mk(10, 'sys', 'activity 领奖 ' + (++EXTRA)));
view.tick(); await tick(20);
await settle(3);
if (!findByClass(mount, 'newlogbar')) bad('又往上翻时新行该只计数，没出追新条');
b.scrollTop = 600; fire(b, 'scroll');        // 滚回底部 = 立刻补行并清零
await tick(10);
paint(); await tick(10);
if (findByClass(mount, 'newlogbar')) bad('滚回底部后追新条没消失（计数没清零）');
if (lines().length !== 10) bad('滚回底部没把欠着的行补上屏，实有 ' + lines().length);
LOG.push(mk(11, 'sys', 'panel 巡检 ' + (++EXTRA)));
view.tick(); await tick(20);
await settle();
if (findByClass(mount, 'newlogbar')) bad('贴底时新行该直接上屏，不该再出计数条');
if (!lines().some(l => l.textContent.includes('panel 巡检'))) bad('贴底时新行没自动上屏');

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('LOGS OK');
// 抽屉弹簧与追新计时都带着无 ref 的定时器；先落 stdout 再主动退出。
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "logs-page.mjs", harness, "LOGS OK", "执行记录页的筛选/折叠/追新不符")
}

// TestAddWizardSingleEntry 「两个入口合一」（清单 20）的源码级不变量：
// 添加账号只剩向导这一条路，旧抽屉里那份表单、ext-add 里那个平台下拉都不许复活
// ——同一件事画两遍，措辞一定会漂，而用户会以为是两个不同的功能。
func TestAddWizardSingleEntry(t *testing.T) {
	// 查的是代码而不是措辞：drawers.js 的说明注释里就写着「添加账号」去哪了，
	// 按中文搜会把这条注释当成第二个入口。真入口只看这几处引用。
	if s := readWebJS(t, "drawers.js"); strings.Contains(s, "openAddAccount") ||
		strings.Contains(s, "extAddPanel") || strings.Contains(s, "zaiAddForm") {
		t.Error("drawers.js 还留着添加账号表单的引用——第二个入口没删干净")
	}
	if s := readWebJS(t, "views/ext-add.js"); strings.Contains(s, "h('select'") {
		t.Error("ext-add.js 又自带平台下拉（选平台是向导第一步的事）")
	}
	if s := readWebJS(t, "boot.js"); !strings.Contains(s, "from './addwizard.js'") {
		t.Error("boot.js 没把添加账号入口接到向导上")
	}
	// extAddPanel 是向导的内部实现，别处不该再直接拼它。
	err := fs.WalkDir(webFS, "web/js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".js" {
			return err
		}
		base := filepath.Base(p)
		if base == "addwizard.js" || base == "ext-add.js" {
			return nil
		}
		data, rerr := fs.ReadFile(webFS, p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), "extAddPanel(") {
			t.Errorf("%s 绕过向导直接调用 extAddPanel", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runNodeHarness 把 js 写成 dir 下的 .mjs 并用 node 跑，要求输出含 marker。
func runNodeHarness(t *testing.T, node, dir, name, js, marker, failMsg string) {
	t.Helper()
	hf := filepath.Join(dir, name)
	if err := os.WriteFile(hf, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, hf)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", failMsg, err, out)
	}
	if !bytes.Contains(out, []byte(marker)) {
		t.Fatalf("%s（未打印 %s）:\n%s", failMsg, marker, out)
	}
}

// TestAccountTableView 「账号」页那张三源合一的表必须真的画对。
//
// 为什么需要：统一表格 + 详情抽屉是命令式建树（paint / replaceChildren），
// 语法检查与顶层求值冒烟都只证明「能 import」，不证明「画出来的是什么」——
// 行少一列、措辞回到「解冻」、chips 漏一个平台、批量条把 Z.AI 的「查额度」
// 当成「查余额」发出去、抽屉的曲线永远停在「读取中」（loader 落地没重画），
// 全是编译与 import 都拦不住、用户第一天就撞的错。无 node 时跳过。
func TestAccountTableView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; table render check skipped")
	}
	dir := copyModules(t)

	harness := domStub + `
/* ── fixture：三个数据源按后端真实形态给 ─────────────────────── */
const j = o => ({ ok: true, status: 200, json: async () => o });
const REG = [
  { id: 'workbuddy', name: '腾讯 WorkBuddy', group: 'gateway', prefix: 'cn:', login: 'oauth', checkin: true, renew: 'auto' },
  { id: 'zai', name: 'Z.AI / 智谱', group: 'gateway', prefix: 'zai:', login: 'oauth', checkin: true, renew: 'auto' },
  { id: 'loomy', name: 'Loomy（讯飞）', group: 'gateway', prefix: 'loomy:', login: 'detect', checkin: true, renew: 'auto' },
  { id: 'marvis', name: 'Marvis（马维斯）', group: 'gateway', prefix: 'marvis:', login: 'manual', renew: 'manual' },
];
const POOL = [
  { uid: 'uid-alice-0123456789', nickname: '爱丽丝', credits: 1200, credits_total: 5000, realm: 'cn', success_count: 40, err_total: 2 },
  { uid: 'uid-bob-0123456789', nickname: '鲍勃', credits: 800, credits_total: 1000, disabled: true, reason: '手动停用' },
  { uid: 'uid-cold-0123456789', nickname: '冷却号', credits: 300, credits_total: 1000, cool_remaining_sec: 90, cool_kind: 'hard_credit' },
];
const ZAI = { configured: true, accounts: [
  { id: 'z1', name: '主号', mode: 'jwt', status: 'active', enabled: true, masked: 'eyJz**.sig',
    quota: { 'glm-4.5': { remaining: 20, total: 100 } } },
  { id: 'z2', name: '失效号', mode: 'jwt', status: 'invalid', enabled: true, last_error: '401 token invalid' },
] };
const EXT = { ok: true, accounts: [
  { provider: 'loomy', id: 'lm-1', label: 'Loomy 主力', balance_ok: true, balance: 88 },
  { provider: 'marvis', id: 'mv-1', label: 'Marvis 抓包号' },
] };
const SERIES = { uid: 'uid-alice-0123456789', days: 7, points: [
  { t: '2026-10-02', requests: 1, total_tokens: 100 },
  { t: '2026-10-03', requests: 0, total_tokens: 0 },
  { t: '2026-10-04', requests: 2, total_tokens: 900 },
  { t: '2026-10-05', requests: 3, total_tokens: 1200 },
  { t: '2026-10-06', requests: 1, total_tokens: 300 },
  { t: '2026-10-07', requests: 4, total_tokens: 2000 },
  { t: '2026-10-08', requests: 2, total_tokens: 700 },
] };
const LOGS = { entries: [
  { ts: '2026-10-08T10:00:00Z', text: 'chat uid=uid-alice-0123456789 model=glm 200' },
  { ts: '2026-10-08T10:01:00Z', text: 'chat uid=uid-whoever-9999 model=glm 500' },
  { ts: '2026-10-08T10:02:00Z', text: 'checkin 爱丽丝(uid-alic) 429 限流' },
  { ts: '2026-10-08T10:03:00Z', text: 'route 无可用账号' },
] };
const TASKS = { tasks: [
  { task_code: 'chat_5', title: '对话 5 次', current: 5, target: 5, claimable: true, credit: 100 },
  { task_code: 'Buddy_App', title: '进入 Buddy 应用', current: 0, target: 1, accept_status: 'unaccepted' },
] };
const POSTS = [];   // 动作真实下发了什么（撤销链要能对上号）
// 动作之后视图会重新拉概览；桩必须照原样把池吐回来，否则「/api/overview」落到
// 兜底分支变成 {ok:true}，池就被清空，测试会误报成「行丢了」。
const OVERVIEW = { total: 3, healthy: 2, version: '1.11.8', redis_mode: 'local', uptime_sec: 120, accounts: POOL };
globalThis.fetch = async (url, opt) => {
  const u = String(url);
  // 带上 body：只记方法 + 路径的话，「往不存在的端点发」和「发对了端点」在
  // /disable|toggle/ 这类宽松正则下长得一样（Z.AI 只有 toggle 就是这个坑）。
  POSTS.push(((opt && opt.method) || 'GET') + ' ' + u + (opt && opt.body ? ' ' + String(opt.body) : ''));
  if (u.includes('/api/overview')) return j(OVERVIEW);
  if (u.includes('/api/platforms')) return j({ ok: true, platforms: REG });
  if (u.includes('/api/zai/accounts')) return j(ZAI);
  if (u.includes('/api/ext/accounts')) return j(EXT);
  if (u.includes('/api/usage/series')) return j(SERIES);
  if (u.includes('/api/logs')) return j(LOGS);
  if (u.includes('/tasks')) return j(TASKS);
  return j({ ok: true });
};

const problems = [];
const bad = m => problems.push(m);
const tick = async () => { for (let i = 0; i < 3; i++) await new Promise(r => setTimeout(r, 0)); };
const hasCls = (el, c) => String(el.className || '').split(' ').includes(c);
// 把 null / undefined 交给真实 DOM 会得到一个字面的 "null" 文本节点；
// 这里同时查文本子节点与被 text: 写进去的字符串，两种都是用户看得见的脏字。
const stray = el => { const out = []; (function walk(n) {
  for (const c of n.children || []) {
    if (String(c.tagName) === '#TEXT') { const t = c.textContent.trim(); if (['null', 'undefined', 'NaN', '[object Object]'].includes(t)) out.push(t); }
    else { const t = String(c._text || '').trim(); if (['null', 'undefined', 'NaN'].includes(t)) out.push(t + '@' + (c.className || c.tagName)); walk(c); }
  } })(el); return out; };
const rowsOf = el => findAll(el, 'acctrow').filter(r => !hasCls(r, 'head'));
const btnTexts = els => els.map(e => e.textContent.trim()).filter(Boolean);
const chipNM = c => { const n = findByClass(c, 'nm'); return n ? n.textContent.trim() : ''; };
const rowNamed = (el, nm) => rowsOf(el).find(r => { const c = findByClass(r, 'nm'); return c && c.textContent.trim() === nm; });

const { loadPlatforms } = await import('./platforms.js');
const { overview } = await import('./store.js');
const { loadZai, zaiData } = await import('./views/zai-segment.js');
const { loadExt, extData } = await import('./views/ext-segment.js');
const { closeMenu } = await import('./kernel.js');
await loadPlatforms(); await loadZai(); await loadExt();
overview.set(OVERVIEW);

const view = (await import('./views/accounts.js')).default;
let root = view.render();
// 面板把视图挂在 #view 容器里，桩也要挂：不挂的话宿主永远不在文档上，而视图
// 的 isConnected 守卫（这是 2026-10-09 冻结缺陷的修复）会正确地拒绝往没上屏的
// 树里画——测试里表现为「什么都没渲染」，其实测的是生产不存在的一种状态。
const viewHost = document.createElement('div');
document.body.append(viewHost);
viewHost.append(root);
await tick();

/* ── 1. 一张表：三个源排在一起（清单 I.3）──────────────────── */
if (!findByClass(root, 'acctlist')) bad('没画出 .acctlist 容器');
const got = rowsOf(root);
if (got.length !== 7) bad('表体 ' + got.length + ' 行，期望 7（池 3 + Z.AI 2 + 外部 2）');
if (findByClass(root, 'prail') || findByClass(root, 'acct-layout')) bad('第三栏（垂直平台导航）还在');

/* ── 2. 平台 chips：名字来自注册表，计数来自行模型 ─────────── */
const chips = findAll(root, 'fchip').map(chipNM);
for (const want of ['全部', '腾讯 WorkBuddy', 'Z.AI / 智谱', 'Loomy（讯飞）', 'Marvis（马维斯）', '需处理']) {
  if (!chips.includes(want)) bad('chips 缺「' + want + '」，实有 ' + chips.join('/'));
}
const allChip = findAll(root, 'fchip').find(c => chipNM(c) === '全部');
const allN = allChip ? findByClass(allChip, 'n') : null;
if (!allN || allN.textContent.trim() !== '7') bad('「全部」chip 的计数不是 7');

/* ── 3. 行瘦身 + 统一词表（清单 II.5 / II.7 / 49）──────────── */
const WORDS = new Set(['签到', '查余额', '启用', '查额度', '重新登录', '重新授权']);
for (const r of rowsOf(root)) {
  const bs = findAll(r, 'btn');
  const words = btnTexts(bs);
  if (bs.length !== 2) bad('行内应有「一个动作 + ⋯」两个按钮，实有 ' + bs.length + '：' + words.join('/'));
  if (words.length !== 1) bad('行内应只有一个带文案的动作：' + words.join('/'));
  if (words.length && !WORDS.has(words[0])) bad('行内动作词表外的说法：' + words[0]);
  if (bs.some(b => hasCls(b, 'primary'))) bad('列表里出现了主按钮（一屏只许一个）：' + words.join('/'));
  if (!findAll(r, 'cell').length) bad('行缺列容器 .cell');
}
const alice = rowNamed(root, '爱丽丝');
if (!alice) bad('没有「爱丽丝」这一行');
else if (!hasCls(findByClass(alice, 'st'), 'cell')) bad('行里没有状态列');
const cold = rowNamed(root, '冷却号');
if (!cold) bad('没有「冷却号」这一行');
else if (!findByClass(cold, 'st').textContent.includes('积分冷却')) bad('冷却行没说出原因：' + findByClass(cold, 'st').textContent);
const bob = rowNamed(root, '鲍勃');
if (!bob) bad('没有「鲍勃」这一行');
else if (btnTexts(findAll(bob, 'btn'))[0] !== '启用') bad('停用中的账号行主按钮应是「启用」');
const zbad = rowNamed(root, '失效号');
if (!zbad) bad('没有「失效号」这一行');
else if (btnTexts(findAll(zbad, 'btn'))[0] !== '重新登录') bad('凭证失效的账号行应给「重新登录」');
if (root.textContent.includes('解冻')) bad('界面上还出现「解冻」');
if (root.textContent.includes('extstore')) bad('界面上出现了实现细节词 extstore');
if (stray(root).length) bad('账号页渲染出脏字文本：' + stray(root).join('/'));

/* ── 3b. 数据重跑必须复用同一棵树（2026-10-09 浏览器实测的冻结缺陷）
      换整棵树时若被重绘护栏挂起，模块里的宿主还没上屏，用户点 chips、
      打字都画在一棵看不见的树上——表现是「账号页点什么都没反应」。 */
{
  const q0 = findByClass(root, 'search');
  const before = rowsOf(root).length;
  overview.set({ total: 4, healthy: 3, version: '1.11.8', redis_mode: 'local', uptime_sec: 200,
    accounts: [...POOL, { uid: 'uid-carol-0123456789', nickname: '卡罗尔', credits: 10, credits_total: 100, realm: 'cn' }] });
  const again = view.render();
  if (again !== root) {
    bad('数据重跑换了整棵树（视图必须复用已上屏的骨架，否则交互会画进没上屏的分身里）');
  } else {
    if (rowsOf(root).length !== before + 1) bad('数据重跑没就地重画：期望 ' + (before + 1) + ' 行，实有 ' + rowsOf(root).length);
    if (stray(root).length) bad('数据重跑后出现脏字文本：' + stray(root).join('/'));
    // 搜索框是非受控输入：重建一次就等于把焦点与光标位置扔掉一次
    if (findByClass(root, 'search') !== q0) bad('重绘重建了搜索框（焦点会随旧节点丢掉）');
  }
  overview.set({ total: 3, healthy: 2, version: '1.11.8', redis_mode: 'local', uptime_sec: 120, accounts: POOL });
  view.render();
}

/* ── 4. 搜索替代筛选界面（清单 I.3）────────────────────────── */
const q = findByClass(root, 'search');
if (!q) bad('没有搜索框');
else {
  q.value = 'alice'; fire(q, 'input');
  if (rowsOf(root).length !== 1) bad('搜 alice 应命中 1 行，实有 ' + rowsOf(root).length);
  q.value = 'loomy'; fire(q, 'input');
  if (rowsOf(root).length !== 1) bad('搜 loomy 应命中 1 行（平台名也算）');
  q.value = '查无此号'; fire(q, 'input');
  if (!findByClass(root, 'empty')) bad('搜不到时应出「没有匹配的账号」空状态');
  if (!findButton(root, '清除筛选')) bad('筛选空状态缺「清除筛选」');
  q.value = ''; fire(q, 'input');
  if (rowsOf(root).length !== 7) bad('清空搜索后应回到 7 行');
}

/* ── 5. 芯片筛选写进地址栏（可深链、后退不堆历史）───────────── */
const zaiChip = findAll(root, 'fchip').find(c => chipNM(c) === 'Z.AI / 智谱');
if (!zaiChip) bad('找不到 Z.AI 芯片');
else {
  fire(zaiChip, 'click');
  if (rowsOf(root).length !== 2) bad('按 Z.AI 筛选后应剩 2 行');
  if (location.hash !== '#accounts?tab=zai') bad('芯片筛选没落到 #accounts?tab=zai，实为 ' + location.hash);
  const back = findAll(root, 'fchip').find(c => chipNM(c) === '全部');
  fire(back, 'click');
  if (location.hash !== '#accounts') bad('回到「全部」后地址栏应清掉 tab 参数，实为 ' + location.hash);
}

/* ── 6. 多选 + 批量条：按行换算真实动作（清单 II.8）────────── */
for (const r of rowsOf(root)) fire(findByClass(r, 'cbx'), 'change');
const bar = findByClass(root, 'bulkbar');
if (!bar) bad('勾选后没浮出批量条');
else {
  const words = btnTexts(findAll(bar, 'btn'));
  // 池查余额、Z.AI 查额度，在批量条里都归到「查余额」；启用只有 1 个账号能点
  const want = ['启用 1', '停用 6', '签到 4', '查余额 5', '移除 7', '取消选择'];
  if (words.join(' ') !== want.join(' ')) bad('批量条动作/计数不符：' + words.join(' ') + '，期望 ' + want.join(' '));
  if (!findAll(bar, 'danger').length) bad('批量移除没标成危险态');
  const rm = findButton(bar, '移除');
  fire(rm, 'click');
  await tick();
  if (!document.documentElement.textContent.includes('将删除 7 个账号')) bad('批量删除没先确认就准备执行');
  fire(findButton(bar, '取消选择'), 'click');
  // 批量条宿主常驻（收起靠 hidden）：宿主一消失，吸底位置与行间距就会跳一下
  const bar2 = findByClass(root, 'bulkbar');
  if (!bar2 || !bar2.hidden || bar2.children.length) bad('取消选择后批量条没收起（应 hidden 且清空）');
}

/* ── 6b. 活动栏：批量操作变成看得见的作业（清单 14 / 16）────────
   原先一把批量只有一条 toast，跑完就得自己去日志翻哪个账号失败了。现在每条
   作业常驻右下角（挂在 shell 上，不是挂在账号页里），点开逐账号看结果，可逆的
   旁边直接给「撤销」。 */
{
  const { mountActivityBar, jobsSnapshot } = await import('./jobs.js');
  mountActivityBar();
  const host = qsel(document.body, '.activitybar');
  if (!host) bad('活动栏没挂到 body 上（挂在页面里就会随换页丢掉作业）');
  else if (!host.hidden) bad('没有作业时活动栏应收起');

  for (const r of rowsOf(root)) fire(findByClass(r, 'cbx'), 'change');
  const toggleBefore = POSTS.filter(p => /disable|toggle/.test(p)).length;
  fire(findButton(findByClass(root, 'bulkbar'), '停用'), 'click');
  await tick(); await tick();

  const jobs = jobsSnapshot();
  if (!jobs.length) bad('批量停用没记成作业');
  else {
    const j0 = jobs[0];
    if (j0.total !== 6 || j0.done !== 6) bad('作业计数应为 6/6，实为 ' + j0.done + '/' + j0.total);
    if (j0.bad !== 0) bad('桩里全部该成功，失败数应为 0，实为 ' + j0.bad);
    if (j0.state !== 'done') bad('跑完的作业状态应是 done，实为 ' + j0.state);
    if (POSTS.filter(p => /disable|toggle/.test(p)).length !== toggleBefore + 6) bad('停用没有逐账号下发');
    // Z.AI 只有翻转端点：enable/disable 这两个路径后端根本不存在，发过去就是 404
    // （停用按钮点了没反应、账号还活着）。批量与单行都必须走 toggle + {enabled}。
    const zaiWrong = POSTS.filter(p => /zai\/accounts\/[^/]+\/(enable|disable)/.test(p));
    if (zaiWrong.length) bad('Z.AI 启停打了不存在的端点：' + zaiWrong[0]);
    const zaiOff = POSTS.filter(p => /zai\/accounts\/[^/]+\/toggle/.test(p));
    if (!zaiOff.length) bad('Z.AI 停用没发 toggle');
    else if (!zaiOff.every(p => /"enabled"\s*:\s*false/.test(p))) bad('Z.AI toggle 没带 {"enabled":false}：' + zaiOff[0]);
  }
  if (host.hidden) bad('有作业时活动栏还收着');
  if (!host.textContent.includes('停用 6 个')) bad('作业标题没说清动作与数量：' + host.textContent.trim().slice(0, 40));
  // 起跑即收起批量条：进度与回执只有一个落点（活动栏），两个进度条不该同时存在
  if (!findByClass(root, 'bulkbar').hidden) bad('批量起跑后批量条没收起');

  fire(qsel(host, '.job-x'), 'click');                    // 展开看每个账号
  let items = findAll(qsel(document.body, '.activitybar'), 'job-item');
  if (items.length !== 6) bad('展开后逐账号结果应有 6 条，实有 ' + items.length);
  else {
    const undoable = items.filter(it => findButton(it, '撤销'));
    if (undoable.length !== 6) bad('停用是可逆动作，每条结果都该给「撤销」，实有 ' + undoable.length);
    const revBefore = POSTS.filter(p => /revive|toggle/.test(p)).length;
    fire(findButton(items[0], '撤销'), 'click');
    await tick(); await tick();
    if (POSTS.filter(p => /revive|toggle/.test(p)).length === revBefore) bad('点「撤销」没下发反向动作');
    // 撤销会重画作业卡，必须回屏幕上那一棵取，不能拿旧节点断言
    items = findAll(qsel(document.body, '.activitybar'), 'job-item');
    if (!items[0].textContent.includes('已撤销')) bad('撤销成功后结果行没标「已撤销」：' + items[0].textContent.trim());
    if (findButton(items[0], '撤销')) bad('已撤销的结果还给「撤销」（撤一次就够）');
  }
  if (stray(qsel(document.body, '.activitybar')).length) bad('活动栏渲染出脏字文本：' + stray(qsel(document.body, '.activitybar')).join('/'));
}

/* ── 6c. 单次动作：免确认直接生效，三态与撤销都落在行内（清单 15 / 16）── */
{
  const bob0 = rowNamed(root, '鲍勃');                     // 停用中的号，主按钮是「启用」
  if (!bob0) bad('批量作业跑完后「鲍勃」那行丢了');
  else {
    const reviveBefore = POSTS.filter(p => /revive/.test(p)).length;
    fire(qsel(bob0, '.btn'), 'click');
    // 动作还没落地时：按钮禁用并转圈，且没有弹确认框——可逆的直接生效
    const during = qsel(rowNamed(root, '鲍勃'), '.btn');
    if (!during) bad('执行中动作按钮丢了');
    else if (!hasCls(during, 'spin') || !during.disabled) bad('动作执行中行内按钮没进「转圈（禁用）」态');
    await tick(); await tick();
    if (POSTS.filter(p => /revive/.test(p)).length === reviveBefore) bad('「启用」没直接下发（不该等确认框）');
    const bob1 = rowNamed(root, '鲍勃');
    const btns1 = findAll(bob1, 'btn');
    const words = btnTexts(btns1);
    if (words.join(' ') !== '完成 撤销') bad('成功后行内应是「完成 + 撤销」，实为 ' + words.join('/'));
    if (!hasCls(btns1[0], 'ok')) bad('成功态没打勾（缺 .ok）');
    // ⋯ 在那一秒里必须还在：少一个按钮，行宽就跳一下
    if (btns1.length !== 3) bad('回执那一秒行内按钮数变了（⋯ 该常驻）：' + btns1.length);
    // 结果就地更新在行内，不再飘到顶部：动作成功后不新增 toast
    fire(findAll(bob1, 'btn')[1], 'click');                  // 撤销「启用」= 再停用
    await tick(); await tick();
    if (POSTS.filter(p => /\/disable/.test(p)).length === 0) bad('行内「撤销」没下发反向动作');
    const words2 = btnTexts(findAll(rowNamed(root, '鲍勃'), 'btn'));
    if (words2.join(' ') !== '已撤销') bad('撤销后行内回执不对：' + words2.join('/'));
  }
}

/* ── 7. ⋯ 菜单：其余动作 + 移除置末（清单 II.7）─────────────── */
const moreBtn = findAll(alice, 'btn').slice(-1)[0];
fire(moreBtn, 'click');
const menu = qsel(document.body, '.menu');
if (!menu) bad('点 ⋯ 没出菜单');
else {
  const words = btnTexts(findAll(menu, 'menu-item'));
  const want = ['查看详情', '查余额', '停用', '移除'];
  if (words.join(' ') !== want.join(' ')) bad('菜单项不对：' + words.join(' ') + '，期望 ' + want.join(' '));
  if (!hasCls(findAll(menu, 'menu-item').slice(-1)[0], 'danger')) bad('「移除」没排在末尾并标危险');
}
closeMenu();
if (qsel(document.body, '.menu')) bad('closeMenu 后菜单还挂在文档上');

/* ── 8. 详情抽屉：指标 / 近 7 天曲线 / 该账号日志 / 任务 / 凭证 / 全部操作
      （清单 II.6）——曲线与日志是异步落到子页的，loader 必须自己喊重画 */
fire(findByClass(alice, 'name'), 'click');
const drawer = qsel(document.body, '.drawer');
if (!drawer) bad('点行没打开详情抽屉');
await tick();
if (!findByClass(drawer, 'detail-status')) bad('抽屉没有状态行');
const mets = findAll(drawer, 'metric');
if (mets.length < 8) bad('抽屉指标不足 ' + mets.length + ' 个');
const k0 = findByClass(mets[0], 'k'), v0 = findByClass(mets[0], 'v');
if (!k0 || !v0 || !v0.textContent.includes('分')) bad('指标不是「标签在上、数值带单位在下」');
const cols = findAll(drawer, 'sp-col');
if (cols.length !== 7) bad('近 7 天曲线画出 ' + cols.length + ' 根柱子（期望 7；loader 落地没重画就是这个症状）');
if (!drawer.textContent.includes('凭证由网关自动续期')) bad('抽屉没说清凭证怎么续期');
const foot = findAll(drawer, 'detail-foot')[0];
const footWords = foot ? btnTexts(findAll(foot, 'btn')) : [];
if (footWords.join(' ') !== '概览 任务 日志 签到 查余额 停用 移除') bad('抽屉底部按钮不对：' + footWords.join(' '));
if (foot) {
  const actBtns = findAll(foot, 'btn').filter(b => ['签到', '查余额', '停用', '移除'].includes(b.textContent.trim()));
  if (actBtns.length !== 4) bad('抽屉动作按钮数量不对');
  else {
    if (!hasCls(actBtns[0], 'primary')) bad('抽屉里主行动没点亮（一屏一个主按钮）');
    if (actBtns.filter(b => hasCls(b, 'primary')).length !== 1) bad('抽屉里有多个主按钮');
    if (!hasCls(actBtns.slice(-1)[0], 'danger')) bad('抽屉里的「移除」没标危险态');
  }
}
/* 抽屉里的按钮三态（清单 15）：转圈（禁用）→ 原位打勾 1 秒。
   「原位」是重点：启用/停用这类动作成功后整排按钮会换一套（kind 已经不存在），
   ✓ 只有按位置落在用户刚点过的那一格，视线才接得上。 */
{
  fire(findAll(foot, 'btn')[3], 'click');                 // 签到
  const mid = findAll(foot, 'btn');
  if (!hasCls(mid[3], 'spin') || !mid[3].disabled) bad('抽屉动作执行中没进「转圈（禁用）」态');
  if (mid[3].textContent.trim() !== '执行中…') bad('抽屉执行中的文案不对：' + mid[3].textContent.trim());
  await tick(); await tick();
  const okBtns = findAll(foot, 'btn');
  if (!hasCls(okBtns[3], 'ok') || okBtns[3].textContent.trim() !== '完成') {
    bad('抽屉成功后没在刚点过的那一格打勾：' + btnTexts(okBtns).join('/'));
  }
  // 打勾那一秒整排按钮冻结在点击时的样子（换成新动作表会把 ✓ 挪到别的格子上）
  if (btnTexts(okBtns).join(' ') !== '概览 任务 日志 完成 查余额 停用 移除') {
    bad('打勾那一秒的按钮排布变了：' + btnTexts(okBtns).join('/'));
  }
  if (okBtns.slice(3).some(b => !b.disabled)) bad('打勾那一秒动作按钮没禁用（用户会点到过期动作）');
}
fire(findButton(foot, '日志'), 'click');
await tick();
const lines = findAll(drawer, 'ln');
if (lines.length !== 2) bad('该账号日志筛出 ' + lines.length + ' 行，期望 2（按 uid 与昵称两种写法命中）');
fire(findButton(foot, '任务'), 'click');
await tick();
const th = qsel(document.body, '.drawer .task-host');
if (!th) bad('任务子页没有 .task-host');
else if (!th.textContent.includes('chat_5')) bad('任务台账没重画（还是「查询任务进度」）：' + th.textContent.trim());

/* ── 9. 空池：空状态直接给按钮（清单 56）───────────────────── */
overview.set({ total: 0, healthy: 0, accounts: [], version: '1.11.8', redis_mode: 'local', uptime_sec: 1 });
zaiData.set({ configured: true, accounts: [] });
extData.set({ ok: true, accounts: [] });
root = view.render();
await tick();
if (!findButton(root, '添加账号')) bad('空池时没直接给「添加账号」按钮');
if (rowsOf(root).length) bad('空池还有残留行');

// 全页扫一遍：抽屉/菜单/toast 这些挂在 body 上的浮层也要一起查
if (stray(document.documentElement).length) bad('界面里出现脏字文本：' + stray(document.documentElement).join('/'));

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('TABLE OK');
// 活动栏的自动收起计时（8 秒/2 分钟）是无 ref 的定时器，node 会等它们跑完才退；
// 先让 stdout 落盘，再主动退出，别让一次绿测试挂成超时。
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "table.mjs", harness, "TABLE OK", "账号表渲染不符")
}

// TestPanelNavIA 导航骨架的源码级不变量：五个一级页、页与分段的归属、旧
// hash 全覆盖、被删掉的第三栏不许复活。
//
// 为什么需要：这些都属于「删一行照样跑」的结构约定——少一个一级页、某个
// 视图悄悄挂到不存在的一级页上、README 里的 #keys 深链没人接、第三栏的样式
// 留在 CSS 里被下一轮改动重新用上，浏览器里都是看得见的错，Go 编译器看不见。
func TestPanelNavIA(t *testing.T) {
	shell := readWebJS(t, "shell.js")

	// 一级页恰好五个，且顺序 = 侧栏顺序
	meta := sliceAround(shell, "export const PAGE_META = {", "\n};")
	if meta == "" {
		t.Fatal("shell.js 找不到 PAGE_META")
	}
	pages := regexp.MustCompile(`(?m)^\s*(\w+): \{`).FindAllStringSubmatch(meta, -1)
	var got []string
	for _, m := range pages {
		got = append(got, m[1])
	}
	wantPages := []string{"home", "accounts", "automation", "gateway", "settings"}
	if strings.Join(got, ",") != strings.Join(wantPages, ",") {
		t.Errorf("一级页 = %v，期望 %v", got, wantPages)
	}

	// 旧 hash 一个都不能断（README、收藏夹、日志里的深链）
	legacy := sliceAround(shell, "const LEGACY_ROUTE = {", "\n};")
	if legacy == "" {
		t.Fatal("shell.js 找不到 LEGACY_ROUTE")
	}
	for _, id := range []string{"overview", "accounts", "usage", "tasks", "logs", "keys", "models", "config"} {
		if !strings.Contains(legacy, id+": [") {
			t.Errorf("LEGACY_ROUTE 没有接住旧的 #%s", id)
		}
	}

	// 视图 → 一级页的归属（清单 I.1 / I.2：积分归账号页、调用统计归网关）
	wantView := map[string]string{
		"overview": "home", "accounts": "accounts", "credits": "accounts",
		"stats": "gateway", "models": "gateway", "keys": "gateway",
		"tasks": "automation", "logs": "automation", "config": "settings",
	}
	seen := map[string]string{}
	views := []string{"overview.js", "accounts.js", "usage.js", "models.js", "keys.js", "tasks.js", "logs.js", "config.js"}
	for _, f := range views {
		src := readWebJS(t, "views/"+f)
		for _, m := range regexp.MustCompile(`defineView\(\{\s*\n\s*id: '(\w[\w-]*)',\s*\n\s*page: '(\w+)'`).FindAllStringSubmatch(src, -1) {
			seen[m[1]] = m[2]
		}
	}
	for id, page := range wantView {
		if seen[id] != page {
			t.Errorf("视图 %q 挂在 %q，期望 %q", id, seen[id], page)
		}
	}
	if len(seen) != len(wantView) {
		t.Errorf("注册的分段有 %d 个，期望 %d：%v", len(seen), len(wantView), seen)
	}

	// 第三栏（垂直平台导航）必须真的删干净：JS 与 CSS 都不许再出现这些类名
	dead := []string{"prail", "acct-layout", "srow", "stask"}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := filepath.Ext(p)
		if ext != ".js" && ext != ".css" {
			return nil
		}
		data, rerr := fs.ReadFile(webFS, p)
		if rerr != nil {
			return rerr
		}
		for _, cls := range dead {
			if strings.Contains(string(data), cls) {
				t.Errorf("%s 又出现了第三栏的 %s（清单 I.3 已删）", p, cls)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
	if !strings.Contains(body, `type="module"`) {
		t.Error("index.html should load the entry as an ES module")
	}
}

// TestWebAssetsServed 模块与样式必须作为同源资源可取，且 Content-Type 正确
// （类型错了浏览器会拒绝执行模块 → 页面白屏）。
func TestWebAssetsServed(t *testing.T) {
	p := newTestPanel()
	cases := []struct{ path, ctype, want string }{
		{"/panel/js/boot.js", "javascript", "import"},
		{"/panel/js/kernel.js", "javascript", "export function signal"},
		{"/panel/css/tokens.css", "text/css", "--fg"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d want 200", c.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.ctype) {
			t.Errorf("%s Content-Type=%q want %s", c.path, ct, c.ctype)
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s 内容异常，未包含 %q", c.path, c.want)
		}
	}
}

// TestAssetTraversalBlocked 静态资源服务不得穿越目录或吐出未白名单的文件。
// 穿越路径由 net/http 的 mux 先行 301/307 归一（重定向目标是 /panel 之外，
// 不会落到本 handler），未白名单与不存在的路径必须 404。
func TestAssetTraversalBlocked(t *testing.T) {
	p := newTestPanel()
	cases := []struct {
		path string
		want []int
	}{
		{"/panel/../index.go", []int{301, 307, 404}},
		{"/panel/js/../../../go.mod", []int{301, 307, 404}},
		{"/panel/js/nope.js", []int{404}},
		{"/panel/js/boot.txt", []int{404}},
		{"/panel/panel.go", []int{404}},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		ok := false
		for _, w := range c.want {
			if rec.Code == w {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s code=%d want one of %v", c.path, rec.Code, c.want)
		}
	}
}
