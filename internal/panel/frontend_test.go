package panel

import (
	"bytes"
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
  setAttribute(k, v) { if (k === 'class') { this.className = v; return; } this.props[k] = String(v); }
  getAttribute(k) { return this.props[k] ?? null; }
  removeAttribute(k) { delete this.props[k]; }
  setAttributeNS(a, k, v) { this.setAttribute(k, v); }
  addEventListener(t, f) { (this.listeners[t] ||= []).push(f); }
  removeEventListener() {}
  appendChild(c) { this.children.push(c); c.parentNode = this; c.isConnected = this.isConnected; return c; }
  // 真实 DOM 的 append/replaceChildren 只接受 Node 或字符串：传 null 不会报错，
  // 而是渲染出一个字面的 "null" 文本节点（2026-10-09 浏览器实测踩过）。
  // 桩以前把 null 悄悄滤掉，于是这类缺陷在测试里永远是绿的——现在照浏览器画。
  _node(c) { return (c && c.tagName) ? c : globalThis.document.createTextNode(String(c)); }
  append(...cs) { for (const c of cs) this.appendChild(this._node(c)); }
  prepend(...cs) { this.children.unshift(...cs); }
  before() {} after() {}
  remove() { this.isConnected = false; const p = this.parentNode;
    if (p) { const i = p.children.indexOf(this); if (i >= 0) p.children.splice(i, 1); } }
  replaceWith(n) { const p = this.parentNode; if (!p) return;
    const i = p.children.indexOf(this); if (i >= 0) { p.children[i] = n; n.parentNode = p; }
    this.isConnected = false; }
  insertBefore(c) { return this.appendChild(c); }
  // 被换下去的节点要断开（视图靠 isConnected 拒绝写进废弃容器，抽屉的 loader 也靠它）
  replaceChildren(...cs) { const next = cs.map(c => this._node(c));
    for (const old of this.children) if (!next.includes(old)) old.isConnected = false;
    this.children = next; for (const c of this.children) { c.parentNode = this; c.isConnected = true; } }
  focus() {}
  get firstChild() { return this.children[0] ?? null; }
  get lastChild() { return this.children[this.children.length - 1] ?? null; }
  get firstElementChild() { return this.children.find(c => !String(c.tagName).startsWith('#')) ?? null; }
  get childElementCount() { return this.children.filter(c => !String(c.tagName).startsWith('#')).length; }
  get textContent() { return this._text || this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get value() { return this._value ?? ''; }
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
// html/head/body 真的连起来：抽屉、菜单、toast 都 append 到 body，
// 而 loadTasks 这类「按选择器回找自己那一点」的写法必须能找到才测得准。
const htmlEl = new El('html'), headEl = new El('head'), bodyEl = new El('body');
htmlEl.append(headEl, bodyEl);
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

// TestAddPanelsRender 「添加账号」各平台表单必须真的渲染出对应字段。
//
// 为什么需要：这几个面板是命令式的（切换平台只重绘字段区，不重建整棵子树），
// 所以语法检查与顶层求值冒烟都看不见它们——字段漏一个、设备流分支抛异常、
// 或平台切换后字段不刷新，Go 侧测试全绿而用户看到的是空表单。无 node 时跳过。
func TestAddPanelsRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	harness := domStub + `
// 平台注册表：面板所有平台清单都从它来（js/platforms.js），测试里模拟后端下发。
const PLATFORMS = [
  { id: 'lobsterai', name: 'LobsterAI（有道）', group: 'points', login: 'manual', checkin: true },
  { id: 'raccoon', name: '小浣熊（商汤）', group: 'points', login: 'qr', checkin: true },
  { id: 'qoder', name: 'Qoder（阿里）', group: 'gateway', prefix: 'qoder:', login: 'device', checkin: true },
  { id: 'codearts', name: 'CodeArts（华为云）', group: 'points', login: 'manual', checkin: true },
  { id: 'copilot', name: 'GitHub Copilot', group: 'gateway', prefix: 'copilot:', login: 'code' },
  { id: 'cline', name: 'Cline', group: 'gateway', prefix: 'cline:', login: 'code' },
  { id: 'autoclaw', name: 'AutoClaw（智谱）', group: 'gateway', prefix: 'autoclaw:', login: 'sms' },
  { id: 'qclaw', name: 'QClaw（腾讯）', group: 'gateway', prefix: 'qclaw:', login: 'paste' },
  { id: 'trae', name: 'Trae（字节）', group: 'gateway', prefix: 'trae:', login: 'callback' },
  { id: 'accio', name: 'Accio（阿里）', group: 'gateway', prefix: 'accio:', login: 'callback' },
  { id: 'traework', name: 'TraeWork（字节）', group: 'gateway', prefix: 'traework:', login: 'manual', checkin: true },
];

// fetch 桩必须**先**装好再拉注册表，否则会挂在前面那个永不 resolve 的空桩上
globalThis.fetch = async url => {
  if (String(url).includes('/api/platforms')) {
    return { ok: true, status: 200, json: async () => ({ ok: true, platforms: PLATFORMS }) };
  }
  return { ok: true, status: 200, json: async () => ({}) };
};

const { extAddPanel } = await import('./views/ext-add.js');
const { zaiAddForm } = await import('./views/zai-segment.js');
const { loadPlatforms } = await import('./platforms.js');
await loadPlatforms(); // 先把注册表灌进缓存，面板首次渲染就是完整的

// 每个平台的凭据字段数（账号标识输入框另计）。
// 有登录方式的平台把凭据表单收进折叠区，字段仍然要渲染出来（手工兜底路径）。
const WANT = {
  lobsterai: { fields: 4, login: null },
  raccoon: { fields: 2, login: '微信扫码登录' },
  qoder: { fields: 4, login: '浏览器授权登录' },
  codearts: { fields: 3, login: null },
  traework: { fields: 3, login: null },
  copilot: { fields: 0, login: '开始授权' },
  cline: { fields: 0, login: '开始授权' },
  // 短信登录没有「发起」按钮，直接出表单（发码 → 校验两段同步调用），
  // 输入框是「手机号 + 验证码」两个，不走「标识框 + N 个字段」那套。
  autoclaw: { fields: 0, login: '发送验证码', inputs: 2 },
  // 扫码回填：点「微信扫码登录」才出二维码与回填框（发起前只有一个按钮）
  qclaw: { fields: 0, login: '微信扫码登录' },
  // 本机回调复用「浏览器授权」交互（发起前只有一个按钮）
  trae: { fields: 0, login: '浏览器授权登录' },
  accio: { fields: 0, login: '浏览器授权登录' },
};
const problems = [];
for (const [provider, want] of Object.entries(WANT)) {
  const panel = extAddPanel(() => {}, { flat: true });
  const sel = panel.children[0].children[0].children[0];
  if (!sel || (sel.tagName || '').toLowerCase() !== 'select') { problems.push(provider + ': 未找到平台选择器'); continue; }
  sel.value = provider;
  for (const f of sel.listeners.change || []) f({ target: sel });
  const got = walk(panel);

  // Copilot 没有可手填的凭据字段，不该出现空表单；inputs 可显式覆盖
  const wantInputs = want.inputs != null ? want.inputs : (want.fields === 0 ? 0 : want.fields + 1);
  if (got.inputs !== wantInputs) problems.push(provider + ': 输入框 ' + got.inputs + ' 个，期望 ' + wantInputs);

  if (want.login) {
    if (!has(got.buttons, want.login)) problems.push(provider + ': 缺「' + want.login + '」按钮');
  }
  // 有凭据字段就必须有手工添加入口；没有字段就不该有
  if (want.fields > 0 && !has(got.buttons, '添加')) problems.push(provider + ': 缺手工「添加」按钮');
  if (want.fields === 0 && has(got.buttons, '添加')) problems.push(provider + ': 无凭据字段却出现「添加」按钮');
}

const zai = walk(zaiAddForm(() => {}));
if (zai.inputs !== 2) problems.push('zai: 输入框 ' + zai.inputs + ' 个，期望 2');
if (!has(zai.buttons, '入池')) problems.push('zai: 缺「入池」按钮');
if (!has(zai.buttons, 'OAuth')) problems.push('zai: 缺「OAuth 免密登录」按钮');

/* ── 二维码必须是真的 SVG 元素，不是标记字符串 ──────────────────
   h() 把字符串当文本节点，传标记进去会把 "<svg ...>" 原样显示出来。 */
const { qrMatrix, qrSVG } = await import('./qr.js');
const qrText = 'https://xiaohuanxiong.com/login/mp?code=abc';
const qrM = qrMatrix(qrText);
const svg = qrSVG(qrM, 160);
if (typeof svg === 'string') problems.push('qrSVG 返回字符串——h() 会当成文本节点，二维码不显示');
else {
  if ((svg.tagName || '').toLowerCase() !== 'svg') problems.push('qrSVG 未返回 svg 元素');
  const paths = (svg.children || []).filter(c => (c.tagName || '').toLowerCase() === 'path');
  if (!paths.length) problems.push('qrSVG 没有深色模块 path');
  else if (!paths[0].props.d || paths[0].props.d.length < 20) problems.push('qrSVG 的 path d 为空');
  const wantVB = '0 0 ' + (qrM.length + 8) + ' ' + (qrM.length + 8);
  if (svg.props['viewBox'] !== wantVB) problems.push('qrSVG viewBox = ' + svg.props['viewBox'] + '，期望 ' + wantVB);
}

/* ── 驱动一次真实登录：点按钮 → 挑战物必须渲染出来 ──────────────
   登录面板是命令式的，只有走一遍点击才覆盖到 renderChallenge。 */
globalThis.fetch = async url => {
  if (String(url).includes('/api/platforms')) {
    return { ok: true, status: 200, json: async () => ({ ok: true, platforms: PLATFORMS }) };
  }
  return { ok: true, status: 200,
    json: async () => ({ ok: true, mode: 'qr', session: 's1',
      qr_url: 'https://xiaohuanxiong.com/login/mp?code=abc&appname=x', expires_in: 600 }) };
};

for (const [provider, btnText, wantCls] of [['raccoon', '微信扫码登录', 'qr']]) {
  const panel = extAddPanel(() => {}, { flat: true });
  const sel = panel.children[0].children[0].children[0];
  sel.value = provider;
  for (const f of sel.listeners.change || []) f({ target: sel });
  const btn = findButton(panel, btnText);
  if (!btn) { problems.push(provider + ': 找不到「' + btnText + '」按钮'); continue; }
  // 两种绑定方式都要认：h(..., {onclick}) 走 addEventListener，btn.onclick= 是属性赋值
  const click = btn.onclick || (btn.listeners.click || [])[0];
  if (typeof click !== 'function') { problems.push(provider + ': 登录按钮没绑点击'); continue; }
  await click({ currentTarget: btn });
  const box = findByClass(panel, wantCls);
  if (!box) { problems.push(provider + ': 点登录后没渲染出 .' + wantCls); continue; }
  const inner = (box.children || [])[0];
  if (!inner || (inner.tagName || '').toLowerCase() !== 'svg') {
    problems.push(provider + ': .' + wantCls + ' 里不是 svg 元素');
  }
}
const { stopExtAddTimers } = await import('./views/ext-add.js');
stopExtAddTimers();

/* ── 面板被 tick 重建后，登录轮询必须继续 ──────────────────────
   自动化视图每 5s 重渲染一次，会重建 extAddPanel。早先的实现在构造时清掉
   轮询定时器，于是「授权完成却没人接着轮询」——用户看到的就是授权后没入池。
   实测小浣熊扫码快能成，Qoder / Copilot 要在浏览器里操作更久，必死。 */
let pollCount = 0, addedCount = 0;
globalThis.fetch = async url => {
  if (String(url).includes('/api/platforms')) {
    return { ok: true, status: 200, json: async () => ({ ok: true, platforms: PLATFORMS }) };
  }
  if (String(url).includes('/login/start')) {
    return { ok: true, status: 200, json: async () => ({ ok: true, mode: 'device', session: 'sess-A',
      auth_url: 'https://qoder.com/device/selectAccounts?x=1', expires_in: 600 }) };
  }
  pollCount++;
  return { ok: true, status: 200, json: async () => ({ ok: true, done: true,
    account: { id: 'q-1', label: 'Qoder q-1' } }) };
};

const p1 = extAddPanel(() => { addedCount++; }, { flat: true });
const sel1 = p1.children[0].children[0].children[0];
sel1.value = 'qoder';
for (const f of sel1.listeners.change || []) f({ target: sel1 });
const lbtn = findButton(p1, '浏览器授权登录');
await (lbtn.onclick || (lbtn.listeners.click || [])[0])({ currentTarget: lbtn });

// 模拟视图 tick：重建面板（旧实现会在这里把轮询掐死）
extAddPanel(() => { addedCount++; }, { flat: true });

await new Promise(r => setTimeout(r, 2600));
stopExtAddTimers();
if (pollCount === 0) problems.push('面板重建后轮询停了——登录永远不会完成（账号不会入池）');
if (addedCount === 0) problems.push('轮询成功后没回调 onAdded，账号列表不会刷新');

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('RENDER OK');
`
	hf := filepath.Join(dir, "render.mjs")
	if err := os.WriteFile(hf, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, hf)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("添加账号表单渲染失败: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("RENDER OK")) {
		t.Fatalf("渲染检查未通过:\n%s", out)
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
globalThis.fetch = async url => {
  const u = String(url);
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
overview.set({ total: 3, healthy: 2, version: '1.11.8', redis_mode: 'local', uptime_sec: 120, accounts: POOL });

const view = (await import('./views/accounts.js')).default;
let root = view.render();
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
