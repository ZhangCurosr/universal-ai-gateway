// 配置页（UI-31、UI-32）：重载预览与应用、重载记录、服务与工具。

import { h } from '../dom.js';
import { t, fmtTime } from '../i18n.js';
import { api } from '../api.js';
import { errorBox, notice, pill, card, tableWrap, pageHeader, loading, confirmDialog } from '../ui.js';
import { entriesCard } from './config_editor.js';

const POLL_MS = 1000;
const POLL_LIMIT_MS = 2 * 60 * 1000;

export async function render(main, ctx) {
  const head = () => pageHeader(t('cfg.title'), t('cfg.sub'),
    h('button', { type: 'button', class: 'btn', text: t('common.refresh'), onClick: () => render(main, ctx) }));
  main.replaceChildren(head(), loading());
  let cfg, tools, records;
  try {
    ctx.invalidateConfig();
    [cfg, tools, records] = await Promise.all([ctx.config(), api('GET', '/admin/api/tools'), api('GET', '/admin/api/reload')]);
  } catch (e) {
    main.replaceChildren(head(), errorBox(e));
    return;
  }

  const msg = h('div', {});
  const result = h('div', { class: 'stack' });
  let busy = false;
  const previewBtn = h('button', { type: 'button', class: 'btn', text: t('cfg.preview'), onClick: () => doReload(true) });
  const applyBtn = h('button', { type: 'button', class: 'btn btn-primary', text: t('cfg.apply'), onClick: () => doReload(false) });

  async function doReload(dryRun) {
    if (busy) return;
    if (!dryRun && !(await confirmDialog(t('cfg.confirmApply'), t('cfg.apply'), false))) return;
    busy = true;
    previewBtn.disabled = applyBtn.disabled = true;
    msg.replaceChildren();
    result.replaceChildren(loading());
    try {
      const resp = await api('POST', '/admin/api/reload', { dry_run: dryRun });
      result.replaceChildren(changesView(resp, dryRun));
      if (!dryRun && resp.mode === 'restart' && resp.record_id) {
        result.append(notice(t('cfg.restarting'), 'warn'));
        const rec = await waitRestart(resp.record_id);
        result.append(rec && rec.status === 'applied' ? notice(t('cfg.restartDone'), 'ok')
          : notice(t('cfg.restartFailed', { status: rec ? rec.status : 'timeout', error: rec && rec.error ? rec.error : '' }), 'bad'));
      }
      if (!dryRun && resp.mode !== 'none') {
        ctx.invalidateConfig();
        await refreshLists();
      }
    } catch (e) {
      result.replaceChildren();
      msg.replaceChildren(errorBox(e));
    } finally {
      busy = false;
      previewBtn.disabled = applyBtn.disabled = false;
    }
  }

  // 进程内重启期间管理面暂时不可用：请求失败视为尚未就绪，继续等待（UI-31）
  async function waitRestart(id) {
    const deadline = Date.now() + POLL_LIMIT_MS;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, POLL_MS));
      try {
        const { records: recs } = await api('GET', '/admin/api/reload');
        const rec = recs.find((r) => r.id === id);
        if (rec && rec.status !== 'restarting') return rec;
      } catch (_) { /* 重启中 */ }
    }
    return null;
  }

  const recordsBox = h('div', {});
  const servicesBox = h('div', {});
  const toolsBox = h('div', {});
  async function refreshLists() {
    try {
      [cfg, tools, records] = await Promise.all([ctx.config(), api('GET', '/admin/api/tools'), api('GET', '/admin/api/reload')]);
    } catch (e) {
      msg.replaceChildren(errorBox(e));
      return;
    }
    fill();
  }
  function fill() {
    recordsBox.replaceChildren(recordsTable(records.records || []));
    servicesBox.replaceChildren(servicesTable(cfg, tools.tools || []));
    toolsBox.replaceChildren(toolsTable(tools));
  }
  fill();

  main.replaceChildren(head(), msg,
    card(t('cfg.reload'), h('div', { class: 'stack pad' }, h('p', { class: 'muted small', text: t('cfg.reloadHint') }),
      h('div', { class: 'row-wrap' }, previewBtn, applyBtn), result)),
    card(t('cfg.records'), recordsBox),
    entriesCard(ctx, () => refreshLists()), // UI-40：池与服务（T72）
    card(t('cfg.services'), servicesBox),
    card(h('div', { class: 'row-wrap' }, h('h2', { text: t('cfg.tools') }), pill(t('cfg.mode', { mode: tools.tool_mode }), 'off')), toolsBox));
}

function names(list) {
  return list && list.length ? list.join(', ') : '';
}

function changesView(resp, dryRun) {
  const c = resp.changes || {};
  const rows = [
    [t('cfg.poolsAdded'), names(c.pools && c.pools.added)],
    [t('cfg.poolsRemoved'), names(c.pools && c.pools.removed)],
    [t('cfg.poolsChanged'), names(c.pools && c.pools.changed)],
    [t('cfg.servicesAdded'), names(c.services && c.services.added)],
    [t('cfg.servicesRemoved'), names(c.services && c.services.removed)],
    [t('cfg.servicesChanged'), names(c.services && c.services.changed)],
    [t('cfg.settings'), names(c.settings)],
    [t('cfg.restartFields'), names(c.restart_fields)],
  ].filter(([, v]) => v);
  const modeText = t('cfg.mode_' + resp.mode) + (dryRun ? ' · ' + t('cfg.dryRun') : resp.applied ? ' · ' + t('cfg.applied') : '');
  return h('div', { class: 'stack' },
    h('div', {}, pill(modeText, resp.mode === 'restart' ? 'warn' : resp.mode === 'hot' ? 'ok' : 'off')),
    rows.length ? tableWrap(h('table', { class: 'table' }, h('tbody', {}, rows.map(([k, v]) =>
      h('tr', {}, h('th', { scope: 'row', text: k }), h('td', { class: 'mono small', text: v })))))) : h('p', { class: 'muted', text: t('cfg.noChanges') }),
    (resp.warnings || []).map((w) => notice(w, 'warn')));
}

function recordsTable(recs) {
  if (!recs.length) return h('p', { class: 'muted pad', text: t('cfg.noRecords') });
  const tone = { applied: 'ok', noop: 'off', restarting: 'warn', failed: 'bad', rolled_back: 'bad' };
  return tableWrap(h('table', { class: 'table' },
    h('thead', {}, h('tr', {}, [t('cfg.time'), t('cfg.modeCol'), t('cfg.status'), t('cfg.actor'), t('cfg.error')].map((x) => h('th', { scope: 'col', text: x })))),
    h('tbody', {}, recs.map((r) => h('tr', {},
      h('td', { class: 'small', text: fmtTime(r.time), title: r.time }),
      h('td', { class: 'small', text: r.mode ? t('cfg.mode_' + r.mode) : '' }),
      h('td', {}, pill(t('cfg.st_' + r.status), tone[r.status] || 'off')),
      h('td', { class: 'mono small', text: r.actor_client_id ? '#' + r.actor_client_id : '' }),
      h('td', { class: 'small muted', text: r.error || '' }))))));
}

function servicesTable(cfg, tools) {
  const protoOf = new Map((cfg.pools || []).map((p) => [p.name, p.protocol]));
  const toolsOf = new Map();
  for (const tl of tools) {
    if (!tl.service) continue;
    if (!toolsOf.has(tl.service)) toolsOf.set(tl.service, []);
    toolsOf.get(tl.service).push(tl.name);
  }
  const models = (cfg.models || []).slice().sort((a, b) => a.name.localeCompare(b.name));
  if (!models.length) return h('p', { class: 'muted pad', text: t('common.none') });
  return tableWrap(h('table', { class: 'table' },
    h('thead', {}, h('tr', {}, [t('cfg.service'), t('cfg.pool'), t('cfg.protocol'), t('cfg.serviceTools')].map((x) => h('th', { scope: 'col', text: x })))),
    h('tbody', {}, models.map((m) => h('tr', {},
      h('td', { class: 'strong', text: m.name }),
      h('td', { class: 'mono small', text: m.pool }),
      h('td', { class: 'small', text: protoOf.get(m.pool) || '' }),
      h('td', { class: 'mono small', text: names(toolsOf.get(m.name)) || t('common.dash') }))))));
}

function toolsTable(view) {
  const list = view.tools || [];
  if (!list.length) return h('p', { class: 'muted pad', text: t('common.none') });
  return tableWrap(h('table', { class: 'table' },
    h('thead', {}, h('tr', {}, [t('cfg.tool'), t('cfg.service'), t('cfg.access'), t('cfg.summary')].map((x) => h('th', { scope: 'col', text: x })))),
    h('tbody', {}, list.map((tl) => h('tr', {},
      h('td', { class: 'mono small strong', text: tl.name }),
      h('td', { class: 'mono small', text: tl.service || t('cfg.anyService') }),
      h('td', {}, pill(tl.read_only ? t('cfg.readOnly') : t('cfg.write'), tl.read_only ? 'ok' : 'warn')),
      h('td', { class: 'small', text: tl.summary || '' }))))));
}
