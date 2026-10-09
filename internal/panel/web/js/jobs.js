/* ══════════════════════════════════════════════════════════════════
   jobs.js · 活动栏：把动作变成看得见的作业（清单 14 / 15 / 16）
   批量动作原先是一把 fetch + 一条一闪而过的 toast，用户要点开日志翻才知道
   哪个账号失败。这里把每次动作记成一「作业」：右下角常驻一条，跑的时候显示
   5/12，跑完显示 ✓ 成功数 / ✗ 失败数，点开逐账号看结果；可撤销的结果旁边
   就带「撤销」。
   活动栏挂在 shell 上而不是账号页里——换页、换分段都不能把正在跑的作业弄丢。
   ══════════════════════════════════════════════════════════════════ */

import { signal, h, icon, toast } from './kernel.js';

const MAX_JOBS = 6;          // 只留最近几条，跑完自动收
const HIDE_OK_MS = 8000;     // 全部成功的作业过一会儿自己消失
const HIDE_BAD_MS = 120000;  // 有失败的留久一点，别一眨眼就没

const jobs = signal([]);
const expanded = signal(null);
const hideTimers = new Map();
let seq = 0;

// 作业对象是就地改写字段的（同一条目要能在原地从 5/12 变成 ✓11 ✗1），
// 但信号只在数组换引用时才通知——所以每次改写后 publish() 一次。
function publish() { jobs.set(jobs.peek().slice()); }

function scheduleHide(job) {
  const ms = job.bad ? HIDE_BAD_MS : HIDE_OK_MS;
  clearTimeout(hideTimers.get(job.id));
  hideTimers.set(job.id, setTimeout(() => {
    hideTimers.delete(job.id);
    // 有人正在看这条（展开了）就别抢走
    if (expanded.peek() === job.id) { scheduleHide(job); return; }
    remove(job.id);
  }, ms));
}

function remove(id) {
  clearTimeout(hideTimers.get(id));
  hideTimers.delete(id);
  if (expanded.peek() === id) expanded.set(null);
  jobs.set(jobs.peek().filter(j => j.id !== id));
}

/** begin(label, total) —— 开一条作业；total 是预计要处理的账号数。 */
export function begin(label, total) {
  const job = { id: ++seq, label, total, done: 0, ok: 0, bad: 0, items: [], state: 'run' };
  jobs.set([job, ...jobs.peek()].slice(0, MAX_JOBS));
  return job;
}

/** report(job, {name, ok, msg, undo}) —— 逐账号回执。undo 是「怎么撤回这一步」。 */
export function report(job, item) {
  job.done += 1;
  job.items.push({
    name: item.name || '—', ok: !!item.ok, msg: item.msg || '',
    undo: item.undo || null, undone: false, undoing: false,
  });
  if (item.ok) job.ok++; else job.bad++;
  if (job.done >= job.total) { job.state = job.bad ? 'fail' : 'done'; scheduleHide(job); }
  publish();
  return job;
}

/** finish(job) —— 提前收尾（实际处理数少于预期时用）。 */
export function finish(job) {
  if (job.state !== 'run') return;
  job.state = job.bad ? 'fail' : 'done';
  scheduleHide(job);
  publish();
}

async function doUndo(job, it) {
  if (it.undoing || it.undone || !it.undo) return;
  it.undoing = true;
  publish();
  const res = await it.undo();
  it.undoing = false;
  it.undone = !!(res && res.ok);
  if (!it.undone) toast('撤销失败：' + ((res && res.msg) || '未知原因'), 'fail');
  publish();
}

function jobNode(job) {
  const open = expanded.peek() === job.id;
  const counts = job.state === 'run'
    ? `${job.done} / ${job.total}`
    : job.bad ? `✓ ${job.ok} · ✗ ${job.bad}` : `✓ ${job.ok}`;
  const head = h('div', { class: 'job-head' },
    h('i', { class: 'job-dot ' + job.state }),
    h('span', { class: 'job-label', text: job.label }),
    h('span', { class: 'job-count mono', text: counts }),
    h('span', { class: 'grow' }),
    h('button', {
      class: 'job-x', title: open ? '收起' : '展开看每个账号',
      'aria-label': open ? '收起' : '展开',
      onclick: () => { expanded.set(open ? null : job.id); paintBar(); },
    }, icon('chevron', 13)),
    h('button', {
      class: 'job-x', title: '关掉', 'aria-label': '关掉这条作业',
      onclick: () => remove(job.id),
    }, icon('close', 13)),
  );
  const kids = [head];
  if (open) {
    kids.push(h('div', { class: 'job-items' }, ...job.items.map(it => h('div', {
      class: 'job-item' + (it.ok ? '' : ' bad') + (it.undone ? ' undone' : ''),
    },
      h('span', { class: 'ji-name', text: it.name }),
      h('span', { class: 'ji-msg', text: it.undone ? '已撤销' : (it.msg || (it.ok ? '成功' : '失败')) }),
      it.undo && !it.undone ? h('button', {
        class: 'btn sm ghost', text: it.undoing ? '撤销中…' : '撤销',
        disabled: it.undoing, onclick: () => doUndo(job, it),
      }) : null,
    ))));
  }
  return h('div', { class: 'job ' + job.state + (open ? ' open' : '') }, ...kids);
}

let barEl = null;

function paintBar() {
  if (!barEl) return;
  const list = jobs.peek();
  barEl.hidden = list.length === 0;
  // 空列表必须传「没有参数」，不能把 null 交给 replaceChildren：真实 DOM 会
  // 把它渲染成一个字面 "null" 文本节点。
  barEl.replaceChildren(...list.map(jobNode));
}

/** mountActivityBar() —— 由 shell 启动时挂一次，跨页常驻。 */
export function mountActivityBar() {
  if (barEl) return barEl;
  barEl = h('div', { class: 'activitybar', hidden: true, role: 'status', 'aria-live': 'polite' });
  document.body.append(barEl);
  jobs.subscribe(paintBar);
  paintBar();
  return barEl;
}

/* ── 按钮三态（清单 15）：空闲 → 转圈（禁用）→ 打勾 1 秒 → 回原样 ──
   只给宿主稳定的按钮（抽屉底部、菜单项）用。账号行里的按钮每次重绘都换
   节点，那一路由 views/accounts.js 的 flash 信号渲染同样的三态。 */
const idleLabels = new WeakMap();

export async function buttonFlow(btn, work) {
  if (!idleLabels.has(btn)) idleLabels.set(btn, btn.textContent);
  const label = idleLabels.get(btn);
  btn.disabled = true;
  btn.classList.add('spin');
  let res = null;
  try { res = await work(); } finally {
    btn.disabled = false;
    btn.classList.remove('spin');
  }
  if (!btn.isConnected) return res;
  if (res && res.ok) {
    btn.classList.add('ok');
    btn.textContent = '完成';
    setTimeout(() => {
      if (!btn.isConnected) return;
      btn.textContent = label;
      btn.classList.remove('ok');
    }, 1000);
  } else {
    btn.textContent = label;
  }
  return res;
}

/** 测试与调试用：当前作业列表（只读快照）。 */
export function jobsSnapshot() { return jobs.peek().slice(); }
