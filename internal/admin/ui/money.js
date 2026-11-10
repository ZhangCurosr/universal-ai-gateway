// 金额换算（spec/09 UI-13）：API 用 nano-USD 整数；页面显示美元、6 位小数。
// 输入的美元字符串用 BigInt 换算，从不经过浮点数。

const NANO_PER_USD = 1000000000n;
const NANO_PER_MICRO = 1000n;

function group(digits) {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

// formatNano 把 nano-USD 显示为 "$1,234.567890"（截断到 6 位小数）。
export function formatNano(n) {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '—';
  const v = BigInt(Math.trunc(n));
  const neg = v < 0n;
  const abs = neg ? -v : v;
  const whole = abs / NANO_PER_USD;
  const micro = (abs % NANO_PER_USD) / NANO_PER_MICRO;
  return (neg ? '-$' : '$') + group(whole.toString()) + '.' + micro.toString().padStart(6, '0');
}

// isUnsafe 报告这个值是否超出 JS 能精确表示的整数范围（约 900 万美元）。
export function isUnsafe(n) {
  return typeof n === 'number' && !Number.isSafeInteger(n);
}

// parseUSD 把用户输入的美元金额转成 nano-USD 整数。
// 返回 { ok: true, nano } 或 { ok: false, error: 'badFormat' | 'tooManyDecimals' | 'tooLarge' }。
export function parseUSD(input) {
  const s = String(input).trim();
  const m = /^(\d+)(?:\.(\d+))?$/.exec(s);
  if (!m) return { ok: false, error: 'badFormat' };
  const frac = m[2] || '';
  if (frac.length > 6) return { ok: false, error: 'tooManyDecimals' };
  const nano = BigInt(m[1]) * NANO_PER_USD + BigInt(frac.padEnd(6, '0')) * NANO_PER_MICRO;
  if (nano > BigInt(Number.MAX_SAFE_INTEGER)) return { ok: false, error: 'tooLarge' };
  return { ok: true, nano: Number(nano) };
}

// nanoToInput 把 nano-USD 写成表单里可编辑的美元字符串（去掉末尾多余的 0）。
export function nanoToInput(n) {
  if (typeof n !== 'number' || !Number.isFinite(n) || n < 0) return '0';
  const v = BigInt(Math.trunc(n));
  const whole = v / NANO_PER_USD;
  const micro = ((v % NANO_PER_USD) / NANO_PER_MICRO).toString().padStart(6, '0').replace(/0+$/, '');
  return micro ? whole.toString() + '.' + micro : whole.toString();
}
