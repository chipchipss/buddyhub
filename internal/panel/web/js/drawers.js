/* ══════════════════════════════════════════════════════════════════
   drawers.js · 抽屉
   添加账号（腾讯 OAuth / Loomy 三种接入 / JSON 导入）
   账号任务（接受 · 领取 · 一键完成）
   开学季券码（含离线二维码）
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, apiUpload, toast, openDrawer, closeDrawer, copyText, confirmDialog } from './kernel.js';
import { qrMatrix, qrSVG } from './qr.js';
import { refreshOverview } from './store.js';
import { navigate } from './shell.js';
import { extAddPanel, stopExtAddTimers } from './views/ext-add.js';
import { zaiAddForm } from './views/zai-segment.js';

/* ══ 1. 添加账号 ══════════════════════════════════════════════════ */
const addTab = signal('tencent');
const realm = signal('cn');
const loginUrl = signal('');
const loginState = signal('');
const status = signal({ kind: '', text: '' });
const loomyMethod = signal('detect');
const loomyInfo = signal(null);
let pollTimer = null;

function stopPoll() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

export function openAddAccount() {
  addTab.set('tencent');
  loginUrl.set('');
  status.set({ kind: '', text: '' });
  loomyInfo.set(null);
  renderAdd();
}

function setStatus(kind, text) { status.set({ kind, text }); }

function renderAdd() {
  const body = h('div', { class: 'stack' }, tabBody());
  openDrawer({
    title: '添加账号',
    hint: '选择平台与接入方式，凭据直接落盘进池，无需重启',
    body,
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', text: '关闭', onclick: () => { stopPoll(); stopExtAddTimers(); closeDrawer(); } }),
    ),
  });
}

// 每个平台的接入方式（新增平台时只改这张表）
const ADD_TABS = [
  ['tencent', '腾讯 WorkBuddy'],
  ['loomy', 'Loomy'],
  ['zai', 'Z.AI'],
  ['ext', '外部平台'],
  ['import', '手动 JSON'],
];

function tabsRow() {
  return h('div', { class: 'seg', style: { flexWrap: 'wrap' } },
    ...ADD_TABS.map(([v, n]) => h('button', {
      class: addTab.peek() === v ? 'on' : '', text: n,
      onclick: () => { addTab.set(v); stopPoll(); stopExtAddTimers(); renderAdd(); },
    })),
  );
}

function tabBody() {
  return h('div', { class: 'stack' },
    tabsRow(),
    addTab.peek() === 'tencent' ? tencentPanel()
      : addTab.peek() === 'loomy' ? loomyPanel()
        : addTab.peek() === 'zai' ? zaiPanel()
          : addTab.peek() === 'ext' ? extPanel()
            : importPanel(),
    status.peek().text
      ? h('div', { class: 'chip' + (status.peek().kind === 'fail' ? ' faint' : ' strong'),
          style: { padding: '10px 13px', whiteSpace: 'normal', display: 'block' },
          text: status.peek().text })
      : null,
  );
}

// Z.AI / 智谱：JWT 或 API Key 入池，也支持 OAuth 免密登录。
function zaiPanel() {
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: 'JWT 走 Plan 通道（消耗订阅额度）；API Key 走回退通道（免验证码）。入池后可用 zai:GLM-5.3 调用。' }),
    zaiAddForm(() => refreshOverview()),
  );
}

// 外部平台：LobsterAI / 小浣熊 / Qoder / 华为云 CodeArts / GitHub Copilot。
function extPanel() {
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: 'GitHub Copilot 用设备码授权，其余平台粘贴客户端凭据即可。' }),
    extAddPanel(() => refreshOverview(), { flat: true }),
  );
}

function tencentPanel() {
  const st = status.peek();
  return h('div', { class: 'stack' },
    h('div', { class: 'seg' },
      ...[['cn', '国内版 CN'], ['global', '国际版 Global']].map(([v, n]) => h('button', {
        class: realm.peek() === v ? 'on' : '', text: n,
        onclick: () => { realm.set(v); renderAdd(); },
      })),
    ),
    h('div', { class: 'muted', style: { fontSize: '12px' },
      text: realm.peek() === 'global'
        ? '国际版登录后自动完成注册地区、激活与试用额度领取。'
        : '登录后自动完成签到与积分初始化。' }),

    loginUrl()
      ? h('div', { class: 'stack' },
        h('div', { class: 'muted', style: { fontSize: '12px' }, text: '在浏览器打开以下链接并完成登录：' }),
        h('div', { class: 'url-box', text: loginUrl() }),
        h('div', { class: 'row' },
          h('button', {
            class: 'btn sm', onclick: async () => {
              try { await copyText(loginUrl()); toast('链接已复制'); } catch { toast('复制失败，请手动选择', 'fail'); }
            },
          }, icon('copy'), '复制链接'),
          h('button', { class: 'btn sm primary', onclick: () => window.open(loginUrl(), '_blank') }, '在浏览器打开'),
        ),
        h('div', { class: 'busy', text: '等待授权完成，自动检测中' }),
      )
      : h('div', { class: 'row' },
        h('button', {
          class: 'btn primary', onclick: async ev => {
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            setStatus('', '正在获取授权链接…');
            renderAdd();
            try {
              const r = await api('login/start', { method: 'POST', body: JSON.stringify({ realm: realm.peek() }) });
              loginState.set(r.state);
              loginUrl.set(r.url);
              setStatus('', '');
              renderAdd();
              startLoginPoll();
            } catch (e) { setStatus('fail', e.message); renderAdd(); }
            finally { if (__b) __b.disabled = false; }
          },
        }, icon('plus'), '获取授权链接'),
      ),
    st.kind === 'fail' && !loginUrl() ? null : null,
  );
}

function startLoginPoll() {
  stopPoll();
  pollTimer = setInterval(async () => {
    if (!loginState.peek()) return;
    try {
      const r = await api('login/poll?state=' + encodeURIComponent(loginState.peek()));
      if (r.done) {
        stopPoll();
        setStatus('', `已添加 ${r.nickname || r.uid}${r.realm === 'global' ? '（国际版）' : ''}` +
          (r.credits >= 0 ? ` · 积分 ${r.credits}` : '') + '，账号已载入池中');
        loginUrl.set('');
        renderAdd();
        await refreshOverview();
        setTimeout(() => closeDrawer(), 1600);
      }
    } catch (e) {
      stopPoll();
      setStatus('fail', e.message + '（关闭后重新添加）');
      renderAdd();
    }
  }, 3000);
}

function loomyPanel() {
  const m = loomyMethod.peek();
  const info = loomyInfo();
  return h('div', { class: 'stack' },
    h('div', { class: 'seg' },
      ...[['detect', '自动检测'], ['pwd', '账号密码'], ['token', '手动 Token']].map(([v, n]) => h('button', {
        class: m === v ? 'on' : '', text: n,
        onclick: () => { loomyMethod.set(v); if (v === 'detect') detectLoomy(); renderAdd(); },
      })),
    ),

    m === 'detect' ? h('div', { class: 'stack' },
      h('div', { class: 'row' },
        h('button', { class: 'btn sm primary', onclick: () => detectLoomy() }, icon('scan'), '开始检测'),
      ),
      info
        ? h('div', { class: 'task-tile' },
          h('div', null,
            h('div', { class: 't', text: info.userid ? 'Loomy · ' + info.userid : 'Loomy 客户端' }),
            h('div', { class: 'c', text: `手机 ${info.phone_masked || '已登录'} · 本地缓存就绪` }),
          ),
          h('div', { class: 'p', text: `${info.earned} / ${info.total}` }),
        )
        : h('div', { class: 'muted', style: { fontSize: '12.5px' }, text: '未检测到本机 Loomy 客户端时，可改用密码或 Token 接入' }),
    ) : null,

    m === 'pwd' ? h('div', { class: 'stack' },
      h('label', { class: 'field' }, h('span', { class: 'label', text: '手机号' }),
        h('input', { class: 'input', id: 'loomy-phone', placeholder: '手机号' })),
      h('label', { class: 'field' }, h('span', { class: 'label', text: '密码' }),
        h('div', { class: 'row' },
          h('input', { class: 'input', id: 'loomy-pwd', type: 'password', placeholder: '密码' }),
          h('button', {
            class: 'btn sm primary', onclick: async ev => {
              const phone = document.getElementById('loomy-phone').value.trim();
              const password = document.getElementById('loomy-pwd').value;
              if (!phone || !password) { toast('手机号与密码必填', 'fail'); return; }
              var __b = ev.currentTarget; if (__b) __b.disabled = true;
              setStatus('', '登录中…');
              renderAdd();
              try {
                await api('ext/loomy/login_password', { method: 'POST', body: JSON.stringify({ phone, password }) });
                setStatus('', '密码登录成功，账号已入池');
                renderAdd();
                await refreshOverview();
              } catch (e) { setStatus('fail', '登录失败：' + e.message); renderAdd(); }
              finally { if (__b) __b.disabled = false; }
            },
          }, '登录'),
        )),
      h('div', { class: 'row' },
        h('input', { class: 'input', id: 'loomy-sms', placeholder: '短信验证码（备用）' }),
        h('button', {
          class: 'btn sm', onclick: async ev => {
            const phone = document.getElementById('loomy-phone').value.trim();
            if (!phone) { toast('先填手机号', 'fail'); return; }
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            try { await api('ext/loomy/send_sms', { method: 'POST', body: JSON.stringify({ phone }) }); toast('验证码已发送'); }
            catch (e) { toast(e.message, 'fail'); }
            finally { if (__b) __b.disabled = false; }
          },
        }, '发验证码'),
        h('button', {
          class: 'btn sm', onclick: async ev => {
            const phone = document.getElementById('loomy-phone').value.trim();
            const code = document.getElementById('loomy-sms').value.trim();
            if (!phone || !code) { toast('手机号与验证码必填', 'fail'); return; }
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            setStatus('', '短信登录中…');
            renderAdd();
            try {
              await api('ext/loomy/login_sms', { method: 'POST', body: JSON.stringify({ phone, code, msgid: '' }) });
              setStatus('', '短信登录成功，账号已入池');
              renderAdd();
              await refreshOverview();
            } catch (e) { setStatus('fail', '登录失败：' + e.message); renderAdd(); }
            finally { if (__b) __b.disabled = false; }
          },
        }, '短信登录'),
      ),
    ) : null,

    m === 'token' ? h('div', { class: 'stack' },
      h('label', { class: 'field' },
        h('span', { class: 'label', text: 'Loomy Session Token' }),
        h('input', { class: 'input', id: 'loomy-token', placeholder: '从 userData/auth-session.json 复制 session 字段' }),
        h('span', { class: 'tip', text: '验证通过后立即落盘进池' })),
      h('div', { class: 'row' },
        h('button', {
          class: 'btn sm primary', onclick: async ev => {
            const tok = document.getElementById('loomy-token').value.trim();
            if (!tok) { toast('请输入 Session Token', 'fail'); return; }
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            setStatus('', '验证中…');
            renderAdd();
            try {
              await api('loomy/save', { method: 'POST', body: JSON.stringify({ session: tok }) });
              setStatus('', '保存成功');
              renderAdd();
              await refreshOverview();
            } catch (e) { setStatus('fail', e.message); renderAdd(); }
            finally { if (__b) __b.disabled = false; }
          },
        }, icon('check'), '验证并保存'),
      ),
    ) : null,
  );
}

async function detectLoomy() {
  try {
    const d = await api('loomy/status');
    if (!d.has_account) {
      loomyInfo.set(null);
      setStatus('fail', d.message || '未检测到本机 Loomy 客户端');
    } else {
      loomyInfo.set(d.status || {});
      setStatus('', '');
    }
  } catch (e) { setStatus('fail', e.message); }
  renderAdd();
}

function importPanel() {
  const file = h('input', { type: 'file', accept: '.json', class: 'input' });
  file.addEventListener('change', async () => {
    const f = file.files[0];
    if (!f) return;
    const fd = new FormData();
    fd.append('file', f);
    setStatus('', '导入中…');
    renderAdd();
    try {
      const d = await apiUpload('import/cockpit', fd);
      setStatus('', `导入完成：成功 ${d.imported} 个${d.skipped ? `，跳过 ${d.skipped} 个` : ''}`);
      await refreshOverview();
    } catch (e) { setStatus('fail', '导入失败：' + e.message); }
    renderAdd();
  });
  return h('div', { class: 'stack' },
    h('div', { class: 'muted', style: { fontSize: '12.5px' }, text: '选择 cockpit tools 导出的 JSON 文件（账号数组）批量导入。' }),
    file,
  );
}

/* ══ 2. 账号任务 ══════════════════════════════════════════════════ */
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报',
  'Buddy_App': '上报「进入 Buddy 应用」事件链',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链',
  'automation_1': '上报「定时任务创建」事件',
  'Library_read': '上报「读资料库介绍」事件',
  'template_5': '上报「使用模板创建任务」事件组 ×5',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组',
  'create_canvas': '上报「设计创意画布创建」事件组',
  'expert_5': '真实专家召唤+使用链 ×5',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链',
  'skill_1': '真实对话 + skill_info 技能加载事件',
  'school_season': '校园日（小程序口径）',
  'Sequential_Tasks_1': '小程序首对话',
  'Sequential_Tasks_2': '小程序选专家对话',
  'Sequential_Tasks_3': '小程序五次对话',
  'Sequential_Tasks_4': '小程序定时任务',
  'Sequential_Tasks_5': '小程序使用 GLM5.2',
  'Sequential_Tasks_6': '小程序十次对话',
  'Sequential_Tasks_7': '体验灵感功能',
};

const tasks = signal(null);
const taskErr = signal('');
let taskUid = null;

export function openAccountDrawer(uid, nickname) {
  taskUid = uid;
  tasks.set(null);
  taskErr.set('');
  openDrawer({
    title: nickname || '账号任务',
    hint: uid,
    body: h('div', { class: 'stack' }, taskBody()),
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('button', {
        class: 'btn sm', onclick: async ev => {
          var __b = ev.currentTarget; if (__b) __b.disabled = true;
          try {
            const r = await api(`accounts/${encodeURIComponent(taskUid)}/tasks/accept_all`, { method: 'POST' });
            const n = r.accepted || 0;
            toast(r.failed && r.failed.length ? `已接受 ${n} 个，${r.failed.length} 个被上游拒绝` : (n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受')));
            await loadTasks();
          } catch (e) { toast(e.message, 'fail'); }
          finally { if (__b) __b.disabled = false; }
        },
      }, '全部接受'),
      h('button', { class: 'btn sm ghost', onclick: () => loadTasks(false) }, '重新查询'),
      h('span', { class: 'grow' }),
      h('button', {
        class: 'btn sm primary', onclick: async ev => {
          if (!await confirmDialog('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。过程约 1-2 分钟（含真实对话）。', { ok: '开始执行' })) return;
          var __b = ev.currentTarget; if (__b) __b.disabled = true;
          try {
            const r = await api(`accounts/${encodeURIComponent(taskUid)}/tasks/auto_all`, { method: 'POST' });
            const done = (r.results || []).filter(x => x.status === 'done').length;
            const skip = (r.results || []).filter(x => x.status === 'skipped').length;
            const bad = (r.results || []).filter(x => x.status === 'error').length;
            toast(`执行完成：成功 ${done}，跳过 ${skip}${bad ? `，失败 ${bad}` : ''}`, bad ? 'fail' : undefined);
            await loadTasks();
            await refreshOverview();
          } catch (e) { toast(e.message, 'fail'); }
          finally { if (__b) __b.disabled = false; }
        },
      }, icon('play'), '一键完成可自动任务'),
    ),
  });
  loadTasks();
}

async function loadTasks(quiet = true) {
  if (!taskUid) return;
  try {
    const d = await api(`accounts/${encodeURIComponent(taskUid)}/tasks`);
    const list = (d.tasks || []).slice().sort((a, b) =>
      (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    tasks.set(list);
    taskErr.set('');
  } catch (e) {
    taskErr.set(e.message);
    if (!quiet) toast(e.message, 'fail');
  }
  // 抽屉已开时局部刷新任务区
  const host = document.querySelector('.drawer .task-host');
  if (host) host.replaceChildren(taskBody());
}

function taskBody() {
  const host = h('div', { class: 'task-host stack' });
  if (taskErr()) {
    host.append(h('div', { class: 'empty' }, h('div', { class: 'd', text: taskErr() })));
    return host;
  }
  const list = tasks();
  if (!list) { host.append(h('div', { class: 'busy', text: '查询任务进度' })); return host; }
  if (!list.length) { host.append(h('div', { class: 'empty' }, h('div', { class: 'd', text: '该账号暂无任务' }))); return host; }

  host.append(h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
    h('thead', null, h('tr', null,
      h('th', { text: '任务' }), h('th', { class: 'num', text: '进度' }), h('th', { class: 'num', text: '奖励' }),
      h('th', { text: '状态' }), h('th', { class: 'acts', text: '' }))),
    h('tbody', null, ...list.map(t => {
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? `${cur} / ${tgt}` : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push(`+${t.credit} 分`);
      if (t.energy) parts.push(`+${t.energy} 能`);
      if (t.reward_buddy) parts.push('Buddy');

      const state = t.claimed ? ['已领取', 'faint'] : t.claimable ? ['可领取', 'strong']
        : t.locked ? ['未解锁', 'faint'] : t.accept_status === 'accepted' ? ['进行中', ''] : ['未接受', 'faint'];

      const action = t.claimed || t.locked ? null
        : t.claimable ? h('button', {
          class: 'btn sm primary', text: '领取',
          onclick: async ev => {
            var __b = ev.currentTarget; if (__b) __b.disabled = true;
            try {
              await api(`accounts/${encodeURIComponent(taskUid)}/tasks/claim`, { method: 'POST', body: JSON.stringify({ task_code: t.task_code }) });
              toast('已领取奖励');
              await loadTasks(); await refreshOverview();
            } catch (e) { toast(e.message, 'fail'); }
            finally { if (__b) __b.disabled = false; }
          },
        })
          : AUTO_TASKS[t.task_code] ? h('button', {
            class: 'btn sm primary', text: '一键完成', title: AUTO_TASKS[t.task_code],
            onclick: async ev => {
              var __b = ev.currentTarget; if (__b) __b.disabled = true;
              __b.textContent = '执行中…';
              try {
                const r = await api(`accounts/${encodeURIComponent(taskUid)}/tasks/auto`, { method: 'POST', body: JSON.stringify({ task_code: t.task_code }) });
                if (r.skipped) toast(r.message || '已跳过');
                else {
                  const advanced = r.progress_before !== r.progress_after;
                  let msg = r.message || '已执行';
                  if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
                  if (r.claimed) msg += '，奖励已到账';
                  else if (r.attempt && !advanced) msg += '；进度未动，可能需要官方客户端';
                  toast(msg, (r.claimed || advanced) ? undefined : 'fail');
                }
                await loadTasks(); await refreshOverview();
              } catch (e) { toast(e.message, 'fail'); }
              finally { if (__b) __b.disabled = false; }
            },
          })
            : t.accept_status === 'accepted' ? null
              : h('button', {
                class: 'btn sm', text: '接受',
                onclick: async ev => {
                  var __b = ev.currentTarget; if (__b) __b.disabled = true;
                  try {
                    await api(`accounts/${encodeURIComponent(taskUid)}/tasks/accept`, { method: 'POST', body: JSON.stringify({ task_codes: [t.task_code] }) });
                    toast('已接受任务');
                    await loadTasks();
                  } catch (e) { toast(e.message, 'fail'); }
                  finally { if (__b) __b.disabled = false; }
                },
              });

      return h('tr', { title: [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n') },
        h('td', null,
          h('div', { style: { fontWeight: '550' }, text: t.title || t.task_code }),
          h('div', { class: 'muted', style: { font: '10.5px var(--mono)' }, text: t.task_code + (t.tag ? ' · ' + t.tag : '') })),
        h('td', { class: 'num', text: prog }),
        h('td', { class: 'num', text: parts.length ? parts.join(' ') : '—' }),
        h('td', null, h('span', { class: 'chip ' + state[1], text: state[0] })),
        h('td', { class: 'acts' }, action),
      );
    })),
  )));
  return host;
}

/* ══ 3. 开学季券码 ════════════════════════════════════════════════ */
export function openVouchers() {
  const body = h('div', { class: 'stack' }, h('div', { class: 'busy', text: '查询券码' }));
  openDrawer({
    title: '开学季 · 我的券码',
    hint: '抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），到对应 App 或小程序兑换',
    body,
    footer: h('div', { class: 'row', style: { width: '100%' } },
      h('button', { class: 'btn sm', onclick: () => loadVouchers(body) }, icon('refresh'), '刷新'),
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', text: '关闭', onclick: closeDrawer }),
    ),
  });
  loadVouchers(body);
}

async function loadVouchers(host) {
  try {
    const d = await api('school/vouchers');
    const accounts = d.accounts || [];
    const ok = accounts.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    const nodes = [];
    for (const a of ok) {
      const vs = a.vouchers || [];
      if (!vs.length) continue;
      nodes.push(h('div', { class: 'plat-head' },
        h('span', { class: 'nm', text: a.nickname || a.uid }), h('span', { class: 'line' }), h('span', { text: `${vs.length} 张` })));
      nodes.push(...vs.map(voucherCard));
    }
    if (!nodes.length) nodes.push(h('div', { class: 'empty' }, icon('ticket'), h('div', { class: 't', text: '还没有抽到券' })));
    const failed = accounts.filter(a => a.error);
    if (failed.length) {
      nodes.push(h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '8px' },
        text: '查询失败：' + failed.map(a => `${a.nickname || a.uid.slice(0, 8)}（${a.error}）`).join('、') }));
    }
    if (total) nodes.unshift(h('div', { class: 'muted', style: { fontSize: '12px' }, text: `共 ${total} 张可用券` }));
    host.replaceChildren(...nodes);
  } catch (e) {
    host.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'd', text: e.message })));
  }
}

function voucherCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  const qrHost = h('div');
  return h('div', { class: 'voucher' + (expired ? ' expired' : '') },
    h('div', { class: 'hd' },
      h('span', { class: 'nm', text: v.prize_name || v.sku_code || '券' }),
      h('span', { class: 'chip ' + (expired ? 'faint' : 'strong'), text: expired ? '已过期' : '可使用' }),
    ),
    h('div', { class: 'meta', text: (v.valid_to ? `有效期至 ${v.valid_to}` : '长期有效') + (v.granted_at ? ` · ${String(v.granted_at).slice(0, 10)} 抽中` : '') }),
    h('div', { class: 'sep' }),
    h('div', { class: 'ft' },
      h('span', { class: 'lab', text: '券码' }),
      h('code', { text: v.code || '-' }),
      h('div', { class: 'acts' },
        v.code ? h('button', {
          class: 'btn sm', onclick: () => {
            if (qrHost.firstChild) { qrHost.replaceChildren(); return; }
            try { qrHost.replaceChildren(h('div', { class: 'qr' }, qrSVG(qrMatrix(v.code), 148))); }
            catch (e) { qrHost.replaceChildren(h('div', { class: 'muted', style: { padding: '10px' }, text: '二维码生成失败：' + e.message })); }
          },
        }, icon('qr'), '二维码') : null,
        h('button', {
          class: 'btn sm', onclick: async () => {
            try { await copyText(v.code || ''); toast('券码已复制'); } catch { toast('复制失败，请手动选择', 'fail'); }
          },
        }, icon('copy'), '复制'),
      ),
    ),
    qrHost,
  );
}
