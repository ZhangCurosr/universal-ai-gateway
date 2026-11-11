// DOM 构造工具（spec/09 UI-12）：所有文本都经 textContent / 文本节点写入，从不解析 HTML 字符串。

const SVG_NS = 'http://www.w3.org/2000/svg';

// h 创建元素。props：class、text、css（对象，经 CSSOM 设置）、on<Event>（函数）、
// value/checked/disabled/selected（作为属性设置），其余经 setAttribute。
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  applyProps(el, props);
  append(el, children);
  return el;
}

function applyProps(el, props) {
  if (!props) return;
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = String(v);
    else if (k === 'css') Object.assign(el.style, v);
    else if (k.length > 2 && k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'value' || k === 'checked' || k === 'disabled' || k === 'selected' || k === 'readOnly') el[k] = v;
    else el.setAttribute(k, v === true ? '' : String(v));
  }
}

export function append(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// 线条图标：路径数据来自本文件常量，不来自任何外部输入。
const ICONS = {
  overview: ['M3 3h7v9H3z', 'M14 3h7v5h-7z', 'M14 12h7v9h-7z', 'M3 16h7v5H3z'],
  clients: ['M9 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M2 21c0-4 3-6 7-6s7 2 7 6', 'M17 11a3 3 0 1 0 0-6', 'M22 21c0-3-2-5-5-5.5'],
  keys: ['M8 19a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M11 12l9-9', 'M17 6l3 3', 'M14 9l2 2'],
  usage: ['M3 3v18h18', 'M7 15l4-4 3 3 5-6'],
  plus: ['M12 5v14', 'M5 12h14'],
  close: ['M6 6l12 12', 'M18 6L6 18'],
  alert: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z', 'M12 7v6', 'M12 16.5v.5'],
  warn: ['M12 3l10 18H2z', 'M12 10v5', 'M12 18v.5'],
  refresh: ['M20 11a8 8 0 1 0-2.3 5.7', 'M20 4v7h-7'],
  config: ['M4 6h10', 'M18 6h2', 'M16 4v4', 'M4 12h4', 'M12 12h8', 'M10 10v4', 'M4 18h12', 'M20 18h0', 'M18 16v4'],
};

export function icon(name, size = 18) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of ICONS[name] || []) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

export function logoMark(size = 28) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('viewBox', '0 0 28 28');
  svg.setAttribute('aria-hidden', 'true');
  const r = document.createElementNS(SVG_NS, 'rect');
  for (const [k, v] of [['x', '1'], ['y', '1'], ['width', '26'], ['height', '26'], ['rx', '7'], ['fill', '#111827']]) r.setAttribute(k, v);
  const p = document.createElementNS(SVG_NS, 'path');
  for (const [k, v] of [['d', 'M8 10h12M8 14h8M8 18h12'], ['stroke', '#FFFFFF'], ['stroke-width', '2'], ['stroke-linecap', 'round']]) p.setAttribute(k, v);
  svg.append(r, p);
  return svg;
}
