/* ═══════════════════════════════════════════════════════════════════
   BuddyHub Console · 35-models.js
   模型与档位：实测上限标注 / 倍率折扣 / 平台分组
   ═══════════════════════════════════════════════════════════════════ */

function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。有折扣：生效价大字 + 标签 + 划线牌价。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  if (!tb) return;
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    const list = d.models || [];
    if (!list.length) { tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>'; return; }
    const probes = pr.probes || {};
    const probeKeys = Object.keys(probes);
    const probeOf = id => probes[id] || probes[probeKeys.find(k => k.endsWith(':' + id))];
    // 按平台分组（网关拆分展示）；顺序即导航语义
    const platOf = id => id.startsWith('loomy:') ? ['loomy', 'Loomy（讯飞）']
      : id.startsWith('qoder:') ? ['qoder', 'Qoder（阿里）']
      : id.startsWith('zai:') ? ['zai', 'Z.AI 智谱 GLM']
      : id.startsWith('codex:') ? ['codex', 'Codex 订阅池']
      : id.startsWith('free:') ? ['free', '免费 Key 池']
      : ['workbuddy', '腾讯 WorkBuddy'];
    const groups = [];
    for (const m of list) {
      const pk = platOf(m.id);
      let g = groups.find(x => x.key === pk[0]);
      if (!g) { g = { key: pk[0], name: pk[1], models: [] }; groups.push(g); }
      g.models.push(m);
    }
    function rowOf(m) {
      const eff = (m.supported_efforts || []).slice();
      if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
      const effs = eff.length ? eff.map(e => '<span class="tag info no-dot">' + esc(e) + '</span>').join(' ')
        : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      const caps = [];
      if (m.is_default) caps.push('<span class="tag ok no-dot">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag info no-dot">工具</span>');
      if (m.supports_images) caps.push('<span class="tag info no-dot">视觉</span>');
      if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag mute no-dot">思考常开</span>');
      const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
        '<td class="num">' + rateCell(m) + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag ok no-dot">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '<td class="efs" style="white-space:normal">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        outCell(m, probeOf(m.id)) + '</tr>';
    }
    let html = '';
    for (const g of groups) {
      html += '<tr><td colspan="7" style="padding:10px 12px 5px">' +
        '<span class="tag info" style="vertical-align:1px; margin-right:6px">' + esc(g.name) + '</span>' +
        '<span style="font-size:11.5px; color:var(--ink-3)">共 ' + g.models.length + ' 个模型</span></td></tr>';
      html += g.models.map(rowOf).join('');
    }
    tb.innerHTML = html;
    const hit = list.filter(m => probeOf(m.id)).length;
    $('mdNote').textContent = list.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
if ($('btnModels')) $('btnModels').onclick = loadModels;
