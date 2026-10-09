/* ══════════════════════════════════════════════════════════════════
   views/config.js · 配置
   声明式字段表 → 表单；Go 时长字段就地校验（与后端 ParseDuration 同口径）。
   保存后热生效，需重启的项由后端回执列出。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast } from '../kernel.js';
import { defineView } from '../shell.js';
import { theme, setTheme } from '../store.js';

const cfg = signal(null);
const cfgPath = signal('');
const saving = signal(false);
const note = signal('');

/* 字段表：name → 路径 + 展示元信息 */
const FIELDS = [
  { group: '服务', cols: 2, items: [
    { n: 'listen', p: ['listen'], l: '监听地址', tip: '改动需重启进程', ph: ':7863' },
    { n: 'api_key', p: ['api_key'], l: 'API 密钥', tip: '立即生效（含面板自身）', ph: '留空 = 不鉴权', secret: true },
  ] },
  { group: '定时任务', cols: 2, items: [
    { n: 'checkin_enabled', p: ['schedule', 'checkin_enabled'], l: '自动签到', sw: true },
    { n: 'keepalive_enabled', p: ['schedule', 'keepalive_enabled'], l: 'Token 保活', sw: true },
    { n: 'checkin_hours', p: ['schedule', 'checkin_hours'], l: '签到时点（小时）', ph: '9, 21', hours: true },
    { n: 'keepalive_hours', p: ['schedule', 'keepalive_hours'], l: '保活时点（小时）', ph: '22', hours: true },
    { n: 'travel_enabled', p: ['schedule', 'travel_enabled'], l: '猫猫旅行', sw: true },
    { n: 'activity_enabled', p: ['schedule', 'activity_enabled'], l: '活跃上报', sw: true },
    { n: 'travel_hours', p: ['schedule', 'travel_hours'], l: '旅行时点（小时）', ph: '9, 21', hours: true, tip: '一趟派出 + 一趟领奖闭环' },
    { n: 'activity_hours', p: ['schedule', 'activity_hours'], l: '上报时点（小时）', ph: '10', hours: true, tip: '点亮连登 + 解锁领养前置' },
    { n: 'balance_refresh_enabled', p: ['schedule', 'balance_refresh_enabled'], l: '后台刷新余额', sw: true },
    { n: 'balance_refresh_minutes', p: ['schedule', 'balance_refresh_minutes'], l: '刷新间隔（分钟）', type: 'number', min: 1, ph: '5' },
    { n: 'growth_enabled', p: ['schedule', 'growth_enabled'], l: '成长任务自动执行', sw: true },
    { n: 'growth_hours', p: ['schedule', 'growth_hours'], l: '执行时点（小时）', ph: '1', hours: true, tip: '每日到点自动「扫描 + 执行全部待办」' },
  ] },
  { group: '账号池与流量治理', cols: 3, items: [
    { n: 'max_in_flight', p: ['pool', 'max_in_flight'], l: '单账号最大在途', type: 'number', min: 0, ph: '3', tip: '0 = 不限制' },
    { n: 'max_in_flight_global', p: ['pool', 'max_in_flight_global'], l: '国际版在途上限', type: 'number', min: 1, ph: '2', tip: 'global 域风控更紧' },
    { n: 'breaker_threshold', p: ['pool', 'breaker_threshold'], l: '连续失败熔断阈值', type: 'number', min: 1, ph: '3' },
    { n: 'soft_rate', p: ['cooldown', 'soft_rate'], l: '软限流冷却基数', ph: '600s', dur: true },
    { n: 'soft_rate_max', p: ['cooldown', 'soft_rate_max'], l: '软冷却退避上限', ph: '2h', dur: true },
    { n: 'breaker_cooldown', p: ['pool', 'breaker_cooldown'], l: '熔断基础时长', ph: '30m', dur: true },
    { n: 'breaker_cooldown_max', p: ['pool', 'breaker_cooldown_max'], l: '熔断退避上限', ph: '6h', dur: true },
    { n: 'degrade_threshold', p: ['pool', 'degrade_threshold'], l: '连败降权阈值', type: 'number', min: 1, ph: '5' },
    { n: 'degrade_cooldown', p: ['pool', 'degrade_cooldown'], l: '连败降权时长', ph: '10m', dur: true },
    { n: 'degrade_cooldown_max', p: ['pool', 'degrade_cooldown_max'], l: '连败降权上限', ph: '2h', dur: true },
    { n: 'idle_weight_per_hour', p: ['pool', 'idle_weight_per_hour'], l: '闲置补偿 / 小时', type: 'number', step: '0.1', ph: '0.5' },
    { n: 'idle_weight_max', p: ['pool', 'idle_weight_max'], l: '闲置补偿上限', type: 'number', step: '0.1', ph: '5' },
    { n: 'cost_explore_interval', p: ['pool', 'cost_explore_interval'], l: '成本探索窗口', ph: '30m', dur: true, tip: '免费层垄断时定期搭车探索未知号；0 关停' },
    { n: 'ttl', p: ['session_sticky', 'ttl'], l: '会话粘性 TTL', ph: '30m', dur: true, tip: '改动需重启进程' },
  ] },
  { group: '上游与高级', cols: 3, items: [
    { n: 'timeout_seconds', p: ['upstream', 'timeout_seconds'], l: '短请求超时', type: 'number', min: 1, ph: '120', tip: '需重启' },
    { n: 'header_timeout_seconds', p: ['upstream', 'header_timeout_seconds'], l: '聊天首字节超时', type: 'number', min: 1, ph: '120', tip: '需重启' },
    { n: 'idle_timeout_seconds', p: ['upstream', 'idle_timeout_seconds'], l: '流空闲超时', type: 'number', min: 1, ph: '300', tip: '需重启' },
    { n: 'user_agent', p: ['upstream', 'user_agent'], l: '出站 User-Agent', ph: '留空 = 默认 CLI 标识', tip: '影响官网「使用端」显示；需重启', span: 2 },
    { n: 'prompt_mode', p: ['prompt', 'mode'], l: '系统提示词模式', tip: '需重启', options: [['custom', 'custom — 网关自有提示词'], ['append', 'append — 客户端 system 后追加'], ['passthrough', 'passthrough — 透传原始 system']] },
    { n: 'prompt_file', p: ['prompt', 'file'], l: '提示词文件路径', ph: '留空 = 内置默认', tip: '需重启' },
    { n: 'sanitize_blacklist_fingerprints', p: ['features', 'sanitize_blacklist_fingerprints'], l: '出站请求指纹脱敏', sw: true },
    { n: 'session_sticky_enabled', p: ['session_sticky', 'enabled'], l: '会话粘性路由', sw: true },
  ] },
];

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

async function load(quiet = true) {
  try {
    const d = await api('config');
    cfgPath.set(d.path || '');
    note.set('');
    cfg.set(d.config || {});
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

function field(item) {
  const value = dig(cfg() || {}, item.p);

  if (item.sw) {
    const input = h('input', { type: 'checkbox', checked: !!value });
    inputs.set(item.n, input);
    return h('div', { class: 'switch-row' }, h('span', { class: 'lb', text: item.l }), input);
  }

  let input;
  if (item.options) {
    input = h('select', { class: 'input' },
      ...item.options.map(([v, t]) => h('option', { value: v, selected: v === value, text: t })));
  } else {
    input = h('input', {
      class: 'input',
      type: item.secret ? 'password' : item.type === 'number' ? 'number' : 'text',
      min: item.min, step: item.step, placeholder: item.ph || '',
      value: Array.isArray(value) ? value.join(', ') : (value == null ? '' : String(value)),
    });
  }
  inputs.set(item.n, input);

  if (item.dur) {
    const mark = () => {
      const v = input.value.trim();
      const bad = v !== '' && !DUR_RE.test(v);
      input.classList.toggle('bad', bad);
      input.title = bad ? DUR_TIP : '';
    };
    input.addEventListener('input', mark);
    mark();
  }

  return h('label', { class: 'field', style: item.span === 2 ? { gridColumn: 'span 2' } : null },
    h('span', { class: 'label', text: item.l }),
    item.secret ? h('div', { class: 'row' }, input, h('button', {
      class: 'btn sm', type: 'button',
      onclick: ev => {
        const show = input.type === 'password';
        input.type = show ? 'text' : 'password';
        ev.currentTarget.textContent = show ? '隐藏' : '显示';
      },
    }, '显示')) : input,
    item.tip ? h('span', { class: 'tip', text: item.tip }) : null,
  );
}

function collect() {
  const out = {};
  for (const g of FIELDS) for (const item of g.items) {
    const el = inputs.get(item.n);
    if (!el) continue;
    let v;
    if (item.sw) v = el.checked;
    else if (item.type === 'number') { const raw = el.value.trim(); v = raw === '' ? undefined : Number(raw); }
    else {
      const raw = el.value.trim();
      if (raw === '') v = undefined;
      else if (item.hours) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, item.p, v);
  }
  return out;
}

async function save(ev) {
  ev.preventDefault();
  // 时长脏值前置拦截
  for (const g of FIELDS) for (const item of g.items) {
    if (!item.dur) continue;
    const el = inputs.get(item.n);
    const v = el && el.value.trim();
    if (v && !DUR_RE.test(v)) {
      el.focus();
      toast(`「${item.l}」${DUR_TIP}`, 'fail');
      return;
    }
  }
  saving.set(true);
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collect()) });
    const n = (r.restart_required || []).length;
    toast(n ? `配置已保存，其中 ${n} 项需重启进程生效` : '配置已保存并立即生效');
    // 密钥可能已改：本次会话沿用新值，避免下次轮询 401
    const k = inputs.get('api_key');
    if (k && k.value.trim()) { try { localStorage.setItem('buddyhub.key', k.value.trim()); } catch { /* 私密模式 */ } }
    await load();
  } catch (e) { toast('保存失败：' + e.message, 'fail'); }
  finally { saving.set(false); }
}

/** appearance —— 主题开关（清单 53：原先占着顶栏，现在挖进设置）。
 *  刻意不读 theme 信号：这里一订阅，切主题就会重建整页表单，
 *  用户刚填进去、还没保存的配置值会被 cfg 里的旧值冲掉。改由点击就地换 class。 */
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

    return h('form', { class: 'view stack', onsubmit: save },
      ...FIELDS.map(g => h('section', { class: 'card' },
        h('header', null, h('h2', { text: g.group })),
        h('div', { class: 'body' },
          h('div', { class: g.cols === 3 ? 'grid-3' : 'grid-2' }, ...g.items.map(field)),
        ),
      )),
      appearance(),
      h('div', { class: 'savebar' },
        h('span', { class: 'note',
          text: 'Upstash Redis 镜像、凭证目录与状态文件路径需手工编辑配置文件' }),
        h('span', { class: 'grow' }),
        h('button', { class: 'btn', type: 'button', onclick: () => load(false) }, icon('refresh'), '放弃修改'),
        h('button', { class: 'btn primary', type: 'submit', disabled: saving() }, icon('check'), saving() ? '保存中…' : '保存配置'),
      ),
    );
  },
});
