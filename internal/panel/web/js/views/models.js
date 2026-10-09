/* ══════════════════════════════════════════════════════════════════
   views/models.js · 模型与档位
   按平台分组；倍率列展示牌价 vs 生效价；最大输出列标注实测钳制。

   这一屏几十行几十列，用户真正要做的是两件小事：挑一个便宜/能用的档位，
   然后把完整模型 ID 复制进客户端。所以：可搜、可按倍率/上下文/最大输出排序、
   可只看免费或打折，点模型名即复制（清单 43/44）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, fmtK, copyText } from '../kernel.js';
import { defineView } from '../shell.js';
import { platforms, loadPlatforms, platName } from '../platforms.js';

const models = signal(null);
const probes = signal({});
const err = signal('');

// 搜索词 / 筛选 / 排序刻意**不是**信号：它们一旦进信号，每次敲键盘就把整屏
// 重建一遍，搜索框的焦点和光标位置随之丢失（配置页踩过同一个坑）。
// 改成普通变量 + 就地重画表体（repaint）。
let qv = '';
let onlyV = 'all';             // all / free / promo
let sortV = { k: '', dir: 1 }; // 排序只在平台分组内部生效

function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}

/* ── 价格口径 ─────────────────────────────────────────────────────
   credits 是牌价原文（如 "x0.05"），promo_credits 是限时优惠后的生效价原文
   （如 "0x"）。两个都是给客户端显示的字符串，这里只为「免费/打折」筛选与
   排序做一次解析；解析不出来就当作未知，不硬凑一个数出来。 */
function rateNum(s) {
  const n = parseFloat(String(s ?? '').replace(/[x×\s]/gi, ''));
  return Number.isFinite(n) ? n : null;
}
const effRate = m => rateNum(m.promo_credits) ?? rateNum(m.credits);
const isFree = m => effRate(m) === 0;
const isPromo = m => m.promo_factor != null || !!m.promo_label;

const COLS = {
  rate: m => { const r = effRate(m); return r === null ? Infinity : r; },
  context: m => m.context_length || 0,
  output: m => m.max_output_tokens || 0,
};

function matches(m) {
  const s = qv.trim().toLowerCase();
  if (!s) return true;
  return (m.id + ' ' + (m.name || '') + ' ' + (m.vendor || '') + ' ' + (m.description || ''))
    .toLowerCase().includes(s);
}
function passes(m) {
  if (!matches(m)) return false;
  if (onlyV === 'free') return isFree(m);
  if (onlyV === 'promo') return isPromo(m);
  return true;
}
function sorted(list) {
  const f = COLS[sortV.k];
  if (!f) return list;
  // 相等时保持原顺序：同倍率的一串里，用户要的是「和不动它时看到的一样」
  return list.map((m, i) => [f(m), i, m])
    .sort((a, b) => (a[0] - b[0]) * sortV.dir || a[1] - b[1])
    .map(x => x[2]);
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

/** 能力徽章单独一列（清单 43）：原来塞在模型名下面，扫一眼要逐行读文字 */
function capsCell(m) {
  const caps = [];
  if (m.is_default) caps.push(['strong', '默认']);
  if (m.supports_tool_call) caps.push(['', '工具']);
  if (m.supports_images) caps.push(['', '视觉']);
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push(['faint', '思考常开']);
  if (!caps.length) return h('td', { class: 'muted', text: '—' });
  return h('td', null, h('div', { class: 'row wrap', style: { gap: '4px' } },
    ...caps.map(([k, c]) => h('span', { class: 'chip ' + k, text: c }))));
}

// 平台归属从前缀推出——清单来自注册表（platforms.js），不再自带名字表。
// 腾讯模型无前缀（裸 id / cn: / global:），作为兜底分组。
function platOf(id) {
  if (!id.includes(':')) return ['workbuddy', platName('workbuddy')];
  const prefix = id.slice(0, id.indexOf(':') + 1);
  const p = platforms.peek().find(x => x.prefix === prefix);
  return p ? [p.id, p.name] : ['workbuddy', platName('workbuddy')];
}

async function load(quiet = true) {
  err.set('');
  try {
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    models.set(d.models || []);
    probes.set(pr.probes || {});
    loadedAt = new Date().toLocaleTimeString('zh-CN', { hour12: false });
  } catch (e) {
    err.set(e.message);
    if (!quiet) toast(e.message, 'fail');
  }
}

function modelRow(m, probeOf) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');

  return h('tr', { title: m.description || '' },
    h('td', null,
      // 点名字即复制完整 ID（含前缀）——复制模型 ID 是这一页最常见的真实动作，
      // 不该让人选中文字再按 Ctrl+C，也不知道复制到的到底是不是能直接用的那个串。
      h('div', {
        style: { fontWeight: '550', fontFamily: 'var(--mono)', cursor: 'copy' },
        title: '点击复制完整模型 ID（含前缀，可直接填进客户端）',
        text: m.id,
        onclick: async () => {
          try { await copyText(m.id); toast('已复制 ' + m.id); }
          catch { toast('复制失败，请手动选择', 'fail'); }
        },
      }),
      m.name ? h('div', { class: 'muted', style: { fontSize: '11px' }, text: m.name }) : null,
    ),
    h('td', { class: 'num' }, rateCell(m)),
    h('td', { class: 'num', text: m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—' }),
    outCell(m, probeOf(m.id)),
    h('td', null, m.default_effort ? h('span', { class: 'chip strong', text: m.default_effort }) : h('span', { class: 'muted', text: '—' })),
    h('td', null, eff.length
      ? h('div', { class: 'row wrap', style: { gap: '4px' } }, ...eff.map(e => h('span', { class: 'chip', text: e })))
      : h('span', { class: 'muted', style: { fontSize: '12px' }, text: m.supports_reasoning ? `固定档 · 默认 ${m.default_effort || '?'}` : '不支持思考' })),
    capsCell(m),
  );
}

function thSort(key, label) {
  const on = sortV.k === key;
  return h('th', {
    class: 'num',
    style: { cursor: 'pointer', whiteSpace: 'nowrap' },
    title: on ? '再点一次换方向' : `按${label}排序`,
    text: label + (on ? (sortV.dir > 0 ? ' ▲' : ' ▼') : ''),
    onclick: () => {
      sortV = on ? { k: key, dir: -sortV.dir } : { k: key, dir: 1 };
      repaint();
    },
  });
}

function tableHead() {
  return h('thead', null, h('tr', null,
    h('th', { text: '模型' }),
    thSort('rate', '倍率'),
    thSort('context', '上下文'),
    thSort('output', '最大输出'),
    h('th', { text: '默认档' }),
    h('th', { text: '思考档位' }),
    h('th', { text: '能力' }),
  ));
}

function groupBlock(g, rows, probeOf) {
  return h('div', null,
    h('div', { class: 'plat-head' },
      h('span', { class: 'nm', text: g.name }),
      h('span', { class: 'line' }),
      h('span', { text: `${rows.length} 个` }),
    ),
    h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
      tableHead(),
      h('tbody', null, ...rows.map(m => modelRow(m, probeOf))),
    )),
  );
}

// 一次性加载：模型清单每次都实时打上游（慢且上游 WAF 敏感），不适合 5 秒 tick。
// 加载时刻透出在副标题，让「数据多旧」可见，而不是看起来像实时。
let started = false;
let loadedAt = '';
let hostEl = null;      // 表体宿主：搜索/排序只换它的内容，不重建整屏（输入焦点会丢）
let headEl = null;      // 计数那一行

function groupsOf(list) {
  const groups = [];
  for (const m of list) {
    const [key, name] = platOf(m.id);
    let g = groups.find(x => x.key === key);
    if (!g) { g = { key, name, models: [] }; groups.push(g); }
    g.models.push(m);
  }
  return groups;
}

/** repaint —— 筛选/排序后的就地重画：整屏重建会把搜索框的焦点和光标丢掉 */
function repaint() {
  if (!hostEl || !hostEl.isConnected) return;
  const list = models() || [];
  const pr = probes();
  const keys = Object.keys(pr);
  const probeOf = id => pr[id] || pr[keys.find(k => k.endsWith(':' + id))];

  const parts = [];
  let shown = 0;
  for (const g of groupsOf(list)) {
    const rows = sorted(g.models.filter(passes));
    if (!rows.length) continue;
    shown += rows.length;
    parts.push(groupBlock(g, rows, probeOf));
  }
  hostEl.replaceChildren(...(shown ? parts
    : [h('div', { class: 'empty' }, icon('models'),
      h('div', { class: 't', text: '没有符合条件的模型' }),
      h('div', { class: 'd', text: `「${qv}」加当前筛选在 ${list.length} 个模型里都没匹配上；换个词，或点「全部模型」。` }))]));
  if (headEl) {
    headEl.textContent = shown !== list.length
      ? `筛出 ${shown} / ${list.length} 个`
      : `${list.length} 个模型 · ${loadedAt ? loadedAt + ' 获取' : '查询中'}`;
  }
}

/* 清单 51：命令面板搜模型用。没加载过清单就返回空——面板不造假数据。 */
export function paletteItems() {
  const list = models();
  if (!list) return [];
  return list.map(m => ({
    label: m.id,
    id: m.id,
    keywords: (m.id + ' ' + (m.name || '') + ' ' + (m.vendor || '')).toLowerCase(),
  }));
}

export default defineView({
  id: 'models',
  page: 'gateway',
  tab: '模型与档位',
  title: '模型与档位',
  icon: 'models',
  keywords: '模型 倍率 档位 max tokens 上下文 免费',
  sub() {
    const list = models();
    if (!list) return '正在向上游查询…';
    return `${list.length} 个模型 · ${loadedAt ? loadedAt + ' 获取' : '查询中'}`;
  },
  render() {
    ensureLoaded();
    loadPlatforms();
    const list = models();

    if (err()) {
      return h('div', { class: 'view stack' },
        h('section', { class: 'card' },
          h('header', null, h('h2', { text: '模型能力' })),
          h('div', { class: 'body' }, h('div', { class: 'empty' }, h('div', { class: 'd', text: err() })))));
    }
    if (!list) return h('div', { class: 'view stack' }, h('div', { class: 'busy', text: '正在向上游查询' }));
    if (!list.length) {
      return h('div', { class: 'view stack' },
        h('section', { class: 'card' },
          h('header', null, h('h2', { text: '模型能力' })),
          h('div', { class: 'body' }, h('div', { class: 'empty' }, h('div', { class: 'd', text: '上游未返回模型' })))));
    }

    const search = h('input', {
      class: 'input search', type: 'search', placeholder: '搜模型（ID / 名字 / 厂商）',
      value: qv, style: { maxWidth: '260px' },
      oninput: () => { qv = search.value; repaint(); },
      onkeydown: ev => { if (ev.key !== 'Escape') return; search.value = ''; qv = ''; repaint(); },
    });
    const filters = h('div', { class: 'seg' });
    const paintFilters = () => filters.replaceChildren(
      ...[['all', '全部模型'], ['free', '只看免费'], ['promo', '打折中']]
        .map(([v, n]) => h('button', {
          class: onlyV === v ? 'on' : '', text: n,
          onclick: () => { onlyV = v; paintFilters(); repaint(); },
        })),
    );
    paintFilters();
    headEl = h('span', { class: 'muted', style: { fontSize: '11.5px' } });
    hostEl = h('div', { class: 'body stack' });

    const view = h('div', { class: 'view stack' },
      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '模型能力' }),
          h('span', { class: 'hint', text: '点模型名即复制完整 ID · 表头可按倍率/上下文/最大输出排序 · 最大输出含实测标注' }),
          h('span', { class: 'grow' }),
          headEl,
          h('button', { class: 'btn sm', onclick: () => load(false) }, icon('refresh'), '重新获取'),
        ),
        h('div', { class: 'row wrap', style: { gap: '8px', padding: '12px 18px 0' } }, search, filters),
        hostEl,
      ),
    );
    queueMicrotask(repaint);
    return view;
  },
});

/** 首次进入才打上游；失败后允许「重新获取」再试一次 */
function ensureLoaded() {
  if (started) return;
  started = true;
  load();
}
