/* ══════════════════════════════════════════════════════════════════
   views/overview.js · 总览
   一眼看清：池健康 / 积分 / 平台分布 / 最近动作，以及全部批量操作入口。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, ago } from '../kernel.js';
import { defineView, navigate } from '../shell.js';
import { overview, refreshOverview } from '../store.js';
import { statusOf } from '../status.js';

const dir = signal(null);
const recent = signal(null);
let recentFp = '';   // 最近动态指纹：内容未变则不写信号（避免无谓重渲染）

async function loadDir(quiet = true) {
  try { dir.set(await api('accounts/dir')); }
  catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

async function loadRecent(quiet = true) {
  try {
    // 只取尾部若干条：总览展示 8 行，无需每轮拉满环形缓冲
    const d = await api('logs?tail=16');
    const next = (d.entries || []).slice(-14).reverse();
    const fp = next.map(e => e.ts + e.text).join('|');
    if (fp === recentFp) return;      // 内容未变 → 不触发重渲染
    recentFp = fp;
    recent.set(next);
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

async function batch(path, label) {
  try {
    await api(path, { method: 'POST' });
    // 结果落在运行日志：给一条直达入口，而不是让用户自己去找
    toast(label + '已开始', undefined, { action: { label: '查看日志', onclick: () => navigate('logs') } });
  } catch (e) { toast(e.message, 'fail'); }
}

function tile(value, key, quiet) {
  // 长值（如 "2656/4250"）自动缩小字号，避免省略号截断关键信息
  const long = String(value).length > 7;
  return h('div', { class: 'tile' + (quiet ? ' quiet' : '') },
    h('div', { class: 'v' + (long ? ' long' : ''), text: value }),
    h('div', { class: 'k', text: key }),
  );
}

function statusStrip() {
  const d = overview();
  const credits = acctsCredit(d);
  const totals = d ? (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0) : 0;
  return h('div', { class: 'row wrap', style: { gap: '7px', fontSize: '12px', color: 'var(--fg-2)' } },
    h('span', { class: 'chip faint' }, '账号 ', h('b', { text: d ? `${d.healthy}/${d.total}` : '—' })),
    h('span', { class: 'chip faint' }, '积分 ', h('b', { text: totals > 0 ? `${credits}/${totals}` : String(credits) })),
    overview()?.sticky_sessions != null ? h('span', { class: 'chip faint' }, '粘性会话 ', h('b', { text: String(overview().sticky_sessions) })) : null,
    d ? h('span', { class: 'chip faint mono', text: `v${d.version}` }) : null,
    d ? h('span', { class: 'chip faint mono', text: `运行 ${Math.floor((d.uptime_sec || 0) / 3600)} 时` }) : null,
  );
}

function acctsCredit(d) {
  return d ? (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0) : 0;
}

function healthRow(accts) {
  const total = accts.length;
  const healthy = accts.filter(a => !a.disabled && !cooling(a)).length;
  const coolingN = accts.filter(a => !a.disabled && cooling(a)).length;
  const off = accts.filter(a => a.disabled).length;
  return h('div', { class: 'tiles' },
    tile(String(total), '账号总数'),
    tile(String(healthy), '可用'),
    tile(String(coolingN), '冷却中', coolingN > 0),
    tile(String(off), '已禁用', off > 0),
    tile(creditsLabel(accts), '积分 剩余/总额'),
    tile(String(overview()?.sticky_sessions ?? 0), '粘性会话'),
  );
}

function cooling(a) {
  return statusOf(a).level !== 'ok';
}

function creditsLabel(accts) {
  const rem = accts.reduce((s, a) => s + (a.credits || 0), 0);
  const tot = accts.reduce((s, a) => s + (a.credits_total || 0), 0);
  return tot > 0 ? `${rem}/${tot}` : String(rem);
}

function platformList() {
  const d = dir();
  if (!d) return h('div', { class: 'busy', text: '读取平台' });
  const plats = (d.platforms || []).filter(p => (p.accounts || []).length);
  if (!plats.length) return h('div', { class: 'empty' }, h('div', { class: 't', text: '还没有账号' }), h('div', { class: 'd', text: '右上角「添加账号」开始' }));
  return h('div', { class: 'stack', style: { gap: '8px' } },
    ...plats.map(p => {
      const n = p.accounts.length;
      const okN = p.accounts.filter(a => a.status === 'healthy').length;
      return h('div', { class: 'row', style: { padding: '9px 12px', border: '1px solid var(--edge)', borderRadius: 'var(--r-sm)', background: 'var(--glass)' } },
        h('span', { class: 'dot' + (okN ? '' : ' ring') }),
        h('span', { text: p.name || p.id, style: { fontSize: '13px', fontWeight: '550' } }),
        h('span', { class: 'grow' }),
        h('span', { class: 'chip faint', text: `${okN}/${n} 可用` }),
      );
    }),
  );
}

function recentFeed() {
  const list = recent();
  if (!list) return h('div', { class: 'busy', text: '读取日志' });
  if (!list.length) return h('div', { class: 'empty' }, h('div', { class: 'd', text: '暂无日志' }));
  const CH = { task: '任务', chat: '对话', sys: '系统' };
  return h('div', { style: { display: 'grid', gap: '7px' } },
    ...list.slice(0, 8).map(e => h('div', { class: 'row', style: { alignItems: 'flex-start', gap: '9px' } },
      h('span', { class: 'chip faint', text: CH[e.ch] || e.ch, style: { flex: 'none' } }),
      h('span', {
        text: e.text,
        style: {
          fontSize: '12.5px', color: 'var(--fg-2)', overflow: 'hidden',
          textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        },
      }),
    )),
  );
}

export default defineView({
  id: 'overview',
  page: 'home',
  title: '首页',
  icon: 'overview',
  keywords: 'dashboard 首页 概览 待办',
  sub() {
    const d = overview();
    if (!d) return '正在连接…';
    return `账号 ${d.healthy}/${d.total} 可用 · 运行 ${Math.floor((d.uptime_sec || 0) / 3600)} 时`;
  },
  tick() { refreshOverview(); loadDir(); loadRecent(); },
  render() {
    if (dir.peek() === null) loadDir();
    if (recent.peek() === null) loadRecent();
    const d = overview();
    const accts = d ? (d.accounts || []) : [];

    return h('div', { class: 'view stack' },
      statusStrip(),   // 清单 48：状态栏信息手机上也看得见（桌面状态栏之外的一处补充）
      d ? healthRow(accts) : h('div', { class: 'tiles' }, tile('—', '账号总数'), tile('—', '可用'), tile('—', '冷却中'), tile('—', '已禁用'), tile('—', '积分'), tile('—', '粘性会话')),

      h('section', { class: 'card' },
        h('header', null, h('h2', { text: '批量操作' }), h('span', { class: 'grow' }), h('span', { class: 'hint', text: '对所有账号执行，结果写入运行日志' })),
        h('div', { class: 'body row wrap' },
          h('button', { class: 'btn', onclick: () => batch('checkin_all', '全部签到') }, icon('checkin'), '全部签到'),
          h('button', { class: 'btn', onclick: () => batch('keepalive_all', '全部保活') }, icon('power'), '全部保活'),
          h('button', { class: 'btn', onclick: () => batch('travel_all', '旅行巡检') }, icon('ticket'), '旅行巡检'),
          h('button', { class: 'btn', onclick: () => batch('activity_all', '活跃上报') }, icon('scan'), '活跃上报'),
          h('button', {
            class: 'btn', onclick: async (ev) => {
              var __b = ev.currentTarget; if (__b) __b.disabled = true;
              try { await api('balance_all', { method: 'POST' }); await refreshOverview(); toast('余额已刷新'); }
              catch (e) { toast(e.message, 'fail'); }
              finally { if (__b) __b.disabled = false; }
            },
          }, icon('refresh'), '刷新余额'),
        ),
      ),

      h('div', { style: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: '14px' } },
        h('section', { class: 'card' },
          h('header', null, h('h2', { text: '平台分布' }), h('span', { class: 'grow' }),
            h('button', { class: 'btn sm ghost', onclick: () => navigate('accounts') }, '管理 →')),
          h('div', { class: 'body' }, platformList()),
        ),
        h('section', { class: 'card' },
          h('header', null, h('h2', { text: '最近动态' }), h('span', { class: 'grow' }),
            h('button', { class: 'btn sm ghost', onclick: () => navigate('logs') }, '全部日志 →')),
          h('div', { class: 'body' }, recentFeed()),
        ),
      ),
    );
  },
});

export { loadDir, loadRecent };
