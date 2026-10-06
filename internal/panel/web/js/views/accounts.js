/* ══════════════════════════════════════════════════════════════════
   views/accounts.js · 账号
   垂直平台导航 + 内容区：
     左列 = 平台清单（只显示**有账号**的平台，外加固定两项：
            账号池 [腾讯] 与 全部平台 [目录]），每行带账号数徽标；
     右侧 = 所选平台的账号管理界面。
   状态一律单色三档（见 status.js）。hash 子状态（#accounts?tab=xxx）可深链。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, ago, fmtToken, fmtMs, fmtRate, confirmDialog } from '../kernel.js';
import { defineView, parseHash, setHashSeg } from '../shell.js';
import { overview, refreshOverview } from '../store.js';
import { openAccountDrawer } from '../drawers.js';
import { loadPlatforms, platforms, platName } from '../platforms.js';
import { statusOf, statusChip } from '../status.js';
import { zaiSegment } from './zai-segment.js';
import { extSegment } from './ext-segment.js';

const TABS = ['pool', 'zai', 'ext', 'dir'];
const TAB_LABELS = { pool: '账号池', zai: 'Z.AI', ext: '外部平台', dir: '全部平台' };
const tab = signal('pool');
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

/* ── 左列：垂直平台导航 ─────────────────────────────────────────
   只列**有账号**的平台；腾讯恒在（账号池），目录恒在（跨平台总览）。
   每行：平台名 + 右侧账号数徽标；当前项高亮。 */
function railList(dirData, cur, focus) {

  // 从目录数据统计各分段账号数；目录没加载出来时不阻塞导航
  const countOf = seg => {
    if (seg === 'pool') {
      const d = overview();
      return d ? (d.accounts || []).length : null;
    }
    if (seg === 'zai') {
      const g = dirData && (dirData.platforms || []).find(p => p.id === 'zai');
      const n = g ? (g.accounts || []).length : 0;
      return n > 0 ? n : null;
    }
    if (seg === 'ext') {
      const plats = (dirData && dirData.platforms) || [];
      let n = 0;
      for (const p of plats) {
        if (p.id === 'workbuddy' || p.id === 'zai' || p.id === 'codex' || p.id === 'free') continue;
        n += (p.accounts || []).length;
      }
      return n > 0 ? n : null;
    }
    return null;
  };

  const item = (seg, label, hint) => {
    const n = countOf(seg);
    const on = cur === seg;
    return h('button', {
      class: 'prail-item' + (on ? ' on' : ''),
      onclick: () => setTab(seg),
      title: hint || label,
    },
      h('span', { class: 'nm', text: label }),
      n != null ? h('span', { class: 'cnt', text: String(n) }) : null,
    );
  };

  const list = h('div', { class: 'prail' },
    item('pool', '账号池', '腾讯 WorkBuddy 对话账号'),
  );

  // 有账号的外部平台逐个平铺（不用「外部平台」聚合层）——zai 有专属界面，单独一项
  const plats = (dirData && dirData.platforms) || [];
  const zaiG = plats.find(p => p.id === 'zai');
  if (zaiG && (zaiG.accounts || []).length) list.append(item('zai', 'Z.AI', 'Z.AI / 智谱账号池'));

  const extPlats = plats.filter(p => {
    if (p.id === 'workbuddy' || p.id === 'zai') return false;
    if (p.login === 'none' || p.login === 'config') return false; // codex / free 走配置页
    return (p.accounts || []).length > 0;
  });
  for (const p of extPlats) list.append(itemPlatform(p, cur, focus));

  list.append(item('dir', '全部平台', '跨平台只读目录'));
  return list;
}

// 平台行：点击进入外部平台分段并预选该平台（extSegment 按 focus 高亮）
function itemPlatform(p, cur, focus) {
  const on = cur === 'ext' && focus === p.id;
  return h('button', {
    class: 'prail-item' + (on ? ' on' : ''),
    onclick: () => { extFocus.set(p.id); setTab('ext'); },
    title: p.note || p.name,
  },
    h('span', { class: 'nm', text: platName(p.id) }),
    h('span', { class: 'cnt', text: String((p.accounts || []).length) }),
  );
}

const extFocus = signal('');

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
  const plats = (d.platforms || []).filter(p => (p.accounts || []).length > 0);   // 只显示有账号的
  if (!plats.length) return h('div', { class: 'empty' }, h('div', { class: 'd', text: '还没有任何账号' }));
  const segOf = { workbuddy: 'pool', zai: 'zai' };
  return h('div', { class: 'stack' },
    ...plats.map(p => {
      const accts = p.accounts || [];
      return h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: platName(p.id) }),
          p.api_model ? h('span', { class: 'chip', text: 'API ' + p.api_model }) : h('span', { class: 'chip faint', text: '无对话 API' }),
          h('span', { class: 'grow' }),
          h('span', { class: 'hint', text: `${accts.length} 个账号` }),
          h('button', {
            class: 'btn sm ghost',
            onclick: () => {
              if (p.id in segOf) { setTab(segOf[p.id]); return; }
              if (p.login === 'none' || p.login === 'config') { /* codex/free 等在配置页 */ }
              else { extFocus.set(p.id); setTab('ext'); }
            },
          }, '管理 →'),
        ),
        h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
          h('thead', null, h('tr', null, h('th', { text: '账号' }), h('th', { text: '状态' }), h('th', { text: '额度' }), h('th', { text: '备注' }))),
          h('tbody', null, ...accts.map(a => h('tr', null,
            h('td', null, h('div', { style: { fontWeight: '550' }, text: a.label || a.id }), h('div', { class: 'muted', style: { font: '10.5px var(--mono)' }, text: a.id })),
            h('td', null, statusChip(statusOf(a))),
            h('td', { class: 'num', text: a.quota || '—' }),
            h('td', { class: 'muted', text: a.detail || '' }),
          ))),
        )),
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
    if (t === 'dir') return '全部平台目录';
    if (String(t).startsWith('ext')) return platName(String(t).slice(4));
    if (t !== 'pool') return TAB_LABELS[t] || '';
    const d = overview();
    if (!d) return '正在连接…';
    return `${d.total} 个账号 · ${d.healthy} 可用 · ${d.cooling} 冷却 · ${d.disabled} 禁用`;
  },
  tick() {
    refreshOverview();
    if (tab.peek() === 'dir') loadDir();
  },
  render() {
    syncTabFromHash();
    // 目录数据懒加载：左列需要它来列平台；没加载出来时左列先只有「账号池」
    if (dir.peek() === null) loadDir();
    // 响应式读取：tab / extFocus / dir 任一变化都会重渲染本视图
    const t = tab();
    const focus = extFocus();
    const d = dir();

    let body;
    if (t === 'pool') body = poolView();
    else if (t === 'zai') body = zaiSegment();
    else if (t === 'dir') body = dirView();
    else body = extSegment(focus);

    return h('div', { class: 'view acct-layout' },
      railList(d, t, focus),
      h('div', { class: 'acct-content stack' }, body),
    );
  },
});
