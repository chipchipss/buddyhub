/* ══════════════════════════════════════════════════════════════════
   views/accounts.js · 账号页
   三个数据源（腾讯 WorkBuddy 池 / Z.AI / 外部平台）排成**一张表**：
     顶部 = 平台 chips（含每平台账号数与异常数）+ 搜索
     每行 = 勾选框 · 名称 · 状态 · 余额 · 一个动作 · ⋯
     底部 = 选中后浮出的批量条
   其余信息（指标、曲线、该账号日志、凭证、全部操作、任务）都在详情抽屉里
   （views/account-detail.js）。原先那第三栏「垂直平台导航」删掉了——它只是
   把同一批账号切成四个界面，用户要先选对界面才看得见自己的账号（清单 I.3）。

   行模型在 rows.js（一个对象描述一行），动作在 acts.js（一次调用 + 该刷哪个信号），
   这一页只负责：筛选、选择、把行画出来。措辞因此不可能和抽屉不一致。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, openMenu, confirmDialog } from '../kernel.js';
import { defineView, parseHash, setHashTab, patchHash, doRefreshAll } from '../shell.js';
import { overview, refreshOverview } from '../store.js';
import { loadPlatforms } from '../platforms.js';
import { buildRows, platformsOf, matches, bulkKind } from '../rows.js';
import { statusChip } from '../status.js';
import { run } from '../acts.js';
import { openAddAccount } from '../drawers.js';
import { loadZai, zaiData } from './zai-segment.js';
import { loadExt, extData } from './ext-segment.js';
import { openAccountDetail } from './account-detail.js';

const filter = signal('all');    // 'all' 或平台 id
const query = signal('');
const sel = signal(new Set());   // 选中行的 key（provider:id）
const bulk = signal(null);       // 批量执行中：{label, done, total, bad}

function allRows() {
  return buildRows(overview()?.accounts, zaiData()?.accounts, extData()?.accounts);
}
function shownRows(rows) {
  const q = query.peek();
  return rows.filter(r => matches(r, filter.peek(), q));
}

/* ── 工具条：平台 chips + 搜索 ───────────────────────────────── */
function chipKids(rows) {
  const plats = platformsOf(rows);
  const cur = filter.peek();
  const bad = rows.filter(r => r.st.level !== 'ok').length;
  const chip = (id, name, n, alerts, attention) => h('button', {
    class: 'fchip' + (cur === id && !onlyAttention.peek() ? ' on' : '')
      + (attention && onlyAttention.peek() ? ' on' : '')
      + (alerts ? ' warn' : ''),
    title: alerts ? `${alerts} 个账号不在可用状态` : name,
    onclick: () => {
      if (attention) { onlyAttention.set(!onlyAttention.peek()); paint(); return; }
      filter.set(id); onlyAttention.set(false);
      setHashTab('accounts', id === 'all' ? null : id);
      paint();
    },
  }, h('span', { class: 'nm', text: name }),
    n ? h('span', { class: 'n', text: String(n) }) : null,
    alerts ? h('span', { class: 'bd', text: String(alerts) }) : null);

  return [
    chip('all', '全部', rows.length, bad),
    ...plats.map(p => chip(p.provider, p.name, p.n, p.bad)),
    ...(bad ? [chip('', '需处理', 0, bad, true)] : []),
  ];
}

function searchBox() {
  const input = h('input', {
    // 只在骨架建立时创建一次：paint() 从不重建工具条里的它，所以焦点与光标位置
    // 天然保住（id 仍要稳定，深链与测试都靠这个找到输入框）。
    id: 'acct-q', class: 'input search', type: 'search',
    placeholder: '搜索名称 / 平台 / ID',
    value: query.peek(),
    oninput: () => { query.set(input.value.trim()); paint(); },
    onkeydown: ev => { if (ev.key === 'Escape') { input.value = ''; query.set(''); paint(); } },
  });
  return h('div', { class: 'searchwrap' }, icon('search', 14), input);
}

// 右侧按钮组：Z.AI 那颗只在配置了 Plan 通道时出现，所以它归重画管，
// 而搜索框不参与重画（非受控输入，重建一次就丢一次焦点）。
function opsKids(z) {
  return [
    h('button', {
      class: 'btn sm ghost', title: '向每个平台逐个查询余额（上游请求，别高频点）',
      onclick: ev => doRefreshAll(ev),
    }, icon('refresh'), '刷新余额'),
    ...(z && z.configured ? [h('button', {
      class: 'btn sm ghost', title: '刷新全部 Z.AI JWT 额度（billing 族，上游 WAF 敏感）',
      onclick: async ev => {
        const b = ev.currentTarget; b.disabled = true;
        try {
          const r = await api('zai/quota_all', { method: 'POST' });
          toast(`额度刷新完成：成功 ${r.success} · 失败 ${r.failed}`);
          await loadZai(); paint();
        } catch (e) { toast(e.message, 'fail'); }
        finally { b.disabled = false; }
      },
    }, icon('wallet'), '刷新 Z.AI 额度')] : []),
  ];
}

/* ── 平台前提条件提示（不再藏在某个界面里）────────────────────── */
function notices(z) {
  const out = [];
  if (z && z.configured === false && z.message && (filter.peek() === 'all' || filter.peek() === 'zai')) {
    out.push(h('div', { class: 'notice' }, icon('alert', 14), h('span', { text: 'Z.AI：' + z.message })));
  }
  const cap = z && z.captcha;
  if (cap && !cap.enabled && (filter.peek() === 'all' || filter.peek() === 'zai')) {
    out.push(h('div', { class: 'notice' }, icon('alert', 14),
      h('span', { text: 'Z.AI 的 Plan 通道需要验证码求解器，当前未配置：JWT 账号能查额度，但不能领取套餐。' })));
  }
  return out;
}
const onlyAttention = signal(false);

/* ── 行 ──────────────────────────────────────────────────────── */
function balanceCell(r) {
  const b = r.bal;
  if (!b) return h('div', { class: 'cell bal muted', text: r.raw.note || '—' });
  const exact = b.exact ? fmt(b.remain) : String(b.remain ?? '—');
  return h('div', { class: 'cell bal' },
    h('div', { class: 'line' },
      h('span', { class: 'n', text: exact }),
      b.total > 0 ? h('span', { class: 'of', text: `/ ${fmt(b.total)} ${b.unit}` }) : h('span', { class: 'of', text: b.unit }),
    ),
    b.pct == null ? null : h('div', { class: 'meter thin' }, h('i', { style: { width: b.pct + '%' } })),
  );
}

// 大数缩写在列表里只保留一次（余额），抽屉里给全量数字
function fmt(n) {
  const v = Number(n || 0);
  if (v >= 1e6) return (v / 1e6).toFixed(1) + 'm';
  if (v >= 1e4) return Math.round(v / 1e3) + 'k';
  return String(v);
}

function rowNode(r) {
  const checked = sel.peek().has(r.key);
  const busyNow = bulk.peek() && bulk.peek().keys && bulk.peek().keys.has(r.key);
  return h('div', { class: 'acctrow' + (checked ? ' on' : '') + (r.off ? ' off' : '') + (busyNow ? ' run' : '') },
    h('input', {
      type: 'checkbox', class: 'cbx', checked, 'aria-label': '选择 ' + r.name,
      onchange: () => { toggleKey(r.key); },
    }),
    h('button', { class: 'cell name', title: '看详情', onclick: () => openAccountDetail(r) },
      h('span', { class: 'nm', text: r.name }),
      h('span', { class: 'id', text: r.sub || r.id }),
    ),
    h('div', { class: 'cell st', title: r.st.tip || '' }, statusChip(r.st)),
    balanceCell(r),
    h('div', { class: 'cell acts' },
      r.primary ? h('button', {
        // 一屏只允许一个主按钮（清单 49）：行内动作保持普通描边，
        // 异常与否由状态芯片表达，强调留给抽屉里的那一个。
        class: 'btn sm',
        text: r.primary.label,
        title: r.primary.tip || '',
        disabled: !!bulk.peek(),
        onclick: ev => actOne(r, r.primary, ev.currentTarget),
      }) : null,
      h('button', {
        class: 'btn sm icon ghost', title: '更多操作', disabled: !!bulk.peek(),
        onclick: ev => openMenu(ev.currentTarget, menuFor(r)),
      }, icon('more')),
    ),
  );
}

function menuFor(r) {
  const mk = a => ({
    label: a.label, danger: a.danger, tip: a.tip || a.confirm,
    onclick: () => actOne(r, a, null),
  });
  return [
    { label: '查看详情', onclick: () => openAccountDetail(r) },
    ...r.menu.map(mk),
  ];
}

/** actOne —— 单行单个动作：按钮进「执行中」态 → 调用 → 回执 → 就地重画。 */
async function actOne(r, a, btn) {
  if (a.kind === 'relogin') { openAddAccount(r.provider); return; }
  if (a.danger && a.confirm && !await confirmDialog(a.confirm, { ok: a.label })) return;
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '执行中…'; }
  const res = await run(a.kind, r);
  if (btn) { btn.disabled = false; btn.textContent = label; }
  toast(res.ok ? `${a.label}：${res.msg}` : `${a.label}失败：${res.msg}`, res.ok ? undefined : 'fail');
  paint();
}

/* ── 选择 ────────────────────────────────────────────────────── */
function toggleKey(k) {
  const next = new Set(sel.peek());
  if (next.has(k)) next.delete(k); else next.add(k);
  sel.set(next);
  paint();
}

function headRow(rows) {
  const all = rows.length && rows.every(r => sel.peek().has(r.key));
  return h('div', { class: 'acctrow head' },
    h('input', {
      type: 'checkbox', class: 'cbx', checked: all, 'aria-label': '全选本页',
      onchange: () => {
        const next = new Set(sel.peek());
        if (all) rows.forEach(r => next.delete(r.key));
        else rows.forEach(r => next.add(r.key));
        sel.set(next);
        paint();
      },
    }),
    h('span', { class: 'cell name', text: '账号' }),
    h('span', { class: 'cell st', text: '状态' }),
    h('span', { class: 'cell bal', text: '余额' }),
    h('span', { class: 'cell acts' }),
  );
}

/* ── 批量条（常驻一个 .bulkbar 宿主，没选中就 hidden）───────────
   不能用「没有选中就不渲染」：宿主一旦消失，吸底位置和行间距都会跳。 */
const OPS = [
  ['enable', '启用'],
  ['disable', '停用'],
  ['checkin', '签到'],
  ['balance', '查余额'],
  ['remove', '移除'],
];

function paintBulk(rows) {
  if (!bulkEl) return;
  const keys = sel.peek();
  if (!keys.size) { bulkEl.hidden = true; bulkEl.replaceChildren(); return; }
  bulkEl.hidden = false;
  const picked = rows.filter(r => keys.has(r.key));
  const b = bulk.peek();
  if (b) {
    bulkEl.replaceChildren(
      h('span', { class: 't', text: b.label }),
      h('div', { class: 'meter' }, h('i', { style: { width: Math.round(b.done / Math.max(1, b.total) * 100) + '%' } })),
      h('span', { class: 'p', text: `${b.done} / ${b.total}${b.bad ? ` · 失败 ${b.bad}` : ''}` }),
    );
    return;
  }
  const usable = op => picked.filter(r => bulkKind(r, op)).length;
  bulkEl.replaceChildren(
    h('span', { class: 't', text: `已选 ${picked.length} 个账号` }),
    h('span', { class: 'grow' }),
    ...OPS.map(([op, label]) => {
      const n = usable(op);
      if (!n) return null;
      return h('button', {
        class: 'btn sm' + (op === 'remove' ? ' danger' : ''),
        text: `${label} ${n}`,
        title: op === 'remove' ? '逐个删除，凭证文件一并清掉，不可恢复' : '',
        onclick: () => runBulk(op, label, picked),
      });
    }).filter(Boolean),
    h('button', {
      class: 'btn sm ghost', text: '取消选择',
      onclick: () => { sel.set(new Set()); paint(); },
    }),
  );
}

/** runBulk —— 逐账号执行（不是后台一把梭）：每完成一个就更新进度，
 *  失败的账号单独计数，结束后留在原地可见。 */
async function runBulk(op, label, picked) {
  const targets = picked.filter(r => bulkKind(r, op));
  if (op === 'remove' && !targets.length) return;
  if (op === 'remove') {
    if (!await confirmDialog(`将删除 ${targets.length} 个账号，池状态与本地凭证文件一并清掉，不可恢复。`, { ok: '删除' })) return;
  }
  const keys = new Set(targets.map(r => r.key));
  bulk.set({ label: `${label} 中`, done: 0, total: targets.length, bad: 0, keys });
  paint();
  let bad = 0;
  const fails = [];
  for (const r of targets) {
    const res = await run(bulkKind(r, op), r);
    if (!res.ok) { bad++; fails.push(`${r.name}：${res.msg}`); }
    bulk.set({ label: `${label} 中`, done: bulk.peek().done + 1, total: targets.length, bad, keys });
    paint();
  }
  bulk.set(null);
  sel.set(new Set());
  paint();
  if (bad) toast(`${label}完成：成功 ${targets.length - bad} · 失败 ${bad}`, 'fail', {
    action: { label: '看原因', onclick: () => toast(fails.slice(0, 4).join(' / ') || '原因见日志', 'fail') } },
  );
  else toast(`${label}完成：${targets.length} 个`);
}

/* ── 列表容器（命令式局部重画）───────────────────────────────────
   骨架只建一次，重画只换这几块自己的宿主：提示条 / chips / 右侧按钮 / 列表 / 批量条。
   两个理由：
     1) 搜索框是非受控输入，每敲一个字都重建它就等于把焦点和光标位置扔掉；
     2) 数据轮询会让外层 effect 重跑 render()。若每次都产出一棵新树，内核要么
        整棵换掉（用户正在滚动/输入时会跳），要么被重绘护栏挂起——挂起期间
        模块里的宿主还没上屏，用户点 chips、打字都画在一棵看不见的树上，
        表现就是「账号页点什么都没反应」（2026-10-09 浏览器实测）。
   复用同一棵树后：effect 拿到的还是同一个节点（内核不再替换），交互永远画在
   屏幕上那一棵里。 */
let rootEl = null, noticeEl = null, chipsEl = null, opsEl = null, bodyEl = null, bulkEl = null;

function bodyNode(rows) {
  const attn = onlyAttention.peek();
  const visible = shownRows(rows).filter(r => !attn || r.st.level !== 'ok');
  if (visible.length) return h('div', { class: 'acctlist' }, headRow(visible), ...visible.map(rowNode));
  if (rows.length) return h('div', { class: 'card flat' }, h('div', { class: 'body' },
    h('div', { class: 'empty' }, icon('search'),
      h('div', { class: 't', text: '没有匹配的账号' }),
      h('div', { class: 'd', text: '换个关键词，或清掉筛选条件。' }),
      h('button', {
        class: 'btn sm', style: { marginTop: '10px' }, text: '清除筛选',
        onclick: () => { filter.set('all'); query.set(''); onlyAttention.set(false); setHashTab('accounts', null); paint(); },
      }))))
    ;
  return h('div', { class: 'card flat' }, h('div', { class: 'body' },
    h('div', { class: 'empty' }, icon('accounts'),
      h('div', { class: 't', text: '还没有账号' }),
      h('div', { class: 'd', text: '加一个就能开始：登录态由网关自己维持，签到与额度按排程跑。' }),
      h('button', {
        class: 'btn primary', style: { marginTop: '12px' },
        onclick: () => openAddAccount(),
      }, icon('plus'), '添加账号'))));
}

function paint() {
  if (!rootEl) return;
  const rows = allRows();
  const ns = notices(zaiData());
  noticeEl.replaceChildren(...ns);
  noticeEl.hidden = ns.length === 0;
  chipsEl.replaceChildren(...chipKids(rows));
  opsEl.replaceChildren(...opsKids(zaiData()));
  bodyEl.replaceChildren(bodyNode(rows));
  paintBulk(rows);
}

/** deepLink —— #accounts?acct=workbuddy:<id> 直接打开某个账号的详情。 */
function deepLink() {
  const want = parseHash().acct;
  if (!want) return;
  patchHash({});                 // acct 用完即弃，后退键不会重复打开抽屉
  const rows = allRows();
  const hit = rows.find(r => r.key === want) || rows.find(r => r.id === want);
  if (hit) openAccountDetail(hit);
  else toast('账号 ' + want + ' 不在池里（可能已被移除）', 'fail');
}

export default defineView({
  id: 'accounts',
  page: 'accounts',
  tab: '账号',
  title: '账号',
  icon: 'accounts',
  keywords: '账号 池 目录 平台 zai 外部 详情 余额 状态',
  sub() {
    const rows = buildRows(overview()?.accounts, zaiData()?.accounts, extData()?.accounts);
    if (!rows.length) return overview() === null ? '正在连接…' : '还没有账号';
    const ok = rows.filter(r => r.st.level === 'ok').length;
    return `${rows.length} 个账号 · ${ok} 可用 · ${rows.length - ok} 需留意`;
  },
  tick() {
    refreshOverview();
    loadZai();
    loadExt();
  },
  render() {
    loadPlatforms();
    if (!overview()) refreshOverview();
    if (!zaiData()) loadZai();
    if (!extData()) loadExt();

    // 只把「数据」当依赖：筛选词、平台芯片、选择集都走上面的命令式重画
    overview(); zaiData(); extData();
    const rows = allRows();
    if (filter.peek() !== 'all' && !rows.some(r => r.provider === filter.peek())) filter.set('all');

    // 已经上屏就复用（顺带就地重画一次），换页回来才重建骨架
    if (rootEl && rootEl.isConnected) { paint(); return rootEl; }

    noticeEl = h('div', { class: 'stack', hidden: true });
    chipsEl = h('div', { class: 'fchips' });
    opsEl = h('div', { class: 'row wrap', style: { gap: '8px' } });
    bodyEl = h('div');
    bulkEl = h('div', { class: 'bulkbar', hidden: true });
    rootEl = h('div', { class: 'acct-body stack' },
      h('div', { class: 'stack' },
        noticeEl,
        h('div', { class: 'acctbar' }, chipsEl, h('span', { class: 'grow' }), searchBox(), opsEl),
        bodyEl),
      bulkEl,
    );
    // 首帧先画内容再挂上去：挂点由内核/路由决定，这里不能等 isConnected
    paint();
    queueMicrotask(deepLink);
    return rootEl;
  },
});
