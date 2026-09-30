package panel

import (
	"bytes"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, hash := range []string{"#overview", "#accounts", "#usage", "#automation", "#models", "#keys", "#config", "#logs"} {
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

	// 真实 DOM 桩（不是 Proxy 黑洞）：够 h() 建树并回读结构。
	harness := `class El {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.props = {};
    this.style = {}; this.dataset = {}; this.listeners = {}; this._text = ''; this.isConnected = true; }
  setAttribute(k, v) { this.props[k] = String(v); }
  getAttribute(k) { return this.props[k] ?? null; }
  removeAttribute(k) { delete this.props[k]; }
  setAttributeNS(a, k, v) { this.setAttribute(k, v); }
  addEventListener(t, f) { (this.listeners[t] ||= []).push(f); }
  removeEventListener() {}
  appendChild(c) { this.children.push(c); c.parentNode = this; return c; }
  append(...cs) { for (const c of cs) this.appendChild(c); }
  prepend(...cs) { this.children.unshift(...cs); }
  before() {} after() {} remove() { this.isConnected = false; }
  insertBefore(c) { return this.appendChild(c); }
  replaceChildren(...cs) { this.children = cs; for (const c of cs) c.parentNode = this; }
  focus() {}
  get firstChild() { return this.children[0] ?? null; }
  get lastChild() { return this.children[this.children.length - 1] ?? null; }
  get textContent() { return this._text || this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get classList() { const self = this; return { add: c => self._cls(c, 1), remove: c => self._cls(c, 0) }; }
  _cls(c, on) { const s = new Set((this.props.class || '').split(' ').filter(Boolean));
    on ? s.add(c) : s.delete(c); this.props.class = [...s].join(' '); }
  get value() { return this._value ?? ''; }
  set value(v) { this._value = v; }
  querySelector() { return null; }
  closest() { return null; }
}
globalThis.Node = El; globalThis.Element = El; globalThis.HTMLElement = El;
globalThis.document = {
  createElement: t => new El(t), createElementNS: (ns, t) => new El(t),
  createDocumentFragment: () => new El('#fragment'),
  createTextNode: t => { const e = new El('#text'); e._text = t; return e; },
  getElementById: () => null, querySelector: () => null, querySelectorAll: () => [],
  addEventListener() {}, removeEventListener() {},
  documentElement: new El('html'), head: new El('head'), body: new El('body'), cookie: '',
};
globalThis.window = { addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }),
  open() {}, location: { hash: '', href: '' } };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
Object.defineProperty(globalThis, 'navigator',
  { value: { clipboard: { writeText: () => Promise.resolve() } }, configurable: true, writable: true });
globalThis.matchMedia = globalThis.window.matchMedia;
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

const { extAddPanel } = await import('./views/ext-add.js');
const { zaiAddForm } = await import('./views/zai-segment.js');

// 每个平台的凭据字段数（账号标识输入框另计）。
// 有登录方式的平台把凭据表单收进折叠区，字段仍然要渲染出来（手工兜底路径）。
const WANT = {
  lobsterai: { fields: 4, login: null },
  raccoon: { fields: 2, login: '微信扫码登录' },
  qoder: { fields: 4, login: '浏览器授权登录' },
  codearts: { fields: 3, login: null },
  copilot: { fields: 0, login: '开始授权' },
};
const problems = [];
for (const [provider, want] of Object.entries(WANT)) {
  const panel = extAddPanel(() => {}, { flat: true });
  const sel = panel.children[0].children[0].children[0];
  if (!sel || (sel.tagName || '').toLowerCase() !== 'select') { problems.push(provider + ': 未找到平台选择器'); continue; }
  sel.value = provider;
  for (const f of sel.listeners.change || []) f({ target: sel });
  const got = walk(panel);

  // Copilot 没有可手填的凭据字段，不该出现空表单
  const wantInputs = want.fields === 0 ? 0 : want.fields + 1;
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
globalThis.fetch = async () => ({
  ok: true, status: 200,
  json: async () => ({ ok: true, mode: 'qr', session: 's1',
    qr_url: 'https://xiaohuanxiong.com/login/mp?code=abc&appname=x', expires_in: 600 }),
});

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
