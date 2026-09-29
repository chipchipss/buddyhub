/* ══════════════════════════════════════════════════════════════════
   views/usage.js · 用量与积分
   两段：Token 用量（时序图 + 三张表）/ 积分构成（逐包对比）
   图表为原生 SVG 手绘——面板无构建步骤，不引入图表库。
   单色堆叠：prompt 用实色，completion 用弱化色。
   ══════════════════════════════════════════════════════════════════ */

import { h, svgEl, icon, signal, api, toast, fmtTok, fmtMs, fmtRate } from '../kernel.js';
import { defineView } from '../shell.js';

const seg = signal('usage');
const hours = signal(72);
const plat = signal('all');
const usage = signal(null);
const pk = signal(null);
const pkPlat = signal('all');
const note = signal('');

async function loadUsage(quiet = true) {
  try {
    usage.set(await api('usage?hours=' + encodeURIComponent(hours.peek())));
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

async function loadPk(quiet = true) {
  try { pk.set(await api('packages')); }
  catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

/* ── 过滤（平台维度在本地重算，避免重复拉取）───────────────────── */
function filtered() {
  const d = usage();
  if (!d) return null;
  const p = plat();
  if (p === 'all') return d;
  const match = k => k === p || k === p + ':' || (p === 'cn' && (k === 'cn' || !k)) || (p === 'global' && k === 'global');
  const copy = { ...d };
  copy.by_model = (d.by_model || []).filter(x => match(x.key) || String(x.key).startsWith(p + ':'));
  copy.by_account = (d.by_account || []).filter(x => match(x.realm || ''));
  copy.by_realm = (d.by_realm || []).filter(x => match(x.key));
  const t = { requests: 0, total_tokens: 0, prompt_tokens: 0, completion_tokens: 0, errors: 0 };
  for (const x of copy.by_account) {
    t.requests += x.requests || 0; t.total_tokens += x.total_tokens || 0;
    t.prompt_tokens += x.prompt_tokens || 0; t.completion_tokens += x.completion_tokens || 0;
    t.errors += x.errors || 0;
  }
  t.avg_latency_ms = copy.by_account.length
    ? copy.by_account.reduce((a, x) => a + (x.avg_latency_ms || 0), 0) / copy.by_account.length : 0;
  copy.totals = t;
  return copy;
}

/* ── 时序图 ───────────────────────────────────────────────────── */
function parsePointTime(p) {
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

function chart(series) {
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0), ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct), req: p.requests || 0 });
  }
  const host = h('div', { class: 'chart' },
    h('div', { class: 'hd' },
      h('span', { text: 'Token 时序', style: { fontWeight: '550' } }),
      h('span', { class: 'grow' }),
      h('span', { class: 'legend' }, h('i', { class: 'sw sw-a' }), 'prompt', h('i', { class: 'sw sw-b' }), 'completion'),
    ),
  );
  const bd = h('div', { class: 'bd' });
  host.append(bd);

  if (!pts.length) {
    bd.append(h('div', { class: 'empty', style: { padding: '28px 0' } }, h('div', { class: 'd', text: '暂无用量数据。发起一次对话后再刷新。' })));
    return host;
  }

  const W = 760, H = 200, PL = 54, PR = 12, PT = 12, PB = 46;
  const iw = W - PL - PR, ih = H - PT - PB;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...pts.map(p => p.tt));

  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const bw = Math.max(1.5, Math.min(30, iw * (minGap / span) * 0.7));
  const xOf = t => PL + (t - t0) / span * iw;

  const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', preserveAspectRatio: 'xMidYMid meet' });

  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    svg.append(svgEl('line', { class: 'gl', x1: PL, y1: y.toFixed(1), x2: W - PR, y2: y.toFixed(1) }));
    svg.append(svgEl('text', { class: 'tk', x: PL - 7, y: (y + 3.5).toFixed(1), 'text-anchor': 'end', text: fmtTok(max * i / 4) }));
  }

  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    const yBase = PT + ih;
    if (hP > 0) svg.append(svgEl('rect', { x: x.toFixed(2), y: (yBase - hP).toFixed(2), width: bw.toFixed(2), height: hP.toFixed(2), fill: 'var(--fg)', rx: 1.5 }));
    if (hC > 0) svg.append(svgEl('rect', { x: x.toFixed(2), y: (yBase - hP - hC).toFixed(2), width: bw.toFixed(2), height: hC.toFixed(2), fill: 'var(--fg-3)', rx: 1.5 }));
    const title = svgEl('title', { text: `${p.raw}  ${fmtTok(p.pt)} prompt / ${fmtTok(p.ct)} completion / ${p.req} 次` });
    svg.append(title);
  }

  svg.append(svgEl('line', { class: 'ax', x1: PL, y1: PT + ih, x2: W - PR, y2: PT + ih }));

  const TICKS = Math.min(6, pts.length);
  const used = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (used.has(bi)) continue;
    used.add(bi);
    const p = pts[bi];
    const d = new Date(p.t);
    const lab = p.scope === 'day'
      ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
      : String(d.getHours()).padStart(2, '0') + ':00';
    const cx = xOf(p.t);
    const anchor = (cx < PL + 14 || cx > W - PR - 14) ? 'end' : 'middle';
    svg.append(svgEl('text', {
      class: 'tk', x: Math.max(PL - 2, Math.min(W - PR, cx)).toFixed(1),
      y: PT + ih + 18, 'text-anchor': anchor, text: lab,
    }));
  }

  let prevDay = null;
  for (const p of pts) {
    const day = new Date(p.t).getDate();
    if (prevDay !== null && day !== prevDay) {
      svg.append(svgEl('line', { class: 'gl', x1: xOf(p.t).toFixed(1), y1: PT, x2: xOf(p.t).toFixed(1), y2: PT + ih, opacity: '.5' }));
    }
    prevDay = day;
  }

  bd.append(svg);
  return host;
}

function statTile(v, k) {
  return h('div', { class: 'tile' }, h('div', { class: 'v', text: v }), h('div', { class: 'k', text: k }));
}

function usageRows(rows, withPerf, withRealm) {
  return h('tbody', null, ...rows.map(x => h('tr', null,
    h('td', null,
      h('div', { style: { fontFamily: 'var(--mono)', fontSize: '12px' }, text: withRealm ? x.key : String(x.key).slice(0, 8) }),
      x.extra ? h('div', { class: 'muted', style: { fontSize: '11px' }, text: x.extra }) : null),
    withRealm ? h('td', { class: 'num', text: x.realm || '' }) : null,
    h('td', { class: 'num', text: fmtTok(x.requests) }),
    h('td', { class: 'num', text: x.errors ? fmtTok(x.errors) : '—' }),
    h('td', { class: 'num', text: fmtTok(x.prompt_tokens) }),
    h('td', { class: 'num', text: fmtTok(x.completion_tokens) }),
    h('td', { class: 'num', text: fmtTok(x.total_tokens) }),
    withPerf ? h('td', { class: 'num', text: fmtMs(x.avg_latency_ms) }) : null,
    withPerf ? h('td', { class: 'num', text: fmtRate(x.avg_tokens_per_second) }) : null,
  )));
}

function usageSeg() {
  const d = filtered();
  const t = (d && d.totals) || {};
  const winLabel = { 24: '近 24 小时', 72: '近 3 天', 168: '近 7 天', 720: '近 30 天', 0: '全部历史' }[hours()] || '';

  return h('div', { class: 'stack' },
    h('div', { class: 'tiles' },
      statTile(fmtTok(t.requests), '请求数'),
      statTile(fmtTok(t.total_tokens), '总 token'),
      statTile(fmtTok(t.prompt_tokens), 'prompt'),
      statTile(fmtTok(t.completion_tokens), 'completion'),
      statTile(String(t.errors || 0), '失败尝试'),
      statTile(fmtMs(t.avg_latency_ms), '平均延迟'),
    ),
    h('section', { class: 'card' },
      h('header', null,
        h('h2', { text: '用量总览' }),
        h('span', { class: 'grow' }),
        h('span', { class: 'hint', text: d ? `${winLabel} · ${d.buckets || 0} 个分桶${d.since ? ' · 数据自 ' + String(d.since).replace('T', ' ') : ''}` : '' }),
        h('select', {
          class: 'input', style: { width: 'auto' },
          onchange: ev => { hours.set(Number(ev.target.value)); loadUsage(); },
        }, ...[['24', '近 24 小时'], ['72', '近 3 天'], ['168', '近 7 天'], ['720', '近 30 天'], ['0', '全部历史']]
          .map(([v, n]) => h('option', { value: v, selected: Number(v) === hours(), text: n }))),
        h('select', {
          class: 'input', style: { width: 'auto' },
          onchange: ev => { plat.set(ev.target.value); },
        }, ...[['all', '全部平台'], ['cn', '腾讯 CN'], ['global', '腾讯 Global'], ['loomy', 'Loomy'], ['qoder', 'Qoder'], ['codex', 'Codex'], ['zai', 'Z.AI'], ['free', '免费池']]
          .map(([v, n]) => h('option', { value: v, selected: v === plat(), text: n }))),
        h('button', { class: 'btn sm', onclick: () => loadUsage(false) }, icon('refresh'), '刷新'),
      ),
      h('div', { class: 'body' }, d ? chart(d.series || []) : h('div', { class: 'busy', text: '读取用量' })),
    ),
    h('section', { class: 'card' },
      h('header', null, h('h2', { text: '按账号' }), h('span', { class: 'grow' }), h('span', { class: 'hint', text: '请求数含失败尝试' })),
      h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null,
          h('th', { text: '账号' }), h('th', { class: 'num', text: '域' }), h('th', { class: 'num', text: '请求' }), h('th', { class: 'num', text: '失败' }),
          h('th', { class: 'num', text: 'Prompt' }), h('th', { class: 'num', text: 'Completion' }), h('th', { class: 'num', text: '合计' }),
          h('th', { class: 'num', text: '均延迟' }), h('th', { class: 'num', text: '均速率' }))),
        usageRows((d && d.by_account) || [], true, true),
      )),
    ),
    h('section', { class: 'card' },
      h('header', null, h('h2', { text: '按模型' })),
      h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null,
          h('th', { text: '模型' }), h('th', { class: 'num', text: '请求' }), h('th', { class: 'num', text: '失败' }),
          h('th', { class: 'num', text: 'Prompt' }), h('th', { class: 'num', text: 'Completion' }), h('th', { class: 'num', text: '合计' }))),
        usageRows((d && d.by_model) || [], false, false),
      )),
    ),
  );
}

/* ── 积分构成 ─────────────────────────────────────────────────── */
const PK_SHADES = ['var(--fg)', 'rgba(127,127,127,.72)', 'rgba(127,127,127,.5)', 'rgba(127,127,127,.36)', 'rgba(127,127,127,.26)'];
const shadeOf = i => PK_SHADES[i % PK_SHADES.length];

function bySource(packs) {
  const m = new Map();
  for (const p of packs) {
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || { key: k, name: p.name || '(未命名)', n: 0, remain: 0, size: 0, used: 0, minCreated: '', minEnd: '' };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

function pkSeg() {
  const d = pk();
  if (!d) return h('div', { class: 'busy', text: '读取积分构成' });
  const all = d.accounts || [];
  const list = pkPlat() === 'all' ? all : all.filter(a => (a.realm || 'cn') === (pkPlat() === 'workbuddy' ? 'cn' : pkPlat()));

  const names = [];
  for (const a of list) for (const s of bySource(a.packages || [])) if (!names.includes(s.key)) names.push(s.key);
  const shade = n => shadeOf(names.indexOf(n));
  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  return h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('header', null,
        h('h2', { text: '账号对比' }),
        h('span', { class: 'hint', text: '余额 = 若干积分包之和，按来源着色深浅对比' }),
        h('span', { class: 'grow' }),
        h('select', {
          class: 'input', style: { width: 'auto' },
          onchange: ev => pkPlat.set(ev.target.value),
        }, ...[['all', '全部平台'], ['workbuddy', '腾讯'], ['loomy', 'Loomy'], ['qoder', 'Qoder'], ['zai', 'Z.AI']]
          .map(([v, n]) => h('option', { value: v, selected: v === pkPlat(), text: n }))),
        h('button', { class: 'btn sm', onclick: () => loadPk(false) }, icon('refresh'), '刷新'),
      ),
      h('div', { class: 'body' },
        !list.length ? h('div', { class: 'empty' }, h('div', { class: 'd', text: '没有账号' }))
          : h('div', { class: 'acct-grid' }, ...list.map(a => {
            if (a.error) {
              return h('article', { class: 'acct' },
                h('div', { class: 'who' }, h('div', { class: 'nm', text: a.nickname || a.uid.slice(0, 8) })),
                h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '8px' }, text: '查询失败：' + a.error }));
            }
            const srcs = bySource(a.packages || []);
            const total = Math.max(1, Number(a.size || 0));
            return h('article', { class: 'acct' },
              h('div', { class: 'top' },
                h('div', { class: 'who' }, h('div', { class: 'nm', text: a.nickname || a.uid.slice(0, 8) })),
                h('span', { class: 'chip faint', text: a.realm || '' }),
              ),
              h('div', { class: 'credits' },
                h('div', { class: 'line' },
                  h('span', { class: 'n', text: fmtTok(a.remain) }),
                  h('span', { class: 'of', text: `共 ${fmtTok(a.size)} · ${(a.packages || []).length} 个包` }),
                ),
                h('div', { class: 'meter', style: { height: '7px', display: 'flex', borderRadius: '4px' } },
                  ...srcs.map(s => h('i', { style: { width: (s.size / total * 100).toFixed(2) + '%', background: shade(s.key), borderRadius: '0' }, title: `${s.name} ${fmtTok(s.size)}` }))),
              ),
              h('div', { class: 'row wrap', style: { gap: '4px 12px', marginTop: '9px' } },
                ...srcs.map(s => h('span', { class: 'row', style: { gap: '5px', fontSize: '11px', color: 'var(--fg-3)' } },
                  h('i', { style: { width: '9px', height: '9px', borderRadius: '3px', background: shade(s.key), display: 'inline-block' } }),
                  `${String(s.name).replace(/^CodeBuddy/, '')} ×${s.n} · ${fmtTok(s.size)}`)),
              ),
            );
          })),
      ),
    ),
  );
}

export default defineView({
  id: 'usage',
  title: '用量与积分',
  icon: 'usage',
  group: '数据',
  keywords: '用量 token 积分 对比 统计',
  sub() {
    const d = usage();
    return d ? `请求 ${fmtTok((d.totals || {}).requests)} · token ${fmtTok((d.totals || {}).total_tokens)}` : '正在读取…';
  },
  render() {
    const s = seg();
    if (s === 'usage' && !usage()) loadUsage();
    if (s === 'pk' && !pk()) loadPk();
    return h('div', { class: 'view stack' },
      h('div', { class: 'row' },
        h('div', { class: 'seg' },
          h('button', { class: s === 'usage' ? 'on' : '', text: 'Token 用量', onclick: () => seg.set('usage') }),
          h('button', { class: s === 'pk' ? 'on' : '', text: '积分构成', onclick: () => seg.set('pk') }),
        ),
      ),
      s === 'usage' ? usageSeg() : pkSeg(),
    );
  },
});
