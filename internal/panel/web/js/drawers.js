/* ══════════════════════════════════════════════════════════════════
   drawers.js · 通用抽屉件
   活动券码抽屉（含离线二维码）。

   「添加账号」不在这里了 —— 它是四步向导，有自己的状态机与进度，
   见 addwizard.js（清单 20：两个入口合一）。
   账号详情与成长任务在 views/account-detail.js（那是「一个账号」的界面，
   不是通用抽屉件，放在一起才不会两边措辞漂移）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, api, toast, openDrawer, closeDrawer, copyText } from './kernel.js';
import { qrMatrix, qrSVG } from './qr.js';

export function openVouchers() {
  const body = h('div', { class: 'stack' }, h('div', { class: 'busy', text: '查询券码' }));
  openDrawer({
    title: '活动券码',
    hint: '抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），到对应 App 或小程序兑换',
    body,
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('button', { class: 'btn sm', onclick: () => loadVouchers(body) }, icon('refresh'), '刷新'),
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', text: '关闭', onclick: closeDrawer }),
    ),
  });
  loadVouchers(body);
}

async function loadVouchers(host) {
  try {
    const d = await api('school/vouchers');
    const accounts = d.accounts || [];
    const ok = accounts.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    const nodes = [];
    for (const a of ok) {
      const vs = a.vouchers || [];
      if (!vs.length) continue;
      nodes.push(h('div', { class: 'plat-head' },
        h('span', { class: 'nm', text: a.nickname || a.uid }), h('span', { class: 'line' }), h('span', { text: `${vs.length} 张` })));
      nodes.push(...vs.map(voucherCard));
    }
    if (!nodes.length) nodes.push(h('div', { class: 'empty' }, icon('ticket'), h('div', { class: 't', text: '还没有抽到券' })));
    const failed = accounts.filter(a => a.error);
    if (failed.length) {
      nodes.push(h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '8px' },
        text: '查询失败：' + failed.map(a => `${a.nickname || a.uid.slice(0, 8)}（${a.error}）`).join('、') }));
    }
    if (total) nodes.unshift(h('div', { class: 'muted', style: { fontSize: '12px' }, text: `共 ${total} 张可用券` }));
    host.replaceChildren(...nodes);
  } catch (e) {
    host.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'd', text: e.message })));
  }
}

function voucherCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  const qrHost = h('div');
  return h('div', { class: 'voucher' + (expired ? ' expired' : '') },
    h('div', { class: 'hd' },
      h('span', { class: 'nm', text: v.prize_name || v.sku_code || '券' }),
      h('span', { class: 'chip ' + (expired ? 'faint' : 'strong'), text: expired ? '已过期' : '可使用' }),
    ),
    h('div', { class: 'meta', text: (v.valid_to ? `有效期至 ${v.valid_to}` : '长期有效') + (v.granted_at ? ` · ${String(v.granted_at).slice(0, 10)} 抽中` : '') }),
    h('div', { class: 'sep' }),
    h('div', { class: 'ft' },
      h('span', { class: 'lab', text: '券码' }),
      h('code', { text: v.code || '-' }),
      h('div', { class: 'acts' },
        v.code ? h('button', {
          class: 'btn sm', onclick: () => {
            if (qrHost.firstChild) { qrHost.replaceChildren(); return; }
            try { qrHost.replaceChildren(h('div', { class: 'qr' }, qrSVG(qrMatrix(v.code), 148))); }
            catch (e) { qrHost.replaceChildren(h('div', { class: 'muted', style: { padding: '10px' }, text: '二维码生成失败：' + e.message })); }
          },
        }, icon('qr'), '二维码') : null,
        h('button', {
          class: 'btn sm', onclick: async () => {
            try { await copyText(v.code || ''); toast('券码已复制'); } catch { toast('复制失败，请手动选择', 'fail'); }
          },
        }, icon('copy'), '复制'),
      ),
    ),
    qrHost,
  );
}
