/* ══════════════════════════════════════════════════════════════════
   store.js · 共享状态
   全局信号 + 轮询 + 主题。视图只读信号，写入集中在各 refresh 函数里。
   ══════════════════════════════════════════════════════════════════ */

import { signal, api, toast } from './kernel.js';

const LS_THEME = 'buddyhub.theme';
const urlParams = new URLSearchParams(location.search);
const urlTheme = urlParams.get('theme');
if (urlTheme === 'light' || urlTheme === 'dark') {
  try { localStorage.setItem(LS_THEME, urlTheme); } catch { /* 私密模式 */ }
}

/* ── 主题：只有明暗两态，首次跟随系统 ─────────────────────────── */
function storedTheme() {
  try { return localStorage.getItem(LS_THEME); } catch { return null; }
}
export const theme = signal(
  storedTheme() === 'light' || storedTheme() === 'dark'
    ? storedTheme()
    : (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark')
);

export function applyTheme() {
  document.documentElement.dataset.theme = theme.peek();
}

/** setTheme(mode) —— 明暗由「设置 · 外观」决定，只存浏览器本地。 */
export function setTheme(mode) {
  if (mode !== 'light' && mode !== 'dark') return;
  theme.set(mode);
  try { localStorage.setItem(LS_THEME, mode); } catch { /* 私密模式 */ }
}

export function toggleTheme() {
  setTheme(theme.peek() === 'light' ? 'dark' : 'light');
}

/* ── 概览（账号池快照）───────────────────────────────────────── */
export const overview = signal(null);
export const overviewErr = signal(null);

export async function refreshOverview(quiet = true) {
  try {
    const d = await api('overview');
    overview.set(d);
    overviewErr.set(null);
    return d;
  } catch (e) {
    if (!quiet) { overviewErr.set(e.message); toast(e.message, 'fail'); }
    return null;
  }
}

/* ── 当前一级页 ───────────────────────────────────────────────── */
// 初始值取 hash 的 page 部分（#accounts?seg=credits&tab=zai → accounts）；
// 旧 hash 与分段由 shell.applyHash() 在启动时归一，这里只当占位。
export const viewId = signal(((location.hash || '#home').slice(1).split('?')[0]) || 'home');
