/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

function usBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-wrapbar">' +
    '<span class="bar bar-p" style="width:' + (pp * 0.8).toFixed(1) + 'px" title="prompt"></span>' +
    '<span class="bar bar-c" style="width:' + Math.max(2, pc * 0.8).toFixed(1) + 'px" title="completion"></span>' +
    '</span>';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制是否追加延迟/速率两列——只有「按账号」表的表头带这两列；
   模型表与域表没有，多输出会造成列错位。早先靠「mid 是否为 undefined」隐式
   判断，调用方稍一改动就会错列，故改为显式参数。 */
function usRow(name, sub, a, mid, withPerf) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

function renderUsage(d) {
  const t = d.totals || {};
  $('usStats').innerHTML =
    usStat(fmtTok(t.requests), '请求数') +
    usStat(fmtTok(t.total_tokens), '总 token') +
    usStat(fmtTok(t.prompt_tokens), 'prompt') +
    usStat(fmtTok(t.completion_tokens), 'completion') +
    usStat(t.errors ? String(t.errors) : '0', '失败尝试', t.errors ? 'warn' : '') +
    usStat(fmtMs(t.avg_latency_ms), '平均延迟');

  // 卡片、三张表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = ($('usWindow') && $('usWindow').selectedOptions[0]) ?
    $('usWindow').selectedOptions[0].textContent.trim() : '';
  $('usNote').textContent =
    (winLabel ? winLabel + ' · ' : '') +
    (d.buckets || 0) + ' 个分桶' +
    (d.since ? ' · 数据自 ' + d.since.replace('T', ' ') : '') +
    (d.file_bytes ? ' · 文件 ' + (d.file_bytes / 1024).toFixed(1) + ' KB' : '');

  $('usAccBody').innerHTML = (d.by_account || []).map(x =>
    usRow(x.key.slice(0, 8), x.extra || '', x,
      '<td class="num">' + esc(x.realm || '') + '</td>', true)
  ).join('') || '<tr><td colspan="10" class="empty">暂无数据</td></tr>';

  $('usModelBody').innerHTML = (d.by_model || []).map(x =>
    usRow(x.key, '', x, '', false)).join('') || '<tr><td colspan="7" class="empty">暂无数据</td></tr>';

  $('usRealmBody').innerHTML = (d.by_realm || []).map(x =>
    usRow(x.key, '', x, '', false)).join('') || '<tr><td colspan="7" class="empty">暂无数据</td></tr>';

  renderUsageChart(d.series || []);
}

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 760 宽的 viewBox 横向拉伸到
 * 容器宽度，柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    return;
  }

  const W = 760, H = 196, PL = 52, PR = 12, PT = 12, PB = 46;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(1.5, Math.min(30, slot * 0.7));

  const xOf = t => PL + (t - t0) / span * iw;

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 柱子
  for (const p of pts) {
    const cx = xOf(p.t);
    const x = cx - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    const yBase = PT + ih;
    if (hP > 0) out += '<rect x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="var(--accent)" rx="1.5"/>';
    if (hC > 0) out += '<rect x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="var(--ok)" rx="1.5"/>';
    out += '<title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) +
         '" y2="' + (PT + ih) + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    const d = new Date(p.t);
    const lab = p.scope === 'day'
      ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
      : String(d.getHours()).padStart(2, '0') + ':00';
    // 首尾标签靠边对齐，避免被裁掉。两侧都用「向内」锚定（end/middle），
    // start 锚定会把文字推出 viewBox 右缘被裁（"13:00"变"13"）。
    // 标签在基线下方 18px,柱子只长在基线上方,天然无重叠。
    const cx = xOf(p.t);
    let anchor = 'middle';
    if (cx < PL + 14) anchor = 'end';
    else if (cx > W - PR - 14) anchor = 'end';
    const tx = Math.max(PL - 2, Math.min(W - PR, cx)).toFixed(1);
    out += '<text class="tk" x="' + tx +
           '" y="' + (PT + ih + 18) + '" text-anchor="' + anchor + '">' + esc(lab) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

let _lastUsage = null;   // 最近一次原始快照（平台筛选在本地重渲,避免重复拉取）
function currentUsagePlat() { return ($('usPlat') && $('usPlat').value) || 'all'; }

function renderUsageFiltered() {
  if (!_lastUsage) return;
  const plat = currentUsagePlat();
  if (plat === 'all') { renderUsage(_lastUsage); return; }
  const d = JSON.parse(JSON.stringify(_lastUsage));  // 深拷贝避免污染缓存
  const match = k => k === plat || k === plat + ':' || (plat === 'cn' && (k === 'cn' || !k)) || (plat === 'global' && k === 'global');
  // 按模型行:模型 id 带 zai:/loomy: 等前缀,也按前缀匹配
  d.by_model = (d.by_model || []).filter(x => match(x.key) || x.key.startsWith(plat + ':'));
  d.by_account = (d.by_account || []).filter(x => match(x.realm || ''));
  d.by_realm = (d.by_realm || []).filter(x => match(x.key));
  // totals 重算
  const t = { requests: 0, total_tokens: 0, prompt_tokens: 0, completion_tokens: 0, errors: 0 };
  for (const x of d.by_account) {
    t.requests += x.requests || 0; t.total_tokens += x.total_tokens || 0;
    t.prompt_tokens += x.prompt_tokens || 0; t.completion_tokens += x.completion_tokens || 0;
    t.errors += x.errors || 0;
  }
  t.avg_latency_ms = d.by_account.length
    ? d.by_account.reduce((a, x) => a + (x.avg_latency_ms || 0), 0) / d.by_account.length : 0;
  d.totals = t;
  d.series = (d.series || []).filter(() => true);  // series 混合 realm,平台粒度重算成本高——保留原曲线并在 note 说明
  renderUsage(d);
}

async function loadUsage() {
  const hours = ($('usWindow') && $('usWindow').value) || 72;
  try {
    const d = await api('usage?hours=' + encodeURIComponent(hours));
    _lastUsage = d;
    renderUsageFiltered();
  } catch (e) {
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
if ($('usWindow')) $('usWindow').onchange = loadUsage;
if ($('usPlat')) $('usPlat').onchange = renderUsageFiltered;
