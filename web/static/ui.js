// Small DOM toolkit. Everything is built with createElement/textContent so
// server-provided (untrusted) strings are never interpreted as HTML.

let uid = 0;
export const nid = (p = 'f') => `${p}${++uid}`;

export const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* storage unavailable */ } },
};

export function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  const late = [];
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k.length > 2 && k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === 'value' || k === 'checked' || k === 'selected') late.push([k, v]);
      else if (k === 'disabled' || k === 'hidden') el[k] = !!v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else el.setAttribute(k, v === true ? '' : String(v));
    }
  }
  append(el, kids);
  for (const [k, v] of late) el[k] = v;
  return el;
}

export function append(el, kids) {
  for (const k of kids.flat(Infinity)) {
    if (k === null || k === undefined || k === false) continue;
    el.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return el;
}

export const text = (s) => document.createTextNode(String(s ?? ''));
export const muted = (s) => h('span', { class: 'muted' }, s);
export const enc = encodeURIComponent;

// ---------- mode ----------
export function initMode() {
  const m = store.get('loadmeter.mode');
  document.documentElement.dataset.mode = m === 'advanced' ? 'advanced' : 'beginner';
}
export const getMode = () => (document.documentElement.dataset.mode === 'advanced' ? 'advanced' : 'beginner');
export const isAdv = () => getMode() === 'advanced';
export function setMode(m) {
  document.documentElement.dataset.mode = m;
  store.set('loadmeter.mode', m);
}

// ---------- toast ----------
export function toast(msg, kind = 'info', ms) {
  const box = document.getElementById('toasts');
  if (!box) return;
  const t = h('div', { class: 'toast ' + kind });
  const close = h('button', { type: 'button', 'aria-label': 'Dismiss message' }, '×');
  t.append(h('span', {}, String(msg)), close);
  const rm = () => t.remove();
  close.addEventListener('click', rm);
  box.append(t);
  setTimeout(rm, ms || (kind === 'error' ? 12000 : 4500));
}

// ---------- busy buttons ----------
export async function busy(btn, fn) {
  if (btn.disabled) return undefined;
  btn.disabled = true;
  btn.classList.add('loading');
  btn.setAttribute('aria-busy', 'true');
  try {
    return await fn(btn);
  } catch (e) {
    if (!e || !e.handled) toast((e && e.message) || String(e), 'error');
    return undefined;
  } finally {
    btn.disabled = false;
    btn.classList.remove('loading');
    btn.removeAttribute('aria-busy');
  }
}

export function button(label, handler, opts = {}) {
  const b = h('button', { type: 'button', class: 'btn ' + (opts.kind || '') + (opts.small ? ' small' : ''), title: opts.title, 'aria-label': opts.aria }, label);
  if (handler) b.addEventListener('click', () => busy(b, handler));
  return b;
}

// plain small button without busy wrapper (for synchronous form edits)
export function sbtn(label, handler, opts = {}) {
  return h('button', { type: 'button', class: 'btn small ' + (opts.kind || ''), 'aria-label': opts.aria, title: opts.title, onClick: handler }, label);
}

// ---------- modal ----------
// buttons: [{label, value, kind}] -> promise resolved with the value (null if dismissed)
export function modal(title, body, buttons = [{ label: 'Close', value: 'close' }], opts = {}) {
  return new Promise((resolve) => {
    const tid = nid('dlg');
    const d = h('dialog', { class: 'modal' + (opts.wide ? ' wide' : ''), 'aria-labelledby': tid });
    const x = h('button', { type: 'button', class: 'btn ghost small', 'aria-label': 'Close dialog' }, '×');
    x.addEventListener('click', () => d.close(''));
    const foot = h('div', { class: 'modal-f' });
    for (const b of buttons) {
      const el = h('button', { type: 'button', class: 'btn ' + (b.kind || '') }, b.label);
      el.addEventListener('click', () => d.close(b.value));
      foot.append(el);
    }
    d.append(h('div', { class: 'modal-in' },
      h('div', { class: 'modal-h' }, h('h2', { id: tid }, title), x),
      h('div', { class: 'modal-b' }, body),
      buttons.length ? foot : null));
    d.addEventListener('close', () => { const v = d.returnValue; d.remove(); resolve(v || null); });
    document.body.append(d);
    d.showModal();
    const first = d.querySelector('.modal-f .btn.primary, .modal-f .btn') || x;
    first.focus();
  });
}

export async function confirmDialog(title, message, okLabel = 'OK', kind = 'primary') {
  const body = typeof message === 'string' ? h('p', {}, message) : message;
  const v = await modal(title, body, [{ label: 'Cancel', value: 'cancel' }, { label: okLabel, value: 'ok', kind }]);
  return v === 'ok';
}

// ---------- form helpers ----------
export function field(label, control, hint, cls = '') {
  if (!control.id) control.id = nid('i');
  return h('div', { class: 'field ' + cls }, h('label', { for: control.id }, label), control, hint ? h('small', { class: 'hint' }, hint) : null);
}
export function check(label, input) {
  return h('label', { class: 'check' }, input, h('span', {}, label));
}
export function badge(txt, kind = 'neutral') { return h('span', { class: 'badge ' + kind }, txt); }
export const statusKind = (s) => ({ supported: 'ok', partial: 'warn', unsupported: 'bad', ignored: 'neutral' }[s] || 'neutral');

export function note(kind, ...kids) { return h('div', { class: 'note ' + kind, role: kind === 'bad' ? 'alert' : null }, kids); }
export function loading(msg = 'Loading...') { return h('p', { class: 'spinner-line', role: 'status' }, msg); }
export function errorBox(e) { return h('div', { class: 'err-box', role: 'alert' }, 'Could not load this view: ' + ((e && e.message) || e)); }

// tab widget; items: [{id,label,adv,render(panel)}]
export function tabs(items, initial, onSelect) {
  const visible = items.filter((i) => !i.adv || isAdv());
  let cur = visible.find((i) => i.id === initial) ? initial : visible[0].id;
  const list = h('div', { class: 'tabs', role: 'tablist' });
  const panel = h('div', { role: 'tabpanel' });
  const btns = {};
  const idp = nid('tab');
  function select(id) {
    cur = id;
    for (const [k, b] of Object.entries(btns)) {
      b.setAttribute('aria-selected', String(k === id));
      b.tabIndex = k === id ? 0 : -1;
    }
    panel.replaceChildren();
    const it = visible.find((i) => i.id === id);
    panel.setAttribute('aria-labelledby', idp + id);
    it.render(panel);
    if (onSelect) onSelect(id);
  }
  visible.forEach((it, idx) => {
    const b = h('button', { type: 'button', role: 'tab', id: idp + it.id }, it.label);
    b.addEventListener('click', () => select(it.id));
    b.addEventListener('keydown', (ev) => {
      let n = null;
      if (ev.key === 'ArrowRight') n = visible[(idx + 1) % visible.length];
      if (ev.key === 'ArrowLeft') n = visible[(idx - 1 + visible.length) % visible.length];
      if (n) { ev.preventDefault(); select(n.id); btns[n.id].focus(); }
    });
    btns[it.id] = b;
    list.append(b);
  });
  const el = h('div', {}, list, panel);
  select(cur);
  return { el, select, current: () => cur };
}

// ---------- formatting ----------
export function fmtMs(v) {
  if (v === null || v === undefined || Number.isNaN(Number(v))) return '–';
  v = Number(v);
  if (v >= 1000) return (v / 1000).toFixed(2) + ' s';
  if (v < 10) return v.toFixed(1) + ' ms';
  return Math.round(v) + ' ms';
}
export function fmtNum(v, digits = 0) {
  if (v === null || v === undefined || Number.isNaN(Number(v))) return '–';
  return Number(v).toLocaleString(undefined, { maximumFractionDigits: digits, minimumFractionDigits: digits });
}
// error rates from the API are percentages (0..100)
export function fmtRate(v) {
  if (v === null || v === undefined || Number.isNaN(Number(v))) return '–';
  return Number(v).toFixed(Number(v) > 0 && Number(v) < 0.1 ? 3 : 2) + ' %';
}
export function fmtClock(sec) {
  if (sec === null || sec === undefined || Number.isNaN(Number(sec))) return '–';
  sec = Math.max(0, Math.round(Number(sec)));
  const m = Math.floor(sec / 60), s = sec % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}
export function fmtBytes(n) {
  if (!n && n !== 0) return '–';
  const u = ['B', 'KB', 'MB', 'GB']; let i = 0; n = Number(n);
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + ' ' + u[i];
}
export function fmtTime(t) {
  if (!t) return '–';
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? String(t) : d.toLocaleString();
}
// Go-style duration strings -> seconds ("1m30s", "500ms")
export function parseDur(s) {
  if (typeof s === 'number') return s / 1000;
  if (!s || typeof s !== 'string') return 0;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  const mult = { ns: 1e-9, us: 1e-6, 'µs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
  let t = 0, m;
  while ((m = re.exec(s))) t += parseFloat(m[1]) * mult[m[2]];
  return t;
}

// planned duration (seconds) of a workload (mirrors model.PlannedDuration)
export function plannedSeconds(w) {
  if (!w) return 0;
  if (w.executor === 'ramping-vus' || w.executor === 'ramping-arrival-rate') {
    return (w.stages || []).reduce((a, s) => a + parseDur(s.duration), 0);
  }
  return parseDur(w.duration);
}

// ---------- generic value rendering (untrusted, text only) ----------
const short = (v) => {
  if (v === null || v === undefined) return '';
  if (typeof v === 'object') { const s = JSON.stringify(v); return s.length > 240 ? s.slice(0, 240) + '…' : s; }
  return String(v);
};

export function dataTable(rows, opts = {}) {
  let cols = opts.columns;
  if (!cols) {
    const seen = [];
    for (const r of rows) for (const k of Object.keys(r)) if (!seen.includes(k)) seen.push(k);
    cols = seen.slice(0, 14);
  }
  const limit = opts.limit || 500;
  const body = h('tbody');
  rows.slice(0, limit).forEach((r) => {
    const cls = opts.rowClass ? opts.rowClass(r) : '';
    body.append(h('tr', { class: cls || null }, cols.map((c) => {
      const v = r[c];
      if (typeof v === 'boolean') return h('td', {}, badge(v ? 'yes' : 'no', v ? 'ok' : 'neutral'));
      if (typeof v === 'number') return h('td', { class: 'num' }, fmtNum(v, Number.isInteger(v) ? 0 : 2));
      if (v && typeof v === 'object') return h('td', {}, h('code', {}, short(v)));
      return h('td', {}, short(v));
    })));
  });
  const wrap = h('div', { class: 'tw' }, h('table', {}, h('thead', {}, h('tr', {}, cols.map((c) => h('th', {}, c)))), body));
  if (rows.length > limit) wrap.append(h('p', { class: 'muted small pad' }, `Showing first ${limit} of ${rows.length} rows.`));
  return wrap;
}

export function renderValue(v, depth = 0) {
  if (v === null || v === undefined || v === '') return muted('–');
  if (typeof v === 'boolean') return badge(v ? 'yes' : 'no', v ? 'ok' : 'neutral');
  if (typeof v !== 'object') {
    const s = String(v);
    return s.length > 200 || s.includes('\n') ? h('pre', {}, s) : h('span', {}, s);
  }
  if (Array.isArray(v)) {
    if (!v.length) return muted('none');
    if (v.every((x) => x === null || typeof x !== 'object')) return h('ul', { class: 'plain' }, v.map((x) => h('li', {}, String(x))));
    if (v.every((x) => x && typeof x === 'object' && !Array.isArray(x))) return dataTable(v);
    return h('pre', {}, JSON.stringify(v, null, 2));
  }
  const entries = Object.entries(v);
  if (!entries.length) return muted('none');
  if (depth > 2) return h('pre', {}, JSON.stringify(v, null, 2));
  return h('dl', { class: 'kv' }, entries.map(([k, val]) => [h('dt', {}, k), h('dd', {}, renderValue(val, depth + 1))]));
}

export function jsonPre(v) { return h('pre', {}, JSON.stringify(v, null, 2)); }

export function bar(frac) {
  const f = Math.max(0, Math.min(1, Number(frac) || 0));
  const s = h('span', {});
  s.style.width = (f * 100).toFixed(0) + '%';
  return h('span', {}, h('span', { class: 'bar', role: 'img', 'aria-label': 'confidence ' + (f * 100).toFixed(0) + ' percent' }, s), (f * 100).toFixed(0) + '%');
}

export function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', {}, h('h1', { tabindex: '-1' }, title), sub ? h('p', { class: 'muted' }, sub) : null),
    actions.length ? h('div', { class: 'actions' }, actions) : null);
}
