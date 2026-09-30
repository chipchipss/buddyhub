/* ══════════════════════════════════════════════════════════════════
   views/ext-add.js · 外部平台「添加账号」表单
   每个平台按自己的凭据形态出字段（而不是让用户手写 JSON）。

   字段集来自各 provider 的 Credential 结构（internal/extprovider/*）：
     LobsterAI（有道） access_token + uuid（+ refresh_token / first_key_from）
     小浣熊（商汤）     access_token（+ refresh_token）
     Qoder（阿里）      access_token + machine_id（+ refresh_token / security_oauth_token）
    华为云 CodeArts    access_key_id + secret_access_key + security_token

   实现是命令式的：输入框非受控（值在 DOM 里），切换平台只重绘字段区，
   不会清空已填内容；提交时按当前平台收集。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, api, toast } from '../kernel.js';

const PROVIDERS = {
  lobsterai: {
    name: 'LobsterAI（有道）',
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'uuid', label: 'UUID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'first_key_from', label: 'First Key From' },
    ],
    tip: '从有道 LobsterAI 客户端的本地凭据文件复制；access_token 与 uuid 必填。',
  },
  raccoon: {
    name: '小浣熊（商汤）',
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
    ],
    tip: '商汤小浣熊的登录令牌；access_token 必填。',
  },
  qoder: {
    name: 'Qoder（阿里）',
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'machine_id', label: 'Machine ID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'security_oauth_token', label: 'Security OAuth Token' },
    ],
    tip: 'Qoder 客户端凭据；access_token 与 machine_id 必填（machine_id 缺失会导致上游拒绝）。',
  },
  codearts: {
    name: 'CodeArts（华为云）',
    fields: [
      { k: 'access_key_id', label: 'Access Key ID', required: true },
      { k: 'secret_access_key', label: 'Secret Access Key', required: true },
      { k: 'security_token', label: 'Security Token', required: true },
    ],
    tip: '华为云临时 AK/SK（含 security_token）；三项都必填。',
  },
};

/** extAddPanel(onAdded) —— 返回一个命令式的添加面板节点。 */
export function extAddPanel(onAdded) {
  let provider = 'lobsterai';

  const idInput = h('input', {
    class: 'input', placeholder: '账号标识（昵称 / 手机尾号 / uid，用于列表区分）',
    style: { flex: '1', minWidth: '200px' },
  });
  const fieldsBox = h('div', { class: 'stack', style: { gap: '8px', marginTop: '10px' } });
  const tipEl = h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '8px' } });
  const inputs = new Map();

  function paintFields() {
    inputs.clear();
    const spec = PROVIDERS[provider];
    fieldsBox.replaceChildren(
      ...spec.fields.map(f => {
        const input = h('input', {
          class: 'input', placeholder: f.label + (f.required ? '（必填）' : '（可选）'),
          style: { fontFamily: 'var(--mono)', fontSize: '12px' },
        });
        inputs.set(f.k, input);
        return input;
      }),
    );
    tipEl.textContent = spec.tip;
  }

  const providerSel = h('select', {
    class: 'input', style: { width: 'auto' },
    onchange: ev => { provider = ev.target.value; paintFields(); },
  }, ...Object.entries(PROVIDERS).map(([v, p]) => h('option', { value: v, text: p.name })));

  const submit = h('button', {
    class: 'btn primary',
    onclick: async ev => {
      const id = idInput.value.trim();
      if (!id) { toast('请填写账号标识', 'fail'); idInput.focus(); return; }
      const cred = {};
      for (const f of PROVIDERS[provider].fields) {
        const v = (inputs.get(f.k)?.value || '').trim();
        if (!v) {
          if (f.required) { toast(`「${f.label}」必填`, 'fail'); inputs.get(f.k)?.focus(); return; }
          continue; // 可选字段留空不发送
        }
        cred[f.k] = v;
      }
      ev.currentTarget.disabled = true;
      try {
        await api('ext/accounts', { method: 'POST', body: JSON.stringify({ provider, id, cred }) });
        toast('账号已添加');
        idInput.value = '';
        for (const el of inputs.values()) el.value = '';
        await onAdded?.();
      } catch (e) { toast(e.message, 'fail'); }
      finally { ev.currentTarget.disabled = false; }
    },
  }, icon('plus'), '添加');

  paintFields();

  return h('details', { class: 'glass-flat', style: { padding: '12px 14px', marginTop: '10px' } },
    h('summary', { style: { cursor: 'pointer', fontSize: '12.5px', fontWeight: '550' },
      text: '添加外部平台账号（逐字段填写）' }),
    h('div', { class: 'row wrap', style: { gap: '8px', marginTop: '12px' } }, providerSel, idInput),
    fieldsBox,
    tipEl,
    h('div', { class: 'row', style: { marginTop: '12px' } }, submit),
  );
}
