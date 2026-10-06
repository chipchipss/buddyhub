/* ══════════════════════════════════════════════════════════════════
   views/ext-segment.js · 账号 → 外部平台
   extstore 账号的管理面：卡片（状态/余额/签到/停用/删除）+ 添加入口。
   从 automation.js 迁来——账号管理归「账号」，任务队列归「任务」。
   平台名与「有没有签到」都来自注册表（platforms.js）。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog } from '../kernel.js';
import { platforms, loadPlatforms, platName, platHasCheckin } from '../platforms.js';
import { statusOf, statusChip } from '../status.js';
import { extAddPanel } from './ext-add.js';

const ext = signal(null);
const extMsg = signal('');

export async function loadExt(quiet = true) {
  try {
    await loadPlatforms();
    ext.set(await api('ext/accounts'));
    extMsg.set('');
  }
  catch (e) { extMsg.set(e.message); if (!quiet) toast(e.message, 'fail'); }
}

async function extAct(provider, id, action, body) {
  try {
    const r = await api(`ext/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}/${action}`, {
      method: 'POST', body: body ? JSON.stringify(body) : undefined,
    });
    if (action === 'checkin') {
      const res = r.result || {};
      toast(`[${platName(provider)}] ${res.message || res.kind || '完成'}`, res.kind === 'failed' ? 'fail' : undefined);
    } else if (action === 'remove') toast('已删除');
    await loadExt();
  } catch (e) { toast(e.message, 'fail'); }
}

/** extSegment(focusProvider) —— 外部平台账号管理面。
 *  focusProvider 传入平台 id 时只显示该平台的账号，标题随平台，
 *  且隐藏「一键签到全部」（跨平台动作在无 focus 的聚合视图里）。 */
export function extSegment(focusProvider = '') {
  const d = ext();
  const all = (d && d.accounts) || [];
  const list = focusProvider ? all.filter(a => a.provider === focusProvider) : all;
  const title = focusProvider ? platName(focusProvider) : '外部平台账号';
  return h('section', { class: 'card' },
    h('header', null,
      h('h2', { text: title }),
      h('span', { class: 'hint', text: '每日签到由「任务」排程执行' }),
      h('span', { class: 'grow' }),
      h('span', { class: 'hint', text: d ? `${list.length} 个账号 · ${list.filter(a => a.balance_ok).length} 个余额正常` : '' }),
      h('button', { class: 'btn sm ghost', onclick: () => loadExt(false) }, icon('refresh'), '刷新'),
      focusProvider ? null : h('button', {
        class: 'btn sm primary', onclick: async ev => {
          var __b = ev.currentTarget; if (__b) __b.disabled = true;
          try {
            const r = await api('ext/checkin_all', { method: 'POST' });
            const rs = r.results || [];
            const ok = rs.filter(x => x.kind === 'claimed').length;
            const already = rs.filter(x => x.kind === 'already-claimed').length;
            const failed = rs.filter(x => x.kind === 'failed').length;
            toast(`签到完成：成功 ${ok} · 已领过 ${already} · 失败 ${failed}`, failed ? 'fail' : undefined);
            await loadExt();
          } catch (e) { toast(e.message, 'fail'); }
          finally { if (__b) __b.disabled = false; }
        },
      }, icon('check'), '一键签到全部'),
    ),
    h('div', { class: 'body stack' },
      extMsg() ? h('div', { class: 'empty' }, h('div', { class: 'd', text: extMsg() }))
        : list.length
          ? h('div', { class: 'acct-grid' }, ...list.map(a => h('article', { class: 'acct' + (a.disabled ? ' off' : '') },
            h('div', { class: 'top' },
              h('div', { class: 'who' },
                h('div', { class: 'nm', text: focusProvider ? (a.label || a.id) : `${platName(a.provider)} · ${a.label || a.id}` }),
                h('div', { class: 'id', text: a.id }),
              ),
              statusChip(statusOf(a)),
            ),
            h('div', { class: 'credits' },
              h('div', { class: 'line' },
                // 无签到的通道（Copilot / Cline / AutoClaw）按「订阅状态」呈现，
                // 而不是硬凑一个 0 分余额——有没有签到由注册表说了算。
                !platHasCheckin(a.provider)
                  ? h('span', { class: 'of', style: { fontSize: '12px' }, text: a.note || '已接入' })
                  : h('span', { class: 'n', text: a.balance_ok ? String(a.balance ?? 0) : '—' }),
                platHasCheckin(a.provider)
                  ? h('span', { class: 'of', text: a.balance_ok ? '分' : (a.note || '') })
                  : null,
              ),
            ),
            h('div', { class: 'acts' },
              platHasCheckin(a.provider)
                ? h('button', { class: 'btn', disabled: a.disabled, onclick: () => extAct(a.provider, a.id, 'checkin') }, '签到')
                : null,
              h('button', {
                class: 'btn', onclick: () => extAct(a.provider, a.id, 'toggle', { disabled: !a.disabled }),
              }, a.disabled ? '启用' : '停用'),
              h('button', {
                class: 'btn danger', onclick: async () => {
                  if (await confirmDialog(`确定删除 ${platName(a.provider)} 账号 ${a.id}？`, { ok: '删除' })) {
                    await extAct(a.provider, a.id, 'remove');
                  }
                },
              }, '删除'),
            ),
          )))
          : h('div', { class: 'empty' }, icon('accounts'),
            h('div', { class: 't', text: focusProvider ? `还没有 ${platName(focusProvider)} 账号` : '还没有外部账号' }),
            h('div', { class: 'd', text: '点右上角「添加账号」，或在下方表单直接添加' })),

      // 逐字段添加（每个平台按自己的凭据形态出表单）；focus 时锁定该平台
      extAddPanel(loadExt, focusProvider ? { lockProvider: focusProvider } : {}),

      h('details', { style: { marginTop: '6px' } },
        h('summary', { class: 'muted', style: { cursor: 'pointer', fontSize: '12.5px' }, text: '高级：粘贴完整凭据 JSON' }),
        h('div', { class: 'row wrap', style: { marginTop: '10px' } },
          h('select', { class: 'input', id: 'ext-provider', style: { width: 'auto' } },
            ...platformsList().map(p => h('option', { value: p.id, text: p.name }))),
          h('input', { class: 'input', id: 'ext-id', placeholder: '账号 ID', style: { flex: '1', minWidth: '140px' } }),
          h('input', { class: 'input', id: 'ext-cred', placeholder: '凭据 JSON', style: { flex: '2', minWidth: '200px', fontFamily: 'var(--mono)', fontSize: '11.5px' } }),
          h('button', {
            class: 'btn sm primary', onclick: async () => {
              const provider = document.getElementById('ext-provider').value;
              const id = document.getElementById('ext-id').value.trim();
              const raw = document.getElementById('ext-cred').value.trim();
              if (!id) { toast('请填写账号 ID', 'fail'); return; }
              let cred;
              try { cred = JSON.parse(raw); } catch { toast('凭据不是合法 JSON', 'fail'); return; }
              try {
                await api('ext/accounts', { method: 'POST', body: JSON.stringify({ provider, id, cred }) });
                toast('账号已添加');
                document.getElementById('ext-id').value = '';
                document.getElementById('ext-cred').value = '';
                await loadExt();
              } catch (e) { toast(e.message, 'fail'); }
            },
          }, '添加'),
        ),
      ),
    ),
  );
}

const platformsList = () => platforms.peek().filter(p => p.login);
