/* ══════════════════════════════════════════════════════════════════
   views/logs.js · 运行日志
   频道 / 级别 / 全文 / 账号 / 平台五路筛选合成（清单 40）。
   日志流本质是文本（环形缓冲镜像 log.Printf，条目只有 ts/ch/text 三个字段，
   没有结构化的账号/平台列）——所以「账号 / 平台」按行内出现的写法命中：
   行里写了谁的 uid / 账号名 / 平台名，就能按谁筛。写法沿用 rows.js 的
   logMatcher，别处不存在第二份规则；级别判定收进唯一的 levelOf()，
   行样式与级别筛选共用它，永远不会出现「看着是红的却筛不进错误」。

   性能要点：日志框是**命令式增量更新**的——节点跨渲染保持同一份，
   新日志只 append 尾部，不重建整表（环形缓冲满 500 条时若每轮重建，
   每 5 秒就要重建 500 个节点并重置滚动，观感就是"一卡一卡"）。
   只有筛选组合变化或锚点被挤出缓冲才整表重建，其余一律走追加。
   视图的响应式部分只负责头部工具条。

   自动滚动没有开关（清单 42）：贴底与否由滚动位置判定——用户往上翻，
   新行不上屏、只计数（↓ N 条新日志），滚回底部即恢复追加并清零。
   折叠/展开状态记在条目指纹上而非 DOM 节点上（清单 41），
   增量追加与重建都不会把它弄丢。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast } from '../kernel.js';
import { defineView } from '../shell.js';
import { platName, platforms, loadPlatforms } from '../platforms.js';
import { overview, refreshOverview } from '../store.js';
import { buildRows, logMatcher } from '../rows.js';
import { openAccountDetail } from './account-detail.js';
import { zaiData, loadZai } from './zai-segment.js';
import { extData, loadExt } from './ext-segment.js';

const entries = signal([]);
const channel = signal('all');   // all | task | chat | sys
const level = signal('all');     // all | err | warn
// 全文搜索不参与 render 的依赖：打字若触发工具条重建，输入框焦点会当场丢。
// oninput 写完信号直接排一次增量同步，命中数由 matchedN 单独驱动。
const query = signal('');
const acctKey = signal('');      // 账号筛选：行模型 key，'' = 全部
const platF = signal('');        // 平台筛选：注册表 id，'' = 全部
const pending = signal(0);       // 往上翻期间欠着没上屏的新条数（清单 42）
const matchedN = signal(0);      // 当前筛选组合的命中总数

const CH_NAMES = { task: '任务', chat: '对话', sys: '系统' };

/** levelOf —— 级别判定的唯一出处：行样式与级别筛选都走它。 */
function levelOf(text) {
  return /error|失败|错误/.test(text) ? 'err' : /warn|冷却|熔断/.test(text) ? 'warn' : 'info';
}

/* 长行折叠阈值：12px 等宽在面板常规宽度下一行约 80 字，240 ≈ 三行；
   日志的结论几乎总在前两行，第三行起多是转储的参数与响应体。 */
const LONG = 240;
/* 贴底判定容差：滚动条取整与回弹都到不了几个像素。程序追加后自己把
   scrollTop 推到 scrollHeight，落进容差内——所以不必再给「自己造成的
   滚动」打标记：它天然永远不会把自己判成「往上翻」。 */
const PIN_TOL = 24;

const expanded = new Set();   // 展开态按条目指纹记：跨追加与重建都不丢（清单 41）

/* 日志框的持久节点与增量状态 */
let boxEl = null;
let pinnedNow = true;
let lastKey = null;        // 最后一条已上屏条目的指纹（增量锚点）
let shownFilter = null;    // 上次渲染所用的筛选组合指纹
let shownCount = 0;

async function load(quiet = true) {
  try {
    const d = await api('logs');
    const next = d.entries || [];
    // 内容未变则不写信号：避免每 5 秒一次无意义的重渲染
    if (next.length === entries.peek().length && sameTail(next)) return;
    entries.set(next);
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

function sameTail(next) {
  const cur = entries.peek();
  if (!cur.length) return next.length === 0;
  const a = cur[cur.length - 1], b = next[next.length - 1];
  return !!a && !!b && a.ts === b.ts && a.text === b.text;
}

function entryKey(e) { return (e.ts || '') + '|' + (e.ch || '') + '|' + (e.text || ''); }

/* ── 账号 / 平台筛选的原料：三源合一的行模型（与「账号」页同一套）─── */
let sourcesStarted = false;
/** 用户可能从没打开过账号页——首次进日志页把注册表与三个账号源各拉一次；
    拉失败静默：筛选条只是没有可给的选项，日志照常能看。 */
function ensureSources() {
  if (sourcesStarted) return;
  sourcesStarted = true;
  loadPlatforms();
  if (!overview.peek()) refreshOverview();
  if (!zaiData.peek()) loadZai();
  if (!extData.peek()) loadExt();
}

function allRows() {
  return buildRows(overview()?.accounts, zaiData()?.accounts, extData()?.accounts);
}
function rowByKey(k) { return allRows().find(r => r.key === k) || null; }

/* 行内可点的写法与 rows.js 的 logMatcher 一一对应：能按它筛，就能按它跳。
   最短 4 个字符——更短的 id 子串逢什么都像命中，宁可不给可点目标，
   也不给一个多半会点错账号的死链接。 */
function acctTokens(r) {
  if (r.provider === 'workbuddy') return ['uid=' + r.id, '(' + String(r.id).slice(0, 8) + ')'];
  if (r.provider === 'zai') { const n = String((r.raw && r.raw.name) || r.id); return ['账号=' + n, n]; }
  return ['acct=' + r.id, String(r.id)];
}
function acctLink(text) {
  let best = null;
  for (const r of allRows()) {
    for (const tk of acctTokens(r)) {
      if (tk.length < 4) continue;
      const at = text.indexOf(tk);
      if (at >= 0 && (!best || at < best.at || (at === best.at && tk.length > best.token.length))) {
        best = { row: r, token: tk, at };
      }
    }
  }
  return best;
}

function platMatcher(pid) {
  const name = platName(pid);
  const hits = allRows().filter(r => r.provider === pid).map(r => logMatcher(r));
  return t => (!!name && t.includes(name)) || t.includes(pid) || hits.some(f => f(t));
}

/** 五路筛选合成：频道 AND 级别 AND 全文 AND 账号 AND 平台。 */
function filtered() {
  const ch = channel.peek(), lv = level.peek();
  const q = query.peek().trim().toLowerCase();
  const ak = acctKey.peek(), pid = platF.peek();
  const acctHit = ak ? (() => { const r = rowByKey(ak); return r && logMatcher(r); })() : null;
  const platHit = pid ? platMatcher(pid) : null;
  return entries.peek().filter(e => {
    const t = String(e.text || '');
    if (ch !== 'all' && e.ch !== ch) return false;
    if (lv !== 'all' && levelOf(t) !== lv) return false;
    if (q && !t.toLowerCase().includes(q)) return false;
    if (acctHit && !acctHit(t)) return false;
    if (platHit && !platHit(t)) return false;
    return true;
  });
}

function filterKey() {
  return [channel.peek(), level.peek(), query.peek().trim().toLowerCase(),
    acctKey.peek(), platF.peek()].join('\u0001');
}

/** 账号 / 平台 chip 只给当前日志里真的出现过的选项（清单 40）。 */
function scanOptions() {
  const all = entries.peek();
  const acct = [];
  const pl = new Map();
  for (const r of allRows()) {
    const hit = logMatcher(r);
    let n = 0;
    for (const e of all) if (hit(String(e.text || ''))) n++;
    if (!n) continue;
    acct.push({ v: r.key, name: r.name, n });
    pl.set(r.provider, (pl.get(r.provider) || 0) + n);
  }
  // 行模型没覆盖到的写法（日志直接提到平台名/前缀）也算该平台出现过
  for (const p of platforms.peek()) {
    if (pl.has(p.id)) continue;
    let n = 0;
    for (const e of all) {
      const t = String(e.text || '');
      if ((p.name && t.includes(p.name)) || (p.prefix && t.includes(p.prefix))) n++;
    }
    if (n) pl.set(p.id, n);
  }
  acct.sort((a, b) => b.n - a.n);
  return { acct, plat: [...pl].map(([id, n]) => ({ v: id, name: platName(id), n })) };
}

/* ── 行渲染 ─────────────────────────────────────────────────────── */

function paintLine(node, e, withCh) {
  const lv = levelOf(e.text);
  node.className = 'ln' + (lv === 'err' ? ' strong' : lv === 'warn' ? ' dim' : '');
  const key = entryKey(e);
  const time = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
  const kids = [
    withCh ? h('i', { class: 'ch', text: CH_NAMES[e.ch] || e.ch }) : null,
    time ? time + ' ' : null,
  ];
  const fold = e.text.length > LONG && !expanded.has(key);
  const body = fold ? e.text.slice(0, LONG) : e.text;
  // 折叠时行内文本不完整，账号链接留到展开后再给——半截 uid 点不得
  const link = fold ? null : acctLink(body);
  if (link) {
    kids.push(body.slice(0, link.at));
    kids.push(h('button', {
      class: 'ln-a', title: '看该账号详情', text: link.token,
      onclick: () => openAccountDetail(link.row),
    }));
    kids.push(body.slice(link.at + link.token.length));
  } else {
    kids.push(body);
  }
  if (fold) {
    kids.push(h('button', {
      class: 'ln-x', text: `展开（还有 ${e.text.length - LONG} 字）`,
      onclick: () => { expanded.add(key); paintLine(node, e, withCh); },
    }));
  } else if (e.text.length > LONG) {
    kids.push(h('button', {
      class: 'ln-x', text: '收起',
      onclick: () => { expanded.delete(key); paintLine(node, e, withCh); },
    }));
  }
  node.replaceChildren(...kids);
}

function entryNode(e, withCh) {
  const n = h('span', { class: 'ln' });
  paintLine(n, e, withCh);
  return n;
}

function emptyNode() {
  const total = entries.peek().length;
  if (!total) return h('div', { class: 'empty' }, h('div', { class: 't', text: '暂无日志' }));
  const act = [];
  if (channel.peek() !== 'all') act.push('频道：' + (CH_NAMES[channel.peek()] || channel.peek()));
  if (level.peek() !== 'all') act.push('级别：' + (level.peek() === 'err' ? '只看错误' : '只看警告'));
  const q = query.peek().trim();
  if (q) act.push('搜索：' + q);
  const r = acctKey.peek() ? rowByKey(acctKey.peek()) : null;
  if (r) act.push('账号：' + r.name);
  if (platF.peek()) act.push('平台：' + platName(platF.peek()));
  return h('div', { class: 'empty' },
    h('div', { class: 't', text: '没有命中的日志' }),
    h('div', { class: 'd', text: `以下 ${act.length} 个条件排除了全部 ${total} 条：` + act.join(' · ') }),
    h('button', {
      class: 'btn sm', text: '清除筛选',
      onclick: () => { channel.set('all'); level.set('all'); query.set(''); acctKey.set(''); platF.set(''); },
    }),
  );
}

/* ── 增量同步 ───────────────────────────────────────────────────── */

function rebuild(box, list) {
  const withCh = channel.peek() === 'all';
  box.replaceChildren(...(list.length ? list.map(e => entryNode(e, withCh)) : [emptyNode()]));
  shownFilter = filterKey();
  shownCount = list.length;
  lastKey = list.length ? entryKey(list[list.length - 1]) : null;
  pending.set(0);
  if (pinnedNow) box.scrollTop = box.scrollHeight || 0;
}

/** 增量同步日志框：正常只追加新行；筛选变化/内容不连续时才整体重建 */
function syncBox() {
  const box = boxEl;
  if (!box || !box.isConnected) return;
  const list = filtered();
  matchedN.set(list.length);

  if (filterKey() !== shownFilter || list.length < shownCount || (lastKey === null && list.length)) {
    rebuild(box, list);
    return;
  }
  // 以「最后一条已渲染的 key」为锚点，只追加其后的新行——
  // 环形缓冲把旧行挤出时锚点仍在表内，追加依然正确。
  let at = -1;
  for (let i = list.length - 1; i >= 0; i--) {
    if (entryKey(list[i]) === lastKey) { at = i; break; }
  }
  if (at < 0) { rebuild(box, list); return; }
  const fresh = list.length - at - 1;
  if (!fresh) return;
  if (!pinnedNow) { pending.set(fresh); return; }   // 用户往上翻着：只计数，不上屏（清单 42）
  const withCh = channel.peek() === 'all';
  const frag = document.createDocumentFragment();
  for (let i = at + 1; i < list.length; i++) frag.append(entryNode(list[i], withCh));
  box.append(frag);
  shownCount = list.length;
  lastKey = entryKey(list[list.length - 1]);
  pending.set(0);
  box.scrollTop = box.scrollHeight || 0;
}

/** 贴底判定（清单 42）：容差内的 scroll 一律视为「在底部」。
    自己追加后推 scrollTop 造成的事件落在容差里，反转不了贴底态。 */
function onBoxScroll() {
  if (!boxEl) return;
  const gap = (boxEl.scrollHeight || 0) - (boxEl.scrollTop || 0) - (boxEl.clientHeight || 0);
  const near = gap <= PIN_TOL;
  if (near === pinnedNow) return;
  pinnedNow = near;
  // 滚回底部：立刻把欠着的行补上屏并把计数清零（都在 syncBox 里做）
  if (near) syncBox();
}

/* ── 视图 ───────────────────────────────────────────────────────── */

export default defineView({
  id: 'logs',
  page: 'automation',
  tab: '执行记录',
  title: '执行记录',
  icon: 'logs',
  keywords: '日志 log 运行 输出 执行记录',
  sub() {
    const list = entries();
    const counts = {};
    let err = 0;
    for (const e of list) {
      counts[e.ch] = (counts[e.ch] || 0) + 1;
      if (levelOf(String(e.text || '')) === 'err') err++;
    }
    return `任务 ${counts.task || 0} · 对话 ${counts.chat || 0} · 系统 ${counts.sys || 0}` +
      (err ? ` · 错误 ${err}` : '');
  },
  tick() { load(); },
  render() {
    if (!entries().length) load();
    ensureSources();
    // 读信号以注册依赖（数据/筛选变化时重建工具条并触发增量同步）
    const all = entries();
    const ch = channel();
    const lv = level();
    const ak = acctKey();
    const pid = platF();
    const pend = pending();
    const hitN = matchedN();
    overview(); zaiData(); extData(); platforms();

    if (!boxEl) {
      boxEl = h('div', { class: 'logbox' });
      boxEl.addEventListener('scroll', onBoxScroll);
    }
    // 视图节点挂载完成后同步（此时 boxEl 已在文档中，滚动才生效）
    queueMicrotask(syncBox);

    const opts = scanOptions();
    const seg = (cur, items, set) => h('div', { class: 'seg' },
      ...items.map(([v, n]) => h('button', { class: cur === v ? 'on' : '', text: n, onclick: () => set(v) })));
    const chips = (label, items, cur, set) => items.length
      ? h('div', { class: 'fchips' },
          h('span', { class: 'muted', text: label }),
          ...items.map(it => h('button', {
            // 再点一次同一枚 chip = 取消这一路筛选（五路 AND，得能逐路退掉）
            class: 'fchip' + (cur === it.v ? ' on' : ''),
            onclick: () => set(cur === it.v ? '' : it.v),
          }, h('span', { class: 'nm', text: it.name }), h('span', { class: 'n', text: String(it.n) }))))
      : null;

    return h('div', { class: 'view stack' },
      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '运行日志' }),
          h('span', { class: 'grow' }),
          h('span', { class: 'muted', text: `命中 ${hitN} / ${all.length} 条` }),
          h('button', { class: 'btn sm ghost', onclick: () => load(false) }, icon('refresh'), '刷新'),
        ),
        h('div', { class: 'logbar' },
          seg(ch, [['all', '全部'], ['task', '任务'], ['chat', '对话'], ['sys', '系统']], channel.set),
          seg(lv, [['all', '全部'], ['err', '只看错误'], ['warn', '只看警告']], level.set),
          h('input', {
            class: 'input search', type: 'search', placeholder: '搜索日志内容',
            value: query.peek(),
            oninput: ev => { query.set(ev.target.value); queueMicrotask(syncBox); },
          }),
        ),
        chips('账号', opts.acct, ak, acctKey.set),
        chips('平台', opts.plat, pid, platF.set),
        h('div', { class: 'body flush' },
          boxEl,
          pend > 0 ? h('div', { class: 'newlogbar' },
            h('button', {
              class: 'btn sm',
              onclick: () => { pinnedNow = true; syncBox(); },   // 点了才把欠着的行上屏并跳到底
            }, icon('download'), `↓ ${pend} 条新日志`)) : null,
        ),
      ),
    );
  },
});
