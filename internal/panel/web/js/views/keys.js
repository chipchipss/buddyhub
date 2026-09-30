/* ══════════════════════════════════════════════════════════════════
   views/keys.js · API 密钥
   每把 Key 可授权平台子集；留空 = 全平台。客户端以 Bearer 调用。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog, copyText } from '../kernel.js';
import { defineView } from '../shell.js';
import { platforms, loadPlatforms, platName, platWithPrefix } from '../platforms.js';

const keys = signal(null);
const err = signal('');
const picked = signal(new Set(['*']));

// 平台清单来自注册表（platforms.js）——不再自带名字表
const ALL_PLATS = () => platforms.peek().map(p => p.id);

async function load(quiet = true) {
  err.set('');
  await loadPlatforms();
  try { keys.set((await api('apikeys')).keys || []); }
  catch (e) { err.set(e.message); if (!quiet) toast(e.message, 'fail'); }
}

function togglePlat(p) {
  const cur = new Set(picked.peek());
  if (p === '*') { picked.set(new Set(['*'])); return; }
  cur.delete('*');
  cur.has(p) ? cur.delete(p) : cur.add(p);
  if (!cur.size) cur.add('*');
  picked.set(cur);
}

async function generate() {
  const nameInput = document.getElementById('nk-name');
  const noteInput = document.getElementById('nk-note');
  const n = (nameInput?.value || '').trim();
  if (!n) { toast('先填 Key 名称', 'fail'); return; }
  try {
    const platforms = [...picked.peek()].filter(p => p !== '*');
    const d = await api('apikeys', { method: 'POST', body: JSON.stringify({ name: n, platforms, note: (noteInput?.value || '').trim() }) });
    toast('已生成：' + d.key.key);
    if (nameInput) nameInput.value = '';
    if (noteInput) noteInput.value = '';
    await load();
  } catch (e) { toast(e.message, 'fail'); }
}

export default defineView({
  id: 'keys',
  title: 'API 密钥',
  icon: 'keys',
  group: '网关',
  keywords: 'key api 密钥 token',
  sub() {
    const k = keys();
    return k ? `${k.length} 把密钥` : '正在读取…';
  },
  render() {
    if (!keys() && !err()) load();
    const list = keys() || [];

    // 平台 chips 用订阅式局部更新：输入框是「非受控」的（值存在 DOM 里），
    // 这样点选平台触发的重绘不会清掉正在输入的名称/备注。
    const chips = h('div', { class: 'row wrap' });
    const paintChips = () => {
      const set = picked.peek();
      chips.replaceChildren(
        h('button', { class: 'btn sm' + (set.has('*') ? ' primary' : ''), text: '全平台', onclick: () => togglePlat('*') }),
        ...ALL_PLATS().map(p => h('button', {
          class: 'btn sm' + (set.has(p) ? ' primary' : ''), text: platName(p),
          onclick: () => togglePlat(p),
        })),
        h('span', { class: 'grow' }),
        h('button', { class: 'btn primary', onclick: generate }, icon('plus'), '生成新 Key'),
      );
    };
    picked.subscribe(paintChips);
    paintChips();

    return h('div', { class: 'view stack' },
      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '生成新密钥' }),
          h('span', { class: 'hint', text: '平台留空或选「全平台」= 不限制' }),
        ),
        h('div', { class: 'body stack' },
          h('div', { class: 'row wrap' },
            h('input', {
              class: 'input', id: 'nk-name', placeholder: 'Key 名称（如：Cline 桌面端）',
              style: { flex: '1', minWidth: '200px' },
            }),
            h('input', {
              class: 'input', id: 'nk-note', placeholder: '备注（可选）',
              style: { flex: '1', minWidth: '160px' },
            }),
          ),
          chips,
        ),
      ),

      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '已生成的密钥' }),
          h('span', { class: 'grow' }),
          h('button', { class: 'btn sm ghost', onclick: () => load(false) }, icon('refresh'), '刷新'),
        ),
        h('div', { class: 'body stack' },
          err() ? h('div', { class: 'empty' }, h('div', { class: 'd', text: err() }))
            : !list.length ? h('div', { class: 'empty' }, icon('keys'), h('div', { class: 't', text: '还没有密钥' }), h('div', { class: 'd', text: '上方填名称后点「生成新 Key」' }))
              : list.map(k => h('div', { class: 'keyrow' },
                h('div', { style: { flex: '1', minWidth: '220px' } },
                  h('div', { class: 'nm' }, k.name || '未命名',
                    k.note ? h('span', { class: 'note', text: ' · ' + k.note }) : null),
                  h('code', { text: k.key }),
                ),
                h('div', { class: 'row wrap', style: { gap: '5px' } },
                  (!k.platforms || !k.platforms.length || k.platforms.includes('*'))
                    ? h('span', { class: 'chip strong', text: '全平台' })
                    : k.platforms.map(p => h('span', { class: 'chip', text: platName(p) })),
                ),
                h('button', {
                  class: 'btn sm', onclick: async () => {
                    try { await copyText(k.key); toast('已复制'); } catch { toast('复制失败，请手动选择', 'fail'); }
                  },
                }, icon('copy'), '复制'),
                h('button', {
                  class: 'btn sm danger', onclick: async () => {
                    if (!await confirmDialog('删除该 Key？使用它的客户端将立即失效。', { ok: '删除' })) return;
                    try { await api('apikeys/delete', { method: 'POST', body: JSON.stringify({ key: k.key }) }); toast('Key 已删除'); await load(); }
                    catch (e) { toast(e.message, 'fail'); }
                  },
                }, icon('trash'), '删除'),
              )),
        ),
      ),

      h('section', { class: 'card' },
        h('header', null, h('h2', { text: '调用说明' })),
        h('div', { class: 'body', style: { fontSize: '12.5px', color: 'var(--fg-2)', lineHeight: '1.9' } },
          h('div', null, '地址统一 ', h('code', { style: { fontFamily: 'var(--mono)' }, text: 'http://主机:7863/v1' }), '，模型名前缀决定平台：'),
          h('div', { class: 'row wrap', style: { marginTop: '8px', gap: '6px' } },
            ...platWithPrefix().map(p => h('span', { class: 'chip' },
              h('code', { style: { fontFamily: 'var(--mono)' }, text: p.prefix }), p.name)),
          ),
          h('div', { class: 'muted', style: { marginTop: '8px' } }, '未授权对应平台的 Key 调用该平台模型返回 403；主 api_key（配置页）始终全平台。'),
        ),
      ),
    );
  },
});
