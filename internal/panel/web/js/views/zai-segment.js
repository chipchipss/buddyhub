/* ══════════════════════════════════════════════════════════════════
   views/zai-segment.js · 账号 → Z.AI（ZCode）
   Z.AI 账号池：Plan（JWT，需验证码）与 API Key 回退通道的统一管理面。
   由 views/accounts.js 装配（也可独立复用）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog, copyText } from '../kernel.js';
import { flowStore, pollLoop } from '../loginflow.js';
import { statusOf } from '../status.js';

const zai = signal(null);
const zaiErr = signal('');
const addOpen = signal(false);
const addName = signal('');
const addSecret = signal('');
const addProvider = signal('zai');
const oauth = signal(null);   // { url, flowId, status }

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
        addOpen.set(false);
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
async function startOAuth() {
  stopOAuthPoll();
  oauth.set({ status: '正在获取授权链接…' });
  try {
    const r = await api('zai/oauth/start', {
      method: 'POST', body: JSON.stringify({ name: addName.peek().trim() }),
    });
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

async function act(id, action, body, okMsg) {
  try {
    const r = await api(`zai/accounts/${encodeURIComponent(id)}/${action}`, {
      method: 'POST', body: body ? JSON.stringify(body) : undefined,
    });
    toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
    await loadZai();
    return r;
  } catch (e) {
    toast(e.message, 'fail');
    return null;
  }
}

/** 额度数字缩写（1.2m / 340k）。 */
function fmtNum(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

// 状态 chip 用统一组件（status.js）：active→可用 / cooling→冷却中 /
// exhausted→额度用完 / invalid→凭证失效 / disabled→已停用。
function statusChip(a) {
  return statusOf(a);
}

// 逐模型健康 chip：模型上游抖动/限流/额度用完时，只冷却该模型（账号级状态不动）。
const PENALTY_LABEL = { server: '上游抖动', rate: '限流', exhausted: '额度用完' };
function modelHealthChip(mh) {
  if (!mh) return null;
  const left = mh.until ? Math.max(0, Math.round((new Date(mh.until).getTime() - Date.now()) / 1000)) : 0;
  const label = PENALTY_LABEL[mh.kind] || '冷却';
  const text = left > 0 ? `${label} 冷却 ${left}s` : label;
  return h('span', {
    class: 'chip',
    title: (mh.last_err || '') + (mh.fails ? `（连续 ${mh.fails} 次）` : ''),
    style: { fontSize: '10.5px', color: 'var(--fg)', opacity: '0.9' },
    text,
  });
}

function accountCard(a) {
  const fp = a.fingerprint || {};
  const quota = a.quota || {};
  const health = a.model_health || {};
  // 模型行取「额度 ∪ 健康」并集：API Key 号没有额度窗口，但抖动时的逐模型冷却仍要显示。
  const models = Array.from(new Set([...Object.keys(quota), ...Object.keys(health)]));
  return h('article', { class: 'acct' + (a.enabled ? '' : ' off') },
    h('div', { class: 'top' },
      h('div', { class: 'who' },
        h('div', { class: 'nm', text: a.name }),
        h('div', { class: 'id', text: a.masked || '' }),
      ),
      h('div', { class: 'row', style: { gap: '5px', flex: 'none' } },
        h('span', { class: 'chip faint', text: a.mode === 'jwt' ? 'Plan JWT' : 'API Key' }),
        statusChip(a),
      ),
    ),

    h('div', { class: 'metrics' },
      h('div', { class: 'metric' },
        h('div', { class: 'v', text: String(a.use_count || 0) }), h('div', { class: 'k', text: '调用' })),
      h('div', { class: 'metric' },
        h('div', { class: 'v', text: String(a.fail_count || 0) }), h('div', { class: 'k', text: '失败' })),
      h('div', { class: 'metric' },
        h('div', { class: 'v', text: String(a.in_flight || 0) }), h('div', { class: 'k', text: '在途' })),
    ),

    models.length
      ? h('div', { class: 'stack', style: { gap: '8px', marginTop: '10px' } },
        ...models.map((model) => {
          const q = quota[model];
          const mh = health[model];
          const kids = [];
          kids.push(h('div', { class: 'row', style: { justifyContent: 'space-between', fontSize: '11.5px' } },
            h('span', null,
              model,
              mh ? h('span', { style: { marginLeft: '6px' } }, modelHealthChip(mh)) : null),
            q ? (() => {
              const total = Number(q.total || 0), remain = Number(q.remaining || 0);
              const exp = q.expires_at ? new Date(q.expires_at) : null;
              const expSoon = exp && exp.getTime() - Date.now() < 3 * 86400000;
              return h('span', { class: 'muted' },
                `${fmtNum(remain)} / ${fmtNum(total)}`,
                exp ? h('span', { style: { marginLeft: '6px', color: expSoon ? 'var(--fg)' : 'var(--fg-3)' },
                  text: expSoon ? '即将到期' : String(q.expires_at).slice(0, 10) }) : null);
            })() : null,
          ));
          if (q) {
            const total = Number(q.total || 0), remain = Number(q.remaining || 0);
            const pct = total > 0 ? Math.min(100, Math.round(remain / total * 100)) : 0;
            kids.push(h('div', { class: 'meter thin', style: { marginTop: '3px' } },
              h('i', { style: { width: pct + '%' } })));
          }
          return h('div', null, ...kids);
        }))
      : h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '10px' },
        text: a.mode === 'jwt' ? '尚未查询额度' : 'API Key 通道无额度窗口' }),

    h('div', { class: 'row wrap', style: { gap: '5px', marginTop: '10px' } },
      h('span', { class: 'chip faint', title: 'X-Platform / X-Os-Version / X-Device-Mid' },
        `${fp.platform || '?'}-${fp.arch || '?'} · ${fp.os_version || '?'}`),
      a.has_key_fallback ? h('span', { class: 'chip faint', text: '带回退 Key' }) : null,
      a.risk_strikes ? h('span', { class: 'chip', text: `风控 ${a.risk_strikes} 次` }) : null,
    ),

    a.last_error
      ? h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '8px' }, text: a.last_error })
      : null,

    h('div', { class: 'acts' },
      a.mode === 'jwt'
        ? h('button', {
          class: 'btn', title: '查询 Coding Plan 额度（billing 族）',
          onclick: () => act(a.id, 'quota', null, r => {
            const q = r.result && r.result.balance ? '已刷新额度' : '额度已更新';
            return q;
          }),
        }, '额度')
        : null,
      a.mode === 'jwt'
        ? h('button', {
          class: 'btn primary', title: '领取活动套餐（需验证码求解器；上游 WAF 敏感，勿频繁点）',
          onclick: async ev => {
            ev.currentTarget.disabled = true;
            ev.currentTarget.textContent = '领取中…';
            try {
              const r = await api('zai/accounts/' + encodeURIComponent(a.id) + '/claim', { method: 'POST' });
              toast(r.message || '领取完成');
              for (const o of (r.outcomes || [])) {
                if (!o.ok && o.message) toast((o.plan_name || o.plan_id) + '：' + o.message, 'fail');
              }
              await loadZai();
            } catch (e) { toast(e.message, 'fail'); }
            finally { ev.currentTarget.disabled = false; ev.currentTarget.textContent = '领取套餐'; }
          },
        }, '领取套餐')
        : null,
      h('button', {
        class: 'btn', onclick: () => act(a.id, 'toggle', null, r => r.enabled ? '已启用' : '已停用'),
      }, a.enabled ? '停用' : '启用'),
      h('button', {
        class: 'btn', title: '换发整套桌面设备指纹（新 device_mid）',
        onclick: () => act(a.id, 'rotate', null, '已换发设备指纹'),
      }, '换指纹'),
      h('button', {
        class: 'btn danger', onclick: async () => {
          if (!await confirmDialog(`删除 Z.AI 账号「${a.name}」？`, { ok: '删除' })) return;
          await act(a.id, 'remove', null, '已删除');
        },
      }, '删除'),
    ),
  );
}

function addPanel() {
  if (!addOpen()) {
    return h('div', { class: 'row' },
      h('button', { class: 'btn primary', onclick: () => addOpen.set(true) }, icon('plus'), '添加 Z.AI 账号'),
    );
  }
  return h('div', { class: 'stack' },
    zaiAddForm(() => { addOpen.set(false); loadZai(); }),
    h('div', { class: 'row', style: { marginTop: '4px' } },
      h('button', { class: 'btn ghost', onclick: () => { stopOAuthPoll(); oauth.set(null); addOpen.set(false); } }, '取消'),
    ),
  );
}

/** zaiAddForm(onAdded) —— Z.AI / 智谱的「添加账号入池」表单（可独立嵌入抽屉）。
 *  整块命令式管理：输入框是非受控的（值在 DOM 里），provider/OAuth 的局部刷新
 *  走订阅式重绘——避免响应式重渲染把正在输入的名称/密钥清空。 */
export function zaiAddForm(onAdded) {
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
      ev.currentTarget.disabled = true;
      try {
        const r = await api('zai/accounts', {
          method: 'POST',
          body: JSON.stringify({ name: nameInput.value.trim(), secret, provider: addProvider.peek() }),
        });
        const kind = r.mode === 'jwt' ? 'Plan JWT' : 'API Key';
        toast('已入池（识别为 ' + kind + '）');
        nameInput.value = ''; secretInput.value = '';
        await loadZai();
        await onAdded?.();
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
            h('button', { class: 'btn sm primary', onclick: () => window.open(f.url, '_blank') }, '在浏览器打开'),
            h('button', { class: 'btn sm ghost', onclick: () => { stopOAuthPoll(); oauth.set(null); paintFlow(); } }, '取消'),
          ),
          h('div', { class: 'busy', text: f.status }),
        )
        : h('div', { class: 'muted', style: { fontSize: '12px' }, text: f.status }),
    );
  };
  oauth.subscribe(paintFlow);

  const oauthBtn = h('button', {
    class: 'btn primary',
    onclick: () => { startOAuth(); paintFlow(); },
  }, icon('plus'), 'OAuth 免密登录');

  const paintProvider = () => {
    const p = addProvider.peek();
    oauthBtn.hidden = p !== 'zai';
    hintEl.textContent = p === 'bigmodel'
      ? '智谱开放平台（open.bigmodel.cn）的 API Key，走 Anthropic 兼容端点。'
      : 'JWT 走 Plan 通道（消耗订阅额度，需配置验证码求解器）；API Key 走回退通道（免验证码）。';
  };
  return h('div', { class: 'glass-flat', style: { padding: '14px' } },
    h('div', { class: 'row wrap', style: { gap: '8px' } }, providerSel, nameInput),
    h('div', { class: 'stack', style: { marginTop: '12px' } },
      h('div', { class: 'row wrap', style: { gap: '8px' } }, oauthBtn,
        h('span', { class: 'muted', style: { fontSize: '11.5px', alignSelf: 'center' },
          text: '浏览器登录 → 自动入池（同时兑换回退 Key）' })),
      h('div', { class: 'muted', style: { fontSize: '11.5px' },
        text: '登录在智谱自己的页面上完成。若该页面收不到短信验证码，' +
          '可直接用下方的 Coding Plan JWT 或 API Key 入池——功能完全一样。' }),
      flowBox,
      secretInput,
      hintEl,
      h('div', { class: 'row', style: { marginTop: '4px' } }, submitBtn),
    ),
  );
}

export function zaiSegment() {
  // 页面重载后把未完成的 OAuth 轮询续上（有则续，无则什么都不做）
  resumeOAuth();
  // 读信号而非 peek：渲染期读到的信号变化才会触发重渲染（loadZai 回来后本段自动刷新）
  const d = zai();
  const errV = zaiErr();
  if (d === null && !errV) loadZai();
  const captcha = (d && d.captcha) || null;

  if (errV) {
    return h('section', { class: 'card' }, h('div', { class: 'body' },
      h('div', { class: 'empty' }, h('div', { class: 'd', text: errV }))));
  }
  if (!d) return h('section', { class: 'card' }, h('div', { class: 'busy', text: '读取 Z.AI 账号池' }));

  return h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('header', null,
        h('h2', { text: 'Z.AI / ZCode 账号池' }),
        h('span', { class: 'grow' }),
        h('span', { class: 'hint', text: d.configured ? `${(d.accounts || []).length} 个账号` : '' }),
        h('button', {
          class: 'btn sm ghost', title: '刷新全部 JWT 账号额度（billing 族，注意上游 WAF 限流）',
          onclick: async ev => {
            ev.currentTarget.disabled = true;
            try {
              const r = await api('zai/quota_all', { method: 'POST' });
              toast(`额度刷新完成：成功 ${r.success} · 失败 ${r.failed}`);
              await loadZai();
            } catch (e) { toast(e.message, 'fail'); }
            finally { ev.currentTarget.disabled = false; }
          },
        }, icon('wallet'), '刷新额度'),
        h('button', { class: 'btn sm ghost', onclick: () => loadZai(false) }, icon('refresh'), '刷新'),
      ),
      h('div', { class: 'body stack' },
        d.configured ? null : h('div', { class: 'muted', style: { fontSize: '12.5px' }, text: d.message || '' }),

        // 验证码池状态（Plan 通道的前提）
        captcha
          ? h('div', { class: 'row wrap', style: { gap: '8px' } },
            h('span', { class: 'chip ' + (captcha.enabled ? 'strong' : 'faint') },
              h('i', { class: captcha.enabled ? 'dot' : 'dot off' }),
              captcha.enabled ? `验证码求解器已启用 · 池内 ${captcha.pool_size} 枚` : '未配置验证码求解器'),
            captcha.last_error
              ? h('span', { class: 'chip faint', text: captcha.last_error })
              : null)
          : null,

        addPanel(),

        (d.accounts || []).length
          ? h('div', { class: 'acct-grid' }, ...d.accounts.map(accountCard))
          : h('div', { class: 'empty' }, icon('accounts'),
            h('div', { class: 't', text: '还没有 Z.AI 账号' }),
            h('div', { class: 'd', text: '粘贴 Coding Plan JWT（Plan 通道）或 API Key（回退通道）即可入池' })),
      ),
    ),
  );
}
