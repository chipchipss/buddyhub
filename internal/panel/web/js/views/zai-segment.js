/* ══════════════════════════════════════════════════════════════════
   views/zai-segment.js · 自动化 → Z.AI（ZCode）
   Z.AI 账号池：Plan（JWT，需验证码）与 API Key 回退通道的统一管理面。
   ══════════════════════════════════════════════════════════════════ */

import { h, icon, signal, api, toast, confirmDialog } from '../kernel.js';

const zai = signal(null);
const zaiErr = signal('');
const addOpen = signal(false);
const addName = signal('');
const addSecret = signal('');
const addProvider = signal('zai');

const STATUS_LABEL = {
  active: ['可用', 'strong'],
  cooling: ['冷却中', ''],
  exhausted: ['额度用完', 'faint'],
  invalid: ['凭证失效', 'faint'],
  disabled: ['风控禁用', 'faint'],
};

export async function loadZai(quiet = true) {
  try {
    zai.set(await api('zai/accounts'));
    zaiErr.set('');
  } catch (e) {
    zaiErr.set(e.message);
    if (!quiet) toast(e.message, 'fail');
  }
}

async function act(id, action, body, okMsg) {
  try {
    const r = await api(`zai/accounts/${encodeURIComponent(id)}/${action}`, {
      method: 'POST', body: body ? JSON.stringify(body) : undefined,
    });
    toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
    await loadZai();
    return r;
  } catch (e) {
    toast(e.message, 'fail');
    return null;
  }
}

function statusChip(a) {
  const [label, cls] = STATUS_LABEL[a.status] || [a.status, 'faint'];
  return h('span', { class: 'chip ' + cls, title: a.last_error || '' },
    h('i', { class: a.status === 'active' ? 'dot' : a.status === 'cooling' ? 'dot ring' : 'dot off' }),
    label + (a.cool_remaining_sec ? ` · ${Math.round(a.cool_remaining_sec / 60)}分` : ''));
}

function accountCard(a) {
  const fp = a.fingerprint || {};
  const quota = Object.entries(a.quota || {});
  return h('article', { class: 'acct' + (a.enabled ? '' : ' off') },
    h('div', { class: 'top' },
      h('div', { class: 'who' },
        h('div', { class: 'nm', text: a.name }),
        h('div', { class: 'id', text: a.masked || '' }),
      ),
      h('div', { class: 'row', style: { gap: '5px', flex: 'none' } },
        h('span', { class: 'chip faint', text: a.mode === 'jwt' ? 'Plan JWT' : 'API Key' }),
        statusChip(a),
      ),
    ),

    h('div', { class: 'metrics' },
      h('div', { class: 'metric' },
        h('div', { class: 'v', text: String(a.use_count || 0) }), h('div', { class: 'k', text: '调用' })),
      h('div', { class: 'metric' },
        h('div', { class: 'v', text: String(a.fail_count || 0) }), h('div', { class: 'k', text: '失败' })),
      h('div', { class: 'metric' },
        h('div', { class: 'v', text: String(a.in_flight || 0) }), h('div', { class: 'k', text: '在途' })),
    ),

    quota.length
      ? h('div', { class: 'row wrap', style: { gap: '5px', marginTop: '10px' } },
        ...quota.map(([model, q]) => h('span', { class: 'chip', title: '已用 / 总量' },
          `${model} ${q.remaining ?? '—'}`)))
      : null,

    h('div', { class: 'row wrap', style: { gap: '5px', marginTop: '10px' } },
      h('span', { class: 'chip faint', title: 'X-Platform / X-Os-Version / X-Device-Mid' },
        `${fp.platform || '?'}-${fp.arch || '?'} · ${fp.os_version || '?'}`),
      a.has_key_fallback ? h('span', { class: 'chip faint', text: '带回退 Key' }) : null,
      a.risk_strikes ? h('span', { class: 'chip', text: `风控 ${a.risk_strikes} 次` }) : null,
    ),

    a.last_error
      ? h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '8px' }, text: a.last_error })
      : null,

    h('div', { class: 'acts' },
      h('button', {
        class: 'btn', onclick: () => act(a.id, 'toggle', null, r => r.enabled ? '已启用' : '已停用'),
      }, a.enabled ? '停用' : '启用'),
      h('button', {
        class: 'btn', title: '换发整套桌面设备指纹（新 device_mid）',
        onclick: () => act(a.id, 'rotate', null, '已换发设备指纹'),
      }, '换指纹'),
      h('button', {
        class: 'btn danger', onclick: async () => {
          if (!await confirmDialog(`删除 Z.AI 账号「${a.name}」？`, { ok: '删除' })) return;
          await act(a.id, 'remove', null, '已删除');
        },
      }, '删除'),
    ),
  );
}

function addPanel() {
  if (!addOpen.peek()) {
    return h('div', { class: 'row' },
      h('button', { class: 'btn primary', onclick: () => addOpen.set(true) }, icon('plus'), '添加 Z.AI 账号'),
    );
  }
  return h('div', { class: 'glass-flat', style: { padding: '14px' } },
    h('div', { class: 'row wrap', style: { gap: '8px' } },
      h('select', {
        class: 'input', style: { width: 'auto' },
        onchange: ev => addProvider.set(ev.target.value),
      },
        h('option', { value: 'zai', selected: addProvider.peek() === 'zai', text: 'Z.AI（支持 JWT / API Key）' }),
        h('option', { value: 'bigmodel', selected: addProvider.peek() === 'bigmodel', text: '智谱开放平台（API Key）' }),
      ),
      h('input', {
        class: 'input', placeholder: '账号名称（如：主号）', style: { flex: '1', minWidth: '150px' },
        oninput: ev => addName.set(ev.target.value),
      }),
    ),
    h('div', { class: 'stack', style: { marginTop: '10px' } },
      h('input', {
        class: 'input', placeholder: addProvider.peek() === 'bigmodel'
          ? '智谱开放平台 API Key'
          : 'Coding Plan JWT（三段点分）或 Z.AI API Key',
        style: { fontFamily: 'var(--mono)', fontSize: '12px' },
        oninput: ev => addSecret.set(ev.target.value),
      }),
      h('div', { class: 'muted', style: { fontSize: '11.5px' },
        text: addProvider.peek() === 'bigmodel'
          ? '智谱开放平台（open.bigmodel.cn）的 API Key，走 Anthropic 兼容端点。'
          : 'JWT 走 Plan 通道（消耗订阅额度，需配置验证码求解器）；API Key 走回退通道（免验证码）。' }),
      h('div', { class: 'row' },
        h('button', {
          class: 'btn primary', onclick: async ev => {
            const secret = addSecret.peek().trim();
            if (!secret) { toast('请粘贴 JWT 或 API Key', 'fail'); return; }
            ev.currentTarget.disabled = true;
            try {
              const r = await api('zai/accounts', {
                method: 'POST',
                body: JSON.stringify({ name: addName.peek().trim(), secret, provider: addProvider.peek() }),
              });
              toast(`已入池（识别为 ${r.mode === 'jwt' ? 'Plan JWT' : 'API Key'}）`);
              addName.set(''); addSecret.set(''); addOpen.set(false);
              await loadZai();
            } catch (e) { toast(e.message, 'fail'); }
            finally { ev.currentTarget.disabled = false; }
          },
        }, icon('check'), '入池'),
        h('button', { class: 'btn ghost', onclick: () => addOpen.set(false) }, '取消'),
      ),
    ),
  );
}

export function zaiSegment() {
  // 读信号而非 peek：渲染期读到的信号变化才会触发重渲染（loadZai 回来后本段自动刷新）
  const d = zai();
  const errV = zaiErr();
  if (d === null && !errV) loadZai();
  const captcha = (d && d.captcha) || null;

  if (errV) {
    return h('section', { class: 'card' }, h('div', { class: 'body' },
      h('div', { class: 'empty' }, h('div', { class: 'd', text: errV }))));
  }
  if (!d) return h('section', { class: 'card' }, h('div', { class: 'busy', text: '读取 Z.AI 账号池' }));

  return h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('header', null,
        h('h2', { text: 'Z.AI / ZCode 账号池' }),
        h('span', { class: 'grow' }),
        h('span', { class: 'hint', text: d.configured ? `${(d.accounts || []).length} 个账号` : '' }),
        h('button', { class: 'btn sm ghost', onclick: () => loadZai(false) }, icon('refresh'), '刷新'),
      ),
      h('div', { class: 'body stack' },
        d.configured ? null : h('div', { class: 'muted', style: { fontSize: '12.5px' }, text: d.message || '' }),

        // 验证码池状态（Plan 通道的前提）
        captcha
          ? h('div', { class: 'row wrap', style: { gap: '8px' } },
            h('span', { class: 'chip ' + (captcha.enabled ? 'strong' : 'faint') },
              h('i', { class: captcha.enabled ? 'dot' : 'dot off' }),
              captcha.enabled ? `验证码求解器已启用 · 池内 ${captcha.pool_size} 枚` : '未配置验证码求解器'),
            captcha.last_error
              ? h('span', { class: 'chip faint', text: captcha.last_error })
              : null)
          : null,

        addPanel(),

        (d.accounts || []).length
          ? h('div', { class: 'acct-grid' }, ...d.accounts.map(accountCard))
          : h('div', { class: 'empty' }, icon('accounts'),
            h('div', { class: 't', text: '还没有 Z.AI 账号' }),
            h('div', { class: 'd', text: '粘贴 Coding Plan JWT（Plan 通道）或 API Key（回退通道）即可入池' })),
      ),
    ),
  );
}
