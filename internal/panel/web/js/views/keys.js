/* ══════════════════════════════════════════════════════════════════
   views/keys.js · API 密钥
   每把 Key 可授权平台子集；「全平台」是默认，指定平台时才出现可搜索的清单
   （清单 36）。完整密钥只在生成那一刻出现一次，之后列表只剩掩码（清单 37）。
   客户端以 Bearer 调用。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog, copyText } from '../kernel.js';
import { defineView } from '../shell.js';
import { platforms, loadPlatforms, platName, platWithPrefix } from '../platforms.js';

const keys = signal(null);
const keyStats = signal(null);   // id → {calls,errors,last}（清单 38：删 Key 前看这把还有没有人用）
const err = signal('');
const scope = signal('all');       // all = 全平台 / some = 指定平台
const picked = signal(new Set());  // 指定平台时勾选的平台 id
const platQ = signal('');          // 平台清单的搜索词
const justMade = signal(null);     // 刚生成的那一把（一次性卡片）

// 平台清单来自注册表（platforms.js）——不再自带名字表
const ALL_PLATS = () => platforms.peek().map(p => p.id);

// 调用量标注。hours=0 = 全部历史：by_key 本来就是累计口径。
// usage 拉失败只丢标注，不挡密钥列表——列表才是这一屏的主角。
async function load(quiet = true) {
  err.set('');
  await loadPlatforms();
  try {
    const [k, u] = await Promise.all([
      api('apikeys'),
      api('usage?hours=0').catch(() => null),
    ]);
    keys.set(k.keys || []);
    const m = {};
    for (const s of (u && u.by_key) || []) m[s.id] = s;
    keyStats.set(m);
  }
  catch (e) { err.set(e.message); if (!quiet) toast(e.message, 'fail'); }
}

function lastUsed(ms) {
  if (!ms) return '';
  const d = new Date(ms), p = n => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// statLine —— 「没见过调用」就是最硬的删除依据；失败次数顺带说，
// 因为它决定的是「这把是不是坏了」而不是「还在不在用」。
function statLine(s) {
  if (!s || !s.calls) return '没见过调用';
  const t = lastUsed(s.last);
  return `调用 ${s.calls} 次 · 最近 ${t || '时间未知'}` + (s.errors ? `（失败 ${s.errors}）` : '');
}

function togglePlat(p) {
  const cur = new Set(picked.peek());
  cur.has(p) ? cur.delete(p) : cur.add(p);
  picked.set(cur);
}

async function generate() {
  const nameInput = document.getElementById('nk-name');
  const noteInput = document.getElementById('nk-note');
  const n = (nameInput?.value || '').trim();
  if (!n) { toast('先填 Key 名称', 'fail'); return; }
  if (scope.peek() === 'some' && !picked.peek().size) {
    toast('选了「指定平台」就得至少勾一个，否则这把 Key 谁都调用不了', 'fail');
    return;
  }
  try {
    const d = await api('apikeys', {
      method: 'POST',
      body: JSON.stringify({
        name: n,
        platforms: scope.peek() === 'all' ? [] : [...picked.peek()],
        note: (noteInput?.value || '').trim(),
      }),
    });
    // 清单 37：完整密钥只在这一次响应里出现，所以用一张卡片把它连同「怎么用它」
    // 一起交出去——塞进 toast 的话，一闪就没了，等于没给。
    justMade.set(d.key);
    if (nameInput) nameInput.value = '';
    if (noteInput) noteInput.value = '';
    await load();
  } catch (e) { toast(e.message, 'fail'); }
}

function copyBtn(text, label) {
  return h('button', {
    class: 'btn sm', type: 'button',
    onclick: async () => {
      try { await copyText(text); toast('已复制'); }
      catch { toast('复制失败，请手动选择', 'fail'); }
    },
  }, icon('copy'), label || '复制');
}

/** oneTimeCard —— 一次性卡片：完整 key + 复制 + 现成的调用示例。
 *  示例里的 Base URL 用「主机」占位而不是写死 127.0.0.1：面板常常是从另一台机器
 *  打开的，抄过去就连不上。 */
function oneTimeCard(entry) {
  const base = 'http://' + (location.hostname || '主机') + ':7863/v1';
  const platText = !entry.platforms || !entry.platforms.length
    ? '全平台'
    : entry.platforms.map(platName).join('、');
  const curl = `curl ${base}/chat/completions \\\n`
    + `  -H "Authorization: Bearer ${entry.key}" \\\n`
    + `  -H "Content-Type: application/json" \\\n`
    + `  -d '{"model":"cn:workbuddy-plus","messages":[{"role":"user","content":"你好"}]}'`;
  const fill = 'API Provider = OpenAI Compatible\n'
    + `Base URL     = ${base}\n`
    + `API Key      = ${entry.key}\n`
    + 'Model        = 前缀决定平台，如 cn:workbuddy-plus / zai:glm-4.5-air';

  const snippet = (label, text) => h('div', { class: 'stack', style: { gap: '4px' } },
    h('div', { class: 'muted', style: { fontSize: '11.5px' }, text: label }),
    h('div', { class: 'copy-line' },
      h('pre', { class: 'url-box', style: { flex: '1', margin: '0', whiteSpace: 'pre-wrap' }, text }),
      copyBtn(text, '复制这段')),
  );

  return h('section', { class: 'card onecard' },
    h('header', null,
      h('h2', { text: '新密钥 ' + (entry.name || '未命名') }),
      h('span', { class: 'chip strong', text: '只显示这一次' }),
      h('span', { class: 'grow' }),
      h('span', { class: 'chip', text: '可调用：' + platText }),
    ),
    h('div', { class: 'body stack' },
      h('div', { class: 'copy-line' },
        h('code', { class: 'url-box', style: { flex: '1' }, text: entry.key }),
        copyBtn(entry.key, '复制密钥'),
      ),
      h('div', { class: 'muted', style: { fontSize: '12px' },
        text: '关掉这一屏后就再也看不到完整密钥（列表里只剩掩码）。现在把它填进客户端；丢了只能重新生成一把。' }),
      snippet('先拿 curl 试一把', curl),
      snippet('客户端里这么填（Cline / Cursor / 任意 OpenAI 兼容的都一样）', fill),
      h('div', { class: 'row' }, h('span', { class: 'grow' }),
        h('button', { class: 'btn primary', onclick: () => justMade.set(null) }, icon('check'), '我已保存')),
    ),
  );
}

/** scopeBox —— 「全平台 / 指定平台」+（只有选了指定才出现的）可搜索多选清单。
 *  原来这里平铺 20 个按钮，既看不出「默认不用选」，也搜不到要的那个平台。
 *  搜索框和清单都是各自独立的节点、只换内容不重建：重建会把正在输入的搜索词
 *  连同焦点一起丢掉（和添加向导里「切步骤保留同一棵树」是同一条理由）。 */
function scopeBox() {
  const segRow = h('div', { class: 'row wrap', style: { gap: '8px' } });
  const hint = h('span', { class: 'muted', style: { fontSize: '11.5px' } });
  const search = h('input', {
    class: 'input search', type: 'search', placeholder: '搜平台（名字或 id）',
    style: { maxWidth: '260px' },
    oninput: () => { platQ.set(search.value); paintList(); },
  });
  const chipsEl = h('div', { class: 'row wrap', style: { gap: '6px' } });
  const summary = h('div', { class: 'muted', style: { fontSize: '11.5px' } });
  const detail = h('div', { class: 'stack scope-detail', style: { gap: '8px' } }, search, chipsEl, summary);
  const box = h('div', { class: 'stack', style: { gap: '10px' } }, segRow, detail);

  const paintList = () => {
    const q = platQ.peek().trim().toLowerCase();
    const chosen = picked.peek();
    const list = ALL_PLATS().filter(id => !q || (id + ' ' + platName(id)).toLowerCase().includes(q));
    chipsEl.replaceChildren(
      ...list.map(id => h('button', {
        class: 'chip ' + (chosen.has(id) ? 'strong' : 'faint'), text: platName(id),
        onclick: () => { togglePlat(id); paint(); },
      })),
      !list.length ? h('span', { class: 'muted', style: { fontSize: '12px' }, text: '没有匹配的平台' }) : null,
    );
    summary.textContent = chosen.size
      ? `已勾 ${chosen.size} 个：${[...chosen].map(platName).join('、')}` : '';
  };

  const paint = () => {
    const s = scope.peek();
    segRow.replaceChildren(
      h('div', { class: 'seg' },
        h('button', { class: s === 'all' ? 'on' : '', text: '全平台', onclick: () => { scope.set('all'); paint(); } }),
        h('button', { class: s === 'some' ? 'on' : '', text: '指定平台', onclick: () => { scope.set('some'); paint(); } }),
      ),
      hint,
    );
    hint.textContent = s === 'all'
      ? '这把 Key 什么模型都能调用'
      : '只有勾上平台的模型能用这把 Key';
    detail.style.display = s === 'some' ? '' : 'none';
    if (s === 'some') paintList();
  };
  return { box, paint };
}

export default defineView({
  id: 'keys',
  page: 'gateway',
  tab: 'API 密钥',
  title: 'API 密钥',
  icon: 'keys',
  keywords: 'key api 密钥 token',
  sub() {
    const k = keys();
    return k ? `${k.length} 把密钥` : '正在读取…';
  },
  render() {
    if (!keys() && !err()) load();
    const list = keys() || [];
    const stats = keyStats() || {};
    const made = justMade();
    const { box: scopeEl, paint } = scopeBox();
    paint();

    return h('div', { class: 'view stack' },
      made ? oneTimeCard(made) : null,

      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '生成新密钥' }),
          h('span', { class: 'hint', text: '默认全平台；要限制范围就选「指定平台」' }),
          h('span', { class: 'grow' }),
          h('button', { class: 'btn', onclick: generate }, icon('plus'), '生成新 Key'),
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
          scopeEl,
        ),
      ),

      h('section', { class: 'card' },
        h('header', null,
          h('h2', { text: '已生成的密钥' }),
          h('span', { class: 'hint', text: '完整密钥不在此显示，丢了重新生成' }),
          h('span', { class: 'grow' }),
          h('button', { class: 'btn sm ghost', onclick: () => load(false) }, icon('refresh'), '刷新'),
        ),
        h('div', { class: 'body stack' },
          err() ? h('div', { class: 'empty' }, h('div', { class: 'd', text: err() }))
            : !list.length ? h('div', { class: 'empty' }, icon('keys'),
              h('div', { class: 't', text: '还没有密钥' }),
              h('div', { class: 'd', text: '填好名称就能生成第一把。' }),
              h('button', {
                class: 'btn primary', style: { marginTop: '12px' },
                onclick: () => { const n = document.getElementById('nk-name'); if (n) { n.focus(); n.scrollIntoView({ block: 'center', behavior: 'smooth' }); } },
              }, icon('plus'), '去生成第一把'))
              : list.map(k => h('div', { class: 'keyrow' },
                h('div', { style: { flex: '1', minWidth: '220px' } },
                  h('div', { class: 'nm' }, k.name || '未命名',
                    k.note ? h('span', { class: 'note', text: ' · ' + k.note }) : null),
                  h('code', { text: k.masked || '••••', title: '完整密钥只在生成那一次显示' }),
                  h('div', { class: 'muted', style: { fontSize: '11.5px' }, text: statLine(stats[k.id]) }),
                ),
                h('div', { class: 'row wrap', style: { gap: '5px' } },
                  (!k.platforms || !k.platforms.length || k.platforms.includes('*'))
                    ? h('span', { class: 'chip strong', text: '全平台' })
                    : k.platforms.map(pp => h('span', { class: 'chip', text: platName(pp) })),
                ),
                h('button', {
                  class: 'btn sm danger', onclick: async () => {
                    // 确认框要写清是哪一把：只说「删除该 Key」的话，多把同名的一删就错
                    if (!await confirmDialog(`删除「${k.name || '未命名'}」（${k.masked}）？正在用它调用的客户端会立即失效。`, { ok: '删除' })) return;
                    try { await api('apikeys/delete', { method: 'POST', body: JSON.stringify({ id: k.id }) }); toast('Key 已删除'); await load(); }
                    catch (e) { toast(e.message, 'fail'); }
                  },
                }, icon('trash'), '删除'),
              )),
        ),
      ),

      h('section', { class: 'card' },
        h('header', null, h('h2', { text: '调用说明' })),
        h('div', { class: 'body' },
          h('details', { class: 'adv' },
            h('summary', { text: '模型名前缀对照表（决定调用哪个平台）' }),
            h('div', { class: 'stack', style: { fontSize: '12.5px', color: 'var(--fg-2)', lineHeight: '1.9', gap: '8px' } },
              h('div', null, '地址统一 ', h('code', { style: { fontFamily: 'var(--mono)' }, text: 'http://主机:7863/v1' }), '，模型名前缀决定平台：'),
              h('div', { class: 'row wrap', style: { gap: '6px' } },
                ...platWithPrefix().map(pp => h('span', { class: 'chip' },
                  h('code', { style: { fontFamily: 'var(--mono)' }, text: pp.prefix }), pp.name)),
              ),
              h('div', { class: 'muted' }, '未授权对应平台的 Key 调用该平台模型返回 403；主 api_key（配置页）始终全平台。'),
            ),
          ),
        ),
      ),
    );
  },
});
