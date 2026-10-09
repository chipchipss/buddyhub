/* ══════════════════════════════════════════════════════════════════
   shell.js · 应用壳层
   五个一级页（首页 / 账号 / 自动化 / 网关 / 设置）→ 侧栏 / 顶栏 / 状态条
   → 路由挂载 → 命令面板（⌘K）。
   视图只需实现 render()，返回一个 DOM 节点；依赖的信号变化时框架自动重建。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, effect, api, toast, closeDrawer, openDrawer, ensureLayers } from './kernel.js';
import { overview, viewId, applyTheme, theme, toggleTheme, refreshOverview, lastSync } from './store.js';
import { mountActivityBar } from './jobs.js';
import { openAccountDetail } from './views/account-detail.js';

/* ── 导航模型：一级页 + 页内分段 ─────────────────────────────────
   面板原先 8 个导航并列，混了对象（账号/密钥/模型）、动作（任务/日志）、
   设置（配置）三类东西。现在收成五个一级页；同类对象在一个页里用「分段」
   （页顶 tab）承载，页内子状态（平台筛选等）另用 ?tab= 表示：
     #accounts?seg=credits&tab=zai
     └ 一级页 ┘ └─ 分段 ─┘ └ 页内子状态 ┘
   ─────────────────────────────────────────────────────────────── */
const views = new Map();      // 分段 id -> def
const pages = new Map();      // 一级页 id -> { ...meta, sections: [] }

export const PAGE_META = {
  home: { title: '首页', icon: 'overview', primary: 'add', keywords: '首页 待办 概览 状态 dashboard' },
  accounts: { title: '账号', icon: 'accounts', primary: 'add', keywords: '账号 平台 池 积分 用量 凭证 详情' },
  automation: { title: '自动化', icon: 'automation', keywords: '任务 签到 排程 台账 队列 执行记录 日志' },
  gateway: { title: '网关', icon: 'keys', keywords: '密钥 模型 档位 倍率 调用统计 token api' },
  settings: { title: '设置', icon: 'config', keywords: '配置 参数 主题 关于 版本' },
};

// 旧 hash → [一级页, 默认分段]。收藏夹、README、日志链接里的深链不能断。
const LEGACY_ROUTE = {
  overview: ['home', null],
  accounts: ['accounts', null],
  usage: ['gateway', 'stats'],
  tasks: ['automation', 'tasks'],
  logs: ['automation', 'logs'],
  keys: ['gateway', 'keys'],
  models: ['gateway', 'models'],
  config: ['settings', 'config'],
};

/**
 * defineView({ id, page, tab, title, icon, keywords, render, tick, sub })
 *   page    所属一级页（缺省 = 自成一级页）
 *   tab     页顶分段上显示的名字（缺省用 title）
 *   render()  返回 DOM 节点（必填）
 *   tick()    本分段可见时每 5s 调用（可选，用于轮询刷新）
 *   sub()     顶栏副标题（可选）
 * 同一页的分段顺序 = 注册顺序（boot.js 的 import 顺序）。
 */
export function defineView(def) {
  views.set(def.id, def);
  const legacy = LEGACY_ROUTE[def.id];
  const pageId = def.page || (legacy ? legacy[0] : def.id);
  let pg = pages.get(pageId);
  if (!pg) {
    pg = Object.assign({ id: pageId, sections: [] },
      PAGE_META[pageId] || { title: def.title, icon: def.icon, keywords: def.keywords || '' });
    pages.set(pageId, pg);
  }
  if (!pg.sections.includes(def)) pg.sections.push(def);
  return def;
}

export function getView(id) { return views.get(id); }

/** sectionId：当前页内分段。与 viewId（一级页）一起构成完整路由。 */
export const sectionId = signal('');

const firstPage = () => pages.keys().next().value;

function sectionFor(pg, want) {
  if (!pg || !pg.sections.length) return null;
  if (pg.sections.length === 1) return pg.sections[0];
  return pg.sections.find(d => d.id === want) || pg.sections[0];
}

export function activePage() { return pages.get(viewId.peek()); }
export function activeSection() {
  const pg = activePage();
  return pg ? sectionFor(pg, sectionId.peek()) : null;
}

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
  for (const pg of pages.values()) {
    const item = h('div', {
      class: 'navitem', dataset: { view: pg.id }, title: pg.title,
      onclick: () => navigate(pg.id),
    }, icon(pg.icon || 'overview'), h('span', { text: pg.title }));
    pg._nav = item;
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

/* 侧栏选中态跟随一级页 */
function syncRailActive() {
  for (const pg of pages.values()) {
    if (pg._nav) pg._nav.classList.toggle('on', pg.id === viewId.peek());
  }
}

/* ── 底部 tab 栏（手机，清单 45）────────────────────────────────
   桌面侧栏照旧；窄屏 CSS 把 .rail 整个藏掉、露出这条。挂在 .app 网格
   第二行，随 viewId 换选中态。 */
function buildTabbar() {
  const bar = h('nav', { class: 'tabbar', 'aria-label': '页面导航' });
  const sync = () => {
    const cur = viewId.peek();
    for (const b of bar.children) b.classList.toggle('on', b.dataset.view === cur);
  };
  for (const pg of pages.values()) {
    bar.append(h('button', {
      dataset: { view: pg.id },
      onclick: () => navigate(pg.id),
    }, icon(pg.icon || 'overview'), h('span', { text: pg.title })));
  }
  viewId.subscribe(sync);
  sync();
  return bar;
}

/* ── 顶栏 ─────────────────────────────────────────────────────── */
/* 顶栏只留搜索 + 当前页的主操作（清单 12）：
   「刷新余额」原先在顶栏、批量操作、命令面板出现三次，这里收掉；
   主题切换移进「设置」，腾出顶栏。 */
function buildTopbar() {
  const title = h('h1', { text: '' });
  const sub = h('div', { class: 'sub', text: '' });
  const actions = h('div', { class: 'topbar-actions' });

  const sync = () => {
    const pg = activePage();
    const sec = activeSection();
    title.textContent = pg ? pg.title : 'BuddyHub';
    sub.textContent = (sec && sec.sub ? sec.sub() : '') || '';
    const kids = [
      h('button', {
        class: 'btn icon ghost', title: '搜索页面或动作 (Ctrl/⌘ + K)',
        onclick: () => openPalette(),
      }, icon('search')),
    ];
    if (pg && pg.primary === 'add') {
      kids.push(h('button', { class: 'btn sm primary', onclick: () => openAddAccount() }, icon('plus'), '添加账号'));
    }
    actions.replaceChildren(...kids);
  };
  viewId.subscribe(sync);
  sectionId.subscribe(sync);
  overview.subscribe(sync);

  return h('header', { class: 'topbar' },
    h('div', { class: 'titles' }, title, sub),
    h('div', { class: 'grow' }),
    actions,
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
      h('span', { class: 'sync', id: 'sync-ago', title: '数据轮询：5 秒一轮，标签页在后台时暂停', text: syncText(lastSync.peek()) }),
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

/** syncText —— 「轮询要可见」（清单 17）：数据什么时候刷新的要说得出口。 */
function syncText(ts) {
  if (!ts) return '正在连接…';
  const s = Math.max(0, Math.round((Date.now() - ts) / 1000));
  if (s < 5) return '刚刚更新';
  if (s < 60) return `${s} 秒前更新`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前更新`;
  return `${Math.floor(s / 3600)} 小时前更新`;
}

let statusbarEl = null;

// 这一行每秒改一次文字——只改 textContent，不重建状态条（状态条整体跟着
// overview 重画，频率由轮询决定）。
function tickSyncLabel() {
  const el = statusbarEl && statusbarEl.querySelector('#sync-ago');
  if (el) el.textContent = syncText(lastSync.peek());
}

/* ── 路由 ─────────────────────────────────────────────────────── */
let activeEffect = null;
let lastRouteKey = '';

/** parseHash() —— 解析 #page?seg=xxx&tab=yyy&acct=zzz 形态的 hash。
 *  seg = 页内分段；tab = 页内子状态（平台筛选等）；acct = 要直接打开详情的账号。
 *  三者都能深链、刷新保持、后退可用。 */
export function parseHash() {
  const raw = (location.hash || '').slice(1);
  const q = raw.indexOf('?');
  const view = (q < 0 ? raw : raw.slice(0, q)) || 'home';
  const params = new URLSearchParams(q < 0 ? '' : raw.slice(q + 1));
  return { view, seg: params.get('seg'), tab: params.get('tab'), acct: params.get('acct') };
}

/** routeFor(id, seg) —— 把「视图 id」翻译成一页一分段：既认一级页 id，
 *  也认分段 id 与旧 hash（navigate('logs') 仍然可用）。 */
function routeFor(id, seg) {
  if (pages.has(id)) return { page: id, seg: seg || null };
  const legacy = LEGACY_ROUTE[id];
  if (legacy) return { page: legacy[0], seg: seg || legacy[1] || null };
  const def = views.get(id);
  if (def) {
    for (const pg of pages.values()) if (pg.sections.includes(def)) return { page: pg.id, seg: def.id };
  }
  return { page: firstPage(), seg: null };
}

function hashFor(page, seg, tab) {
  let s = '#' + page;
  const qs = [];
  if (seg) qs.push('seg=' + seg);
  if (tab) qs.push('tab=' + tab);
  return qs.length ? s + '?' + qs.join('&') : s;
}

// push=true 留一条历史（换页/换分段），false 只改写当前条目（页内子状态）。
function writeHash(target, push) {
  if (location.hash === target) return;
  if (push && history.pushState) history.pushState(null, '', target);
  else history.replaceState(null, '', target);
}

/** 写入当前页的子状态（?tab=），不新增历史记录。 */
export function setHashTab(page, tab) {
  const cur = parseHash();
  if (cur.view !== page) return;
  writeHash(hashFor(page, cur.seg, tab), false);
}

/** 写入当前页的子状态并保留其它参数（acct 用完即弃的场景）。 */
export function patchHash(patch) {
  const cur = parseHash();
  writeHash(hashFor(cur.view, 'seg' in patch ? patch.seg : cur.seg, 'tab' in patch ? patch.tab : cur.tab), false);
}

/** applyHash() —— 地址栏 → 路由信号（导航、前进/后退、手改 hash 都走这里）。 */
function applyHash() {
  const { view, seg } = parseHash();
  const { page, seg: want } = routeFor(view, seg);
  const pg = pages.get(page) || pages.get(firstPage());
  const sec = sectionFor(pg, want);
  sectionId.set(sec ? sec.id : '');
  viewId.set(pg ? pg.id : firstPage());
}

export function navigate(id, seg) {
  const { page, seg: want } = routeFor(id, seg);
  const pg = pages.get(page);
  const sec = sectionFor(pg, want);
  const cur = routeFor(parseHash().view, parseHash().seg);
  // 跨页或换分段都是真正的导航，要留历史（返回键不该直接退出面板）。
  const push = cur.page !== page || (cur.seg || '') !== (sec ? sec.id : '');
  writeHash(hashFor(page, pg && pg.sections.length > 1 ? (sec ? sec.id : null) : null, null), push);
  sectionId.set(sec ? sec.id : '');
  viewId.set(page);
}

function mountPage() {
  const key = viewId.peek() + '/' + sectionId.peek();
  if (key === lastRouteKey && activeEffect) return;   // 一次导航里两个信号各触发一次
  lastRouteKey = key;
  if (activeEffect) { activeEffect.dispose(); activeEffect = null; }
  const def = activeSection();
  const host = document.getElementById('content');
  if (!host) return;
  host.replaceChildren();
  host.scrollTop = 0;
  const placeholder = document.createComment('root');
  host.append(placeholder);
  const eff = effect(() => renderRoute());
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

/** renderRoute() —— 一级页骨架：多段页先画分段条，再画当前分段。
 *  路由信号用 peek 读：换页/换分段由 mountPage 负责重建，
 *  这里只响应分段内部的数据信号。
 *
 *  同一路由的重跑（轮询到了新数据）尽量原样复用树：视图若就地重画并返回同一个
 *  节点，包装层也必须返回同一个节点，否则 h() 会把那块已上屏的子树搬进还没挂上去
 *  的新包装里——页面当场空一截，而护栏又要等用户停手才落地（2026-10-09 实测：
 *  账号页在搜索框有焦点期间「点什么都没反应」）。 */
let routeCache = null, routeKey = '', routeBody = null;

function renderRoute() {
  const pg = pages.get(viewId.peek()) || pages.get(firstPage());
  const sec = sectionFor(pg, sectionId.peek()) || pg.sections[0];
  if (!sec) return h('div', { class: 'view empty' }, h('div', { class: 'd', text: '没有可用页面' }));
  const body = sec.render();
  const key = pg.id + '/' + sec.id;
  if (key === routeKey && routeCache && body === routeBody && routeCache.isConnected) return routeCache;
  routeKey = key; routeBody = body;
  if (!pg || pg.sections.length < 2) return (routeCache = body);
  const cur = sec.id;
  return (routeCache = h('div', { class: 'page stack' },
    h('div', { class: 'seg page-tabs' },
      ...pg.sections.map(d => h('button', {
        class: d.id === cur ? 'on' : '',
        text: d.tab || d.title,
        title: (pg.title + ' · ' + (d.tab || d.title)),
        onclick: () => navigate(d.id),
      }))),
    body,
  ));
}

/* ── 命令面板（⌘K）────────────────────────────────────────────── */
let paletteEl = null, paletteInput = null, paletteResults = null;
let paletteItems = [], paletteSel = 0, paletteSeq = 0;

async function paletteCommands() {
  const cmds = [];
  for (const pg of pages.values()) {
    if (pg.sections.length > 1) {
      cmds.push({
        label: pg.title, kind: '前往', icon: pg.icon,
        keywords: (pg.title + ' ' + pg.id + ' ' + (pg.keywords || '')).toLowerCase(),
        run: () => navigate(pg.id),
      });
      for (const d of pg.sections) {
        cmds.push({
          label: pg.title + ' · ' + (d.tab || d.title), kind: '前往', icon: pg.icon,
          keywords: ((d.tab || d.title) + ' ' + d.id + ' ' + pg.title + ' ' + (d.keywords || '') + ' ' + (pg.keywords || '')).toLowerCase(),
          run: () => navigate(d.id),
        });
      }
    } else {
      const d = pg.sections[0];
      cmds.push({
        label: pg.title, kind: '前往', icon: pg.icon,
        keywords: (pg.title + ' ' + pg.id + ' ' + (d && d.keywords ? d.keywords : pg.keywords || '')).toLowerCase(),
        run: () => navigate(pg.id),
      });
    }
  }
  const acts = [
    ['刷新全部余额', 'refresh', () => api('balance_all', { method: 'POST' }).then(() => { refreshOverview(); toast('余额已刷新'); }).catch(e => toast(e.message, 'fail'))],
    ['全部签到', 'checkin', () => api('checkin_all', { method: 'POST' }).then(() => toast('全部签到已开始', undefined, { action: { label: '查看执行记录', onclick: () => navigate('logs') } })).catch(e => toast(e.message, 'fail'))],
    ['全部保活', 'power', () => api('keepalive_all', { method: 'POST' }).then(() => toast('保活已开始', undefined, { action: { label: '查看执行记录', onclick: () => navigate('logs') } })).catch(e => toast(e.message, 'fail'))],
    ['旅行巡检', 'ticket', () => api('travel_all', { method: 'POST' }).then(() => toast('旅行巡检已开始', undefined, { action: { label: '查看执行记录', onclick: () => navigate('logs') } })).catch(e => toast(e.message, 'fail'))],
    ['活跃上报', 'scan', () => api('activity_all', { method: 'POST' }).then(() => toast('活跃上报已开始', undefined, { action: { label: '查看执行记录', onclick: () => navigate('logs') } })).catch(e => toast(e.message, 'fail'))],
    ['添加账号', 'plus', () => openAddAccount()],
    ['切换明暗主题', 'moon', () => toggleTheme()],
    ['查看执行记录', 'logs', () => navigate('logs')],
  ];
  for (const [label, ic, run] of acts) {
    cmds.push({ label, kind: '动作', icon: ic, keywords: label.toLowerCase(), run });
  }
  /* 清单 51：面板不止搜页面名——账号、模型、配置项也搜得到、跳得过去。
     账号来自三源行模型，模型来自已加载的清单（没加载过就不给假数据），
     配置项来自配置页的 FIELDS 表（懒加载：首次搜配置才 import）。 */
  try {
    const { buildRows } = await import('./rows.js');
    const { overview } = await import('./store.js');
    const { zaiData } = await import('./views/zai-segment.js');
    const { extData } = await import('./views/ext-segment.js');
    for (const r of buildRows(overview()?.accounts, zaiData()?.accounts, extData()?.accounts)) {
      cmds.push({
        label: r.name, kind: '账号', icon: 'accounts',
        keywords: (r.name + ' ' + r.id + ' ' + r.platformLabel).toLowerCase(),
        run: () => { navigate('accounts'); openAccountDetail(r); },
      });
    }
  } catch { /* rows 不可用时面板退回只搜页面与动作 */ }
  try {
    const mod = await import('./views/models.js');
    if (typeof mod.paletteItems === 'function') {
      for (const m of mod.paletteItems()) {
        cmds.push({
          label: m.label, kind: '模型', icon: 'models',
          keywords: m.keywords,
          run: () => { navigate('models'); navigator.clipboard?.writeText(m.id).catch(() => {}); toast('已复制 ' + m.id); },
        });
      }
    }
  } catch { /* 同上 */ }
  try {
    const cfg = await import('./views/config.js');
    if (typeof cfg.paletteItems === 'function') {
      for (const f of cfg.paletteItems()) {
        cmds.push({ label: f.label, kind: '配置', icon: 'config', keywords: f.keywords, run: () => navigate('config', f.section) });
      }
    }
  } catch { /* 同上 */ }
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
  const seq = ++paletteSeq;
  paletteCommands().then(all => {
    if (seq !== paletteSeq) return; // 输入还在变：旧结果作废，避免慢 import 的结果覆盖新词
    paletteItems = query ? all.filter(c => c.keywords.includes(query) || c.label.toLowerCase().includes(query)) : all;
    paletteSel = 0;
    paintPalette();
  });
}

function paintPalette() {
  if (!paletteItems.length) {
    paletteResults.replaceChildren(h('div', { class: 'empty', text: '没有匹配项' }));
    return;
  }
  const parts = [];
  let lastKind = '';
  paletteItems.forEach((c, i) => {
    if (c.kind !== lastKind) { // 清单 51：按类别给标题行，账号/模型/配置不再混成一锅
      lastKind = c.kind;
      parts.push(h('div', { class: 'group', text: c.kind }));
    }
    parts.push(h('div', {
      class: 'item' + (i === paletteSel ? ' sel' : ''),
      onmouseenter: () => { paletteSel = i; paintPalette(); },
      onclick: () => { closePalette(); c.run(); },
    }, icon(c.icon), h('span', { text: c.label }), h('span', { class: 'kind', text: c.kind })));
  });
  paletteResults.replaceChildren(...parts);
}

/* ── 启动壳层 ─────────────────────────────────────────────────── */
export function startShell() {
  ensureLayers();
  applyTheme();
  theme.subscribe(applyTheme);

  // 先把地址栏翻译成路由，再建界面（首帧就得是用户要的那一页）
  applyHash();

  statusbarEl = buildStatusbar();
  const app = h('div', { class: 'app' + (railMiniState ? ' rail-mini' : '') },
    buildRail(),
    h('div', { class: 'stage' },
      buildTopbar(),
      h('main', { class: 'content', id: 'content' }),
    ),
    statusbarEl,
    buildTabbar(),   // 手机端底部导航；桌面由 CSS 隐藏
  );
  document.body.append(app);
  // 活动栏挂在 shell 上：换页、换分段都不能把正在跑的作业弄丢（清单 14）
  mountActivityBar();

  // 路由：一级页/分段变化 → 换视图 + 同步侧栏选中
  viewId.subscribe(() => { syncRailActive(); mountPage(); });
  sectionId.subscribe(mountPage);
  syncRailActive();
  mountPage();

  addEventListener('hashchange', applyHash);

  // 返回/前进：pushState 不触发 hashchange，得单独接 popstate。
  addEventListener('popstate', applyHash);

  /* 清单 52：键盘直达。g 是前缀键（g a→账号、g g→总览…），j/k 在列表页滚动，
     / 开面板，? 弹快捷键表。输入框聚焦时全部让路。 */
  let gPending = false;
  addEventListener('keydown', ev => {
    const meta = ev.metaKey || ev.ctrlKey;
    if (meta && ev.key.toLowerCase() === 'k') { ev.preventDefault(); openPalette(); return; }
    if (ev.key === 'Escape') { closePalette(); closeDrawer(); gPending = false; return; }
    if (ev.altKey || meta || ev.ctrlKey) return;
    const t = ev.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable || t.tagName === 'SELECT')) { gPending = false; return; }
    if (gPending) {
      gPending = false;
      const dest = { a: 'accounts', g: 'overview', t: 'tasks', k: 'keys', l: 'logs', m: 'gateway', c: 'settings' }[ev.key.toLowerCase()];
      if (dest) { ev.preventDefault(); navigate(dest); return; }
      return;
    }
    const k = ev.key.toLowerCase();
    if (k === 'g') { gPending = true; return; }
    if (k === '/') { ev.preventDefault(); openPalette(); return; }
    if (k === '?') { ev.preventDefault(); openShortcutSheet(); return; }
    if (k === 'j' || k === 'k') {
      // 真正的滚动容器是 .content（.view 可能 overflow:hidden），回退到整页
      const scroller = document.querySelector('.content') || document.querySelector('.view') || document.scrollingElement;
      const step = (scroller.clientHeight || 600) * 0.75 * (k === 'j' ? 1 : -1);
      scroller.scrollBy({ top: step, behavior: 'smooth' });
    }
  });

  // 清单 52：? 的快捷键表（轻量 sheet，复用 drawer 容器样式）
  function openShortcutSheet() {
    const rows = [
      ['g 然后 a / g / t / k / l / m / c', '跳到 账号 / 总览 / 任务 / 密钥 / 日志 / 网关 / 设置'],
      ['j / k', '当前页向下 / 向上滚动一大段'],
      ['/ 或 Ctrl+K', '打开搜索与命令面板'],
      ['?', '这张快捷键表'],
      ['Esc', '关闭面板、抽屉或弹出层'],
    ];
    openDrawer({ title: '键盘快捷键', body: h('div', { class: 'stack' },
      ...rows.map(([k, d]) => h('div', { class: 'row', style: { gap: '14px', alignItems: 'baseline' } },
        h('code', { class: 'url-box', style: { padding: '2px 8px', fontSize: '12px', whiteSpace: 'nowrap' }, text: k }),
        h('span', { style: { fontSize: '13px' }, text: d }))),
      h('div', { class: 'muted', style: { fontSize: '12px' }, text: '在输入框打字时快捷键全部失效，放心搜。' })),
    });
  }

  // 轮询：只驱动当前分段自己的 tick；标签页在后台时整轮跳过——
  // 没人看的页面不该每秒打后端、更不该攒一堆挂起重绘。
  setInterval(() => {
    if (document.hidden) return;
    const def = activeSection();
    if (def && def.tick) { try { def.tick(); } catch (e) { console.error('[tick]', e); } }
  }, 5000);

  // 「N 秒前更新」按秒走；后台标签页不轮询，文字就停在那儿如实反映最后一次成功
  setInterval(tickSyncLabel, 1000);

  refreshOverview();
}

/* 供视图/抽屉调用（避免循环依赖，延迟到运行时解析）。
   变参转发：账号页/详情抽屉的「重新登录」要带平台 id 直接落在那一个平台上。 */
export function openAddAccount(...args) { window.__openAddAccount?.(...args); }

/* 顶栏批量刷新的备用入口（账号页工具条复用同一实现）*/
export { doRefreshAll };
