/* ══════════════════════════════════════════════════════════════════
   platforms.js · 平台注册表（前端唯一事实源）
   平台清单由后端 GET /panel/api/platforms 下发，前端**不再自带任何名字表**。

   为什么：平台数上到两位数后，原先散在各处的 4 张名字表（账号目录 / API 密钥
   授权 / 自动化外部平台 / 添加账号）只要漏改一处，界面就会出现「少一个平台」
   或「多一个点了报错的按钮」。现在只有后端一张表，前端全部从它渲染。

   只加载一次并缓存；失败时返回空表而不是抛错——界面照常渲染，只是列表为空。
   ══════════════════════════════════════════════════════════════════ */

import { signal, api } from './kernel.js';

// 全部平台（{id,name,group,prefix,login,checkin,note}）
export const platforms = signal([]);
let loading = null;

/** loadPlatforms() —— 拉一次并缓存（并发调用合并成一次请求）。 */
export function loadPlatforms() {
  if (loading) return loading;
  loading = api('platforms')
    .then(d => {
      const list = (d && d.platforms) || [];
      platforms.set(list);
      return list;
    })
    .catch(() => {
      // 拉不到就留空表：界面渲染为空，不抛错——比整页崩掉好
      loading = null;
      return [];
    });
  return loading;
}

/** 展示名（未知 id 原样返回，界面不会出现空白）。 */
export function platName(id) {
  const p = platforms.peek().find(x => x.id === id);
  return p ? p.name : id;
}

/** 按 id 取平台元数据。 */
export function plat(id) {
  return platforms.peek().find(x => x.id === id) || null;
}

/** 某分组的平台（gateway / points / local）。 */
export function platGroup(group) {
  return platforms.peek().filter(p => p.group === group);
}

/** 有对话 API 前缀的平台（可出现在「模型名前缀」说明里）。 */
export function platWithPrefix() {
  return platforms.peek().filter(p => p.prefix);
}

/** 该平台是否有每日签到（决定外部平台卡片上出不出现「签到」按钮）。 */
export function platHasCheckin(id) {
  const p = plat(id);
  return !!(p && p.checkin);
}

/** 该平台是否支持通过「添加账号」入池（有 login 方式）。 */
export function platHasLogin(id) {
  const p = plat(id);
  return !!(p && p.login);
}
