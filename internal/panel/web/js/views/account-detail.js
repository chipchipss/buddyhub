/* ══════════════════════════════════════════════════════════════════
   views/account-detail.js · 账号详情抽屉
   列表行只留「名称 / 状态 / 余额」，其余全在这里：完整指标、近 7 天曲线、
   这个账号自己的日志、凭证状态、全部操作、任务子页（清单 II.5–II.7）。

   抽屉内部是**命令式重绘**（host.replaceChildren），不挂 effect：
   抽屉开着时账号页的 5s tick 会重渲染列表，但抽屉只在自己那一次动作后
   才重画——否则用户正在看的指标表会被整体换掉、滚动位置丢失。
   ══════════════════════════════════════════════════════════════════ */

import {
  h, icon, signal, api, toast, openDrawer, closeDrawer, confirmDialog,
  dur, ago, fmtToken, fmtMs, fmtRate,
} from '../kernel.js';
import { run, inverseOf } from '../acts.js';
import { begin, report, finish } from '../jobs.js';
import { buildRows, logMatcher, coolText } from '../rows.js';
import { statusOf, statusChip } from '../status.js';
import { navigate, openAddAccount } from '../shell.js';
import { overview, refreshOverview } from '../store.js';
import { zaiData } from './zai-segment.js';
import { extData } from './ext-segment.js';

const tab = signal('info');        // info | tasks | logs
const curve = signal(null);        // 近 7 天日粒度用量（仅腾讯池有逐请求记录）
const logLines = signal(null);

let row = null;
let host = null;
let footEl = null;
let busy = '';                     // 正在执行的动作 kind（按钮三态：转圈→禁用）
// 成功后的那一秒：doneFlash 存点击时的动作排布，doneKind 标出该打勾的那一个（清单 15）。
// 必须冻结排布：「启用/停用」成功后整排按钮会换一套，✓ 落到别的格子上就是骗人。
let doneFlash = null;
let doneKind = '';
let doneTimer = 0;

/** openAccountDetail(r) —— 打开某个账号的详情抽屉。 */
export function openAccountDetail(r) {
  if (!r) return;
  row = r;
  tab.set('info');
  curve.set(null);
  logLines.set(null);
  // 任务台账是按账号查的，不清就会把上一个账号的表当成这个账号的画出来
  tasks.set(null);
  taskErr.set('');
  busy = '';
  doneFlash = null;
  doneKind = '';
  clearTimeout(doneTimer);
  host = h('div', { class: 'stack' });
  footEl = h('div', { class: 'row wrap detail-foot' });
  openDrawer({
    title: r.name,
    hint: r.platformLabel + ' · ' + (r.sub || r.id),
    body: host,
    footer: footEl,
  });
  paint();
  loadCurve();
}

/** allActions —— 抽屉里的全部操作 = 行尾主按钮 + ⋯ 菜单项（同一份模型，措辞一致）。 */
function allActions() {
  const out = [];
  if (row.primary) out.push(row.primary);
  out.push(...row.menu);
  return out;
}

function paint() {
  if (!host || !host.isConnected) return;
  const t = tab.peek();
  if (t === 'tasks') host.replaceChildren(tasksPane());
  else if (t === 'logs') host.replaceChildren(logsPane());
  else host.replaceChildren(infoPane());
  paintFoot();
}

function paintFoot() {
  if (!footEl) return;
  // 打勾那一秒：排布冻结在点击时的样子（见 doneFlash），否则「停用」成功后
  // 动作表整个换成「启用/查余额/移除」，✓ 会落到别的格子上，等于骗人。
  const acts = doneFlash || allActions();
  const done = a => !!doneFlash && a.kind === doneKind;
  footEl.replaceChildren(
    h('div', { class: 'row', style: { gap: '4px' } }, ...tabs()),
    h('span', { class: 'grow' }),
    h('div', { class: 'row wrap', style: { gap: '5px' } },
      // 一屏一个主按钮（清单 49）：只有行模型认定的那个主行动点亮，其余平铺，
      // 「移除/删除」永远排在最后并标成危险态。
      ...acts.map((a, i) => h('button', {
        class: 'btn sm' + (a.danger ? ' danger' : (i === 0 ? ' primary' : ''))
          + (busy === a.kind ? ' spin' : done(a) ? ' ok' : ''),
        text: busy === a.kind ? '执行中…' : done(a) ? '完成' : a.label,
        title: a.tip || a.confirm || '',
        disabled: !!busy || !!doneFlash,
        onclick: () => doAction(a),
      }))),
  );
}

function tabs() {
  const mk = (v, label) => h('button', {
    class: 'btn sm' + (tab.peek() === v ? ' on' : ''),
    text: label,
    onclick: () => setTab(v),
  });
  return [mk('info', '概览'), row.can.tasks ? mk('tasks', '任务') : null, mk('logs', '日志')].filter(Boolean);
}

function setTab(v) {
  tab.set(v);
  if (v === 'logs' && logLines() === null) loadLogs();
  paint();
}

/** doAction —— 一次动作：按钮转圈（禁用）→ 就地更新数据 → 打勾 1 秒。
 *  结果同时记进右下角活动栏（换页、关掉抽屉都还在，可撤销的旁边带「撤销」）。
 *  账号被删除后抽屉没有意义，直接收起。 */
async function doAction(a) {
  if (a.kind === 'relogin') { openAddAccount(row.provider); return; }
  // 清单 16：可撤销的（停用/启用）直接生效，不再弹确认框；只有移除凭证这种
  // 不可逆的才确认，确认文案里点名账号（a.confirm 由 rows.js 统一措辞）。
  if (a.danger && a.confirm) {
    if (!await confirmDialog(a.confirm, { ok: a.label })) return;
  }
  busy = a.kind;
  paintFoot();
  const job = begin(`${a.label} · ${row.name}`, 1);
  const r = await run(a.kind, row);
  const inv = inverseOf(a.kind);
  report(job, {
    name: row.name, ok: r.ok, msg: r.msg,
    undo: r.ok && inv ? async () => {
      const cur = freshRow(row) || row;
      const res = await run(inv, cur);
      row = freshRow(row) || row;
      paint();
      if (!res.ok) toast('撤销失败：' + res.msg, 'fail');
      return res;
    } : null,
  });
  finish(job);
  busy = '';
  if (!r.ok) toast(`${a.label}失败：${r.msg}`, 'fail');
  else {
    doneFlash = allActions();
    doneKind = a.kind;
    clearTimeout(doneTimer);
    doneTimer = setTimeout(() => { doneFlash = null; doneKind = ''; paintFoot(); }, 1000);
  }
  const fresh = freshRow(row);
  if (!fresh) { closeDrawer(); return; }
  row = fresh;
  if (a.kind === 'checkin' || a.kind === 'balance') loadCurve();
  paint();
}

/** freshRow —— 动作后从最新数据里重建这一行（旧对象里的积分/状态已经过期）。 */
function freshRow(prev) {
  const rows = buildRows(overview()?.accounts, zaiData()?.accounts, extData()?.accounts);
  return rows.find(x => x.key === prev.key) || null;
}

/* ── 数据加载 ───────────────────────────────────────────────────
   抽屉不挂 effect（见文件头），所以「数据到了」必须自己喊重画：
   每个 loader 落地后都要 paint()，否则子页永远停在「读取中…」。 */
async function loadCurve() {
  if (row.provider !== 'workbuddy') { curve.set({ unsupported: true, points: [] }); paint(); return; }
  try {
    curve.set(await api('usage/series?uid=' + encodeURIComponent(row.id) + '&days=7'));
  } catch (e) {
    curve.set({ error: e.message, points: [] });
  }
  paint();
}

async function loadLogs() {
  try {
    const d = await api('logs');
    const hit = logMatcher(row);
    logLines.set((d.entries || []).filter(e => hit(String(e.text || ''))).slice(-160));
  } catch (e) {
    logLines.set(null);
    toast(e.message, 'fail');
  }
  paint();
}

/* ── 概览 ─────────────────────────────────────────────────────── */
function infoPane() {
  const a = row.raw;
  const st = statusOf(a);
  const kids = [statusLine(st)];
  if (row.provider === 'workbuddy') kids.push(poolPane(a), curvePane(), costPane(a));
  else if (row.provider === 'zai') kids.push(zaiPane(a));
  else kids.push(extPane(a));
  kids.push(credPane(st));
  return h('div', { class: 'stack' }, ...kids.filter(Boolean));
}

/** statusLine —— 状态自带下一步（清单 II.9）：不只说「冷却中」，还说还剩多久、能做什么。 */
function statusLine(st) {
  const left = coolText(row);
  let next = '';
  if (st.level === 'off') {
    next = st.kind === 'invalid'
      ? '凭据已失效：自动续期救不了它，只有重新授权才能恢复。'
      : '已停用，不再参与选号。点「启用」立刻恢复。';
  } else if (st.level === 'cool') {
    next = `恢复前不参与选号${left ? `，还剩 ${left}` : ''}。等到点自动恢复，或点「强制恢复」立刻放行。`;
  }
  return h('div', { class: 'detail-status' },
    h('div', { class: 'row', style: { gap: '8px' } },
      statusChip(st),
      h('span', { class: 'grow' }),
      row.can.tasks ? h('button', { class: 'btn sm ghost', onclick: () => setTab('tasks') }, '任务 →') : null),
    next ? h('div', { class: 'hint', style: { marginTop: '7px' }, text: next }) : null,
  );
}

/** metric —— 标签在上、数值在下（清单 II.10），数值一律带单位。 */
function metric(k, v, tip) {
  return h('div', { class: 'metric' },
    h('div', { class: 'k', text: k }),
    h('div', { class: 'v', title: tip || '' }, String(v)),
  );
}

function poolPane(a) {
  const tu = a.token_usage || {};
  const total = a.credits_total || 0;
  return h('div', { class: 'card flat' },
    h('div', { class: 'metrics-4' },
      metric('积分', a.credits == null ? '—' : `${a.credits} 分`, total > 0 ? `总额度 ${total} 分` : ''),
      metric('成功 / 失败', `${a.success_count || 0} / ${a.err_total || 0}`),
      metric('连续失败', a.consecutive_fails || 0, '达到阈值后该账号会被暂时降权'),
      metric('在途请求', a.in_flight || 0),
      metric('最近成功', ago(a.last_success)),
      metric('上次延迟', fmtMs(tu.last_latency_ms)),
      metric('上次吐字速率', fmtRate(tu.last_tokens_per_second)),
      metric('上次 token', tu.total_tokens ? `${fmtToken(tu.total_tokens)} tokens` : '—'),
      metric('累计请求', tu.request_count || 0),
      metric('域', a.realm === 'global' ? '国际版' : '国内版'),
    ),
    total > 0
      ? h('div', { class: 'stack', style: { gap: '5px', marginTop: '12px' } },
        h('div', { class: 'row', style: { justifyContent: 'space-between', fontSize: '11.5px' } },
          h('span', { class: 'muted', text: '积分余量' }),
          h('span', { class: 'mono', text: `${a.credits} / ${total}` })),
        h('div', { class: 'meter' }, h('i', { style: { width: Math.min(100, Math.round((a.credits || 0) / total * 100)) + '%' } })))
      : null,
    (a.rate_limited_models || []).length
      ? h('div', { class: 'stack', style: { gap: '4px', marginTop: '12px' } },
        h('div', { class: 'label', text: '仍在限额中的模型' }),
        ...a.rate_limited_models.map(m => h('div', { class: 'row', style: { fontSize: '12px' } },
          h('span', { class: 'mono', text: m.model || String(m) }),
          h('span', { class: 'grow' }),
          h('span', { class: 'muted', text: m.until ? '预计 ' + new Date(m.until).toLocaleString('zh-CN', { hour12: false }) + ' 恢复' : '恢复中' }))))
      : null,
    (a.disabled_reason || a.reason)
      ? h('div', { class: 'hint', style: { marginTop: '10px' }, text: a.disabled_reason || a.reason }) : null,
  );
}

/** curvePane —— 近 7 天日粒度用量。逐请求用量只有腾讯池在记，别的通道明说没有。 */
function curvePane() {
  const d = curve();
  const box = h('div', { class: 'card flat' }, h('div', { class: 'label', text: '近 7 天用量' }));
  if (!d) { box.append(h('div', { class: 'busy', text: '读取用量' })); return box; }
  if (d.unsupported) {
    box.append(h('div', { class: 'muted', style: { fontSize: '12px' }, text: '该通道没有逐请求用量记录，只有账号级计数。' }));
    return box;
  }
  if (d.error) {
    box.append(h('div', { class: 'muted', style: { fontSize: '12px' }, text: '用量读取失败：' + d.error }));
    return box;
  }
  const pts = d.points || [];
  const max = Math.max(1, ...pts.map(p => Number(p.total_tokens || 0)));
  box.append(h('div', { class: 'spark' },
    ...pts.map(p => {
      const tt = Number(p.total_tokens || 0);
      return h('div', { class: 'sp-col', title: `${p.t} · ${p.requests || 0} 次 · ${fmtToken(tt)} tokens` },
        h('div', { class: 'sp-bar', style: { height: (tt > 0 ? Math.max(3, Math.round(tt / max * 78)) : 2) + 'px', opacity: tt ? '1' : '.22' } }),
        h('div', { class: 'sp-k', text: String(p.t).slice(5) }),
      );
    })),
  );
  box.append(h('div', { class: 'hint', text: `最高一天 ${fmtToken(max)} tokens · 矮柱表示那天没有请求` }));
  return box;
}

/** costPane —— 每模型实测成本（回答「为什么总选它」）。 */
function costPane(a) {
  const list = a.model_costs || [];
  if (!list.length) return null;
  return h('div', { class: 'card flat' },
    h('div', { class: 'label', text: '每模型实测成本' }),
    h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
      h('thead', null, h('tr', null,
        h('th', { text: '模型' }), h('th', { class: 'num', text: '千字单价' }),
        h('th', { class: 'num', text: '样本' }), h('th', { class: 'num', text: '最近观测' }))),
      h('tbody', null, ...list.map(m => h('tr', null,
        h('td', { class: 'mono', text: m.model }),
        h('td', { class: 'num', text: (m.cost_per_1k || 0) > 0 ? String(m.cost_per_1k) : '免费' }),
        h('td', { class: 'num', text: String(m.samples || 0) }),
        h('td', { class: 'num', text: ago(m.last_seen) }),
      ))),
    )),
  );
}

const PENALTY_LABEL = { server: '上游抖动', rate: '限流', exhausted: '额度用完' };

function zaiPane(a) {
  const q = a.quota || {}, mh = a.model_health || {}, fp = a.fingerprint || {};
  const models = Array.from(new Set([...Object.keys(q), ...Object.keys(mh)]));
  const box = h('div', { class: 'card flat' },
    h('div', { class: 'metrics-4' },
      metric('调用', a.use_count || 0),
      metric('失败', a.fail_count || 0),
      metric('在途请求', a.in_flight || 0),
      metric('风控命中', a.risk_strikes || 0, '上游风控拦截次数'),
      metric('冷却剩余', a.cool_remaining_sec ? dur(a.cool_remaining_sec) : '—'),
    ));
  if (models.length) {
    box.append(h('div', { class: 'stack', style: { gap: '9px', marginTop: '12px' } },
      ...models.map(m => {
        const w = q[m], pen = mh[m];
        const head = h('div', { class: 'row', style: { justifyContent: 'space-between', fontSize: '12px' } },
          h('span', null,
            m,
            pen ? (() => {
              const left = pen.until ? Math.max(0, Math.round((new Date(pen.until).getTime() - Date.now()) / 1000)) : 0;
              return h('span', { class: 'chip', title: (pen.last_err || '') + (pen.fails ? `（连续 ${pen.fails} 次）` : ''), text: (PENALTY_LABEL[pen.kind] || '冷却') + (left > 0 ? ` ${dur(left)}` : '') });
            })() : null),
          w ? h('span', { class: 'muted mono' }, `${fmtToken(w.remaining)} / ${fmtToken(w.total)}${w.expires_at ? ' · ' + String(w.expires_at).slice(0, 10) : ''}`) : null,
        );
        const meter = w && Number(w.total) > 0
          ? h('div', { class: 'meter thin' }, h('i', { style: { width: Math.min(100, Math.round(w.remaining / w.total * 100)) + '%' } }))
          : null;
        return h('div', null, head, meter);
      })));
  } else {
    box.append(h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '10px' },
      text: a.mode === 'jwt' ? '尚未查询额度，点「查额度」拉一次。' : 'API Key 通道没有额度窗口。' }));
  }
  box.append(h('div', { class: 'hint', style: { marginTop: '10px' },
    text: `设备指纹：${fp.platform || '?'} ${fp.arch || ''} · 系统 ${fp.os_version || '?'}` }));
  return box;
}

function extPane(a) {
  return h('div', { class: 'card flat' },
    h('div', { class: 'metrics-4' },
      metric('余额', a.balance_ok ? `${a.balance} 分` : '—'),
      metric('连续失败', a.fail_streak || 0),
      metric('冷却剩余', a.cooldown_sec > 0 ? dur(a.cooldown_sec) : '—'),
    ),
    a.note ? h('div', { class: 'hint', style: { marginTop: '9px' }, text: a.note }) : null,
  );
}

/** credPane —— 凭证状态与续期方式：回答「我登陆了，为什么后来不能用」。
 *  这里只显示**续期口径与失效原因**，永不展示凭据值本身。 */
function credPane(st) {
  const err = st.tip || row.raw.last_error || row.raw.last_err || '';
  return h('div', { class: 'card flat' },
    h('div', { class: 'label', text: '凭证' }),
    h('div', { style: { fontSize: '12.5px' }, text: row.cred }),
    err ? h('div', { class: 'hint', style: { marginTop: '8px' }, text: '最近一次错误：' + err }) : null,
    row.provider === 'workbuddy'
      ? h('div', { style: { marginTop: '8px' } },
        h('button', { class: 'btn sm ghost', onclick: () => navigate('credits') }, '看它的积分包构成 →'))
      : null,
  );
}

/* ── 日志子页 ─────────────────────────────────────────────────── */
function logsPane() {
  const box = h('div', { class: 'card flat' },
    h('div', { class: 'row', style: { gap: '8px' } },
      h('div', { class: 'label', text: '与这个账号有关的日志' }),
      h('span', { class: 'grow' }),
      h('button', {
        class: 'btn sm ghost', onclick: () => { logLines.set(null); loadLogs(); paint(); },
      }, icon('refresh'), '刷新')));
  if (logLines() === null) { box.append(h('div', { class: 'busy', text: '读取日志' })); return box; }
  const list = logLines();
  if (!list.length) {
    box.append(h('div', { class: 'muted', style: { fontSize: '12px' },
      text: '日志缓冲区里还没有这个账号的记录（环形缓冲只留最近 500 条）。' }));
    return box;
  }
  box.append(h('div', { class: 'logbox', style: { marginTop: '10px' } },
    ...list.map(e => h('span', { class: 'ln' + (/error|失败|错误/.test(e.text) ? ' strong' : '') },
      `${e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : ''} ${e.text}`))));
  return box;
}

/* ── 任务子页（仅腾讯池）───────────────────────────────────────
   成长任务台账：接受 / 领取 / 一键完成。命令式局部刷新——loadTasks 只换
   .task-host，抽屉的头部、按钮、其它子页都不动。 */
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报',
  'Buddy_App': '上报「进入 Buddy 应用」事件链',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链',
  'automation_1': '上报「定时任务创建」事件',
  'Library_read': '上报「读资料库介绍」事件',
  'template_5': '上报「使用模板创建任务」事件组 ×5',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组',
  'create_canvas': '上报「设计创意画布创建」事件组',
  'expert_5': '真实专家召唤+使用链 ×5',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链',
  'skill_1': '真实对话 + skill_info 技能加载事件',
  'school_season': '校园日（小程序口径）',
  'Sequential_Tasks_1': '小程序首对话',
  'Sequential_Tasks_2': '小程序选专家对话',
  'Sequential_Tasks_3': '小程序五次对话',
  'Sequential_Tasks_4': '小程序定时任务',
  'Sequential_Tasks_5': '小程序使用 GLM5.2',
  'Sequential_Tasks_6': '小程序十次对话',
  'Sequential_Tasks_7': '体验灵感功能',
};

const tasks = signal(null);
const taskErr = signal('');

async function loadTasks(quiet = true) {
  if (!row) return;
  try {
    const d = await api(`accounts/${encodeURIComponent(row.id)}/tasks`);
    tasks.set((d.tasks || []).slice().sort((x, y) =>
      (x.claimed - y.claimed) || (y.claimable - x.claimable) || String(x.task_code).localeCompare(String(y.task_code))));
    taskErr.set('');
  } catch (e) {
    taskErr.set(e.message);
    tasks.set([]);
    if (!quiet) toast(e.message, 'fail');
  }
  const hostEl = document.querySelector('.drawer .task-host');
  if (hostEl) hostEl.replaceChildren(taskTable());
}

function tasksPane() {
  if (tasks() === null && !taskErr()) loadTasks();
  return h('div', { class: 'card flat' },
    h('div', { class: 'row wrap', style: { gap: '6px' } },
      h('div', { class: 'label', text: '成长任务' }),
      h('span', { class: 'grow' }),
      h('button', {
        class: 'btn sm', onclick: async ev => {
          ev.currentTarget.disabled = true;
          try {
            const r = await api(`accounts/${encodeURIComponent(row.id)}/tasks/accept_all`, { method: 'POST' });
            const n = r.accepted || 0;
            toast(r.failed && r.failed.length
              ? `已接受 ${n} 个，${r.failed.length} 个被上游拒绝`
              : (n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受')));
            await loadTasks();
          } catch (e) { toast(e.message, 'fail'); }
          finally { ev.currentTarget.disabled = false; }
        },
      }, '全部接受'),
      h('button', { class: 'btn sm ghost', onclick: () => loadTasks(false) }, '重新查询'),
      h('button', {
        class: 'btn sm', onclick: async ev => {
          if (!await confirmDialog('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。过程约 1-2 分钟（含真实对话）。', { ok: '开始执行' })) return;
          ev.currentTarget.disabled = true;
          try {
            const r = await api(`accounts/${encodeURIComponent(row.id)}/tasks/auto_all`, { method: 'POST' });
            const done = (r.results || []).filter(x => x.status === 'done').length;
            const skip = (r.results || []).filter(x => x.status === 'skipped').length;
            const bad = (r.results || []).filter(x => x.status === 'error').length;
            toast(`执行完成：成功 ${done}，跳过 ${skip}${bad ? `，失败 ${bad}` : ''}`, bad ? 'fail' : undefined);
            await loadTasks();
            await refreshOverview();
          } catch (e) { toast(e.message, 'fail'); }
          finally { ev.currentTarget.disabled = false; }
        },
      }, icon('play'), '一键完成可自动任务')),
    h('div', { class: 'task-host', style: { marginTop: '12px' } }, taskTable()));
}

function taskTable() {
  if (taskErr()) return h('div', { class: 'empty' }, h('div', { class: 'd', text: taskErr() }));
  const list = tasks();
  if (list === null) return h('div', { class: 'busy', text: '查询任务进度' });
  if (!list.length) return h('div', { class: 'empty' }, h('div', { class: 'd', text: '该账号暂无任务' }));

  return h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
    h('thead', null, h('tr', null,
      h('th', { text: '任务' }), h('th', { class: 'num', text: '进度' }), h('th', { class: 'num', text: '奖励' }),
      h('th', { text: '状态' }), h('th', { class: 'acts', text: '' }))),
    h('tbody', null, ...list.map(t => {
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? `${cur} / ${tgt}` : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push(`+${t.credit} 分`);
      if (t.energy) parts.push(`+${t.energy} 能`);
      if (t.reward_buddy) parts.push('Buddy');

      const state = t.claimed ? ['已领取', 'faint'] : t.claimable ? ['可领取', 'strong']
        : t.locked ? ['未解锁', 'faint'] : t.accept_status === 'accepted' ? ['进行中', ''] : ['未接受', 'faint'];

      const uid = row.id;
      const btn = (label, cls, go, title) => h('button', {
        class: 'btn sm ' + cls, text: label, title: title || '',
        onclick: async ev => {
          ev.currentTarget.disabled = true;
          try { await go(ev.currentTarget); } finally { ev.currentTarget.disabled = false; }
        },
      });

      const action = t.claimed || t.locked ? null
        : t.claimable ? btn('领取', 'primary', async () => {
          try {
            await api(`accounts/${encodeURIComponent(uid)}/tasks/claim`, { method: 'POST', body: JSON.stringify({ task_code: t.task_code }) });
            toast('已领取奖励');
            await loadTasks();
          } catch (e) { toast(e.message, 'fail'); }
        })
          : AUTO_TASKS[t.task_code] ? btn('一键完成', 'primary', async el => {
            el.textContent = '执行中…';
            try {
              const r = await api(`accounts/${encodeURIComponent(uid)}/tasks/auto`, { method: 'POST', body: JSON.stringify({ task_code: t.task_code }) });
              if (r.skipped) { toast(r.message || '已跳过'); } else {
                const advanced = r.progress_before !== r.progress_after;
                let msg = r.message || '已执行';
                if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
                if (r.claimed) msg += '，奖励已到账';
                else if (r.attempt && !advanced) msg += '；进度未动，可能需要官方客户端';
                toast(msg, (r.claimed || advanced) ? undefined : 'fail');
              }
              await loadTasks();
            } catch (e) { toast(e.message, 'fail'); }
            finally { el.textContent = '一键完成'; }
          }, AUTO_TASKS[t.task_code])
            : t.accept_status === 'accepted' ? null
              : btn('接受', '', async () => {
                try {
                  await api(`accounts/${encodeURIComponent(uid)}/tasks/accept`, { method: 'POST', body: JSON.stringify({ task_codes: [t.task_code] }) });
                  toast('已接受任务');
                  await loadTasks();
                } catch (e) { toast(e.message, 'fail'); }
              });

      return h('tr', { title: [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n') },
        h('td', null,
          h('div', { style: { fontWeight: '550' }, text: t.title || t.task_code }),
          h('div', { class: 'muted', style: { font: '10.5px var(--mono)' }, text: t.task_code + (t.tag ? ' · ' + t.tag : '') })),
        h('td', { class: 'num', text: prog }),
        h('td', { class: 'num', text: parts.length ? parts.join(' ') : '—' }),
        h('td', null, h('span', { class: 'chip ' + state[1], text: state[0] })),
        h('td', { class: 'acts' }, action),
      );
    })),
  ));
}
