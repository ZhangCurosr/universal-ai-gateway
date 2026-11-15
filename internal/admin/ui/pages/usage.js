// 用量页（UI-41，T79）：趋势图 + 按服务 / 客户端 / 池 / Key 分组的明细 + CSV 导出。
// 单位列由接口返回的 units 动态生成（kind = tokens 的在前），页面不写死任何单位名（PRC-06）：
// 新接入的适配器报告新单位时不用改页面。时间按本机时区分桶与显示，查询时换算成 UTC（UI-14）。

import { h } from '../dom.js';
import { t, fmtInt, fmtNum, fmtTime } from '../i18n.js';
import { api, getToken } from '../api.js';
import { errorBox, card, tableWrap, pageHeader, loading, money, field, input, select, MAX_ROWS } from '../ui.js';

const RANGES = [['24h', 24], ['7d', 24 * 7], ['30d', 24 * 30]];
const GROUPS = ['model', 'client', 'pool', 'key'];
const HOURLY_MAX_HOURS = 48; // 区间不超过 48 小时按小时分桶，否则按天（接口限制按小时最多 7 天）
const SERIES = 6; // 图表配色数（style.css 的 .chart-s0 ~ .chart-s5）

function toLocalInput(d) {
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function parseLocal(v) {
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

function rfc3339(d) {
  return d.toISOString().replace(/\.\d+Z$/, 'Z');
}

// tzParam 是本机当前的 UTC 偏移（±HH:MM），用于按本地日界分桶。
function tzParam(d) {
  const off = -d.getTimezoneOffset();
  const s = off >= 0 ? '+' : '-';
  const a = Math.abs(off);
  return s + String(Math.floor(a / 60)).padStart(2, '0') + ':' + String(a % 60).padStart(2, '0');
}

// unitLabel：常见单位有中文名，其他单位显示原名（未来的新单位照样可用）。
function unitLabel(u) {
  const key = 'u.unit.' + u;
  const s = t(key);
  return s === key ? u : s;
}

export async function render(main) {
  const head = () => pageHeader(t('u.title'), t('u.sub'));
  main.replaceChildren(head(), loading());
  let clients, config;
  try {
    [{ clients }, config] = await Promise.all([api('GET', '/admin/api/clients'), api('GET', '/admin/api/config')]);
  } catch (e) {
    main.replaceChildren(head(), errorBox(e));
    return;
  }

  const now = new Date();
  const clientSel = select([{ value: '', label: t('u.allClients') }].concat(
    clients.slice().sort((a, b) => a.name.localeCompare(b.name)).map((c) => ({ value: String(c.id), label: c.name }))), '');
  const pools = (config.pools || []).map((p) => p.name).sort();
  const poolSel = select([{ value: '', label: t('u.allPools') }].concat(pools.map((p) => ({ value: p, label: p }))), '');
  const groupSel = select(GROUPS.map((g) => ({ value: g, label: t('u.by.' + g) })), 'model');
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
  const csvBtn = h('button', { type: 'button', class: 'btn', text: t('u.csv') });
  const form = h('form', { class: 'card filters-card', 'aria-label': t('u.query'), novalidate: true },
    h('div', { class: 'grow' }, field('u-client', t('u.client'), clientSel)),
    h('div', { class: 'grow' }, field('u-pool', t('u.pool'), poolSel)),
    h('div', { class: 'grow' }, field('u-group', t('u.groupBy'), groupSel)),
    h('div', { class: 'grow' }, field('u-from', t('u.from'), fromIn)),
    h('div', { class: 'grow' }, field('u-to', t('u.to'), toIn)),
    h('div', { class: 'row-wrap' }, rangeBtns),
    h('div', { class: 'row-wrap' }, queryBtn, csvBtn));
  const out = h('div', { class: 'stack-lg' });
  form.addEventListener('submit', (ev) => { ev.preventDefault(); query(); });
  groupSel.addEventListener('change', () => query());
  for (const el of [fromIn, toIn]) el.addEventListener('change', () => { for (const x of rangeBtns) x.setAttribute('aria-pressed', 'false'); });

  // params 返回公共查询参数；区间不合法时返回 null。
  function params() {
    const from = parseLocal(fromIn.value);
    const to = parseLocal(toIn.value);
    if (!from || !to || from >= to) return null;
    const p = new URLSearchParams({ from: rfc3339(from), to: rfc3339(to), tz: tzParam(from) });
    if (clientSel.value) p.set('client_id', clientSel.value);
    if (poolSel.value) p.set('pool', poolSel.value);
    const hourly = (to - from) <= HOURLY_MAX_HOURS * 3600 * 1000;
    return { p, from, to, interval: hourly ? 'hour' : 'day' };
  }

  async function query() {
    const q = params();
    if (!q) {
      out.replaceChildren(errorBox({ code: 'invalid_request', message: t('u.badRange') }));
      return;
    }
    const trendQ = new URLSearchParams(q.p);
    trendQ.set('group', 'time');
    trendQ.set('interval', q.interval);
    const tableQ = new URLSearchParams(q.p);
    tableQ.set('group', groupSel.value);
    out.replaceChildren(loading());
    queryBtn.disabled = true;
    try {
      const [trend, table] = await Promise.all([
        api('GET', '/admin/api/usage/stats?' + trendQ.toString()),
        api('GET', '/admin/api/usage/stats?' + tableQ.toString()),
      ]);
      out.replaceChildren(...result(trend, table, q, groupSel.value));
    } catch (e) {
      out.replaceChildren(errorBox(e));
    } finally {
      queryBtn.disabled = false;
    }
  }

  // CSV：带 Authorization 头取回后交给浏览器下载（UI-09：Token 不进 URL）。
  csvBtn.addEventListener('click', async () => {
    const q = params();
    if (!q) {
      out.replaceChildren(errorBox({ code: 'invalid_request', message: t('u.badRange') }));
      return;
    }
    q.p.set('group', groupSel.value);
    q.p.set('format', 'csv');
    csvBtn.disabled = true;
    try {
      const resp = await fetch('/admin/api/usage/stats?' + q.p.toString(), {
        headers: { Authorization: 'Bearer ' + getToken() }, credentials: 'omit', cache: 'no-store',
      });
      if (!resp.ok) throw { code: 'http_' + resp.status, message: t('u.csvFailed', { status: resp.status }) };
      const url = URL.createObjectURL(await resp.blob());
      const a = h('a', { href: url, download: 'usage-' + groupSel.value + '.csv' });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      out.prepend(errorBox(e));
    } finally {
      csvBtn.disabled = false;
    }
  });

  main.replaceChildren(head(), form, out);
  await query();
}

function result(trend, table, q, group) {
  const totals = table.totals || { requests: 0, usage: {}, tokens: 0, cost_nano_usd: 0, estimated_requests: 0 };
  if (!totals.requests) return [card(null, h('p', { class: 'muted pad', text: t('u.empty') }))];
  const units = (table.units || []).filter((u) => (totals.usage || {})[u.name] > 0); // 只显示这段时间有用量的单位
  const tokenUnits = units.filter((u) => u.kind === 'tokens');
  const otherUnits = units.filter((u) => u.kind !== 'tokens');
  const usage = totals.usage || {};

  const estShare = totals.requests ? (100 * (totals.estimated_requests || 0)) / totals.requests : 0;
  const statCards = [
    stat(t('u.requests'), fmtInt(totals.requests)),
    stat(t('u.tokens'), fmtInt(totals.tokens || 0), tokenUnits.map((u) => unitLabel(u.name) + ' ' + fmtInt(usage[u.name])).join(' · ')),
  ];
  for (const u of otherUnits) statCards.push(stat(unitLabel(u.name), fmtInt(usage[u.name])));
  statCards.push(h('div', { class: 'stat' }, h('span', { class: 'muted small', text: t('u.cost') }), h('span', { class: 'stat-value' }, money(totals.cost_nano_usd))));
  statCards.push(stat(t('u.estimated'), fmtNum(estShare) + '%', t('u.estimatedHint')));
  const stats = h('section', { class: 'stats', 'aria-label': t('u.total') }, statCards);

  return [
    stats,
    trendCard(trend, q, tokenUnits, otherUnits),
    detailCard(table, group, units),
    h('p', { class: 'muted small', text: t('u.note') }),
  ];
}

function stat(label, value, sub) {
  return h('div', { class: 'stat' },
    h('span', { class: 'muted small', text: label }),
    h('span', { class: 'stat-value mono', text: value }),
    sub ? h('span', { class: 'muted small', text: sub }) : null);
}

// ---------------------------------------------------------------- 趋势图（内联 SVG，无依赖）

const SVG_NS = 'http://www.w3.org/2000/svg';

function s(tag, attrs, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
  for (const c of children.flat()) if (c) el.append(c);
  return el;
}

function sTitle(text) {
  const el = document.createElementNS(SVG_NS, 'title');
  el.textContent = text;
  return el;
}

// buckets 生成 [from, to) 内的全部桶起点（按 tz 偏移对齐，与接口的分桶一致），缺的桶补 0。
function buckets(q) {
  const width = (q.interval === 'hour' ? 3600 : 86400) * 1000;
  const off = -q.from.getTimezoneOffset() * 60000;
  const out = [];
  for (let b = Math.floor((q.from.getTime() + off) / width) * width - off; b < q.to.getTime(); b += width) out.push(b);
  return out;
}

function trendCard(trend, q, tokenUnits, otherUnits) {
  const metrics = [{ value: 'tokens', label: t('u.metricTokens') }]
    .concat(otherUnits.map((u) => ({ value: 'unit:' + u.name, label: unitLabel(u.name) })))
    .concat([{ value: 'requests', label: t('u.requests') }]);
  const metricSel = select(metrics, 'tokens');
  const holder = h('div', { class: 'chart' });
  const legend = h('div', { class: 'chart-legend row-wrap' });
  const byTime = new Map((trend.items || []).map((it) => [new Date(it.time).getTime(), it]));
  const starts = buckets(q);

  function draw() {
    const m = metricSel.value;
    const series = m === 'tokens' ? tokenUnits.map((u) => ({ name: u.name, label: unitLabel(u.name), get: (it) => (it.usage || {})[u.name] || 0 }))
      : m === 'requests' ? [{ name: 'requests', label: t('u.requests'), get: (it) => it.requests || 0 }]
        : [{ name: m.slice(5), label: unitLabel(m.slice(5)), get: (it) => (it.usage || {})[m.slice(5)] || 0 }];
    const rows = starts.map((b) => {
      const it = byTime.get(b);
      return { b, vals: series.map((sr) => (it ? sr.get(it) : 0)) };
    });
    const max = Math.max(1, ...rows.map((r) => r.vals.reduce((a, v) => a + v, 0)));
    const W = 760, H = 220, L = 56, B = 24, T = 8;
    const cw = (W - L) / Math.max(1, rows.length);
    const bw = Math.max(1, cw * 0.72);
    const y = (v) => T + (H - T - B) * (1 - v / max);
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, class: 'chart-svg', role: 'img', 'aria-label': t('u.trend') });
    for (const f of [0, 0.5, 1]) {
      const v = max * f;
      svg.append(s('line', { x1: L, x2: W, y1: y(v), y2: y(v), class: 'chart-grid' }));
      const lbl = s('text', { x: L - 6, y: y(v) + 4, class: 'chart-axis', 'text-anchor': 'end' });
      lbl.textContent = fmtNum(Math.round(v));
      svg.append(lbl);
    }
    const every = Math.max(1, Math.ceil(rows.length / 8));
    rows.forEach((r, i) => {
      const x = L + i * cw + (cw - bw) / 2;
      let acc = 0;
      r.vals.forEach((v, k) => {
        if (v <= 0) return;
        const top = y(acc + v), bottom = y(acc);
        acc += v;
        svg.append(s('rect', { x, y: top, width: bw, height: Math.max(0.5, bottom - top), class: 'chart-s' + (k % SERIES) },
          sTitle(fmtTime(new Date(r.b).toISOString()) + ' · ' + series[k].label + ' ' + fmtInt(v))));
      });
      if (i % every === 0) {
        const d = new Date(r.b);
        const lbl = s('text', { x: x + bw / 2, y: H - 6, class: 'chart-axis', 'text-anchor': 'middle' });
        lbl.textContent = q.interval === 'hour' ? String(d.getHours()).padStart(2, '0') + ':00' : (d.getMonth() + 1) + '/' + d.getDate();
        svg.append(lbl);
      }
    });
    holder.replaceChildren(svg);
    legend.replaceChildren(...series.map((sr, k) => h('span', { class: 'chart-key' },
      h('span', { class: 'chart-swatch chart-bg' + (k % SERIES), 'aria-hidden': 'true' }), sr.label)));
  }
  metricSel.addEventListener('change', draw);
  draw();
  const title = t('u.trend') + ' · ' + t(q.interval === 'hour' ? 'u.perHour' : 'u.perDay');
  return card(title, h('div', { class: 'row-wrap chart-head' }, field('u-metric', t('u.metric'), metricSel), legend), holder);
}

// ---------------------------------------------------------------- 明细表

function dimLabel(it, group) {
  switch (group) {
    case 'client': return it.client_name || '#' + it.client_id;
    case 'pool': return it.pool || t('u.noPool');
    case 'key': return it.key_name ? it.key_name + ' (#' + it.key_id + ')' : '#' + it.key_id;
    default: return it.model;
  }
}

function detailCard(table, group, units) {
  const rows = (table.items || []).slice().sort((a, b) => (b.cost_nano_usd - a.cost_nano_usd) || (b.tokens - a.tokens));
  const th = (text, cls) => h('th', { scope: 'col', class: cls || null, text });
  const num = (v) => h('td', { class: 'num mono', text: v ? fmtInt(v) : t('common.dash') });
  const body = rows.slice(0, MAX_ROWS).map((it) => h('tr', {},
    h('td', { class: group === 'model' ? 'mono small strong' : 'strong', text: dimLabel(it, group) }),
    h('td', { class: 'num mono', text: fmtInt(it.requests) }),
    h('td', { class: 'num mono strong', text: fmtInt(it.tokens || 0) }),
    units.map((u) => num((it.usage || {})[u.name])),
    num(it.estimated_requests),
    h('td', { class: 'num strong' }, money(it.cost_nano_usd))));
  const tt = table.totals;
  const tbl = h('table', { class: 'table table-wide' },
    h('thead', {}, h('tr', {}, th(t('u.by.' + group)), th(t('u.requests'), 'num'), th(t('u.tokens'), 'num'),
      units.map((u) => th(unitLabel(u.name), 'num')), th(t('u.estimatedShort'), 'num'), th(t('u.cost'), 'num'))),
    h('tbody', {}, body),
    h('tfoot', {}, h('tr', {},
      h('th', { scope: 'row', text: t('u.total') }),
      h('td', { class: 'num mono', text: fmtInt(tt.requests) }),
      h('td', { class: 'num mono', text: fmtInt(tt.tokens || 0) }),
      units.map((u) => num((tt.usage || {})[u.name])),
      num(tt.estimated_requests),
      h('td', { class: 'num strong' }, money(tt.cost_nano_usd)))));
  const parts = [tableWrap(tbl)];
  if (rows.length > MAX_ROWS) parts.push(h('p', { class: 'muted small pad', text: t('common.tooMany', { n: MAX_ROWS }) }));
  return card(t('u.detail'), ...parts);
}
