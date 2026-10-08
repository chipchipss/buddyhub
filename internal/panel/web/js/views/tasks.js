/* ══════════════════════════════════════════════════════════════════
   views/tasks.js · 任务
   纯任务工作台：腾讯成长任务队列 / Loomy 新手之旅 / 开学季券码。
   （账号管理归「账号」视图：Z.AI 段在 views/zai-segment.js，
   外部平台段在 views/ext-segment.js，由 accounts.js 装配。）
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog } from '../kernel.js';
import { defineView, parseHash, setHashSeg } from '../shell.js';
import { refreshOverview } from '../store.js';
import { openVouchers } from '../drawers.js';
import { platName } from '../platforms.js';

const SEGS = ['tencent', 'loomy', 'sched'];
const seg = signal('tencent');

// hash 子状态：#tasks?seg=loomy —— 深链/刷新/后退都落在同一段
export function syncSegFromHash() {
  const v = parseHash().seg;
  if (SEGS.includes(v)) seg.set(v);
}
function setSeg(v) {
  seg.set(v);
  setHashSeg('tasks', v);
  if (v === 'loomy') { loadLoomy(); loadCredits(); }
  if (v === 'sched') loadReport();
}
const queue = signal(null);
const queueSeq = signal(0);
const conc = signal(1);
const loomy = signal(null);
const loomyMsg = signal('');
const credits = signal(null);
const ext = signal(null);
const report = signal(null);

let queueTimer = null;
const GROWTH_TITLES = {};

/* ── 腾讯：成长任务队列 ───────────────────────────────────────── */
async function scanAll(ev) {
  const btn = ev.currentTarget;
  btn.disabled = true;
  stopQueuePoll();
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    queue.set({ started: true, running: false, items: [], scanned: d });
    scannedGroups.set(groupScanned(d));
  } catch (e) { toast(e.message, 'fail'); }
  finally { btn.disabled = false; }
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

async function runQueue(ev) {
  const btn = ev.currentTarget;
  const c = conc.peek();
  if (!await confirmDialog(`扫描全部账号待办并排队执行（账号并发 ${c}，账号内串行）。含真实对话的任务耗时较长。`, { ok: '开始执行' })) return;
  btn.disabled = true;
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: c }) });
    if (!r.started) { toast(r.message || '没有待办任务'); return; }
    queueSeq.set(r.seq || 0);
    scannedGroups.set(null);
    toast(`队列已启动：${r.total} 项（并发 ${c}）`);
    startQueuePoll();
  } catch (e) { toast(e.message, 'fail'); }
  finally { btn.disabled = false; }
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
    by.get(it.uid).rows.push({ kind: it.kind, code: it.code, prog: it.kind === 'school' ? '—' : '', status: it.status, message: it.message });
  }
  return [...by.values()];
}

const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
const DOT_CLS = { done: 'dot', running: 'dot ring', error: 'dot off', skipped: 'dot off', pending: 'dot ring', scan: 'dot ring' };

function queueRow(it) {
  const isExt = it.kind === 'ext';
  const title = it.kind === 'school' ? '开学季闭环'
    : isExt ? '每日签到 / 领奖'
    : (GROWTH_TITLES[it.code] || it.code);
  const st = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return h('div', { class: 'qrow', title: it.message || it.note || '' },
    h('span', { class: 'code', text: it.code }),
    h('span', { class: 'name' }, h('span', { class: 't', text: title }),
      it.kind === 'school' ? h('span', { class: 'chip faint', text: '开学季' }) : null,
      isExt ? h('span', { class: 'chip faint', text: platName(String(it.code).split('/')[0]) }) : null),
    h('span', { class: 'prog', text: it.prog || '' }),
    h('span', { class: 'st' }, h('i', { class: DOT_CLS[it.status] || 'dot ring' }), st),
    h('span', { class: 'msg', text: it.message || '' }),
  );
}

function tencentSeg() {
  const q = queue();
  const groups = scannedGroups() || (q && q.running !== undefined && q.items && q.items.length ? groupsFromQueue(q.items) : null);
  const items = q && q.items ? q.items : [];
  const total = items.length;
  const done = items.filter(i => ['done', 'error', 'skipped'].includes(i.status)).length;
  const scannedTotal = (scannedGroups() || []).reduce((s, g) => s + g.rows.length, 0);

  return h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('header', null,
        h('h2', { text: '成长任务队列' }),
        h('span', { class: 'hint', text: '扫描全部账号待办 → 排队执行（账号内串行）' }),
        h('span', { class: 'grow' }),
        h('div', { class: 'seg' },
          ...[1, 2, 3].map(n => h('button', { class: conc() === n ? 'on' : '', text: `并发 ${n}`, onclick: () => conc.set(n) })),
        ),
        h('button', { class: 'btn sm', onclick: () => openVouchers() }, icon('ticket'), '券码'),
        h('button', { class: 'btn sm', onclick: scanAll }, icon('scan'), '扫描待办'),
        h('button', { class: 'btn sm primary', onclick: runQueue }, icon('play'), '执行全部待办'),
      ),
      (q && total > 0) || scannedTotal > 0
        ? h('div', { style: { padding: '0 18px 12px' } },
          h('div', { class: 'row', style: { gap: '12px' } },
            h('div', { class: 'meter', style: { flex: '1' } },
              h('i', { style: { width: (total ? Math.round(done / total * 100) : 0) + '%' } })),
            h('span', { class: 'muted', style: { font: '11.5px var(--mono)' },
              text: total ? `${q.running ? '执行中' : '已结束'} ${done}/${total}` : `${scannedTotal} 项待办` }),
          ))
        : null,
      groups && groups.length
        ? h('div', null, ...groups.map(g => h('div', { class: 'qgroup' },
          h('div', { class: 'head' },
            h('span', { class: 'nm', text: g.nick || g.uid.slice(0, 12) }),
            h('span', { class: 'cnt', text: `${g.rows.length} 项` })),
          ...g.rows.map(queueRow),
        )))
        : h('div', { class: 'body' }, h('div', { class: 'empty' }, icon('automation'),
          h('div', { class: 't', text: '还没有扫描过' }),
          h('div', { class: 'd', text: '扫描腾讯账号的成长任务与开学季待办，以及外部平台的每日签到，把没做的排成一列，一键执行。' }))),
    ),
  );
}

/* ── Loomy ────────────────────────────────────────────────────── */
async function loadLoomy(quiet = true) {
  try {
    const d = await api('loomy/status');
    loomy.set(d);
    loomyMsg.set('');
  } catch (e) {
    loomy.set(null);
    loomyMsg.set(e.message);
    if (!quiet) toast(e.message, 'fail');
  }
}

async function loadCredits(quiet = true) {
  try {
    const d = await api('loomy/credits');
    credits.set(d.has_account ? d : null);
  } catch (e) { credits.set({ error: e.message }); if (!quiet) toast(e.message, 'fail'); }
}

function loomySeg() {
  const d = loomy();
  const st = (d && d.status) || {};
  const c = credits();

  const head = h('header', null,
    h('h2', { text: 'Loomy 新手之旅' }),
    h('span', { class: 'chip', text: '讯飞' }),
    h('span', { class: 'grow' }),
    h('button', { class: 'btn sm', onclick: async () => { await loadCredits(false); } }, icon('wallet'), '余额'),
    h('button', {
      class: 'btn sm', onclick: async ev => {
        var __b = ev.currentTarget; if (__b) __b.disabled = true;
        try { const r = await api('loomy/checkin', { method: 'POST' }); toast((r.checkin || {}).message || '每日额度已激活'); await loadLoomy(); await loadCredits(); }
        catch (e) { toast(e.message, 'fail'); }
        finally { if (__b) __b.disabled = false; }
      },
    }, icon('checkin'), '每日签到'),
    h('button', { class: 'btn sm ghost', onclick: () => { loadLoomy(false); loadCredits(); } }, icon('refresh'), '刷新'),
    h('button', {
      class: 'btn sm primary', onclick: async ev => {
        var __b = ev.currentTarget; if (__b) __b.disabled = true;
        try {
          const r = await api('loomy/complete_all', { method: 'POST' });
          const n = (r.completed || []).length;
          toast(n > 0 ? `新点亮 ${n} 项任务` : '所有任务已是完成态');
          await loadLoomy();
        } catch (e) { toast(e.message, 'fail'); }
        finally { if (__b) __b.disabled = false; }
      },
    }, icon('check'), '一键完成'),
  );

  if (loomyMsg()) {
    return h('section', { class: 'card' }, head, h('div', { class: 'body' },
      h('div', { class: 'empty' }, icon('automation'), h('div', { class: 't', text: '未检测到 Loomy 客户端' }),
        h('div', { class: 'd', text: loomyMsg() }))));
  }
  if (!d) return h('section', { class: 'card' }, head, h('div', { class: 'busy', text: '读取 Loomy 状态' }));
  if (!d.has_account) {
    return h('section', { class: 'card' }, head, h('div', { class: 'body' },
      h('div', { class: 'empty' }, icon('automation'), h('div', { class: 't', text: '尚未接入 Loomy' }),
        h('div', { class: 'd', text: d.message || '可在「添加账号 → Loomy」用手机号或 Token 接入' }))));
  }

  const earned = st.earned || 0, total = st.total || 10000;
  const pct = Math.min(100, Math.round(earned / (total || 1) * 100));

  return h('section', { class: 'card' }, head,
    h('div', { class: 'body stack' },
      h('div', { class: 'row wrap' },
        h('div', { class: 'grow' },
          h('div', { style: { fontSize: '14px', fontWeight: '600' }, text: st.userid ? 'Loomy · ' + st.userid : 'Loomy 客户端' }),
          h('div', { class: 'muted', style: { font: '11.5px var(--mono)', marginTop: '3px' },
            text: `手机 ${st.phone_masked || '已登录'} · 缓存 ${st.user_data_dir || '本地'}` }),
        ),
        h('span', { class: 'chip strong', text: `${earned} / ${total} 积分` }),
      ),
      h('div', { class: 'meter' }, h('i', { style: { width: pct + '%' } })),
      c ? h('div', { class: 'chip', style: { padding: '9px 12px', display: 'flex', gap: '14px', whiteSpace: 'normal' } },
        c.error ? `积分查询失败：${c.error}` : h('span', { text: '' })) : null,
      c && !c.error ? h('div', { class: 'row wrap', style: { gap: '8px' } },
        h('span', { class: 'chip', text: `永久 ${c.credits?.permanent ?? 0}` }),
        h('span', { class: 'chip', text: `每日 ${c.credits?.daily ?? 0}` }),
        h('span', { class: 'chip', text: `合计 ${c.credits?.total ?? 0}` }),
        c.credits?.has_quota ? h('span', { class: 'chip faint', text: `今日额度 ${c.credits.daily_quota}，已用 ${c.credits.daily_consumed}` })
          : h('span', { class: 'chip faint', text: '未签到' }),
      ) : null,
      h('div', { class: 'tile-grid' }, ...(st.items || []).map(it => h('div', { class: 'task-tile' + (it.completed ? ' done' : '') },
        h('div', null,
          h('div', { class: 'c', text: it.category }),
          h('div', { class: 't', text: it.title }),
        ),
        h('div', { style: { textAlign: 'right', flex: 'none' } },
          h('div', { class: 'p', text: `+${it.points}` }),
          h('div', { class: 'c', text: it.completed ? '已完成' : '待完成' }),
        ),
      ))),
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

function schedSeg() {
  const r = report();
  const head = h('header', null,
    h('h2', { text: '任务运行台账' }),
    h('span', { class: 'hint', text: '每一轮排程的真实结论：成功 / 瞬时失败（会自动退避重试）/ 需重新登录（不重试，等重登）' }),
    h('span', { class: 'grow' }),
    r && r.next_fire ? h('span', { class: 'chip faint', text: `下一次 ${hm(r.next_fire)}：${(r.next_kinds || []).join('、')}` }) : null,
    r ? h('span', { class: 'chip', text: `今日已成 ${r.day_done} 项` }) : null,
    h('button', { class: 'btn sm', onclick: () => loadReport(false) }, icon('refresh'), '刷新'));

  if (!r) return h('section', { class: 'card' }, head, h('div', { class: 'body' },
    h('div', { class: 'empty' }, icon('automation'), h('div', { class: 't', text: '暂无台账' }),
      h('div', { class: 'd', text: '调度器还没有派发过任务；也可以在上面点「一键签到 / 旅行 / 活跃」先跑一轮。' }))));

  const pend = r.pending || [];
  const rounds = (r.rounds || []).slice().reverse();
  const body = h('div', { class: 'body stack' },
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
  title: '任务',
  icon: 'automation',
  group: '自动化',
  keywords: '任务 签到 loomy 队列 自动化 排程 台账',
  sub() {
    const q = queue();
    if (seg.peek() === 'tencent' && q && q.running) return '任务队列执行中…';
    if (seg.peek() === 'sched') return '腾讯成长任务 · Loomy 新手之旅 · 排程台账';
    return '腾讯成长任务 · Loomy 新手之旅';
  },
  tick() {
    if (seg.peek() === 'loomy') loadLoomy();
    if (seg.peek() === 'tencent' && queueTimer) pollQueue();
    if (seg.peek() === 'sched') loadReport();
  },
  render() {
    syncSegFromHash();
    const s = seg();
    return h('div', { class: 'view stack' },
      h('div', { class: 'row' },
        h('div', { class: 'seg' },
          h('button', { class: s === 'tencent' ? 'on' : '', text: '腾讯任务', onclick: () => setSeg('tencent') }),
          h('button', { class: s === 'loomy' ? 'on' : '', text: 'Loomy', onclick: () => setSeg('loomy') }),
          h('button', { class: s === 'sched' ? 'on' : '', text: '排程台账', onclick: () => setSeg('sched') }),
        ),
      ),
      s === 'tencent' ? tencentSeg() : s === 'loomy' ? loomySeg() : schedSeg(),
    );
  },
});
