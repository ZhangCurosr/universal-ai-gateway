// 管理 Web UI 入口（spec/09 §6）：路由（location.hash）、登录状态与页面外壳。

import { h, icon, logoMark } from './dom.js';
import { t, getLang, setLang } from './i18n.js';
import { api, getToken, setToken, clearToken, setUnauthorizedHandler } from './api.js';
import { field, input } from './ui.js';
import * as overview from './pages/overview.js';
import * as clients from './pages/clients.js';
import * as keys from './pages/keys.js';
import * as usage from './pages/usage.js';
import * as config from './pages/config.js';
import * as share from './pages/share.js';

const PAGES = {
  overview: { mod: overview, label: 'nav.overview', icon: 'overview' },
  clients: { mod: clients, label: 'nav.clients', icon: 'clients' },
  keys: { mod: keys, label: 'nav.keys', icon: 'keys' },
  usage: { mod: usage, label: 'nav.usage', icon: 'usage' },
  share: { mod: share, label: 'nav.share', icon: 'share' },
  config: { mod: config, label: 'nav.config', icon: 'config' },
};

const root = document.getElementById('app');
let cleanup = null;
let health = null;   // 最近一次 /admin/api/health 的结果（外壳显示版本）
let configCache = null;

// ctx 是传给各页面的共享能力。
const ctx = {
  async config() {
    if (!configCache) configCache = await api('GET', '/admin/api/config');
    return configCache;
  },
  invalidateConfig() { configCache = null; }, // 重载后配置可能已变（UI-31）
  async health() {
    health = await api('GET', '/admin/api/health');
    return health;
  },
  rerender: () => route(),
};

function currentPage() {
  const name = (location.hash.replace(/^#\/?/, '') || 'overview').split('?')[0];
  return PAGES[name] ? name : 'overview';
}

function runCleanup() {
  if (cleanup) {
    try { cleanup(); } catch (_) { /* 页面清理失败不影响导航 */ }
    cleanup = null;
  }
}

async function route() {
  runCleanup();
  if (!getToken()) {
    renderLogin();
    return;
  }
  const name = currentPage();
  const main = h('main', { class: 'main', id: 'main' });
  root.replaceChildren(shell(name), main);
  root.className = 'layout';
  document.title = t(PAGES[name].label) + ' · ' + t('app.name');
  try {
    const c = await PAGES[name].mod.render(main, ctx);
    if (typeof c === 'function') cleanup = c;
  } catch (e) {
    // 页面自己会显示 API 错误；这里只兜底未预期的异常
    main.append(h('div', { class: 'alert alert-bad', role: 'alert' }, h('div', { class: 'alert-body' }, h('span', { text: String(e && e.message || e) }))));
  }
}

function shell(active) {
  const links = Object.entries(PAGES).map(([name, p]) =>
    h('a', { href: '#/' + name, class: 'nav-link' + (name === active ? ' active' : ''), 'aria-current': name === active ? 'page' : null },
      icon(p.icon), t(p.label)));
  const version = health && health.version ? health.version : '';
  return h('nav', { class: 'nav', 'aria-label': t('nav.main') },
    h('div', { class: 'brand' }, logoMark(28),
      h('div', { class: 'brand-text' }, h('span', { class: 'brand-name', text: t('app.name') }), h('span', { class: 'muted small', text: t('app.console') }))),
    h('div', { class: 'nav-links' }, links),
    h('div', { class: 'nav-foot' },
      version ? h('span', {}, t('nav.version') + ' ', h('span', { class: 'mono strong', text: version })) : null,
      h('span', {}, h('span', { class: 'mono strong', text: location.host })),
      h('div', { class: 'nav-foot-actions' },
        h('button', { type: 'button', class: 'link-btn', 'aria-label': t('nav.langLabel'), text: t('nav.lang'), onClick: () => { setLang(getLang() === 'zh' ? 'en' : 'zh'); route(); } }),
        h('button', { type: 'button', class: 'link-btn', text: t('nav.logout'), onClick: logout }))));
}

function logout() {
  clearToken();
  health = null;
  configCache = null;
  location.hash = '';
  route();
}

function renderLogin() {
  root.className = 'login-page';
  document.title = t('login.title') + ' · ' + t('app.name');
  const msg = h('div', { 'aria-live': 'polite' });
  const tokenInput = input({ type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: 'uai_live_...', class: 'input mono' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-block', text: t('login.submit') });
  const form = h('form', { class: 'card login-card', novalidate: true },
    h('h1', { text: t('login.title') }),
    msg,
    field('token', t('login.token'), tokenInput, t('login.tokenHint')),
    submit);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const token = tokenInput.value.trim();
    msg.replaceChildren();
    if (!token) {
      msg.append(loginError(t('login.empty')));
      return;
    }
    submit.disabled = true;
    submit.textContent = t('login.checking');
    try {
      health = await api('GET', '/admin/api/health', undefined, { token });
      setToken(token);
      tokenInput.value = '';
      if (!location.hash) location.hash = '#/overview';
      route();
    } catch (e) {
      const key = { 401: 'login.e401', 403: 'login.e403', 429: 'login.e429', 421: 'login.e421' }[e.status];
      msg.append(loginError(key ? t(key) : e.code + ': ' + e.message));
    } finally {
      submit.disabled = false;
      submit.textContent = t('login.submit');
    }
  });

  root.replaceChildren(h('div', { class: 'login-wrap' },
    h('div', { class: 'brand brand-lg' }, logoMark(36),
      h('div', { class: 'brand-text' }, h('span', { class: 'brand-name', text: t('app.name') }), h('span', { class: 'muted', text: t('app.console') }))),
    form,
    h('div', { class: 'login-help muted small' }, h('span', { text: t('login.help1') }), h('span', { text: t('login.help2') })),
    h('button', { type: 'button', class: 'link-btn login-lang', 'aria-label': t('nav.langLabel'), text: t('nav.lang'), onClick: () => { setLang(getLang() === 'zh' ? 'en' : 'zh'); renderLogin(); } })));
  tokenInput.focus();
}

function loginError(text) {
  return h('div', { class: 'alert alert-bad', role: 'alert' }, icon('alert'), h('div', { class: 'alert-body' }, h('span', { text })));
}

setUnauthorizedHandler(() => {
  health = null;
  configCache = null;
  route();
});

window.addEventListener('hashchange', route);
route();
