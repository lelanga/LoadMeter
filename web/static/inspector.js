// Request Inspector: shows the exchanges from POST /api/scenarios/{file}/debug
import { h, modal, badge, renderValue, fmtMs, tabs, muted } from './ui.js';

const MAX_BODY = 200000;

function body(txt) {
  if (txt === undefined || txt === null || txt === '') return muted('(empty)');
  let s = typeof txt === 'string' ? txt : JSON.stringify(txt);
  let cut = false;
  if (s.length > MAX_BODY) { s = s.slice(0, MAX_BODY); cut = true; }
  try { const j = JSON.parse(s); if (j && typeof j === 'object') s = JSON.stringify(j, null, 2); } catch { /* keep raw */ }
  return h('div', {}, h('pre', {}, s), cut ? h('p', { class: 'muted small' }, 'Body truncated for display.') : null);
}

const list = (v) => (Array.isArray(v) ? v : v && typeof v === 'object' ? Object.keys(v) : []);

function section(title, node) { return h('section', {}, h('h3', {}, title), node); }

function detail(ex) {
  const missing = list(ex.missingExtractions);
  const unresolved = list(ex.unresolved);
  const assertions = ex.assertions || [];
  const summary = h('div', { class: 'actions' },
    badge(ex.ok ? 'OK' : 'FAILED', ex.ok ? 'ok' : 'bad'),
    h('code', {}, `${ex.method || ''} ${ex.url || ''}`),
    badge('HTTP ' + (ex.status ?? '?'), ex.status >= 400 || !ex.status ? 'bad' : 'info'),
    badge(fmtMs(ex.latencyMs), 'neutral'));
  const root = h('div', {}, summary);
  if (ex.error) root.append(h('div', { class: 'note bad' }, 'Error: ' + ex.error));
  if (missing.length) root.append(h('div', { class: 'note warn' }, 'Missing extractions (the value was not found in the response): ', missing.map((m) => h('code', {}, typeof m === 'string' ? m : JSON.stringify(m))).flatMap((c) => [c, ' '])));
  if (unresolved.length) root.append(h('div', { class: 'note warn' }, 'Unresolved variables (used but never set): ', unresolved.map((m) => h('code', {}, String(m))).flatMap((c) => [c, ' '])));
  const t = tabs([
    { id: 'req', label: 'Request', render: (p) => p.append(section('Headers', renderValue(ex.requestHeaders)), section('Body', body(ex.requestBody))) },
    { id: 'res', label: 'Response', render: (p) => p.append(section('Headers', renderValue(ex.responseHeaders)), section('Body', body(ex.responseBody))) },
    { id: 'ext', label: `Extracted (${Object.keys(ex.extracted || {}).length})`, render: (p) => p.append(section('Extracted variables', renderValue(ex.extracted)), section('Missing extractions', renderValue(ex.missingExtractions))) },
    { id: 'as', label: `Assertions (${assertions.length})`, render: (p) => p.append(assertions.length ? h('div', { class: 'tw' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Result', 'Type', 'Expression', 'Expected', 'Message'].map((x) => h('th', {}, x)))),
      h('tbody', {}, assertions.map((a) => h('tr', { class: a.passed ? null : 'bad-row' }, h('td', {}, badge(a.passed ? 'PASS' : 'FAIL', a.passed ? 'ok' : 'bad')),
        h('td', {}, a.type || ''), h('td', {}, a.expr || ''), h('td', {}, a.value || ''), h('td', {}, a.message || ''))))))
      : muted('No assertions on this request.')) },
    { id: 'vars', label: 'Variables used', render: (p) => p.append(section('Variables used', renderValue(ex.varsUsed)), section('Unresolved', renderValue(ex.unresolved))) },
  ], 'res');
  root.append(t.el);
  return root;
}

export function showInspector(result) {
  const ex = result.exchanges || [];
  const pane = h('div', {});
  const ul = h('ul', { class: 'xlist', 'aria-label': 'Exchanges' });
  const buttons = [];
  function pick(i) {
    buttons.forEach((b, j) => b.setAttribute('aria-current', String(i === j)));
    pane.replaceChildren(detail(ex[i]));
  }
  ex.forEach((e, i) => {
    const b = h('button', { type: 'button' },
      h('span', {}, `${i + 1}. ${e.step || e.method || ''}`),
      h('span', {}, badge(String(e.status ?? '?'), e.ok ? 'ok' : 'bad'), ' ', fmtMs(e.latencyMs)),
      h('span', { class: 'u' }, `${e.method || ''} ${e.url || ''}`));
    b.addEventListener('click', () => pick(i));
    buttons.push(b);
    ul.append(h('li', {}, b));
  });
  const body = ex.length
    ? h('div', { class: 'inspector' }, h('div', {}, ul), pane)
    : h('p', { class: 'muted' }, 'The debug run produced no exchanges.');
  const head = h('div', { class: 'note ' + (result.ok ? 'ok' : 'warn') }, result.ok ? 'Debug iteration passed.' : 'Debug iteration reported problems. Select an exchange to see why.',
    ' Sensitive headers and fields are redacted by the server.');
  const p = modal('Request Inspector (1 iteration, 1 virtual user)', h('div', {}, head, body), [{ label: 'Close', value: 'close' }], { wide: true });
  if (ex.length) {
    const firstBad = ex.findIndex((e) => !e.ok);
    pick(firstBad >= 0 ? firstBad : 0);
  }
  return p;
}
