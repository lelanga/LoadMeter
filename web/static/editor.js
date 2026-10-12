// Scenario editor: step tree, request form, variables/data, workload, SLAs, raw JSON.
import { api } from './api.js';
import {
  h, button, sbtn, toast, confirmDialog, modal, field, check, badge, note, loading, tabs, pageHead, isAdv, enc, nid, muted, renderValue,
} from './ui.js';
import { showInspector } from './inspector.js';

const S = { file: null, sc: null, dirty: false, sel: null, section: 'steps', problems: [], onChange: null, refreshTree: () => {}, refreshAll: () => {}, tab: 'steps' };

export const isDirty = () => S.dirty;
export function discard() { S.dirty = false; S.file = null; S.sc = null; S.sel = null; }

const CONTAINERS = new Set(['transaction', 'group', 'loop', 'if']);
const STEP_TYPES = [
  ['request', 'Request'], ['transaction', 'Transaction (timed group)'], ['group', 'Group'], ['loop', 'Loop'],
  ['if', 'If condition'], ['think', 'Think time'], ['set', 'Set variables'],
];
const TAG = { request: 'REQ', transaction: 'TX', group: 'GRP', loop: 'LOOP', if: 'IF', think: 'WAIT', set: 'SET' };
const METHODS = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'];

function changed() { S.dirty = true; if (S.onChange) S.onChange(); }

function normalize(sc) {
  sc.steps = sc.steps || [];
  sc.variables = sc.variables || {};
  sc.defaults = sc.defaults || {};
  sc.dataSources = sc.dataSources || [];
  sc.slas = sc.slas || [];
  sc.workload = sc.workload || { executor: 'constant-vus', vus: 1, duration: '30s' };
  return sc;
}

// ---------- bound inputs ----------
function inp(obj, key, o = {}) {
  const num = o.type === 'number';
  const i = h('input', { type: num ? 'number' : 'text', value: obj[key] ?? '', placeholder: o.placeholder, min: o.min, step: o.step, 'aria-label': o.label, id: o.id, spellcheck: 'false' });
  i.addEventListener('input', () => {
    const v = i.value;
    if (num) { if (v === '') delete obj[key]; else obj[key] = Number(v); }
    else if (v === '' && o.optional) delete obj[key];
    else obj[key] = v;
    changed();
    if (o.onInput) o.onInput(v);
  });
  return i;
}
function sel(obj, key, options, o = {}) {
  const s = h('select', { 'aria-label': o.label },
    o.blank ? h('option', { value: '' }, '(default)') : null,
    options.map((op) => { const [v, l] = Array.isArray(op) ? op : [op, op]; return h('option', { value: v }, l); }));
  s.value = obj[key] ?? (o.blank ? '' : options[0] ? (Array.isArray(options[0]) ? options[0][0] : options[0]) : '');
  s.addEventListener('change', () => {
    if (s.value === '' && o.optional) delete obj[key]; else obj[key] = s.value;
    changed();
    if (o.onChange) o.onChange(s.value);
  });
  return s;
}
function triBool(obj, key) {
  const s = h('select', { 'aria-label': key }, h('option', { value: '' }, '(default)'), h('option', { value: 'true' }, 'Yes'), h('option', { value: 'false' }, 'No'));
  s.value = obj[key] === undefined ? '' : String(obj[key]);
  s.addEventListener('change', () => { if (s.value === '') delete obj[key]; else obj[key] = s.value === 'true'; changed(); });
  return s;
}
function chk(obj, key, label, o = {}) {
  const c = h('input', { type: 'checkbox', checked: !!obj[key] });
  c.addEventListener('change', () => { if (c.checked) obj[key] = true; else delete obj[key]; changed(); if (o.onChange) o.onChange(c.checked); });
  return check(label, c);
}

// ---------- generic rows editor ----------
// cols: [{key,label,type:'text'|'number'|'select'|'checkbox',options,optional,adv,placeholder,visible(item),rerender,width}]
function rowsEditor(items, cols, o) {
  const wrap = h('div', { class: 'rows' });
  const vis = cols.filter((c) => !c.adv || isAdv());
  function draw() {
    wrap.replaceChildren();
    if (!items.length) wrap.append(h('p', { class: 'muted small' }, o.empty || 'Nothing here yet.'));
    else {
      const tb = h('tbody');
      items.forEach((it, idx) => {
        const tr = h('tr');
        for (const c of vis) {
          if (c.visible && !c.visible(it)) { tr.append(h('td')); continue; }
          let ctl;
          const opts = typeof c.options === 'function' ? c.options(it) : c.options;
          if (c.type === 'select') ctl = sel(it, c.key, opts, { label: c.label, optional: c.optional, blank: c.optional, onChange: c.rerender ? draw : undefined });
          else if (c.type === 'checkbox') ctl = chk(it, c.key, '', { onChange: c.rerender ? draw : undefined });
          else if (c.type === 'static') ctl = muted(c.text(it));
          else ctl = inp(it, c.key, { type: c.type, label: c.label, placeholder: c.placeholder, optional: c.optional, onInput: o.onInput });
          if (c.type === 'checkbox') ctl.querySelector('input').setAttribute('aria-label', c.label);
          tr.append(h('td', {}, ctl));
        }
        const rm = sbtn('✕', () => { items.splice(idx, 1); changed(); if (o.onChange) o.onChange(); draw(); }, { aria: 'Remove row ' + (idx + 1), kind: 'danger' });
        tr.append(h('td', {}, rm));
        tb.append(tr);
      });
      wrap.append(h('div', { class: 'tw' }, h('table', { class: 'grid' },
        h('thead', {}, h('tr', {}, vis.map((c) => h('th', {}, c.label)), h('th', {}, h('span', { class: 'sr' }, 'Remove')))), tb)));
    }
    wrap.append(sbtn('+ ' + (o.addLabel || 'Add'), () => { items.push(o.make()); changed(); if (o.onChange) o.onChange(); draw(); }));
  }
  draw();
  return wrap;
}

// ---------- steps ----------
function newStep(type) {
  switch (type) {
    case 'request': return { type, name: 'New request', request: { method: 'GET', url: '/' } };
    case 'transaction': return { type, name: 'New transaction', steps: [] };
    case 'group': return { type, name: 'New group', steps: [] };
    case 'loop': return { type, name: 'Loop', count: 2, steps: [] };
    case 'if': return { type, name: 'If', condition: '${var} == value', steps: [] };
    case 'think': return { type, think: { min: '1s', max: '3s' } };
    default: return { type: 'set', name: 'Set variables', set: [{ name: '', value: '' }] };
  }
}
function stepSummary(st) {
  switch (st.type) {
    case 'request': return `${st.request?.method || ''} ${st.request?.url || ''}`;
    case 'think': return `${st.think?.min || '0'}${st.think?.max ? ' - ' + st.think.max : ''}`;
    case 'loop': return `×${st.count || 0}`;
    case 'if': return st.condition || '';
    case 'set': return (st.set || []).map((k) => k.name).filter(Boolean).join(', ');
    default: return `${(st.steps || []).length} step(s)`;
  }
}
function findParent(list, target) {
  for (let i = 0; i < list.length; i++) {
    if (list[i] === target) return { arr: list, idx: i };
    if (list[i].steps) { const r = findParent(list[i].steps, target); if (r) return r; }
  }
  return null;
}
function contains(st, target) {
  if (st === target) return true;
  return (st.steps || []).some((c) => contains(c, target));
}

const stepIds = new WeakMap();
let stepSeq = 0;
const sid = (st) => { if (!stepIds.has(st)) stepIds.set(st, 's' + (++stepSeq)); return stepIds.get(st); };
let focusKey = null;

function treeList(list, depth) {
  return h('ul', { class: depth ? '' : 'tree', role: depth ? 'group' : null }, list.map((st, i) => treeItem(list, i)));
}
function treeItem(list, i) {
  const st = list[i];
  const selected = S.sel === st;
  const id = sid(st);
  const move = (d) => {
    const j = i + d;
    if (j < 0 || j >= list.length) return;
    [list[i], list[j]] = [list[j], list[i]];
    focusKey = `${id}:${d < 0 ? 'up' : 'down'}`;
    changed(); S.refreshTree();
  };
  const en = h('input', { type: 'checkbox', checked: !st.disabled, 'aria-label': `Enabled: ${st.name || st.type}`, 'data-focus': id + ':en' });
  en.addEventListener('change', () => { if (en.checked) delete st.disabled; else st.disabled = true; focusKey = id + ':en'; changed(); S.refreshTree(); });
  const mk = (txt, label, fn, key, dis) => h('button', { type: 'button', class: 'btn small ghost', 'aria-label': `${label}: ${st.name || st.type}`, title: label, disabled: dis, 'data-focus': id + ':' + key, onClick: fn }, txt);
  const row = h('div', { class: 'tnode' + (selected ? ' sel' : '') + (st.disabled ? ' off' : '') },
    h('button', { type: 'button', class: 'tlabel', 'aria-current': selected ? 'true' : null, onClick: () => { S.sel = st; S.refreshAll(); } },
      h('span', { class: 'chip t-' + st.type }, TAG[st.type] || st.type),
      h('span', { class: 'tname' }, st.name || '(unnamed)', h('span', { class: 'tsub' }, stepSummary(st)))),
    h('span', { class: 'tbtns' }, en,
      mk('▲', 'Move up', () => move(-1), 'up', i === 0), mk('▼', 'Move down', () => move(1), 'down', i === list.length - 1),
      mk('✕', 'Remove', async () => {
        if ((st.steps || []).length && !(await confirmDialog('Remove step?', `"${st.name || st.type}" contains ${st.steps.length} step(s) that will be removed too.`, 'Remove', 'danger'))) return;
        list.splice(i, 1); if (S.sel && contains(st, S.sel)) S.sel = null;
        changed(); S.refreshAll();
      }, 'rm')));
  const li = h('li', {}, row);
  if (st.steps && st.steps.length) li.append(treeList(st.steps, 1));
  return li;
}

function stepsTab(panel) {
  const treeBox = h('div', { class: 'card' });
  const detailBox = h('div', { class: 'card' });
  const typeSel = h('select', { id: nid('ty'), 'aria-label': 'Step type to add' }, STEP_TYPES.map(([v, l]) => h('option', { value: v }, l)));
  const secSel = h('select', { 'aria-label': 'Scenario section' }, [['setup', 'Setup (runs once per VU)'], ['steps', 'Main steps'], ['teardown', 'Teardown']].map(([v, l]) => h('option', { value: v }, l)));
  secSel.value = S.section;
  secSel.addEventListener('change', () => { S.section = secSel.value; S.sel = null; S.refreshAll(); });

  function addStep() {
    const ns = newStep(typeSel.value);
    const root = S.sc[S.section] = S.sc[S.section] || [];
    if (S.sel && CONTAINERS.has(S.sel.type)) { S.sel.steps = S.sel.steps || []; S.sel.steps.push(ns); }
    else if (S.sel) { const p = findParent(root, S.sel); if (p) p.arr.splice(p.idx + 1, 0, ns); else root.push(ns); }
    else root.push(ns);
    S.sel = ns; changed(); S.refreshAll();
  }
  function drawTree() {
    treeBox.replaceChildren();
    const list = S.sc[S.section] = S.sc[S.section] || [];
    const target = S.sel ? (CONTAINERS.has(S.sel.type) ? `inside "${S.sel.name || S.sel.type}"` : `after "${S.sel.name || S.sel.type}"`) : 'at the end of the list';
    treeBox.append(h('h2', {}, 'Steps'), h('div', { class: 'tools' }, h('label', { class: 'sr', for: secSel.id || (secSel.id = nid('sec')) }, 'Section'), secSel),
      h('div', { class: 'tools' }, typeSel, button('Add step', async () => addStep(), { kind: 'primary', small: true }), h('span', { class: 'hint' }, `Adds ${target}.`)),
      list.length ? treeList(list, 0) : h('p', { class: 'muted' }, 'No steps yet. Add a request to begin.'));
    if (focusKey) { const el = treeBox.querySelector(`[data-focus="${focusKey}"]`); focusKey = null; if (el && !el.disabled) el.focus(); }
  }
  function drawDetail() {
    detailBox.replaceChildren();
    if (!S.sel) {
      detailBox.append(h('h2', {}, 'Step details'), h('p', { class: 'muted' }, 'Select a step on the left to edit it.'),
        h('div', { class: 'note beg-only' }, 'A scenario is a list of steps that each virtual user repeats. Requests call your server. Transactions time a group of requests as one business action (for example "login"). Think steps pause like a real person would.'));
      return;
    }
    detailBox.append(stepDetail(S.sel, drawTree));
  }
  S.refreshTree = drawTree;
  S.refreshAll = () => { drawTree(); drawDetail(); };
  panel.append(h('div', { class: 'split' }, treeBox, detailBox));
  S.refreshAll();
}

function stepDetail(st, redrawTree) {
  const root = h('div', {});
  root.append(h('h2', {}, `${TAG[st.type] || st.type} step`));
  const g = h('div', { class: 'form-grid' });
  g.append(field('Name', inp(st, 'name', { optional: true, onInput: redrawTree, placeholder: st.type === 'transaction' ? 'e.g. 01_login (required)' : '' }),
    st.type === 'transaction' ? 'Transactions are timed and reported by this name.' : null));
  if (st.type === 'loop') g.append(field('Repeat count', inp(st, 'count', { type: 'number', min: 1 })));
  if (st.type === 'if') g.append(field('Condition', inp(st, 'condition'), 'Forms: ${a} == b, ${a} != b, or ${a} (true when not empty).', 'wide'));
  g.append(field('Comment', inp(st, 'comment', { optional: true }), null, 'wide'));
  root.append(g, chk(st, 'disabled', 'Disabled (skipped during runs)', { onChange: redrawTree }));
  if (st.type === 'request') root.append(requestForm(st, redrawTree));
  else if (st.type === 'think') {
    st.think = st.think || { min: '1s' };
    root.append(h('div', { class: 'form-grid' }, field('Minimum', inp(st.think, 'min'), 'Go duration such as 500ms, 2s or 1m.'), field('Maximum (optional)', inp(st.think, 'max', { optional: true }), 'A random time between min and max is used.')),
      h('p', { class: 'hint' }, 'The workload "think-time scale" multiplies these values (0 turns them off).'));
  } else if (st.type === 'set') {
    st.set = st.set || [];
    root.append(h('h3', {}, 'Variables to set'), rowsEditor(st.set, [{ key: 'name', label: 'Variable', type: 'text' }, { key: 'value', label: 'Value', type: 'text' }], { make: () => ({ name: '', value: '' }), addLabel: 'Add variable', onInput: redrawTree }));
  } else {
    root.append(h('p', { class: 'hint' }, `Child steps are shown beneath this step in the tree (${(st.steps || []).length}). With this step selected, "Add step" adds inside it.`));
  }
  return root;
}

const EXTRACT_TYPES = ['jsonpath', 'regex', 'xpath', 'header', 'cookie', 'boundary', 'status'];
const ASSERT_TYPES = ['status', 'contains', 'not_contains', 'regex', 'jsonpath', 'max_latency', 'header'];

function requestForm(step, redrawTree) {
  const r = step.request = step.request || { method: 'GET', url: '' };
  const root = h('div', {});
  const urlIn = inp(r, 'url', { placeholder: '/path or https://host/path', onInput: redrawTree });
  const grid = h('div', { class: 'form-grid' },
    field('Method', sel(r, 'method', METHODS)),
    field('URL', urlIn, 'Use ${variable} to insert values. Relative URLs use the scenario base URL.', 'wide'));
  root.append(grid);
  root.append(h('h3', {}, 'Headers'), rowsEditor(r.headers = r.headers || [], [
    { key: 'name', label: 'Name', type: 'text' }, { key: 'value', label: 'Value', type: 'text' }], { make: () => ({ name: '', value: '' }), addLabel: 'Add header', empty: 'No request-specific headers.' }));
  root.append(h('h3', {}, 'Body'),
    field('Raw body', h('textarea', { class: 'mono', rows: '5', spellcheck: 'false', 'aria-label': 'Request body', value: r.body || '', onInput: (e) => { if (e.target.value) r.body = e.target.value; else delete r.body; changed(); } })),
    h('h3', {}, 'Form fields (application/x-www-form-urlencoded)'),
    rowsEditor(r.form = r.form || [], [{ key: 'name', label: 'Field', type: 'text' }, { key: 'value', label: 'Value', type: 'text' }], { make: () => ({ name: '', value: '' }), addLabel: 'Add form field', empty: 'No form fields.' }));
  if (isAdv()) {
    root.append(h('h3', {}, 'Multipart parts'), rowsEditor(r.multipart = r.multipart || [], [
      { key: 'name', label: 'Name', type: 'text' }, { key: 'value', label: 'Value', type: 'text', optional: true }, { key: 'file', label: 'File', type: 'text', optional: true }, { key: 'contentType', label: 'Content-Type', type: 'text', optional: true }],
    { make: () => ({ name: '' }), addLabel: 'Add part', empty: 'No multipart parts.' }),
    h('div', { class: 'form-grid' }, field('Timeout override', inp(r, 'timeout', { optional: true, placeholder: 'e.g. 10s' })),
      field('Follow redirects', triBool(r, 'followRedirects'), 'Blank uses the scenario default.')));
  }
  // extractors
  root.append(h('h3', {}, 'Extractors (save values from the response)'),
    h('p', { class: 'hint beg-only' }, 'Extractors copy a dynamic value (a token, an id) from the response into a variable so later requests can use it as ${name}.'),
    rowsEditor(r.extract = r.extract || [], [
      { key: 'name', label: 'Variable', type: 'text', placeholder: 'token' },
      { key: 'type', label: 'Type', type: 'select', rerender: true, options: (it) => (isAdv() || it.type === 'regex' ? EXTRACT_TYPES : EXTRACT_TYPES.filter((t) => t !== 'regex' && t !== 'xpath')) },
      { key: 'expr', label: 'Expression / name', type: 'text', visible: (it) => it.type !== 'status' && it.type !== 'boundary' && (isAdv() || it.type !== 'regex'), optional: true, placeholder: '$.token' },
      { key: 'left', label: 'Left boundary', type: 'text', visible: (it) => it.type === 'boundary', optional: true },
      { key: 'right', label: 'Right boundary', type: 'text', visible: (it) => it.type === 'boundary', optional: true },
      { key: 'group', label: 'Group', type: 'number', adv: true, visible: (it) => it.type === 'regex' },
      { key: 'match', label: 'Match', type: 'number', adv: true, optional: true, placeholder: '0 first, -1 random' },
      { key: 'default', label: 'Default', type: 'text', optional: true },
      { key: 'required', label: 'Required', type: 'checkbox' },
    ], { make: () => ({ name: '', type: 'jsonpath', expr: '' }), addLabel: 'Add extractor', empty: 'No extractors on this request.' }));
  if (!isAdv() && (r.extract || []).some((e) => e.type === 'regex')) {
    root.append(note('info', 'This request uses a regex extractor. The pattern is hidden in Beginner mode; switch to Advanced to edit it.'));
  }
  root.append(h('h3', {}, 'Assertions (checks that must pass)'),
    rowsEditor(r.assert = r.assert || [], [
      { key: 'type', label: 'Type', type: 'select', rerender: true, options: (it) => (isAdv() || it.type === 'regex' ? ASSERT_TYPES : ASSERT_TYPES.filter((t) => t !== 'regex')) },
      { key: 'expr', label: 'Expression / header', type: 'text', optional: true, visible: (it) => ['regex', 'jsonpath', 'header'].includes(it.type) && (isAdv() || it.type !== 'regex') },
      { key: 'value', label: 'Value', type: 'text', optional: true, placeholder: '200, 2xx, text, 500ms' },
    ], { make: () => ({ type: 'status', value: '200' }), addLabel: 'Add assertion', empty: 'No assertions. Add at least a status check.' }));
  return root;
}

// ---------- general / variables / data ----------
function generalTab(panel) {
  const sc = S.sc;
  panel.append(h('div', { class: 'card' }, h('h2', {}, 'General'),
    h('div', { class: 'form-grid' }, field('Name', inp(sc, 'name')), field('Description', inp(sc, 'description', { optional: true }), null, 'wide'),
      field('Base URL', inp(sc.defaults, 'baseUrl', { optional: true, placeholder: 'http://127.0.0.1:8099' }), 'Prepended to relative request URLs. May be ${baseUrl}.'),
      field('Default timeout', inp(sc.defaults, 'timeout', { optional: true, placeholder: '10s' }))),
    h('h3', {}, 'Default headers (sent with every request)'),
    rowsEditor(sc.defaults.headers = sc.defaults.headers || [], [{ key: 'name', label: 'Name', type: 'text' }, { key: 'value', label: 'Value', type: 'text' }], { make: () => ({ name: '', value: '' }), addLabel: 'Add default header' }),
    isAdv() ? h('div', {}, chk(sc.defaults, 'insecureSkipVerify', 'Skip TLS certificate verification (only for test systems with self-signed certificates)'),
      chk(sc.defaults, 'disableCookies', 'Disable the cookie jar')) : null));

  const pairs = Object.entries(sc.variables).map(([name, value]) => ({ name, value }));
  const sync = () => { const o = {}; for (const p of pairs) if (p.name) o[p.name] = p.value; sc.variables = o; };
  panel.append(h('div', { class: 'card' }, h('h2', {}, 'Variables'),
    h('p', { class: 'hint' }, 'Constants available as ${name} in URLs, headers and bodies. Values extracted during a run override them.'),
    rowsEditor(pairs, [{ key: 'name', label: 'Name', type: 'text' }, { key: 'value', label: 'Value', type: 'text' }], { make: () => ({ name: '', value: '' }), addLabel: 'Add variable', onInput: sync, onChange: sync })));

  const dsBox = h('div', { class: 'card' });
  const drawDS = () => {
    dsBox.replaceChildren(h('h2', {}, 'Data sources'),
      h('p', { class: 'hint' }, 'CSV data gives each virtual user different values (for example logins). Columns become variables like ${username}.'));
    sc.dataSources.forEach((ds, idx) => {
      const colsIn = h('input', { type: 'text', value: (ds.columns || []).join(','), 'aria-label': 'Columns' });
      colsIn.addEventListener('input', () => { const v = colsIn.value.split(',').map((x) => x.trim()).filter(Boolean); if (v.length) ds.columns = v; else delete ds.columns; changed(); });
      dsBox.append(h('div', { class: 'card' }, h('div', { class: 'form-grid' },
        field('Name', inp(ds, 'name')), field('CSV file (relative to scenario)', inp(ds, 'file', { optional: true, placeholder: 'users.csv' })),
        field('Mode', sel(ds, 'mode', [['sequential', 'Sequential'], ['random', 'Random'], ['unique', 'Unique (each row once)'], ['per-vu', 'Per VU (row = VU number)']])),
        field('When data runs out', sel(ds, 'onExhaust', [['recycle', 'Start over'], ['stop', 'Stop the VU'], ['error', 'Fail the iteration']])),
        isAdv() ? field('Delimiter', inp(ds, 'delimiter', { optional: true, placeholder: ',' })) : null,
        isAdv() ? field('Scope', sel(ds, 'scope', [['iteration', 'New row each iteration'], ['vu', 'Fixed row per VU']])) : null,
        isAdv() ? field('Columns (blank = header row)', colsIn) : null),
        chk(ds, 'sensitive', 'Sensitive (mask values in logs and reports)'),
        ds.rows && ds.rows.length ? h('p', { class: 'hint' }, `${ds.rows.length} inline row(s) (edit in the JSON tab).`) : null,
        sbtn('Remove data source', () => { sc.dataSources.splice(idx, 1); changed(); drawDS(); }, { kind: 'danger' })));
    });
    dsBox.append(sbtn('+ Add data source', () => { sc.dataSources.push({ name: 'data', file: 'data.csv', mode: 'sequential', onExhaust: 'recycle' }); changed(); drawDS(); }));
  };
  drawDS();
  panel.append(dsBox);
}

// ---------- workload ----------
const EXEC_BEGINNER = [['constant-vus', 'Fixed users'], ['ramping-vus', 'Ramp up users']];
const EXEC_ADV = [['constant-vus', 'constant-vus (fixed users)'], ['ramping-vus', 'ramping-vus (ramp users)'], ['constant-arrival-rate', 'constant-arrival-rate (fixed request rate)'],
  ['ramping-arrival-rate', 'ramping-arrival-rate (ramp request rate)'], ['per-vu-iterations', 'per-vu-iterations (fixed work per user)']];

function workloadTab(panel) {
  const w = S.sc.workload;
  const card = h('div', { class: 'card' });
  panel.append(card, explainModel());
  function draw() {
    card.replaceChildren(h('h2', {}, 'Workload'));
    let opts = isAdv() ? EXEC_ADV : EXEC_BEGINNER;
    if (!opts.some(([v]) => v === w.executor)) opts = opts.concat([[w.executor, w.executor]]);
    const ex = sel(w, 'executor', opts, { label: 'Executor', onChange: draw });
    const g = h('div', { class: 'form-grid' }, field('Load model', ex,
      w.executor === 'constant-vus' ? 'A fixed number of virtual users loop through the scenario.' :
        w.executor === 'ramping-vus' ? 'The number of virtual users changes in stages (ramp up, hold, ramp down).' :
          w.executor === 'constant-arrival-rate' ? 'New iterations start at a fixed rate, regardless of response time (open model).' :
            w.executor === 'ramping-arrival-rate' ? 'The start rate changes in stages (open model).' : 'Each virtual user runs a fixed number of iterations.'));
    const e = w.executor;
    if (e === 'constant-vus' || e === 'per-vu-iterations') g.append(field('Virtual users', inp(w, 'vus', { type: 'number', min: 1 })));
    if (e === 'constant-vus' || e === 'constant-arrival-rate') g.append(field('Duration', inp(w, 'duration', { placeholder: '1m' }), 'e.g. 30s, 5m, 1h'));
    if (e === 'per-vu-iterations') g.append(field('Iterations per VU', inp(w, 'iterations', { type: 'number', min: 1 })));
    if (e === 'constant-arrival-rate') g.append(field('Rate (iterations / second)', inp(w, 'rate', { type: 'number', min: 0, step: 'any' })));
    if (e === 'constant-arrival-rate' || e === 'ramping-arrival-rate') {
      g.append(field('Max VUs (pool)', inp(w, 'maxVUs', { type: 'number', min: 1 }), 'Upper limit of users that may be started to keep up with the rate.'));
      g.append(field('Pre-allocated VUs', inp(w, 'vus', { type: 'number', min: 0 })));
    }
    if (e === 'ramping-vus') g.append(field('Starting users', inp(w, 'vus', { type: 'number', min: 0 })));
    g.append(
      field('Pacing', inp(w, 'pacing', { optional: true, placeholder: 'e.g. 5s' }), 'Minimum time between iteration starts per user. Blank = none.'),
      field('Think-time scale', inp(w, 'thinkTimeScale', { type: 'number', min: 0, step: '0.1' }), '1 = as recorded, 0 = no waiting, 0.5 = half.'),
      field('On error', sel(w, 'onError', [['continue', 'Continue with next step'], ['abort-iteration', 'Abort this iteration'], ['stop-vu', 'Stop this user']])),
      isAdv() ? field('Graceful stop', inp(w, 'gracefulStop', { optional: true, placeholder: '5s' }), 'How long running iterations may finish after the end.') : null);
    card.append(g);
    if (e === 'ramping-vus' || e === 'ramping-arrival-rate') {
      card.append(h('h3', {}, e === 'ramping-vus' ? 'Stages (ramp-up, hold, ramp-down)' : 'Stages (target iterations per second)'),
        h('p', { class: 'hint beg-only' }, 'Each stage moves from the previous level to the target over its duration. Example: 30s to 20 ramps up; 5m to 20 holds; 30s to 0 ramps down.'),
        rowsEditor(w.stages = w.stages || [], [{ key: 'duration', label: 'Duration', type: 'text', placeholder: '30s' }, { key: 'target', label: e === 'ramping-vus' ? 'Target users' : 'Target rate', type: 'number' }],
          { make: () => ({ duration: '30s', target: 10 }), addLabel: 'Add stage', empty: 'No stages. Add at least one.' }));
    }
  }
  draw();
}

function explainModel() {
  return h('div', { class: 'card' }, h('h2', {}, 'Virtual users or request rate? Why it matters'),
    h('details', { open: !isAdv() },
      h('summary', {}, 'Closed model (virtual users) vs open model (arrival rate)'),
      h('p', {}, h('strong', {}, 'Virtual users (concurrency, closed model). '), 'A fixed group of users each finish a request, think, then start the next one. If the server slows down, users wait longer and send fewer requests, so the load falls exactly when the system struggles. This matches a fixed population such as staff using an internal app.'),
      h('p', {}, h('strong', {}, 'Request or iteration rate (open model). '), 'New iterations arrive on a schedule whatever the response time, like public traffic. If the server slows down, work keeps arriving and queues build, so you see the real latency and failures of overload. A closed test can hide this problem, because slow responses quietly reduce the load.'),
      h('p', {}, 'Rule of thumb: use users to answer "how does it behave with N people?" and rate to answer "can it handle N requests per second?". Rate tests need enough Max VUs, otherwise the generator, not the server, becomes the limit and iterations are reported as dropped.')));
}

// ---------- SLAs ----------
function slaTab(panel) {
  panel.append(h('div', { class: 'card' }, h('h2', {}, 'Service level objectives (SLA)'),
    h('p', { class: 'hint' }, 'Pass/fail rules checked live and in the final report. Latency metrics are in milliseconds, error_rate in percent, rps in requests per second. Leave scope empty for all requests, or enter a transaction or request name.'),
    rowsEditor(S.sc.slas = S.sc.slas || [], [
      { key: 'metric', label: 'Metric', type: 'select', options: ['p50', 'p90', 'p95', 'p99', 'avg', 'max', 'error_rate', 'rps'] },
      { key: 'scope', label: 'Scope', type: 'text', optional: true, placeholder: 'all' },
      { key: 'op', label: 'Operator', type: 'select', options: ['<', '<=', '>', '>='] },
      { key: 'threshold', label: 'Threshold', type: 'number' },
    ], { make: () => ({ metric: 'p95', op: '<', threshold: 500 }), addLabel: 'Add SLA', empty: 'No SLAs. Add one so runs get a pass/fail verdict.' })));
}

// ---------- raw JSON ----------
function jsonTab(panel) {
  const ta = h('textarea', { class: 'mono', rows: '28', spellcheck: 'false', 'aria-label': 'Scenario JSON', value: JSON.stringify(S.sc, null, 2) });
  const status = h('p', { role: 'status', class: 'small' }, badge('valid JSON', 'ok'));
  ta.addEventListener('input', () => {
    try {
      const v = JSON.parse(ta.value);
      if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error('top level must be an object');
      for (const k of Object.keys(S.sc)) delete S.sc[k];
      Object.assign(S.sc, v); normalize(S.sc); S.sel = null; changed();
      status.replaceChildren(badge('valid JSON, applied to the visual editor', 'ok'));
    } catch (e) {
      status.replaceChildren(badge('invalid JSON (not applied)', 'bad'), ' ', e.message);
    }
  });
  panel.append(h('div', { class: 'card' }, h('h2', {}, 'Raw JSON'),
    h('p', { class: 'hint' }, 'Edits that parse as valid JSON are applied to the visual model immediately; the visual editor and this view stay in sync.'),
    ta, status, h('div', { class: 'actions' }, sbtn('Format', () => { try { ta.value = JSON.stringify(JSON.parse(ta.value), null, 2); } catch { /* invalid */ } }))));
}

// ---------- page ----------
export async function renderEditor(root, params, ctx = {}) {
  const file = params.file;
  if (!(ctx.keep && S.file === file && S.sc)) {
    root.replaceChildren(loading());
    const sc = await api('GET', '/api/scenarios/' + enc(file));
    Object.assign(S, { file, sc: normalize(sc), dirty: false, sel: null, section: 'steps', problems: [], tab: 'steps' });
  }
  root.replaceChildren();
  const dirtyBadge = badge('Unsaved changes', 'warn');
  dirtyBadge.hidden = !S.dirty;
  S.onChange = () => { dirtyBadge.hidden = false; };
  const problemsBox = h('div', { 'aria-live': 'polite' });
  const showProblems = (problems, extra) => {
    problemsBox.replaceChildren();
    if (problems && problems.length) problemsBox.append(note('warn', h('strong', {}, `${problems.length} problem(s) found:`), h('ul', {}, problems.map((p) => h('li', {}, String(p))))));
    else if (problems) problemsBox.append(note('ok', 'No problems found.'));
    if (extra) problemsBox.append(extra);
  };
  if (S.problems.length) showProblems(S.problems);

  async function save() {
    const res = await api('PUT', '/api/scenarios/' + enc(S.file), S.sc);
    S.dirty = false; dirtyBadge.hidden = true; S.problems = res.problems || [];
    showProblems(S.problems);
    toast(S.problems.length ? `Saved with ${S.problems.length} problem(s)` : 'Saved', S.problems.length ? 'info' : 'ok');
  }
  async function validate() {
    if (S.dirty) await save();
    const res = await api('POST', `/api/scenarios/${enc(S.file)}/validate`);
    const extra = [];
    if ((res.targets || []).length) extra.push(note('info', 'Target hosts: ', (res.targets).join(', ')));
    if ((res.blocked || []).length) extra.push(note('bad', 'Blocked by the safety policy: ', res.blocked.join(', '), '. Add a host in Settings only if you are authorized to test it.'));
    showProblems(res.problems || [], extra);
  }
  const debugVars = h('input', { type: 'text', placeholder: '{"user":"alice"}', 'aria-label': 'Debug variables as JSON' });
  async function debug() {
    let vars;
    if (debugVars.value.trim()) { try { vars = JSON.parse(debugVars.value); } catch { throw new Error('Debug variables must be a JSON object'); } }
    if (S.dirty) await save();
    const res = await api('POST', `/api/scenarios/${enc(S.file)}/debug`, vars ? { vars } : {});
    await showInspector(res);
  }

  root.append(pageHead(`Edit: ${S.sc.name || S.file}`, h('span', {}, S.file, ' ', dirtyBadge),
    button('Save', save, { kind: 'primary' }),
    button('Validate', validate),
    button('Debug 1 iteration', debug, { title: 'Runs one iteration with one virtual user and shows every request and response' }),
    h('a', { class: 'btn', href: '#/correlate/' + enc(S.file) }, 'Correlations'),
    h('a', { class: 'btn', href: '#/run?file=' + enc(S.file) }, 'Run...'),
    h('a', { class: 'btn ghost', href: '#/scenarios' }, 'Back')),
  h('div', { class: 'adv-only tools' }, h('label', { for: (debugVars.id = nid('dv')) }, 'Debug vars (JSON, optional):'), debugVars),
  problemsBox);

  const t = tabs([
    { id: 'steps', label: 'Steps', render: stepsTab },
    { id: 'general', label: 'General & data', render: generalTab },
    { id: 'workload', label: 'Workload', render: workloadTab },
    { id: 'sla', label: 'SLAs', render: slaTab },
    { id: 'json', label: 'Raw JSON', adv: true, render: jsonTab },
  ], S.tab, (id) => { S.tab = id; });
  root.append(t.el);
  return () => { S.onChange = null; };
}

export { muted, renderValue };
