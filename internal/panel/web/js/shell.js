/* ══════════════════════════════════════════════════════════════════
   shell.js · 应用壳层
   注册视图 → 生成侧栏/顶栏/状态条 → 路由挂载 → 命令面板（⌘K）
   视图只需实现 render()，返回一个 DOM 节点；依赖的信号变化时框架自动重建。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, effect, api, toast, closeDrawer, ensureLayers } from './kernel.js';
import { overview, theme, toggleTheme, viewId, applyTheme, refreshOverview } from './store.js';

/* ── 视图注册表 ───────────────────────────────────────────────── */
const views = new Map();

/**
 * defineView({ id, title, sub, icon, group, keywords, render, tick, actions })
 *   render()  返回 DOM 节点（必填）
 *   tick()    视图可见时每 5s 调用（可选，用于轮询刷新）
 *   actions() 返回视图工具条按钮数组（可选）
 */
export function defineView(def) {
  views.set(def.id, def);
  return def;
}

export function getView(id) { return views.get(id); }

/* ── 侧栏 ─────────────────────────────────────────────────────── */
const LS_RAIL = 'buddyhub.rail';
const railMini = signalFromStorage();

function signalFromStorage() {
  let mini = false;
  try { mini = localStorage.getItem(LS_RAIL) === 'mini'; } catch { /* 私密模式 */ }
  return mini;
}

let railMiniState = railMini;

function setRailMini(mini) {
  railMiniState = mini;
  try { localStorage.setItem(LS_RAIL, mini ? 'mini' : 'full'); } catch { /* 私密模式 */ }
  const app = document.querySelector('.app');
  if (app) app.classList.toggle('rail-mini', mini);
}

function buildRail() {
  const list = h('div', { class: 'navlist' });
  let lastGroup = null;
  for (const def of views.values()) {
    if (def.group && def.group !== lastGroup) {
      list.append(h('div', { class: 'navgroup', text: def.group }));
      lastGroup = def.group;
    }
    const item = h('div', {
      class: 'navitem', dataset: { view: def.id }, title: def.title,
      onclick: () => navigate(def.id),
    }, icon(def.icon || 'overview'), h('span', { text: def.title }));
    def._nav = item;
    list.append(item);
  }
  const toggle = h('button', {
    class: 'rail-toggle', title: '折叠侧栏',
    onclick: () => {
      const mini = !railMiniState;
      setRailMini(mini);
      toggle.replaceChildren(icon('chevron'), h('span', { text: '折叠侧栏' }));
      toggle.querySelector('svg').style.transform = mini ? 'rotate(-90deg)' : '';
    },
  }, icon('chevron'), h('span', { text: '折叠侧栏' }));

  const inner = h('div', { class: 'rail-inner' },
    h('div', { class: 'brand' },
      h('div', { class: 'brand-mark' }),
      h('div', { class: 'brand-text' },
        h('div', { class: 'n', text: 'BuddyHub' }),
        h('div', { class: 's', text: 'CONSOLE' }),
      ),
    ),
    list,
    h('div', { class: 'rail-foot' }, toggle),
  );
  return h('aside', { class: 'rail' }, inner);
}

/* 侧栏选中态跟随 viewId */
function syncRailActive() {
  for (const def of views.values()) {
    if (def._nav) def._nav.classList.toggle('on', def.id === viewId.peek());
  }
}

/* ── 顶栏 ─────────────────────────────────────────────────────── */
function buildTopbar() {
  const title = h('h1', { text: '' });
  const sub = h('div', { class: 'sub', text: '' });

  const sync = () => {
    const def = views.get(viewId.peek());
    title.textContent = def ? def.title : 'BuddyHub';
    sub.textContent = def && def.sub ? def.sub() : '';
  };
  viewId.subscribe(sync);
  overview.subscribe(sync);

  const themeBtn = h('button', {
    class: 'btn icon ghost', title: '切换明暗',
    onclick: () => toggleTheme(),
  });
  const paintTheme = () => themeBtn.replaceChildren(theme.peek() === 'light' ? icon('moon') : icon('sun'));
  theme.subscribe(paintTheme);
  paintTheme();

  return h('header', { class: 'topbar' },
    h('div', { class: 'titles' }, title, sub),
    h('div', { class: 'grow' }),
    h('div', { class: 'topbar-actions' },
      h('button', {
        class: 'btn icon ghost', title: '命令面板 (Ctrl/⌘ + K)',
        onclick: () => openPalette(),
      }, icon('search')),
      themeBtn,
      h('button', { class: 'btn sm', onclick: doRefreshAll }, icon('refresh'), '刷新'),
      h('button', { class: 'btn sm primary', onclick: () => openAddAccount() }, icon('plus'), '添加账号'),
    ),
  );
}

async function doRefreshAll(ev) {
  const btn = ev.currentTarget;
  btn.disabled = true;
  try {
    await api('balance_all', { method: 'POST' });
    await refreshOverview();
    toast('余额已从上游刷新');
  } catch (e) {
    toast('刷新失败：' + e.message, 'fail');
  } finally {
    btn.disabled = false;
  }
}

/* ── 状态条 ───────────────────────────────────────────────────── */
function buildStatusbar() {
  const host = h('footer', { class: 'statusbar' });
  const placeholder = document.createComment('sb');
  host.append(placeholder);
  const eff = effect(() => {
    const d = overview();
    const dotCls = !d ? 'dot ring' : d.healthy > 0 ? 'dot pulse' : d.total ? 'dot ring' : 'dot off';
    const state = !d ? '连接中' : d.healthy > 0 ? '服务正常' : d.total ? '无可用账号' : '待添加账号';
    const credits = d ? (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0) : 0;
    const totals = d ? (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0) : 0;
    const inflight = d ? (d.accounts || []).reduce((a, s) => a + (s.in_flight || 0), 0) : 0;
    return h('div', { class: 'statusbar-in' },
      h('span', { class: 'stat-item' }, h('i', { class: dotCls }), state),
      h('span', { class: 'stat-item' }, '账号 ', h('b', { text: d ? `${d.healthy}/${d.total}` : '—' })),
      h('span', { class: 'stat-item' }, '积分 ', h('b', { text: totals > 0 ? `${credits}/${totals}` : String(credits) })),
      h('span', { class: 'stat-item' }, '在途 ', h('b', { text: String(inflight) })),
      h('span', { class: 'grow' }),
      h('span', { class: 'mono', text: d ? `v${d.version} · ${d.redis_mode === 'upstash' ? 'redis 镜像' : '本地内存'}` : '' }),
      h('span', { class: 'mono', text: d ? `运行 ${fmtUptime(d.uptime_sec)}` : '' }),
    );
  });
  // effect 首跑产物挂在 placeholder 位置；后续重渲染由 effect 自行原地替换
  if (eff.node && eff.node.parentNode == null) placeholder.replaceWith(eff.node);
  return host;
}

function fmtUptime(sec) {
  const up = Math.floor(sec || 0);
  return (up >= 86400 ? Math.floor(up / 86400) + '天' : '') +
    Math.floor(up % 86400 / 3600) + '时' + Math.floor(up % 3600 / 60) + '分';
}

/* ── 路由 ─────────────────────────────────────────────────────── */
let activeEffect = null;

export function navigate(id) {
  if (!views.has(id)) id = views.keys().next().value;
  if (location.hash !== '#' + id) history.replaceState(null, '', '#' + id);
  viewId.set(id);
}

function mountView(id) {
  if (activeEffect) { activeEffect.dispose(); activeEffect = null; }
  const def = views.get(id) || views.get([...views.keys()][0]);
  const host = document.getElementById('content');
  host.replaceChildren();
  host.scrollTop = 0;
  const placeholder = document.createComment('root');
  host.append(placeholder);
  const eff = effect(() => def.render());
  if (eff.node && eff.node.parentNode == null) host.replaceChild(eff.node, placeholder);
  else placeholder.remove();
  activeEffect = eff;
  // 视图切换只做一次极短的透明度过渡（140ms）。
  // 这里是导航——每天几十次的动作，动效要"大幅削减"而非放大；且位移类
  // 动画在长列表页面上代价高。Apple 的空间一致性由侧栏选中态承担即可。
  if (host.animate && !matchMedia('(prefers-reduced-motion: reduce)').matches) {
    host.animate([{ opacity: 0.55 }, { opacity: 1 }], { duration: 140, easing: 'ease-out' });
  }
}

/* ── 命令面板（⌘K）────────────────────────────────────────────── */
let paletteEl = null, paletteInput = null, paletteResults = null;
let paletteItems = [], paletteSel = 0;

function paletteCommands() {
  const cmds = [];
  for (const def of views.values()) {
    cmds.push({
      label: def.title, kind: '前往', icon: def.icon || 'overview',
      keywords: (def.title + ' ' + def.id + ' ' + (def.keywords || '')).toLowerCase(),
      run: () => navigate(def.id),
    });
  }
  const acts = [
    ['刷新全部余额', 'refresh', () => api('balance_all', { method: 'POST' }).then(() => { refreshOverview(); toast('余额已刷新'); }).catch(e => toast(e.message, 'fail'))],
    ['全部签到', 'checkin', () => api('checkin_all', { method: 'POST' }).then(() => toast('全部签到已开始，结果见日志')).catch(e => toast(e.message, 'fail'))],
    ['全部保活', 'power', () => api('keepalive_all', { method: 'POST' }).then(() => toast('保活已开始')).catch(e => toast(e.message, 'fail'))],
    ['旅行巡检', 'ticket', () => api('travel_all', { method: 'POST' }).then(() => toast('旅行巡检已开始')).catch(e => toast(e.message, 'fail'))],
    ['活跃上报', 'scan', () => api('activity_all', { method: 'POST' }).then(() => toast('活跃上报已开始')).catch(e => toast(e.message, 'fail'))],
    ['添加账号', 'plus', () => openAddAccount()],
    ['切换明暗主题', 'moon', () => toggleTheme()],
    ['查看运行日志', 'logs', () => navigate('logs')],
  ];
  for (const [label, ic, run] of acts) {
    cmds.push({ label, kind: '动作', icon: ic, keywords: label.toLowerCase(), run });
  }
  return cmds;
}

/* ── 命令面板（⌘K）──────────────────────────────────────────────
   刻意不做进出动画：这是键盘触发的高频动作（每天上百次），
   任何动效都会让它显得慢半拍。Raycast 的零动画正是最优解。 */
export function openPalette() {
  if (!paletteEl) buildPalette();
  paletteEl.classList.add('on');
  paletteInput.value = '';
  renderPalette('');
  setTimeout(() => paletteInput.focus(), 0);
}

export function closePalette() {
  if (paletteEl) paletteEl.classList.remove('on');
}

function buildPalette() {
  paletteInput = h('input', {
    type: 'text', placeholder: '搜索页面或动作…', spellcheck: 'false',
    oninput: () => renderPalette(paletteInput.value),
    onkeydown: ev => {
      if (ev.key === 'ArrowDown') { ev.preventDefault(); paletteSel = Math.min(paletteSel + 1, paletteItems.length - 1); paintPalette(); }
      else if (ev.key === 'ArrowUp') { ev.preventDefault(); paletteSel = Math.max(paletteSel - 1, 0); paintPalette(); }
      else if (ev.key === 'Enter') { ev.preventDefault(); const it = paletteItems[paletteSel]; if (it) { closePalette(); it.run(); } }
      else if (ev.key === 'Escape') { closePalette(); }
    },
  });
  paletteResults = h('div', { class: 'results' });
  const box = h('div', { class: 'palette' }, paletteInput, paletteResults);
  paletteEl = h('div', {
    class: 'palette-wrap',
    onclick: ev => { if (ev.target === paletteEl) closePalette(); },
  }, box);
  document.body.append(paletteEl);
}

function renderPalette(q) {
  const query = q.trim().toLowerCase();
  const all = paletteCommands();
  paletteItems = query ? all.filter(c => c.keywords.includes(query) || c.label.toLowerCase().includes(query)) : all;
  paletteSel = 0;
  paintPalette();
}

function paintPalette() {
  if (!paletteItems.length) {
    paletteResults.replaceChildren(h('div', { class: 'empty', text: '没有匹配项' }));
    return;
  }
  paletteResults.replaceChildren(...paletteItems.map((c, i) =>
    h('div', {
      class: 'item' + (i === paletteSel ? ' sel' : ''),
      onmouseenter: () => { paletteSel = i; paintPalette(); },
      onclick: () => { closePalette(); c.run(); },
    }, icon(c.icon), h('span', { text: c.label }), h('span', { class: 'kind', text: c.kind })),
  ));
}

/* ── 启动壳层 ─────────────────────────────────────────────────── */
export function startShell() {
  ensureLayers();
  applyTheme();
  theme.subscribe(applyTheme);

  const app = h('div', { class: 'app' + (railMiniState ? ' rail-mini' : '') },
    buildRail(),
    h('div', { class: 'stage' },
      buildTopbar(),
      h('main', { class: 'content', id: 'content' }),
    ),
    buildStatusbar(),
  );
  document.body.append(app);

  // 路由：viewId 变化 → 换视图 + 同步侧栏选中
  viewId.subscribe(() => { syncRailActive(); mountView(viewId.peek()); });
  syncRailActive();
  mountView(viewId.peek());

  addEventListener('hashchange', () => {
    const id = (location.hash || '#overview').slice(1);
    if (views.has(id)) viewId.set(id);
  });

  addEventListener('keydown', ev => {
    const meta = ev.metaKey || ev.ctrlKey;
    if (meta && ev.key.toLowerCase() === 'k') { ev.preventDefault(); openPalette(); return; }
    if (ev.key === 'Escape') { closePalette(); closeDrawer(); }
  });

  // 轮询：只驱动当前视图的 tick
  setInterval(() => {
    const def = views.get(viewId.peek());
    if (def && def.tick) { try { def.tick(); } catch (e) { console.error('[tick]', e); } }
  }, 5000);

  refreshOverview();
}

/* 供视图/抽屉调用（避免循环依赖，延迟到运行时解析）*/
export function openAddAccount() { window.__openAddAccount?.(); }
