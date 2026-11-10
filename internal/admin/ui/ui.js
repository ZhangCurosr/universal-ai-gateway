// 页面共用的组件：错误提示、状态标签、对话框、表单字段。

import { h, icon } from './dom.js';
import { t } from './i18n.js';
import { formatNano, isUnsafe } from './money.js';

// errorBox 显示 API 错误（UI-16）：错误码、消息与 X-Request-Id。
export function errorBox(err, onDismiss) {
  const lines = [
    h('span', { class: 'alert-title', text: t('err.title', { code: err.code || 'error' }) }),
    h('span', { text: err.message || String(err) }),
  ];
  if (err.requestId) lines.push(h('span', { class: 'mono small', text: t('common.requestId') + ': ' + err.requestId }));
  const box = h('div', { class: 'alert alert-bad', role: 'alert' }, icon('alert'), h('div', { class: 'alert-body' }, lines));
  if (onDismiss) {
    box.append(h('button', { type: 'button', class: 'icon-btn', 'aria-label': t('common.close'), onClick: onDismiss }, icon('close', 16)));
  }
  return box;
}

export function notice(text, kind = 'ok') {
  return h('div', { class: 'alert alert-' + kind, role: 'status' }, h('div', { class: 'alert-body' }, h('span', { text })));
}

// pill 是「圆点 + 文字」的状态标签；kind：ok | warn | bad | off。
export function pill(text, kind) {
  return h('span', { class: 'pill pill-' + kind }, h('span', { class: 'dot' }), text);
}

export function money(n) {
  const el = h('span', { class: 'mono', text: formatNano(n) });
  if (isUnsafe(n)) {
    el.classList.add('unsafe');
    el.setAttribute('title', t('common.unsafe'));
  }
  return el;
}

export function bar(fraction, kind = 'accent') {
  const f = Math.max(0, Math.min(1, Number.isFinite(fraction) ? fraction : 0));
  return h('div', { class: 'bar' }, h('div', { class: 'bar-fill bar-' + kind, css: { width: (f * 100).toFixed(1) + '%' } }));
}

// openDialog 打开一个模态对话框，关闭时移除。返回 { dialog, close }。
export function openDialog(title, body, actions, opts = {}) {
  const heading = h('h2', { id: 'dlg-title', text: title });
  const dialog = h('dialog', { class: 'dialog' + (opts.wide ? ' dialog-wide' : ''), 'aria-labelledby': 'dlg-title' },
    h('div', { class: 'dialog-head' }, heading,
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': t('common.close'), onClick: () => close() }, icon('close', 16))),
    h('div', { class: 'dialog-body' }, body),
    actions ? h('div', { class: 'dialog-actions' }, actions) : null);
  function close() {
    if (dialog.open) dialog.close();
  }
  dialog.addEventListener('close', () => {
    dialog.remove();
    if (opts.onClose) opts.onClose();
  });
  document.body.append(dialog);
  dialog.showModal();
  return { dialog, close };
}

// confirmDialog 二次确认（UI-15）。返回 Promise<boolean>。
export function confirmDialog(message, confirmLabel, danger = true) {
  return new Promise((resolve) => {
    let answered = false;
    const yes = h('button', { type: 'button', class: danger ? 'btn btn-danger' : 'btn btn-primary', text: confirmLabel || t('common.confirm') });
    const no = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const { close } = openDialog(t('common.confirm'), h('p', { text: message }), [no, yes], {
      onClose: () => { if (!answered) resolve(false); },
    });
    yes.addEventListener('click', () => { answered = true; resolve(true); close(); });
    no.addEventListener('click', () => close());
  });
}

// field 生成「标签 + 控件 + 提示」。
export function field(id, label, control, hint) {
  control.id = id;
  const parts = [h('label', { for: id, text: label }), control];
  if (hint) parts.push(h('span', { class: 'hint', text: hint }));
  return h('div', { class: 'field' }, parts);
}

export function input(props) {
  return h('input', Object.assign({ class: 'input' }, props));
}

export function select(options, value) {
  const el = h('select', { class: 'input' });
  for (const o of options) el.append(h('option', { value: o.value, text: o.label, selected: o.value === value }));
  return el;
}

export function pageHeader(title, sub, ...right) {
  return h('header', { class: 'page-head' },
    h('div', { class: 'page-title' }, h('h1', { text: title }), sub ? h('span', { class: 'muted', text: sub }) : null),
    right.length ? h('div', { class: 'page-tools' }, right) : null);
}

export function card(title, ...children) {
  const head = title ? h('div', { class: 'card-head' }, typeof title === 'string' ? h('h2', { text: title }) : title) : null;
  return h('section', { class: 'card' }, head, children);
}

export function tableWrap(table) {
  return h('div', { class: 'table-wrap' }, table);
}

export function loading() {
  return h('p', { class: 'muted', text: t('common.loading') });
}

export const MAX_ROWS = 500;
