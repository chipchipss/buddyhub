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

// 设备流轮询定时器（模块级：面板重绘/切换平台时统一清理，避免轮询泄漏）。
let deviceTimer = null;

/** stopExtAddTimers() —— 停掉进行中的设备流轮询（抽屉关闭 / 视图卸载时调用）。 */
export function stopExtAddTimers() {
  if (deviceTimer) { clearInterval(deviceTimer); deviceTimer = null; }
}

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
  copilot: {
    name: 'GitHub Copilot',
    // 设备流登录：无需手填凭据，浏览器授权即可（见 paintDevice）。
    device: true,
    tip: '点「开始授权」拿到设备码，在 GitHub 页面输入即可；授权后账号自动入池，'
      + '用 copilot:<模型名> 调用（如 copilot:gpt-4o）。',
  },
};

/** extAddPanel(onAdded, opts) —— 返回一个命令式的添加面板节点。
 *  opts.flat = true 时平铺（抽屉内嵌用），否则收进 <details>。 */
export function extAddPanel(onAdded, opts = {}) {
  let provider = 'lobsterai';

  // 设备流轮询定时器（模块级：面板重绘时先清掉上一个，避免轮询泄漏）。
  if (deviceTimer) { clearInterval(deviceTimer); deviceTimer = null; }

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
    // 设备流平台不需要手填标识与凭据，整行表单隐藏。
    idInput.hidden = !!spec.device;
    submit.hidden = !!spec.device;
    if (spec.device) { paintDevice(); return; }
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

  /* ── GitHub Copilot：设备流登录 ─────────────────────────────── */

  function paintDevice() {
    if (deviceTimer) { clearInterval(deviceTimer); deviceTimer = null; }
    inputs.clear();

    const codeEl = h('div', {
      style: {
        font: '600 26px var(--mono)', letterSpacing: '3px', userSelect: 'all',
        padding: '10px 0',
      },
    });
    const linkEl = h('a', { class: 'link', target: '_blank', rel: 'noreferrer' });
    const statusEl = h('div', { class: 'muted', style: { fontSize: '12px' } });
    const startBtn = h('button', { class: 'btn primary' }, icon('key'), '开始授权');

    const box = h('div', { class: 'stack', style: { gap: '6px', marginTop: '4px' } },
      startBtn, codeEl, linkEl, statusEl);
    fieldsBox.replaceChildren(box);
    tipEl.textContent = PROVIDERS.copilot.tip;

    startBtn.onclick = async () => {
      startBtn.disabled = true;
      if (deviceTimer) { clearInterval(deviceTimer); deviceTimer = null; }
      try {
        const d = await api('ext/copilot/start', { method: 'POST' });
        codeEl.textContent = d.user_code || '';
        linkEl.textContent = d.verification_uri || 'https://github.com/login/device';
        linkEl.href = d.verification_uri || 'https://github.com/login/device';
        statusEl.textContent = '在 GitHub 页面输入上方设备码并授权，本页会自动完成接入…';
        // 按 GitHub 下发的 interval 轮询（最少 5s，避免 slow_down）。
        const every = Math.max(5, d.interval || 5) * 1000;
        const deadline = Date.now() + (d.expires_in || 900) * 1000;
        deviceTimer = setInterval(async () => {
          if (Date.now() > deadline) {
            clearInterval(deviceTimer); deviceTimer = null;
            statusEl.textContent = '设备码已过期，请重新开始授权。';
            startBtn.disabled = false;
            return;
          }
          let r;
          try {
            r = await api('ext/copilot/poll', { method: 'POST', body: JSON.stringify({ session: d.session }) });
          } catch (e) {
            clearInterval(deviceTimer); deviceTimer = null;
            statusEl.textContent = e.message;
            startBtn.disabled = false;
            return;
          }
          if (!r.done) return;
          clearInterval(deviceTimer); deviceTimer = null;
          codeEl.textContent = '';
          linkEl.textContent = '';
          linkEl.removeAttribute('href');
          statusEl.textContent = `已接入：${(r.account || {}).label || (r.account || {}).id || 'Copilot 账号'}`;
          toast(`Copilot 账号已入池：${(r.account || {}).id || ''}`);
          await onAdded?.();
        }, every);
      } catch (e) {
        statusEl.textContent = e.message;
      } finally {
        startBtn.disabled = false;
      }
    };
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

  const inner = h('div', { class: 'stack' },
    h('div', { class: 'row wrap', style: { gap: '8px' } }, providerSel, idInput),
    fieldsBox,
    tipEl,
    h('div', { class: 'row', style: { marginTop: '12px' } }, submit),
  );

  // 嵌进抽屉时直接平铺（opts.flat），放在视图里时收进 <details> 免得占版面。
  if (opts.flat) {
    return h('div', { class: 'glass-flat', style: { padding: '12px 14px' } }, inner);
  }
  return h('details', { class: 'glass-flat', style: { padding: '12px 14px', marginTop: '10px' } },
    h('summary', { style: { cursor: 'pointer', fontSize: '12.5px', fontWeight: '550' },
      text: '添加外部平台账号（逐字段填写）' }),
    h('div', { style: { marginTop: '12px' } }, inner),
  );
}
