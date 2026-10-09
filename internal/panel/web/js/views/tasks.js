/* ══════════════════════════════════════════════════════════════════
   views/tasks.js · 任务
   纯任务工作台：今日待办（按任务类型组织）+ 排程台账。
   （账号管理归「账号」视图：Z.AI 段在 views/zai-segment.js，
   外部平台段在 views/ext-segment.js，由 accounts.js 装配。）
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog } from '../kernel.js';
import { defineView, parseHash, setHashTab } from '../shell.js';
import { refreshOverview } from '../store.js';
import { openVouchers } from '../drawers.js';
import { platName } from '../platforms.js';

const SEGS = ['todo', 'sched'];
const seg = signal('todo');

// 旧深链里这两个 tab 名是平台（腾讯 / Loomy）。清单 28 之后它们都落在同一个
// 「今日待办」屏上——按类型组织后，平台不再是一屏的分界，也就没有各自的 tab。
const SEG_LEGACY = { tencent: 'todo', loomy: 'todo' };

// 页内子状态：#tasks?tab=sched —— 深链/刷新/后退都落在同一屏
export function syncSegFromHash() {
  const v = parseHash().tab;
  if (SEGS.includes(v)) seg.set(v);
  else if (SEG_LEGACY[v]) seg.set(SEG_LEGACY[v]);
}
function setSeg(v) {
  seg.set(v);
  setHashTab('automation', v);
  if (v === 'todo') { loadLoomy(); loadCredits(); }
  if (v === 'sched') loadReport();
}
const queue = signal(null);
const queueSeq = signal(0);
const conc = signal(1);
const platFilter = signal('all');
const loomy = signal(null);
const credits = signal(null);
const report = signal(null);

let queueTimer = null;
const GROWTH_TITLES = {};

/* ── 成长任务队列 ─────────────────────────────────────────────── */
let checking = false;   // 检查是一次上游扫描，重绘期间不许再点一次

/** checkAndRun —— 「检查并执行」（清单 25：两条动作合一）。
 *  早先「扫描待办」和「执行全部待办」是两个按钮、两次独立的上游扫描：用户点完
 *  扫描再看一眼列表，再点执行，服务端其实又扫了一遍——而中间那一眼并没有拦住
 *  任何东西，确认框里只写着「确定吗」。现在一条动作走到底：先扫，把要做的东西
 *  按类型列出来给人看过，确认后才排队。 */
async function checkAndRun(ev) {
  const btn = ev.currentTarget;
  if (checking) return;
  checking = true;
  btn.disabled = true;
  stopQueuePoll();
  let d;
  try {
    d = await api('tasks/scan_all', { method: 'POST' });
  } catch (e) {
    toast(e.message, 'fail');
    return;
  } finally {
    checking = false;
    btn.disabled = false;
  }
  queue.set({ started: true, running: false, items: [], scanned: d });
  const groups = groupScanned(d);
  scannedGroups.set(groups);
  const n = groups.reduce((s, g) => s + g.rows.length, 0);
  if (!n) { toast('检查完了：没有待办任务'); return; }
  if (!await confirmDialog(scanSummary(groups, n), { ok: '排队执行' })) return;
  await startQueue();
}

/** scanSummary —— 确认框要说清「将要做什么」：几项、哪几类、怎么个跑法。 */
function scanSummary(groups, n) {
  const extN = groups.filter(g => String(g.uid).startsWith('ext:'))
    .reduce((s, g) => s + g.rows.length, 0);
  const parts = [];
  if (n - extN > 0) parts.push(`成长任务 ${n - extN} 项`);
  if (extN > 0) parts.push(`外部平台签到/领奖 ${extN} 项`);
  return `共 ${n} 项待办，涉及 ${groups.length} 个账号：${parts.join('、')}。` +
    `账号之间同时跑 ${conc.peek()} 个、账号内串行；含真实对话的任务耗时较长。`;
}

/** startQueue —— 真的排队。服务端会自己重新扫一遍待办，所以前端这份清单
 *  只用来给人看，不作为执行输入（队列的准确性以服务端为准）。 */
async function startQueue() {
  const c = conc.peek();
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: c }) });
    if (!r.started) { toast(r.message || '没有待办任务'); return; }
    queueSeq.set(r.seq || 0);
    scannedGroups.set(null);
    toast(`队列已启动：${r.total} 项（同时跑 ${c} 个）`);
    startQueuePoll();
  } catch (e) { toast(e.message, 'fail'); }
}

const scannedGroups = signal(null);

function groupScanned(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? `${t.current}/${t.target}` : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  // 外部平台待办（签到 / 领奖类，一账号一条）。
  // uid 必须与队列项一致（"ext:provider/id"），否则扫描结果与执行进度
  // 会分到两个组里、对不上。
  for (const e of (d.ext || [])) {
    groups.push({
      uid: 'ext:' + e.provider + '/' + e.id,
      nick: platName(e.provider) + ' · ' + (e.label || e.id),
      rows: [{ kind: 'ext', code: e.provider, prog: '—', status: 'scan', note: e.note }],
    });
  }
  return groups;
}

function startQueuePoll() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(pollQueue, 3000);
  pollQueue();
}

function stopQueuePoll() { if (queueTimer) { clearInterval(queueTimer); queueTimer = null; } }

async function pollQueue() {
  let q;
  try { q = await api('tasks/queue'); } catch { return; }
  if (!q.started) return;
  if (queueSeq.peek() && q.seq !== queueSeq.peek()) return;
  queue.set(q);
  if (!q.running) {
    stopQueuePoll();
    toast('任务队列执行结束');
    refreshOverview();
  }
}

function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({ kind: it.kind, code: it.code, prog: '', status: it.status, message: it.message });
  }
  return [...by.values()];
}

const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
const DOT_CLS = { done: 'dot', running: 'dot ring', error: 'dot off', skipped: 'dot off', pending: 'dot ring', scan: 'dot ring' };

/* ── 清单 28：这一屏的主轴是「任务类型」，平台只是行上的标签 ──────
   以前这里按平台分 tab（腾讯任务 / Loomy），可用户打开这一页要回答的是
   「今天有什么没做」，不是「哪个平台有什么没做」——平台数上到十几个以后，
   按平台分 tab 等于把同一类任务切成十几份，每份都要点进去看一遍。
   现在三类任务各占一组（成长任务 / 每日签到 / 本机客户端任务），
   平台降级成筛选条件，账号名和平台名留在每一行上。 */
const TYPES = [
  { kind: 'growth', name: '成长任务', tip: '账号池里每个账号的每日成长任务；含真实对话的那几项耗时较长' },
  { kind: 'ext', name: '每日签到 / 领奖', tip: '外部平台客户端的每日一次动作' },
  { kind: 'client', name: '本机客户端任务', tip: '本机装着的客户端自己报回来的任务清单' },
];

/** flatRows —— 把「按账号分组」的扫描/队列结果摊平成按类型的行。
 *  uid 的形态就是来源：ext:provider/id 是外部平台，其余是账号池（腾讯）。 */
function flatRows(groups) {
  const out = [];
  for (const g of groups || []) {
    for (const r of g.rows) {
      const plat = r.kind === 'ext' ? String(r.code).split('/')[0] : 'workbuddy';
      out.push({ ...r, plat, acct: g.nick || String(g.uid).slice(0, 12) });
    }
  }
  return out;
}

/** clientRows —— 本机 Loomy 的任务清单摊成同样的行形状（没有客户端就什么都没有）。 */
function clientRows() {
  const d = loomy();
  if (!d || !d.has_account) return [];
  return ((d.status || {}).items || []).map(it => ({
    kind: 'client',
    code: it.category || 'task',
    plat: 'loomy',
    acct: 'Loomy',
    title: it.title,
    prog: it.points ? `+${it.points}` : '',
    status: it.completed ? 'done' : 'scan',
  }));
}

function queueRow(it) {
  const title = it.kind === 'client' ? (it.title || it.code)
    : it.kind === 'ext' ? (it.name || '每日签到 / 领奖')
    : (GROWTH_TITLES[it.code] || it.code);
  const st = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return h('div', { class: 'qrow', title: it.message || it.note || '' },
    h('span', { class: 'code', text: String(it.code || '').split('/').pop() }),
    h('span', { class: 'name' }, h('span', { class: 't', text: title }),
      // 平台和账号名是这一行的两个标签（清单 28：它们不再是分 tab 的理由）
      h('span', { class: 'chip faint', text: it.acct || '' }),
      h('span', { class: 'chip faint', text: platName(it.plat) })),
    h('span', { class: 'prog', text: it.prog || '' }),
    h('span', { class: 'st' }, h('i', { class: DOT_CLS[it.status] || 'dot ring' }), st),
    h('span', { class: 'msg', text: it.message || '' }),
  );
}

/* ── 本机客户端：状态与余额（原 Loomy tab 的数据源）────────────── */
async function loadLoomy(quiet = true) {
  try {
    loomy.set(await api('loomy/status'));
  } catch (e) {
    loomy.set(null);
    if (!quiet) toast(e.message, 'fail');
  }
}

async function loadCredits(quiet = true) {
  try {
    const d = await api('loomy/credits');
    credits.set(d.has_account ? d : null);
  } catch (e) { credits.set({ error: e.message }); if (!quiet) toast(e.message, 'fail'); }
}

/** clientActions —— 那一组自己的动作：成长值/积分两个数 + 签到、一键完成。
 *  原来这些按钮在 Loomy 那个 tab 的页头；按类型组织后它们跟着所属任务组走。 */
function clientActions() {
  const d = loomy();
  const st = (d && d.status) || {};
  const c = credits();
  const act = (path, word, after) => async ev => {
    const btn = ev.currentTarget;
    btn.disabled = true;
    try {
      const r = await api(path, { method: 'POST' });
      toast(typeof r === 'string' ? r : (r.message || word));
      await after(r);
    } catch (e) { toast(e.message, 'fail'); }
    finally { btn.disabled = false; }
  };
  return h('span', { class: 'row wrap', style: { gap: '5px' } },
    st.earned != null ? h('span', { class: 'chip', text: `成长值 ${st.earned}/${st.total || 10000}` }) : null,
    c && !c.error ? h('span', {
      class: 'chip faint',
      text: `积分 永久 ${c.credits?.permanent ?? 0} · 每日 ${c.credits?.daily ?? 0}` +
        (c.credits?.has_quota ? ` · 今日额度 ${c.credits.daily_quota} 已用 ${c.credits.daily_consumed}` : ' · 未签到'),
    }) : null,
    h('button', {
      class: 'btn sm',
      onclick: act('loomy/checkin', '每日额度已激活', async () => { await loadLoomy(); await loadCredits(); }),
    }, icon('checkin'), '每日签到'),
    h('button', {
      class: 'btn sm',
      onclick: act('loomy/complete_all', '所有任务已是完成态',
        async r => { toast((r.completed || []).length > 0 ? `新点亮 ${(r.completed || []).length} 项任务` : '所有任务已是完成态'); await loadLoomy(); }),
    }, icon('check'), '一键完成'),
    h('button', { class: 'btn sm ghost', onclick: () => { loadLoomy(false); loadCredits(false); } }, icon('refresh'), '刷新'),
  );
}

function todoSeg() {
  const q = queue();
  const groups = scannedGroups() || (q && q.items && q.items.length ? groupsFromQueue(q.items) : null);
  const rows = [...flatRows(groups), ...clientRows()];
  const plats = [...new Set(rows.map(r => r.plat))];
  const f = platFilter();
  const shown = f === 'all' ? rows : rows.filter(r => r.plat === f);
  const items = (q && q.items) || [];
  const total = items.length;
  const done = items.filter(i => ['done', 'error', 'skipped'].includes(i.status)).length;

  return h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('header', null,
        h('h2', { text: '今日待办' }),
        h('span', { class: 'hint', text: '先检查出没做的，看过清单再排队' }),
        h('span', { class: 'grow' }),
        // 清单 26：「并发」是实现词，且这一档大多数人根本不该每次动——标签说清它
        // 数的是什么、为什么默认 1，选择器本身留在原地（一次动作的选项，不是配置）。
        h('div', {
          class: 'seg',
          title: '账号之间同时跑几个，账号内始终串行。默认 1 最稳：并行越多越容易被上游判成异常流量。',
        },
          h('span', { class: 'muted', style: { fontSize: '11.5px', padding: '0 7px' }, text: '同时跑' }),
          ...[1, 2, 3].map(n => h('button', { class: conc() === n ? 'on' : '', text: String(n), onclick: () => conc.set(n) })),
        ),
        h('button', { class: 'btn sm', onclick: () => openVouchers() }, icon('ticket'), '活动券码'),
        checking
          ? h('button', { class: 'btn sm primary spin', disabled: true, text: '检查中…' })
          : h('button', { class: 'btn sm primary', onclick: checkAndRun }, icon('play'), '检查并执行'),
      ),
      // 平台筛选（清单 28：平台从 tab 降成筛选条件，只在确实有多于一个平台时才出现）
      plats.length > 1
        ? h('div', { class: 'row wrap', style: { gap: '6px', padding: '12px 18px 0' } },
          ...[['all', '全部平台'], ...plats.map(p => [p, platName(p)])].map(([v, name]) => h('button', {
            class: 'chip ' + (f === v ? 'strong' : 'faint'), text: name, onclick: () => platFilter.set(v),
          })),
        )
        : null,
      total > 0
        ? h('div', { style: { padding: '0 18px 12px' } },
          h('div', { class: 'row', style: { gap: '12px' } },
            h('div', { class: 'meter', style: { flex: '1' } },
              h('i', { style: { width: (total ? Math.round(done / total * 100) : 0) + '%' } })),
            h('span', {
              class: 'muted', style: { font: '11.5px var(--mono)' },
              text: `${q.running ? '执行中' : '已结束'} ${done}/${total}`,
            }),
          ))
        : null,
      shown.length
        ? h('div', null, ...TYPES.map(t => {
          const rs = shown.filter(r => r.kind === t.kind);
          if (!rs.length) return null;
          return h('div', { class: 'qgroup' },
            h('div', { class: 'head' },
              h('span', { class: 'nm', text: t.name, title: t.tip }),
              h('span', { class: 'cnt', text: `${rs.length} 项` }),
              h('span', { class: 'grow' }),
              t.kind === 'client' ? clientActions() : null),
            ...rs.map(queueRow),
          );
        }))
        : h('div', { class: 'body' }, h('div', { class: 'empty' }, icon('automation'),
          h('div', { class: 't', text: f === 'all' ? '还没有检查过' : '这个平台现在没有待办' }),
          h('div', {
            class: 'd',
            text: '检查各账号的成长任务与各平台的每日签到，把没做的按类型排成一列，看过清单再执行。',
          }))),
    ),
  );
}

/* ── 排程：任务运行台账（每轮成/败/待补跑）───────────────────── */
async function loadReport(quiet = true) {
  try { report.set(await api('task_report')); }
  catch (e) { report.set(null); if (!quiet) toast(e.message, 'fail'); }
}

const RES_WORD = { done: '成功', skip: '跳过', failed: '失败', needs_relogin: '需重新登录' };
const RES_DOT = { done: 'dot', failed: 'dot off', needs_relogin: 'dot off', skip: 'dot ring' };

function hm(v) {
  const d = new Date(v);
  return isNaN(d) ? String(v || '') : d.toLocaleTimeString('zh-CN', { hour12: false });
}

function outcomeRow(o) {
  return h('div', { class: 'qrow', title: o.message || '' },
    h('span', { class: 'code', text: o.task }),
    h('span', { class: 'name' }, h('span', { class: 't', text: o.account })),
    h('span', { class: 'st' }, h('i', { class: RES_DOT[o.result] || 'dot ring' }), RES_WORD[o.result] || o.result),
    h('span', { class: 'msg', text: o.message || '' }),
    h('span', { class: 'prog', text: hm(o.at) }),
  );
}

function roundBlock(rd, isLast) {
  const counts = {};
  for (const o of rd.outcomes || []) counts[o.result] = (counts[o.result] || 0) + 1;
  const ok = counts.done || 0, bad = (counts.failed || 0) + (counts.needs_relogin || 0);
  return h('div', { class: 'qgroup' },
    h('div', { class: 'head' },
      h('span', { class: 'nm', text: rd.trigger }),
      h('span', { class: 'cnt', text: `${hm(rd.started)} · 成 ${ok} / 败 ${bad} / 跳 ${counts.skip || 0}` }),
      isLast ? h('span', { class: 'chip strong', text: '最近一轮' }) : null),
    ...(rd.outcomes || []).map(outcomeRow),
  );
}

/** schedStrip —— 排程要先回答的三个问题（清单 27）：今天跑过了什么、现在有没有在
 *  跑、下一次几点。原来这一屏开口就是逐轮台账，没跑过时是一片空白，什么也没说。
 *  「现在」只报手动队列的真实状态——排程的自动轮次没有运行中这个信号，不编。 */
function schedStrip(r) {
  const midnight = new Date(); midnight.setHours(0, 0, 0, 0);
  const rounds = r.rounds || [];
  const ranToday = rounds.filter(x => new Date(x.started) >= midnight).length;
  const last = rounds[rounds.length - 1];
  const q = queue();
  const items = (q && q.items) || [];
  const done = items.filter(i => ['done', 'error', 'skipped'].includes(i.status)).length;

  const tile = (label, value, note) => h('div', {
    class: 'glass-flat', style: { padding: '10px 12px', minWidth: '158px' },
  },
    h('div', { class: 'muted', style: { fontSize: '11px' }, text: label }),
    h('div', { style: { fontSize: '14.5px', fontWeight: '600', marginTop: '2px' }, text: value }),
    note ? h('div', { class: 'muted', style: { fontSize: '11px', marginTop: '2px' }, text: note }) : null,
  );

  return h('div', { class: 'row wrap', style: { gap: '8px' } },
    tile('今天', ranToday ? `跑过 ${ranToday} 轮` : '还没跑过',
      ranToday ? `累计成 ${r.day_done} 项`
        : (last ? `上一轮 ${hm(last.started)} · ${last.trigger}` : '台账里还没有轮次')),
    tile('现在', q && q.running ? '正在执行' : '空闲',
      q && q.running ? `手动队列 ${done}/${items.length} 项` : '到点才会自己动'),
    tile('下一次', r.next_fire ? hm(r.next_fire) : '未排程',
      r.next_fire ? (r.next_kinds || []).join('、') : '在「设置」里启用排程时段'),
  );
}

function schedSeg() {
  const r = report();
  const head = h('header', null,
    h('h2', { text: '任务运行台账' }),
    h('span', { class: 'hint', text: '每一轮排程的真实结论：成功 / 瞬时失败（会自动退避重试）/ 需重新登录（不重试，等重登）' }),
    h('span', { class: 'grow' }),
    h('button', { class: 'btn sm', onclick: () => loadReport(false) }, icon('refresh'), '刷新'));

  if (!r) return h('section', { class: 'card' }, head, h('div', { class: 'body' },
    h('div', { class: 'empty' }, icon('automation'), h('div', { class: 't', text: '暂无台账' }),
      h('div', { class: 'd', text: '调度器还没有派发过任务；也可以在「今日待办」里点「检查并执行」先跑一轮。' }))));

  const pend = r.pending || [];
  const rounds = (r.rounds || []).slice().reverse();
  const body = h('div', { class: 'body stack' },
    // 时间线排在台账前面（清单 27）：先看今天动过没有、下一次几点，再往下看逐轮明细
    schedStrip(r),
    pend.length
      ? h('div', { class: 'row wrap', style: { gap: '8px' } },
        ...pend.map(p => h('span', {
          class: 'chip', style: { whiteSpace: 'normal' },
          text: `${p.task} 待补跑 ${(p.uids || []).length} 个（第 ${p.tries} 次）· ${p.next_at}` })))
      : null,
    rounds.length
      ? h('div', null, ...rounds.map((rd, i) => roundBlock(rd, i === 0)))
      : h('div', { class: 'empty' }, icon('automation'), h('div', { class: 't', text: '本轮还没有结论' })));
  return h('section', { class: 'card' }, head, body);
}

export default defineView({
  id: 'tasks',
  page: 'automation',
  tab: '任务',
  title: '任务',
  icon: 'automation',
  keywords: '任务 签到 成长任务 队列 自动化 排程 台账',
  sub() {
    const q = queue();
    if (seg.peek() === 'todo' && q && q.running) return '任务队列执行中…';
    return seg.peek() === 'sched' ? '今日待办 · 排程台账' : '成长任务 · 每日签到 · 本机客户端任务';
  },
  tick() {
    if (seg.peek() === 'todo') { loadLoomy(); if (queueTimer) pollQueue(); }
    if (seg.peek() === 'sched') loadReport();
  },
  render() {
    syncSegFromHash();
    const s = seg();
    return h('div', { class: 'view stack' },
      h('div', { class: 'row' },
        // 两条屏按「做什么」分：今天要办的事情 / 排程跑出来的台账（清单 28）
        h('div', { class: 'seg' },
          h('button', { class: s === 'todo' ? 'on' : '', text: '今日待办', onclick: () => setSeg('todo') }),
          h('button', { class: s === 'sched' ? 'on' : '', text: '排程台账', onclick: () => setSeg('sched') }),
        ),
      ),
      s === 'todo' ? todoSeg() : schedSeg(),
    );
  },
});
