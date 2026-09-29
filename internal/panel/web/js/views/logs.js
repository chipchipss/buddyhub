/* ══════════════════════════════════════════════════════════════════
   views/logs.js · 运行日志
   频道筛选（全部/任务/对话/系统）+ 自动滚动。等宽终端质感。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast } from '../kernel.js';
import { defineView } from '../shell.js';

const entries = signal(null);
const channel = signal('all');
const pinned = signal(true);

const CH_NAMES = { task: '任务', chat: '对话', sys: '系统' };

async function load(quiet = true) {
  try {
    const d = await api('logs');
    entries.set(d.entries || []);
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

export default defineView({
  id: 'logs',
  title: '运行日志',
  icon: 'logs',
  group: '网关',
  keywords: '日志 log 运行 输出',
  sub() {
    const list = entries();
    if (!list) return '正在读取…';
    const counts = {};
    for (const e of list) counts[e.ch] = (counts[e.ch] || 0) + 1;
    return `任务 ${counts.task || 0} · 对话 ${counts.chat || 0} · 系统 ${counts.sys || 0}`;
  },
  tick() { load(); },
  render() {
    if (!entries()) load();
    const all = entries() || [];
    const ch = channel();
    const shown = ch === 'all' ? all : all.filter(e => e.ch === ch);

    const box = h('div', { class: 'logbox' },
      shown.length
        ? shown.map(e => {
          const level = /error|失败|错误/.test(e.text) ? ' strong'
            : /warn|冷却|熔断/.test(e.text) ? ' dim' : '';
          const time = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
          return h('span', { class: 'ln' + level },
            ch === 'all' ? h('i', { class: 'ch', text: CH_NAMES[e.ch] || e.ch }) : null,
            `${time} ${e.text}`,
          );
        })
        : h('span', { class: 'muted', text: '暂无日志' }),
    );
    if (pinned()) requestAnimationFrame(() => { box.scrollTop = box.scrollHeight; });

    return h('div', { class: 'view stack' },
      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '运行日志' }),
          h('span', { class: 'grow' }),
          h('div', { class: 'seg' },
            ...[['all', '全部'], ['task', '任务'], ['chat', '对话'], ['sys', '系统']]
              .map(([v, n]) => h('button', { class: ch === v ? 'on' : '', text: n, onclick: () => channel.set(v) })),
          ),
          h('button', {
            class: 'btn sm' + (pinned() ? ' primary' : ''),
            onclick: () => pinned.set(!pinned.peek()),
          }, icon('download'), pinned() ? '自动滚动：开' : '自动滚动：关'),
          h('button', { class: 'btn sm ghost', onclick: () => load(false) }, icon('refresh'), '刷新'),
        ),
        h('div', { class: 'body flush' }, box),
      ),
    );
  },
});
