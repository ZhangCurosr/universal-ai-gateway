// 批量导入上游 Key（T70，UI-34）：在浏览器里解析粘贴的文本或本地文件，只把密钥、过期时间与标签
// 提交给 POST /admin/api/keys/import。页面上不显示任何密钥；对话框关闭后清空文本与条目。

import { h } from '../dom.js';
import { t, fmtInt } from '../i18n.js';
import { api } from '../api.js';
import { errorBox, notice, field, input, select, openDialog, bar } from '../ui.js';

const MAX_FILE_BYTES = 8 << 20;
const CHUNK_ITEMS = 500; // 服务端上限 1000（ADM-23）
const CHUNK_BYTES = 900 << 10; // 请求体上限 1 MiB
const IMPORT_TIMEOUT_MS = 120000;
const MAX_ISSUES = 50;
const NONE = '';

// openImportDialog 打开批量导入对话框；导入过至少一批后调用 onDone。
export function openImportDialog(cfg, onDone) {
  const pools = (cfg.pools || []).map((p) => ({ value: p.name, label: p.name + ' · ' + p.protocol }));
  let fileText = null; // 选择了文件时的内容；为 null 时用文本框
  let parsed = null;
  let imported = false;

  const err = h('div', { 'aria-live': 'polite' });
  const fPool = select(pools, pools.length ? pools[0].value : '');
  const fPrefix = input({ type: 'text', maxlength: '56', placeholder: fPool.value });
  const fWeight = input({ type: 'number', min: '1', step: '1', value: '1' });
  const fMif = input({ type: 'number', min: '1', step: '1', value: '8' });
  const fText = h('textarea', { class: 'input mono textarea', rows: '6', spellcheck: 'false', autocomplete: 'off', placeholder: t('ki.textPh') });
  const fFile = input({ type: 'file', accept: '.txt,.jsonl,.json,.csv,.tsv,text/plain,text/csv,application/json' });
  const fileInfo = h('span', { class: 'muted small' });
  const clearFile = h('button', { type: 'button', class: 'link-btn', text: t('ki.clearFile'), hidden: true });
  const fFormat = select([
    { value: 'auto', label: t('ki.fmtAuto') }, { value: 'lines', label: t('ki.fmtLines') },
    { value: 'jsonl', label: t('ki.fmtJsonl') }, { value: 'csv', label: t('ki.fmtCsv') }], 'auto');
  const fSecretField = select([], '');
  const fExpField = select([], NONE);
  const fLabelField = select([], NONE);
  const fWhereField = select([], NONE);
  const fWhereValue = input({ type: 'text', placeholder: 'ok' });
  const fTz = input({ type: 'text', placeholder: t('ki.tzPh') });
  const fLabel = input({ type: 'text', maxlength: '64' });
  const fSecretPrefix = input({ type: 'text', class: 'input mono', spellcheck: 'false', placeholder: 'bearer:' });
  const fInclExpired = h('input', { type: 'checkbox' });
  const fUpdate = h('input', { type: 'checkbox' });
  const mapping = h('div', { class: 'stack', hidden: true },
    h('div', { class: 'grid-2' }, field('ki-sf', t('ki.secretField'), fSecretField), field('ki-ef', t('ki.expField'), fExpField)),
    h('div', { class: 'grid-2' }, field('ki-lf', t('ki.labelField'), fLabelField),
      h('div', { class: 'field' }, h('label', { for: 'ki-wf', text: t('ki.where') }),
        h('div', { class: 'row-wrap' }, fWhereField, h('span', { text: '=' }), fWhereValue))));
  fWhereField.id = 'ki-wf';
  const preview = h('div', { class: 'stack-xs', 'aria-live': 'polite' });
  const progress = h('div', { class: 'stack-xs' });
  const result = h('div', { class: 'stack-xs', 'aria-live': 'polite' });

  const body = h('div', { class: 'stack' }, err,
    h('div', { class: 'grid-2' }, field('ki-pool', t('k.pool'), fPool), field('ki-prefix', t('ki.prefix'), fPrefix, t('ki.prefixHint'))),
    h('div', { class: 'grid-2' }, field('ki-w', t('k.weight'), fWeight), field('ki-m', t('k.maxInFlight'), fMif)),
    field('ki-text', t('ki.text'), fText, t('ki.textHint')),
    h('div', { class: 'row-wrap' }, fFile, fileInfo, clearFile),
    h('div', { class: 'grid-2' }, field('ki-fmt', t('ki.format'), fFormat), field('ki-sp', t('ki.secretPrefix'), fSecretPrefix, t('ki.secretPrefixHint'))),
    mapping,
    h('div', { class: 'grid-2' }, field('ki-tz', t('ki.tz'), fTz, t('ki.tzHint')), field('ki-label', t('ki.label'), fLabel, t('ki.labelHint'))),
    h('label', { class: 'check' }, fInclExpired, t('ki.includeExpired')),
    h('label', { class: 'check' }, fUpdate, t('ki.updateExisting')),
    preview, progress, result);

  const go = h('button', { type: 'button', class: 'btn btn-primary', text: t('ki.import'), disabled: true });
  const cancel = h('button', { type: 'button', class: 'btn', text: t('common.close') });
  const { close } = openDialog(t('ki.title'), body, [cancel, go], {
    wide: true,
    onClose: () => { // UI-34：关闭后清空文本与内存中的条目
      fText.value = '';
      fileText = null;
      parsed = null;
      fFile.value = '';
      if (imported && onDone) onDone();
    },
  });
  cancel.addEventListener('click', () => close());

  fPool.addEventListener('change', () => { fPrefix.placeholder = fPool.value; });
  let timer = null;
  const refresh = () => { clearTimeout(timer); timer = setTimeout(update, 150); };
  for (const el of [fText, fWhereValue, fTz, fLabel, fSecretPrefix]) el.addEventListener('input', refresh);
  for (const el of [fSecretField, fExpField, fLabelField, fWhereField, fInclExpired]) el.addEventListener('change', update);
  fFormat.addEventListener('change', () => { lastFields = null; update(); });

  fFile.addEventListener('change', async () => {
    err.replaceChildren();
    const f = fFile.files && fFile.files[0];
    if (!f) return;
    if (f.size > MAX_FILE_BYTES) {
      fFile.value = '';
      err.append(errorBox({ code: 'invalid_request', message: t('ki.tooLarge', { mb: MAX_FILE_BYTES >> 20 }) }));
      return;
    }
    try {
      fileText = await f.text();
    } catch (e) {
      fileText = null;
      err.append(errorBox({ code: 'read_error', message: t('ki.readError') }));
      return;
    }
    fText.value = '';
    fText.disabled = true;
    fileInfo.textContent = t('ki.fileInfo', { name: f.name, kb: fmtInt(Math.ceil(f.size / 1024)) });
    clearFile.hidden = false;
    lastFields = null;
    update();
  });
  clearFile.addEventListener('click', () => {
    fileText = null;
    fFile.value = '';
    fText.disabled = false;
    fileInfo.textContent = '';
    clearFile.hidden = true;
    lastFields = null;
    update();
  });

  let lastFields = null;
  function fillFields(fields) {
    const key = fields.join('\n');
    if (key === lastFields) return;
    lastFields = key;
    const opts = fields.map((f) => ({ value: f, label: f }));
    const none = [{ value: NONE, label: t('ki.none') }].concat(opts);
    const pick = (cands, fallback) => cands.find((c) => fields.includes(c)) ?? fallback;
    setOptions(fSecretField, opts, pick(['api_key', 'apikey', 'key', 'token', 'secret'], fields[0] || ''));
    setOptions(fExpField, none, pick(['expires_at', 'expire_at', 'expires', 'expiry', 'expired_at'], NONE));
    setOptions(fLabelField, none, NONE);
    setOptions(fWhereField, none, NONE);
  }

  function update() {
    result.replaceChildren();
    const text = fileText !== null ? fileText : fText.value;
    if (!text.trim()) {
      parsed = null;
      mapping.hidden = true;
      preview.replaceChildren();
      go.disabled = true;
      return;
    }
    const tz = parseTZ(fTz.value.trim());
    if (tz === undefined) {
      parsed = null;
      preview.replaceChildren(errorBox({ code: 'invalid_request', message: t('ki.badTz') }));
      go.disabled = true;
      return;
    }
    const format = fFormat.value === 'auto' ? detectFormat(text) : fFormat.value;
    const table = format === 'lines' ? null : tabulate(text, format);
    mapping.hidden = format === 'lines';
    if (table) fillFields(table.fields);
    parsed = buildItems(format, text, table, {
      secretField: fSecretField.value, expField: fExpField.value, labelField: fLabelField.value,
      whereField: fWhereField.value, whereValue: fWhereValue.value, tz, label: fLabel.value.trim(),
      includeExpired: fInclExpired.checked, now: Date.now(), secretPrefix: fSecretPrefix.value.trim(),
    });
    const s = parsed.stats;
    const parts = [h('span', { class: 'strong', text: t('ki.preview', { fmt: t('ki.fmt.' + format), ok: fmtInt(parsed.items.length), expired: fmtInt(s.expired), invalid: fmtInt(s.invalid), filtered: fmtInt(s.filtered) }) })];
    if (parsed.issues.length) parts.push(issueList(parsed.issues));
    preview.replaceChildren(...parts);
    go.disabled = parsed.items.length === 0 || !fPool.value;
  }

  go.addEventListener('click', async () => {
    err.replaceChildren();
    result.replaceChildren();
    const w = Number.parseInt(fWeight.value, 10);
    const m = Number.parseInt(fMif.value, 10);
    if (!parsed || !parsed.items.length || !fPool.value || !(w > 0) || !(m > 0)) {
      err.append(errorBox({ code: 'invalid_request', message: t('ki.required') }));
      return;
    }
    const items = parsed.items;
    const skipped = parsed.stats;
    const chunks = chunk(items);
    const total = { created: 0, updated: 0, duplicate: 0, invalid: 0, failed: 0 };
    const issues = [];
    go.disabled = true;
    cancel.disabled = true;
    let done = 0;
    const showProgress = () => progress.replaceChildren(bar(done / items.length),
      h('span', { class: 'muted small', text: t('ki.progress', { done: fmtInt(done), total: fmtInt(items.length) }) }));
    showProgress();
    try {
      for (const c of chunks) {
        const res = await api('POST', '/admin/api/keys/import', {
          pool: fPool.value, name_prefix: fPrefix.value.trim(), weight: w, max_in_flight: m,
          update_existing: fUpdate.checked, items: c.items.map((it) => it.body),
        }, { timeout: IMPORT_TIMEOUT_MS });
        imported = true;
        for (const k of Object.keys(total)) total[k] += res[k] || 0;
        for (const r of res.results || []) {
          if ((r.status === 'invalid' || r.status === 'failed') && issues.length < MAX_ISSUES) {
            issues.push({ line: c.items[r.index] ? c.items[r.index].line : 0, why: t('ki.st.' + r.status) + (r.error ? ' · ' + r.error : '') });
          }
        }
        done += c.items.length;
        showProgress();
      }
      // 导入完成：清空文本与条目（UI-34）
      fText.value = '';
      fileText = null;
      fFile.value = '';
      fText.disabled = false;
      fileInfo.textContent = '';
      clearFile.hidden = true;
      parsed = null;
      preview.replaceChildren();
    } catch (e) {
      err.append(errorBox(e));
      if (done) result.append(notice(t('ki.partial', { done: fmtInt(done), total: fmtInt(items.length) }), 'warn'));
    } finally {
      cancel.disabled = false;
      go.disabled = parsed === null || !parsed.items.length;
    }
    result.prepend(notice(t('ki.result', { created: fmtInt(total.created), updated: fmtInt(total.updated), duplicate: fmtInt(total.duplicate), invalid: fmtInt(total.invalid), failed: fmtInt(total.failed) }),
      total.failed || total.invalid ? 'warn' : 'ok'));
    if (skipped.expired || skipped.invalid || skipped.filtered) {
      result.append(h('span', { class: 'muted small', text: t('ki.skipped', { expired: fmtInt(skipped.expired), invalid: fmtInt(skipped.invalid), filtered: fmtInt(skipped.filtered) }) }));
    }
    if (issues.length) result.append(issueList(issues));
  });
}

// setOptions 替换下拉框的选项并选中 value（只在字段集合变化时调用，所以总是用默认值）。
function setOptions(sel, opts, value) {
  sel.replaceChildren(...opts.map((o) => h('option', { value: o.value, text: o.label, selected: o.value === value })));
  sel.value = value;
}

function issueList(issues) {
  return h('ul', { class: 'issues small' }, issues.slice(0, MAX_ISSUES).map((i) => h('li', { text: t('ki.line', { n: i.line }) + ' · ' + i.why })));
}

// chunk 按条数与估算的请求体大小分批。
function chunk(items) {
  const out = [];
  let cur = { items: [], bytes: 0 };
  for (const it of items) {
    const n = JSON.stringify(it.body).length + 1;
    if (cur.items.length && (cur.items.length >= CHUNK_ITEMS || cur.bytes + n > CHUNK_BYTES)) {
      out.push(cur);
      cur = { items: [], bytes: 0 };
    }
    cur.items.push(it);
    cur.bytes += n;
  }
  if (cur.items.length) out.push(cur);
  return out;
}

// ---- 解析（纯函数，不接触 DOM） ----

export function detectFormat(text) {
  const first = firstLine(text);
  if (first.startsWith('{')) return 'jsonl';
  if (first.includes(',') || first.includes('\t')) return 'csv';
  return 'lines';
}

function firstLine(text) {
  for (const l of stripBOM(text).split(/\r?\n/)) {
    const s = l.trim();
    if (s && !s.startsWith('#')) return s;
  }
  return '';
}

function stripBOM(s) {
  return s.charCodeAt(0) === 0xfeff ? s.slice(1) : s;
}

// tabulate 把 JSONL / CSV 转成 { fields, rows: [{ line, obj | error }] }。
export function tabulate(text, format) {
  text = stripBOM(text);
  if (format === 'jsonl') {
    const rows = [];
    const fields = new Set();
    text.split(/\r?\n/).forEach((l, i) => {
      const s = l.trim();
      if (!s) return;
      let obj;
      try { obj = JSON.parse(s); } catch (_) { obj = null; }
      if (!obj || typeof obj !== 'object' || Array.isArray(obj)) {
        rows.push({ line: i + 1, error: 'notObject' });
        return;
      }
      if (rows.length < 50) for (const k of Object.keys(obj)) fields.add(k);
      rows.push({ line: i + 1, obj });
    });
    return { fields: [...fields], rows };
  }
  const recs = parseCSV(text, firstLine(text).includes('\t') && !firstLine(text).includes(',') ? '\t' : ',');
  const header = recs.length ? recs[0].cells.map((c) => c.trim()) : [];
  const rows = [];
  for (const r of recs.slice(1)) {
    if (r.cells.length === 1 && r.cells[0].trim() === '') continue;
    const obj = {};
    header.forEach((name, j) => { if (name) obj[name] = r.cells[j] === undefined ? '' : r.cells[j]; });
    rows.push({ line: r.line, obj });
  }
  return { fields: header.filter(Boolean), rows };
}

// parseCSV 实现 RFC 4180：引号内可以有分隔符、换行与两个连续引号。返回 [{ line, cells }]。
export function parseCSV(text, sep) {
  const out = [];
  let cells = [];
  let cell = '';
  let quoted = false;
  let line = 1;
  let start = 1;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quoted) {
      if (c === '"') {
        if (text[i + 1] === '"') { cell += '"'; i++; } else quoted = false;
      } else {
        if (c === '\n') line++;
        cell += c;
      }
    } else if (c === '"' && cell === '') {
      quoted = true;
    } else if (c === sep) {
      cells.push(cell);
      cell = '';
    } else if (c === '\n' || c === '\r') {
      if (c === '\r' && text[i + 1] === '\n') i++;
      cells.push(cell);
      out.push({ line: start, cells });
      cells = [];
      cell = '';
      line++;
      start = line;
    } else {
      cell += c;
    }
  }
  if (cell !== '' || cells.length) {
    cells.push(cell);
    out.push({ line: start, cells });
  }
  return out;
}

function scalar(v) {
  if (v === null || v === undefined) return '';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

// buildItems 把解析结果变成要提交的条目，并统计可导入、已过期、无效、被过滤的数量。
export function buildItems(format, text, table, o) {
  const items = [];
  const issues = [];
  const stats = { expired: 0, invalid: 0, filtered: 0 };
  const bad = (line, why) => { stats.invalid++; if (issues.length < MAX_ISSUES) issues.push({ line, why }); };
  if (format === 'lines') {
    stripBOM(text).split(/\r?\n/).forEach((l, i) => {
      const s = l.trim();
      if (!s || s.startsWith('#')) return;
      if (/\s/.test(s)) { bad(i + 1, t('ki.why.space')); return; }
      const body = { secret: (o.secretPrefix || '') + s };
      if (body.secret.length > 4096) { bad(i + 1, t('ki.why.long')); return; }
      if (o.label) body.label = o.label;
      items.push({ line: i + 1, body });
    });
    return { items, issues, stats };
  }
  for (const r of table.rows) {
    if (r.error) { bad(r.line, t('ki.why.notObject')); continue; }
    if (o.whereField && scalar(r.obj[o.whereField]).trim() !== o.whereValue.trim()) { stats.filtered++; continue; }
    const raw = r.obj[o.secretField];
    const secret = typeof raw === 'string' || typeof raw === 'number' ? String(raw).trim() : '';
    if (!secret) { bad(r.line, t('ki.why.noSecret', { f: o.secretField })); continue; }
    const body = { secret: (o.secretPrefix || '') + secret };
    if (body.secret.length > 4096) { bad(r.line, t('ki.why.long')); continue; }
    if (o.expField) {
      const exp = parseExpiry(scalar(r.obj[o.expField]), o.tz);
      if (exp === undefined) { bad(r.line, t('ki.why.badTime', { f: o.expField })); continue; }
      if (exp !== null && exp.getTime() <= o.now && !o.includeExpired) { stats.expired++; continue; }
      body.expires_at = exp === null ? '' : exp.toISOString().replace(/\.\d{3}Z$/, 'Z');
    }
    let label = o.label;
    if (o.labelField) label = scalar(r.obj[o.labelField]).trim();
    if (new TextEncoder().encode(label).length > 64) { bad(r.line, t('ki.why.longLabel')); continue; }
    if (label || o.labelField) body.label = label;
    items.push({ line: r.line, body });
  }
  return { items, issues, stats };
}

// parseTZ：空 = 浏览器本地时区（返回 null）；UTC / Z；±HH:MM（返回分钟偏移）。无法识别返回 undefined。
export function parseTZ(s) {
  if (s === '' || /^local$/i.test(s)) return null;
  if (/^(utc|z)$/i.test(s)) return 0;
  const m = /^([+-])(\d{2}):(\d{2})$/.exec(s);
  if (!m || Number(m[2]) > 14 || Number(m[3]) >= 60) return undefined;
  return (m[1] === '-' ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3]));
}

// parseExpiry 与命令行 gateway keys import 相同（T62）：RFC 3339、Unix 秒（≥ 10^11 视为毫秒）、
// 「YYYY-MM-DD HH:MM:SS」「YYYY-MM-DDTHH:MM:SS」「YYYY-MM-DD HH:MM」「YYYY-MM-DD」（当天 0 点）。
// 空值返回 null（不过期）；无法识别返回 undefined。
export function parseExpiry(s, tz) {
  s = s.trim();
  if (s === '' || s === 'null') return null;
  if (/^-?\d+$/.test(s)) {
    const n = Number(s);
    if (n <= 0) return undefined;
    return new Date(n >= 1e11 ? n : n * 1000);
  }
  if (/^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/.test(s)) {
    const d = new Date(s);
    return Number.isNaN(d.getTime()) ? undefined : d;
  }
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2})(?::(\d{2}))?)?$/.exec(s);
  if (!m) return undefined;
  const p = m.slice(1).map((x) => (x === undefined ? 0 : Number(x)));
  if (p[1] < 1 || p[1] > 12 || p[2] < 1 || p[2] > 31 || p[3] > 23 || p[4] > 59 || p[5] > 59) return undefined;
  const d = tz === null
    ? new Date(p[0], p[1] - 1, p[2], p[3], p[4], p[5])
    : new Date(Date.UTC(p[0], p[1] - 1, p[2], p[3], p[4], p[5]) - tz * 60000);
  return Number.isNaN(d.getTime()) || d.getTime() <= 0 ? undefined : d;
}
