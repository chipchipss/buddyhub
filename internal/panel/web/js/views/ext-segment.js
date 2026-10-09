/* ══════════════════════════════════════════════════════════════════
   views/ext-segment.js · 外部平台（extstore）数据源
   管理面已并入「账号」页的统一表格（rows.js 行模型 / acts.js 动作 /
   views/account-detail.js 详情）。这里只提供：
     1. 数据源信号 extData / loadExt
     2. extJsonImport —— 「粘贴完整凭据 JSON」的高级入池表单，由添加向导调用
   平台名与「有没有签到」仍来自注册表（platforms.js），这里不带任何名字表。
   ══════════════════════════════════════════════════════════════════ */

import { h, signal, api, toast } from '../kernel.js';
import { platforms, loadPlatforms, plat } from '../platforms.js';

const ext = signal(null);
const extMsg = signal('');

/** extData —— 外部平台数据源信号（{ok, accounts}）。 */
export const extData = ext;
export const extError = extMsg;

export async function loadExt(quiet = true) {
  try {
    await loadPlatforms();
    ext.set(await api('ext/accounts'));
    extMsg.set('');
  }
  catch (e) { extMsg.set(e.message); if (!quiet) toast(e.message, 'fail'); }
}

/** extJsonImport(onAdded, opts) —— 高级入口：直接粘贴客户端导出的凭据 JSON。
 *  与逐字段表单（views/ext-add.js）互为补充：客户端能整包导出时用这个更快。
 *  opts.provider —— 锁定平台（添加向导已经选好平台，这里不该再出第二个下拉；
 *  选中一个不走外部账号表的平台只会得到一句报错，不如根本不给他这个选项）。
 *  onAdded(ref) 落盘后调用，ref = {provider, id}。 */
export function extJsonImport(onAdded, opts = {}) {
  const locked = opts.provider || '';
  const providerSel = h('select', { class: 'input', style: { width: 'auto' } },
    ...platforms.peek().map(p => h('option', { value: p.id, text: p.name })));
  const idInput = h('input', { class: 'input', placeholder: '账号 ID', style: { flex: '1', minWidth: '140px' } });
  const credInput = h('input', {
    class: 'input', placeholder: '凭据 JSON',
    style: { flex: '2', minWidth: '200px', fontFamily: 'var(--mono)', fontSize: '11.5px' },
  });
  return h('div', { class: 'row wrap', style: { gap: '8px' } },
    // 锁定平台时用一个静态标签代替下拉：向导第一步已经选好平台，多一个下拉
    // 只是让人有机会选到一个「不走外部账号表」的平台上然后收一句报错。
    locked ? h('span', { class: 'chip faint', text: (plat(locked) || {}).name || locked }) : providerSel,
    idInput, credInput,
    h('button', {
      class: 'btn sm', onclick: async ev => {
        const provider = locked || providerSel.value;
        const id = idInput.value.trim();
        if (!id) { toast('请填写账号 ID', 'fail'); return; }
        let cred;
        try { cred = JSON.parse(credInput.value.trim()); } catch { toast('凭据不是合法 JSON', 'fail'); return; }
        ev.currentTarget.disabled = true;
        try {
          await api('ext/accounts', { method: 'POST', body: JSON.stringify({ provider, id, cred }) });
          idInput.value = ''; credInput.value = '';
          await loadExt();
          await onAdded?.({ provider, id });
        } catch (e) { toast(e.message, 'fail'); }
        finally { ev.currentTarget.disabled = false; }
      },
    }, '添加'),
  );
}
