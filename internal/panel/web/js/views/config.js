/* ══════════════════════════════════════════════════════════════════
   views/config.js · 配置
   声明式字段表 → 表单。四十来项配置分成几组，左边一列小目录跳位，
   顶部一个搜索框（清单 30）；日常项露着，熔断/降权这类调参项折进
   「高级」（清单 35）；改了什么、其中几项要重启，保存栏随时在说（清单 33）。

   值的真相仍然只在后端：默认值取 Default()，需重启清单取
   restartRequiredFields()，前端不自带副本——自带一份迟早和线上漂开。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog } from '../kernel.js';
import { defineView } from '../shell.js';
import { theme, setTheme } from '../store.js';

const cfg = signal(null);
const cfgPath = signal('');
const defs = signal(null);            // 默认配置（恢复默认用）
const restartList = signal([]);       // 装配期定死的字段路径
const saving = signal(false);
const justSaved = signal([]);         // 上一次保存里「需重启」的那几项
const q = signal('');                 // 搜索关键字
const edits = signal(new Map());      // 未保存的改动：字段名 → 界面值

/* 字段表：n=配置名 p=路径 l=展示名 tip=用途 sw=开关 dur=时长 hours=小时列表
   adv=高级项（默认折叠）。开关一律要有 tip——一片没有字的勾选框没人敢动。 */
const FIELDS = [
  { group: '服务', cols: 2, items: [
    { n: 'listen', p: ['listen'], l: '监听地址', tip: '改完要重启进程', ph: ':7863' },
    { n: 'api_key', p: ['api_key'], l: 'API 密钥', tip: '立即生效（含面板自身）', ph: '留空 = 不鉴权', secret: true },
  ] },
  { group: '定时任务', cols: 2, items: [
    { n: 'checkin_enabled', p: ['schedule', 'checkin_enabled'], l: '自动签到', sw: true,
      tip: '到点替池内账号签到领积分；关掉就再也没有自动收益' },
    { n: 'keepalive_enabled', p: ['schedule', 'keepalive_enabled'], l: 'Token 保活', sw: true,
      tip: '定期刷一次登录态，免得账号静默两天后被上游判成已失效' },
    { n: 'checkin_hours', p: ['schedule', 'checkin_hours'], l: '签到时点（小时）', ph: '9, 21', hours: true,
      tip: '每天在这几个整点各跑一轮，逗号分隔' },
    { n: 'keepalive_hours', p: ['schedule', 'keepalive_hours'], l: '保活时点（小时）', ph: '22', hours: true },
    { n: 'travel_enabled', p: ['schedule', 'travel_enabled'], l: '猫猫旅行', sw: true,
      tip: '把猫派出去旅行再领回来，一趟两个动作' },
    { n: 'activity_enabled', p: ['schedule', 'activity_enabled'], l: '活跃上报', sw: true,
      tip: '上报活跃：连登天数继续走，也是领养的前置条件' },
    { n: 'travel_hours', p: ['schedule', 'travel_hours'], l: '旅行时点（小时）', ph: '9, 21', hours: true },
    { n: 'activity_hours', p: ['schedule', 'activity_hours'], l: '上报时点（小时）', ph: '10', hours: true },
    { n: 'balance_refresh_enabled', p: ['schedule', 'balance_refresh_enabled'], l: '后台刷新余额', sw: true,
      tip: '按下面的间隔拉一次各账号余额，供面板显示和选号参考' },
    { n: 'balance_refresh_minutes', p: ['schedule', 'balance_refresh_minutes'], l: '刷新间隔（分钟）',
      type: 'number', min: 1, ph: '5' },
    { n: 'growth_enabled', p: ['schedule', 'growth_enabled'], l: '成长任务自动执行', sw: true,
      tip: '每日到点自动「检查并执行」待办；含真实对话的任务耗时较长' },
    { n: 'growth_hours', p: ['schedule', 'growth_hours'], l: '执行时点（小时）', ph: '1', hours: true },
  ] },
  { group: '账号池与流量治理', cols: 3, items: [
    { n: 'max_in_flight', p: ['pool', 'max_in_flight'], l: '单账号最大在途', type: 'number', min: 0, ph: '3',
      tip: '一个账号同时最多挂几个请求；0 = 不限制' },
    { n: 'max_in_flight_global', p: ['pool', 'max_in_flight_global'], l: '国际版在途上限', type: 'number', min: 1, ph: '2',
      tip: 'global 域风控更紧，默认比国内低' },
    { n: 'ttl', p: ['session_sticky', 'ttl'], l: '会话粘性 TTL', dur: true, ph: '30m', tip: '改完要重启进程' },
    { n: 'soft_rate', p: ['cooldown', 'soft_rate'], l: '软限流冷却基数', dur: true, ph: '600s', adv: true,
      tip: '撞上限流后第一次要等多久，之后按次数退避' },
    { n: 'soft_rate_max', p: ['cooldown', 'soft_rate_max'], l: '软冷却退避上限', dur: true, ph: '2h', adv: true },
    { n: 'breaker_threshold', p: ['pool', 'breaker_threshold'], l: '连续失败熔断阈值', type: 'number', min: 1, ph: '3', adv: true,
      tip: '连败到这个数就把账号暂时摘出选号' },
    { n: 'breaker_cooldown', p: ['pool', 'breaker_cooldown'], l: '熔断基础时长', dur: true, ph: '30m', adv: true },
    { n: 'breaker_cooldown_max', p: ['pool', 'breaker_cooldown_max'], l: '熔断退避上限', dur: true, ph: '6h', adv: true },
    { n: 'degrade_threshold', p: ['pool', 'degrade_threshold'], l: '连败降权阈值', type: 'number', min: 1, ph: '5', adv: true },
    { n: 'degrade_cooldown', p: ['pool', 'degrade_cooldown'], l: '连败降权时长', dur: true, ph: '10m', adv: true },
    { n: 'degrade_cooldown_max', p: ['pool', 'degrade_cooldown_max'], l: '连败降权上限', dur: true, ph: '2h', adv: true },
    { n: 'idle_weight_per_hour', p: ['pool', 'idle_weight_per_hour'], l: '闲置补偿 / 小时', type: 'number', step: '0.1', ph: '0.5', adv: true },
    { n: 'idle_weight_max', p: ['pool', 'idle_weight_max'], l: '闲置补偿上限', type: 'number', step: '0.1', ph: '5', adv: true },
    { n: 'cost_explore_interval', p: ['pool', 'cost_explore_interval'], l: '成本探索窗口', dur: true, ph: '30m', adv: true,
      tip: '免费层垄断时定期搭车探索未知号；0 关停' },
  ] },
  { group: '上游与高级', cols: 3, items: [
    { n: 'user_agent', p: ['upstream', 'user_agent'], l: '出站 User-Agent', ph: '留空 = 默认 CLI 标识',
      tip: '影响官网「使用端」显示；改完要重启', span: 2 },
    { n: 'sanitize_blacklist_fingerprints', p: ['features', 'sanitize_blacklist_fingerprints'], l: '出站请求指纹脱敏', sw: true,
      tip: '去掉上游用来判客户端形态的指纹头' },
    { n: 'session_sticky_enabled', p: ['session_sticky', 'enabled'], l: '会话粘性路由', sw: true,
      tip: '同一会话固定落回同一个账号；关掉后每次重新选号' },
    { n: 'timeout_seconds', p: ['upstream', 'timeout_seconds'], l: '短请求超时（秒）', type: 'number', min: 1, ph: '120',
      tip: '改完要重启', adv: true },
    { n: 'header_timeout_seconds', p: ['upstream', 'header_timeout_seconds'], l: '聊天首字节超时（秒）', type: 'number', min: 1, ph: '120',
      tip: '改完要重启', adv: true },
    { n: 'idle_timeout_seconds', p: ['upstream', 'idle_timeout_seconds'], l: '流空闲超时（秒）', type: 'number', min: 1, ph: '300',
      tip: '改完要重启', adv: true },
    { n: 'prompt_mode', p: ['prompt', 'mode'], l: '系统提示词模式', tip: '改完要重启', adv: true,
      options: [['custom', 'custom — 网关自有提示词'], ['append', 'append — 客户端 system 后追加'], ['passthrough', 'passthrough — 透传原始 system']] },
    { n: 'prompt_file', p: ['prompt', 'file'], l: '提示词文件路径', ph: '留空 = 内置默认', tip: '改完要重启', adv: true },
  ] },
];

const UNITS = [['s', '秒'], ['m', '分钟'], ['h', '小时']];
// 单个「数字 + 单位」才用下拉对；1h30m 这类复合值仍按原样文本处理——
// 不能为了套控件把人已有的写法改坏。
const DUR_ONE = /^(\d+(?:\.\d+)?)(s|m|h)$/;
const DUR_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DUR_TIP = 'Go 时长格式：30m / 2h / 600s / 1h30m';
const inputs = new Map();

const dig = (obj, path) => path.reduce((o, k) => (o == null ? undefined : o[k]), obj);
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) {
    if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {};
    o = o[path[i]];
  }
  o[path[path.length - 1]] = val;
}

const pathOf = item => item.p.join('.');
const needRestart = item => restartList.peek().includes(pathOf(item));

/** shown —— 这一项在搜索结果里吗（名字、用途、字段路径都能搜）。 */
function shown(item) {
  const s = q.peek().trim().toLowerCase();
  if (!s) return true;
  return (item.l + ' ' + (item.tip || '') + ' ' + pathOf(item) + ' ' + item.n).toLowerCase().includes(s);
}

async function load(quiet = true) {
  try {
    const d = await api('config');
    cfgPath.set(d.path || '');
    cfg.set(d.config || {});
    defs.set(d.defaults || null);
    restartList.set(d.restart_fields || []);
    // 重新拉配置 = 基线换了一份：旧的那棵树的输入元素必须一起丢掉。留着的话，
    // 下一轮建表时 curVal 会先从旧元素读回用户刚放弃的值（「放弃修改」白点，
    // 保存后那一行也永远挂着「已改动」的小点）。
    inputs.clear();
    edits.set(new Map());
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

/* ── 值：基线（cfg）+ 未保存的改动（edits）────────────────────── */

/** baseVal —— 配置文件里的值，界面形状（比较与初始值都走这里）。 */
function baseVal(item) {
  const v = dig(cfg() || {}, item.p);
  if (item.sw) return v ? '1' : '0';
  if (Array.isArray(v)) return v.join(', ');
  return v == null ? '' : String(v);
}

/** curVal —— 现在界面上的值（没改过就等于基线）。时长拼回「数字+单位」的形状，
 *  这样它和 baseVal、和 defaults 里的值才是同一种东西，能直接比。 */
function curVal(item) {
  const e = edits.peek();
  if (e.has(item.n)) return e.get(item.n);
  const el = inputs.get(item.n);
  if (!el) return baseVal(item);
  if (item.sw) return el.checked ? '1' : '0';
  if (item.dur && el._unit) {
    const raw = String(el.value ?? '').trim();
    return raw === '' ? '' : raw + el._unit.value;
  }
  return String(el.value ?? '').trim();
}

function changedItems() {
  const out = [];
  for (const g of FIELDS) for (const item of g.items) {
    if (!inputs.has(item.n)) continue;
    if (curVal(item) !== baseVal(item)) out.push(item);
  }
  return out;
}

/* ── 保存栏与改动小点（清单 33）────────────────────────────────── */
let barEl = null;
let barOn = false;

function refreshBar() {
  const ch = changedItems();
  const need = ch.filter(needRestart).length;
  if (barEl) {
    barEl.textContent = ch.length
      ? `${ch.length} 项改动` + (need ? `，其中 ${need} 项要重启进程才生效` : '，保存即生效')
      : '没有改动';
    barEl.classList.toggle('dirty', ch.length > 0);
  }
  for (const g of FIELDS) for (const item of g.items) {
    if (!item._wrap) continue;
    item._wrap.classList.toggle('changed', inputs.has(item.n) && curVal(item) !== baseVal(item));
  }
  // 有没保存的东西时，关掉标签/刷新前拦一道
  if (ch.length && !barOn) { window.addEventListener('beforeunload', warnLeave); barOn = true; }
  else if (!ch.length && barOn) { window.removeEventListener('beforeunload', warnLeave); barOn = false; }
}

function warnLeave(ev) {
  ev.preventDefault();
  ev.returnValue = '配置还没保存';
}

/* ── 搜索：就地显隐，不重建表单（重建会把没保存的输入冲掉）────── */
const nodes = [];      // { item, wrap, card, advBox }
const cards = [];      // { card, nav, items }

function filterNow() {
  let any = false;
  for (const n of nodes) {
    const hit = shown(n.item);
    n.wrap.style.display = hit ? '' : 'none';
    if (hit) any = true;
  }
  for (const c of cards) {
    const vis = c.items.filter(it => shown(it));
    c.card.style.display = vis.length ? '' : 'none';
    c.nav.style.display = vis.length ? '' : 'none';
    // 搜到高级项就把那一组摊开，否则命中的东西藏在折叠里等于没搜到
    if (c.advBox) c.advBox.open = vis.some(it => it.adv) && q.peek().trim() !== '';
  }
  if (emptyEl) emptyEl.style.display = any ? 'none' : '';
}

let emptyEl = null;

/* ── 单个字段 ─────────────────────────────────────────────────── */
function field(item) {
  // 初值取「当前界面上的值」（含没保存的改动），不是配置文件里的值：
  // 恢复默认、搜索后重画这一项时，用户刚填的东西不能被打回原状。
  const v = curVal(item);
  let input;
  let control;

  if (item.sw) {
    input = h('input', { type: 'checkbox', checked: v === '1' });
    inputs.set(item.n, input);
    // 清单 31：左边写用途，右边才是开关，下面一行小字——不再是一片无字的勾选框
    const wrap = h('div', { class: 'switch-row' },
      h('div', { class: 'grow' },
        h('div', { class: 'lb' }, item.l, needRestart(item) ? h('span', { class: 'chip faint', text: '要重启' }) : null),
        item.tip ? h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '3px' }, text: item.tip }) : null,
      ),
      h('div', { class: 'row', style: { gap: '6px', flex: 'none' } }, resetBtn(item), input),
    );
    input.addEventListener('change', refreshBar);
    item._wrap = wrap;
    return wrap;
  }

  const label = h('span', { class: 'label' }, item.l,
    needRestart(item) ? h('span', { class: 'chip faint', text: '要重启' }) : null);

  if (item.options) {
    input = h('select', { class: 'input' },
      ...item.options.map(([val, t]) => h('option', { value: val, selected: val === v, text: t })));
    control = input;
  } else if (item.dur && (v === '' || DUR_ONE.test(v))) {
    const m = DUR_ONE.exec(v);
    input = h('input', { class: 'input', type: 'number', min: 0, step: 'any',
      value: m ? m[1] : '', placeholder: item.ph || '', style: { flex: '1' } });
    input._unit = h('select', { class: 'input', style: { width: '86px', flex: 'none' } },
      ...UNITS.map(([val, t]) => h('option', { value: val, selected: val === (m ? m[2] : 'm'), text: t })));
    control = h('div', { class: 'row', style: { gap: '6px' } }, input, input._unit);
  } else {
    input = h('input', {
      class: 'input',
      type: item.secret ? 'password' : item.type === 'number' ? 'number' : 'text',
      min: item.min, step: item.step, placeholder: item.ph || '',
      value: v,
    });
    control = item.secret
      ? h('div', { class: 'row' }, input, h('button', {
        class: 'btn sm', type: 'button',
        onclick: ev => {
          const show = input.type === 'password';
          input.type = show ? 'text' : 'password';
          ev.currentTarget.textContent = show ? '隐藏' : '显示';
        },
      }, '显示'))
      : input;
  }
  inputs.set(item.n, input);

  if (item.dur) {
    const mark = () => {
      const raw = input.value.trim();
      const val = raw === '' ? '' : raw + (input._unit ? input._unit.value : '');
      const bad = val !== '' && !DUR_RE.test(val);
      input.classList.toggle('bad', bad);
      input.title = bad ? DUR_TIP : '';
    };
    input.addEventListener('input', () => { mark(); edits.peek().delete(item.n); refreshBar(); });
    if (input._unit) input._unit.addEventListener('change', () => { mark(); refreshBar(); });
    mark();
  } else {
    const onIn = () => {
      const e = new Map(edits.peek());
      e.set(item.n, String(input.value ?? '').trim());
      edits.set(e);
      refreshBar();
    };
    input.addEventListener('input', onIn);
    input.addEventListener('change', onIn);
  }

  const wrap = h('label', {
    class: 'field',
    style: item.span === 2 ? { gridColumn: 'span 2' } : null,
  }, label, h('div', { class: 'row', style: { gap: '6px' } }, control, resetBtn(item)),
    item.tip ? h('span', { class: 'tip', text: item.tip }) : null);
  item._wrap = wrap;
  return wrap;
}

/** resetBtn —— 「恢复默认」（清单 34）。默认值来自后端 Default()；
 *  已经等于默认值时不出现，免得满屏都是没人点的按钮。 */
function resetBtn(item) {
  const d = defs();
  if (!d) return null;
  const dv = dig(d, item.p);
  const want = item.sw ? (dv ? '1' : '0') : (Array.isArray(dv) ? dv.join(', ') : (dv == null ? '' : String(dv)));
  if (want === baseVal(item) && !edits.peek().has(item.n)) return null;
  if (curVal(item) === want) return null;
  return h('button', {
    class: 'btn sm ghost', type: 'button',
    title: `默认值：${want === '' ? '（空）' : want}`,
    onclick: ev => {
      ev.preventDefault();
      applyVal(item, want);
      refreshBar();
      const w = item._wrap;
      if (w) w.replaceWith(field(item));   // 重画这一项：该消失的按钮要消失
    },
  }, '恢复默认');
}

/** applyVal —— 把值写回控件（恢复默认用；时长拆成数字 + 单位）。 */
function applyVal(item, want) {
  const el = inputs.get(item.n);
  if (!el) return;
  const e = new Map(edits.peek());
  if (item.sw) { el.checked = want === '1'; e.set(item.n, want); edits.set(e); return; }
  if (item.dur && el._unit) {
    const m = DUR_ONE.exec(want);
    if (m) { el.value = m[1]; el._unit.value = m[2]; }
    else { el.value = want; }
    e.set(item.n, want);
  } else {
    el.value = want;
    e.set(item.n, want);
  }
  edits.set(e);
}

/* ── 分组：日常项露着，高级项折叠（清单 35）───────────────────── */
function section(g, idx) {
  const plain = g.items.filter(it => !it.adv);
  const adv = g.items.filter(it => it.adv);
  const wrapEls = [];
  const card = h('section', { class: 'card', id: 'cfg-g-' + idx },
    h('header', null, h('h2', { text: g.group })),
    h('div', { class: 'body stack' }));
  const body = card.children[1];

  const grid = list => {
    const gEl = h('div', { class: g.cols === 3 ? 'grid-3' : 'grid-2' });
    for (const it of list) { const f = field(it); gEl.append(f); wrapEls.push(f); nodes.push({ item: it, wrap: f }); }
    return gEl;
  };

  let advBox = null;
  if (plain.length) body.append(grid(plain));
  if (adv.length) {
    advBox = h('details', { class: 'adv' },
      h('summary', { text: `高级（${adv.length} 项：熔断 / 降权 / 探索窗口 / 超时等）` }),
      grid(adv));
    body.append(advBox);
  }
  cards.push({ card, items: g.items, advBox, nav: null });
  return card;
}

/** nav —— 左侧吸附小目录（清单 30）：四十来项配置靠滚是找不到的。 */
function navCol() {
  const el = h('nav', { class: 'cfg-nav' });
  cards.forEach((c, i) => {
    const b = h('button', {
      class: 'chip ghost', type: 'button', text: FIELDS[i].group,
      onclick: () => {
        const target = document.getElementById('cfg-g-' + i);
        if (target) target.scrollIntoView({ behavior: 'smooth', block: 'start' });
      },
    });
    c.nav = b;
    el.append(b);
  });
  return el;
}

function collect() {
  const out = {};
  for (const g of FIELDS) for (const item of g.items) {
    const el = inputs.get(item.n);
    if (!el) continue;
    let v;
    if (item.sw) v = el.checked;
    else if (item.options) v = el.value;
    else if (item.dur) {
      const raw = el.value.trim();
      v = raw === '' ? undefined : raw + (el._unit ? el._unit.value : '');
    } else {
      const raw = String(el.value ?? '').trim();
      v = raw === '' ? undefined : item.type === 'number' ? Number(raw)
        : item.hours ? raw.split(/[,，\s]+/).filter(Boolean).map(Number) : raw;
    }
    if (v !== undefined && v !== '') put(out, item.p, v);
    else if (item.sw) put(out, item.p, false);
  }
  return out;
}

async function save(ev) {
  ev.preventDefault();
  for (const g of FIELDS) for (const item of g.items) {
    if (!item.dur) continue;
    const el = inputs.get(item.n);
    if (!el) continue;
    const raw = el.value.trim();
    const v = raw === '' ? '' : raw + (el._unit ? el._unit.value : '');
    if (v && !DUR_RE.test(v)) {
      el.focus();
      toast(`「${item.l}」${DUR_TIP}`, 'fail');
      return;
    }
  }
  const ch = changedItems();
  if (!ch.length) { toast('没有要保存的改动'); return; }
  // 后端回执给的是「这个进程里哪些字段装配期定死」的完整清单，不是「你刚才改的那几项
  // 里哪些要重启」。直接拿它的条数说话会出现只改了个热生效开关却被告知「9 项要重启」，
  // 并递上一颗会让人无谓重启网关的按钮——所以要和本次真实改动求交集。
  const changedPaths = new Set(ch.map(pathOf));
  saving.set(true);
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collect()) });
    const need = (r.restart_required || []).filter(p => changedPaths.has(p));
    justSaved.set(need);
    toast(need.length
      ? `已保存 ${ch.length} 项，其中 ${need.length} 项要重启进程才生效`
      : `已保存 ${ch.length} 项，立即生效`);
    const k = inputs.get('api_key');
    if (k && k.value.trim()) { try { localStorage.setItem('buddyhub.key', k.value.trim()); } catch { /* 私密模式 */ } }
    await load();
  } catch (e) { toast('保存失败：' + e.message, 'fail'); }
  finally { saving.set(false); refreshBar(); }
}

/** doRestart —— 保存栏上那颗「立即重启」：会断几秒，所以先说清代价再问。 */
async function doRestart() {
  if (!await confirmDialog(
    `重启网关进程，让那 ${justSaved.peek().length} 项改动生效。进程会中断几秒，正在进行的请求会断掉。`,
    { ok: '立即重启' })) return;
  try {
    await api('restart', { method: 'POST' });
    toast('正在重启…页面稍后自动刷新');
    setTimeout(() => location.reload(), 4000);
  } catch (e) { toast('重启请求没发出去：' + e.message, 'fail'); }
}

/** appearance —— 主题（清单 53：原先占着顶栏，现在挖进设置）。
 *  刻意不读 theme 信号：一订阅，切主题就会重建整页表单，
 *  用户刚填进去还没保存的值会被 cfg 里的旧值冲掉。改由点击就地换 class。 */
function appearance() {
  const btns = [['dark', '暗色'], ['light', '浅色']].map(([v, label]) => {
    const b = h('button', {
      type: 'button', class: theme.peek() === v ? 'on' : '', text: label,
      onclick: () => { setTheme(v); for (const x of btns) x.classList.toggle('on', x === b); },
    });
    return b;
  });
  return h('section', { class: 'card' },
    h('header', null, h('h2', { text: '外观' })),
    h('div', { class: 'body' },
      h('div', { class: 'switch-row' },
        h('div', null,
          h('div', { class: 'lb', text: '主题' }),
          h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '3px' },
            text: '只改这台浏览器的面板观感，存在本地，不写进配置文件。' })),
        h('div', { class: 'seg' }, ...btns),
      ),
    ),
  );
}

export default defineView({
  id: 'config',
  page: 'settings',
  tab: '配置',
  title: '配置',
  icon: 'config',
  keywords: '配置 设置 config 参数',
  sub() { return cfgPath() ? cfgPath() : '读取配置…'; },
  render() {
    if (!cfg() && !cfgPath()) load();
    const data = cfg();
    if (!data) return h('div', { class: 'view' }, h('div', { class: 'busy', text: '读取配置' }));

    nodes.length = 0;
    cards.length = 0;

    const search = h('input', {
      class: 'input search', type: 'search', id: 'cfg-q',
      placeholder: '搜配置项（名字 / 用途 / 字段名）',
      value: q.peek(),
      oninput: () => { q.set(search.value); filterNow(); },
      onkeydown: ev => {
        if (ev.key !== 'Escape') return;
        search.value = '';
        q.set('');
        filterNow();
      },
    });

    const secs = FIELDS.map(section);
    const nav = navCol();
    emptyEl = h('div', { class: 'empty' }, icon('config'),
      h('div', { class: 't', text: `没有匹配「${q.peek()}」的配置项` }),
      h('div', { class: 'd', text: '配置项的名字、用途说明和字段路径都能搜。' }));
    emptyEl.style.display = 'none';

    barEl = h('span', { class: 'note' });
    const restartBtn = justSaved.peek().length
      ? h('button', { class: 'btn primary', type: 'button', onclick: doRestart }, icon('refresh'), '立即重启')
      : null;

    const form = h('form', { class: 'view stack', onsubmit: save },
      h('div', { class: 'row' }, search),
      emptyEl,
      h('div', { class: 'cfg-layout' },
        nav,
        h('div', { class: 'stack' }, ...secs, appearance()),
      ),
      h('div', { class: 'savebar' },
        barEl,
        h('span', { class: 'note faint', text: 'Redis 镜像、凭证目录与状态文件路径需手工编辑配置文件' }),
        h('span', { class: 'grow' }),
        restartBtn,
        h('button', { class: 'btn', type: 'button', onclick: () => { justSaved.set([]); load(false).then(refreshBar); } },
          icon('refresh'), '放弃修改'),
        h('button', { class: 'btn primary', type: 'submit', disabled: saving() },
          icon('check'), saving() ? '保存中…' : '保存配置'),
      ),
    );
    refreshBar();
    return form;
  },
});
