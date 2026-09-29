/* ── 外部积分账号（LobsterAI / 小浣熊 / Qoder / 华为云） ─────────────── */
const EXT_PROVIDER_NAMES = { lobsterai: 'LobsterAI', raccoon: '小浣熊', qoder: 'Qoder', codearts: '华为云' };

async function loadExtAccounts(quiet) {
  const st = $('extState'), content = $('extContent');
  if (!st || !content) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; content.hidden = true; }
  try {
    const d = await api('ext/accounts');
    const list = d.accounts || [];
    st.hidden = true; content.hidden = false;
    const okCount = list.filter(a => a.balance_ok).length;
    $('extSummary').textContent = list.length === 0 ? '暂无账号' : (list.length + ' 个账号 · ' + okCount + ' 个余额正常');
    if (list.length === 0) {
      $('extGrid').innerHTML = '<div class="empty" style="grid-column: 1/-1; padding: 18px; text-align: center; color: var(--ink-3); font-size: 12.5px;">还没有外部账号 —— 展开下方「手工添加账号」粘贴凭据，或等待登录流程集成</div>';
      return;
    }
    $('extGrid').innerHTML = list.map(a => {
      const name = EXT_PROVIDER_NAMES[a.provider] || a.provider;
      const bal = a.balance_ok ? '<span style="font-family: var(--mono); font-size: 12px; color: var(--accent);">' + (a.balance ?? 0) + ' 分</span>'
        : '<span style="font-size: 11.5px; color: var(--ink-3);">' + esc(a.note || '—') + '</span>';
      const disabled = a.disabled;
      return '<div style="background: var(--surface-2); border: 1px solid var(--line-soft); border-radius: 8px; padding: 10px 12px;' + (disabled ? ' opacity: .5;' : '') + '">' +
        '<div style="display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 6px;">' +
          '<div style="font-size: 12.5px; font-weight: 550;">' + esc(name) + ' · ' + esc(a.label || a.id) + '</div>' +
          '<div style="display: flex; gap: 4px;">' +
            '<button class="xs" data-ext-checkin="' + esc(a.provider) + '|' + esc(a.id) + '"' + (disabled ? ' disabled' : '') + '>签到</button>' +
            '<button class="xs" data-ext-toggle="' + esc(a.provider) + '|' + esc(a.id) + '" data-ext-disabled="' + (disabled ? '0' : '1') + '">' + (disabled ? '启用' : '停用') + '</button>' +
            '<button class="xs" data-ext-remove="' + esc(a.provider) + '|' + esc(a.id) + '">删除</button>' +
          '</div>' +
        '</div>' +
        '<div style="display: flex; align-items: center; justify-content: space-between; gap: 8px;">' +
          '<span style="font-size: 11px; color: var(--ink-3); font-family: var(--mono);">' + esc(a.id) + '</span>' + bal +
        '</div>' +
      '</div>';
    }).join('');
    document.querySelectorAll('[data-ext-checkin]').forEach(b => b.onclick = () => extCheckinOne(b));
    document.querySelectorAll('[data-ext-toggle]').forEach(b => b.onclick = () => extToggleOne(b));
    document.querySelectorAll('[data-ext-remove]').forEach(b => b.onclick = () => extRemoveOne(b));
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
    content.hidden = true;
  }
}

async function extCheckinOne(b) {
  const [provider, id] = b.dataset.extCheckin.split('|');
  b.disabled = true; b.textContent = '…';
  try {
    const r = await api('ext/accounts/' + encodeURIComponent(provider) + '/' + encodeURIComponent(id) + '/checkin', { method: 'POST' });
    const res = r.result || {};
    toast('[' + (EXT_PROVIDER_NAMES[provider] || provider) + '] ' + (res.message || res.kind || '完成'), res.kind === 'claimed' ? 'ok' : (res.kind === 'failed' ? 'err' : 'ok'));
    loadExtAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '签到'; }
}

async function extToggleOne(b) {
  const [provider, id] = b.dataset.extToggle.split('|');
  const disabled = b.dataset.extDisabled === '1';
  try {
    await api('ext/accounts/' + encodeURIComponent(provider) + '/' + encodeURIComponent(id) + '/toggle', { method: 'POST', body: JSON.stringify({ disabled }) });
    loadExtAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
}

async function extRemoveOne(b) {
  const [provider, id] = b.dataset.extRemove.split('|');
  if (!confirm('确定删除 ' + (EXT_PROVIDER_NAMES[provider] || provider) + ' 账号 ' + id + '？')) return;
  try {
    await api('ext/accounts/' + encodeURIComponent(provider) + '/' + encodeURIComponent(id) + '/remove', { method: 'POST' });
    toast('已删除', 'ok');
    loadExtAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
}

if ($('btnExtRefresh')) $('btnExtRefresh').onclick = () => loadExtAccounts(false);
if ($('btnExtCheckinAll')) $('btnExtCheckinAll').onclick = async () => {
  const b = $('btnExtCheckinAll');
  b.disabled = true; b.textContent = '签到中…';
  try {
    const r = await api('ext/checkin_all', { method: 'POST' });
    const results = r.results || [];
    const ok = results.filter(x => x.kind === 'claimed').length;
    const already = results.filter(x => x.kind === 'already-claimed').length;
    const failed = results.filter(x => x.kind === 'failed').length;
    toast('签到完成：成功 ' + ok + ' · 已领过 ' + already + ' · 失败 ' + failed, failed === 0 ? 'ok' : 'err');
    loadExtAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '一键签到全部'; }
};
if ($('btnExtAdd')) $('btnExtAdd').onclick = async () => {
  const provider = $('extProvider').value, id = $('extIdInput').value.trim(), raw = $('extCredInput').value.trim();
  if (!id) { toast('请填写账号 ID', 'err'); return; }
  let cred;
  try { cred = JSON.parse(raw); } catch (e) { toast('凭据不是合法 JSON', 'err'); return; }
  try {
    await api('ext/accounts', { method: 'POST', body: JSON.stringify({ provider, id, cred }) });
    toast('账号已添加', 'ok');
    $('extIdInput').value = ''; $('extCredInput').value = '';
    loadExtAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
};
