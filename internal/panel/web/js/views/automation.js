/* ══════════════════════════════════════════════════════════════════
   views/automation.js · 自动化
   三段工作台：腾讯成长任务 / Loomy 新手之旅 / 外部平台签到
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog } from '../kernel.js';
import { defineView } from '../shell.js';
import { refreshOverview } from '../store.js';
import { openVouchers } from '../drawers.js';
import { zaiSegment, loadZai } from './zai-segment.js';
import { extAddPanel } from './ext-add.js';

const seg = signal('tencent');
const queue = signal(null);
const queueSeq = signal(0);
const conc = signal(1);
const loomy = signal(null);
const loomyMsg = signal('');
const credits = signal(null);
const ext = signal(null);
const extMsg = signal('');

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
  const title = it.kind === 'school' ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const st = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return h('div', { class: 'qrow', title: it.message || '' },
    h('span', { class: 'code', text: it.code }),
    h('span', { class: 'name' }, h('span', { class: 't', text: title }),
      it.kind === 'school' ? h('span', { class: 'chip faint', text: '开学季' }) : null),
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
          h('div', { class: 'd', text: '扫描所有账号的成长任务与开学季待办，把没做的排成一列，一键执行。' }))),
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

/* ── 外部平台 ─────────────────────────────────────────────────── */
const EXT_NAMES = { lobsterai: 'LobsterAI', raccoon: '小浣熊', qoder: 'Qoder', codearts: '华为云', copilot: 'GitHub Copilot' };

// Copilot 是网关直连通道（无积分、无签到），卡片按「订阅状态」而非「余额」呈现。
const EXT_NO_CHECKIN = { copilot: true };

async function loadExt(quiet = true) {
  try { ext.set(await api('ext/accounts')); extMsg.set(''); }
  catch (e) { extMsg.set(e.message); if (!quiet) toast(e.message, 'fail'); }
}

async function extAct(provider, id, action, body) {
  try {
    const r = await api(`ext/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}/${action}`, {
      method: 'POST', body: body ? JSON.stringify(body) : undefined,
    });
    if (action === 'checkin') {
      const res = r.result || {};
      toast(`[${EXT_NAMES[provider] || provider}] ${res.message || res.kind || '完成'}`, res.kind === 'failed' ? 'fail' : undefined);
    } else if (action === 'remove') toast('已删除');
    await loadExt();
  } catch (e) { toast(e.message, 'fail'); }
}

function extSeg() {
  const d = ext();
  const list = (d && d.accounts) || [];
  return h('section', { class: 'card' },
    h('header', null,
      h('h2', { text: '外部平台签到' }),
      h('span', { class: 'hint', text: '每日 10:00 自动执行' }),
      h('span', { class: 'grow' }),
      h('span', { class: 'hint', text: d ? `${list.length} 个账号 · ${list.filter(a => a.balance_ok).length} 个余额正常` : '' }),
      h('button', { class: 'btn sm ghost', onclick: () => loadExt(false) }, icon('refresh'), '刷新'),
      h('button', {
        class: 'btn sm primary', onclick: async ev => {
          var __b = ev.currentTarget; if (__b) __b.disabled = true;
          try {
            const r = await api('ext/checkin_all', { method: 'POST' });
            const rs = r.results || [];
            const ok = rs.filter(x => x.kind === 'claimed').length;
            const already = rs.filter(x => x.kind === 'already-claimed').length;
            const failed = rs.filter(x => x.kind === 'failed').length;
            toast(`签到完成：成功 ${ok} · 已领过 ${already} · 失败 ${failed}`, failed ? 'fail' : undefined);
            await loadExt();
          } catch (e) { toast(e.message, 'fail'); }
          finally { if (__b) __b.disabled = false; }
        },
      }, icon('check'), '一键签到全部'),
    ),
    h('div', { class: 'body stack' },
      extMsg() ? h('div', { class: 'empty' }, h('div', { class: 'd', text: extMsg() }))
        : list.length
          ? h('div', { class: 'acct-grid' }, ...list.map(a => h('article', { class: 'acct' + (a.disabled ? ' off' : '') },
            h('div', { class: 'top' },
              h('div', { class: 'who' },
                h('div', { class: 'nm', text: `${EXT_NAMES[a.provider] || a.provider} · ${a.label || a.id}` }),
                h('div', { class: 'id', text: a.id }),
              ),
              h('span', { class: 'chip' + (a.disabled ? ' faint' : '') }, h('i', { class: a.disabled ? 'dot off' : 'dot' }), a.disabled ? '已停用' : '启用中'),
            ),
            h('div', { class: 'credits' },
              h('div', { class: 'line' },
                a.provider === 'copilot'
                  ? h('span', { class: 'of', style: { fontSize: '12px' }, text: a.note || '订阅状态未知' })
                  : h('span', { class: 'n', text: a.balance_ok ? String(a.balance ?? 0) : '—' }),
                a.provider === 'copilot' ? null
                  : h('span', { class: 'of', text: a.balance_ok ? '分' : (a.note || '') }),
              ),
            ),
            h('div', { class: 'acts' },
              EXT_NO_CHECKIN[a.provider] ? null
                : h('button', { class: 'btn', disabled: a.disabled, onclick: () => extAct(a.provider, a.id, 'checkin') }, '签到'),
              h('button', {
                class: 'btn', onclick: () => extAct(a.provider, a.id, 'toggle', { disabled: !a.disabled }),
              }, a.disabled ? '启用' : '停用'),
              h('button', {
                class: 'btn danger', onclick: async () => {
                  if (await confirmDialog(`确定删除 ${EXT_NAMES[a.provider] || a.provider} 账号 ${a.id}？`, { ok: '删除' })) {
                    await extAct(a.provider, a.id, 'remove');
                  }
                },
              }, '删除'),
            ),
          )))
          : h('div', { class: 'empty' }, icon('accounts'), h('div', { class: 't', text: '还没有外部账号' }),
            h('div', { class: 'd', text: '可用「添加账号」弹层，或在下方手工粘贴凭据' })),

      // 逐字段添加（每个平台按自己的凭据形态出表单）
      extAddPanel(loadExt),

      h('details', { style: { marginTop: '6px' } },
        h('summary', { class: 'muted', style: { cursor: 'pointer', fontSize: '12.5px' }, text: '高级：粘贴完整凭据 JSON' }),
        h('div', { class: 'row wrap', style: { marginTop: '10px' } },
          h('select', { class: 'input', id: 'ext-provider', style: { width: 'auto' } },
            ...Object.entries(EXT_NAMES).map(([v, n]) => h('option', { value: v, text: n }))),
          h('input', { class: 'input', id: 'ext-id', placeholder: '账号 ID', style: { flex: '1', minWidth: '140px' } }),
          h('input', { class: 'input', id: 'ext-cred', placeholder: '凭据 JSON', style: { flex: '2', minWidth: '200px', fontFamily: 'var(--mono)', fontSize: '11.5px' } }),
          h('button', {
            class: 'btn sm primary', onclick: async () => {
              const provider = document.getElementById('ext-provider').value;
              const id = document.getElementById('ext-id').value.trim();
              const raw = document.getElementById('ext-cred').value.trim();
              if (!id) { toast('请填写账号 ID', 'fail'); return; }
              let cred;
              try { cred = JSON.parse(raw); } catch { toast('凭据不是合法 JSON', 'fail'); return; }
              try {
                await api('ext/accounts', { method: 'POST', body: JSON.stringify({ provider, id, cred }) });
                toast('账号已添加');
                document.getElementById('ext-id').value = '';
                document.getElementById('ext-cred').value = '';
                await loadExt();
              } catch (e) { toast(e.message, 'fail'); }
            },
          }, '添加'),
        ),
      ),
    ),
  );
}

export default defineView({
  id: 'automation',
  title: '自动化',
  icon: 'automation',
  group: '自动化',
  keywords: '任务 签到 loomy 外部 队列 自动化',
  sub() {
    const q = queue();
    if (seg.peek() === 'tencent' && q && q.running) return '任务队列执行中…';
    return '腾讯成长任务 · Loomy · 外部平台';
  },
  tick() {
    if (seg.peek() === 'loomy') loadLoomy();
    if (seg.peek() === 'ext') loadExt();
    if (seg.peek() === 'zai') loadZai();
    if (seg.peek() === 'tencent' && queueTimer) pollQueue();
  },
  render() {
    const s = seg();
    return h('div', { class: 'view stack' },
      h('div', { class: 'row' },
        h('div', { class: 'seg' },
          h('button', { class: s === 'tencent' ? 'on' : '', text: '腾讯任务', onclick: () => seg.set('tencent') }),
          h('button', { class: s === 'loomy' ? 'on' : '', text: 'Loomy', onclick: () => { seg.set('loomy'); loadLoomy(); loadCredits(); } }),
          h('button', { class: s === 'ext' ? 'on' : '', text: '外部平台', onclick: () => { seg.set('ext'); loadExt(); } }),
          h('button', { class: s === 'zai' ? 'on' : '', text: 'Z.AI', onclick: () => { seg.set('zai'); loadZai(); } }),
        ),
      ),
      s === 'tencent' ? tencentSeg() : s === 'loomy' ? loomySeg() : s === 'zai' ? zaiSegment() : extSeg(),
    );
  },
});
