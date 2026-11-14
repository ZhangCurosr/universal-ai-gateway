// 「池与服务」卡片（UI-40，T72）：列出、新建、编辑、删除池与服务，写回配置文件并热重载（spec/09 §9）。
// 对话框有「表单」与「高级（YAML）」两个标签页；两边经 POST /admin/api/config/compose 同步。

import { h } from '../dom.js';
import { t, fmtInt } from '../i18n.js';
import { api } from '../api.js';
import { errorBox, notice, card, tableWrap, loading, openDialog, confirmDialog, field, input, select } from '../ui.js';

// entriesCard 返回卡片元素；onChanged 在任何修改生效后调用（例如刷新服务与工具列表）。
export function entriesCard(ctx, onChanged) {
  const body = h('div', { class: 'stack pad' }, loading());
  const el = card(t('pe.title'), body);
  let state = null;

  async function refresh() {
    try {
      state = await api('GET', '/admin/api/config/entries');
    } catch (e) {
      body.replaceChildren(errorBox(e));
      return;
    }
    draw();
  }

  function protoOf(name) { return state.protocols.find((p) => p.name === name); }
  function poolProto(pool) { const p = state.pools.find((x) => x.name === pool); return p ? p.protocol : ''; }

  function draw() {
    const writable = state.writable;
    const newPool = h('button', { type: 'button', class: 'btn btn-primary', text: t('pe.newPool'), disabled: !writable, onClick: () => editor('pools', null) });
    const newSvc = h('button', { type: 'button', class: 'btn', text: t('pe.newService'), disabled: !writable || !state.pools.length, onClick: () => editor('models', null) });
    const act = (kind, it) => h('td', { class: 'num nowrap' },
      h('button', { type: 'button', class: 'link-btn', text: t('pe.edit'), disabled: !writable, onClick: () => editor(kind, it) }),
      h('button', { type: 'button', class: 'link-btn tone-bad', text: t('pe.delete'), disabled: !writable, onClick: () => remove(kind, it) }));
    const th = (x, cls) => h('th', { scope: 'col', class: cls || null, text: x });
    const pools = state.pools.length ? tableWrap(h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, th(t('pe.name')), th(t('pe.protocol')), th(t('pe.baseURL')), th(t('pe.services'), 'num'), th(t('pe.keys'), 'num'), th('', 'num'))),
      h('tbody', {}, state.pools.map((p) => h('tr', {},
        h('td', { class: 'strong', text: p.name }), h('td', { class: 'mono small', text: p.protocol }),
        h('td', { class: 'mono small ellipsis', text: p.fields.base_url || '' }),
        h('td', { class: 'num', text: fmtInt(p.services) }), h('td', { class: 'num', text: fmtInt(p.keys) }), act('pools', p))))))
      : h('p', { class: 'muted', text: t('pe.noPools') });
    const services = state.models.length ? tableWrap(h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, th(t('pe.name')), th(t('pe.pool')), th(t('pe.price')), th('', 'num'))),
      h('tbody', {}, state.models.map((m) => h('tr', {},
        h('td', { class: 'strong', text: m.name }), h('td', { class: 'mono small', text: m.pool }),
        h('td', { class: 'mono small', text: Object.entries(m.fields.price || {}).map(([u, v]) => u + ' ' + v).join(' · ') }), act('models', m))))))
      : null;
    body.replaceChildren(
      h('p', { class: 'small muted', text: t(writable ? 'pe.hint' : 'pe.readOnly') }),
      h('div', { class: 'row-wrap' }, newPool, newSvc),
      h('h3', { class: 'small strong', text: t('pe.pools') }), pools,
      services ? h('h3', { class: 'small strong', text: t('pe.servicesTitle') }) : null, services);
  }

  async function compose(kind, yaml, fields) {
    return api('POST', '/admin/api/config/compose', fields ? { kind, yaml, fields } : { kind, yaml });
  }

  // 价格（与预留）输入：按协议的计量单位逐项列出；已有但不在列表中的单位也列出
  function unitInputs(units, values, placeholder) {
    const all = [...new Set([...(units || []), ...Object.keys(values || {})])];
    const inputs = new Map();
    const rows = all.map((u) => {
      const inp = input({ type: 'text', value: values && values[u] !== undefined ? String(values[u]) : '', placeholder });
      inputs.set(u, inp);
      return field('pe-u-' + placeholder + '-' + u, u, inp);
    });
    return { el: h('div', { class: 'grid-2' }, rows), read: () => Object.fromEntries([...inputs].map(([u, i]) => [u, i.value.trim()])) };
  }

  function editor(kind, item) {
    const creating = !item;
    let yaml = item ? item.yaml : '';
    let fields = item ? Object.assign({}, item.fields) : (kind === 'pools' ? { protocol: '', max_attempts: 0 } : { pool: state.pools[0] ? state.pools[0].name : '' });
    let tab = 'form';
    const msg = h('div', { 'aria-live': 'polite' });
    const formBox = h('div', { class: 'stack' });
    const yamlArea = h('textarea', { class: 'input mono small', rows: '16', spellcheck: 'false' });
    const tabForm = h('button', { type: 'button', class: 'tab', text: t('pe.tabForm') });
    const tabYAML = h('button', { type: 'button', class: 'tab', text: t('pe.tabYAML') });
    let readForm = () => fields;
    let readService = null; // 新建池时「同时新建服务」的表单

    function drawForm() {
      const f = fields;
      if (kind === 'pools') {
        const name = input({ type: 'text', value: f.name || '', disabled: !creating });
        const protos = [{ value: '', label: t('pe.pick') }].concat(state.protocols.map((p) => ({ value: p.name, label: p.name })));
        const proto = select(protos, f.protocol || '');
        const base = input({ type: 'text', value: f.base_url || '' });
        const strategy = select([{ value: '', label: t('pe.default') }, { value: 'least_inflight', label: 'least_inflight' }, { value: 'weighted_round_robin', label: 'weighted_round_robin' }], f.strategy || '');
        const attempts = input({ type: 'number', min: '0', step: '1', value: f.max_attempts ? String(f.max_attempts) : '' , placeholder: '3' });
        const svcBox = h('div', { class: 'stack' });
        const withSvc = h('input', { type: 'checkbox', checked: creating });
        let lastDefault = '';
        const drawSvc = () => {
          readService = null;
          if (!creating || !withSvc.checked) { svcBox.replaceChildren(); return; }
          const p = protoOf(proto.value);
          const sname = input({ type: 'text', value: name.value || '' });
          const upstream = input({ type: 'text', placeholder: t('pe.sameAsName') });
          const price = unitInputs(p ? p.units : [], {}, '0');
          svcBox.replaceChildren(field('pe-sname', t('pe.serviceName'), sname), field('pe-sup', t('pe.upstreamModel'), upstream),
            h('p', { class: 'small muted', text: t(p && p.units.length ? 'pe.priceHint' : 'pe.pickProtocol') }), price.el);
          readService = () => ({ name: sname.value.trim(), pool: name.value.trim(), upstream_model: upstream.value.trim(), price: price.read() });
        };
        proto.addEventListener('change', () => {
          const p = protoOf(proto.value);
          if (p && (!base.value || base.value === lastDefault)) { base.value = p.default_base_url || ''; }
          lastDefault = p ? p.default_base_url || '' : '';
          drawSvc();
        });
        withSvc.addEventListener('change', drawSvc);
        formBox.replaceChildren(field('pe-name', t('pe.name'), name, creating ? t('pe.nameHint') : t('pe.noRename')),
          field('pe-proto', t('pe.protocol'), proto), field('pe-base', t('pe.baseURL'), base),
          h('div', { class: 'grid-2' }, field('pe-strat', t('pe.strategy'), strategy), field('pe-att', t('pe.maxAttempts'), attempts)),
          creating ? h('label', { class: 'check' }, withSvc, h('span', { text: t('pe.withService') })) : null, svcBox);
        drawSvc();
        readForm = () => ({ name: name.value.trim(), protocol: proto.value, base_url: base.value.trim(), strategy: strategy.value, max_attempts: Number(attempts.value) || 0 });
      } else {
        const name = input({ type: 'text', value: f.name || '', disabled: !creating });
        const pool = select(state.pools.map((p) => ({ value: p.name, label: p.name + ' · ' + p.protocol })), f.pool || '');
        const upstream = input({ type: 'text', value: f.upstream_model || '', placeholder: t('pe.sameAsName') });
        const priceBox = h('div', {});
        const reserveBox = h('div', {});
        let price, reserve;
        const drawUnits = (keep) => {
          const p = protoOf(poolProto(pool.value));
          price = unitInputs(p ? p.units : [], keep ? price.read() : f.price, '0');
          reserve = unitInputs([], keep ? reserve.read() : f.reserve, '');
          priceBox.replaceChildren(price.el);
          reserveBox.replaceChildren(reserve.el);
        };
        drawUnits(false);
        pool.addEventListener('change', () => drawUnits(true));
        formBox.replaceChildren(field('pe-name', t('pe.name'), name, creating ? t('pe.nameHint') : t('pe.noRename')),
          field('pe-pool', t('pe.pool'), pool), field('pe-up', t('pe.upstreamModel'), upstream),
          h('h3', { class: 'small strong', text: t('pe.price') }), h('p', { class: 'small muted', text: t('pe.priceHint') }), priceBox,
          h('h3', { class: 'small strong', text: t('pe.reserve') }), reserveBox);
        readForm = () => ({ name: name.value.trim(), pool: pool.value, upstream_model: upstream.value.trim(), price: price.read(), reserve: reserve.read() });
      }
    }

    async function toYAML() { // 表单 → YAML
      fields = readForm();
      const r = await compose(kind, yaml, fields);
      yaml = r.yaml;
      fields = r.fields;
    }
    async function toForm() { // YAML → 表单
      yaml = yamlArea.value;
      const r = await compose(kind, yaml);
      fields = r.fields;
    }
    async function switchTo(next) {
      if (next === tab) return;
      msg.replaceChildren();
      try {
        if (next === 'yaml') { await toYAML(); yamlArea.value = yaml; } else { await toForm(); drawForm(); }
        tab = next;
        paint();
      } catch (e) { msg.replaceChildren(errorBox(e)); }
    }
    function paint() {
      tabForm.classList.toggle('active', tab === 'form');
      tabYAML.classList.toggle('active', tab === 'yaml');
      tabForm.setAttribute('aria-selected', String(tab === 'form'));
      tabYAML.setAttribute('aria-selected', String(tab === 'yaml'));
      formBox.hidden = tab !== 'form';
      yamlArea.hidden = tab !== 'yaml';
    }
    tabForm.addEventListener('click', () => switchTo('form'));
    tabYAML.addEventListener('click', () => switchTo('yaml'));

    async function submit(check) {
      msg.replaceChildren(loading());
      checkBtn.disabled = saveBtn.disabled = true;
      try {
        if (tab === 'form') await toYAML(); else yaml = yamlArea.value;
        const path = '/admin/api/config/' + kind + (creating ? '' : '/' + encodeURIComponent(item.name));
        const res = await api(creating ? 'POST' : 'PUT', path, { version: state.version, yaml, check });
        if (check) { msg.replaceChildren(notice(t('pe.checkOK'))); return; }
        let version = res.version;
        const svc = readService && readService();
        if (svc && svc.name) { // 新建池时同时新建服务
          const r = await compose('models', '', svc);
          const res2 = await api('POST', '/admin/api/config/models', { version, yaml: r.yaml });
          version = res2.version;
        }
        close();
        ctx.invalidateConfig();
        await refresh();
        if (onChanged) onChanged();
      } catch (e) {
        msg.replaceChildren(errorBox(e));
        if (e.status === 409) await refresh();
      } finally {
        checkBtn.disabled = saveBtn.disabled = false;
      }
    }
    const checkBtn = h('button', { type: 'button', class: 'btn', text: t('pe.check'), onClick: () => submit(true) });
    const saveBtn = h('button', { type: 'button', class: 'btn btn-primary', text: t('pe.save'), onClick: () => submit(false) });
    drawForm();
    paint();
    const title = t(kind === 'pools' ? (creating ? 'pe.newPool' : 'pe.editPool') : (creating ? 'pe.newService' : 'pe.editService'), { name: item ? item.name : '' });
    const { close } = openDialog(title, h('div', { class: 'stack' },
      h('div', { class: 'tabs', role: 'tablist' }, tabForm, tabYAML), formBox, yamlArea,
      h('p', { class: 'small muted', text: t('pe.writeNote') }), msg), [checkBtn, saveBtn], { wide: true });
  }

  async function remove(kind, item) {
    const text = kind === 'pools'
      ? t(item.keys > 0 ? 'pe.confirmDeletePoolKeys' : 'pe.confirmDeletePool', { name: item.name, n: fmtInt(item.keys) })
      : t('pe.confirmDeleteService', { name: item.name });
    if (!(await confirmDialog(text, t('pe.delete')))) return;
    try {
      const res = await api('DELETE', '/admin/api/config/' + kind + '/' + encodeURIComponent(item.name), { version: state.version });
      ctx.invalidateConfig();
      await refresh();
      if (res.warnings && res.warnings.length) body.prepend(...res.warnings.map((w) => notice(w, 'warn')));
      if (onChanged) onChanged();
    } catch (e) {
      body.prepend(errorBox(e));
      if (e.status === 409) await refresh();
    }
  }

  refresh();
  return el;
}
