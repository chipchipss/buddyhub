/* ══════════════════════════════════════════════════════════════════
   status.js · 统一账号状态语言
   「这个账号能不能用」在四张卡与目录页各有一套表达（可用/冷却细分/启用停用/
   active-cooling），颜色与词汇不一致——用户要重新学每个页面。
   这里把口径收敛成一个函数 + 一个组件：

     statusOf(account) → { level: 'ok'|'cool'|'off', kind, label, tip }
       level  三档机器语义（颜色只由它决定）
       kind   细分原因（熔断/降权/限流/积分冷却/风控…），展示层可留可去
       label  中文文案（可被调用方覆盖）
       tip    悬浮说明（last_error / reason 之类）

     statusChip(st) → DOM   统一状态 chip：三档三色，i.dot 表状态点
   ══════════════════════════════════════════════════════════════════ */

import { h, dur } from './kernel.js';

/** 腾讯池形态：disabled / breaker_until / degrade_until / cool_remaining_sec / cool_kind / reason */
function ofWorkbuddy(a) {
  if (a.disabled) return { level: 'off', kind: 'disabled', label: '已禁用', tip: a.reason || '' };
  const bl = (new Date(a.breaker_until || 0) - Date.now()) / 1000;
  const dg = (new Date(a.degrade_until || 0) - Date.now()) / 1000;
  const cool = Math.max(a.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  if (cool > 0) {
    const kind = bl > Math.max(a.cool_remaining_sec || 0, dg) ? '熔断'
      : dg > (a.cool_remaining_sec || 0) ? '降权'
        : a.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却';
    return { level: 'cool', kind, label: `${kind} · ${dur(cool)}`, tip: a.reason || '' };
  }
  return { level: 'ok', kind: '', label: '可用', tip: '' };
}

/** 目录页形态：status ∈ healthy/cooling/off/unknown */
function ofDir(a) {
  const map = { healthy: ['ok', '可用'], cooling: ['cool', '冷却中'], off: ['off', '停用'], unknown: ['cool', '未知'] };
  const [level, label] = map[a.status] || ['cool', a.status || '未知'];
  return { level, kind: a.status || '', label, tip: a.detail || '' };
}

/** Z.AI 形态：enabled / status ∈ active/cooling/exhausted/invalid/disabled / cool_remaining_sec */
function ofZai(a) {
  if (a.enabled === false || a.status === 'disabled')
    return { level: 'off', kind: a.status || 'disabled', label: '已停用', tip: a.last_error || '' };
  const known = {
    active: ['ok', '可用'],
    cooling: ['cool', '冷却中'],
    exhausted: ['cool', '额度用完'],
    invalid: ['off', '凭证失效'],
  }[a.status] || ['cool', a.status || '未知'];
  const [level, label] = known;
  const suffix = a.cool_remaining_sec ? ` · ${dur(a.cool_remaining_sec)}` : '';
  return { level, kind: a.status || '', label: label + suffix, tip: a.last_error || '' };
}

/** 外部平台形态：disabled / cooldown_sec / fail_streak */
function ofExt(a) {
  if (a.disabled) return { level: 'off', kind: 'disabled', label: '已停用', tip: a.note || '' };
  if (a.cooldown_sec > 0)
    return { level: 'cool', kind: 'cooling', label: `冷却 · ${dur(a.cooldown_sec)}`, tip: `连续失败 ${a.fail_streak || 1} 次，恢复后重新参与轮换` };
  return { level: 'ok', kind: '', label: '启用中', tip: '' };
}

/**
 * statusOf(a) —— 按形态自动分派。各字段都以「有则优先」叠加：
 * 传目录条目（status ∈ healthy/cooling/off/unknown）走 ofDir；
 * 传外部平台条目（cooldown_sec/fail_streak）走 ofExt；
 * Z.AI 条目（enabled/status ∈ active/exhausted/invalid）走 ofZai；
 * 其余按腾讯池口径（disabled/breaker_until/cool_remaining_sec）。
 */
export function statusOf(a) {
  if (!a) return { level: 'cool', kind: '', label: '—', tip: '' };
  const DIR_STATUS = ['healthy', 'cooling', 'off', 'unknown'];
  if (typeof a.status === 'string' && DIR_STATUS.includes(a.status) && !('cooldown_sec' in a)) return ofDir(a);
  if ('cooldown_sec' in a) return ofExt(a);
  if ('enabled' in a || 'risk_strikes' in a) return ofZai(a);
  return ofWorkbuddy(a);
}

/** 三档 → dot 类名与 chip 强弱。颜色语言只在这里定义一次。 */
const LEVELS = {
  ok: { dot: 'dot', chip: ' strong' },
  cool: { dot: 'dot ring', chip: '' },
  off: { dot: 'dot off', chip: ' faint' },
};

/** statusChip(st) —— 统一状态 chip。st 来自 statusOf()。 */
export function statusChip(st) {
  const lv = LEVELS[st.level] || LEVELS.cool;
  return h('span', { class: 'chip' + lv.chip, title: st.tip || '' },
    h('i', { class: lv.dot }), st.label);
}
