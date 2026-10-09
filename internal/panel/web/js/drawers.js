/* ══════════════════════════════════════════════════════════════════
   drawers.js · 抽屉
   添加账号（选平台 → 该平台自己的接入方式）
   开学季券码（含离线二维码）
   账号详情与成长任务在 views/account-detail.js（那是「一个账号」的界面，
   不是通用抽屉件，放在一起才不会两边措辞漂移）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, apiUpload, toast, openDrawer, closeDrawer, copyText } from './kernel.js';
import { qrMatrix, qrSVG } from './qr.js';
import { refreshOverview } from './store.js';
import { flowStore, pollLoop } from './loginflow.js';
import { platforms } from './platforms.js';
import { extAddPanel, stopExtAddTimers } from './views/ext-add.js';
import { zaiAddForm } from './views/zai-segment.js';
import { extJsonImport } from './views/ext-segment.js';

/* ══ 1. 添加账号 ══════════════════════════════════════════════════ */
const addTab = signal('tencent');
const realm = signal('cn');
const loginUrl = signal('');
const loginState = signal('');
const status = signal({ kind: '', text: '' });
const loomyMethod = signal('detect');
const loomyInfo = signal(null);

// 腾讯 OAuth 会话持久化：用户去浏览器登录期间页面可能刷新（或弹窗被 5s tick
// 重建的是外层、这里状态在模块级已安全，但**页面级重载**会全丢）。会话存
// localStorage（后端 loginTTL 15 分钟），回来时自动续上轮询——与 ext-add /
// zai 的做法统一（loginflow.js）。
const tencentFlow = flowStore('tencentLogin');
const TENCENT_FLOW_TTL = 15 * 60 * 1000;
let pollLoopCtl = null;

function stopPoll() { if (pollLoopCtl) { pollLoopCtl.stop(); pollLoopCtl = null; } }

/** resumeTencentLogin() —— 页面重载后把未完成的腾讯授权轮询续上。 */
function resumeTencentLogin() {
  if (pollLoopCtl) return;                 // 已在轮询
  const saved = tencentFlow.getFresh(TENCENT_FLOW_TTL);
  if (!saved) return;
  loginState.set(saved.state);
  loginUrl.set(saved.url);
  startLoginPoll();
}

/** openAddAccount(provider) —— 打开添加向导。
 *  传平台 id（详情页「重新登录 / 重新授权」用）则直接落在该平台的接入表单上，
 *  不传则先出平台清单。 */
export function openAddAccount(provider) {
  addTab.set(tabForProvider(provider));
  status.set({ kind: '', text: '' });
  loomyInfo.set(null);
  if (!loginState.peek()) resumeTencentLogin();  // 有进行中的授权则恢复显示
  renderAdd();
}

// 行模型里的 provider 与向导内部的 tab id 不完全同名（workbuddy 走 tencent 面板，
// 外部平台带 ext: 前缀），这里做一次翻译；认不出的平台回清单。
function tabForProvider(provider) {
  if (!provider) return '';
  if (provider === 'workbuddy') return 'tencent';
  if (provider === 'zai' || provider === 'loomy') return provider;
  const p = platforms.peek().find(x => x.id === provider);
  return p && p.login ? 'ext:' + provider : '';
}

// 页面加载即尝试续上未完成的腾讯授权轮询（即使抽屉没打开也继续收尾——
// 用户在浏览器里完成登录后回来，账号应当已经入池，而不是停在「等待授权」）。
resumeTencentLogin();

function setStatus(kind, text) { status.set({ kind, text }); }

function renderAdd() {
  const body = h('div', { class: 'stack' }, tabBody());
  openDrawer({
    title: '添加账号',
    hint: '选择平台与接入方式，凭据直接落盘进池，无需重启',
    body,
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', text: '关闭', onclick: () => { stopPoll(); stopExtAddTimers(); closeDrawer(); } }),
    ),
  });
}

/* 添加账号入口：先选平台，再出该平台的导入表单（一次只看一个平台）。
   平台清单来自注册表（platforms.js）：有 login 方式的都可入池；
   三个有专属面板的平台（腾讯 / Loomy / Z.AI）走各自的 panel，
   其余走通用 extAddPanel；JSON 批量导入作为最后一项工具入口。 */
const BESPOKE_PANELS = { tencent: tencentPanel, loomy: loomyPanel, zai: zaiPanel };

function platformRows() {
  const cur = addTab.peek();
  const reg = platforms.peek();

  const row = (id, name, hint) => h('button', {
    class: 'addrow' + (cur === id ? ' on' : ''),
    onclick: () => { addTab.set(id); stopPoll(); stopExtAddTimers(); renderAdd(); },
  },
    h('span', { class: 'nm', text: name }),
    h('span', { class: 'hint', text: hint }),
    h('span', { class: 'grow' }),
    h('span', { class: 'chev', 'aria-hidden': 'true' }, '›'),
  );

  const rows = [];

  // 三个专属面板的平台（顺序固定）
  rows.push(row('tencent', '腾讯 WorkBuddy', '浏览器 OAuth 登录 · CN / Global'));
  const loomyP = reg.find(p => p.id === 'loomy');
  if (loomyP) rows.push(row('loomy', loomyP.name, loomyP.note || '客户端检测 / 手机号 / Token'));
  const zaiP = reg.find(p => p.id === 'zai');
  if (zaiP) rows.push(row('zai', zaiP.name, 'OAuth 免密 / JWT / API Key'));

  // 其余有 login 方式的平台（通用表单）
  for (const p of reg) {
    if (!p.login || p.id in BESPOKE_PANELS || p.id === 'loomy' || p.id === 'zai') continue;
    if (p.login === 'none' || p.login === 'config') continue;   // codex / free 走配置页
    rows.push(row('ext:' + p.id, p.name, p.note || '按平台引导入池'));
  }

  rows.push(row('import', '批量 JSON 导入', 'cockpit tools 导出的账号数组'));
  return rows;
}

function tabBody() {
  const cur = addTab.peek();
  // 未选择平台：出平台清单（默认状态）
  if (cur === 'tencent' || cur === 'loomy' || cur === 'zai' || cur === 'import') {
    return h('div', { class: 'stack' },
      h('button', { class: 'addrow back', onclick: () => { addTab.set(''); stopPoll(); stopExtAddTimers(); renderAdd(); } },
        h('span', { class: 'chev', 'aria-hidden': 'true' }, '‹'),
        h('span', { class: 'nm', text: '选择其他平台' })),
      cur === 'tencent' ? tencentPanel()
        : cur === 'loomy' ? loomyPanel()
          : cur === 'zai' ? zaiPanel()
            : importPanel(),
      status.peek().text
        ? h('div', { class: 'chip' + (status.peek().kind === 'fail' ? ' faint' : ' strong'),
            style: { padding: '10px 13px', whiteSpace: 'normal', display: 'block' },
            text: status.peek().text })
        : null,
    );
  }
  if (String(cur).startsWith('ext:')) {
    const prov = String(cur).slice(4);
    return h('div', { class: 'stack' },
      h('button', { class: 'addrow back', onclick: () => { addTab.set(''); stopPoll(); stopExtAddTimers(); renderAdd(); } },
        h('span', { class: 'chev', 'aria-hidden': 'true' }, '‹'),
        h('span', { class: 'nm', text: '选择其他平台' })),
      extAddPanel(() => refreshOverview(), { flat: true, lockProvider: prov }),
      h('details', { style: { marginTop: '6px' } },
        h('summary', { class: 'muted', style: { cursor: 'pointer', fontSize: '12.5px' }, text: '高级：粘贴完整凭据 JSON' }),
        h('div', { style: { marginTop: '10px' } }, extJsonImport(() => refreshOverview()))),
      status.peek().text
        ? h('div', { class: 'chip' + (status.peek().kind === 'fail' ? ' faint' : ' strong'),
            style: { padding: '10px 13px', whiteSpace: 'normal', display: 'block' },
            text: status.peek().text })
        : null,
    );
  }
  // 平台选择页
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12px' }, text: '选择要添加的平台，凭据直接落盘进池，无需重启。' }),
    h('div', { class: 'addlist' }, ...platformRows()),
  );
}

// Z.AI / 智谱：JWT 或 API Key 入池，也支持 OAuth 免密登录。
function zaiPanel() {
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: 'JWT 走 Plan 通道（消耗订阅额度）；API Key 走回退通道（免验证码）。入池后可用 zai:GLM-5.3 调用。' }),
    zaiAddForm(() => refreshOverview()),
  );
}

// 外部平台：LobsterAI / 小浣熊 / Qoder / 华为云 CodeArts / GitHub Copilot。
function extPanel() {
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: 'GitHub Copilot 用设备码授权，其余平台粘贴客户端凭据即可。' }),
    extAddPanel(() => refreshOverview(), { flat: true }),
  );
}

function tencentPanel() {
  const st = status.peek();
  return h('div', { class: 'stack' },
    h('div', { class: 'seg' },
      ...[['cn', '国内版 CN'], ['global', '国际版 Global']].map(([v, n]) => h('button', {
        class: realm.peek() === v ? 'on' : '', text: n,
        onclick: () => { realm.set(v); renderAdd(); },
      })),
    ),
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: realm.peek() === 'global'
        ? '国际版登录后自动完成注册地区、激活与试用额度领取。'
        : '登录后自动完成签到与积分初始化。' }),

    loginUrl()
      ? h('div', { class: 'stack' },
        h('div', { class: 'muted', style: { fontSize: '12px' }, text: '在浏览器打开以下链接并完成登录：' }),
        h('div', { class: 'url-box', text: loginUrl() }),
        h('div', { class: 'row' },
          h('button', {
            class: 'btn sm', onclick: async () => {
              try { await copyText(loginUrl()); toast('链接已复制'); } catch { toast('复制失败，请手动选择', 'fail'); }
            },
          }, icon('copy'), '复制链接'),
          h('button', { class: 'btn sm primary', onclick: () => window.open(loginUrl(), '_blank') }, '在浏览器打开'),
        ),
        h('div', { class: 'busy', text: '等待授权完成，自动检测中' }),
      )
      : h('div', { class: 'row' },
        h('button', {
          class: 'btn primary', onclick: async ev => {
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            setStatus('', '正在获取授权链接…');
            renderAdd();
            try {
              const r = await api('login/start', { method: 'POST', body: JSON.stringify({ realm: realm.peek() }) });
              loginState.set(r.state);
              loginUrl.set(r.url);
              tencentFlow.set({ state: r.state, url: r.url, at: Date.now() });
              setStatus('', '');
              renderAdd();
              startLoginPoll();
            } catch (e) { setStatus('fail', e.message); renderAdd(); }
            finally { if (__b) __b.disabled = false; }
          },
        }, icon('plus'), '获取授权链接'),
      ),
    st.kind === 'fail' && !loginUrl() ? null : null,
  );
}

function startLoginPoll() {
  stopPoll();
  const state = loginState.peek();
  if (!state) return;
  pollLoopCtl = pollLoop();
  pollLoopCtl.register({
    every: 3000,
    run: async () => {
      if (!loginState.peek()) { pollLoopCtl.finish(); return; }
      try {
        const r = await api('login/poll?state=' + encodeURIComponent(loginState.peek()));
        if (r.done) {
          pollLoopCtl.finish();
          tencentFlow.clear();
          setStatus('', `已添加 ${r.nickname || r.uid}${r.realm === 'global' ? '（国际版）' : ''}` +
            (r.credits >= 0 ? ` · 积分 ${r.credits}` : '') + '，账号已载入池中');
          loginState.set('');
          loginUrl.set('');
          renderAdd();
          await refreshOverview();
          setTimeout(() => closeDrawer(), 1600);
        }
      } catch (e) {
        pollLoopCtl.finish();
        tencentFlow.clear();
        setStatus('fail', e.message + '（关闭后重新添加）');
        loginState.set('');
        renderAdd();
      }
    },
  });
}

function loomyPanel() {
  const m = loomyMethod.peek();
  const info = loomyInfo();
  return h('div', { class: 'stack' },
    h('div', { class: 'seg' },
      ...[['detect', '自动检测'], ['pwd', '账号密码'], ['token', '手动 Token']].map(([v, n]) => h('button', {
        class: m === v ? 'on' : '', text: n,
        onclick: () => { loomyMethod.set(v); if (v === 'detect') detectLoomy(); renderAdd(); },
      })),
    ),

    m === 'detect' ? h('div', { class: 'stack' },
      h('div', { class: 'row' },
        h('button', { class: 'btn sm primary', onclick: () => detectLoomy() }, icon('scan'), '开始检测'),
      ),
      info
        ? h('div', { class: 'task-tile' },
          h('div', null,
            h('div', { class: 't', text: info.userid ? 'Loomy · ' + info.userid : 'Loomy 客户端' }),
            h('div', { class: 'c', text: `手机 ${info.phone_masked || '已登录'} · 本地缓存就绪` }),
          ),
          h('div', { class: 'p', text: `${info.earned} / ${info.total}` }),
        )
        : h('div', { class: 'muted', style: { fontSize: '12.5px' }, text: '未检测到本机 Loomy 客户端时，可改用密码或 Token 接入' }),
    ) : null,

    m === 'pwd' ? h('div', { class: 'stack' },
      h('label', { class: 'field' }, h('span', { class: 'label', text: '手机号' }),
        h('input', { class: 'input', id: 'loomy-phone', placeholder: '手机号' })),
      h('label', { class: 'field' }, h('span', { class: 'label', text: '密码' }),
        h('div', { class: 'row' },
          h('input', { class: 'input', id: 'loomy-pwd', type: 'password', placeholder: '密码' }),
          h('button', {
            class: 'btn sm primary', onclick: async ev => {
              const phone = document.getElementById('loomy-phone').value.trim();
              const password = document.getElementById('loomy-pwd').value;
              if (!phone || !password) { toast('手机号与密码必填', 'fail'); return; }
              var __b = ev.currentTarget; if (__b) __b.disabled = true;
              setStatus('', '登录中…');
              renderAdd();
              try {
                await api('ext/loomy/login_password', { method: 'POST', body: JSON.stringify({ phone, password }) });
                setStatus('', '密码登录成功，账号已入池');
                renderAdd();
                await refreshOverview();
              } catch (e) { setStatus('fail', '登录失败：' + e.message); renderAdd(); }
              finally { if (__b) __b.disabled = false; }
            },
          }, '登录'),
        )),
      h('div', { class: 'row' },
        h('input', { class: 'input', id: 'loomy-sms', placeholder: '短信验证码（备用）' }),
        h('button', {
          class: 'btn sm', onclick: async ev => {
            const phone = document.getElementById('loomy-phone').value.trim();
            if (!phone) { toast('先填手机号', 'fail'); return; }
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            try { await api('ext/loomy/send_sms', { method: 'POST', body: JSON.stringify({ phone }) }); toast('验证码已发送'); }
            catch (e) { toast(e.message, 'fail'); }
            finally { if (__b) __b.disabled = false; }
          },
        }, '发验证码'),
        h('button', {
          class: 'btn sm', onclick: async ev => {
            const phone = document.getElementById('loomy-phone').value.trim();
            const code = document.getElementById('loomy-sms').value.trim();
            if (!phone || !code) { toast('手机号与验证码必填', 'fail'); return; }
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            setStatus('', '短信登录中…');
            renderAdd();
            try {
              await api('ext/loomy/login_sms', { method: 'POST', body: JSON.stringify({ phone, code, msgid: '' }) });
              setStatus('', '短信登录成功，账号已入池');
              renderAdd();
              await refreshOverview();
            } catch (e) { setStatus('fail', '登录失败：' + e.message); renderAdd(); }
            finally { if (__b) __b.disabled = false; }
          },
        }, '短信登录'),
      ),
    ) : null,

    m === 'token' ? h('div', { class: 'stack' },
      h('label', { class: 'field' },
        h('span', { class: 'label', text: 'Loomy Session Token' }),
        h('input', { class: 'input', id: 'loomy-token', placeholder: '从 userData/auth-session.json 复制 session 字段' }),
        h('span', { class: 'tip', text: '验证通过后立即落盘进池' })),
      h('div', { class: 'row' },
        h('button', {
          class: 'btn sm primary', onclick: async ev => {
            const tok = document.getElementById('loomy-token').value.trim();
            if (!tok) { toast('请输入 Session Token', 'fail'); return; }
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            setStatus('', '验证中…');
            renderAdd();
            try {
              await api('loomy/save', { method: 'POST', body: JSON.stringify({ session: tok }) });
              setStatus('', '保存成功');
              renderAdd();
              await refreshOverview();
            } catch (e) { setStatus('fail', e.message); renderAdd(); }
            finally { if (__b) __b.disabled = false; }
          },
        }, icon('check'), '验证并保存'),
      ),
    ) : null,
  );
}

async function detectLoomy() {
  try {
    const d = await api('loomy/status');
    if (!d.has_account) {
      loomyInfo.set(null);
      setStatus('fail', d.message || '未检测到本机 Loomy 客户端');
    } else {
      loomyInfo.set(d.status || {});
      setStatus('', '');
    }
  } catch (e) { setStatus('fail', e.message); }
  renderAdd();
}

function importPanel() {
  const file = h('input', { type: 'file', accept: '.json', class: 'input' });
  file.addEventListener('change', async () => {
    const f = file.files[0];
    if (!f) return;
    const fd = new FormData();
    fd.append('file', f);
    setStatus('', '导入中…');
    renderAdd();
    try {
      const d = await apiUpload('import/cockpit', fd);
      setStatus('', `导入完成：成功 ${d.imported} 个${d.skipped ? `，跳过 ${d.skipped} 个` : ''}`);
      await refreshOverview();
    } catch (e) { setStatus('fail', '导入失败：' + e.message); }
    renderAdd();
  });
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12.5px' }, text: '选择 cockpit tools 导出的 JSON 文件（账号数组）批量导入。' }),
    file,
  );
}

/* ══ 2. 开学季券码 ════════════════════════════════════════════════ */
export function openVouchers() {
  const body = h('div', { class: 'stack' }, h('div', { class: 'busy', text: '查询券码' }));
  openDrawer({
    title: '开学季 · 我的券码',
    hint: '抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），到对应 App 或小程序兑换',
    body,
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('button', { class: 'btn sm', onclick: () => loadVouchers(body) }, icon('refresh'), '刷新'),
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', text: '关闭', onclick: closeDrawer }),
    ),
  });
  loadVouchers(body);
}

async function loadVouchers(host) {
  try {
    const d = await api('school/vouchers');
    const accounts = d.accounts || [];
    const ok = accounts.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    const nodes = [];
    for (const a of ok) {
      const vs = a.vouchers || [];
      if (!vs.length) continue;
      nodes.push(h('div', { class: 'plat-head' },
        h('span', { class: 'nm', text: a.nickname || a.uid }), h('span', { class: 'line' }), h('span', { text: `${vs.length} 张` })));
      nodes.push(...vs.map(voucherCard));
    }
    if (!nodes.length) nodes.push(h('div', { class: 'empty' }, icon('ticket'), h('div', { class: 't', text: '还没有抽到券' })));
    const failed = accounts.filter(a => a.error);
    if (failed.length) {
      nodes.push(h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '8px' },
        text: '查询失败：' + failed.map(a => `${a.nickname || a.uid.slice(0, 8)}（${a.error}）`).join('、') }));
    }
    if (total) nodes.unshift(h('div', { class: 'muted', style: { fontSize: '12px' }, text: `共 ${total} 张可用券` }));
    host.replaceChildren(...nodes);
  } catch (e) {
    host.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'd', text: e.message })));
  }
}

function voucherCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  const qrHost = h('div');
  return h('div', { class: 'voucher' + (expired ? ' expired' : '') },
    h('div', { class: 'hd' },
      h('span', { class: 'nm', text: v.prize_name || v.sku_code || '券' }),
      h('span', { class: 'chip ' + (expired ? 'faint' : 'strong'), text: expired ? '已过期' : '可使用' }),
    ),
    h('div', { class: 'meta', text: (v.valid_to ? `有效期至 ${v.valid_to}` : '长期有效') + (v.granted_at ? ` · ${String(v.granted_at).slice(0, 10)} 抽中` : '') }),
    h('div', { class: 'sep' }),
    h('div', { class: 'ft' },
      h('span', { class: 'lab', text: '券码' }),
      h('code', { text: v.code || '-' }),
      h('div', { class: 'acts' },
        v.code ? h('button', {
          class: 'btn sm', onclick: () => {
            if (qrHost.firstChild) { qrHost.replaceChildren(); return; }
            try { qrHost.replaceChildren(h('div', { class: 'qr' }, qrSVG(qrMatrix(v.code), 148))); }
            catch (e) { qrHost.replaceChildren(h('div', { class: 'muted', style: { padding: '10px' }, text: '二维码生成失败：' + e.message })); }
          },
        }, icon('qr'), '二维码') : null,
        h('button', {
          class: 'btn sm', onclick: async () => {
            try { await copyText(v.code || ''); toast('券码已复制'); } catch { toast('复制失败，请手动选择', 'fail'); }
          },
        }, icon('copy'), '复制'),
      ),
    ),
    qrHost,
  );
}
