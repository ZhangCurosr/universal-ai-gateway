// 用量页：按客户端与时间范围查询（UI-14：本地时间输入，换算成 UTC 发送）。

import { h } from '../dom.js';
import { t, fmtInt } from '../i18n.js';
import { api } from '../api.js';
import { errorBox, card, tableWrap, pageHeader, loading, money, field, input, select, MAX_ROWS } from '../ui.js';

const TOKEN_UNITS = ['input_tokens', 'cache_read_tokens', 'cache_write_tokens', 'output_tokens'];
const RANGES = [['24h', 24], ['7d', 24 * 7], ['30d', 24 * 30]];

// toLocalInput 把 Date 写成 datetime-local 控件的值（本地时区，精确到分钟）。
function toLocalInput(d) {
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function toUTC(localValue) {
  const d = new Date(localValue); // datetime-local 的值按本地时区解析
  return Number.isNaN(d.getTime()) ? null : d;
}

function rfc3339(d) {
  return d.toISOString().replace(/\.\d+Z$/, 'Z');
}

export async function render(main) {
  main.replaceChildren(pageHeader(t('u.title'), t('u.sub')), loading());
  let clients;
  try {
    ({ clients } = await api('GET', '/admin/api/clients'));
  } catch (e) {
    main.replaceChildren(pageHeader(t('u.title'), t('u.sub')), errorBox(e));
    return;
  }
  const nameOf = new Map(clients.map((c) => [c.id, c.name]));

  const now = new Date();
  const clientSel = select([{ value: '', label: t('u.allClients') }].concat(
    clients.slice().sort((a, b) => a.name.localeCompare(b.name)).map((c) => ({ value: String(c.id), label: c.name }))), '');
  const fromIn = input({ type: 'datetime-local', value: toLocalInput(new Date(now.getTime() - 24 * 3600 * 1000)) });
  const toIn = input({ type: 'datetime-local', value: toLocalInput(now) });
  let range = '24h';
  const rangeBtns = RANGES.map(([id, hours]) => {
    const b = h('button', { type: 'button', class: 'btn btn-chip', text: t('u.r' + id), 'aria-pressed': String(id === range) });
    b.addEventListener('click', () => {
      range = id;
      const end = new Date();
      fromIn.value = toLocalInput(new Date(end.getTime() - hours * 3600 * 1000));
      toIn.value = toLocalInput(end);
      for (const x of rangeBtns) x.setAttribute('aria-pressed', String(x === b));
      query();
    });
    return b;
  });
  const queryBtn = h('button', { type: 'submit', class: 'btn btn-primary', text: t('u.query') });
  const form = h('form', { class: 'card filters-card', 'aria-label': t('u.query'), novalidate: true },
    h('div', { class: 'grow' }, field('u-client', t('u.client'), clientSel)),
    h('div', { class: 'grow' }, field('u-from', t('u.from'), fromIn)),
    h('div', { class: 'grow' }, field('u-to', t('u.to'), toIn)),
    h('div', { class: 'row-wrap' }, rangeBtns),
    queryBtn);
  const out = h('div', { class: 'stack-lg' });
  form.addEventListener('submit', (ev) => { ev.preventDefault(); query(); });
  for (const el of [fromIn, toIn]) el.addEventListener('change', () => { for (const x of rangeBtns) x.setAttribute('aria-pressed', 'false'); });

  async function query() {
    const from = toUTC(fromIn.value);
    const to = toUTC(toIn.value);
    if (!from || !to || from >= to) {
      out.replaceChildren(errorBox({ code: 'invalid_request', message: t('u.badRange') }));
      return;
    }
    let qs = 'from=' + encodeURIComponent(rfc3339(from)) + '&to=' + encodeURIComponent(rfc3339(to));
    if (clientSel.value) qs += '&client_id=' + encodeURIComponent(clientSel.value);
    out.replaceChildren(loading());
    queryBtn.disabled = true;
    try {
      const { items } = await api('GET', '/admin/api/usage?' + qs);
      out.replaceChildren(...result(items, nameOf));
    } catch (e) {
      out.replaceChildren(errorBox(e));
    } finally {
      queryBtn.disabled = false;
    }
  }

  main.replaceChildren(pageHeader(t('u.title'), t('u.sub')), form, out);
  await query();
}

function result(items, nameOf) {
  if (!items.length) return [card(null, h('p', { class: 'muted pad', text: t('u.empty') }))];
  const rows = items.slice().sort((a, b) => (b.cost_nano_usd || 0) - (a.cost_nano_usd || 0));
  const totals = { requests: 0, cost: 0, units: {} };
  for (const it of rows) {
    totals.requests += it.requests || 0;
    totals.cost += it.cost_nano_usd || 0;
    for (const [u, n] of Object.entries(it.usage || {})) totals.units[u] = (totals.units[u] || 0) + n;
  }
  const unitCell = (usage, u) => h('td', { class: 'num mono', text: usage && u in usage ? fmtInt(usage[u]) : t('common.dash') });
  const otherText = (usage) => Object.entries(usage || {}).filter(([u]) => !TOKEN_UNITS.includes(u))
    .map(([u, n]) => u + ' ' + fmtInt(n)).join(', ') || t('common.dash');

  const stats = h('section', { class: 'stats', 'aria-label': t('u.total') },
    stat(t('u.requests'), fmtInt(totals.requests)),
    stat(t('u.totalInput'), fmtInt(totals.units.input_tokens || 0)),
    stat(t('u.totalOutput'), fmtInt(totals.units.output_tokens || 0)),
    h('div', { class: 'stat' }, h('span', { class: 'muted small', text: t('u.cost') }), h('span', { class: 'stat-value' }, money(totals.cost))));

  const body = rows.slice(0, MAX_ROWS).map((it) => h('tr', {},
    h('td', { class: 'strong', text: nameOf.get(it.client_id) || '#' + it.client_id }),
    h('td', { class: 'mono small', text: it.model }),
    h('td', { class: 'num mono', text: fmtInt(it.requests) }),
    TOKEN_UNITS.map((u) => unitCell(it.usage, u)),
    h('td', { class: 'num small muted', text: otherText(it.usage) }),
    h('td', { class: 'num strong' }, money(it.cost_nano_usd))));

  const table = h('table', { class: 'table table-wide' },
    h('thead', {}, h('tr', {},
      th(t('u.client')), th(t('u.model')), th(t('u.requests'), 'num'), th(t('u.input'), 'num'), th(t('u.cacheRead'), 'num'),
      th(t('u.cacheWrite'), 'num'), th(t('u.output'), 'num'), th(t('u.other'), 'num'), th(t('u.cost'), 'num'))),
    h('tbody', {}, body),
    h('tfoot', {}, h('tr', {},
      h('th', { scope: 'row', colspan: '2', text: t('u.total') }),
      h('td', { class: 'num mono', text: fmtInt(totals.requests) }),
      TOKEN_UNITS.map((u) => unitCell(totals.units, u)),
      h('td', { class: 'num small muted', text: otherText(totals.units) }),
      h('td', { class: 'num strong' }, money(totals.cost)))));

  const parts = [stats, card(null, tableWrap(table))];
  if (rows.length > MAX_ROWS) parts.push(h('p', { class: 'muted small', text: t('common.tooMany', { n: MAX_ROWS }) }));
  parts.push(h('p', { class: 'muted small', text: t('u.note') }));
  return parts;
}

function stat(label, value) {
  return h('div', { class: 'stat' }, h('span', { class: 'muted small', text: label }), h('span', { class: 'stat-value mono', text: value }));
}

function th(text, cls) {
  return h('th', { scope: 'col', class: cls || null, text });
}
