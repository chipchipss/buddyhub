/* ═══════════════════════════════════════════════════════════════════
   BuddyHub Console · 30-accounts.js
   统一账号目录 / 账号池概览与操作 / 顶部刷新
   ═══════════════════════════════════════════════════════════════════ */

/* ── 统一账号目录（全部 + 平台子目录）────────────────────────────── */
const DIR_PLAT_NAMES = {
  workbuddy: '腾讯 WorkBuddy', loomy: 'Loomy（讯飞）', qoder: 'Qoder',
  lobsterai: 'LobsterAI', raccoon: '小浣熊', codearts: 'CodeArts',
  zai: 'Z.AI 智谱', codex: 'Codex', free: '免费池',
};
let dirActive = 'all';

async function loadAccountDir() {
  const tabs = $('dirTabs'), grid = $('dirGrid');
  if (!tabs || !grid) return;
  try {
    const d = await api('accounts/dir');
    const plats = d.platforms || [];
    const counts = plats.map(p => ({ id: p.id, name: DIR_PLAT_NAMES[p.id] || p.name, n: (p.accounts || []).length }));
    const total = counts.reduce((a, c) => a + c.n, 0);
    tabs.innerHTML = [{ id: 'all', name: '全部', n: total }].concat(counts).map(c =>
      '<button class="chip' + (dirActive === c.id ? ' on' : '') + '" data-dir-plat="' + esc(c.id) + '">' +
      esc(c.name) + ' <span style="opacity:.65">' + c.n + '</span></button>').join('');
    tabs.querySelectorAll('[data-dir-plat]').forEach(b => b.onclick = () => { dirActive = b.dataset.dirPlat; loadAccountDir(); });
    $('dirSummary').textContent = total + ' 个账号 · ' + counts.filter(c => c.n > 0).length + ' 个平台';

    const shown = dirActive === 'all' ? plats : plats.filter(p => p.id === dirActive);
    const ST = { healthy: '<span class="tag ok">正常</span>', cooling: '<span class="tag warn">冷却</span>', off: '<span class="tag mute">停用</span>' };
    grid.innerHTML = shown.map(p => {
      const n = (p.accounts || []).length;
      const rows = (p.accounts || []).map(a =>
        '<tr>' +
        '<td class="mark" aria-hidden="true"><i style="background:' + (a.status === 'healthy' ? 'var(--ok)' : a.status === 'cooling' ? 'var(--warn)' : 'var(--ink-3)') + '"></i></td>' +
        '<td><div class="who"><div class="nm">' + esc(a.label || a.id) + '</div><div class="id">' + esc(a.id) + '</div></div></td>' +
        '<td>' + (ST[a.status] || '<span class="tag mute">未知</span>') + '</td>' +
        '<td class="num">' + esc(a.quota || '—') + '</td>' +
        '<td style="color:var(--ink-3); font-size:12px">' + esc(a.detail || '') + '</td>' +
        '<td class="c-acts"><a class="link" href="' + (a.manage_to || '#accounts') + '">管理 →</a></td>' +
        '</tr>').join('');
      const apiTag = p.api_model
        ? '<span class="tag info">API ' + esc(p.api_model) + '*</span>'
        : '<span class="tag mute">无对话 API</span>';
      return '<div class="dir-card">' +
        '<div class="dir-hd"><span class="nm">' + esc(DIR_PLAT_NAMES[p.id] || p.name) + '</span>' + apiTag +
        '<span class="cnt">' + n + ' 个账号</span><span class="grow"></span>' +
        '<a class="link" href="' + (p.manage_to || '#accounts') + '">平台管理 →</a></div>' +
        (n
          ? '<div class="tbl-wrap"><table class="acc dir-tbl"><thead><tr><th class="mark" aria-hidden="true"></th><th>账号</th><th>状态</th><th>额度</th><th>备注</th><th></th></tr></thead><tbody>' + rows + '</tbody></table></div>'
          : '<div class="dir-empty">暂无账号 —— 添加入口见平台管理页</div>') +
        '</div>';
    }).join('') || '<div class="empty">该平台暂无账号</div>';
  } catch (e) {
    grid.innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
  }
}
if ($('btnDirRefresh')) $('btnDirRefresh').onclick = loadAccountDir;

/* ── 账号池表格 ─────────────────────────────────────────────────── */
function renderAccounts(list) {
  const tb = $('accBody');
  if (!tb) return;
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    return;
  }
  // 有总额度 → 进度条按 剩余/总额；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>';
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    const cred = s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits));
    const pct = s.credits_total > 0
      ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
      : Math.round((s.credits || 0) / maxCred * 100);
    // 积分条语义变色：有总额时低于 25% 警示 / 10% 危险
    const barCls = s.credits_total > 0 ? (pct <= 10 ? ' bar bad' : pct <= 25 ? ' bar warn' : '') : '';
    // 成本台账 tooltip：每模型实测单价（≤0 = 实测免费）
    let credTip = s.credits_total > 0 ? '剩余 ' + s.credits + ' / 总额 ' + s.credits_total + '（' + pct + '%）' : '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar' + barCls + '"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '">签到</button>' +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '">余额</button>' +
        '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>' +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</td></tr>';
  }).join('');
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
    $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    $('sSticky').textContent = d.sticky_sessions;
    const nv = $('navSub');
    if (nv) nv.textContent = 'v' + d.version;
    const nvv = $('navVer');
    if (nvv) nvv.textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

/* ── 账号行操作（事件委托）──────────────────────────────────────── */
function bindAccountActions() {
  const tb = $('accBody');
  if (!tb) return;
  tb.addEventListener('click', async ev => {
    const b = ev.target.closest('button[data-a]');
    if (!b) return;
    const u = b.dataset.u, a = b.dataset.a;
    if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
    if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
    b.disabled = true;
    try {
      if (a === 'checkin') {
        const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
        toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
      } else if (a === 'balance') {
        const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
        toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
      } else if (a === 'revive') {
        await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
        toast('已解冻', 'ok');
      } else if (a === 'disable') {
        await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
        toast('已禁用', 'ok');
      } else if (a === 'tasks') {
        openTasks(u);
      } else if (a === 'remove') {
        const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
        toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
      }
    } catch (e) { toast(e.message, 'err'); }
    finally { b.disabled = false; loadOverview(true); }
  });
}

/* ── 池级批量操作 ───────────────────────────────────────────────── */
function bindPoolActions() {
  const bind = (id, path, msg) => {
    const b = $(id);
    if (!b) return;
    b.onclick = async () => {
      try { await api(path, { method: 'POST' }); toast(msg, 'ok'); }
      catch (e) { toast(e.message, 'err'); }
    };
  };
  bind('btnCheckinAll', 'checkin_all', '全部签到已开始，结果见日志');
  bind('btnKeepaliveAll', 'keepalive_all', '全部保活已开始，结果见日志');
  bind('btnTravelAll', 'travel_all', '旅行巡检已开始（含领养链路），结果见日志');
  bind('btnActivityAll', 'activity_all', '活跃上报已开始，结果见日志');
}

/* ── 顶部刷新（余额全量刷新）────────────────────────────────────── */
function bindTopRefresh() {
  const b = $('btnRefresh');
  if (!b) return;
  b.onclick = async () => {
    b.disabled = true; const t = b.textContent; b.textContent = '刷新中…';
    try {
      await api('balance_all', { method: 'POST' });
      await loadOverview(true);
      toast('余额已从上游刷新', 'ok');
    } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
    finally { b.disabled = false; b.textContent = t; }
    if (view === 'logs') loadLogs();
  };
}
