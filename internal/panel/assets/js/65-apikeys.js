/* ── API 密钥管理 ─────────────────────────────────────────────────── */
let akPlatforms = new Set(['*']);  // 生成表单当前勾选平台
const AK_PLAT_NAMES = { workbuddy: '腾讯', loomy: 'Loomy', qoder: 'Qoder', zai: 'Z.AI', codex: 'Codex', free: '免费池' };

function renderAKPlatforms() {
  document.querySelectorAll('#nkPlatforms .seg-btn').forEach(b => {
    b.classList.toggle('on', akPlatforms.has(b.dataset.plat));
  });
}
if ($('nkPlatforms')) document.querySelectorAll('#nkPlatforms .seg-btn').forEach(b => {
  b.onclick = () => {
    const p = b.dataset.plat;
    if (p === '*') { akPlatforms = new Set(['*']); }        // 全平台独占
    else {
      akPlatforms.delete('*');
      akPlatforms.has(p) ? akPlatforms.delete(p) : akPlatforms.add(p);
      if (akPlatforms.size === 0) akPlatforms = new Set(['*']);
    }
    renderAKPlatforms();
  };
});

async function loadAPIKeys() {
  const grid = $('akGrid');
  try {
    const d = await api('apikeys');
    const keys = d.keys || [];
    if (!keys.length) {
      grid.innerHTML = '<div class="empty" style="padding:18px">还没有生成过 Key —— 上方填写名称后点「生成新 Key」</div>';
      return;
    }
    grid.innerHTML = keys.map(k => {
      const plats = (!k.platforms || !k.platforms.length || k.platforms.includes('*'))
        ? '<span class="tag ok">全平台</span>'
        : k.platforms.map(p => '<span class="tag info">' + (AK_PLAT_NAMES[p] || p) + '</span>').join(' ');
      return '<div style="background: var(--raise); border-radius: 14px; padding: 11px 14px; display:flex; align-items:center; gap:10px; flex-wrap:wrap;">' +
        '<div style="flex:1; min-width:200px">' +
          '<div style="font-weight:600; font-size:13px">' + esc(k.name || '未命名') +
            (k.note ? ' <span style="color:var(--ink-3); font-weight:400; font-size:11.5px">· ' + esc(k.note) + '</span>' : '') + '</div>' +
          '<div style="font-family:var(--mono); font-size:11.5px; color:var(--accent); margin-top:2px; user-select:all">' + esc(k.key) + '</div>' +
        '</div>' +
        '<div style="display:flex; gap:4px; flex-wrap:wrap">' + plats + '</div>' +
        '<button class="xs danger" data-ak-del="' + esc(k.key) + '">删除</button>' +
      '</div>';
    }).join('');
    grid.querySelectorAll('[data-ak-del]').forEach(b => b.onclick = async () => {
      if (!confirm('删除该 Key？使用它的客户端将立即失效。')) return;
      try {
        await api('apikeys/delete', { method: 'POST', body: JSON.stringify({ key: b.dataset.akDel }) });
        toast('Key 已删除', 'ok');
        loadAPIKeys();
      } catch (e) { toast(e.message, 'err'); }
    });
  } catch (e) {
    grid.innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
  }
}
if ($('btnKeyList')) $('btnKeyList').onclick = loadAPIKeys;

if ($('btnKeyGen')) $('btnKeyGen').onclick = async () => {
  const name = $('nkName').value.trim();
  if (!name) { toast('先填 Key 名称', 'err'); return; }
  const plats = [...akPlatforms].filter(p => p !== '*');
  const st = $('akState');
  try {
    const d = await api('apikeys', { method: 'POST', body: JSON.stringify({ name, platforms: plats, note: $('nkNote').value.trim() }) });
    st.hidden = false; st.className = 'state ok';
    st.textContent = '已生成: ' + d.key.key;
    $('nkName').value = ''; $('nkNote').value = '';
    toast('API Key 已生成', 'ok');
    loadAPIKeys();
  } catch (e) { toast(e.message, 'err'); }
};
