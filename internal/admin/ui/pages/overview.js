// 概览页：池级告警（T64）、健康状态、24 小时用量、各池 Key 状态、需要处理的事项。

import { h } from '../dom.js';
import { t, fmtInt, fmtDuration, fmtTime } from '../i18n.js';
import { api } from '../api.js';
import { formatNano } from '../money.js';
import { errorBox, pill, card, tableWrap, pageHeader, loading, money } from '../ui.js';

// keyKind 把 KeyView 归为 ready / cooling / bad / off（状态条与汇总共用）。
export function keyKind(k) {
  if (k.state === 'disabled' || k.state === 'orphaned' || k.state === 'expired') return 'off'; // UI-30、T62
  if (k.state === 'quarantined' || k.breaker === 'open') return 'bad';
  if (k.state === 'exhausted' || k.cooldown_seconds > 0 || k.breaker === 'half_open') return 'cooling'; // T63：到重置时刻自动恢复
  return 'ready';
}

export async function render(main, ctx) {
  main.replaceChildren(pageHeader(t('ov.title'), ''), loading());
  const now = new Date();
  const from = new Date(now.getTime() - 24 * 3600 * 1000);
  let health, keyList, items, clientList, cfg, poolAlerts;
  try {
    [health, { keys: keyList }, { items }, { clients: clientList }, cfg, { alerts: poolAlerts }] = await Promise.all([
      ctx.health(),
      api('GET', '/admin/api/keys'),
      api('GET', '/admin/api/usage?from=' + encodeURIComponent(from.toISOString().replace(/\.\d+Z$/, 'Z')) + '&to=' + encodeURIComponent(now.toISOString().replace(/\.\d+Z$/, 'Z'))),
      api('GET', '/admin/api/clients'),
      ctx.config(),
      api('GET', '/admin/api/alerts'), // UI-36：池级告警（T64）
    ]);
  } catch (e) {
    main.replaceChildren(pageHeader(t('ov.title'), ''), errorBox(e));
    return;
  }

  const refresh = h('button', { type: 'button', class: 'btn', text: t('common.refresh'), onClick: () => render(main, ctx) });
  const head = pageHeader(t('ov.title'), t('ov.sub', { uptime: fmtDuration(health.uptime_seconds) }), pill(t('ov.healthy'), 'ok'), refresh);

  // 关键指标
  const totalReq = items.reduce((s, it) => s + (it.requests || 0), 0);
  const totalCost = items.reduce((s, it) => s + (it.cost_nano_usd || 0), 0);
  const activeClients = new Set(items.filter((it) => it.requests > 0).map((it) => it.client_id)).size;
  const kinds = { ready: 0, cooling: 0, bad: 0, off: 0 };
  for (const k of keyList) kinds[keyKind(k)]++;
  const quarantined = keyList.filter((k) => k.state === 'quarantined').length;

  const stats = [
    stat(t('ov.requests'), fmtInt(totalReq), t('ov.requestsNote', { n: activeClients })),
    stat(t('ov.cost'), formatNano(totalCost), ''),
    stat(t('ov.keysReady'), kinds.ready + ' / ' + keyList.length,
      t('ov.keysNote', { c: kinds.cooling, q: quarantined, d: kinds.off }), kinds.ready < keyList.length ? 'warn' : ''),
  ];
  if (health.ledger_queue) {
    const { length, capacity } = health.ledger_queue;
    const high = capacity > 0 && length / capacity > 0.5;
    stats.push(stat(t('ov.ledger'), fmtInt(length) + ' / ' + fmtInt(capacity), t(high ? 'ov.ledgerHigh' : 'ov.ledgerOk'), high ? 'warn' : 'ok'));
  }

  // 各池
  const protoOf = new Map((cfg.pools || []).map((p) => [p.name, p.protocol]));
  const pools = new Map();
  for (const k of keyList) {
    if (!pools.has(k.pool)) pools.set(k.pool, { ready: 0, cooling: 0, bad: 0, off: 0, inflight: 0, total: 0 });
    const p = pools.get(k.pool);
    p[keyKind(k)]++;
    p.inflight += k.in_flight || 0;
    p.total++;
  }
  const poolRows = [...pools.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([name, p]) =>
    h('tr', {},
      h('td', { class: 'strong', text: name }),
      h('td', {}, h('span', { class: 'tag mono', text: protoOf.get(name) || t('ov.unknownProtocol') })),
      h('td', {}, h('div', { class: 'stack-sm' }, segBar(p), h('span', { class: 'muted small', text: t('ov.poolSummary', p) }))),
      h('td', { class: 'num mono', text: fmtInt(p.inflight) })));
  const poolsCard = card(h('div', { class: 'card-head-row' }, h('h2', { text: t('ov.pools') }), h('a', { href: '#/keys', class: 'small', text: t('ov.allKeys') })),
    tableWrap(h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, th(t('ov.pool')), th(t('ov.protocol')), th(t('ov.keyState')), th(t('ov.inflight'), 'num'))),
      h('tbody', {}, poolRows))));

  // 需要处理
  const alerts = [];
  for (const k of keyList) {
    const label = k.pool + ' / ' + k.name;
    if (k.state === 'quarantined') alerts.push(alertItem(t('ov.tagQuarantined'), 'bad', label, k.state_reason || ''));
    else if (k.breaker === 'open') alerts.push(alertItem(t('ov.tagBreaker'), 'bad', label, t('ov.breakerDetail', { state: t('k.brOpen') })));
    else if (k.state !== 'disabled' && k.cooldown_seconds > 0) alerts.push(alertItem(t('ov.tagCooling'), 'warn', label, t('ov.coolingDetail', { s: k.cooldown_seconds })));
  }
  if (health.audit_dropped > 0) alerts.push(alertItem(t('ov.tagAudit'), 'off', t('ov.tagAudit'), t('ov.auditDetail', { n: fmtInt(health.audit_dropped) })));
  const attnCard = card(t('ov.attention'), alerts.length
    ? h('ul', { class: 'alert-list' }, alerts)
    : h('p', { class: 'muted pad', text: t('ov.nothing') }));

  // 花费最多的客户端
  const nameOf = new Map(clientList.map((c) => [c.id, c.name]));
  const byClient = new Map();
  for (const it of items) byClient.set(it.client_id, (byClient.get(it.client_id) || 0) + (it.cost_nano_usd || 0));
  const top = [...byClient.entries()].filter(([, v]) => v > 0).sort((a, b) => b[1] - a[1]).slice(0, 5);
  const max = top.length ? top[0][1] : 0;
  const topCard = card(h('div', { class: 'card-head-row' }, h('h2', { text: t('ov.top') }), h('a', { href: '#/usage', class: 'small', text: t('ov.usageLink') })),
    top.length
      ? h('div', { class: 'top-list' }, top.map(([id, cost]) =>
        h('div', { class: 'top-row' },
          h('span', { class: 'strong ellipsis', text: nameOf.get(id) || '#' + id }),
          h('div', { class: 'bar' }, h('div', { class: 'bar-fill bar-accent', css: { width: (max ? (cost / max) * 100 : 0).toFixed(1) + '%' } })),
          h('span', { class: 'num' }, money(cost)))))
      : h('p', { class: 'muted pad', text: t('ov.noUsage') }));

  // UI-36：池级告警放在最上面；没有告警时不显示
  const poolAlertCard = poolAlerts.length
    ? card(t('ov.poolAlerts'), h('ul', { class: 'alert-list' }, poolAlerts.map((a) => alertItem(
      t(a.level === 'critical' ? 'ov.alertCritical' : 'ov.alertWarning'), a.level === 'critical' ? 'bad' : 'warn',
      a.pool + ' · ' + t('ov.alert.' + a.kind),
      t('ov.alertDetail.' + a.kind, { value: fmtInt(a.value), threshold: fmtInt(a.threshold) }) + ' · ' + t('ov.alertSince', { time: fmtTime(a.since) })))))
    : null;

  main.replaceChildren(head,
    ...(poolAlertCard ? [poolAlertCard] : []), // replaceChildren 会把 null 当成文本
    h('section', { class: 'stats', 'aria-label': t('ov.title') }, stats),
    h('div', { class: 'split' }, h('div', { class: 'split-main' }, poolsCard), h('div', { class: 'split-side' }, attnCard)),
    topCard);
}

function stat(label, value, note, tone) {
  return h('div', { class: 'stat' },
    h('span', { class: 'muted small', text: label }),
    h('span', { class: 'stat-value mono', text: value }),
    note ? h('span', { class: 'small tone-' + (tone || 'muted'), text: note }) : null);
}

function th(text, cls) {
  return h('th', { scope: 'col', class: cls || null, text });
}

function segBar(p) {
  const seg = (n, kind) => (n > 0 ? h('span', { class: 'seg seg-' + kind, css: { flexGrow: String(n) } }) : null);
  return h('div', { class: 'segbar', role: 'img', 'aria-label': t('ov.poolSummary', p) },
    seg(p.ready, 'ok'), seg(p.cooling, 'warn'), seg(p.bad, 'bad'), seg(p.off, 'off'));
}

function alertItem(tag, kind, title, detail) {
  return h('li', { class: 'alert-item' },
    h('span', { class: 'tag tag-' + kind, text: tag }),
    h('div', { class: 'stack-xs' }, h('span', { class: 'strong', text: title }), detail ? h('span', { class: 'muted small', text: detail }) : null));
}
