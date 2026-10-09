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

/* ── 交互护栏：人正在看/操作时不要换 DOM ────────────────────────
   轮询会重建整棵视图子树，直接换 DOM 的代价是用户实测得到的四类故障：
   滚动位置跳、悬停态闪、选中的文字丢、下拉框被关。所以任一「正在用」
   信号成立时，把新产物挂起，等操作结束再一次性落地——数据照常更新，
   只是那一次不动界面。判据（宁多勿漏）：抽屉未关 / 焦点在输入控件里 /
   页面有文字选区 / 1.2 秒内滚动过。 */
const pendingSwap = new Set();   // 挂起中的 effect（后到的产物覆盖先到的）
let lastScrollAt = 0;

function isScroller(el) {
  if (!el || el.nodeType !== 1) return false;
  const cs = el.ownerDocument && el.ownerDocument.defaultView
    ? el.ownerDocument.defaultView.getComputedStyle(el) : null;
  const ov = (cs && cs.overflowY) || '';
  return (ov === 'auto' || ov === 'scroll' || el.classList.contains('content'))
    && el.scrollHeight > el.clientHeight + 1;
}

export function isEngaged() {
  if (drawerState !== 'closed') return true;
  if (menuEl) return true;          // 菜单开着 = 人正在做选择，别换掉它锚定的那一行
  const a = document.activeElement;
  if (a && a.nodeType === 1 && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName)) return true;
  const sel = document.getSelection && document.getSelection();
  if (sel && !sel.isCollapsed && String(sel).trim() !== '') return true;
  return (typeof performance !== 'undefined' ? performance.now() : 0) - lastScrollAt < 1200;
}

/* 滚动位置快照：跨替换留存的祖先容器按元素记，子树内的按文档序下标记。
   回填时新树同下标元素形状一致；不一致时 scrollTop 赋值会被浏览器静默
   夹到上限，不会抛错。 */
function captureScroll(oldNode) {
  const snap = { hosts: [], inner: [] };
  for (let el = oldNode.parentNode; el; el = el.parentNode) {
    if (isScroller(el)) snap.hosts.push([el, el.scrollTop]);
  }
  if (isScroller(oldNode)) snap.inner.push(oldNode.scrollTop);
  const kids = oldNode.querySelectorAll ? oldNode.querySelectorAll('*') : [];
  for (const el of kids) if (isScroller(el)) snap.inner.push(el.scrollTop);
  return snap;
}

function restoreScroll(snap, newNode) {
  for (const [el, top] of snap.hosts) {
    if (el) el.scrollTop = top;
  }
  let targets = [];
  if (isScroller(newNode)) targets.push(newNode);
  const kids = newNode.querySelectorAll ? newNode.querySelectorAll('*') : [];
  for (const el of kids) if (isScroller(el)) targets.push(el);
  for (let i = 0; i < targets.length && i < snap.inner.length; i++) {
    targets[i].scrollTop = snap.inner[i];
  }
}

/* 焦点 + 选区快照：只在焦点位于被替换的子树内时才需要恢复。 */
function captureFocus(oldNode) {
  const a = document.activeElement;
  if (!a || !oldNode.contains || !oldNode.contains(a)) return null;
  const f = { id: a.id || null, top: a.scrollTop, selStart: null, selEnd: null };
  try {
    if ('selectionStart' in a) { f.selStart = a.selectionStart; f.selEnd = a.selectionEnd; }
  } catch { /* 不支持选区的控件 */ }
  return f;
}

function restoreFocus(f, newNode) {
  if (!f) return;
  let el = f.id && document.getElementById ? document.getElementById(f.id) : null;
  if (!el || !newNode.contains(el)) return;   // id 不在新树里就别乱猜，交给浏览器
  try {
    el.focus({ preventScroll: true });
    if (f.selStart != null && 'selectionStart' in el) {
      el.setSelectionRange(f.selStart, f.selEnd);
    }
  } catch { /* 控件已不可交互 */ }
}

// 「操作结束就补落地」的唯一一条路：挂起队列非空时才开表，排空即停。
// 不常驻定时器——既省掉每秒空转，也让无头 Node 测试脚本能自然退出。
let flushTimer = 0;
function flushPending() {
  // 定时器句柄保持有效（不能被置零后再 clearInterval，那会把它变成常驻轮询）：
  // 队列空了就自己停手。
  if (pendingSwap.size === 0) { stopFlush(); return; }
  if (isEngaged()) return;          // 人还在操作，下一轮再看
  for (const e of [...pendingSwap]) {
    pendingSwap.delete(e);
    if (e.disposed || !e.node || !e.node.parentNode || !e.pending) continue;
    const next = e.pending;
    e.pending = null;
    const snap = captureScroll(e.node);
    const fsnap = captureFocus(e.node);
    e.node.replaceWith(next);
    e.node = next;
    restoreScroll(snap, next);
    restoreFocus(fsnap, next);
  }
  if (pendingSwap.size > 0) armFlush();
}
function armFlush() {
  if (!flushTimer) flushTimer = setInterval(flushPending, 400);
}
function stopFlush() {
  if (flushTimer) { clearInterval(flushTimer); flushTimer = 0; }
}

document.addEventListener('scroll', () => {
  lastScrollAt = typeof performance !== 'undefined' ? performance.now() : 0;
}, { capture: true, passive: true });

/* ── effect：自动追踪依赖；返回 DOM 时自动替换旧节点 ──────────── */
export function effect(fn) {
  const e = {
    deps: new Set(),
    node: null,
    pending: null,
    disposed: false,
    dispose() { e.disposed = true; pendingSwap.delete(e); for (const s of e.deps) s.delete(e); e.deps.clear(); },
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
      // 视图复用同一棵树（数据到了就地重画，见 views/accounts.js）：没什么要换的，
      // 更不能换——把已经上屏的节点搬进新包装会让页面当场空一截。
      if (out === e.node) return out;
      if (out instanceof Node) {
        if (e.node && e.node.parentNode) {
          if (isEngaged()) { e.pending = out; pendingSwap.add(e); armFlush(); return out; }
          const snap = captureScroll(e.node);
          const fsnap = captureFocus(e.node);
          e.node.replaceWith(out);
          restoreScroll(snap, out);
          restoreFocus(fsnap, out);
        }
        e.pending = null;
        e.node = out;
      }
      return out;
    },
  };
  e.run();
  return e;
}

/* ── 弹簧动画 ───────────────────────────────────────────────────
   用 Apple 的两个参数而不是物理三件套：
     damping  阻尼比：1.0 = 临界阻尼（不过冲），< 1 有过冲
     response 到达目标的快慢（秒）——不是"时长"，弹簧没有固定时长
   关键能力：可随时改目标并保留当前速度（可中断、可反转、无速度断点）。
   半隐式欧拉 + 固定子步长，保证不同帧率下行为一致。 */
export function springValue({ from = 0, velocity = 0, damping = 1, response = 0.4, onUpdate, onDone }) {
  let x = from, v = velocity, target = from, raf = 0, last = 0;
  const omega = (2 * Math.PI) / Math.max(0.05, response);
  const stiffness = omega * omega;
  const dampCoef = 2 * damping * omega;

  const step = now => {
    const elapsed = last ? (now - last) / 1000 : 1 / 60;
    last = now;
    // NaN 防御：一旦状态被污染（外部传入坏速度等），立即落到目标并停表——
    // 否则 NaN 让收敛判定永远为假，rAF 每帧空转烧 CPU。
    if (!Number.isFinite(x) || !Number.isFinite(v)) {
      x = target; v = 0;
      onUpdate?.(x, v);
      raf = 0;
      onDone?.();
      return;
    }
    let t = Math.min(0.064, Math.max(0, elapsed));   // 切后台回来时不做巨跳
    const h = 1 / 240;
    while (t > 0) {
      const s = Math.min(h, t);
      const a = -stiffness * (x - target) - dampCoef * v;
      v += a * s;
      x += v * s;
      t -= s;
    }
    const settled = Math.abs(v) < 0.6 && Math.abs(x - target) < 0.4;
    onUpdate?.(x, v);
    if (settled) {
      x = target; v = 0;
      onUpdate?.(x, v);
      raf = 0;
      onDone?.();
      return;
    }
    raf = requestAnimationFrame(step);
  };

  return {
    get value() { return x; },
    get velocity() { return v; },
    /** 改目标（可带初速度）：飞行中改目标即为中断——速度不丢 */
    to(next, nextVelocity) {
      target = next;
      if (nextVelocity != null) v = nextVelocity;
      if (!raf) { last = 0; raf = requestAnimationFrame(step); }
    },
    /** 直接落到某值（不播动画）*/
    jump(next) { target = next; x = next; v = 0; onUpdate?.(x, v); },
    stop() { if (raf) cancelAnimationFrame(raf); raf = 0; },
    get settled() { return !raf; },
  };
}

/* 动量投影：由释放速度推出"它会滑到哪里"（Apple 的指数衰减式，不是 v²/2a）。
   然后从投影落点里挑最近的目标，而不是从释放点挑——这样轻甩才有"抛出去"的感觉。 */
export function projectMomentum(velocity, deceleration = 0.998) {
  return (velocity / 1000) * deceleration / (1 - deceleration);
}

/* 边界阻尼：越界越拖不动（真东西是慢下来，不是撞墙停下）。 */
export function rubberband(overshoot, dimension, constant = 0.55) {
  return (overshoot * dimension * constant) / (dimension + constant * Math.abs(overshoot));
}

/** 把一组 {x, t} 采样点换算成速度（px/s）。取最近几帧，避免用整段拖动平均值。*/
export function sampleVelocity(samples) {
  if (!samples || samples.length < 2) return 0;
  const a = samples[0], b = samples[samples.length - 1];
  const dt = (b[1] - a[1]) / 1000;
  if (!Number.isFinite(dt) || dt <= 0) return 0;
  const v = (b[0] - a[0]) / dt;
  return Number.isFinite(v) ? v : 0;
}

/** 材质化：玻璃表面进入/离开时让 模糊+缩放+透明度 一起动，
    读起来像"一层真实材料到达"，而不是单纯的淡入。*/
export function materialize(el, { open, onDone } = {}) {
  if (el._mat) el._mat.stop();
  const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (reduce) {                       // 减动效：只做短交叉淡入，不做位移/缩放
    el.style.transform = 'none';
    el.style.filter = 'none';
    el.style.opacity = open ? '1' : '0';
    onDone?.();
    return;
  }
  const s = springValue({
    from: open ? 0 : 1,
    damping: 1,
    response: open ? 0.34 : 0.24,     // 退出更快（非对称时长：系统响应要利落）
    onUpdate: p => {
      el.style.opacity = String(1 - p);
      el.style.transform = `scale(${(1 - 0.04 * p).toFixed(4)})`;
      el.style.filter = p > 0.02 ? `blur(${(6 * p).toFixed(2)}px)` : 'none';
    },
    onDone,
  });
  el._mat = s;
  s.to(open ? 0 : 1);
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

/* ── Toast ──────────────────────────────────────────────────────
   用 transition 而非 keyframes：toast 会被连续快速触发，关键帧中断后从零重播，
   过渡会平滑改道（Sonner 的结论）。离开比进入快——系统响应要利落。
   标签页隐藏时暂停计时，避免切回来发现消息已经过期。 */
const MAX_TOASTS = 4;
const liveToasts = new Set();

document.addEventListener('visibilitychange', () => {
  for (const t of liveToasts) t.onVisibility(document.hidden);
});

export function toast(msg, kind, opts = {}) {
  const host = document.getElementById('toasts');
  if (!host) return;
  while (host.childElementCount >= MAX_TOASTS) host.firstElementChild?.remove();

  // opts.action = { label, onclick }：可选的跟随动作（如批量操作后「查看日志」），
  // 让 fire-and-forget 的操作有闭环入口，不必自己去翻日志页。
  const el = h('div', { class: 'toast' + (kind === 'fail' ? ' fail' : '') },
    h('span', { text: msg }),
    opts.action ? h('button', {
      class: 'toast-act', text: opts.action.label,
      onclick: ev => { ev.stopPropagation(); el.remove(); opts.action.onclick(); },
    }) : null,
  );
  host.append(el);
  requestAnimationFrame(() => el.classList.add('in'));

  let remaining = 3400, startedAt = performance.now(), timer = 0;
  const ctl = {
    onVisibility(hidden) {
      if (!el.isConnected) { liveToasts.delete(ctl); return; }
      if (hidden) { clearTimeout(timer); remaining -= performance.now() - startedAt; }
      else if (remaining > 0) run();
    },
  };
  function run() {
    startedAt = performance.now();
    timer = setTimeout(() => {
      liveToasts.delete(ctl);
      el.classList.remove('in');
      el.classList.add('out');
      setTimeout(() => el.remove(), 220);
    }, remaining);
  }
  liveToasts.add(ctl);
  run();
}

/* ── 抽屉（玻璃滑层，可拖拽关闭）──────────────────────────────
   实现 Apple 的流体交互四件事：
     · 直接操作：拖动 1:1 跟手（尊重抓取偏移），不是等手势结束才动
     · 可中断：飞行中抓住即从"当前屏幕值"继续，速度不丢
     · 速度交接：松手时把指针速度交给弹簧，拖动与动画之间没有缝
     · 动量投影：轻甩即可关闭——由速度投影落点决定去留，而非拖过半屏
   方向判定在横纵之间二选一：横向占优才接管，否则放行给内容滚动。 */
let drawerEl = null, scrimEl = null, drawerSpring = null;
let drawerState = 'closed';   // open | closing | closed
let dragState = null;

const DRAWER_MAX_W = 520;
const drawerWidth = () => Math.min(DRAWER_MAX_W, window.innerWidth || DRAWER_MAX_W);

function paintDrawer(x) {
  if (!drawerEl) return;
  drawerEl.style.transform = `translate3d(${x.toFixed(2)}px, 0, 0)`;
  const p = Math.max(0, Math.min(1, 1 - x / drawerWidth()));
  if (scrimEl) {
    scrimEl.style.opacity = String(p);
    scrimEl.style.pointerEvents = p > 0.02 ? 'auto' : 'none';
  }
}

export function ensureLayers() {
  if (scrimEl) return;
  scrimEl = h('div', { class: 'scrim', onclick: () => closeDrawer() });
  drawerEl = h('aside', { class: 'drawer', 'aria-hidden': 'true' });
  drawerEl.addEventListener('pointerdown', onDragStart);
  document.body.append(scrimEl, drawerEl);
  drawerSpring = springValue({
    from: drawerWidth(),
    damping: 0.82,          // Apple 的抽屉/面板档位：略有过冲，因为手势本身带速度
    response: 0.32,
    onUpdate: paintDrawer,
  });
  paintDrawer(drawerWidth());
}

/** openDrawer({ title, hint, body, footer }) —— body/footer 为 DOM 节点 */
export function openDrawer({ title, hint, body, footer }) {
  ensureLayers();
  // 没有 footer 时不能把 null 交给 replaceChildren：真实 DOM 会把它当字符串渲染成
  // 一个 "null" 文本节点（h() 会过滤，replaceChildren 不会）。2026-10-09 浏览器实测。
  drawerEl.replaceChildren(
    h('header', null,
      h('div', { class: 'grow' }, h('h2', { text: title }), hint ? h('div', { class: 'hint', text: hint }) : null),
      h('button', { class: 'btn icon ghost', title: '关闭', onclick: () => closeDrawer() }, icon('close')),
    ),
    h('div', { class: 'body' }, body),
    ...(footer ? [h('footer', null, footer)] : []),
  );
  drawerState = 'open';
  drawerEl.setAttribute('aria-hidden', 'false');
  scrimEl.style.display = 'block';
  drawerSpring.to(0);          // 从中断处继续（若正在关闭则自然反转）
}

export function closeDrawer(velocity = 0) {
  if (!drawerEl || !drawerSpring) return;
  if (drawerState === 'closed') return;
  drawerState = 'closing';
  drawerEl.setAttribute('aria-hidden', 'true');   // 对辅助技术而言它正在离开
  drawerSpring.to(drawerWidth(), velocity);
  // 落定且没有被重新打开/抓住时才清场（下次打开是全新的一棵）
  const wait = () => {
    if (!drawerSpring.settled) return setTimeout(wait, 90);
    if (drawerState === 'closing') {
      drawerState = 'closed';
      drawerEl.replaceChildren();
      if (scrimEl) scrimEl.style.display = 'none';
    }
  };
  setTimeout(wait, 90);
}

function onDragStart(e) {
  if (!drawerEl || drawerState === 'closed') return;   // closing 时抓住 = 反转，必须放行
  if (e.pointerType === 'mouse' && e.button !== 0) return;
  if (e.target.closest('input, textarea, select, button, a, label, .no-drag')) return;
  const wasClosing = drawerState === 'closing';
  if (wasClosing) {
    // 抓住即反转意图：内容重新可交互；弹簧立即冻结——抽屉停在手指下，
    // 而不是继续从指缝里飞走（响应必须连续，不止在松手后）。
    drawerState = 'open';
    drawerEl.setAttribute('aria-hidden', 'false');
    drawerSpring.stop();
    //  ↑ closeDrawer 的落定清理循环见到 open 就不会清场——否则内容被拆掉，
    //    正在派发的指针事件失去冒泡路径，抽屉会冻在半路。
  }
  dragState = {
    id: e.pointerId,
    startX: e.clientX, startY: e.clientY,
    startVal: drawerSpring.value,
    samples: [[e.clientX, performance.now()]],
    active: false,
    wasClosing,
  };
  drawerEl.addEventListener('pointermove', onDragMove);
  drawerEl.addEventListener('pointerup', onDragEnd);
  drawerEl.addEventListener('pointercancel', onDragEnd);
}

function onDragMove(e) {
  try {
    if (!dragState || e.pointerId !== dragState.id) return;
    const dx = e.clientX - dragState.startX;
    const dy = e.clientY - dragState.startY;
    if (!dragState.active) {
      if (Math.abs(dx) < 10) return;                        // 迟滞：先确认意图
      if (Math.abs(dx) < Math.abs(dy) * 1.2) { return stopDrag(); }  // 纵向占优 → 让给滚动
      dragState.active = true;
      drawerSpring.stop();
      try { drawerEl.setPointerCapture(dragState.id); }     // 合成事件/无效指针会抛，捕获只是优化
      catch { /* 忽略：事件仍会派发到元素上 */ }
    }
    let next = dragState.startVal + dx;
    if (next < 0) next = -rubberband(-next, drawerWidth());  // 左拉越界：越拉越沉
    drawerSpring.jump(next);                                 // 1:1 跟手（onUpdate 绘制）
    dragState.samples.push([e.clientX, performance.now()]);
    if (dragState.samples.length > 6) dragState.samples.shift();
  } catch (err) { if (typeof window !== 'undefined') window.__dragErr = String(err && err.message || err); }
}

function onDragEnd(e) {
  try {
    if (!dragState || e.pointerId !== dragState.id) return;
    const v = sampleVelocity(dragState.samples);             // px/s（向右为正）
    const wasActive = dragState.active;
    const wasClosing = dragState.wasClosing;
    stopDrag();
    if (!wasActive) {
      // 只是点了一下没拖：恢复被打断的运动（点住暂停，松手继续）
      if (wasClosing) closeDrawer();
      return;
    }
    const w = drawerWidth();
    const projected = drawerSpring.value + projectMomentum(v);
    // 速度符号优先：轻快一甩就关，不必拖过半屏
    if (v > 520 || projected > w * 0.45) closeDrawer(v);
    else drawerSpring.to(0, v);                              // 否则带着速度弹回
  } catch (err) { if (typeof window !== 'undefined') window.__dragErr = String(err && err.message || err); }
}

function stopDrag() {
  if (!dragState) return;
  drawerEl.removeEventListener('pointermove', onDragMove);
  drawerEl.removeEventListener('pointerup', onDragEnd);
  drawerEl.removeEventListener('pointercancel', onDragEnd);
  dragState = null;
}

/* ── 确认框（单色玻璃，替代原生 confirm）────────────────────────
   弹层保持 transform-origin: center（弹层不锚定触发源），
   进出用材质化：模糊+缩放+透明度一起动。 */
export function confirmDialog(message, { ok = '确认', cancel = '取消' } = {}) {
  return new Promise(resolve => {
    const wrap = h('div', { class: 'sheet-wrap' });
    const sheet = h('div', { class: 'sheet' },
      h('h2', { text: '请确认' }),
      h('div', { class: 'hint', text: message }),
      h('div', { class: 'row', style: { marginTop: '20px', justifyContent: 'flex-end' } },
        h('button', { class: 'btn', text: cancel, onclick: () => done(false) }),
        h('button', { class: 'btn primary', text: ok, onclick: () => done(true) }),
      ),
    );
    const onKey = ev => { if (ev.key === 'Escape') done(false); };
    const done = val => {
      document.removeEventListener('keydown', onKey);
      materialize(sheet, { open: false, onDone: () => wrap.remove() });
      resolve(val);
    };
    wrap.append(sheet);
    wrap.addEventListener('click', ev => { if (ev.target === wrap) done(false); });
    document.body.append(wrap);
    wrap.classList.add('on');
    document.addEventListener('keydown', onKey);
    materialize(sheet, { open: true });
    sheet.querySelector('.btn.primary').focus();
  });
}

/* ── 上下文菜单（⋯ 按钮的落点）────────────────────────────────
   一行账号上有五六个动作，全部平铺就成了一堵按钮墙（清单 II.7：一个主按钮
   + 一个 ⋯）。菜单锚定触发源、放不下就朝上翻，点外面 / Escape / 滚动 / 改窗口
   大小即关——菜单是临时物，任何把视线从它上面移开的动作都该结束它。 */
let menuEl = null, menuOff = null;

export function closeMenu() {
  if (!menuEl) return;
  const el = menuEl;
  menuEl = null;
  if (menuOff) { menuOff(); menuOff = null; }
  el.remove();
  armFlush();    // 菜单开着时挂起的重绘，此刻补落地
}

/** openMenu(anchor, items) —— items: [{label, onclick, danger?, tip?, disabled?}] */
export function openMenu(anchor, items) {
  closeMenu();
  const panel = h('div', { class: 'menu', role: 'menu' },
    ...items.map(it => h('button', {
      class: 'menu-item' + (it.danger ? ' danger' : ''),
      role: 'menuitem',
      title: it.tip || '',
      disabled: !!it.disabled,
      onclick: () => { const fn = it.onclick; closeMenu(); fn?.(); },
    }, h('span', { text: it.label }))),
  );
  const wrap = h('div', {
    class: 'menu-wrap',
    onpointerdown: ev => { if (ev.target === wrap) closeMenu(); },
  }, panel);
  document.body.append(wrap);
  menuEl = wrap;

  const r = anchor.getBoundingClientRect();
  const pw = panel.offsetWidth || 186, ph = panel.offsetHeight;
  panel.style.left = Math.max(8, Math.min(r.right - pw, window.innerWidth - pw - 8)) + 'px';
  panel.style.top = (r.bottom + ph + 14 > window.innerHeight
    ? Math.max(8, r.top - ph - 6) : r.bottom + 6) + 'px';

  const onKey = ev => { if (ev.key === 'Escape') { ev.stopPropagation(); closeMenu(); } };
  const onScroll = () => closeMenu();
  document.addEventListener('keydown', onKey, true);
  addEventListener('scroll', onScroll, true);
  addEventListener('resize', onScroll);
  menuOff = () => {
    document.removeEventListener('keydown', onKey, true);
    removeEventListener('scroll', onScroll, true);
    removeEventListener('resize', onScroll);
  };
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
  more: 'M3.4 8h.01M8 8h.01M12.6 8h.01',
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

// fmtTok 与 fmtToken 是同一件事的两个名字（历史遗留，用量视图用的是短名）。
// 保留别名避免全量替换，但实现只此一份。
export const fmtTok = fmtToken;

export function uptime(sec) {
  const up = Math.floor(sec || 0);
  return (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
    Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
}
