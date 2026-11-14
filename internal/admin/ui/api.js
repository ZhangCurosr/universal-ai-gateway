// 管理 API 客户端（spec/09 UI-09、UI-16、UI-19）。
// Token 只放在 sessionStorage，每次请求经 Authorization 头发送；不用 Cookie。

import { t } from './i18n.js';

const TOKEN_KEY = 'uaig.token';
const TIMEOUT_MS = 15000;

export function getToken() {
  try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch (_) { return ''; }
}

export function setToken(token) {
  try { sessionStorage.setItem(TOKEN_KEY, token); } catch (_) { /* 存储不可用时本标签页内仍可用到刷新为止 */ }
}

export function clearToken() {
  try { sessionStorage.removeItem(TOKEN_KEY); } catch (_) { /* 同上 */ }
}

export class ApiError extends Error {
  constructor(status, code, message, requestId) {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

let onUnauthorized = () => {};
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

// api 发送一次请求并返回解析后的 JSON。opts.token 用于登录时验证尚未保存的 Token；opts.timeout 覆盖默认的 15 秒（UI-19）。
export async function api(method, path, body, opts = {}) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), opts.timeout || TIMEOUT_MS); // opts.timeout：批量导入等较慢的写入
  const headers = { Accept: 'application/json' };
  const token = opts.token !== undefined ? opts.token : getToken();
  if (token) headers.Authorization = 'Bearer ' + token;
  let payload;
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }

  let resp, text;
  try {
    resp = await fetch(path, { method, headers, body: payload, signal: ctrl.signal, credentials: 'omit', cache: 'no-store' });
    text = await resp.text();
  } catch (e) {
    const timedOut = e && e.name === 'AbortError';
    throw new ApiError(0, timedOut ? 'timeout' : 'network', t(timedOut ? 'err.timeout' : 'err.network'), '');
  } finally {
    clearTimeout(timer);
  }

  const requestId = resp.headers.get('X-Request-Id') || '';
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch (_) { data = null; }
  }
  if (!resp.ok) {
    const e = data && data.error ? data.error : {};
    const err = new ApiError(resp.status, e.code || 'http_' + resp.status, e.message || resp.statusText || String(resp.status), requestId);
    if (resp.status === 401 && opts.token === undefined) {
      clearToken();
      onUnauthorized();
    }
    throw err;
  }
  return data;
}
