/* ══════════════════════════════════════════════════════════════════
   views/models.js · 模型与档位
   按平台分组；倍率列展示牌价 vs 生效价；最大输出列标注实测钳制。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, fmtK } from '../kernel.js';
import { defineView } from '../shell.js';

const models = signal(null);
const probes = signal({});
const err = signal('');

function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}

function outCell(m, pr) {
  if (!pr) return h('td', { class: 'num', text: m.max_output_tokens ? fmtK(m.max_output_tokens) : '—' });
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ` · ${days} 天前` : '';
  const tip = `声称 ${pr.claimed ? fmtK(pr.claimed) : '?'} · 实测 ${pr.measured ? fmtK(pr.measured) : '?'}${pr.note ? ' · ' + pr.note : ''}${pr.tested_at ? ' · 探测于 ' + pr.tested_at : ''}`;

  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return h('td', { class: 'num', title: tip },
        h('div', { style: { fontWeight: '600' }, text: `${fmtK(pr.measured)} ⚠` }),
        h('div', { class: 'muted', style: { fontSize: '11px' }, text: `钳制 ${xs}${stale}` }));
    }
    return h('td', { class: 'num', title: tip, text: fmtK(pr.measured) + (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') });
  }
  if (pr.verdict === 'at_least' && pr.measured) return h('td', { class: 'num', title: tip, text: '≥' + fmtK(pr.measured) });
  return h('td', { class: 'num', title: tip }, h('div', { text: '?' }), h('div', { class: 'muted', style: { fontSize: '11px' }, text: '未测出' + stale }));
}

/** 倍率：有折扣则生效价大字 + 标签 + 划线牌价 */
function rateCell(m) {
  const tip = m.promo_note || '';
  if (m.promo_factor != null && m.promo_credits) {
    return h('span', { title: tip, style: { cursor: 'help' } },
      h('b', { text: m.promo_credits }),
      m.promo_label ? h('span', { class: 'chip strong', style: { marginLeft: '6px' }, text: m.promo_label }) : null,
      m.credits ? h('s', { class: 'muted', style: { fontSize: '11.5px', marginLeft: '6px' }, text: m.credits }) : null,
    );
  }
  if (m.promo_label) {
    return h('span', { title: tip, style: { cursor: 'help' } },
      m.credits || '—', h('span', { class: 'chip faint', style: { marginLeft: '6px' }, text: m.promo_label }));
  }
  return m.credits || '—';
}

function platOf(id) {
  if (id.startsWith('loomy:')) return ['loomy', 'Loomy（讯飞）'];
  if (id.startsWith('qoder:')) return ['qoder', 'Qoder（阿里）'];
  if (id.startsWith('zai:')) return ['zai', 'Z.AI 智谱 GLM'];
  if (id.startsWith('codex:')) return ['codex', 'Codex 订阅池'];
  if (id.startsWith('free:')) return ['free', '免费 Key 池'];
  return ['workbuddy', '腾讯 WorkBuddy'];
}

async function load(quiet = true) {
  err.set('');
  try {
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    models.set(d.models || []);
    probes.set(pr.probes || {});
  } catch (e) {
    err.set(e.message);
    if (!quiet) toast(e.message, 'fail');
  }
}

function modelRow(m, probeOf) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');

  const caps = [];
  if (m.is_default) caps.push('默认');
  if (m.supports_tool_call) caps.push('工具');
  if (m.supports_images) caps.push('视觉');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('思考常开');

  return h('tr', { title: m.description || '' },
    h('td', null,
      h('div', { style: { fontWeight: '550' }, text: m.id }),
      m.name ? h('div', { class: 'muted', style: { fontSize: '11px' }, text: m.name }) : null,
      caps.length ? h('div', { class: 'row', style: { gap: '5px', marginTop: '4px' } },
        ...caps.map(c => h('span', { class: 'chip faint', text: c }))) : null,
    ),
    h('td', { class: 'num' }, rateCell(m)),
    h('td', null, m.default_effort ? h('span', { class: 'chip strong', text: m.default_effort }) : h('span', { class: 'muted', text: '—' })),
    h('td', null, eff.length
      ? h('div', { class: 'row wrap', style: { gap: '4px' } }, ...eff.map(e => h('span', { class: 'chip', text: e })))
      : h('span', { class: 'muted', style: { fontSize: '12px' }, text: m.supports_reasoning ? `固定档 · 默认 ${m.default_effort || '?'}` : '不支持思考' })),
    h('td', { class: 'num', text: m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—' }),
    outCell(m, probeOf(m.id)),
  );
}

let started = false;
function ensureLoaded() {
  if (started) return;
  started = true;
  load();
}

export default defineView({
  id: 'models',
  title: '模型与档位',
  icon: 'models',
  group: '网关',
  keywords: '模型 倍率 档位 max tokens',
  sub() {
    const list = models();
    return list ? `${list.length} 个模型 · 实时查询上游` : '正在向上游查询…';
  },
  render() {
    ensureLoaded();
    const list = models();
    const pr = probes();
    const keys = Object.keys(pr);
    const probeOf = id => pr[id] || pr[keys.find(k => k.endsWith(':' + id))];

    let body;
    if (err()) body = h('div', { class: 'body' }, h('div', { class: 'empty' }, h('div', { class: 'd', text: err() })));
    else if (!list) body = h('div', { class: 'busy', text: '正在向上游查询' });
    else if (!list.length) body = h('div', { class: 'body' }, h('div', { class: 'empty' }, h('div', { class: 'd', text: '上游未返回模型' })));
    else {
      const groups = [];
      for (const m of list) {
        const [key, name] = platOf(m.id);
        let g = groups.find(x => x.key === key);
        if (!g) { g = { key, name, models: [] }; groups.push(g); }
        g.models.push(m);
      }
      body = h('div', null, ...groups.map(g => fragRows(g, probeOf)));
    }

    return h('div', { class: 'view stack' },
      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '模型能力' }),
          h('span', { class: 'hint', text: '最大输出含实测标注，官方声称值仅供参考' }),
          h('span', { class: 'grow' }),
          h('button', { class: 'btn sm', onclick: () => load(false) }, icon('refresh'), '重新获取'),
        ),
        body,
      ),
    );
  },
});

function fragRows(g, probeOf) {
  return h('div', null,
    h('div', { class: 'plat-head' },
      h('span', { class: 'nm', text: g.name }),
      h('span', { class: 'line' }),
      h('span', { text: `${g.models.length} 个` }),
    ),
    h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
      h('thead', null, h('tr', null,
        h('th', { text: '模型' }), h('th', { class: 'num', text: '倍率' }), h('th', { text: '默认档' }),
        h('th', { text: '思考档位' }), h('th', { class: 'num', text: '上下文' }), h('th', { class: 'num', text: '最大输出' }))),
      h('tbody', null, ...g.models.map(m => modelRow(m, probeOf))),
    )),
  );
}

