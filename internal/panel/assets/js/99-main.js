/* ═══════════════════════════════════════════════════════════════════
   BuddyHub Console · 99-main.js
   视图路由 / 顶部标题 / 自动化分组折叠 / 全局轮询 / 启动引导
   必须最后拼接：start() 依赖此前各模块定义的函数。
   ═══════════════════════════════════════════════════════════════════ */

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = {
  accounts: '账号池', usage: '用量', packages: '积分对比',
  taskscenter: '任务 · 腾讯', loomy: '任务 · Loomy', ext: '任务 · 外部平台',
  models: '模型与档位', apikeys: 'API 密钥', config: '配置', logs: '运行日志',
};
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  // 自动化二级目录：进入子页自动展开，离开不强制收起（用户手动控制）
  if (['taskscenter', 'loomy', 'ext'].includes(v)) setAutoGroup(true);
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'taskscenter') reattachQueueView();
  if (v === 'loomy') { if (typeof loadLoomyStatus === 'function') loadLoomyStatus(true); }
  if (v === 'ext') { if (typeof loadExtAccounts === 'function') loadExtAccounts(true); }
  if (v === 'apikeys') { if (typeof loadAPIKeys === 'function') loadAPIKeys(); }
  if (v === 'accounts') { if (typeof loadAccountDir === 'function') loadAccountDir(); }
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => {
  e.preventDefault();
  go(a.dataset.view);
  history.replaceState(null, '', '#' + a.dataset.view);
});
// 浏览器前进/后退、手动改 hash 也跟随切换（go 幂等，同视图重复调用无害）
addEventListener('hashchange', () => {
  const v = (location.hash || '#accounts').slice(1);
  if (v in TITLES) go(v);
});

/* 自动化分组折叠 */
function setAutoGroup(open) {
  const t = $('autoGroupToggle');
  if (!t) return;
  t.classList.toggle('open', open);
  t.setAttribute('aria-expanded', open ? 'true' : 'false');
  document.querySelectorAll('.nav li.sub').forEach(li => li.classList.toggle('show', open));
}
if ($('autoGroupToggle')) $('autoGroupToggle').onclick = () => setAutoGroup(!$('autoGroupToggle').classList.contains('open'));

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') reattachQueueView();
}
function start() {
  loadOverview(true);
  // 认证成功后重载当前视图：首次 go() 发生在鉴权前，401 留下的错误态（如
  // 账号目录的「密钥无效」）需要清掉。go 幂等，重跑视图加载逻辑即可。
  go(view);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}

/* ── 启动 ─────────────────────────────────────────────────────────── */
bindKeyGate();
bindAccountActions();
bindPoolActions();
bindTopRefresh();
go((location.hash || '#accounts').slice(1) in TITLES ? (location.hash || '#accounts').slice(1) : 'accounts');
