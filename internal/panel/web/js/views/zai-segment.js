/* ══════════════════════════════════════════════════════════════════
   views/zai-segment.js · Z.AI（智谱）数据源 + 入池表单
   Z.AI 账号的管理面已经并入「账号」页的统一表格（views/accounts.js 行模型
   在 rows.js，动作在 acts.js，详情在 views/account-detail.js）。
   这个文件只剩三件 Z.AI 独有的事：
     1. 数据源信号 loadZai / zaiData —— 表格和抽屉读同一份，不各存一份
     2. 入池表单 zaiAddForm(onAdded, {way}) —— 由添加向导按接入方式调用
     3. OAuth 免密登录的轮询收尾（页面重载后要能续上）
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, copyText } from '../kernel.js';
import { flowStore, pollLoop } from '../loginflow.js';

const zai = signal(null);
const zaiErr = signal('');
const addProvider = signal('zai');
const oauth = signal(null);   // { url, flowId, status }

/** zaiData —— Z.AI 数据源信号（{configured, accounts, captcha, message}）。 */
export const zaiData = zai;

/** zaiLastAdded —— 最近一次通过 OAuth 入池的账号：{ref, at}。
 *  用信号而不是回调：轮询可能在向导没开着的时候就跑完（页面重载后自动续上），
 *  那一刻没有订阅者；向导回来时按时间戳判断这条是不是自己等的那一次。 */
export const zaiLastAdded = signal(null);

/* 进行中的 OAuth 流程 id 落 localStorage（loginflow.js 统一件）。
   用户在智谱页面登录期间如果刷新/切走了面板，模块状态会丢——服务端那条流程
   还在（15 分钟 TTL），但前端不知道 flow_id 就再也轮询不到了，表现是
   「登录完成了却一直没入池」。存下来，回来时自动续上。 */
const zaiFlow = flowStore('zaiOAuth');
const ZAI_FLOW_TTL = 15 * 60 * 1000;
let oauthLoop = null;

function stopOAuthPoll() { if (oauthLoop) { oauthLoop.finish(); oauthLoop = null; } }

function saveOAuthFlow(flowId) { zaiFlow.set({ flowId, at: Date.now() }); }
function loadOAuthFlow() { const v = zaiFlow.getFresh(ZAI_FLOW_TTL); return v ? v.flowId : ''; }
function clearOAuthFlow() { zaiFlow.clear(); }

/** resumeOAuth 页面加载后恢复未完成的 OAuth 轮询（有则续上）。 */
export function resumeOAuth() {
  if (oauthLoop) return;
  const flowId = loadOAuthFlow();
  if (!flowId) return;
  oauth.set({ flowId, status: '继续等待智谱页面完成登录…' });
  pollOAuth(flowId);
}

// pollOAuth 轮询一条流程直到完成/失败。
function pollOAuth(flowId) {
  stopOAuthPoll();
  oauthLoop = pollLoop();
  oauthLoop.register({
    every: 3000,
    run: async () => {
      const cur = oauth.peek();
      if (!cur || !cur.flowId) return;
      try {
        const p = await api('zai/oauth/poll?flow_id=' + encodeURIComponent(flowId));
        if (!p.done) return;
        stopOAuthPoll();
        clearOAuthFlow();
        oauth.set(null);
        zaiLastAdded.set({ ref: { provider: 'zai', id: p.id, label: p.name }, at: Date.now() });
        toast(p.message || '账号已入池');
        await loadZai();
      } catch (e) {
        stopOAuthPoll();
        clearOAuthFlow();
        oauth.set({ status: '授权失败：' + e.message });
      }
    },
  });
}

/** 发起 OAuth 免密登录：拿授权链接 → 轮询 → 完成后自动兑换回退 Key 并入池。 */
async function startOAuth(name) {
  stopOAuthPoll();
  oauth.set({ status: '正在获取授权链接…' });
  try {
    const r = await api('zai/oauth/start', { method: 'POST', body: JSON.stringify({ name }) });
    oauth.set({ url: r.authorize_url, flowId: r.flow_id, status: '在浏览器完成登录，此处自动检测' });
    saveOAuthFlow(r.flow_id);
    pollOAuth(r.flow_id);
  } catch (e) {
    oauth.set({ status: '发起失败：' + e.message });
  }
}

export async function loadZai(quiet = true) {
  try {
    zai.set(await api('zai/accounts'));
    zaiErr.set('');
  } catch (e) {
    zaiErr.set(e.message);
    if (!quiet) toast(e.message, 'fail');
  }
}

/** zaiAddForm(onAdded, opts) —— Z.AI / 智谱的「添加账号入池」表单。
 *  opts.way = 'oauth' | 'secret'（缺省 = 两种都出）。添加向导的第二步已经选好
 *  接入方式，所以这里只画那一种：一屏一条主路径（清单 49）。
 *  onAdded(ref) 在账号落盘后调用，ref = {provider:'zai', id}。
 *
 *  整块命令式管理：输入框是非受控的（值在 DOM 里），局部状态走订阅式重绘——
 *  避免响应式重渲染把正在输入的名称/密钥清空。 */
export function zaiAddForm(onAdded, opts = {}) {
  const way = opts.way || '';
  const showOAuth = way !== 'secret';
  const showSecret = way !== 'oauth';

  const nameInput = h('input', {
    class: 'input', placeholder: '账号名称（如：主号）', style: { flex: '1', minWidth: '150px' },
  });
  const secretInput = h('input', {
    class: 'input',
    placeholder: 'Coding Plan JWT（三段点分）或 Z.AI API Key',
    style: { fontFamily: 'var(--mono)', fontSize: '12px' },
  });
  const hintEl = h('div', { class: 'muted', style: { fontSize: '11.5px' } });
  const providerSel = h('select', { class: 'input', style: { width: 'auto' } },
    h('option', { value: 'zai', text: 'Z.AI（支持 JWT / API Key）' }),
    h('option', { value: 'bigmodel', text: '智谱开放平台（API Key）' }),
  );
  providerSel.value = addProvider.peek();

  const submitBtn = h('button', {
    class: 'btn primary',
    onclick: async ev => {
      const secret = secretInput.value.trim();
      if (!secret) { toast('请粘贴 JWT 或 API Key', 'fail'); return; }
      const label = nameInput.value.trim();
      ev.currentTarget.disabled = true;
      try {
        const r = await api('zai/accounts', {
          method: 'POST',
          body: JSON.stringify({ name: label, secret, provider: addProvider.peek() }),
        });
        nameInput.value = ''; secretInput.value = '';
        await loadZai();
        await onAdded?.({ provider: 'zai', id: r.id, label, mode: r.mode });
      } catch (e) { toast(e.message, 'fail'); }
      finally { ev.currentTarget.disabled = false; }
    },
  }, icon('check'), '入池');

  // OAuth 进行中状态（订阅式更新，不依赖响应式重渲染）
  const flowBox = h('div');
  const paintFlow = () => {
    if (!flowBox.isConnected) return;
    const f = oauth.peek();
    if (!f) { flowBox.replaceChildren(); return; }
    flowBox.replaceChildren(
      f.url
        ? h('div', { class: 'stack', style: { gap: '8px' } },
          h('div', { class: 'url-box', text: f.url }),
          h('div', { class: 'row wrap', style: { gap: '8px' } },
            h('button', {
              class: 'btn sm', onclick: async () => {
                try { await copyText(f.url); toast('链接已复制'); } catch { toast('复制失败', 'fail'); }
              },
            }, icon('copy'), '复制链接'),
            h('button', { class: 'btn sm', onclick: () => window.open(f.url, '_blank') }, '在浏览器打开'),
            h('button', { class: 'btn sm ghost', onclick: () => { stopOAuthPoll(); oauth.set(null); paintFlow(); } }, '取消'),
          ),
          h('div', { class: 'busy', text: f.status }),
        )
        : h('div', { class: 'muted', style: { fontSize: '12px' }, text: f.status }),
    );
  };
  oauth.subscribe(paintFlow);
  // 打开表单时若已有进行中的授权，先把状态画出来（等挂载完再画，isConnected 才为真）
  setTimeout(paintFlow, 0);

  // 授权完成的回执：轮询可能是在这一屏没开着的时候跑完的，所以订阅信号，
  // 并按时间戳只认「这个节点建立之后」的那一次。
  if (showOAuth) {
    const since = Date.now();
    let settled = false;
    const off = zaiLastAdded.subscribe(v => {
      if (settled || !v || v.at < since) return;
      settled = true;
      off();
      onAdded?.(v.ref);
    });
  }

  const oauthBtn = h('button', {
    class: 'btn primary',
    onclick: () => { startOAuth(nameInput.value.trim()); paintFlow(); },
  }, icon('plus'), 'OAuth 免密登录');

  const paintProvider = () => {
    const p = addProvider.peek();
    oauthBtn.hidden = p !== 'zai';
    hintEl.textContent = p === 'bigmodel'
      ? '智谱开放平台（open.bigmodel.cn）的 API Key，走 Anthropic 兼容端点。'
      : 'JWT 走 Plan 通道（消耗订阅额度，需配置验证码求解器）；API Key 走回退通道（免验证码）。';
  };
  // 选择器是非受控的：换了平台要自己重画一遍，否则说明文字与 OAuth 按钮不跟着变
  providerSel.addEventListener('change', () => { addProvider.set(providerSel.value); paintProvider(); });
  paintProvider();

  const parts = [];
  if (showSecret) {
    parts.push(h('div', { class: 'row wrap', style: { gap: '8px' } }, providerSel, nameInput));
  } else {
    parts.push(h('div', { class: 'row wrap', style: { gap: '8px' } }, nameInput));
  }
  if (showOAuth) {
    parts.push(h('div', { class: 'stack', style: { marginTop: '12px', gap: '8px' } },
      h('div', { class: 'row wrap', style: { gap: '8px' } }, oauthBtn,
        h('span', { class: 'muted', style: { fontSize: '11.5px', alignSelf: 'center' },
          text: '浏览器登录 → 自动入池（同时兑换回退 Key）' })),
      h('div', { class: 'muted', style: { fontSize: '11.5px' },
        text: '登录在智谱自己的页面上完成。若该页面收不到短信验证码，' +
          '可改用「粘贴 JWT 或 API Key」那一种方式——功能完全一样。' }),
      flowBox));
  }
  if (showSecret) {
    parts.push(h('div', { class: 'stack', style: { marginTop: '12px', gap: '8px' } },
      secretInput, hintEl,
      h('div', { class: 'row' }, submitBtn)));
  }
  return h('div', { class: 'glass-flat', style: { padding: '14px' } }, ...parts);
}

// 模块加载即尝试续上未完成的 OAuth 轮询（与 addwizard.js 的腾讯授权同口径）：
// 用户在智谱页面登录期间刷新了面板，回来时账号仍应自动入池，而不是没人接着轮询。
resumeOAuth();
