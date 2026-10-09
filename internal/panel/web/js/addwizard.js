/* ══════════════════════════════════════════════════════════════════
   addwizard.js · 添加账号向导（清单 20–24）

   四步：**选平台 → 选接入方式 → 完成这一步 → 加入池中**。
   只有一个入口（顶栏「添加账号」、账号页空状态、账号详情里的「重新登录」都走
   这里）；传平台 id 就自动落在那一个平台上，不再有两份表单各画一遍。

   为什么第一步是搜索 + 厂商分组：平台已经上到二十个，一屏 20 行的清单
   等于没有清单。厂商（注册表的 vendor 字段）是天然的分组，前端不自带归类表。

   为什么第二步才问「怎么接入」：同一个平台往往有好几种入池方式（本机客户端
   检测 / 手机号 / 粘 token），把它们平铺在第一步就是四五行 Loomy。
   **列表里排在第一个的就是推荐方式**——顺序由后端注册表的 login 字段决定。

   等待态为什么不会再被冲掉：抽屉只挂一次，切步骤只换 stage 的内容，
   而每个 (平台, 方式) 的表单节点建好后缓存在 nodes 里——切走再切回来还是
   **同一棵 DOM**，输入框里的值和进行中的授权轮询都还在。
   之前靠 localStorage / 模块级变量四处打补丁（那些仍然留着，但它们管的是
   「整页重新加载」这种更狠的情况，不再负责抽屉内部的每一次重绘）。
   ══════════════════════════════════════════════════════════════════ */

import {
  h, svgEl, icon, signal, api, apiUpload, toast, openDrawer, closeDrawer, copyText,
} from './kernel.js';
import { refreshOverview, overview } from './store.js';
import { buildRows, bulkKind } from './rows.js';
import { run } from './acts.js';
import { begin, report, finish } from './jobs.js';
import { platforms, loadPlatforms, plat, platName } from './platforms.js';
import { flowStore, pollLoop } from './loginflow.js';
import { navigate } from './shell.js';
import { extAddPanel, hasCredFields, credHelp } from './views/ext-add.js';
import { zaiAddForm, loadZai, zaiData } from './views/zai-segment.js';
import { extJsonImport, loadExt, extData } from './views/ext-segment.js';
import { openAccountDetail } from './views/account-detail.js';

/* ── 状态与稳定宿主 ───────────────────────────────────────────── */
const step = signal('pick');    // pick | method | form | done
const picked = signal('');      // 平台 id（'' = 还没选）
const way = signal('');         // 接入方式 id
const receipt = signal(null);   // 完成回执（没有独立账号行时才画出来）

let mounted = null;             // { stepsEl, hintEl, stageEl, footEl }
let searchInput = null;         // 第一步的搜索框（非受控，只建一次）
let searchQ = '';
const nodes = new Map();        // 'pick' / 'method:p' / 'form:p:w' → 缓存的节点

// 批量 JSON 导入不是平台，但走同一套步骤壳，所以给它一个伪平台 id。
const IMPORT_ID = 'import';
const IMPORT_PSEUDO = { id: IMPORT_ID, name: '批量 JSON 导入', vendor: '工具', login: 'manual', note: '整包入池' };

/* ── 接入方式（第二步的候选；第一个 = 推荐） ─────────────────── */
const LOGIN_WAY = {
  oauth:    { name: '浏览器授权登录', hint: '点开链接在网页里登录，面板自己把结果接回来' },
  device:   { name: '浏览器授权登录', hint: '在浏览器打开链接并完成授权，回来就已在池中' },
  callback: { name: '浏览器授权登录', hint: '授权完成后回调由网关在本机自己接住，不用粘贴任何东西' },
  qr:       { name: '扫码登录', hint: '用手机扫二维码，在手机上确认即可' },
  code:     { name: '设备码授权', hint: '面板给一串码，在浏览器里输入并确认' },
  sms:      { name: '手机号验证码登录', hint: '填手机号 → 收验证码 → 填回来，两步就进池' },
  detect:   { name: '自动检测本机客户端', hint: '读本机客户端已登录的状态，不用抄任何凭据' },
  paste:    { name: '扫码后粘贴回调链接', hint: '扫码确认后，把浏览器跳转到的那条地址贴回来' },
};
const CREDS_WAY = { id: 'creds', name: '粘贴客户端凭据', hint: '从客户端本地凭据里逐字段复制过来' };
const JSON_WAY = { id: 'json', name: '粘贴完整凭据 JSON', hint: '客户端能整包导出凭据时用这个更快' };

// 三个有专属交互的平台：方式清单写在它们自己的面板能力上，
// 但每一项都是「这个平台真有的路径」，不是通用 kind 的派生。
const BESPOKE_WAYS = {
  workbuddy: [
    { id: 'login', name: '浏览器授权登录', hint: '登录后自动完成签到与积分初始化；国际版还会自动领试用额度' },
  ],
  loomy: [
    { id: 'detect', name: '自动检测本机客户端', hint: '本机装着 Loomy 客户端并已登录时，直接用它' },
    { id: 'pwd', name: '手机号 + 密码', hint: '密码会加密存盘，凭据到期能无人值守续上' },
    { id: 'sms', name: '手机号 + 短信验证码', hint: '收一次码即入池；不存密码，到期要重新登录' },
    { id: 'token', name: '手动粘贴 Session Token', hint: '从客户端 auth-session.json 复制 session 字段' },
  ],
  zai: [
    { id: 'oauth', name: 'OAuth 免密登录', hint: '在智谱页面登录，面板自动入池并顺手兑换回退 Key' },
    { id: 'secret', name: '粘贴 JWT 或 API Key', hint: 'JWT 走 Plan 通道（消耗订阅额度）；API Key 走回退通道（免验证码）' },
  ],
};

/** waysFor(p) —— 一个平台有哪些入池方式，推荐排第一。
 *  注册表的 login 字段说的就是「推荐那一种」；另一种（逐字段粘贴）由该平台
 *  有没有凭据字段决定（字段表在 views/ext-add.js，那是纯 UI 细节）。 */
function waysFor(p) {
  if (p.id === IMPORT_ID) {
    return [{ id: 'file', name: '导入 cockpit tools 导出的 JSON', hint: '账号数组整包入池' }];
  }
  if (BESPOKE_WAYS[p.id]) return BESPOKE_WAYS[p.id];
  const out = [];
  const w = LOGIN_WAY[p.login];
  if (w) out.push({ id: 'login', ...w });
  out.push(hasCredFields(p.id) ? { ...CREDS_WAY } : { ...JSON_WAY });
  return out;
}

/** addable() —— 能从这个向导入池的平台。
 *  注册表的 login 字段就是答案：'' / none（本机凭据）/ config（配置页填写）都不是
 *  「添加账号」这条路径；alias_of 指向别的平台的不单列（loomy-cli 就是 loomy
 *  密码/短信登录的落库形态，列出来人会看到两行 Loomy）。 */
function addable() {
  return platforms.peek().filter(p =>
    p.login && p.login !== 'none' && p.login !== 'config' && !p.alias_of);
}

/* ── 抽屉骨架（只挂一次）─────────────────────────────────────── */
function mount() {
  const stepsEl = h('div', { class: 'wiz-steps' });
  const hintEl = h('div', { class: 'wiz-hint' });
  const stageEl = h('div', { class: 'wiz-stage' });
  const footEl = h('div', { class: 'row', style: { width: '100%' } });
  openDrawer({
    title: '添加账号',
    // 副标题说人话（清单 24）：讲用户关心的结果，不讲 extstore / provider 这些实现词
    hint: '凭据只写进本机的数据目录，落盘就生效，不用重启网关',
    body: h('div', { class: 'stack' }, stepsEl, hintEl, stageEl),
    footer: footEl,
  });
  mounted = { stepsEl, hintEl, stageEl, footEl };
}

/** openAddAccount(provider) —— 打开向导；传平台 id 就跳过第一步直接落在那平台上。
 *  异步只为注册表：平台身份（厂商分组、有几种接入方式）全在后端那张表里，
 *  没拿到就选不出平台——先画一遍空清单再跳两步，用户看到的是闪一下。 */
export async function openAddAccount(provider) {
  // 上一次的树可能已经被别的抽屉（账号详情）换掉了，宿主不在屏上就得重建。
  if (!mounted || !mounted.stageEl.isConnected) { mounted = null; nodes.clear(); searchInput = null; }
  if (!mounted) mount();
  receipt.set(null);
  await loadPlatforms();
  applyProvider(provider);
  paint();
}

function applyProvider(provider) {
  const p = provider ? resolvePlatform(provider) : null;
  if (!p) { picked.set(''); way.set(''); step.set('pick'); return; }
  const ways = waysFor(p);
  picked.set(p.id);
  // 只有一种接入方式时不必再问第二步（老用户的「重新登录」直奔表单）
  way.set(ways.length === 1 ? ways[0].id : '');
  step.set(ways.length === 1 ? 'form' : 'method');
}

// 行模型里的 provider 与注册表 id 不总是同一个词：workbuddy 就是腾讯那一个，
// loomy-cli 是 loomy 的落库形态。别名一律归到注册表条目上。
function resolvePlatform(id) {
  const p = plat(id);
  if (!p) return null;
  if (p.alias_of) return plat(p.alias_of) || p;
  return p;
}

/* ── 步骤进度（清单 22）──────────────────────────────────────── */
const STEP_WORDS = { pick: '选平台', method: '选接入方式', form: '完成接入', done: '加入池中' };

/** pickedPlat() —— 当前选中平台的元数据（注册表条目，或批量导入那个伪条目）。 */
function pickedPlat() {
  const id = picked.peek();
  if (!id) return null;
  return id === IMPORT_ID ? IMPORT_PSEUDO : plat(id);
}

/** pickedWays() —— 它的接入方式，推荐排第一。 */
function pickedWays() {
  const p = pickedPlat();
  return p ? waysFor(p) : [];
}

function pathSteps() {
  const out = ['pick'];
  if (pickedWays().length > 1) out.push('method');
  out.push('form', 'done');
  return out;
}

function stepKids(cur) {
  const path = pathSteps();
  const at = Math.max(0, path.indexOf(cur));
  const kids = [];
  path.forEach((s, i) => {
    const state = i < at ? 'ok' : i === at ? 'on' : '';
    // 已经走过的步骤可以点回去（正在等待的那一步不用重填）
    kids.push(h('button', {
      class: 'wiz-step ' + state,
      text: STEP_WORDS[s],
      disabled: i >= at,
      onclick: () => { step.set(s); paint(); },
    }));
    if (i < path.length - 1) kids.push(h('span', { class: 'wiz-sep', 'aria-hidden': 'true' }));
  });
  return kids;
}

function hintFor(cur) {
  if (cur === 'pick') return '先选平台。同一个平台有好几种接入方式，下一步再挑。';
  if (cur === 'method') {
    const p = pickedPlat();
    return `${p ? p.name : ''} 支持下面这几种方式，排第一的是推荐那条。`;
  }
  if (cur === 'form') {
    const ways = pickedWays();
    return (ways[wayIndex()] || {}).hint || '';
  }
  return '这一步走完，账号就在池里了。';
}

function wayIndex() {
  const i = pickedWays().findIndex(x => x.id === way.peek());
  return i < 0 ? 0 : i;
}

function paint() {
  if (!mounted || !mounted.stageEl.isConnected) return;
  const cur = step.peek();
  mounted.stepsEl.replaceChildren(...stepKids(cur));
  mounted.hintEl.textContent = hintFor(cur);
  const node = stageFor(cur);
  // 同一棵树就别重挂：replaceChildren 会把它 detach 一遍，焦点与滚动都丢。
  if (mounted.stageEl.firstElementChild !== node) mounted.stageEl.replaceChildren(node);
  mounted.footEl.replaceChildren(...footKids(cur));
}

function footKids(cur) {
  const out = [];
  const backTo = cur === 'form' ? (pickedWays().length > 1 ? 'method' : 'pick')
    : cur === 'method' ? 'pick' : '';
  if (backTo) {
    out.push(h('button', {
      class: 'btn ghost', text: '上一步',
      onclick: () => { step.set(backTo); paint(); },
    }));
  }
  out.push(h('span', { class: 'grow' }));
  // 关闭**不**停掉进行中的授权轮询：网关侧已自驱入池，人回来时账号应当已经在池里。
  out.push(h('button', { class: 'btn', text: '关闭', onclick: () => closeDrawer() }));
  return out;
}

/* ── 第一步：搜索 + 厂商分组 ─────────────────────────────────── */
function pickNode() {
  const cached = nodes.get('pick');
  if (cached) return cached;

  const listEl = h('div', { class: 'stack' });
  searchInput = h('input', {
    id: 'add-q', class: 'input search', type: 'search',
    placeholder: '搜索平台名 / 厂商 / 接入方式',
    oninput: () => { searchQ = searchInput.value.trim(); paintPickList(listEl); },
    onkeydown: ev => { if (ev.key === 'Escape') { searchInput.value = ''; searchQ = ''; paintPickList(listEl); } },
  });
  const root = h('div', { class: 'stack' },
    h('div', { class: 'searchwrap' }, icon('search', 14), searchInput),
    listEl,
  );
  nodes.set('pick', root);
  paintPickList(listEl);
  return root;
}

function groupByVendor(list) {
  const order = [];
  const by = new Map();
  for (const p of list) {
    const v = p.vendor || '其他';
    if (!by.has(v)) { by.set(v, []); order.push(v); }
    by.get(v).push(p);
  }
  return order.map(v => [v, by.get(v)]);
}

function paintPickList(listEl) {
  const q = searchQ.toLowerCase();
  const all = addable().concat([IMPORT_PSEUDO]);
  const hit = all.filter(p => {
    if (!q) return true;
    const ways = waysFor(p).map(w => w.name).join(' ');
    return (p.name || '').toLowerCase().includes(q)
      || (p.vendor || '').toLowerCase().includes(q)
      || (p.id || '').toLowerCase().includes(q)
      || (p.note || '').toLowerCase().includes(q)
      || ways.toLowerCase().includes(q);
  });
  if (!hit.length) {
    listEl.replaceChildren(h('div', { class: 'empty' },
      h('div', { class: 't', text: '没有匹配的平台' }),
      h('div', { class: 'd', text: '换个关键词试试，比如厂商名。' })));
    return;
  }
  const kids = [];
  for (const [vendor, ps] of groupByVendor(hit)) {
    kids.push(h('div', { class: 'wiz-vendor' },
      h('span', { text: vendor }),
      h('span', { class: 'n', text: String(ps.length) })));
    kids.push(...ps.map(p => platformRow(p)));
  }
  listEl.replaceChildren(...kids);
}

function platformRow(p) {
  const ways = waysFor(p);
  return h('button', {
    class: 'addrow' + (picked.peek() === p.id ? ' on' : ''),
    onclick: () => {
      picked.set(p.id);
      way.set(ways.length === 1 ? ways[0].id : '');
      step.set(ways.length === 1 ? 'form' : 'method');
      paint();
    },
  },
    h('span', { class: 'nm', text: p.name }),
    h('span', { class: 'hint', text: ways[0].name + (ways.length > 1 ? ` 等 ${ways.length} 种方式` : '') }),
    h('span', { class: 'grow' }),
    h('span', { class: 'chev', 'aria-hidden': 'true' }, '›'),
  );
}

/* ── 第二步：这个平台的接入方式 ──────────────────────────────── */
function methodNode() {
  const p = pickedPlat();
  if (!p) return h('div', { class: 'busy', text: '读取平台列表' });
  const key = 'method:' + p.id;
  const cached = nodes.get(key);
  if (cached) return cached;
  const ways = waysFor(p);
  const root = h('div', { class: 'addlist' },
    ...ways.map((w, i) => h('button', {
      class: 'addrow wiz-way',
      onclick: () => { way.set(w.id); step.set('form'); paint(); },
    },
      h('span', { class: 'nm', text: w.name }),
      i === 0 ? h('span', { class: 'chip strong', text: '推荐' }) : null,
      h('span', { class: 'grow' }),
      h('span', { class: 'chev', 'aria-hidden': 'true' }, '›'),
      h('span', { class: 'wiz-way-hint', text: w.hint }),
    )));
  nodes.set(key, root);
  return root;
}

/* ── 第三步：该平台该方式的接入节点（缓存 = 同一棵树）───────── */
function formNode() {
  const ways = pickedWays();
  const p = pickedPlat();
  if (!p || !ways.length) return pickNode();
  const w = ways[wayIndex()] || ways[0];
  const key = 'form:' + p.id + ':' + w.id;
  const cached = nodes.get(key);
  if (cached) return cached;
  const root = buildForm(p, w);
  nodes.set(key, root);
  return root;
}

function buildForm(p, w) {
  const onAdded = ref => added(ref);
  let core;
  if (p.id === IMPORT_ID) core = importNode();
  else if (p.id === 'workbuddy') core = tencentNode(onAdded);
  else if (p.id === 'loomy') core = loomyNode(w.id, onAdded);
  else if (p.id === 'zai') core = zaiAddForm(onAdded, { way: w.id });
  else if (w.id === 'login' || w.id === 'creds') {
    core = extAddPanel(onAdded, { lockProvider: p.id, only: w.id === 'creds' ? 'creds' : 'login' });
  } else {
    // json：整包凭据（没有逐字段表的平台唯一的手填路径）
    core = h('div', { class: 'glass-flat stack', style: { padding: '12px 14px', gap: '10px' } },
      h('div', { class: 'muted', style: { fontSize: '12px' },
        text: '把客户端导出的凭据整包粘进来。各平台的字段名不一样，照客户端文件里的原样贴即可。' }),
      extJsonImport(onAdded, { provider: p.id }),
    );
  }
  return h('div', { class: 'stack' }, core, helpFor(p, w));
}

/** helpFor —— 操作说明放进对应步骤（清单 24），配一张示意图。
 *  只有「要自己去客户端/浏览器里把值找出来」的方式才需要说明；
 *  扫码与浏览器授权不需要。 */
function helpFor(p, w) {
  if (!w || (w.id !== 'creds' && w.id !== 'json')) return null;
  const help = w.id === 'creds' ? credHelp(p.id) : { steps: [], fig: 'file' };
  const steps = help.steps || [];
  if (!steps.length && !help.fig) return null;
  return h('details', { class: 'wiz-help', open: steps.length > 0 ? true : null },
    h('summary', { text: steps.length ? '这些值从哪里复制' : '说明' }),
    steps.length ? h('ol', null, ...steps.map(s => h('li', { text: s }))) : null,
    help.fig ? figFor(help.fig) : null,
  );
}

/* ── 示意图（CSP 下不引外链图片，SVG 现画）─────────────────────── */
function figFor(kind) {
  return h('div', { class: 'wiz-fig' },
    pictogram(kind),
    h('div', { class: 'cap', text: CAPTIONS[kind] || '' }));
}

const CAPTIONS = {
  devtools: '值在浏览器开发者工具的 Network 面板里，请求头的 Request Headers 一段',
  proxy: '客户端 ↔ 代理 ↔ 服务器：代理能看到请求头，三个值都在那里',
  file: '值在客户端落盘的凭据文件里（storage.json / auth-session.json 一类）',
  console: '在云控制台的「API 密钥」页新建，AK/SK 只显示一次，当场复制',
};

function svgNode(...kids) {
  return svgEl('svg', { viewBox: '0 0 240 96', class: 'wiz-picto', 'aria-hidden': 'true' }, ...kids);
}

function rect(x, y, w, hh, label, hl) {
  return svgEl('g', null,
    svgEl('rect', { x, y, width: w, height: hh, rx: 5, class: hl ? 'p-box hl' : 'p-box' }),
    label ? svgEl('text', { x: x + w / 2, y: y + hh / 2 + 3.5, 'text-anchor': 'middle', class: 'p-t', text: label }) : null,
  );
}

function arrow(x1, x2, y) {
  return svgEl('path', { d: `M${x1} ${y} H${x2 - 4}m0 0 l-4 -3m4 3 l-4 3`, class: 'p-arrow' });
}

function pictogram(kind) {
  if (kind === 'devtools') {
    return svgNode(
      rect(6, 10, 150, 76, ''),                       // 浏览器窗口
      rect(12, 18, 138, 14, '网页', false),
      rect(12, 38, 60, 12, 'F12 面板', false),
      rect(12, 56, 96, 12, '那条请求的请求头', true),  // 高亮：值在这里
      rect(168, 40, 66, 16, 'x-…-cookie', true),
      arrow(158, 166, 62),
    );
  }
  if (kind === 'proxy') {
    return svgNode(
      rect(8, 34, 62, 28, '客户端', false),
      rect(96, 34, 52, 28, '代理', true),
      rect(180, 34, 54, 28, '服务器', false),
      arrow(72, 94, 48), arrow(150, 178, 48),
      rect(96, 70, 52, 16, '请求头', true),
    );
  }
  if (kind === 'console') {
    return svgNode(
      rect(8, 20, 110, 56, '云控制台', false),
      rect(24, 34, 78, 12, '我的凭证 → API 密钥', false),
      rect(24, 52, 78, 14, 'AK / SK（只显示一次）', true),
      rect(140, 34, 92, 28, '粘贴到这里', true),
      arrow(104, 138, 48),
    );
  }
  // file（默认）：客户端目录 → 凭据文件 → 输入框
  return svgNode(
    rect(8, 28, 64, 40, '客户端目录', false),
    rect(88, 34, 62, 28, '凭据文件', true),
    rect(162, 34, 70, 28, '粘贴进表单', false),
    arrow(72, 86, 48), arrow(150, 160, 48),
  );
}

/* ── 腾讯 WorkBuddy：浏览器 OAuth（从旧抽屉搬来，逻辑不变）───── */
const tencentFlow = flowStore('tencentLogin');
const TENCENT_FLOW_TTL = 15 * 60 * 1000;
let tencentCtl = null;
let tencentOnAdded = added;      // 当前该把结果交给谁（抽屉关了也仍能收尾）
const realm = signal('cn');
const loginUrl = signal('');
const loginState = signal('');
// tencentNode 里的重绘钩子：授权状态变了要刷新那一屏（节点没上屏时是空操作）。
let paintTencent = () => {};

function stopTencent() { if (tencentCtl) { tencentCtl.stop(); tencentCtl = null; } }

/** startTencentPoll() —— 3s 轮询 login/poll 直到终态。
 *  放在模块作用域：页面重载后 resumeTencentLogin() 也要能启动它，
 *  那时抽屉可能根本没打开、tencentNode() 从未执行过。 */
function startTencentPoll() {
  stopTencent();
  const state = loginState.peek();
  if (!state) return;
  tencentCtl = pollLoop();
  tencentCtl.register({
    every: 3000,
    run: async () => {
      if (!loginState.peek()) { tencentCtl.finish(); return; }
      try {
        const r = await api('login/poll?state=' + encodeURIComponent(loginState.peek()));
        if (!r.done) return;
        tencentCtl.finish();
        tencentCtl = null;
        tencentFlow.clear();
        loginState.set('');
        loginUrl.set('');
        paintTencent();
        await tencentOnAdded({ provider: 'workbuddy', id: r.uid, label: r.nickname });
      } catch (e) {
        tencentCtl.finish();
        tencentCtl = null;
        tencentFlow.clear();
        loginState.set('');
        loginUrl.set('');
        paintTencent();
        toast('授权失败：' + e.message + '（可重新获取链接）', 'fail');
      }
    },
  });
}

/** resumeTencentLogin() —— 页面重载后把未完成的腾讯授权轮询续上。 */
function resumeTencentLogin() {
  if (tencentCtl) return;      // 已在轮询
  const saved = tencentFlow.getFresh(TENCENT_FLOW_TTL);
  if (!saved || !saved.state) return;
  loginState.set(saved.state);
  loginUrl.set(saved.url || '');
  realm.set(saved.realm || 'cn');
  startTencentPoll();
}

function tencentNode(onAdded) {
  tencentOnAdded = onAdded;
  const urlBox = h('div', { class: 'stack', style: { display: 'none' } });
  const statusEl = h('div', { class: 'muted', style: { fontSize: '12px' } });
  const startRow = h('div', { class: 'row' });
  const realmSeg = h('div', { class: 'seg' });

  const REALMS = [['cn', '国内版 CN'], ['global', '国际版 Global']];

  function paintRealm() {
    realmSeg.replaceChildren(
      ...REALMS.map(([v, n]) => h('button', {
        class: realm.peek() === v ? 'on' : '', text: n,
        onclick: () => { realm.set(v); paintRealm(); paintStart(); },
      })));
    statusEl.textContent = realm.peek() === 'global'
      ? '国际版登录后自动完成注册地区、激活与试用额度领取。'
      : '登录后自动完成签到与积分初始化。';
  }

  function paintStart() {
    if (loginUrl.peek()) {
      urlBox.style.display = '';
      urlBox.replaceChildren(
        h('div', { class: 'muted', style: { fontSize: '12px' }, text: '在浏览器打开下面链接并完成登录：' }),
        h('div', { class: 'url-box', text: loginUrl.peek() }),
        h('div', { class: 'row wrap', style: { gap: '6px' } },
          h('button', {
            class: 'btn sm', onclick: async () => {
              try { await copyText(loginUrl.peek()); toast('链接已复制'); }
              catch { toast('复制失败，请手动选择', 'fail'); }
            },
          }, icon('copy'), '复制链接'),
          h('button', { class: 'btn sm', onclick: () => window.open(loginUrl.peek(), '_blank') }, '在浏览器打开'),
        ),
        h('div', { class: 'busy', text: '等待授权完成，自动检测中' }),
      );
      startRow.replaceChildren();
      return;
    }
    urlBox.style.display = 'none';
    urlBox.replaceChildren();
    startRow.replaceChildren(h('button', {
      class: 'btn primary',
      onclick: async ev => {
        const b = ev.currentTarget; b.disabled = true;
        try {
          const r = await api('login/start', { method: 'POST', body: JSON.stringify({ realm: realm.peek() }) });
          loginState.set(r.state);
          loginUrl.set(r.url);
          tencentFlow.set({ state: r.state, url: r.url, realm: realm.peek(), at: Date.now() });
          b.disabled = false;
          paintStart();
          startTencentPoll();
        } catch (e) { b.disabled = false; toast(e.message, 'fail'); }
      },
    }, icon('plus'), '获取授权链接'));
  }

  paintTencent = () => { if (realmSeg.isConnected) { paintRealm(); paintStart(); } };

  paintRealm();
  paintStart();
  if (loginState.peek() && !tencentCtl) startTencentPoll();

  return h('div', { class: 'glass-flat stack', style: { padding: '12px 14px', gap: '10px' } },
    realmSeg, statusEl, startRow, urlBox);
}

/* ── Loomy：四种方式各自的节点 ───────────────────────────────── */
function loomyNode(m, onAdded) {
  const box = h('div', { class: 'glass-flat stack', style: { padding: '12px 14px', gap: '10px' } });

  if (m === 'detect') {
    const out = h('div', { class: 'muted', style: { fontSize: '12.5px' },
      text: '还没检测。本机装着 Loomy 客户端并登录过时，这里会直接把它的状态读出来。' });
    const btn = h('button', { class: 'btn primary' }, icon('scan'), '开始检测');
    btn.onclick = async () => {
      btn.disabled = true;
      try {
        const d = await api('loomy/status');
        if (!d.has_account) {
          out.textContent = d.message || '未检测到本机 Loomy 客户端——改用手机号或 Session Token 接入。';
          return;
        }
        const s = d.status || {};
        out.replaceChildren
          ? out.replaceChildren(h('div', { class: 'task-tile' },
            h('div', null,
              h('div', { class: 't', text: 'Loomy · ' + (s.userid || '本机客户端') }),
              h('div', { class: 'c', text: `手机 ${s.phone_masked || '已登录'} · 本地缓存就绪` })),
            h('div', { class: 'p', text: `${s.earned || 0} / ${s.total || 0}` })))
          : (out.textContent = '已检测到本机 Loomy 客户端');
        await onAdded({ provider: 'loomy', id: '', label: '本机 Loomy 客户端' });
      } catch (e) { out.textContent = e.message; }
      finally { btn.disabled = false; }
    };
    box.append(btn, out);
    return box;
  }

  if (m === 'token') {
    const tok = h('input', {
      class: 'input', placeholder: 'session 字段（从客户端 userData/auth-session.json 复制）',
      style: { fontFamily: 'var(--mono)', fontSize: '12px' },
    });
    const btn = h('button', { class: 'btn primary' }, icon('check'), '验证并保存');
    btn.onclick = async () => {
      if (!tok.value.trim()) { toast('请输入 Session Token', 'fail'); return; }
      btn.disabled = true;
      try {
        await api('loomy/save', { method: 'POST', body: JSON.stringify({ session: tok.value.trim() }) });
        tok.value = '';
        await onAdded({ provider: 'loomy', id: '', label: 'Loomy Token' });
      } catch (e) { toast(e.message, 'fail'); }
      finally { btn.disabled = false; }
    };
    box.append(h('div', { class: 'muted', style: { fontSize: '12px' },
      text: '验证通过后立即落盘。这条路径不存密码，凭据到期后要用手机号或 token 再登录一次。' }),
      tok, btn);
    return box;
  }

  // pwd 与 sms 都要手机号
  const phone = h('input', { class: 'input', placeholder: '手机号', style: { flex: '1', minWidth: '160px' } });
  const kids = [h('div', { class: 'row wrap', style: { gap: '8px' } }, phone)];

  if (m === 'pwd') {
    const pwd = h('input', { class: 'input', type: 'password', placeholder: '密码', style: { flex: '1', minWidth: '160px' } });
    const btn = h('button', { class: 'btn primary' }, '登录并入池');
    btn.onclick = async () => {
      if (!phone.value.trim() || !pwd.value) { toast('手机号与密码必填', 'fail'); return; }
      btn.disabled = true;
      try {
        const r = await api('ext/loomy/login_password', {
          method: 'POST', body: JSON.stringify({ phone: phone.value.trim(), password: pwd.value }),
        });
        kids.push(h('div', { class: 'muted', style: { fontSize: '12px' }, text: '已入池。' }));
        await onAdded({ provider: 'loomy-cli', id: r.id, label: phone.value.trim() });
      } catch (e) { toast('登录失败：' + e.message, 'fail'); }
      finally { btn.disabled = false; }
    };
    kids.push(h('div', { class: 'row wrap', style: { gap: '8px' } }, pwd, btn),
      h('div', { class: 'muted', style: { fontSize: '11.5px' },
        text: '密码会加密后存进本机凭据文件，用于无人值守续期；不想存密码就改用短信验证码那一种。' }));
    box.append(...kids);
    return box;
  }

  // sms：发码 → 填码（等短信的几十秒里这一屏不会被重绘冲掉）
  const code = h('input', { class: 'input', placeholder: '6 位短信验证码', style: { flex: '1', minWidth: '120px' } });
  const sendBtn = h('button', { class: 'btn' }, '发送验证码');
  let msgid = '';
  sendBtn.onclick = async () => {
    if (!phone.value.trim()) { toast('先填手机号', 'fail'); return; }
    sendBtn.disabled = true;
    try {
      const r = await api('ext/loomy/send_sms', { method: 'POST', body: JSON.stringify({ phone: phone.value.trim() }) });
      msgid = r.msgid || '';
      toast('验证码已发送');
    } catch (e) { toast(e.message, 'fail'); }
    finally { sendBtn.disabled = false; }
  };
  const goBtn = h('button', { class: 'btn primary' }, '登录并入池');
  goBtn.onclick = async () => {
    if (!phone.value.trim() || !code.value.trim()) { toast('手机号与验证码必填', 'fail'); return; }
    goBtn.disabled = true;
    try {
      const r = await api('ext/loomy/login_sms', {
        method: 'POST', body: JSON.stringify({ phone: phone.value.trim(), code: code.value.trim(), msgid }),
      });
      await onAdded({ provider: 'loomy-cli', id: r.id, label: phone.value.trim() });
    } catch (e) { toast('登录失败：' + e.message, 'fail'); }
    finally { goBtn.disabled = false; }
  };
  kids.push(h('div', { class: 'row wrap', style: { gap: '8px' } }, sendBtn, code, goBtn),
    h('div', { class: 'muted', style: { fontSize: '11.5px' },
      text: '收到验证码前这一屏不会自己刷新，填到一半去回消息也没事。' }));
  box.append(...kids);
  return box;
}

/* ── 批量 JSON 导入 ──────────────────────────────────────────── */
function importNode() {
  const file = h('input', { type: 'file', accept: '.json', class: 'input' });
  const out = h('div', { class: 'muted', style: { fontSize: '12px' },
    text: '选择 cockpit tools 导出的 JSON 文件（账号数组）整包入池。' });
  file.addEventListener('change', async () => {
    const f = file.files && file.files[0];
    if (!f) return;
    const fd = new FormData();
    fd.append('file', f);
    out.textContent = '导入中…';
    try {
      const d = await apiUpload('import/cockpit', fd);
      out.textContent = `导入完成：成功 ${d.imported} 个${d.skipped ? `，跳过 ${d.skipped} 个` : ''}`;
      await added(null, out.textContent);
    } catch (e) { out.textContent = '导入失败：' + e.message; }
  });
  return h('div', { class: 'glass-flat stack', style: { padding: '12px 14px', gap: '10px' } }, out, file);
}

/* ── 第四步：入池确认（清单 23）─────────────────────────────── */
/** findRow(key) —— 在行模型里找刚入池的那一行。
 *  三个数据源与账号页用同一套（overview / zai / ext），所以这里的行对象与
 *  列表页里的完全同构，能直接丢给 openAccountDetail。 */
function findRow(key, id) {
  const rows = buildRows(overview()?.accounts, zaiData()?.accounts, extData()?.accounts);
  return rows.find(r => r.key === key) || rows.find(r => String(r.id) === String(id)) || null;
}

/** added —— 账号落盘后的统一收尾：刷数据 → 找到那一行 → 自动查一次余额 →
 *  直接落到该账号的详情抽屉（清单 23）。找不到行（本机 Loomy / 批量导入）
 *  就停在「加入池中」这一步把回执说清楚。 */
async function added(ref, note) {
  await Promise.all([refreshOverview(), loadZai(), loadExt()]);
  const key = ref && ref.id && ref.provider ? ref.provider + ':' + ref.id : '';
  const row = key ? findRow(key, ref.id) : null;

  if (ref && ref.provider) {
    const job = begin(`添加账号 · ${platName(ref.provider)}`, 1);
    report(job, { name: (row && row.name) || ref.label || ref.id || '新账号', ok: true, msg: '已入池' });
    finish(job);
  }

  let balNote = note || '';
  if (row) {
    const kind = bulkKind(row, 'balance');
    if (kind) {
      const r = await run(kind, row);
      balNote = r.ok ? r.msg : '余额没查到：' + r.msg;
    }
    if (mounted && mounted.stageEl.isConnected) openAccountDetail(row);
    return;
  }

  receipt.set({
    name: (ref && (ref.label || ref.id)) || '新账号',
    provider: ref ? ref.provider : '',
    note: balNote,
  });
  step.set('done');
  paint();
}

function doneNode() {
  const r = receipt.peek() || {};
  return h('div', { class: 'glass-flat stack', style: { padding: '14px', gap: '10px' } },
    h('div', { class: 'row', style: { gap: '8px' } },
      icon('check', 18),
      h('div', { style: { fontSize: '13.5px', fontWeight: '600' }, text: r.name || '已加入池中' })),
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: r.provider === 'loomy'
        ? '本机 Loomy 客户端的登录态已经可用——它不是池里的一行，任务进度在「自动化」页看。'
        : '账号已经在池里。它出现在「账号」页的平台分组下。' }),
    r.note ? h('div', { class: 'hint', text: r.note }) : null,
    h('div', { class: 'row wrap', style: { gap: '8px' } },
      r.provider === 'loomy'
        ? h('button', { class: 'btn sm', onclick: () => { closeDrawer(); navigate('tasks'); } }, '看任务进度')
        : h('button', { class: 'btn sm', onclick: () => { closeDrawer(); navigate('accounts'); } }, '打开账号页'),
      h('button', { class: 'btn sm ghost', onclick: () => { step.set('pick'); picked.set(''); paint(); } }, '再加一个'),
    ),
  );
}

function stageFor(cur) {
  if (cur === 'pick') return pickNode();
  if (cur === 'method') return methodNode();
  if (cur === 'done') return doneNode();
  return formNode();
}

/* ── 启动 ─────────────────────────────────────────────────────── */
// 页面加载即尝试续上未完成的腾讯授权（用户在浏览器里登录后，即使没开着抽屉，
// 账号也应当入池，而不是停在「等待授权」）。
resumeTencentLogin();
