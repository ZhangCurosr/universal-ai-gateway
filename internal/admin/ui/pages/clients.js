// 客户端页：列表、新建、编辑、禁用 / 启用、轮换 Token（UI-10、UI-15）。

import { h, icon } from '../dom.js';
import { t, fmtInt, fmtTime } from '../i18n.js';
import { api } from '../api.js';
import { parseUSD, nanoToInput } from '../money.js';
import { errorBox, pill, card, tableWrap, pageHeader, loading, money, bar, field, input, select, openDialog, confirmDialog, MAX_ROWS } from '../ui.js';

export async function render(main, ctx) {
  main.replaceChildren(pageHeader(t('cl.title'), ''), loading());
  let list, cfg;
  try {
    [{ clients: list }, cfg] = await Promise.all([api('GET', '/admin/api/clients'), ctx.config()]);
  } catch (e) {
    main.replaceChildren(pageHeader(t('cl.title'), ''), errorBox(e));
    return;
  }
  const models = (cfg.models || []).map((m) => m.name);
  const msg = h('div', { 'aria-live': 'polite' });
  const reload = () => render(main, ctx);

  const showError = (e) => msg.replaceChildren(errorBox(e, () => msg.replaceChildren()));

  const newBtn = h('button', { type: 'button', class: 'btn btn-primary' }, icon('plus', 16), t('cl.new'));
  newBtn.addEventListener('click', () => clientForm(null, models, showError, reload));

  const disabledCount = list.filter((c) => c.disabled).length;
  const head = pageHeader(t('cl.title'), t('cl.sub', { n: list.length, d: disabledCount }), newBtn);

  const q = input({ type: 'search', placeholder: t('cl.searchPh') });
  const st = select([
    { value: 'all', label: t('cl.statusAll') },
    { value: 'on', label: t('cl.statusOn') },
    { value: 'off', label: t('cl.statusOff') },
  ], 'all');
  const filters = h('div', { class: 'filters' },
    h('div', { class: 'grow' }, field('cl-q', t('cl.search'), q)),
    field('cl-st', t('cl.status'), st));

  const tbody = h('tbody');
  const foot = h('div');
  const table = h('table', { class: 'table table-wide' },
    h('thead', {}, h('tr', {},
      th(t('cl.name')), th(t('cl.token')), th(t('cl.quota')), th(t('cl.reserved'), 'num'), th(t('cl.rpm'), 'num'),
      th(t('cl.models')), th(t('cl.status')), th(t('cl.actions'), 'num'))),
    tbody);

  function draw() {
    const needle = q.value.trim().toLowerCase();
    const rows = list.filter((c) => {
      if (st.value === 'on' && c.disabled) return false;
      if (st.value === 'off' && !c.disabled) return false;
      return !needle || c.name.toLowerCase().includes(needle) || (c.token_display || '').toLowerCase().includes(needle);
    });
    tbody.replaceChildren(...rows.slice(0, MAX_ROWS).map((c) => row(c)));
    if (rows.length === 0) tbody.append(h('tr', {}, h('td', { colspan: '8', class: 'muted pad', text: t('cl.empty') })));
    foot.replaceChildren(rows.length > MAX_ROWS ? h('p', { class: 'muted small', text: t('common.tooMany', { n: MAX_ROWS }) }) : '');
  }

  function row(c) {
    const quota = c.quota_unlimited ? null : c.quota_nano_usd;
    const frac = quota ? c.spent_nano_usd / quota : 0;
    const edit = h('button', { type: 'button', class: 'link-btn', text: t('common.edit'), onClick: () => clientForm(c, models, showError, reload) });
    const rotate = h('button', { type: 'button', class: 'link-btn', text: t('cl.rotate'), onClick: () => rotateToken(c, showError, reload) });
    const toggle = h('button', {
      type: 'button',
      class: c.disabled ? 'link-btn' : 'link-btn link-danger',
      text: c.disabled ? t('cl.enable') : t('cl.disable'),
      onClick: () => toggleClient(c, showError, reload),
    });
    return h('tr', { class: c.disabled ? 'row-off' : null },
      h('td', {}, h('div', { class: 'stack-xs' }, h('span', { class: 'strong', text: c.name }),
        h('span', { class: 'muted small', title: c.created_at, text: '#' + c.id + ' · ' + c.role + ' · ' + fmtTime(c.created_at) }))),
      h('td', { class: 'mono small', text: c.token_display }),
      h('td', {}, h('div', { class: 'stack-xs minw-160' },
        h('span', { class: 'small' }, money(c.spent_nano_usd), ' / ', quota === null ? t('common.unlimited') : money(quota)),
        quota === null ? null : bar(frac, frac >= 0.9 ? 'warn' : 'accent'))),
      h('td', { class: 'num small' }, money(c.reserved_nano_usd)),
      h('td', { class: 'num mono', text: c.rpm_limit > 0 ? fmtInt(c.rpm_limit) : t('common.unlimited') }),
      h('td', { class: 'small models-cell', text: c.allowed_models && c.allowed_models.length ? c.allowed_models.join(', ') : t('common.all') }),
      h('td', {}, c.disabled ? pill(t('cl.statusOff'), 'off') : pill(t('cl.statusOn'), 'ok')),
      h('td', { class: 'num actions' }, edit, rotate, toggle));
  }

  q.addEventListener('input', draw);
  st.addEventListener('change', draw);
  draw();

  main.replaceChildren(head, msg, filters, card(null, tableWrap(table)), foot, h('p', { class: 'muted small', text: t('cl.note') }));
  // UI-38：从「局域网共享」页跳来（#/clients?new=<名称>）时直接打开新建表单并预填名称
  const want = new URLSearchParams(location.hash.split('?')[1] || '').get('new');
  if (want) {
    history.replaceState(null, '', '#/clients');
    clientForm(null, models, showError, reload, want.slice(0, 64));
  }
}

function th(text, cls) {
  return h('th', { scope: 'col', class: cls || null, text });
}

async function toggleClient(c, showError, reload) {
  if (!c.disabled) {
    const ok = await confirmDialog(t('cl.confirmDisable', { name: c.name }), t('cl.disable'));
    if (!ok) return;
  }
  try {
    await api('PATCH', '/admin/api/clients/' + c.id, { disabled: !c.disabled });
    reload();
  } catch (e) {
    showError(e);
  }
}

async function rotateToken(c, showError, reload) {
  const ok = await confirmDialog(t('cl.confirmRotate', { name: c.name }), t('cl.rotate'));
  if (!ok) return;
  try {
    const res = await api('POST', '/admin/api/clients/' + c.id + '/rotate-token');
    showToken(res.client.name, res.token, reload);
  } catch (e) {
    showError(e);
  }
}

// showToken 只显示一次新 Token（UI-10）：不写任何存储，对话框关闭后从页面移除。
function showToken(name, token, onDone) {
  const tokenField = input({ type: 'text', readOnly: true, value: token, class: 'input mono' });
  const copy = h('button', { type: 'button', class: 'btn', text: t('common.copy') });
  copy.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(tokenField.value);
    } catch (_) {
      tokenField.select(); // 剪贴板不可用（非安全上下文）时选中文本，由用户手动复制
      return;
    }
    copy.textContent = t('common.copied');
    copy.classList.add('btn-ok');
  });
  const body = h('div', { class: 'stack' },
    h('p', { class: 'muted', text: t('cl.tokenSub') }),
    h('div', { class: 'alert alert-warn' }, icon('warn'), h('div', { class: 'alert-body' },
      h('span', {}, h('strong', { text: t('cl.tokenWarnStrong') }), ' ', t('cl.tokenWarn')))),
    h('div', { class: 'field' }, h('label', { for: 'new-token', text: t('cl.token') }), h('div', { class: 'row-wrap' }, tokenField, copy)),
    h('div', { class: 'help-box small' },
      h('span', { class: 'strong', text: t('cl.tokenUse') }),
      h('span', {}, 'Claude Code: ', h('span', { class: 'mono', text: 'ANTHROPIC_AUTH_TOKEN=<token>' })),
      h('span', {}, 'OpenAI SDK: ', h('span', { class: 'mono', text: 'OPENAI_API_KEY=<token>' }))));
  tokenField.id = 'new-token';
  const done = h('button', { type: 'button', class: 'btn btn-primary', text: t('cl.tokenDone') });
  const { close } = openDialog(t('cl.tokenTitle', { name }), body, [done], {
    wide: true,
    onClose: () => { tokenField.value = ''; onDone(); },
  });
  done.addEventListener('click', () => close());
  tokenField.select();
}

// clientForm 新建（c 为 null）或编辑客户端。
function clientForm(c, models, showError, reload, prefillName) {
  const editing = !!c;
  const err = h('div', { 'aria-live': 'polite' });
  const name = input({ type: 'text', maxlength: '64', value: editing ? c.name : (prefillName || '') });
  const role = select([{ value: 'user', label: t('cl.roleUser') }, { value: 'admin', label: t('cl.roleAdmin') }], editing ? c.role : 'user');
  const unlimited = h('input', { type: 'checkbox', checked: editing ? c.quota_unlimited : false });
  const quota = input({ type: 'text', inputmode: 'decimal', value: editing ? nanoToInput(c.quota_nano_usd) : '0' });
  const rpm = input({ type: 'number', min: '0', step: '1', value: String(editing ? c.rpm_limit : 60) });
  const picked = new Set(editing && c.allowed_models ? c.allowed_models : []);
  const modelBoxes = models.map((m) => {
    const box = h('input', { type: 'checkbox', value: m, checked: picked.has(m) });
    return { m, box, el: h('label', { class: 'check' }, box, h('span', { class: 'mono small', text: m })) };
  });
  const syncQuota = () => { quota.disabled = unlimited.checked; };
  unlimited.addEventListener('change', syncQuota);
  syncQuota();

  const body = h('form', { class: 'stack', novalidate: true },
    err,
    field('cf-name', t('cl.name'), name),
    field('cf-role', t('cl.role'), role),
    h('label', { class: 'check' }, unlimited, h('span', { text: t('cl.quotaUnlimited') })),
    field('cf-quota', t('cl.quotaUSD'), quota, t('cl.quotaHint')),
    field('cf-rpm', t('cl.rpm'), rpm, t('cl.rpmHint')),
    h('fieldset', { class: 'fieldset' }, h('legend', { text: t('cl.models') }),
      h('span', { class: 'hint', text: t('cl.modelsHint') }),
      h('div', { class: 'check-grid' }, modelBoxes.map((x) => x.el))));

  const save = h('button', { type: 'button', class: 'btn btn-primary', text: t('common.save') });
  const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
  const { close } = openDialog(editing ? t('cl.formEdit', { name: c.name }) : t('cl.formNew'), body, [cancel, save], { wide: true });
  cancel.addEventListener('click', () => close());
  body.addEventListener('submit', (ev) => ev.preventDefault());

  save.addEventListener('click', async () => {
    err.replaceChildren();
    const nm = name.value.trim();
    if (!nm || nm.length > 64) {
      err.append(errorBox({ code: 'invalid_request', message: t('cl.nameRequired') }));
      return;
    }
    let quotaNano = 0;
    if (!unlimited.checked) {
      const p = parseUSD(quota.value);
      if (!p.ok) {
        err.append(errorBox({ code: 'invalid_request', message: t('money.' + p.error) }));
        return;
      }
      quotaNano = p.nano;
    }
    const rpmVal = Number.parseInt(rpm.value, 10);
    const payload = {
      name: nm,
      role: role.value,
      quota_unlimited: unlimited.checked,
      quota_nano_usd: quotaNano,
      rpm_limit: Number.isFinite(rpmVal) ? rpmVal : 0,
      allowed_models: modelBoxes.filter((x) => x.box.checked).map((x) => x.m),
    };
    save.disabled = true;
    try {
      if (editing) {
        await api('PATCH', '/admin/api/clients/' + c.id, payload);
        close();
        reload();
      } else {
        const res = await api('POST', '/admin/api/clients', payload);
        close();
        showToken(res.client.name, res.token, reload);
      }
    } catch (e) {
      err.append(errorBox(e));
    } finally {
      save.disabled = false;
    }
  });
  name.focus();
}
