/* ══════════════════════════════════════════════════════════════════
   acts.js · 逐账号动作的唯一入口
   行内按钮、⋯ 菜单、详情抽屉、批量条调的是同一批接口。原先每处各写一遍
   fetch + toast + 刷新，四处口径会漂（刷新了哪个信号、成功文案怎么说）。
   这里只负责「调用 + 让对应数据源重新加载」，**不弹 toast、不抛错**：
   单次点击由调用方 toast 结果，批量执行由调用方累计进度。
   ══════════════════════════════════════════════════════════════════ */

import { api } from './kernel.js';
import { refreshOverview } from './store.js';
import { loadZai } from './views/zai-segment.js';
import { loadExt } from './views/ext-segment.js';

/** 动作结果：{ok, msg, data}。msg 失败时是原因，成功时是可读回执。 */

async function post(path, body) {
  return api(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined });
}

/** poolAct(uid, path, body) —— 腾讯池动作。
 *  path ∈ checkin | balance | revive | disable | remove | tasks/*。 */
export async function poolAct(uid, path, body) {
  try {
    const d = await post(`accounts/${encodeURIComponent(uid)}/${path}`, body);
    await refreshOverview();
    return { ok: true, data: d, msg: poolMsg(path, d) };
  } catch (e) {
    return { ok: false, msg: e.message };
  }
}

function poolMsg(path, d) {
  if (path === 'checkin') return '签到完成' + (d.credits != null ? `，积分 ${d.credits}` : '');
  if (path === 'balance') return `余额已更新：${d.credits}`;
  if (path === 'revive') return '已恢复参与选号';
  if (path === 'disable') return '已停用';
  if (path === 'remove') return d.file_error ? '已移除（凭证文件删除失败）' : '已移除';
  return '已完成';
}

/** zaiAct(id, action, body) —— Z.AI 动作。action ∈ quota | claim | toggle | rotate | remove。 */
export async function zaiAct(id, action, body) {
  try {
    const d = await post(`zai/accounts/${encodeURIComponent(id)}/${action}`, body);
    await loadZai();
    let msg = '已完成';
    if (action === 'toggle') msg = d.enabled ? '已启用' : '已停用';
    else if (action === 'rotate') msg = '已换发设备指纹';
    else if (action === 'remove') msg = '已删除';
    else if (action === 'quota') msg = '额度已刷新';
    else if (action === 'claim') msg = d.message || '领取完成';
    return { ok: true, data: d, msg };
  } catch (e) {
    return { ok: false, msg: e.message };
  }
}

/** extAct(provider, id, action, body) —— 外部平台动作。action ∈ checkin | toggle | remove。 */
export async function extAct(provider, id, action, body) {
  try {
    const d = await post(`ext/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}/${action}`, body);
    await loadExt();
    let msg = '已完成';
    if (action === 'toggle') msg = d.disabled ? '已停用' : '已启用';
    else if (action === 'remove') msg = '已删除';
    else if (action === 'checkin') msg = (d.result && (d.result.message || d.result.kind)) || '签到完成';
    return { ok: true, data: d, msg };
  } catch (e) {
    return { ok: false, msg: e.message };
  }
}

/** run(kind, row) —— 按动作类型在对应数据源上执行（行内按钮、菜单、批量条共用）。
 *  kind ∈ checkin | balance | quota | claim | rotate | remove | enable | disable。
 *  「启用/停用」三家形态不同（池=revive+disable、Z.AI=单个翻转接口、外部=带 body 的
 *  toggle），在这里收敛成一个词，界面上才不会出现「解冻/启用/停用」三套说法。 */
export async function run(kind, row) {
  if (row.provider === 'workbuddy') {
    if (kind === 'enable') return poolAct(row.id, 'revive');
    return poolAct(row.id, kind);
  }
  // Z.AI 只有一个翻转端点：调用方负责只在状态不符时下发（批量条已按此过滤）。
  if (row.provider === 'zai') return zaiAct(row.id, kind);
  if (kind === 'enable') return extAct(row.provider, row.id, 'toggle', { disabled: false });
  if (kind === 'disable') return extAct(row.provider, row.id, 'toggle', { disabled: true });
  return extAct(row.provider, row.id, kind);
}
