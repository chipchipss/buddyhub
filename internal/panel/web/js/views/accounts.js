/* ══════════════════════════════════════════════════════════════════
   views/accounts.js · 账号
   两个视角：账号池（可运维的卡片）/ 全部平台目录（跨平台只读总览）。
   状态一律单色表达：实心点=可用 · 空心环=冷却 · 虚线环=停用。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, ago, dur, fmtToken, fmtMs, fmtRate, confirmDialog } from '../kernel.js';
import { defineView, navigate } from '../shell.js';
import { overview, refreshOverview } from '../store.js';
import { openAccountDrawer } from '../drawers.js';

const tab = signal('pool');          // pool | dir
const dir = signal(null);
const dirPlat = signal('all');

async function loadDir(quiet = true) {
  try { dir.set(await api('accounts/dir')); }
  catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

function coolSec(a) {
  const bl = (new Date(a.breaker_until || 0) - Date.now()) / 1000;
  const dg = (new Date(a.degrade_until || 0) - Date.now()) / 1000;
  return Math.max(a.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
}

function statusOf(a) {
  if (a.disabled) return { cls: 'dot off', label: '已禁用', note: a.reason || '' };
  const cool = coolSec(a);
  if (cool > 0) {
    const bl = (new Date(a.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(a.degrade_until || 0) - Date.now()) / 1000;
    const kind = bl > Math.max(a.cool_remaining_sec || 0, dg) ? '熔断'
      : dg > (a.cool_remaining_sec || 0) ? '降权'
      : a.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却';
    return { cls: 'dot ring', label: `${kind} · ${dur(cool)}`, note: a.reason || '' };
  }
  return { cls: 'dot', label: '可用', note: '' };
}

async function act(uid, path, body, okMsg) {
  try {
    const r = await api(`accounts/${encodeURIComponent(uid)}/${path}`, {
      method: 'POST', body: body ? JSON.stringify(body) : undefined,
    });
    toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
    await refreshOverview();
    return r;
  } catch (e) {
    toast(e.message, 'fail');
    return null;
  }
}

function accountCard(a) {
  const st = statusOf(a);
  const tu = a.token_usage || {};
  const total = a.credits_total || 0;
  const pct = total > 0 ? Math.min(100, Math.round((a.credits || 0) / total * 100))
    : Math.round((a.credits || 0) / Math.max(1, ...(overview()?.accounts || []).map(s => s.credits || 0)) * 100);
  const frozen = a.disabled || coolSec(a) > 0;

  const btn = (label, onclick, cls = '') => h('button', {
    class: 'btn ' + cls,
    onclick: async ev => { var __b = ev.currentTarget; if (__b) __b.disabled = true; await onclick(); if (__b) __b.disabled = false; },
  }, label);

  return h('article', { class: 'acct' + (a.disabled ? ' off' : '') },
    h('div', { class: 'top' },
      h('div', { class: 'who' },
        h('div', { class: 'nm', text: a.nickname || '未命名' }),
        h('div', { class: 'id', text: a.uid }),
      ),
      h('span', { class: 'chip' + (a.disabled ? ' faint' : st.label === '可用' ? ' strong' : ''), title: st.note },
        h('i', { class: st.cls }), st.label),
    ),

    h('div', { class: 'credits' },
      h('div', { class: 'line' },
        h('span', { class: 'n', text: a.credits == null ? '—' : String(a.credits) }),
        total > 0 ? h('span', { class: 'of', text: `/ ${total} · ${pct}%` }) : null,
      ),
      h('div', { class: 'meter' }, h('i', { style: { width: pct + '%' } })),
    ),

    h('div', { class: 'metrics' },
      h('div', { class: 'metric' }, h('div', { class: 'v', text: `${a.success_count || 0}/${a.err_total || 0}` }), h('div', { class: 'k', text: '成功/失败' })),
      h('div', { class: 'metric' }, h('div', { class: 'v', text: fmtToken(tu.total_tokens) }), h('div', { class: 'k', text: '上次 token' })),
      h('div', { class: 'metric' }, h('div', { class: 'v', text: ago(a.last_success) }), h('div', { class: 'k', text: '最近成功' })),
    ),

    h('div', { class: 'row', style: { marginTop: '10px', gap: '10px' } },
      h('span', { class: 'chip faint', text: `${tu.request_count || 0} 次` }),
      h('span', { class: 'chip faint', text: fmtMs(tu.last_latency_ms) }),
      h('span', { class: 'chip faint', text: fmtRate(tu.last_tokens_per_second) }),
      a.in_flight ? h('span', { class: 'chip', text: `在途 ${a.in_flight}` }) : null,
    ),

    h('div', { class: 'acts' },
      btn('签到', () => act(a.uid, 'checkin', null, r => '签到完成' + (r.credits != null ? `，积分 ${r.credits}` : ''))),
      btn('余额', () => act(a.uid, 'balance', null, r => `余额已更新：${r.credits}`)),
      btn('任务', async () => { openAccountDrawer(a.uid, a.nickname); }),
      frozen
        ? btn('解冻', () => act(a.uid, 'revive', null, '已解冻'), 'primary')
        : btn('禁用', async () => {
          if (await confirmDialog('禁用后该账号不再参与选号，需手动解冻才能恢复。', { ok: '禁用' })) {
            await act(a.uid, 'disable', null, '已禁用');
          }
        }),
      btn('移除', async () => {
        if (await confirmDialog('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。', { ok: '移除' })) {
          await act(a.uid, 'remove', null, r => r.file_error ? '已移除（凭证文件删除失败）' : '已移除');
        }
      }, 'danger'),
    ),
  );
}

function poolView() {
  const d = overview();
  if (!d) return h('div', { class: 'busy', text: '读取账号池' });
  const accts = d.accounts || [];
  if (!accts.length) {
    return h('section', { class: 'card' }, h('div', { class: 'body' },
      h('div', { class: 'empty' }, icon('accounts'),
        h('div', { class: 't', text: '账号池是空的' }),
        h('div', { class: 'd', text: '点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号' }))));
  }
  return h('div', { class: 'acct-grid' }, ...accts.map(accountCard));
}

const PLAT_NAMES = {
  workbuddy: '腾讯 WorkBuddy', loomy: 'Loomy（讯飞）', qoder: 'Qoder',
  lobsterai: 'LobsterAI', raccoon: '小浣熊', codearts: 'CodeArts',
  copilot: 'GitHub Copilot', cline: 'Cline', autoclaw: 'AutoClaw（智谱）',
  zai: 'Z.AI 智谱', codex: 'Codex', free: '免费池',
};

function dirView() {
  const d = dir();
  if (!d) return h('div', { class: 'busy', text: '读取平台目录' });
  const plats = d.platforms || [];
  const shown = dirPlat() === 'all' ? plats : plats.filter(p => p.id === dirPlat());
  return h('div', { class: 'stack' },
    h('div', { class: 'row wrap' },
      ...[{ id: 'all', name: '全部', n: plats.reduce((s, p) => s + (p.accounts || []).length, 0) }]
        .concat(plats.map(p => ({ id: p.id, name: PLAT_NAMES[p.id] || p.name, n: (p.accounts || []).length })))
        .map(c => h('button', {
          class: 'btn sm' + (dirPlat() === c.id ? ' primary' : ''),
          onclick: () => { dirPlat.set(c.id); },
        }, `${c.name} ${c.n}`)),
    ),
    ...shown.map(p => {
      const accts = p.accounts || [];
      return h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: PLAT_NAMES[p.id] || p.name }),
          p.api_model ? h('span', { class: 'chip', text: 'API ' + p.api_model }) : h('span', { class: 'chip faint', text: '无对话 API' }),
          h('span', { class: 'grow' }),
          h('span', { class: 'hint', text: `${accts.length} 个账号` }),
          h('button', { class: 'btn sm ghost', onclick: () => navigate(p.manage_to?.replace('#', '') || 'accounts') }, '平台管理 →'),
        ),
        accts.length
          ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
            h('thead', null, h('tr', null, h('th', { text: '账号' }), h('th', { text: '状态' }), h('th', { text: '额度' }), h('th', { text: '备注' }))),
            h('tbody', null, ...accts.map(a => h('tr', null,
              h('td', null, h('div', { style: { fontWeight: '550' }, text: a.label || a.id }), h('div', { class: 'muted', style: { font: '10.5px var(--mono)' }, text: a.id })),
              h('td', null, h('span', { class: 'chip' + (a.status === 'healthy' ? ' strong' : ' faint') },
                h('i', { class: a.status === 'healthy' ? 'dot' : a.status === 'cooling' ? 'dot ring' : 'dot off' }),
                a.status === 'healthy' ? '正常' : a.status === 'cooling' ? '冷却' : '停用')),
              h('td', { class: 'num', text: a.quota || '—' }),
              h('td', { class: 'muted', text: a.detail || '' }),
            ))),
          ))
          : h('div', { class: 'body' }, h('div', { class: 'empty' }, h('div', { class: 'd', text: '该平台暂无账号' }))),
      );
    }),
  );
}

export default defineView({
  id: 'accounts',
  title: '账号',
  icon: 'accounts',
  group: '账号',
  keywords: '账号 池 目录 平台',
  sub() {
    const d = overview();
    if (!d) return '正在连接…';
    return `${d.total} 个账号 · ${d.healthy} 可用 · ${d.cooling} 冷却 · ${d.disabled} 禁用`;
  },
  tick() { refreshOverview(); if (tab.peek() === 'dir') loadDir(); },
  render() {
    const isPool = tab() === 'pool';
    return h('div', { class: 'view stack' },
      h('div', { class: 'row' },
        h('div', { class: 'seg' },
          h('button', { class: isPool ? 'on' : '', text: '账号池', onclick: () => tab.set('pool') }),
          h('button', { class: isPool ? '' : 'on', text: '全部平台', onclick: () => { tab.set('dir'); loadDir(); } }),
        ),
        h('span', { class: 'grow' }),
        h('button', { class: 'btn sm ghost', onclick: () => { refreshOverview(); loadDir(); } }, icon('refresh'), '刷新'),
      ),
      isPool ? poolView() : dirView(),
    );
  },
});
