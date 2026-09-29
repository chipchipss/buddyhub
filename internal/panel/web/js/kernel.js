/* ══════════════════════════════════════════════════════════════════
   kernel.js · 响应式微内核
   从零实现，不依赖任何库，也无构建步骤：
     signal / effect   —— 信号与自动追踪的副作用（视图在依赖变化时自动重建）
     h / svg / frag    —— 声明式 DOM 构建（不拼字符串 → 天然免疫注入）
     api / toast / drawer / sheet / confirm —— 与后端和界面原语
   视图 = 一个返回 DOM 节点的函数；渲染期间读到的信号变化时，框架自动换掉旧节点。
   ══════════════════════════════════════════════════════════════════ */

/* ── 调度：同一轮微任务内的多次 set 合并成一次重渲染 ───────────── */
let ACTIVE = null;              // 正在收集依赖的 effect
const dirty = new Set();        // 待重跑的 effect
let scheduled = false;

function schedule(e) {
  dirty.add(e);
  if (scheduled) return;
  scheduled = true;
  queueMicrotask(() => {
    scheduled = false;
    const jobs = [...dirty];
    dirty.clear();
    for (const job of jobs) job.run();
  });
}

/* ── signal：可读可写的响应式值 ───────────────────────────────── */
export function signal(init) {
  let value = init;
  const subs = new Set();
  const read = () => {
    if (ACTIVE) { ACTIVE.deps.add(subs); subs.add(ACTIVE); }
    return value;
  };
  read.set = next => {
    const v = typeof next === 'function' ? next(value) : next;
    if (Object.is(v, value)) return;
    value = v;
    for (const e of [...subs]) schedule(e);
  };
  read.peek = () => value;
  read.subscribe = fn => { const s = { deps: new Set(), run: fn }; subs.add(s); return () => subs.delete(s); };
  return read;
}

/* ── effect：自动追踪依赖；返回 DOM 时自动替换旧节点 ──────────── */
export function effect(fn) {
  const e = {
    deps: new Set(),
    node: null,
    disposed: false,
    dispose() { e.disposed = true; for (const s of e.deps) s.delete(e); e.deps.clear(); },
    run() {
      if (e.disposed) { return; }
      for (const s of e.deps) s.delete(e);
      e.deps.clear();
      const prev = ACTIVE;
      ACTIVE = e;
      let out;
      try {
        out = fn();
      } catch (err) {
        console.error('[view]', err);
        out = h('div', { class: 'empty' }, h('div', { class: 't' }, '渲染出错'), h('div', { class: 'd' }, String(err && err.message || err)));
      } finally {
        ACTIVE = prev;
      }
      if (out instanceof Node) {
        if (e.node && e.node.parentNode) e.node.replaceWith(out);
        e.node = out;
      }
      return out;
    },
  };
  e.run();
  return e;
}

/* ── DOM 构建 ─────────────────────────────────────────────────── */
function append(el, kids) {
  for (const k of kids.flat(Infinity)) {
    if (k == null || k === false || k === true) continue;
    el.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
}

function applyProps(el, props) {
  if (!props) return;
  const isSvg = typeof SVGElement !== 'undefined' && el instanceof SVGElement;
  for (const [k, v] of Object.entries(props)) {
    if (v == null || v === false) continue;
    if (k === 'class') {
      // SVG 的 className 是只读的 SVGAnimatedString，必须走 setAttribute
      if (isSvg) el.setAttribute('class', v);
      else el.className = v;
    }
    else if (k === 'text') el.textContent = v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
}

/** h('div', {class:'x'}, child, ...) */
export function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  applyProps(el, props);
  append(el, kids);
  return el;
}

/** SVG 命名空间元素（图标与图表用）*/
export function svgEl(tag, props, ...kids) {
  const el = document.createElementNS('http://www.w3.org/2000/svg', tag);
  applyProps(el, props);
  append(el, kids);
  return el;
}

/** 文档片段 */
export function frag(...kids) {
  const f = document.createDocumentFragment();
  append(f, kids);
  return f;
}

/* ── 请求 ─────────────────────────────────────────────────────── */
const LS_KEY = 'buddyhub.key', LS_THEME = 'buddyhub.theme';

export function getKey() { try { return localStorage.getItem(LS_KEY); } catch { return null; } }
export function setKey(k) { try { localStorage.setItem(LS_KEY, k); } catch { /* 私密模式 */ } }

export async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  const k = getKey();
  if (k) headers['Authorization'] = 'Bearer ' + k;
  if (opts.body && typeof opts.body === 'string') headers['Content-Type'] = 'application/json';
  const res = await fetch('/panel/api/' + path, { ...opts, headers });
  if (res.status === 401) { onUnauthorized?.(); throw new Error('密钥无效或未填写'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}

/** 带文件的表单提交（导入账号用；不能设 Content-Type，浏览器要自己加 boundary）*/
export async function apiUpload(path, form) {
  const headers = {};
  const k = getKey();
  if (k) headers['Authorization'] = 'Bearer ' + k;
  const res = await fetch('/panel/api/' + path, { method: 'POST', body: form, headers });
  if (res.status === 401) { onUnauthorized?.(); throw new Error('密钥无效或未填写'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}

let onUnauthorized = null;
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

/* ── Toast ────────────────────────────────────────────────────── */
export function toast(msg, kind) {
  const host = document.getElementById('toasts');
  if (!host) return;
  const el = h('div', { class: 'toast' + (kind === 'fail' ? ' fail' : ''), text: msg });
  host.append(el);
  setTimeout(() => {
    el.style.transition = 'opacity .3s, transform .3s';
    el.style.opacity = '0';
    el.style.transform = 'translateY(6px)';
    setTimeout(() => el.remove(), 320);
  }, 3400);
}

/* ── 抽屉 ─────────────────────────────────────────────────────── */
let drawerEl = null, scrimEl = null;

export function ensureLayers() {
  if (scrimEl) return;
  scrimEl = h('div', { class: 'scrim', onclick: () => closeDrawer() });
  drawerEl = h('aside', { class: 'drawer', 'aria-hidden': 'true' });
  document.body.append(scrimEl, drawerEl);
}

/** openDrawer({ title, hint, body, footer }) —— body/footer 为 DOM 节点 */
export function openDrawer({ title, hint, body, footer }) {
  ensureLayers();
  drawerEl.replaceChildren(
    h('header', null,
      h('div', { class: 'grow' }, h('h2', { text: title }), hint ? h('div', { class: 'hint', text: hint }) : null),
      h('button', { class: 'btn icon ghost', title: '关闭', onclick: closeDrawer }, icon('close')),
    ),
    h('div', { class: 'body' }, body),
    footer ? h('footer', null, footer) : null,
  );
  scrimEl.classList.add('on');
  drawerEl.classList.add('on');
  drawerEl.setAttribute('aria-hidden', 'false');
}

export function closeDrawer() {
  if (!drawerEl) return;
  scrimEl.classList.remove('on');
  drawerEl.classList.remove('on');
  drawerEl.setAttribute('aria-hidden', 'true');
  setTimeout(() => { if (!drawerEl.classList.contains('on')) drawerEl.replaceChildren(); }, 360);
}

/* ── 确认框（单色玻璃，替代原生 confirm）──────────────────────── */
export function confirmDialog(message, { ok = '确认', cancel = '取消' } = {}) {
  return new Promise(resolve => {
    const wrap = h('div', { class: 'sheet-wrap on' });
    const done = val => { wrap.remove(); document.removeEventListener('keydown', onKey); resolve(val); };
    const onKey = ev => { if (ev.key === 'Escape') done(false); };
    document.addEventListener('keydown', onKey);
    wrap.append(
      h('div', { class: 'sheet' },
        h('h2', { text: '请确认' }),
        h('div', { class: 'hint', text: message }),
        h('div', { class: 'row', style: { marginTop: '20px', justifyContent: 'flex-end' } },
          h('button', { class: 'btn', text: cancel, onclick: () => done(false) }),
          h('button', { class: 'btn primary', text: ok, onclick: () => done(true) }),
        ),
      ),
    );
    wrap.addEventListener('click', ev => { if (ev.target === wrap) done(false); });
    document.body.append(wrap);
    wrap.querySelector('.btn.primary').focus();
  });
}

/* ── 复制（非安全上下文降级）────────────────────────────────── */
export function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.append(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

/* ── busy：包装异步点击处理器 ───────────────────────────────────
   执行期间禁用按钮。必须在派发期间同步捕获节点（await 之后
   ev.currentTarget 已被置 null——事件生命周期结束的浏览器行为）。*/
export function busy(fn) {
  return async ev => {
    const b = ev.currentTarget;
    if (b) b.disabled = true;
    try { await fn(ev); }
    finally { if (b) b.disabled = false; }
  };
}

/* ── 图标（单色线性，currentColor）──────────────────────────── */
const PATHS = {
  overview: 'M2.5 8.5 8 3.5l5.5 5M4 7.5V13h8V7.5',
  accounts: 'M8 7.6a2.4 2.4 0 1 0 0-4.8 2.4 2.4 0 0 0 0 4.8ZM3.2 13.4c.5-2.4 2.4-3.7 4.8-3.7s4.3 1.3 4.8 3.7',
  automation: 'M8 2.4v2.2M8 11.4v2.2M2.4 8h2.2M11.4 8h2.2M4.4 4.4l1.6 1.6M10 10l1.6 1.6M11.6 4.4 10 6M6 10l-1.6 1.6',
  models: 'M8 2.2 13.5 5v6L8 13.8 2.5 11V5z M2.5 5 8 7.9 13.5 5M8 7.9v5.9',
  usage: 'M2.4 13.2h11.2M4.6 13.2V8.4M8 13.2V4.2M11.4 13.2V9.6',
  keys: 'M6.4 10.4a2.9 2.9 0 1 0 0-5.8 2.9 2.9 0 0 0 0 5.8ZM8.6 7.5 13.4 2.7M11.2 2.7h2.2v2.2',
  config: 'M8 9.7a1.7 1.7 0 1 0 0-3.4 1.7 1.7 0 0 0 0 3.4ZM8 1.9v1.7M8 12.4v1.7M1.9 8h1.7M12.4 8h1.7M3.7 3.7l1.2 1.2M11.1 11.1l1.2 1.2M12.3 3.7l-1.2 1.2M4.9 11.1 3.7 12.3',
  logs: 'M2.6 3.8h10.8M2.6 8h10.8M2.6 12.2h6.6',
  refresh: 'M13 8a5 5 0 1 1-1.6-3.7M13 2.6v2.6h-2.6',
  plus: 'M8 3.4v9.2M3.4 8h9.2',
  sun: 'M8 10.6a2.6 2.6 0 1 0 0-5.2 2.6 2.6 0 0 0 0 5.2ZM8 1.6v1.6M8 12.8v1.6M1.6 8h1.6M12.8 8h1.6M3.5 3.5l1.1 1.1M11.4 11.4l1.1 1.1M12.5 3.5l-1.1 1.1M4.6 11.4l-1.1 1.1',
  moon: 'M13.2 9.7A5.6 5.6 0 0 1 6.3 2.8a5.6 5.6 0 1 0 6.9 6.9Z',
  close: 'M4 4l8 8M12 4l-8 8',
  search: 'M7.2 12a4.8 4.8 0 1 0 0-9.6 4.8 4.8 0 0 0 0 9.6ZM10.8 10.8 14 14',
  chevron: 'M5.5 6.5 8 9l2.5-2.5',
  check: 'M3.4 8.4 6.4 11.4 12.6 5',
  alert: 'M8 5.4v3.4M8 11.3h.01M8 2.2 14 12.6H2z',
  copy: 'M5.6 5.6V3.4h7v7h-2.2M3.4 5.6h7v7h-7z',
  qr: 'M3 3h4v4H3zM9 3h4v4H9zM3 9h4v4H3zM9.5 9.5h1M11.5 11.5h1.5M9.5 12.5h.01M12.5 9.5h.01',
  download: 'M8 3v7M5 7.5 8 10.5l3-3M3.4 12.6h9.2',
  scan: 'M3 6V3.6h2.6M13 6V3.6h-2.6M3 10v2.4h2.6M13 10v2.4h-2.6M3.6 8h8.8',
  play: 'M5.4 3.6 12 8l-6.6 4.4z',
  trash: 'M3.6 4.6h8.8M6.4 4.6V3.2h3.2v1.4M5 4.6l.6 8.2h4.8l.6-8.2',
  power: 'M8 2.6v5M4.6 4.4a4.8 4.8 0 1 0 6.8 0',
  ticket: 'M2.6 6.2V4.4h10.8v1.8a1.8 1.8 0 0 0 0 3.6v1.8H2.6v-1.8a1.8 1.8 0 0 0 0-3.6Z',
  checkin: 'M3 4.6h10v8.4H3zM3 7.2h10M5.6 3.2v2.6M10.4 3.2v2.6M6 9.8l1.4 1.4L10.4 8',
  wallet: 'M2.6 4.6h9.6a1.2 1.2 0 0 1 1.2 1.2v4.4a1.2 1.2 0 0 1-1.2 1.2H2.6zM2.6 4.6V3.4h8M10.4 8.5h.01',
  eye: 'M1.8 8S4.2 4.4 8 4.4 14.2 8 14.2 8 11.8 11.6 8 11.6 1.8 8 1.8 8Z M8 9.6a1.6 1.6 0 1 0 0-3.2 1.6 1.6 0 0 0 0 3.2Z',
  lock: 'M4.4 7.2V5.4a3.6 3.6 0 0 1 7.2 0v1.8M3.4 7.2h9.2v6H3.4z',
};

/** icon('plus') → SVG 节点 */
export function icon(name, size = 16) {
  const el = svgEl('svg', {
    viewBox: '0 0 16 16', width: size, height: size, fill: 'none',
    stroke: 'currentColor', 'stroke-width': '1.4',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true',
  });
  for (const d of String(PATHS[name] || PATHS.alert).split(' M').map((s, i) => (i ? 'M' + s : s))) {
    el.append(svgEl('path', { d }));
  }
  return el;
}

/* ── 格式化 ───────────────────────────────────────────────────── */
export function ago(iso) {
  if (!iso || String(iso).startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

export function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (h) return h + '时' + String(m).padStart(2, '0') + '分';
  if (m) return m + '分' + String(s).padStart(2, '0') + '秒';
  return s + '秒';
}

/** token 数：999999 → 1m（四舍五入后自动升级单位）*/
export function fmtToken(n) {
  if (n == null || n === '') return '—';
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return '—';
  if (v < 1000) return String(Math.round(v));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let u = units[0];
  for (const c of units) if (v >= c[1]) u = c;
  let val = v / u[1], rounded = Number(val.toFixed(1));
  const next = units[units.indexOf(u) + 1];
  if (next && rounded >= 1000) { u = next; val = v / u[1]; rounded = Number(val.toFixed(1)); }
  return rounded + u[0];
}

export function fmtMs(ms) {
  const n = Number(ms || 0);
  if (!n) return '—';
  return n >= 1000 ? (n / 1000).toFixed(2) + 's' : Math.round(n) + 'ms';
}

export function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

export function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }

export function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

export function uptime(sec) {
  const up = Math.floor(sec || 0);
  return (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
    Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
}
