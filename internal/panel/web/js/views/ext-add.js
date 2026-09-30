/* ══════════════════════════════════════════════════════════════════
   views/ext-add.js · 外部平台「添加账号入池」

   两条路径，优先走登录（不用去客户端里手抄 token）：

     小浣熊（商汤）  微信扫码   → 面板出二维码，手机扫码即入池
     Qoder（阿里）   设备授权   → 浏览器完成授权，面板轮询即入池
     GitHub Copilot  设备码     → 输入 GitHub 设备码即入池
     LobsterAI / CodeArts       → 无登录协议，逐字段填写凭据

   有登录方式的平台仍保留「手动填写凭据」折叠区作为兜底。

   字段集来自各 provider 的 Credential 结构（internal/extprovider/*）。
   实现是命令式的：输入框非受控（值在 DOM 里），切换平台只重绘主体区，
   不会清空已填内容；提交时按当前平台收集。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, api, toast, copyText } from '../kernel.js';
import { qrMatrix, qrSVG } from '../qr.js';

// 登录轮询定时器（模块级：面板重绘/切换平台时统一清理，避免轮询泄漏）。
let loginTimer = null;

/** stopExtAddTimers() —— 停掉进行中的登录轮询（抽屉关闭 / 视图卸载时调用）。 */
export function stopExtAddTimers() {
  if (loginTimer) { clearInterval(loginTimer); loginTimer = null; }
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
    tip: '从有道 LobsterAI 客户端的本地凭据复制；access_token 与 uuid 必填。',
  },
  raccoon: {
    name: '小浣熊（商汤）',
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
    ],
    // 微信扫码：面板出二维码，手机确认即入池（code 由网关生成，不依赖官方客户端）
    login: { kind: 'qr', button: '微信扫码登录', hint: '用微信扫一扫，在手机上确认登录' },
    tip: '推荐直接扫码；也可从客户端本地凭据复制 access_token 手工填写。',
  },
  qoder: {
    name: 'Qoder（阿里）',
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'machine_id', label: 'Machine ID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'security_oauth_token', label: 'Security OAuth Token' },
    ],
    // 设备授权：PKCE 授权 URL，浏览器完成登录后网关轮询取 token
    login: { kind: 'device', button: '浏览器授权登录', hint: '在浏览器打开链接并完成登录授权' },
    tip: '推荐直接授权登录；手工填写时 machine_id 必填（缺失会被上游拒绝）。',
  },
  codearts: {
    name: 'CodeArts（华为云）',
    fields: [
      { k: 'access_key_id', label: 'Access Key ID', required: true },
      { k: 'secret_access_key', label: 'Secret Access Key', required: true },
      { k: 'security_token', label: 'Security Token（仅临时 AK/SK 需要）' },
    ],
    tip: '华为云 AK/SK：永久密钥留空 Security Token 即可；临时 STS 凭据才需要填（几小时就过期，不建议入池）。',
  },
  copilot: {
    name: 'GitHub Copilot',
    // 设备码授权：无需手填凭据
    login: { kind: 'code', button: '开始授权', hint: '在 GitHub 页面输入设备码并授权' },
    tip: '点「开始授权」拿到设备码，在 GitHub 页面输入即可；授权后账号自动入池，'
      + '用 copilot:<模型名> 调用（如 copilot:gpt-4o）。',
  },
};

/** extAddPanel(onAdded, opts) —— 返回一个命令式的添加面板节点。
 *  opts.flat = true 时平铺（抽屉内嵌用），否则收进 <details>。 */
export function extAddPanel(onAdded, opts = {}) {
  let provider = 'lobsterai';

  // 面板重绘时先清掉上一个轮询，避免泄漏。
  stopExtAddTimers();

  const bodyBox = h('div', { class: 'stack', style: { gap: '10px', marginTop: '10px' } });
  const tipEl = h('div', { class: 'muted', style: { fontSize: '11.5px' } });
  const inputs = new Map();

  const providerSel = h('select', {
    class: 'input', style: { width: 'auto' },
    onchange: ev => { provider = ev.target.value; paint(); },
  }, ...Object.entries(PROVIDERS).map(([v, p]) => h('option', { value: v, text: p.name })));

  /* ── 手工填写凭据（所有平台的兜底路径） ─────────────────────── */

  function credForm() {
    const idInput = h('input', {
      class: 'input', placeholder: '账号标识（昵称 / uid，用于列表区分）',
      style: { flex: '1', minWidth: '180px' },
    });
    const rows = PROVIDERS[provider].fields.map(f => {
      const input = h('input', {
        class: 'input', placeholder: f.label + (f.required ? '（必填）' : '（可选）'),
        style: { fontFamily: 'var(--mono)', fontSize: '12px' },
      });
      inputs.set(f.k, input);
      return input;
    });
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
    return h('div', { class: 'stack', style: { gap: '8px' } },
      h('div', { class: 'row wrap', style: { gap: '8px' } }, idInput),
      ...rows,
      h('div', { class: 'row' }, submit),
    );
  }

  /* ── 登录入池（扫码 / 设备授权 / 设备码） ───────────────────── */

  function loginPanel(spec) {
    const host = h('div', { class: 'stack', style: { gap: '10px' } });
    const statusEl = h('div', { class: 'muted', style: { fontSize: '12px' }, text: spec.hint });
    const startBtn = h('button', { class: 'btn primary' }, icon('key'), spec.button);

    const stop = () => { stopExtAddTimers(); };

    startBtn.onclick = async () => {
      startBtn.disabled = true;
      stop();
      try {
        const d = await api(`ext/${provider}/login/start`, { method: 'POST' });
        renderChallenge(host, spec, d);
        statusEl.textContent = spec.hint + '（本页会自动完成入池）';
        const every = Math.max(2, d.interval || 2) * 1000;
        const deadline = Date.now() + (d.expires_in || 600) * 1000;
        loginTimer = setInterval(async () => {
          if (Date.now() > deadline) { stop(); statusEl.textContent = '登录已超时，请重新发起。'; startBtn.disabled = false; return; }
          let r;
          try {
            r = await api(`ext/${provider}/login/poll`, { method: 'POST', body: JSON.stringify({ session: d.session }) });
          } catch (e) {
            stop(); statusEl.textContent = e.message; startBtn.disabled = false; return;
          }
          if (!r.done) {
            if (r.status && r.status !== 'pending') statusEl.textContent = STATUS_WORDS[r.status] || r.status;
            return;
          }
          stop();
          host.replaceChildren();
          statusEl.textContent = `已入池：${(r.account || {}).label || (r.account || {}).id || ''}`;
          toast(`${PROVIDERS[provider].name} 账号已入池`);
          await onAdded?.();
        }, every);
      } catch (e) {
        statusEl.textContent = e.message;
      } finally {
        startBtn.disabled = false;
      }
    };

    return h('div', { class: 'stack', style: { gap: '10px' } }, startBtn, host, statusEl);
  }

  // 按登录方式渲染「待用户完成的那一步」
  function renderChallenge(host, spec, d) {
    if (spec.kind === 'qr') {
      // 离线生成二维码（CSP 下不引外链服务）。
      // 编码器上限 106 字节（版本 5 / ECC L），超了只能退化成可复制的链接。
      let svg = null;
      try { svg = qrSVG(qrMatrix(d.qr_url), 168); } catch { svg = null; }
      if (svg) {
        host.replaceChildren(
          h('div', { class: 'qr', style: { alignSelf: 'flex-start' } }, svg),
          h('div', { class: 'muted', style: { fontSize: '11.5px' }, text: '二维码仅用于本次登录，过期请重新发起。' }),
        );
        return;
      }
      host.replaceChildren(
        h('div', { class: 'muted', style: { fontSize: '11.5px' },
          text: '链接过长无法生成二维码，请复制后在手机浏览器打开完成登录。' }),
        h('div', { class: 'url-box', text: d.qr_url }),
        h('div', { class: 'row' },
          h('button', {
            class: 'btn sm', onclick: async ev => {
              try { await copyText(d.qr_url); toast('链接已复制'); }
              catch { toast('复制失败，请手动选择', 'fail'); }
            },
          }, '复制链接'),
        ),
      );
      return;
    }
    if (spec.kind === 'code') {
      host.replaceChildren(
        h('div', { style: { font: '600 26px var(--mono)', letterSpacing: '3px', userSelect: 'all' }, text: d.user_code || '' }),
        h('a', { class: 'link', href: d.verification_uri || 'https://github.com/login/device',
          target: '_blank', rel: 'noreferrer', text: d.verification_uri || 'https://github.com/login/device' }),
      );
      return;
    }
    // device：浏览器打开授权链接
    const url = d.auth_url || '';
    host.replaceChildren(
      h('div', { class: 'url-box', text: url }),
      h('div', { class: 'row wrap', style: { gap: '8px' } },
        h('button', { class: 'btn sm primary', onclick: () => window.open(url, '_blank') }, '在浏览器打开'),
      ),
    );
  }

  /* ── 平台切换：重绘主体区 ───────────────────────────────────── */

  function paint() {
    stopExtAddTimers();
    inputs.clear();
    const spec = PROVIDERS[provider];
    if (spec.login) {
      // 有登录方式：登录为主；无凭据字段的平台（Copilot）不出口填表单。
      const parts = [loginPanel(spec.login)];
      if (spec.fields && spec.fields.length) {
        parts.push(h('details', null,
          h('summary', { class: 'muted', style: { cursor: 'pointer', fontSize: '12px' }, text: '或手动填写凭据' }),
          h('div', { style: { marginTop: '10px' } }, credForm()),
        ));
      }
      bodyBox.replaceChildren(...parts);
    } else {
      bodyBox.replaceChildren(credForm());
    }
    tipEl.textContent = spec.tip;
  }

  paint();

  const inner = h('div', { class: 'stack' },
    h('div', { class: 'row wrap', style: { gap: '8px' } }, providerSel),
    bodyBox,
    tipEl,
  );

  if (opts.flat) return h('div', { class: 'glass-flat', style: { padding: '12px 14px' } }, inner);
  return h('details', { class: 'glass-flat', style: { padding: '12px 14px', marginTop: '10px' } },
    h('summary', { style: { cursor: 'pointer', fontSize: '12.5px', fontWeight: '550' },
      text: '添加外部平台账号（扫码 / 授权 / 逐字段填写）' }),
    h('div', { style: { marginTop: '12px' } }, inner),
  );
}

// 上游轮询状态的中文说明（小浣熊会回 logging 等中间态）
const STATUS_WORDS = { logging: '已扫码，等待手机确认…', pending: '等待扫码…' };
