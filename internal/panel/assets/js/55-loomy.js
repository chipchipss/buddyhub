/* ── Loomy 新手之旅任务（讯飞） ─────────────────────────────────────── */
async function loadLoomyStatus(quiet) {
  const st = $('loomyState'), content = $('loomyContent');
  if (!st || !content) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; content.hidden = true; }
  try {
    const d = await api('loomy/status');
    const s = d.status || {};
    if (!d.has_account) {
      st.hidden = false; st.className = 'state';
      st.textContent = d.message || '未检测到本地 Loomy 客户端登录态';
      content.hidden = true;
      $('loomySummary').textContent = '未检测到客户端';
      return;
    }
    st.hidden = true; content.hidden = false;
    $('loomyAcctTitle').textContent = s.userid ? 'Loomy · ' + s.userid : 'Loomy 客户端';
    $('loomyAcctSub').textContent = '手机: ' + (s.phone_masked || '已登录') + ' · 缓存: ' + (s.user_data_dir || '本地');
    $('loomyPointsBadge').textContent = s.earned + ' / ' + s.total + ' 积分';
    $('loomyBarFill').style.width = Math.min(100, Math.round((s.earned / (s.total || 10000)) * 100)) + '%';
    $('loomySummary').textContent = s.earned >= s.total ? '新手任务全部完成 🎉' : '已达成 ' + s.earned + ' / ' + s.total + ' 分';
    const items = s.items || [];
    $('loomyGrid').innerHTML = items.map(it => {
      const ok = it.completed;
      const tagCls = ok ? 'tag ok' : 'tag mute';
      const icon = ok ? '✓ 已完成' : '○ 待完成';
      return '<div style="background: var(--surface-2); border: 1px solid var(--line-soft); border-radius: 8px; padding: 10px 12px; display: flex; align-items: center; justify-content: space-between; gap: 8px;">' +
        '<div>' +
          '<div style="font-size: 11px; color: var(--ink-3); margin-bottom: 2px;">' + esc(it.category) + '</div>' +
          '<div style="font-size: 13px; font-weight: 550;">' + esc(it.title) + '</div>' +
        '</div>' +
        '<div style="display: flex; flex-direction: column; align-items: flex-end; gap: 4px; flex: none;">' +
          '<span class="' + tagCls + '" style="font-size: 11px;">' + icon + '</span>' +
          '<span style="font-family: var(--mono); font-size: 11px; color: var(--accent);">+' + it.points + '</span>' +
        '</div>' +
      '</div>';
    }).join('');
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
    content.hidden = true;
  }
}
if ($('btnLoomyRefresh')) $('btnLoomyRefresh').onclick = () => loadLoomyStatus(false);
if ($('btnLoomyRunAll')) $('btnLoomyRunAll').onclick = async () => {
  const b = $('btnLoomyRunAll');
  b.disabled = true; b.textContent = '执行中…';
  try {
    const r = await api('loomy/complete_all', { method: 'POST' });
    const cnt = (r.completed || []).length;
    toast(cnt > 0 ? 'Loomy 任务自动完成：新点亮 ' + cnt + ' 项，积分已达 ' + (r.status && r.status.earned ? r.status.earned : 10000) : '所有 Loomy 任务已是完成态', 'ok');
    loadLoomyStatus(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '一键自动完成'; }
};
if ($('btnLoomyCheckin')) $('btnLoomyCheckin').onclick = async () => {
  const b = $('btnLoomyCheckin');
  b.disabled = true; b.textContent = '签到中…';
  try {
    const r = await api('loomy/checkin', { method: 'POST' });
    const c = r.checkin || {};
    toast(c.message || (c.already_processed ? '今日额度已初始化' : '每日额度已激活'), c.already_processed ? 'ok' : 'ok');
    loadLoomyStatus(true);
    loadLoomyCredits(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '每日签到'; }
};
async function loadLoomyCredits(quiet) {
  const box = $('loomyCreditBox');
  if (!box) return;
  if (!quiet) box.hidden = false;
  try {
    const d = await api('loomy/credits');
    if (!d.has_account) { box.hidden = true; return; }
    if (d.error) { box.hidden = false; box.textContent = '积分查询失败: ' + d.error; return; }
    const c = d.credits || {};
    box.hidden = false;
    box.innerHTML = '<b>永久积分</b> ' + (c.permanent ?? 0) + ' · <b>每日赠送</b> ' + (c.daily ?? 0) +
      (c.has_quota ? '（今日额度 ' + (c.daily_quota ?? 0) + '，已用 ' + (c.daily_consumed ?? 0) + '）' : '（未签到，点「每日签到」激活）') +
      ' · 合计 <b>' + (c.total ?? 0) + '</b>';
  } catch (e) { box.hidden = false; box.textContent = '积分查询失败: ' + e.message; }
}
if ($('btnLoomyCredits')) $('btnLoomyCredits').onclick = () => loadLoomyCredits(false);

/* ── 添加账号弹窗里的 Loomy Tab ─────────────────────────────────────── */
async function detectLoomyClient() {
  const st = $('loomyModalState'), box = $('loomyDetectBox');
  if (!st || !box) return;
  st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">检测中</span>';
  box.hidden = true;
  try {
    const d = await api('loomy/status');
    const s = d.status || {};
    if (!d.has_account) {
      st.textContent = d.message || '未检测到本机 Loomy 客户端，可手动输入 Token';
      $('loomyManualBox').hidden = false;
      return;
    }
    st.hidden = true;
    box.hidden = false;
    $('loomyDetectName').textContent = s.userid ? 'Loomy · ' + s.userid : 'Loomy 客户端';
    $('loomyDetectScore').textContent = s.earned + ' / ' + s.total + ' 积分';
    $('loomyDetectDesc').textContent = '手机: ' + (s.phone_masked || '已登录') + ' · 本地缓存已就绪';
  } catch (e) {
    st.className = 'state err'; st.textContent = e.message;
  }
}
if ($('btnDetectLoomy')) $('btnDetectLoomy').onclick = detectLoomyClient;
// Loomy 添加方式分段切换：detect / pwd / token 一次只显示一种
function switchLoomyMethod(m) {
  document.querySelectorAll('#loomyMethodSeg .seg-btn').forEach(b => b.classList.toggle('on', b.dataset.lm === m));
  $('loomyMethodDetect').hidden = m !== 'detect';
  $('loomyMethodPwd').hidden = m !== 'pwd';
  $('loomyMethodToken').hidden = m !== 'token';
}
if ($('loomyMethodSeg')) document.querySelectorAll('#loomyMethodSeg .seg-btn').forEach(b => {
  b.onclick = () => switchLoomyMethod(b.dataset.lm);
});
if ($('btnSubmitLoomyToken')) $('btnSubmitLoomyToken').onclick = async () => {
  const tok = $('loomyTokenInput').value.trim();
  if (!tok) { toast('请输入 Session Token', 'err'); return; }
  const st = $('loomyModalState');
  st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">验证中</span>';
  try {
    await api('loomy/save', { method: 'POST', body: JSON.stringify({ session: tok }) });
    st.className = 'state ok'; st.textContent = '保存成功';
    detectLoomyClient();
  } catch (e) {
    st.className = 'state err'; st.textContent = e.message;
  }
};
async function loomyLogin(endpoint, payload, okMsg) {
  const st = $('loomyLoginState');
  st.textContent = '登录中…';
  try {
    const r = await api('ext/loomy/' + endpoint, { method: 'POST', body: JSON.stringify(payload) });
    st.textContent = okMsg + '（账号已入池 → 外部平台页 / Loomy 任务页）';
    toast(okMsg + '，账号见「外部平台」页', 'ok');
    detectLoomyClient();
    if (typeof loadExtAccounts === 'function') loadExtAccounts(true);
    if (typeof loadLoomyStatus === 'function') loadLoomyStatus(true);
    setTimeout(() => { go('ext'); }, 1600);
  } catch (e) { st.textContent = '失败: ' + e.message; toast(e.message, 'err'); }
}
if ($('btnLoomyLoginPwd')) $('btnLoomyLoginPwd').onclick = () => {
  const phone = $('loomyLoginPhone').value.trim(), pwd = $('loomyLoginPwd').value;
  if (!phone || !pwd) { toast('手机号与密码必填', 'err'); return; }
  loomyLogin('login_password', { phone, password: pwd }, '密码登录成功');
};
if ($('btnLoomySendSms')) $('btnLoomySendSms').onclick = async () => {
  const phone = $('loomyLoginPhone').value.trim();
  if (!phone) { toast('先填手机号', 'err'); return; }
  try {
    await api('ext/loomy/send_sms', { method: 'POST', body: JSON.stringify({ phone }) });
    $('loomySendMsgId').dataset.pending = '1';
    toast('验证码已发送', 'ok');
  } catch (e) { toast(e.message, 'err'); }
};
if ($('btnLoomyLoginSms')) $('btnLoomyLoginSms').onclick = async () => {
  const phone = $('loomyLoginPhone').value.trim(), code = $('loomySmsCode').value.trim();
  if (!phone || !code) { toast('手机号与验证码必填', 'err'); return; }
  // msgid 由服务端 send_sms 时缓存（phone → msgid）；前端不持有，空串即可，
  // 服务端按 phone 回填后调 LoginBySMS。
  loomyLogin('login_sms', { phone, code, msgid: '' }, '短信登录成功');
};
if ($('btnLoomyModalAutoAll')) $('btnLoomyModalAutoAll').onclick = async () => {
  const b = $('btnLoomyModalAutoAll');
  b.disabled = true; b.textContent = '执行中…';
  try {
    const r = await api('loomy/complete_all', { method: 'POST' });
    const cnt = (r.completed || []).length;
    toast(cnt > 0 ? '新点亮 ' + cnt + ' 项任务' : '所有任务已是完成态', 'ok');
    detectLoomyClient();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '一键自动完成所有任务'; }
};
