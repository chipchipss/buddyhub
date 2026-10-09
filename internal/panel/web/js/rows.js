/* ══════════════════════════════════════════════════════════════════
   rows.js · 账号行的统一模型
   三个数据源（腾讯池 / Z.AI / 外部平台 extstore）的字段形态完全不同，
   「账号」页要把它排成**一张表**：同一套列（名称、状态、余额）、同一个词表
   （启用/停用，不再有「解冻」）、同一个 ⋯ 菜单结构。
   这里只做数据归一，不碰 DOM——列表渲染在 views/accounts.js，
   详情渲染在 views/account-detail.js，两边读的是同一批行对象。
   ══════════════════════════════════════════════════════════════════ */

import { dur } from './kernel.js';
import { statusOf } from './status.js';
import { plat, platName, platHasCheckin } from './platforms.js';

const key = (provider, id) => provider + ':' + id;

/* 凭据续期方式（注册表 renew 字段）：回答「我登陆了，为什么后来不能用」——
   自动续的到期网关自己换，需人工的到期只能再授权一次，界面上必须先说清是哪一类。 */
const RENEW_WORD = { auto: '凭据到期自动续期', manual: '凭据到期需人工重新授权', static: '凭据长期有效' };
export function renewWord(provider) {
  const p = plat(provider);
  return (p && RENEW_WORD[p.renew]) || '';
}

/** poolRow —— 腾讯 WorkBuddy 池账号。 */
function poolRow(a) {
  const st = statusOf(a);
  const total = a.credits_total || 0;
  const cooling = st.level === 'cool';
  return {
    key: key('workbuddy', a.uid),
    provider: 'workbuddy',
    platformLabel: platName('workbuddy'),
    id: a.uid,
    name: a.nickname || a.uid,
    sub: a.uid,
    st,
    off: !!a.disabled,
    bal: { remain: a.credits, total, pct: total > 0 ? pct(a.credits, total) : null, unit: '分' },
    cred: '凭证由网关自动续期（token 刷新在请求路径上完成）',
    tags: [a.realm === 'global' ? '国际版' : '国内版', a.in_flight ? `在途 ${a.in_flight}` : ''].filter(Boolean),
    can: { checkin: true, balance: true, enable: !!a.disabled, disable: !a.disabled, revive: cooling || !!a.disabled, tasks: true, remove: true },
    primary: a.disabled
      ? { kind: 'enable', label: '启用' }
      : { kind: 'checkin', label: '签到' },
    menu: [
      { kind: 'balance', label: '查余额' },
      cooling && !a.disabled ? { kind: 'revive', label: '强制恢复', tip: '清除冷却与熔断，立刻重新参与选号' } : null,
      a.disabled ? null : { kind: 'disable', label: '停用', tip: '停用后不再参与选号，需手动启用' },
      { kind: 'remove', label: '移除', danger: true, confirm: `移除账号「${a.nickname || a.uid}」会删除池状态与本地凭证文件，且不可恢复。` },
    ].filter(Boolean),
    raw: a,
  };
}

/** zaiRow —— Z.AI / 智谱账号（Plan JWT 与 API Key 回退通道）。 */
function zaiRow(a) {
  const st = statusOf(a);
  const q = a.quota || {};
  const quotaModels = Object.keys(q);
  let remain = 0, total = 0;
  for (const m of quotaModels) { remain += Number(q[m].remaining || 0); total += Number(q[m].total || 0); }
  const dead = a.status === 'invalid';
  return {
    key: key('zai', a.id),
    provider: 'zai',
    platformLabel: platName('zai'),
    id: a.id,
    name: a.name || a.id,
    sub: a.masked || a.id,
    st,
    off: a.enabled === false,
    bal: total > 0 ? { remain, total, pct: pct(remain, total), unit: '额度', exact: true } : null,
    cred: a.mode === 'jwt'
      ? 'Plan JWT：到期需重新粘贴或用 OAuth 免密登录'
      : 'API Key：长期有效，失效时重新生成即可',
    tags: [a.mode === 'jwt' ? 'Plan JWT' : 'API Key', a.has_key_fallback ? '带回退 Key' : ''].filter(Boolean),
    can: {
      quota: a.mode === 'jwt',
      claim: a.mode === 'jwt',
      rotate: true,
      enable: a.enabled === false,
      disable: a.enabled !== false,
      relogin: true,
      remove: true,
    },
    primary: dead
      ? { kind: 'relogin', label: '重新登录' }
      : a.enabled === false
        ? { kind: 'enable', label: '启用' }
        : a.mode === 'jwt' ? { kind: 'quota', label: '查额度' } : null,
    menu: [
      a.mode === 'jwt' ? { kind: 'claim', label: '领取套餐', tip: '需验证码求解器；上游 WAF 敏感，勿频繁点' } : null,
      { kind: 'rotate', label: '换设备指纹', tip: '换发整套桌面指纹（新 device_mid）' },
      a.enabled === false ? null : { kind: 'disable', label: '停用' },
      a.enabled === false ? { kind: 'enable', label: '启用' } : null,
      { kind: 'remove', label: '删除', danger: true, confirm: `删除 Z.AI 账号「${a.name || a.id}」？` },
    ].filter(Boolean),
    raw: a,
  };
}

/** extRow —— extstore 里的外部平台账号（Loomy / Qoder / Copilot / CodeArts / Trae …）。 */
function extRow(a) {
  const st = statusOf(a);
  const hasCheckin = platHasCheckin(a.provider);
  const manual = (plat(a.provider) || {}).renew === 'manual';
  return {
    key: key(a.provider, a.id),
    provider: a.provider,
    platformLabel: platName(a.provider),
    id: a.id,
    name: a.label || a.id,
    sub: a.id,
    st,
    off: !!a.disabled,
    // 没有签到概念的通道（Copilot / Cline / AutoClaw）不硬凑 0 分余额。
    bal: hasCheckin && a.balance_ok ? { remain: a.balance, total: 0, pct: null, unit: '分' } : null,
    cred: renewWord(a.provider),
    tags: [],
    can: { checkin: hasCheckin && !a.disabled, enable: !!a.disabled, disable: !a.disabled, relogin: manual, remove: true },
    primary: a.disabled
      ? { kind: 'enable', label: '启用' }
      : hasCheckin
        ? { kind: 'checkin', label: '签到' }
        : manual ? { kind: 'relogin', label: '重新授权' } : null,
    menu: [
      manual ? { kind: 'relogin', label: '重新授权' } : null,
      a.disabled ? null : { kind: 'disable', label: '停用' },
      a.disabled ? { kind: 'enable', label: '启用' } : null,
      { kind: 'remove', label: '删除', danger: true, confirm: `确定删除 ${platName(a.provider)} 账号 ${a.id}？` },
    ].filter(Boolean),
    raw: a,
  };
}

function pct(remain, total) {
  const r = Number(remain || 0), t = Number(total || 0);
  if (t <= 0) return null;
  return Math.max(0, Math.min(100, Math.round(r / t * 100)));
}

/** buildRows(pool, zai, ext) —— 三源合一，按「平台 → 名称」稳定排序。
 *  缺数据源（还没加载完 / 该平台没配）就跳过，不报错——列表先出能出的部分。 */
export function buildRows(pool, zai, ext) {
  const rows = [];
  for (const a of pool || []) rows.push(poolRow(a));
  for (const a of zai || []) rows.push(zaiRow(a));
  for (const a of ext || []) rows.push(extRow(a));
  rows.sort((x, y) => (x.provider === y.provider
    ? String(x.name).localeCompare(String(y.name))
    : x.provider < y.provider ? -1 : 1));
  return rows;
}

/** platformsOf(rows) —— 出现过的平台（chips 用），带每平台账号数。 */
export function platformsOf(rows) {
  const m = new Map();
  for (const r of rows) {
    const e = m.get(r.provider) || { provider: r.provider, name: r.platformLabel, n: 0, bad: 0 };
    e.n++;
    if (r.st.level !== 'ok') e.bad++;
    m.set(r.provider, e);
  }
  return [...m.values()];
}

/** bulkKind(row, op) —— 批量动作 op 落到这一行时该调的真实动作；不支持则 null。
 *  三个源的动词不一样（池查余额、Z.AI 查额度），批量条必须按行换算，
 *  否则会出现「选中 5 个账号，2 个报 404」。 */
export function bulkKind(row, op) {
  const can = row.can || {};
  if (op === 'enable') return can.enable ? 'enable' : null;
  if (op === 'disable') return can.disable ? 'disable' : null;
  if (op === 'checkin') return can.checkin ? 'checkin' : null;
  if (op === 'balance') return can.balance ? 'balance' : can.quota ? 'quota' : null;
  if (op === 'remove') return can.remove ? 'remove' : null;
  return null;
}

/** matches(r, provider, query) —— 芯片筛选 + 搜索（名称、平台、ID 都算）。 */
export function matches(r, provider, query) {
  if (provider !== 'all' && r.provider !== provider) return false;
  if (!query) return true;
  const q = query.toLowerCase();
  return String(r.name).toLowerCase().includes(q)
    || String(r.id).toLowerCase().includes(q)
    || String(r.platformLabel).toLowerCase().includes(q)
    || String(r.provider).toLowerCase().includes(q);
}

/** logMatcher(row) —— 该账号在日志文本里的标识。
 *  日志行没有结构化账号字段（只有 ts/ch/text），三种源各有各的写法：
 *    池：panel 侧 `uid=<全量>`，chat 流水 `昵称(<uid8>)`
 *    外部：桥接层统一 `acct=<id>`
 *    Z.AI：`账号=<显示名>`
 *  全都在后端文本里，前端只能按子串命中——所以宁多勿漏。 */
export function logMatcher(row) {
  if (row.provider === 'workbuddy') {
    const short = String(row.id).slice(0, 8);
    return t => t.includes('uid=' + row.id) || t.includes('(' + short + ')');
  }
  if (row.provider === 'zai') {
    const name = String(row.raw.name || row.id);
    return t => t.includes('账号=' + name) || t.includes(name);
  }
  return t => t.includes('acct=' + row.id) || t.includes(row.id);
}

/** 倒计时（状态自带下一步：冷却中的行要把「还剩多久」显示出来）。 */
export function coolText(row) {
  const s = Number(row.raw.cool_remaining_sec || row.raw.cooldown_sec || 0);
  const bl = row.raw.breaker_until ? (new Date(row.raw.breaker_until) - Date.now()) / 1000 : 0;
  const dg = row.raw.degrade_until ? (new Date(row.raw.degrade_until) - Date.now()) / 1000 : 0;
  const left = Math.max(s, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  return left > 0 ? dur(left) : '';
}
