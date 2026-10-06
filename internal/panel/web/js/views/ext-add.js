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

/* 短信登录的 device_id：必须与发码那一步是同一个（上游把设备和登录会话绑定），
   而两步之间用户要等短信、往往隔了几十秒——期间面板会被视图的 5 秒 tick 重建，
   挂在 DOM 闭包里的 device_id 就没了，点「登录并入池」只会得到「请先点发送验证码」。
   所以按平台存在模块级，面板重建后仍能取回。 */
const smsDevice = new Map(); // provider → device_id

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

/* ── 未完成登录的持久化 ─────────────────────────────────────────
   用户点「开始授权」后要去另一个标签页（有时是同一个）完成授权，回来时
   页面可能已经重新加载——模块级状态一并丢失，轮询停摆，界面就**一直停在
   「等待授权」**。把会话存进 localStorage，回来时自动续上。

   只存到会话自身过期为止（服务端 TTL 10–15 分钟），过期即丢。 */
const LS_LOGIN = 'buddyhub.login';

function saveLogin(provider, d) {
  try { localStorage.setItem(LS_LOGIN, JSON.stringify({ provider, d, at: Date.now() })); } catch { /* 私密模式 */ }
}

function loadSavedLogin() {
  let raw;
  try { raw = localStorage.getItem(LS_LOGIN); } catch { return null; }
  if (!raw) return null;
  let v;
  try { v = JSON.parse(raw); } catch { clearLogin(); return null; }
  const ttl = ((v && v.d && v.d.expires_in) || 600) * 1000;
  if (!v || !v.provider || !v.d || Date.now() - v.at > ttl) { clearLogin(); return null; }
  return v;
}

function clearLogin() {
  try { localStorage.removeItem(LS_LOGIN); } catch { /* 私密模式 */ }
}

// 本抽屉里用**通用表单**处理的平台（有专属面板的三个——腾讯 / Loomy / Z.AI
// ——不在这里，它们各有自己的分段）。
// 只列字段与提示：展示名、入池方式、有无签到都由后端注册表下发。
const FORMS = {
  traework: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'device_id', label: 'Device ID' },
    ],
    tip: '从 TraeWork 客户端 storage.json 的「iCubeAuthInfo://icube.cloudide」条目里取；access_token 必填。',
  },
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
  ima: {
    fields: [
      { k: 'cookie', label: 'x-ima-cookie（完整值）', required: true },
      { k: 'user_id', label: 'IMA-UID（可选，展示用）' },
    ],
    tip: '打开 ima.qq.com 并登录 → F12 → Network → 随便发一条消息 → 找 /cgi-bin/assistant/qa 请求 → 复制请求头 x-ima-cookie 的完整值粘贴到这里（须含 IMA-TOKEN）。',
  },
  marvis: {
    fields: [
      { k: 'access_token', label: 'Ual-Access-Access-Token（mv_ token）', required: true },
      { k: 'openid', label: 'Ual-Access-Openid', required: true },
      { k: 'device_guid', label: 'Ual-Access-Guid（设备 GUID）', required: true },
      { k: 'login_type', label: 'Ual-Access-Login-Type（如 6）' },
    ],
    tip: '从已登录的 Marvis 客户端抓包：Fiddler/mitmproxy 代理后随便发一条消息，复制请求头 Ual-Access-Access-Token / Ual-Access-Openid / Ual-Access-Guid 三个值。token 过期需重新抓。',
  },
};

// 注册表 login kind → 交互描述。返回 null 表示「只需手填凭据」。
function loginSpecFor(p) {
  switch (p && p.login) {
    case 'qr':     return { kind: 'qr', button: '微信扫码登录', hint: '用微信扫一扫，在手机上确认登录' };
    case 'device': return { kind: 'device', button: '浏览器授权登录', hint: '在浏览器打开链接并完成登录授权' };
    // 本机回调：交互与 device 相同（出链接 + 自动轮询），差别在回调由网关自己接住
    case 'callback': return { kind: 'device', button: '浏览器授权登录', hint: '在浏览器打开链接完成授权，本页会自动接住回调' };
    case 'code':   return { kind: 'code', button: '开始授权', hint: '在浏览器打开链接、输入设备码并确认' };
    case 'sms':    return { kind: 'sms', button: '手机号登录', hint: '支持手机短信登录' };
    case 'paste':  return { kind: 'paste', button: '微信扫码登录', hint: '扫码后把回调地址里的 code 贴回来' };
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
  // opts.lockProvider：锁定单一平台（聚焦视图用）——不出现平台选择器，
  // 直接渲染该平台的登录/凭据表单。
  let provider = opts.lockProvider || '';

  const bodyBox = h('div', { class: 'stack', style: { gap: '10px', marginTop: '10px' } });
  const tipEl = h('div', { class: 'muted', style: { fontSize: '11.5px' } });
  const inputs = new Map();

  const providerSel = h('select', {
    class: 'input', style: { width: 'auto' },
    onchange: ev => { provider = ev.target.value; paint(); },
  });

  // 平台清单是异步拉的：到位后重建下拉并选中第一个（保持当前选择若还在）。
  function fillProviders() {
    if (opts.lockProvider) return plat(opts.lockProvider) != null;
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

    // 恢复未完成的登录：用户点「开始授权」后往往要切到另一个标签页（甚至
    // 同一个标签页）去完成授权，回来时页面可能已经重新加载——模块级状态
    // 一并没了，轮询也就停了，表现就是**一直停在「等待授权」**。
    // 会话存在 localStorage 里，回来时自动续上。
    if (!loginTimer) {
      const saved = loadSavedLogin();
      if (saved && saved.provider === provider) {
        activeLogin = { provider, d: saved.d, status: '继续等待授权…' };
        if (spec.kind !== 'paste') startPolling(spec, saved.d);
      }
    }
    paintActiveLogin(spec);

    startBtn.onclick = async () => {
      startBtn.disabled = true;
      stopExtAddTimers(); // 重新发起 → 上一个会话的轮询作废
      try {
        const d = await api(`ext/${provider}/login/start`, { method: 'POST' });
        activeLogin = { provider, d, status: '等待完成授权…' };
        saveLogin(provider, d);
        paintActiveLogin(spec);
        // paste 型（微信回调落在上游域名上，网关截不到）不轮询，
        // 等用户把 code 贴回来再提交——见 renderChallenge 的 paste 分支。
        if (spec.kind !== 'paste') startPolling(spec, d);
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

    loginTimer = setInterval(async () => {
      // 已被别的登录顶掉 → 只停自己的定时器，别去动 activeLogin（那是新会话的）
      if (!activeLogin || activeLogin.d.session !== d.session) { clearLoginTimer(); return; }
      if (Date.now() > deadline) {
        stopExtAddTimers();
        clearLogin();
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
        // 后端只在**终态**时返回非 200（设备码过期 / 授权被拒 / 会话失效）——
        // 上游的瞬时抖动后端自己会吞掉并回 200 + pending。所以这里直接停并
        // 把原因显示出来，而不是当成网络抖动重试三次才告诉用户。
        stopExtAddTimers();
        clearLogin();
        paintActiveLogin(spec);
        setLoginStatus(e.message);
        toast(e.message, 'fail');
        return;
      }
      if (!r.done) {
        if (r.status && r.status !== 'pending') setLoginStatus(STATUS_WORDS[r.status] || r.status);
        return;
      }
      stopExtAddTimers();
      clearLogin();
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
    if (spec.kind === 'paste') {
      // 微信把 code 回给腾讯自己的域名，网关截不到 —— 出二维码让用户扫，
      // 扫完把跳转后地址里的 code 贴回来。
      let svg = null;
      try { svg = qrSVG(qrMatrix(d.auth_url), 180); } catch { svg = null; }
      const codeInput = h('input', {
        class: 'input', placeholder: '粘贴回调地址，或只贴 code',
        style: { fontFamily: 'var(--mono)', fontSize: '12px', flex: '1', minWidth: '200px' },
      });
      const goBtn = h('button', { class: 'btn primary' }, '完成登录');
      goBtn.onclick = async () => {
        const raw = codeInput.value.trim();
        if (!raw) { toast('请先扫码，再把 code 贴回来', 'fail'); codeInput.focus(); return; }
        goBtn.disabled = true;
        try {
          const r = await api(`ext/${provider}/login/poll`, {
            method: 'POST', body: JSON.stringify({ session: d.session, code: raw }),
          });
          if (!r.done) { toast('还没完成，请确认已在微信里确认授权', 'fail'); return; }
          stopExtAddTimers();
          clearLogin();
          host.replaceChildren();
          setLoginStatus(`已入池：${(r.account || {}).label || (r.account || {}).id || ''}`);
          toast(`${spec.name} 账号已入池`);
          await onAdded?.();
        } catch (e) { toast(e.message, 'fail'); }
        finally { goBtn.disabled = false; }
      };
      const parts = [];
      if (svg) parts.push(h('div', { class: 'qr', style: { alignSelf: 'flex-start' } }, svg));
      saveLogin(provider, d); // 页面重载后还能把二维码与回填框恢复出来
      host.replaceChildren(
        ...parts,
        h('div', { class: 'row wrap', style: { gap: '8px' } },
          h('button', { class: 'btn sm', onclick: () => window.open(d.auth_url, '_blank') }, '在浏览器打开授权页'),
        ),
        h('div', { class: 'muted', style: { fontSize: '11.5px' },
          text: '用微信扫码 → 手机上确认 → 浏览器会跳到一个带 code 的地址，把整条地址或 code 复制到这里。' }),
        h('div', { class: 'row wrap', style: { gap: '8px' } }, codeInput, goBtn),
      );
      return;
    }
    if (spec.kind === 'sms') {
      // 恢复上次的填写：面板被 tick 重建后（等短信的几十秒里必然发生），
      // 号码/验证码/device_id 都要还在，否则用户得从头再来一遍。
      const saved = smsDevice.get(provider) || {};
      const phone = h('input', { class: 'input', placeholder: '手机号（国内版）', style: { flex: '1', minWidth: '160px' } });
      const code = h('input', { class: 'input', placeholder: '6 位短信验证码', style: { flex: '1', minWidth: '120px' } });
      phone.value = saved.phone || '';
      code.value = saved.code || '';
      let deviceId = saved.deviceId || '';

      const keep = () => smsDevice.set(provider, { phone: phone.value.trim(), code: code.value.trim(), deviceId });
      phone.addEventListener('input', keep);
      code.addEventListener('input', keep);

      const sendBtn = h('button', { class: 'btn' }, '发送验证码');
      sendBtn.onclick = async () => {
        const p = phone.value.trim();
        if (!p) { toast('请填写手机号', 'fail'); phone.focus(); return; }
        sendBtn.disabled = true;
        try {
          const r = await api('ext/autoclaw/send_code', { method: 'POST', body: JSON.stringify({ phone: p, region: 'cn' }) });
          deviceId = r.device_id || '';
          keep();
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
          smsDevice.delete(provider); // 登录成功，这一轮的状态作废
          toast('已入池：' + ((r.account || {}).label || ''));
          await onAdded?.();
        } catch (e) { toast(e.message, 'fail'); }
        finally { loginBtn.disabled = false; }
      };
      host.replaceChildren(
        h('div', { class: 'row wrap', style: { gap: '8px' } }, phone, sendBtn),
        h('div', { class: 'row wrap', style: { gap: '8px' } }, code, loginBtn),
        deviceId
          ? h('div', { class: 'muted', style: { fontSize: '11.5px' }, text: '验证码已发送，填写后点「登录并入池」。' })
          : h('div', { class: 'muted', style: { fontSize: '11.5px' },
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
    opts.lockProvider ? null : h('div', { class: 'row wrap', style: { gap: '8px' } }, providerSel),
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
