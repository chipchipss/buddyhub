/* ══════════════════════════════════════════════════════════════════
   boot.js · 启动
   注册视图 → 密钥门 → 启动壳层。ES 模块入口（index.html 唯一 <script>）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, setKey, getKey, setUnauthorizedHandler, api, toast, materialize } from './kernel.js';
import { startShell } from './shell.js';
import { overview, refreshOverview } from './store.js';
import { openAddAccount } from './drawers.js';

// 视图注册（import 即注册，顺序决定侧栏顺序）
import './views/overview.js';
import './views/accounts.js';
import './views/usage.js';
import './views/tasks.js';
import './views/models.js';
import './views/keys.js';
import './views/config.js';
import './views/logs.js';

/* ── 密钥门 ───────────────────────────────────────────────────── */
let gateEl = null;

function openGate() {
  if (gateEl) return;
  const input = h('input', {
    class: 'input', type: 'password', placeholder: 'api_key',
    autocomplete: 'current-password',
    onkeydown: ev => { if (ev.key === 'Enter') submit(); },
  });
  const errLine = h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '8px', color: 'var(--fg-2)' } });

  const submit = async () => {
    const v = input.value.trim();
    if (!v) return;
    setKey(v);
    try {
      await api('overview');
      gateEl.remove();
      gateEl = null;
      refreshOverview();
    } catch {
      errLine.textContent = '密钥不正确，请重试';
      input.focus();
    }
  };

  const sheet = h('div', { class: 'sheet' },
    h('div', { class: 'row' }, icon('lock', 18), h('h2', { text: '需要访问密钥' })),
    h('div', { class: 'hint', text: '该网关已启用 api_key 鉴权，请输入 config.json 中的密钥。' }),
    h('div', { style: { marginTop: '16px' } }, input),
    errLine,
    h('div', { class: 'row', style: { marginTop: '18px', justifyContent: 'flex-end' } },
      h('button', { class: 'btn primary', text: '进入', onclick: submit }),
    ),
  );
  gateEl = h('div', { class: 'sheet-wrap' }, sheet);
  document.body.append(gateEl);
  gateEl.classList.add('on');
  materialize(sheet, { open: true });   // 玻璃材质到达，而不是单纯淡入
  setTimeout(() => input.focus(), 50);
}

setUnauthorizedHandler(openGate);

/* ── 启动 ─────────────────────────────────────────────────────── */
window.__openAddAccount = openAddAccount;

// URL 参数注入（登录脚本 / 预览用）：?key=xxx&theme=dark
const params = new URLSearchParams(location.search);
const urlKey = params.get('key');
if (urlKey) setKey(urlKey);

startShell();

// 有密钥时先探一次：401 会触发密钥门；无密钥且后端要求鉴权时也会触发
if (getKey()) refreshOverview(false);
else if (overview() === null) {
  // 未配置密钥时后端可能不鉴权：探一次，401 自然弹门
  api('overview').then(d => overview.set(d)).catch(() => { /* 密钥门已由 handler 打开 */ });
}
