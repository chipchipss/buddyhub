/* ══════════════════════════════════════════════════════════════════
   views/ext-add.js · 外部平台「添加账号入池」

   两条路径，优先走登录（不用去客户端里手抄 token）：

     小浣熊（商汤）  微信扫码   → 面板出二维码，手机扫码即入池
     Qoder（阿里）   设备授权   → 浏览器完成授权，面板轮询即入池
     GitHub Copilot  设备码     → 输入 GitHub 设备码即入池
     LobsterAI / CodeArts       → 无登录协议，逐字段填写凭据

   有登录方式的平台仍保留「手动填写凭据」折叠区作为兜底。

   **平台清单来自后端注册表**（js/platforms.js），这里只提供两样东西：
     1. 每个平台的凭据字段（纯 UI 细节，按 id 索引，见 FORMS）
     2. 每种 login kind 对应哪套交互（见 loginSpecFor）

   加一个平台 = 后端注册表加一行 +（若需要手填凭据）这里加一条 FORMS。
   交互种类（扫码 / 设备码 / 短信 / 手填）由注册表的 login 字段决定。

   实现是命令式的：输入框非受控（值在 DOM 里），切换平台只重绘主体区，
   不会清空已填内容；提交时按当前平台收集。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, api, toast, copyText } from '../kernel.js';
import { qrMatrix, qrSVG } from '../qr.js';
import { platforms, loadPlatforms, plat } from '../platforms.js';

// 登录轮询定时器 + 进行中的登录状态。
//
// 这几个是**模块级**的，不随面板实例生死：面板会被视图的 5 秒 tick 反复重建，
// 状态挂在实例上就会丢（用户授权完却没人接着轮询——这正是「授权后没入池」的成因）。
let loginTimer = null;
let activeLogin = null; // { provider, d, status }
let loginHost = null;   // 当前面板实例的展示位（重建时被新实例覆盖）
let loginStatus = null;
let lastLoginMsg = null; // { provider, text } 终态提示，面板重建后仍保留

/** stopExtAddTimers() —— 放弃进行中的登录（抽屉关闭时调用）。
 *  注意不要在面板重建时调用，那会把用户正在做的授权掐死。 */
export function stopExtAddTimers() {
  clearLoginTimer();
  activeLogin = null;
  lastLoginMsg = null;
}

// clearLoginTimer 只停定时器，不动登录状态（被新会话顶掉时用）。
function clearLoginTimer() {
  if (loginTimer) { clearInterval(loginTimer); loginTimer = null; }
}

// 本抽屉里用**通用表单**处理的平台（有专属面板的三个——腾讯 / Loomy / Z.AI
// ——不在这里，它们各有自己的分段）。
// 只列字段与提示：展示名、入池方式、有无签到都由后端注册表下发。
const FORMS = {
  lobsterai: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'uuid', label: 'UUID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'first_key_from', label: 'First Key From' },
    ],
    tip: '从有道 LobsterAI 客户端的本地凭据复制；access_token 与 uuid 必填。',
  },
  raccoon: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
    ],
    tip: '推荐直接扫码；也可从客户端本地凭据复制 access_token 手工填写。',
  },
  qoder: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'machine_id', label: 'Machine ID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'security_oauth_token', label: 'Security OAuth Token' },
    ],
    tip: '推荐直接授权登录；手工填写时 machine_id 必填（缺失会被上游拒绝）。',
  },
  codearts: {
    fields: [
      { k: 'access_key_id', label: 'Access Key ID', required: true },
      { k: 'secret_access_key', label: 'Secret Access Key', required: true },
      { k: 'security_token', label: 'Security Token（仅临时 AK/SK 需要）' },
    ],
    tip: '华为云 AK/SK：永久密钥留空 Security Token 即可；临时 STS 凭据才需要填（几小时就过期，不建议入池）。',
  },
};

// 注册表 login kind → 交互描述。返回 null 表示「只需手填凭据」。
function loginSpecFor(p) {
  switch (p && p.login) {
    case 'qr':     return { kind: 'qr', button: '微信扫码登录', hint: '用微信扫一扫，在手机上确认登录' };
    case 'device': return { kind: 'device', button: '浏览器授权登录', hint: '在浏览器打开链接并完成登录授权' };
    case 'code':   return { kind: 'code', button: '开始授权', hint: '在浏览器打开链接、输入设备码并确认' };
    case 'sms':    return { kind: 'sms', button: '手机号登录', hint: '支持手机短信登录' };
    default:       return null; // manual：只出字段表单
  }
}

// 合成某平台的完整规格：注册表给身份与入池方式，FORMS 给字段。
function specFor(id) {
  const p = plat(id) || { id, name: id, note: '' };
  const f = FORMS[id] || { fields: [] };
  return {
    name: p.name,
    note: p.note || '',
    login: loginSpecFor(p),
    fields: f.fields || [],
    tip: f.tip || p.note || '',
  };
}

// 本抽屉处理的平台：注册表里有入池方式、且不是那三个专属面板的。
const BESPOKE = new Set(['workbuddy', 'loomy', 'zai']);
function genericPlatforms() {
  return platforms.peek().filter(p => p.login && !BESPOKE.has(p.id));
}

/** extAddPanel(onAdded, opts) —— 返回一个命令式的添加面板节点。
 *  opts.flat = true 时平铺（抽屉内嵌用），否则收进 <details>。
 *
 *  注意：**不要**在构造时清掉轮询定时器。这个面板会被视图的 5 秒 tick
 *  （loadExt → 重渲染）反复重建，一清就等于把用户正在进行的扫码/授权登录掐死——
 *  实测小浣熊扫码快所以能成，Qoder / Copilot 要在浏览器里操作更久，必死。
 *  定时器只在用户显式放弃（关闭抽屉 / 重新发起）时才停。 */
export function extAddPanel(onAdded, opts = {}) {
  let provider = '';

  const bodyBox = h('div', { class: 'stack', style: { gap: '10px', marginTop: '10px' } });
  const tipEl = h('div', { class: 'muted', style: { fontSize: '11.5px' } });
  const inputs = new Map();

  const providerSel = h('select', {
    class: 'input', style: { width: 'auto' },
    onchange: ev => { provider = ev.target.value; paint(); },
  });

  // 平台清单是异步拉的：到位后重建下拉并选中第一个（保持当前选择若还在）。
  function fillProviders() {
    const list = genericPlatforms();
    if (!list.length) return false;
    const keep = list.some(p => p.id === provider) ? provider : list[0].id;
    providerSel.replaceChildren(...list.map(p => h('option', { value: p.id, text: p.name })));
    provider = keep;
    providerSel.value = keep;
    return true;
  }

  /* ── 手工填写凭据（所有平台的兜底路径） ─────────────────────── */

  function credForm() {
    const idInput = h('input', {
      class: 'input', placeholder: '账号标识（昵称 / uid，用于列表区分）',
      style: { flex: '1', minWidth: '180px' },
    });
    const rows = specFor(provider).fields.map(f => {
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
        for (const f of specFor(provider).fields) {
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
    const statusEl = h('div', { class: 'muted', style: { fontSize: '12px' } });
    const startBtn = h('button', { class: 'btn primary' }, icon('key'), spec.button);

    // 短信登录是两段同步调用（发码 → 校验），没有「发起/轮询」这一步：
    // 直接出表单，且不参与 activeLogin 那套会话状态。
    if (spec.kind === 'sms') {
      renderChallenge(host, spec, {});
      statusEl.textContent = spec.hint;
      return h('div', { class: 'stack', style: { gap: '10px' } }, host, statusEl);
    }

    // 把本实例登记为「进行中登录的展示位」：面板被 tick 重建后，新实例接手继续显示。
    loginHost = host;
    loginStatus = statusEl;
    paintActiveLogin(spec);

    startBtn.onclick = async () => {
      startBtn.disabled = true;
      stopExtAddTimers(); // 重新发起 → 上一个会话的轮询作废
      try {
        const d = await api(`ext/${provider}/login/start`, { method: 'POST' });
        activeLogin = { provider, d, status: '等待完成授权…' };
        paintActiveLogin(spec);
        startPolling(spec, d);
      } catch (e) {
        activeLogin = null;
        setLoginStatus(e.message);
      } finally {
        startBtn.disabled = false;
      }
    };

    return h('div', { class: 'stack', style: { gap: '10px' } }, startBtn, host, statusEl);
  }

  // 轮询循环与面板实例解耦：面板被重建也不影响它跑完。
  function startPolling(spec, d) {
    const every = Math.max(2, d.interval || 2) * 1000;
    const deadline = Date.now() + (d.expires_in || 600) * 1000;
    let errStreak = 0;

    loginTimer = setInterval(async () => {
      // 已被别的登录顶掉 → 只停自己的定时器，别去动 activeLogin（那是新会话的）
      if (!activeLogin || activeLogin.d.session !== d.session) { clearLoginTimer(); return; }
      if (Date.now() > deadline) {
        stopExtAddTimers();
        paintActiveLogin(spec);
        setLoginStatus('登录已超时，请重新发起。');
        return;
      }
      let r;
      try {
        r = await api(`ext/${provider}/login/poll`, {
          method: 'POST', body: JSON.stringify({ session: d.session }),
        });
      } catch (e) {
        // 单次网络抖动不该判死整个登录；连续失败才放弃并把原因留给用户看。
        errStreak++;
        if (errStreak < 3) { setLoginStatus(`轮询异常（第 ${errStreak} 次）：${e.message}`); return; }
        const msg = `轮询连续失败，已停止：${e.message}`;
        stopExtAddTimers();
        paintActiveLogin(spec);   // 会先把状态重置成默认提示
        setLoginStatus(msg);      // 再把原因盖回去
        return;
      }
      errStreak = 0;
      if (!r.done) {
        if (r.status && r.status !== 'pending') setLoginStatus(STATUS_WORDS[r.status] || r.status);
        return;
      }
      stopExtAddTimers();
      paintActiveLogin(spec);
      setLoginStatus(`已入池：${(r.account || {}).label || (r.account || {}).id || ''}`);
      toast(`${specFor(provider).name} 账号已入池`);
      await onAdded?.();
    }, every);
  }

  // 当前面板实例的展示位（模块级：面板重建时被新实例覆盖）
  function setLoginStatus(t) {
    if (activeLogin) activeLogin.status = t; // 记在状态里，面板重建后仍能看到最新进展
    else lastLoginMsg = { provider, text: t }; // 终态消息也留着，别被 5 秒 tick 冲掉
    if (loginStatus && loginStatus.isConnected) loginStatus.textContent = t;
  }

  // 按进行中的登录状态重绘展示区；无进行中的登录则清空。
  function paintActiveLogin(spec) {
    if (!loginHost || !loginHost.isConnected) return;
    const st = activeLogin;
    if (!st || st.provider !== provider) {
      loginHost.replaceChildren();
      const kept = lastLoginMsg && lastLoginMsg.provider === provider ? lastLoginMsg.text : '';
      setLoginStatus(kept || spec.hint);
      return;
    }
    renderChallenge(loginHost, spec, st.d);
    setLoginStatus(st.status);
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
    if (spec.kind === 'sms') {
      const phone = h('input', { class: 'input', placeholder: '手机号（国内版）', style: { flex: '1', minWidth: '160px' } });
      const code = h('input', { class: 'input', placeholder: '6 位短信验证码', style: { flex: '1', minWidth: '120px' } });
      let deviceId = '';
      const sendBtn = h('button', { class: 'btn' }, '发送验证码');
      sendBtn.onclick = async () => {
        const p = phone.value.trim();
        if (!p) { toast('请填写手机号', 'fail'); phone.focus(); return; }
        sendBtn.disabled = true;
        try {
          const r = await api('ext/autoclaw/send_code', { method: 'POST', body: JSON.stringify({ phone: p, region: 'cn' }) });
          deviceId = r.device_id || '';
          toast('验证码已发送');
        } catch (e) { toast(e.message, 'fail'); }
        finally { sendBtn.disabled = false; }
      };
      const loginBtn = h('button', { class: 'btn primary' }, '登录并入池');
      loginBtn.onclick = async () => {
        const p = phone.value.trim();
        if (!p) { toast('请填写手机号', 'fail'); return; }
        if (!deviceId) { toast('请先点「发送验证码」', 'fail'); return; }
        loginBtn.disabled = true;
        try {
          const r = await api('ext/autoclaw/login', {
            method: 'POST',
            body: JSON.stringify({ phone: p, code: code.value.trim(), device_id: deviceId, region: 'cn' }),
          });
          toast('已入池：' + ((r.account || {}).label || ''));
          await onAdded?.();
        } catch (e) { toast(e.message, 'fail'); }
        finally { loginBtn.disabled = false; }
      };
      host.replaceChildren(
        h('div', { class: 'row wrap', style: { gap: '8px' } }, phone, sendBtn),
        h('div', { class: 'row wrap', style: { gap: '8px' } }, code, loginBtn),
        h('div', { class: 'muted', style: { fontSize: '11.5px' },
          text: '国际版上游已关闭短信入口，需从桌面端导入或手工填写凭据。' }),
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
    inputs.clear();
    const spec = specFor(provider);
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
      loginHost = null;
      loginStatus = null;
      bodyBox.replaceChildren(credForm());
    }
    tipEl.textContent = spec.tip;
  }

  // 首次：注册表已在缓存里就直接渲染；否则先出加载态，拉回来再重绘
  if (fillProviders()) {
    paint();
  } else {
    bodyBox.replaceChildren(h('div', { class: 'busy', text: '读取平台列表' }));
    loadPlatforms().then(() => { if (fillProviders()) paint(); });
  }

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
