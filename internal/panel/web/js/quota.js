/* ══════════════════════════════════════════════════════════════════
   quota.js · 配额弹窗（阶段 5 收尾）
   数据源 GET /panel/api/quota（后端 QuotaSource 注入 CreditLedger 快照）。
   fork 对应物是 9router 的 quota modal：流式展示各账号余额——buddyhub 的
   快照是同步接口，一次拉全量即可，无需流式；支持一键移除分配的能力
   在后端账本（DELETE 分配）还没开面板端点，弹窗先只读展示。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, api, toast, openDrawer, closeDrawer } from './kernel.js';

export function openQuotaModal() {
  const body = h('div', { class: 'stack' }, h('div', { class: 'busy', text: '读取账号配额' }));
  openDrawer({
    title: 'Qoder 账号配额',
    hint: '积分账本快照：已分配 / 已用 / 剩余（-1 = 无上限）。无分配记录的账号不在此列。',
    body,
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('button', { class: 'btn sm', onclick: () => loadQuota(body) }, icon('refresh'), '刷新'),
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', text: '关闭', onclick: closeDrawer }),
    ),
  });
  loadQuota(body);
}

async function loadQuota(host) {
  try {
    const d = await api('quota');
    if (!d.ok) throw new Error(d.error || '读取失败');
    if (!d.available) {
      host.replaceChildren(h('div', { class: 'empty' },
        h('div', { class: 'd', text: '额度账本未启用（无 Qoder 账号或网关未开启记账）。' })));
      return;
    }
    const rows = d.quotas || [];
    if (!rows.length) {
      host.replaceChildren(h('div', { class: 'empty' },
        h('div', { class: 'd', text: '暂无分配记录。给 Qoder 账号设置额度上限后出现在这里。' })));
      return;
    }
    const nodes = [];
    const canRemove = !!d.can_remove;
    nodes.push(h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
      h('thead', null, h('tr', null,
        h('th', { text: '账号' }),
        h('th', { class: 'num', text: '已分配' }),
        h('th', { class: 'num', text: '已用' }),
        h('th', { class: 'num', text: '剩余' }),
        h('th', { class: 'num', text: '口径' }),
        ...(canRemove ? [h('th', { class: 'num', text: '操作' })] : []))),
      h('tbody', null, ...rows.map(r => {
        const unlimited = Number(r.remaining) < 0;
        const pct = (!unlimited && r.allocated > 0)
          ? Math.min(100, Math.round(r.used / r.allocated * 100)) : null;
        return h('tr', null,
          h('td', null, h('span', { text: r.account_key || '—' })),
          h('td', { class: 'num', text: r.allocated > 0 ? fmt(r.allocated) : '未分配' }),
          h('td', { class: 'num', text: fmt(r.used) }),
          h('td', { class: 'num' + (r.exhausted ? ' bad' : '') },
            h('span', { text: unlimited ? '无上限' : fmt(r.remaining),
              style: r.exhausted ? { color: 'var(--danger, #d33)', fontWeight: '600' } : null }),
            pct !== null ? h('div', { class: 'meter', style: { height: '5px', marginTop: '4px', borderRadius: '3px' } },
              h('i', { style: { width: pct + '%', background: pct >= 90 ? 'var(--danger, #d33)' : 'var(--fg-2, #888)', borderRadius: '3px', display: 'block', height: '100%' } })) : null),
          h('td', { class: 'num muted', text: r.precise ? '精确' : '估算' }),
          ...(canRemove ? [h('td', { class: 'num' }, r.allocated > 0 ? h('button', {
            class: 'btn sm ghost danger', title: '移除分配上限：该账号回到无上限（记账继续）',
            onclick: async () => {
              try {
                await api('quota/remove', { method: 'POST', body: { account_key: r.account_key } });
                toast('已移除 ' + r.account_key + ' 的分配');
                loadQuota(host);
              } catch (e) { toast(e.message, 'fail'); }
            },
          }, '移除') : h('span', { class: 'muted', text: '—' }))] : []),
        );
      })),
    )));
    const exhausted = rows.filter(r => r.exhausted).length;
    if (exhausted) {
      nodes.push(h('div', { class: 'muted', style: { fontSize: '12px' },
        text: `${exhausted} 个账号已达分配上限（请求会被额度门拒绝，直到调高分配或记账回退）。` }));
    }
    host.replaceChildren(...nodes);
  } catch (e) {
    host.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'd', text: e.message })));
  }
}

function fmt(n) {
  const v = Number(n) || 0;
  return Number.isInteger(v) ? String(v) : v.toFixed(2);
}
