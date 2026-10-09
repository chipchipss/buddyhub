/* ══════════════════════════════════════════════════════════════════
   views/logs.js · 运行日志
   频道筛选（全部/任务/对话/系统）+ 自动滚动。

   性能要点：日志框是**命令式增量更新**的——节点跨渲染保持同一份，
   新日志只 append 尾部，不重建整表（环形缓冲满 500 条时若每轮重建，
   每 5 秒就要重建 500 个节点并重置滚动，观感就是"一卡一卡"）。
   视图的响应式部分只负责头部工具条。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast } from '../kernel.js';
import { defineView } from '../shell.js';

const entries = signal([]);
const channel = signal('all');
const pinned = signal(true);

const CH_NAMES = { task: '任务', chat: '对话', sys: '系统' };

/* 日志框的持久节点与增量状态 */
let boxEl = null;
let lastKey = null;     // 最后一条已渲染条目的指纹（增量锚点）
let shownCh = null;     // 上次渲染所用频道
let shownCount = 0;

async function load(quiet = true) {
  try {
    const d = await api('logs');
    const next = d.entries || [];
    // 内容未变则不写信号：避免每 5 秒一次无意义的重渲染
    if (next.length === entries.peek().length && sameTail(next)) return;
    entries.set(next);
  } catch (e) { if (!quiet) toast(e.message, 'fail'); }
}

function sameTail(next) {
  const cur = entries.peek();
  if (!cur.length) return next.length === 0;
  const a = cur[cur.length - 1], b = next[next.length - 1];
  return !!a && !!b && a.ts === b.ts && a.text === b.text;
}

function entryKey(e) { return (e.ts || '') + '|' + (e.ch || '') + '|' + (e.text || ''); }

function entryNode(e, withCh) {
  const level = /error|失败|错误/.test(e.text) ? ' strong'
    : /warn|冷却|熔断/.test(e.text) ? ' dim' : '';
  const time = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
  return h('span', { class: 'ln' + level },
    withCh ? h('i', { class: 'ch', text: CH_NAMES[e.ch] || e.ch }) : null,
    `${time} ${e.text}`,
  );
}

/** 增量同步日志框：正常只追加新行；频道切换/内容不连续时才整体重建 */
function syncBox() {
  const box = boxEl;
  if (!box || !box.isConnected) return;
  const ch = channel.peek();
  const all = entries();
  const list = ch === 'all' ? all : all.filter(e => e.ch === ch);

  const rebuild = () => {
    box.replaceChildren(...list.map(e => entryNode(e, ch === 'all')));
  };

  if (ch !== shownCh || list.length < shownCount || lastKey === null) {
    rebuild();
  } else {
    // 以"最后一条已渲染的 key"为锚点，只追加其后的新行——
    // 环形缓冲把旧行挤出时锚点仍在表内，追加依然正确。
    let at = -1;
    for (let i = list.length - 1; i >= 0; i--) {
      if (entryKey(list[i]) === lastKey) { at = i; break; }
    }
    if (at < 0) rebuild();
    else if (at + 1 < list.length) {
      const frag = document.createDocumentFragment();
      for (let i = at + 1; i < list.length; i++) frag.append(entryNode(list[i], ch === 'all'));
      box.append(frag);
    }
  }

  shownCh = ch;
  shownCount = list.length;
  lastKey = list.length ? entryKey(list[list.length - 1]) : null;
  if (pinned.peek()) box.scrollTop = box.scrollHeight;
}

export default defineView({
  id: 'logs',
  page: 'automation',
  tab: '执行记录',
  title: '执行记录',
  icon: 'logs',
  keywords: '日志 log 运行 输出 执行记录',
  sub() {
    const list = entries();
    const counts = {};
    for (const e of list) counts[e.ch] = (counts[e.ch] || 0) + 1;
    return `任务 ${counts.task || 0} · 对话 ${counts.chat || 0} · 系统 ${counts.sys || 0}`;
  },
  tick() { load(); },
  render() {
    if (!entries().length) load();
    // 读信号以注册依赖（日志/频道/自动滚动变化时重建工具条并触发增量同步）
    const all = entries();
    const ch = channel();
    const pin = pinned();

    if (!boxEl) boxEl = h('div', { class: 'logbox' });
    if (!all.length && !boxEl.childNodes.length) boxEl.replaceChildren(h('span', { class: 'muted', text: '暂无日志' }));
    // 视图节点挂载完成后同步（此时 boxEl 已在文档中，滚动才生效）
    queueMicrotask(syncBox);

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
            class: 'btn sm' + (pin ? ' primary' : ''),
            onclick: () => pinned.set(!pinned.peek()),
          }, icon('download'), pin ? '自动滚动：开' : '自动滚动：关'),
          h('button', { class: 'btn sm ghost', onclick: () => load(false) }, icon('refresh'), '刷新'),
        ),
        h('div', { class: 'body flush' }, boxEl),
      ),
    );
  },
});
