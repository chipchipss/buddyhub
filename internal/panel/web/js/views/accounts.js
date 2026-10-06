/* ══════════════════════════════════════════════════════════════════
   views/accounts.js · 账号
   所有平台账号的**唯一管理地**：
     账号池   —— 腾讯 WorkBuddy 对话真账号（可运维卡片）
     Z.AI     —— Z.AI 账号池（Plan JWT / API Key）
     外部平台 —— extstore 账号（扫码/授权/逐字段入池）
     全部平台 —— 跨平台只读目录（点击进对应管理分段）
   hash 子状态（#accounts?tab=pool 等）可深链、可刷新保持。
   状态一律单色三档：实心点=可用 · 空心环=冷却 · 虚线/灰点=停用（见 status.js）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, ago, fmtToken, fmtMs, fmtRate, confirmDialog } from '../kernel.js';
import { defineView, navigate, parseHash, setHashSeg } from '../shell.js';
import { overview, refreshOverview } from '../store.js';
import { openAccountDrawer } from '../drawers.js';
import { loadPlatforms, platName } from '../platforms.js';
import { statusOf, statusChip } from '../status.js';
import { zaiSegment } from './zai-segment.js';
import { extSegment } from './ext-segment.js';

const TABS = ['pool', 'zai', 'ext', 'dir'];
const tab = signal('pool');          // pool | zai | ext | dir
const dir = signal(null);
const dirPlat = signal('all');

// hash 子状态：#accounts?tab=ext —— 深链/刷新/后退都落在同一段
export function syncTabFromHash() {
  const seg = parseHash().seg || 'pool';
  if (TABS.includes(seg)) tab.set(seg);
}
function setTab(v) {
  tab.set(v);
  setHashSeg('accounts', v);
  if (v === 'dir') loadDir();
}

async function loadDir(quiet = true) {
  try {
    await loadPlatforms();
    dir.set(await api('accounts/dir'));
  }
  catch (e) { if (!quiet) toast(e.message, 'fail'); }
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
  const frozen = a.disabled || st.level !== 'ok';

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
      statusChip(st),
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

function dirView() {
  const d = dir();
  if (!d) return h('div', { class: 'busy', text: '读取平台目录' });
  const plats = d.platforms || [];
  const shown = dirPlat() === 'all' ? plats : plats.filter(p => p.id === dirPlat());
  return h('div', { class: 'stack' },
    h('div', { class: 'row wrap' },
      ...[{ id: 'all', name: '全部', n: plats.reduce((s, p) => s + (p.accounts || []).length, 0) }]
        .concat(plats.map(p => ({ id: p.id, name: platName(p.id), n: (p.accounts || []).length })))
        .map(c => h('button', {
          class: 'btn sm' + (dirPlat() === c.id ? ' primary' : ''),
          onclick: () => { dirPlat.set(c.id); },
        }, `${c.name} ${c.n}`)),
    ),
    ...shown.map(p => {
      const accts = p.accounts || [];
      return h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: platName(p.id) }),
          p.api_model ? h('span', { class: 'chip', text: 'API ' + p.api_model }) : h('span', { class: 'chip faint', text: '无对话 API' }),
          h('span', { class: 'grow' }),
          h('span', { class: 'hint', text: `${accts.length} 个账号` }),
          h('button', {
            class: 'btn sm ghost',
            // 后端下发 manage_to（#accounts / #accounts?seg=zai|ext / #config）；
            // 同视图跳转时按平台落到真实分段，异视图直接导航。
            onclick: () => {
              const to = p.manage_to || '#accounts';
              if (to.startsWith('#accounts')) {
                const seg = (to.split('?seg=')[1]) || 'pool';
                setTab(TABS.includes(seg) ? seg : 'pool');
              } else {
                navigate(to.slice(1));
              }
            },
          }, '管理 →'),
        ),
        accts.length
          ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
            h('thead', null, h('tr', null, h('th', { text: '账号' }), h('th', { text: '状态' }), h('th', { text: '额度' }), h('th', { text: '备注' }))),
            h('tbody', null, ...accts.map(a => h('tr', null,
              h('td', null, h('div', { style: { fontWeight: '550' }, text: a.label || a.id }), h('div', { class: 'muted', style: { font: '10.5px var(--mono)' }, text: a.id })),
              h('td', null, statusChip(statusOf(a))),
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
  keywords: '账号 池 目录 平台 zai 外部',
  sub() {
    const t = tab.peek();
    if (t === 'zai') return 'Z.AI 账号池';
    if (t === 'ext') return '外部平台账号';
    if (t === 'dir') return '全部平台目录';
    const d = overview();
    if (!d) return '正在连接…';
    return `${d.total} 个账号 · ${d.healthy} 可用 · ${d.cooling} 冷却 · ${d.disabled} 禁用`;
  },
  tick() {
    const t = tab.peek();
    if (t === 'pool' || t === 'dir') refreshOverview();
    if (t === 'dir') loadDir();
    if (t === 'ext') { /* extSegment 自带 tick 数据源（ext 订阅在视图内轮询） */ }
  },
  render() {
    syncTabFromHash();
    const t = tab();
    const segBtn = (v, label) => h('button', { class: t === v ? 'on' : '', text: label, onclick: () => setTab(v) });
    return h('div', { class: 'view stack' },
      h('div', { class: 'row' },
        h('div', { class: 'seg' },
          segBtn('pool', '账号池'),
          segBtn('zai', 'Z.AI'),
          segBtn('ext', '外部平台'),
          segBtn('dir', '全部平台'),
        ),
        h('span', { class: 'grow' }),
        h('button', { class: 'btn sm ghost', onclick: () => { refreshOverview(); loadDir(); } }, icon('refresh'), '刷新'),
      ),
      t === 'pool' ? poolView()
        : t === 'zai' ? zaiSegment()
          : t === 'ext' ? extSegment()
            : dirView(),
    );
  },
});
