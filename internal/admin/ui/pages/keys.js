// 上游 Key 页：按池分组、每 10 秒刷新运行时状态（UI-17）、新增、编辑、停用 / 恢复；过期时间与标签（T62）；批量导入（T70）；池摘要与 24 小时统计（T65）。

import { h } from '../dom.js';
import { t, fmtInt, fmtTime, fmtNum } from '../i18n.js';
import { api } from '../api.js';
import { errorBox, notice, pill, tableWrap, pageHeader, loading, bar, field, input, select, openDialog, confirmDialog } from '../ui.js';
import { openImportDialog } from './keys_import.js';

const REFRESH_MS = 10000;

export async function render(main, ctx) {
  main.replaceChildren(pageHeader(t('k.title'), ''), loading());
  let cfg;
  try {
    cfg = await ctx.config();
  } catch (e) {
    main.replaceChildren(pageHeader(t('k.title'), ''), errorBox(e));
    return undefined;
  }

  const status = h('span', { class: 'muted live' });
  const msg = h('div', { 'aria-live': 'polite' });
  const listBox = h('div', { class: 'stack-lg' });
  const refreshBtn = h('button', { type: 'button', class: 'btn', text: t('k.refreshNow') });
  const showError = (e) => msg.replaceChildren(errorBox(e, () => msg.replaceChildren()));
  const protoOf = new Map((cfg.pools || []).map((p) => [p.name, p.protocol]));

  let stopped = false;
  let timer = null;
  const ALL = '*all*'; // 下拉框中「全部标签」的取值
  let lastKeys = [];
  let labelFilter = null; // null = 全部标签；'' = 无标签
  // UI-39：每个池的展开 / 收起状态，在本页停留期间保持（每 10 秒刷新不会重置）；没设置过的池按 Key 数决定
  const COLLAPSE_OVER = 10;
  const collapsed = new Map();
  const isCollapsed = (pool, n) => (collapsed.has(pool) ? collapsed.get(pool) : n > COLLAPSE_OVER);
  const labelSelect = h('select', { class: 'input', 'aria-label': t('k.filterLabel') });
  labelSelect.addEventListener('change', () => {
    labelFilter = labelSelect.value === ALL ? null : labelSelect.value;
    drawList(lastKeys);
  });

  function drawLabelFilter(keys) {
    const labels = [...new Set(keys.map((k) => k.label || ''))].sort();
    if (labelFilter !== null && !labels.includes(labelFilter)) labelFilter = null;
    const cur = labelFilter === null ? ALL : labelFilter;
    const opts = [{ value: ALL, label: t('k.allLabels') }].concat(labels.map((l) => ({ value: l, label: l || t('k.noLabel') })));
    labelSelect.replaceChildren(...opts.map((o) => h('option', { value: o.value, text: o.label, selected: cur === o.value })));
    labelSelect.hidden = labels.length < 2;
  }

  // UI-37：池摘要与每把 Key 的 24 小时统计单独请求；失败时只隐藏这些信息
  let extra = null;
  async function loadExtra() {
    try {
      const [{ pools }, { keys }] = await Promise.all([api('GET', '/admin/api/pools'), api('GET', '/admin/api/keys/stats')]);
      return { pools: new Map(pools.map((p) => [p.pool, p])), stats: new Map(keys.map((k) => [k.id, k])) };
    } catch (_) {
      return null;
    }
  }

  async function load() {
    try {
      const [{ keys }, ex] = await Promise.all([api('GET', '/admin/api/keys'), loadExtra()]);
      if (stopped) return;
      extra = ex;
      lastKeys = keys;
      drawLabelFilter(keys);
      drawList(keys);
      status.replaceChildren(h('span', { class: 'dot dot-ok' }), t('k.auto', { time: new Date().toLocaleTimeString([], { hour12: false }) }));
    } catch (e) {
      if (!stopped) showError(e);
    }
  }

  function schedule() {
    clearTimeout(timer);
    if (stopped) return;
    if (document.hidden) {
      status.replaceChildren(h('span', { class: 'dot dot-off' }), t('k.paused'));
      return;
    }
    timer = setTimeout(async () => { await load(); schedule(); }, REFRESH_MS);
  }

  function onVisibility() {
    if (document.hidden) {
      schedule();
    } else {
      load().then(schedule);
    }
  }

  function drawList(all) {
    const keys = labelFilter === null ? all : all.filter((k) => (k.label || '') === labelFilter);
    const pools = new Map();
    for (const k of keys) {
      if (!pools.has(k.pool)) pools.set(k.pool, []);
      pools.get(k.pool).push(k);
    }
    if (pools.size === 0) {
      listBox.replaceChildren(h('section', { class: 'card' }, h('p', { class: 'muted pad', text: t('k.empty') })));
      return;
    }
    const entries = [...pools.entries()].sort((a, b) => a[0].localeCompare(b[0]));
    const anyOpen = entries.some(([pool, list]) => !isCollapsed(pool, list.length));
    const allBtn = h('button', { type: 'button', class: 'link-btn small', text: t(anyOpen ? 'k.collapseAll' : 'k.expandAll'),
      onClick: () => { for (const [pool] of entries) collapsed.set(pool, anyOpen); drawList(lastKeys); } });
    listBox.replaceChildren(
      entries.length > 1 ? h('div', { class: 'pool-tools' }, allBtn) : null,
      ...entries.map(([pool, list]) => poolCard(pool, list)));
  }

  function poolCard(pool, list) {
    list.sort((a, b) => a.id - b.id);
    const ready = list.filter((k) => k.state === 'ready' && !(k.cooldown_seconds > 0) && k.breaker !== 'open').length;
    const inflight = list.reduce((s, k) => s + (k.in_flight || 0), 0);
    const quota = list.some((k) => k.quota_limit > 0); // UI-35：声明了额度的池多一列
    const closed = isCollapsed(pool, list.length);
    const bodyId = 'pool-body-' + pool.replace(/[^A-Za-z0-9_-]/g, '_');
    const toggle = h('button', { type: 'button', class: 'pool-toggle', 'aria-expanded': String(!closed), 'aria-controls': bodyId,
      title: t(closed ? 'k.expand' : 'k.collapse'), 'aria-label': pool + ' · ' + t(closed ? 'k.expand' : 'k.collapse'),
      onClick: () => { collapsed.set(pool, !closed); drawList(lastKeys); } },
      h('span', { class: 'chev' + (closed ? '' : ' chev-open'), 'aria-hidden': 'true', text: '▸' }),
      h('h2', { text: pool }));
    return h('section', { class: 'card' + (closed ? ' pool-closed' : '') },
      h('div', { class: 'card-head card-head-row' },
        h('div', { class: 'row-wrap' }, toggle, h('span', { class: 'tag mono', text: protoOf.get(pool) || '?' })),
        h('span', { class: 'muted small', text: t('k.poolSummary', { n: list.length, ready, inflight }) })),
      poolLine(extra && extra.pools.get(pool)),
      closed ? null : tableWrap(h('table', { class: 'table table-wide', id: bodyId },
        h('thead', {}, h('tr', {}, th(t('k.name')), th(t('k.secret')), th(t('k.state')), quota ? th(t('k.quota')) : null, extra ? th(t('k.day')) : null, th(t('k.expires')), th(t('k.breaker')), th(t('k.load')), th(t('k.weight'), 'num'), th(t('k.actions'), 'num'))),
        h('tbody', {}, list.map((k) => keyRow(k, quota))))),
      closed ? h('button', { type: 'button', class: 'link-btn small pool-more', text: t('k.showKeys', { n: fmtInt(list.length) }),
        onClick: () => { collapsed.set(pool, false); drawList(lastKeys); } }) : null);
  }

  // poolLine 是池摘要（UI-37）：可用 / 总数；额度：剩余、每小时消耗、预计用完或能撑到重置；7 天内过期
  function poolLine(p) {
    if (!p) return null;
    const parts = [t('k.plAvailable', { a: fmtInt(p.keys.available), n: fmtInt(p.keys.total) })];
    let tone = p.keys.available === 0 ? 'bad' : '';
    if (p.quota) {
      const q = p.quota;
      parts.push(t('k.plLeft', { left: fmtInt(q.left), unit: q.unit }));
      if (q.burn_per_hour > 0) parts.push(t('k.plBurn', { n: fmtNum(q.burn_per_hour), unit: q.unit }));
      if (q.runs_out_at) {
        parts.push(t('k.plRunsOut', { time: fmtTime(q.runs_out_at) }));
        tone = tone || 'warn';
      } else if (q.lasts_until_reset) {
        parts.push(t('k.plLasts', { time: fmtTime(q.next_reset) }));
      } else {
        parts.push(t('k.plNoBurn'));
      }
    }
    if (p.expiring_7d > 0) parts.push(t('k.plExpiring', { n: fmtInt(p.expiring_7d) }));
    return h('div', { class: 'pool-line small' + (tone ? ' tone-' + tone : ' muted') }, parts.join(' · '));
  }

  // dayCell 是 24 小时统计（UI-37）：请求数 · 失败率；title 列出各类失败与最近一次失败
  function dayCell(k) {
    const s = extra && extra.stats.get(k.id);
    if (!s) return h('td', { class: 'small muted', text: t('common.dash') });
    const rate = s.failure_rate_24h || 0;
    const tone = rate >= 0.5 ? ' tone-bad' : rate >= 0.2 ? ' tone-warn' : '';
    const kinds = Object.entries(s.failures_24h || {}).map(([kind, n]) => kind + ' ' + n);
    const title = [t('k.dayAttempts', { n: fmtInt(s.attempts_24h) })].concat(kinds)
      .concat(s.last_failure ? [t('k.dayLast', { kind: s.last_failure.kind, time: fmtTime(s.last_failure.at) })] : []).join('\n');
    return h('td', { class: 'small mono nowrap' + tone, title,
      text: fmtInt(s.requests) + ' · ' + (s.attempts_24h > 0 ? (rate * 100).toFixed(rate >= 0.1 ? 0 : 1) + '%' : t('common.dash')) });
  }

  // quotaCell 显示本期用量：已用 / 上限与进度条（UI-35）
  function quotaCell(k) {
    if (!(k.quota_limit > 0)) return h('td', { class: 'small muted', text: t('common.dash') });
    const used = k.quota_used || 0;
    const frac = used / k.quota_limit;
    return h('td', { title: k.quota_reset_at ? t('k.quotaResets', { time: fmtTime(k.quota_reset_at) }) : t('k.quotaNoReset') },
      h('div', { class: 'load' }, bar(frac, frac >= 1 ? 'bad' : frac >= 0.8 ? 'warn' : 'accent'),
        h('span', { class: 'mono small', text: fmtInt(used) + ' / ' + fmtInt(k.quota_limit) })));
  }

  function keyRow(k, quota) {
    let state;
    let reason = k.state_reason || '';
    if (k.state === 'orphaned') { state = pill(t('k.orphaned'), 'off'); reason = t('k.orphanedReason'); } // UI-30
    else if (k.state === 'disabled') state = pill(t('k.disabled'), 'off');
    else if (k.state === 'quarantined') state = pill(t('k.quarantined'), 'bad');
    else if (k.state === 'expired') { state = pill(t('k.expired'), 'off'); reason = t('k.expiredReason', { time: fmtTime(k.expires_at) }); } // T62
    else if (k.state === 'exhausted') { // T63
      state = pill(t('k.exhausted'), 'warn');
      reason = k.quota_reset_at ? t('k.exhaustedReason', { time: fmtTime(k.quota_reset_at) }) : t('k.exhaustedTotal');
    }
    else if (k.cooldown_seconds > 0) { state = pill(t('k.cooling'), 'warn'); reason = reason || t('k.coolingReason', { s: k.cooldown_seconds }); }
    else state = pill(t('k.ready'), 'ok');

    const br = { closed: t('k.brClosed'), open: t('k.brOpen'), half_open: t('k.brHalf') }[k.breaker] || k.breaker;
    const frac = k.max_in_flight > 0 ? (k.in_flight || 0) / k.max_in_flight : 0;
    const off = (k.stored_state || k.state) !== 'ready'; // 已过期的 Key 库中仍是 ready：提供停用
    const toggle = h('button', {
      type: 'button',
      class: off ? 'link-btn' : 'link-btn link-danger',
      text: off ? t('k.enable') : t('k.disable'),
      onClick: () => setState(k, off ? 'ready' : 'disabled'),
    });
    return h('tr', {},
      h('td', {}, h('div', { class: 'stack-xs' }, h('span', { class: 'strong', text: k.name }),
        h('span', { class: 'muted small' }, '#' + k.id + ' ', k.label ? h('span', { class: 'tag mono', text: k.label }) : null))),
      h('td', { class: 'mono small', text: k.secret_hint }),
      h('td', {}, h('div', { class: 'stack-xs' }, state, reason ? h('span', { class: 'muted small', text: reason }) : null)),
      quota ? quotaCell(k) : null,
      extra ? dayCell(k) : null,
      k.expires_at ? h('td', { class: 'small mono', title: k.expires_at, text: fmtTime(k.expires_at) }) : h('td', { class: 'small muted', text: t('k.never') }),
      h('td', { class: 'small' + (k.breaker === 'open' ? ' tone-bad' : ''), text: br }),
      h('td', {}, h('div', { class: 'load' }, bar(frac), h('span', { class: 'mono small', text: fmtInt(k.in_flight || 0) + ' / ' + fmtInt(k.max_in_flight) }))),
      h('td', { class: 'num mono', text: fmtInt(k.weight) }),
      h('td', { class: 'num actions' }, h('button', { type: 'button', class: 'link-btn', text: t('common.edit'), onClick: () => editKey(k) }),
        k.state === 'orphaned' ? null : toggle)); // UI-30：孤立的 Key 不提供启用 / 禁用
  }

  async function setState(k, next) {
    if (next === 'disabled') {
      const ok = await confirmDialog(t('k.confirmDisable', { name: k.name }), t('k.disable'));
      if (!ok) return;
    }
    try {
      await api('PATCH', '/admin/api/keys/' + k.id, { state: next });
      msg.replaceChildren();
      await load();
    } catch (e) {
      showError(e);
    }
  }

  function editKey(k) {
    const err = h('div', { 'aria-live': 'polite' });
    const name = input({ type: 'text', value: k.name });
    const weight = input({ type: 'number', min: '1', step: '1', value: String(k.weight) });
    const mif = input({ type: 'number', min: '1', step: '1', value: String(k.max_in_flight) });
    const exp = input({ type: 'datetime-local', step: '1', value: toLocalInput(k.expires_at) });
    const label = input({ type: 'text', maxlength: '64', value: k.label || '' });
    const body = h('div', { class: 'stack' }, err,
      field('ke-name', t('k.name'), name),
      h('div', { class: 'grid-2' }, field('ke-w', t('k.weight'), weight), field('ke-m', t('k.maxInFlight'), mif)),
      field('ke-exp', t('k.expires'), exp, t('k.expiresHint')),
      field('ke-label', t('k.label'), label, t('k.labelHint')));
    const save = h('button', { type: 'button', class: 'btn btn-primary', text: t('common.save') });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const { close } = openDialog(t('k.formEdit', { name: k.name }), body, [cancel, save]);
    cancel.addEventListener('click', () => close());
    save.addEventListener('click', async () => {
      err.replaceChildren();
      const w = Number.parseInt(weight.value, 10);
      const m = Number.parseInt(mif.value, 10);
      if (!name.value.trim() || !(w > 0) || !(m > 0)) {
        err.append(errorBox({ code: 'invalid_request', message: t('k.required') }));
        return;
      }
      save.disabled = true;
      try {
        await api('PATCH', '/admin/api/keys/' + k.id, { name: name.value.trim(), weight: w, max_in_flight: m,
          expires_at: fromLocalInput(exp.value), label: label.value.trim() });
        close();
        await load();
      } catch (e) {
        err.append(errorBox(e));
      } finally {
        save.disabled = false;
      }
    });
  }

  // 新增表单（UI-11：secret 用密码框，提交后清空）
  const pools = (cfg.pools || []).map((p) => ({ value: p.name, label: p.name + ' · ' + p.protocol }));
  const formErr = h('div', { 'aria-live': 'polite' });
  const fPool = select(pools, pools.length ? pools[0].value : '');
  const fName = input({ type: 'text', placeholder: 'team-c' });
  const fSecret = input({ type: 'password', autocomplete: 'new-password', spellcheck: 'false', placeholder: t('k.secretPh'), class: 'input mono' });
  const fWeight = input({ type: 'number', min: '1', step: '1', value: '1' });
  const fMif = input({ type: 'number', min: '1', step: '1', value: '8' });
  const fExp = input({ type: 'datetime-local', step: '1' });
  const fLabel = input({ type: 'text', maxlength: '64' });
  const addBtn = h('button', { type: 'submit', class: 'btn btn-primary btn-block', text: t('k.add') });
  // UI-34：批量导入（标题栏按钮与「新增 Key」卡片中的入口打开同一个对话框）
  const openImport = () => openImportDialog(cfg, () => { load().then(schedule); });
  const form = h('form', { class: 'card form-card stack', 'aria-labelledby': 'k-new-h', novalidate: true },
    h('h2', { id: 'k-new-h', text: t('k.new') }),
    pools.length ? h('p', { class: 'small muted import-hint' }, t('k.importHint') + ' ',
      h('button', { type: 'button', class: 'link-btn', text: t('k.importLink'), onClick: openImport })) : null,
    formErr,
    pools.length ? null : h('p', { class: 'muted' }, t('k.noPools') + ' ', h('a', { href: '#/config', text: t('k.createPool') })), // UI-40
    field('k-pool', t('k.pool'), fPool),
    field('k-name', t('k.name'), fName),
    field('k-secret', t('k.secret'), fSecret, t('k.secretHint')),
    h('div', { class: 'grid-2' }, field('k-w', t('k.weight'), fWeight), field('k-m', t('k.maxInFlight'), fMif)),
    field('k-exp', t('k.expires'), fExp, t('k.expiresHint')),
    field('k-label', t('k.label'), fLabel, t('k.labelHint')),
    addBtn);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    formErr.replaceChildren();
    const secret = fSecret.value;
    const w = Number.parseInt(fWeight.value, 10);
    const m = Number.parseInt(fMif.value, 10);
    if (!fPool.value || !fName.value.trim() || !secret || !(w > 0) || !(m > 0)) {
      formErr.append(errorBox({ code: 'invalid_request', message: t('k.required') }));
      return;
    }
    addBtn.disabled = true;
    fSecret.value = ''; // UI-11：无论成功失败都清空
    try {
      const res = await api('POST', '/admin/api/keys', { pool: fPool.value, name: fName.value.trim(), secret, weight: w, max_in_flight: m,
        expires_at: fromLocalInput(fExp.value) || '', label: fLabel.value.trim() });
      fName.value = '';
      formErr.append(notice(t('k.added', { name: res.key.name })));
      await load();
    } catch (e) {
      formErr.append(errorBox(e));
    } finally {
      addBtn.disabled = false;
    }
  });
  if (!pools.length) addBtn.disabled = true;

  refreshBtn.addEventListener('click', () => { load().then(schedule); });
  const importBtn = h('button', { type: 'button', class: 'btn', text: t('k.import'), disabled: !pools.length, onClick: openImport });

  main.replaceChildren(
    pageHeader(t('k.title'), '', labelSelect, importBtn, refreshBtn),
    h('div', { class: 'live-row' }, status),
    msg,
    h('div', { class: 'split' }, h('div', { class: 'split-main' }, listBox), h('div', { class: 'split-side' }, form)));

  document.addEventListener('visibilitychange', onVisibility);
  await load();
  schedule();

  return () => {
    stopped = true;
    clearTimeout(timer);
    document.removeEventListener('visibilitychange', onVisibility);
  };
}

// toLocalInput 把 RFC 3339 时间转成 datetime-local 输入框的本地时间值；没有时返回空串。
function toLocalInput(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

// fromLocalInput 把 datetime-local 的本地时间转成 RFC 3339（UTC）；空值返回 null（PATCH 时清除过期时间）。
function fromLocalInput(v) {
  if (!v) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

function th(text, cls) {
  return h('th', { scope: 'col', class: cls || null, text });
}
