'use strict';
/* ═══════════════════════════════════════════════════════════════════
   BuddyHub Console · 10-core.js
   全局状态 / 主题 / 请求封装 / Toast / 转义与相对时间
   所有模块为经典脚本（无模块系统），按 jsOrder 拼接后共享词法作用域。
   ═══════════════════════════════════════════════════════════════════ */

/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'buddyhub.key', LS_THEME = 'buddyhub.theme';
const _urlParams = new URLSearchParams(location.search);
const _themeParam = _urlParams.get('theme');
const _keyParam = _urlParams.get('key');
if (_keyParam) { try { localStorage.setItem(LS_KEY, _keyParam); } catch (e) {} }
let theme = _themeParam === 'light' || _themeParam === 'dark'
  ? _themeParam
  : (localStorage.getItem(LS_THEME) || 'auto'); // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 三态（auto/light/dark）存 localStorage；applyTheme 解析为可见外观。
   初始 auto 跟随系统，点击在两个可见外观间翻转。 */
function effTheme() {
  return theme === 'auto'
    ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark')
    : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  const ico = $('icoTheme');
  if (ico) ico.innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  const btn = $('btnTheme');
  if (btn) btn.title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
if ($('btnTheme')) $('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  try { localStorage.setItem(LS_THEME, theme); } catch (e) {}
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  let k = null;
  try { k = localStorage.getItem(LS_KEY); } catch (e) {}
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
/* esc 文本/属性双安全转义：显式替换 & < > " ' 五字符（& 最先，避免二次转义）。
   不能只用 div.innerHTML——它不转义引号，字符串拼进属性时引号可闭合属性注入。 */
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || String(iso).startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() {
  const v = $('keyVeil');
  if (!v) return;
  v.classList.add('on');
  setTimeout(() => { const i = $('keyInput'); if (i) i.focus(); }, 60);
}
function bindKeyGate() {
  if (!$('btnKey')) return;
  $('btnKey').onclick = async () => {
    const v = $('keyInput').value.trim();
    if (!v) return;
    try { localStorage.setItem(LS_KEY, v); } catch (e) {}
    try {
      await api('overview');
      $('keyErr').hidden = true;
      $('keyVeil').classList.remove('on');
      start();
    } catch (e) { $('keyErr').hidden = false; }
  };
  $('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });
}

/* ── 复制（secure context 降级 execCommand）───────────────────────── */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}
