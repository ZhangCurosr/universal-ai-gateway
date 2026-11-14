// 局域网共享页（UI-38，T71）：状态与开关、访问地址、防火墙、在另一台设备上使用。
// 页面不显示、不保存任何 Token：配置片段中只有 <Token> 占位符。

import { h } from '../dom.js';
import { t } from '../i18n.js';
import { api } from '../api.js';
import { errorBox, notice, pill, card, pageHeader, loading, confirmDialog, select } from '../ui.js';

const POLL_MS = 1000;
const POLL_LIMIT_MS = 2 * 60 * 1000;

export async function render(main, ctx) {
  const head = () => pageHeader(t('sh.title'), t('sh.sub'),
    h('button', { type: 'button', class: 'btn', text: t('common.refresh'), onClick: () => render(main, ctx) }));
  main.replaceChildren(head(), loading());
  let v;
  try {
    v = await api('GET', '/admin/api/share');
  } catch (e) {
    main.replaceChildren(head(), errorBox(e));
    return;
  }

  const msg = h('div', { 'aria-live': 'polite' });
  let busy = false;
  const toggle = h('button', { type: 'button', class: 'btn' + (v.lan_open ? '' : ' btn-primary'),
    text: t(v.lan_open ? 'sh.turnOff' : 'sh.turnOn'), onClick: () => setOpen(!v.lan_open) });

  async function setOpen(open) {
    if (busy) return;
    if (!(await confirmDialog(t(open ? 'sh.confirmOn' : 'sh.confirmOff', { port: v.port }), t(open ? 'sh.turnOn' : 'sh.turnOff'), !open))) return;
    busy = true;
    toggle.disabled = true;
    msg.replaceChildren(loading());
    try {
      const resp = await api('POST', '/admin/api/share', { lan_open: open });
      if (resp.changed && resp.reload && resp.reload.mode === 'restart' && resp.reload.record_id) {
        msg.replaceChildren(notice(t('sh.restarting'), 'warn'));
        const rec = await waitRestart(resp.reload.record_id);
        if (!rec || rec.status !== 'applied') {
          msg.replaceChildren(notice(t('cfg.restartFailed', { status: rec ? rec.status : 'timeout', error: rec && rec.error ? rec.error : '' }), 'bad'));
          return;
        }
      }
      ctx.invalidateConfig();
      await render(main, ctx);
    } catch (e) {
      msg.replaceChildren(errorBox(e));
    } finally {
      busy = false;
      toggle.disabled = false;
    }
  }

  // 进程内重启期间管理面暂时不可用：请求失败视为尚未就绪（同 UI-31）
  async function waitRestart(id) {
    const deadline = Date.now() + POLL_LIMIT_MS;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, POLL_MS));
      try {
        const { records } = await api('GET', '/admin/api/reload');
        const rec = records.find((r) => r.id === id);
        if (rec && rec.status !== 'restarting') return rec;
      } catch (_) { /* 重启中 */ }
    }
    return null;
  }

  // 1. 状态与开关
  const status = card(t('sh.status'), h('div', { class: 'stack pad' },
    h('div', { class: 'share-status' },
      pill(t(v.lan_open ? 'sh.open' : 'sh.closed'), v.lan_open ? 'ok' : 'off'),
      h('span', { class: 'mono small muted', text: t('sh.listen', { addr: v.listen }) }),
      v.config_writable ? toggle : null),
    v.config_writable ? null : h('p', { class: 'small muted', text: t('sh.notWritable') }),
    v.lan_open ? notice(t('sh.plainHttp'), 'warn') : null,
    h('p', { class: 'small muted', text: t('sh.adminLocal') }),
    msg));

  // 2. 访问地址
  const urls = v.addresses; // 每项的 url 由服务端给出（LAN-01）
  const addrCard = card(t('sh.addresses'), h('div', { class: 'stack pad' },
    !v.lan_open ? h('p', { class: 'muted', text: t('sh.addrClosed') }) : null,
    urls.length
      ? h('ul', { class: 'share-list' }, urls.map((a) => h('li', {},
        h('span', { class: 'mono strong', text: a.url }), h('span', { class: 'small muted', text: a.interface }), copyBtn(a.url))))
      : h('p', { class: 'muted', text: t('sh.noLan') })));

  // 3. 防火墙
  const fwCmd = v.os === 'windows'
    ? 'New-NetFirewallRule -DisplayName "AI Gateway" -Direction Inbound -Protocol TCP -LocalPort ' + v.port + ' -Action Allow -Profile Private'
    : 'sudo ufw allow from 192.168.0.0/16 to any port ' + v.port + ' proto tcp';
  const fwCard = card(t('sh.firewall'), h('div', { class: 'stack pad' },
    h('p', { class: 'small', text: t(v.os === 'windows' ? 'sh.fwWindows' : 'sh.fwLinux') }),
    snippet(fwCmd)));

  // 4. 在另一台设备上使用
  const pick = select(urls.length ? urls.map((a) => ({ value: a.url, label: a.url + ' · ' + a.interface })) : [{ value: v.url_template, label: v.url_template }]);
  const snippets = h('div', { class: 'stack' });
  const renderSnippets = () => {
    const base = pick.value;
    snippets.replaceChildren(
      h('h3', { class: 'small strong', text: t('sh.env') }), snippet('UAIG_URL=' + base + '\nUAIG_TOKEN=<Token>'),
      h('h3', { class: 'small strong', text: 'Claude Code' }), snippet('ANTHROPIC_BASE_URL=' + base + '\nANTHROPIC_AUTH_TOKEN=<Token>'),
      h('h3', { class: 'small strong', text: 'OpenAI SDK' }), snippet('base_url = "' + base + '/v1"\napi_key = "<Token>"'),
      h('h3', { class: 'small strong', text: 'MCP' }), snippet('claude mcp add --transport http uaig ' + base + '/mcp --header "Authorization: Bearer <Token>"'),
      h('p', { class: 'small muted', text: t('sh.quickstart') }));
  };
  pick.addEventListener('change', renderSnippets);
  renderSnippets();
  const tokenBtn = h('a', { class: 'btn', href: '#/clients?new=lan-device', text: t('sh.createToken') });
  const deviceCard = card(t('sh.device'), h('div', { class: 'stack pad' },
    h('p', { class: 'small', text: t('sh.deviceHint') }),
    h('div', { class: 'share-pick' }, pick, tokenBtn),
    snippets));

  main.replaceChildren(head(), h('div', { class: 'stack-lg' }, status, addrCard, fwCard, deviceCard));
}

function snippet(text) {
  return h('div', { class: 'snippet' }, h('pre', { class: 'mono small', text }), copyBtn(text));
}

function copyBtn(text) {
  const b = h('button', { type: 'button', class: 'btn btn-sm', text: t('common.copy') });
  b.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = t('common.copied');
      setTimeout(() => { b.textContent = t('common.copy'); }, 1500);
    } catch (_) { /* 剪贴板不可用（非安全上下文）：用户可以手动选择文本 */ }
  });
  return b;
}
