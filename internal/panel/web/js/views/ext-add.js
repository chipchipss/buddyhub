/* ══════════════════════════════════════════════════════════════════
   views/ext-add.js · 单个外部平台的「接入」节点

   只做一件事：给定平台 id，画出该平台自己的入池交互（扫码 / 浏览器授权 /
   设备码 / 短信 / 逐字段填写凭据），成功后把新账号的 {provider,id} 交给调用方。

   「选哪个平台、走哪种方式」是添加向导（js/addwizard.js）第二步的事，这里不再
   自带平台下拉——原先那个下拉和向导里的清单是同一件事的两份实现，两边措辞
   一定会漂（清单 20「两个入口合一」）。

   平台身份与交互种类来自后端注册表（js/platforms.js）：
     1. 每个平台的凭据字段与操作说明（纯 UI 细节，按 id 索引，见 FORMS）
     2. 每种 login kind 对应哪套交互（见 loginSpecFor）
   加一个平台 = 后端注册表加一行 +（若要手填凭据）这里加一条 FORMS。

   实现是命令式的：输入框非受控（值在 DOM 里）。向导会缓存这个节点，
   切走再切回来还是同一棵树，所以进行中的授权轮询与已填内容都不丢。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, api, toast, copyText } from '../kernel.js';
import { qrMatrix, qrSVG } from '../qr.js';
import { plat } from '../platforms.js';

// 登录轮询定时器 + 进行中的登录状态。
//
// 这几个是**模块级**的，不随面板实例生死：用户点「开始授权」后要切到浏览器
// 标签页操作几十秒，回来时页面若重新加载，挂在 DOM 闭包里的状态就没了——
// 「授权完成了却没入池」就是这么来的。
let loginTimer = null;
let activeLogin = null; // { provider, d, status }
let loginHost = null;   // 当前面板实例的展示位（重建时被新实例覆盖）
let loginStatus = null;
let lastLoginMsg = null; // { provider, text } 终态提示，面板重建后仍保留

/* 短信登录的 device_id：必须与发码那一步是同一个（上游把设备和登录会话绑定），
   而两步之间用户要等短信、往往隔了几十秒——期间页面可能重新加载，
   device_id 若在 DOM 闭包里就没了，点「登录并入池」只会得到「请先点发送验证码」。
   所以按平台存在模块级。 */
const smsDevice = new Map(); // provider → device_id

// clearLoginTimer 只停定时器，不动登录状态（被新会话顶掉时用）。
function clearLoginTimer() {
  if (loginTimer) { clearTimeout(loginTimer); loginTimer = null; }
}

// stopExtAddTimers 只在**用户重新发起一条登录**时调用：作废上一条会话的轮询。
// 不导出——抽屉关闭、面板重建都不该掐死用户正在做的授权（网关侧已自驱入池，
// 前端轮询只是把结果接回来展示）。
function stopExtAddTimers() {
  clearLoginTimer();
  activeLogin = null;
  lastLoginMsg = null;
}

/* ── 未完成登录的持久化 ─────────────────────────────────────────
   会话存进 localStorage，页面重新加载后自动续上轮询；只存到会话自身
   过期为止（服务端 TTL 10–15 分钟），过期即丢。 */
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

/* ── 凭据字段与操作说明 ─────────────────────────────────────────
   steps 是给「人」看的第几步做什么（清单 24：抓包、F12 这类操作说明放进
   对应步骤里）；fig 选哪张示意图（见 addwizard.js 的 figFor）。 */
const FORMS = {
  traework: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'device_id', label: 'Device ID' },
    ],
    fig: 'file',
    steps: [
      '打开 TraeWork 客户端的数据目录，找到 storage.json。',
      '定位「iCubeAuthInfo://icube.cloudide」条目，复制里面的 access_token（必填）。',
      '有 refresh_token / device_id 就一并填上，能少一次重新登录。',
    ],
  },
  lobsterai: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'uuid', label: 'UUID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'first_key_from', label: 'First Key From' },
    ],
    fig: 'file',
    steps: [
      '打开有道 LobsterAI 客户端的本地凭据文件。',
      '复制 access_token 与 uuid（两个都必填），其余可选。',
    ],
  },
  raccoon: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
    ],
    fig: 'file',
    steps: [
      '推荐直接扫码登录，不用手抄凭据。',
      '非要手工填：从小浣熊客户端本地凭据里复制 access_token（必填）。',
    ],
  },
  qoder: {
    fields: [
      { k: 'access_token', label: 'Access Token', required: true },
      { k: 'machine_id', label: 'Machine ID', required: true },
      { k: 'refresh_token', label: 'Refresh Token' },
      { k: 'security_oauth_token', label: 'Security OAuth Token' },
    ],
    fig: 'file',
    steps: [
      '推荐直接授权登录，不用手抄凭据。',
      '手工填写时 machine_id 必填——缺了会被上游拒绝。',
      '值取自 Qoder 客户端的本地凭据与机器标识文件。',
    ],
  },
  codearts: {
    fields: [
      { k: 'access_key_id', label: 'Access Key ID', required: true },
      { k: 'secret_access_key', label: 'Secret Access Key', required: true },
      { k: 'security_token', label: 'Security Token（仅临时 AK/SK 需要）' },
    ],
    fig: 'console',
    steps: [
      '在华为云控制台「我的凭证 → API 密钥」里新建或查看永久 AK/SK。',
      'Access Key ID 与 Secret Access Key 都填上；Security Token 留空。',
      '不建议入池临时 STS 凭据——几小时就过期，到期只能重新抓。',
    ],
  },
  ima: {
    fields: [
      { k: 'cookie', label: 'x-ima-cookie（完整值）', required: true },
      { k: 'user_id', label: 'IMA-UID（可选，展示用）' },
    ],
    fig: 'devtools',
    steps: [
      '浏览器打开 ima.qq.com 并登录。',
      '按 F12 打开开发者工具，切到 Network（网络）面板。',
      '在 ima 里随便发一条消息。',
      '点中那条 /cgi-bin/assistant/qa 请求，在 Request Headers 里找到 x-ima-cookie。',
      '把它的完整值（含 IMA-TOKEN）粘贴到下面的输入框。',
    ],
  },
  marvis: {
    fields: [
      { k: 'access_token', label: 'Ual-Access-Access-Token', required: true },
      { k: 'openid', label: 'Ual-Access-Openid', required: true },
      { k: 'device_guid', label: 'Ual-Access-Guid（设备 GUID）', required: true },
      { k: 'login_type', label: 'Ual-Access-Login-Type（如 6）' },
    ],
    fig: 'proxy',
    steps: [
      '用 Fiddler / mitmproxy 之类的代理接住 Marvis 客户端的流量。',
      '在已登录的 Marvis 里随便发一条消息。',
      '找到那条请求的请求头，复制 Ual-Access-Access-Token、Ual-Access-Openid、Ual-Access-Guid 三个值。',
      'token 过期后自动续不了，要重新抓一次。',
    ],
  },
};

/** hasCredFields(id) —— 该平台有没有「逐字段粘贴凭据」这条路径。
 *  添加向导据此决定第二步列几种接入方式。 */
export function hasCredFields(id) {
  return !!(FORMS[id] && FORMS[id].fields && FORMS[id].fields.length);
}

/** credHelp(id) —— 该平台的凭据字段 + 操作说明（向导的「完成接入」步用）。 */
export function credHelp(id) {
  const f = FORMS[id] || { fields: [], steps: [] };
  return { fields: f.fields || [], steps: f.steps || [], fig: f.fig || '' };
}

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

/** extAddPanel(onAdded, opts) —— 单个平台的接入节点。
 *  opts.lockProvider（必填）平台 id；opts.only = 'login' | 'creds' 只显示一条路径。
 *  onAdded(ref) 在账号确定落盘后调用，ref = {provider, id, label}。
 *
 *  注意：**不要**在构造时清掉轮询定时器。这个节点会被抽屉缓存复用，也可能在
 *  面板重建时重新构造一次，一清就等于把用户正在进行的扫码/授权掐死——
 *  实测小浣熊扫码快所以能成，Qoder / Copilot 要在浏览器里操作更久，必死。 */
export function extAddPanel(onAdded, opts = {}) {
  const provider = opts.lockProvider;
  const p = plat(provider) || { id: provider, name: provider };
  const spec = loginSpecFor(p);
  const only = opts.only || '';

  const tipEl = h('div', { class: 'muted', style: { fontSize: '11.5px' } });
  const inputs = new Map();

  /* ── 手工填写凭据（所有可手填平台的兜底路径） ─────────────────── */

  function credForm() {
    const idInput = h('input', {
      class: 'input', placeholder: '账号标识（昵称 / uid，用于列表区分）',
      style: { flex: '1', minWidth: '180px' },
    });
    const rows = (FORMS[provider]?.fields || []).map(f => {
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
        for (const f of FORMS[provider]?.fields || []) {
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
          idInput.value = '';
          for (const el of inputs.values()) el.value = '';
          await onAdded?.({ provider, id });
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

  /* ── 登录入池（扫码 / 设备授权 / 设备码 / 短信） ──────────────── */

  function loginPanel() {
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

    // 把本实例登记为「进行中登录的展示位」：面板被重建后，新实例接手继续显示。
    loginHost = host;
    loginStatus = statusEl;

    // 恢复未完成的登录：用户点「开始授权」后往往要切到另一个标签页（甚至
    // 同一个标签页）去完成授权，回来时页面可能已经重新加载——模块级状态
    // 一并没了，轮询也就停了，表现是**一直停在「等待授权」**。
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
        // paste 型（微信回调落在上游域名，网关截不到）不轮询，
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
  function startPolling(s, d) {
    const baseEvery = Math.max(2, d.interval || 2) * 1000;
    const deadline = Date.now() + (d.expires_in || 600) * 1000;

    const tick = async () => {
      // 已被别的登录顶掉 → 只停自己的定时器，别去动 activeLogin（那是新会话的）
      if (!activeLogin || activeLogin.d.session !== d.session) { clearLoginTimer(); return; }
      if (Date.now() > deadline) {
        stopExtAddTimers();
        clearLogin();
        paintActiveLogin(s);
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
        paintActiveLogin(s);
        setLoginStatus(e.message);
        toast(e.message, 'fail');
        return;
      }
      if (!r.done) {
        if (r.status && r.status !== 'pending') setLoginStatus(STATUS_WORDS[r.status] || r.status);
        // 后端要退避就按它给的秒数等：设备码类上游被问烦只会一直回 slow_down，
        // 用户明明已在浏览器点过授权，令牌却永远换不出来。
        loginTimer = setTimeout(tick, r.retry_in ? Math.max(baseEvery, r.retry_in * 1000) : baseEvery);
        return;
      }
      stopExtAddTimers();
      clearLogin();
      paintActiveLogin(s);
      const acct = r.account || {};
      setLoginStatus(`已入池：${acct.label || acct.id || ''}`);
      await onAdded?.({ provider: acct.provider || provider, id: acct.id, label: acct.label });
    };
    loginTimer = setTimeout(tick, baseEvery);
  }

  // 当前面板实例的展示位（模块级：面板重建时被新实例覆盖）
  function setLoginStatus(t) {
    if (activeLogin) activeLogin.status = t; // 记在状态里，面板重建后仍能看到最新进展
    else lastLoginMsg = { provider, text: t }; // 终态消息也留着，别被 5 秒 tick 冲掉
    if (loginStatus && loginStatus.isConnected) loginStatus.textContent = t;
  }

  // 按进行中的登录状态重绘展示区；无进行中的登录则清空。
  function paintActiveLogin(s) {
    if (!loginHost || !loginHost.isConnected) return;
    const st = activeLogin;
    if (!st || st.provider !== provider) {
      loginHost.replaceChildren();
      const kept = lastLoginMsg && lastLoginMsg.provider === provider ? lastLoginMsg.text : '';
      setLoginStatus(kept || s.hint);
      return;
    }
    renderChallenge(loginHost, s, st.d);
    setLoginStatus(st.status);
  }

  // 按登录方式渲染「待用户完成的那一步」
  function renderChallenge(host, s, d) {
    if (s.kind === 'qr') {
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
            class: 'btn sm', onclick: async () => {
              try { await copyText(d.qr_url); toast('链接已复制'); }
              catch { toast('复制失败，请手动选择', 'fail'); }
            },
          }, '复制链接'),
        ),
      );
      return;
    }
    if (s.kind === 'code') {
      host.replaceChildren(
        h('div', { style: { font: '600 26px var(--mono)', letterSpacing: '3px', userSelect: 'all' }, text: d.user_code || '' }),
        h('a', { class: 'link', href: d.verification_uri || 'https://github.com/login/device',
          target: '_blank', rel: 'noreferrer', text: d.verification_uri || 'https://github.com/login/device' }),
      );
      return;
    }
    if (s.kind === 'paste') {
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
          const acct = r.account || {};
          setLoginStatus(`已入池：${acct.label || acct.id || ''}`);
          await onAdded?.({ provider: acct.provider || provider, id: acct.id, label: acct.label });
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
    if (s.kind === 'sms') {
      // 恢复上次的填写：等短信的几十秒里页面可能重新加载，
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
          const acct = r.account || {};
          await onAdded?.({ provider: acct.provider || provider, id: acct.id, label: acct.label });
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
        h('button', { class: 'btn sm', onclick: () => window.open(url, '_blank') }, '在浏览器打开'),
      ),
    );
  }

  /* ── 组装：only 决定只出哪一条路径 ───────────────────────────── */

  const showLogin = spec && only !== 'creds';
  const showCreds = hasCredFields(provider) && only !== 'login';
  const parts = [];
  // 只出登录、或只出凭据：向导第二步已经选好走哪条路。
  // 没出登录面板时不用去清 loginHost——paintActiveLogin 认 isConnected，
  // 挂在废弃节点上的旧展示位本来就写不进去。
  if (showLogin) parts.push(loginPanel());
  if (showCreds) {
    parts.push(credForm());
    tipEl.textContent = '账号标识只是列表里显示的名字，不影响登录。';
  }
  if (!parts.length) {
    parts.push(h('div', { class: 'muted', style: { fontSize: '12.5px' },
      text: '该平台没有可从面板发起的接入方式，请在配置页填写。' }));
  }

  return h('div', { class: 'glass-flat stack', style: { padding: '12px 14px', gap: '10px' } }, ...parts, tipEl);
}

// 上游轮询状态的中文说明（小浣熊会回 logging 等中间态）
const STATUS_WORDS = { logging: '已扫码，等待手机确认…', pending: '等待扫码…' };
