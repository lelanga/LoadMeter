// Results: run list, summaries, comparison, downloads.
import { api } from './api.js';
import { h, button, badge, note, loading, pageHead, enc, fmtMs, fmtNum, fmtRate, fmtTime, fmtClock, renderValue, dataTable, muted, toast } from './ui.js';
import { renderSummary, downloads } from './summary.js';

const COLS = [
  ['runId', 'Run', 'text'], ['scenario', 'Scenario', 'text'], ['startedAt', 'Started', 'time'], ['durationSec', 'Duration', 'clock'],
  ['requests', 'Requests', 'num'], ['p95Ms', 'p95', 'ms'], ['errorRate', 'Error rate', 'rate'], ['slaPassed', 'SLA', 'sla'], ['status', 'Status', 'text'],
];

const regressWord = /regress|worse|degrad|slower|breach/i;
function isRegression(row) {
  for (const [k, v] of Object.entries(row)) {
    if (regressWord.test(k) && (v === true || (typeof v === 'string' && v && !/^(no|false|none|ok|improved|same)$/i.test(v)))) return true;
    if (/^(verdict|status|change|result|direction)$/i.test(k) && typeof v === 'string' && regressWord.test(v)) return true;
  }
  return false;
}
function renderComparison(c) {
  const root = h('div', {});
  const scalars = {};
  for (const [k, v] of Object.entries(c)) {
    if (Array.isArray(v) && v.length && v.every((x) => x && typeof x === 'object')) {
      root.append(h('h3', {}, k), dataTable(v, { rowClass: (r) => (isRegression(r) ? 'bad-row' : '') }));
    } else if (v && typeof v === 'object' && !Array.isArray(v)) {
      root.append(h('h3', {}, k), renderValue(v));
    } else scalars[k] = v;
  }
  if (Object.keys(scalars).length) root.prepend(renderValue(scalars));
  return root;
}

export async function renderResults(root) {
  root.replaceChildren(loading());
  let runs = await api('GET', '/api/runs');
  let sortKey = 'startedAt', sortDir = -1;
  const picked = [];
  const tbody = h('tbody');
  const thead = h('tr');
  const out = h('div', { 'aria-live': 'polite' });
  const cmpBtn = button('Compare selected', async () => {
    if (picked.length !== 2) throw new Error('Select exactly two runs to compare.');
    const [a, b] = picked.map((id) => runs.find((r) => r.runId === id));
    const [base, run] = new Date(a.startedAt) <= new Date(b.startedAt) ? [a, b] : [b, a];
    out.replaceChildren(loading('Comparing...'));
    const c = await api('GET', `/api/compare?base=${enc(base.runId)}&run=${enc(run.runId)}`);
    out.replaceChildren(h('div', { class: 'card' }, h('h2', {}, `Comparison: ${run.runId} against baseline ${base.runId}`),
      h('p', { class: 'hint' }, 'The older selected run is the baseline. Rows marked in red are regressions.'), renderComparison(c)));
    out.scrollIntoView({ block: 'start' });
  }, { kind: 'primary' });
  cmpBtn.disabled = true;

  function cell(r, [k, , t]) {
    const v = r[k];
    switch (t) {
      case 'time': return h('td', {}, fmtTime(v));
      case 'clock': return h('td', { class: 'num' }, fmtClock(v));
      case 'num': return h('td', { class: 'num' }, fmtNum(v));
      case 'ms': return h('td', { class: 'num' }, fmtMs(v));
      case 'rate': return h('td', { class: 'num' }, fmtRate(v));
      case 'sla': return h('td', {}, r.hasSLAs ? badge(v ? 'pass' : 'fail', v ? 'ok' : 'bad') : muted('none'));
      default: return h('td', {}, String(v ?? ''));
    }
  }
  function draw() {
    const sorted = [...runs].sort((a, b) => {
      const x = a[sortKey], y = b[sortKey];
      if (x === y) return 0;
      if (sortKey === 'startedAt') return (new Date(x) - new Date(y)) * sortDir;
      return (x > y ? 1 : -1) * sortDir;
    });
    thead.replaceChildren(h('th', {}, h('span', { class: 'sr' }, 'Select')), ...COLS.map(([k, label, t]) => {
      const b = h('button', { type: 'button', class: 'sort' }, label + (sortKey === k ? (sortDir > 0 ? ' ▲' : ' ▼') : ''));
      b.addEventListener('click', () => { if (sortKey === k) sortDir = -sortDir; else { sortKey = k; sortDir = 1; } draw(); });
      return h('th', { class: ['num', 'ms', 'rate', 'clock'].includes(t) ? 'num' : '', 'aria-sort': sortKey === k ? (sortDir > 0 ? 'ascending' : 'descending') : 'none' }, b);
    }), h('th', {}, 'Actions'));
    tbody.replaceChildren();
    if (!sorted.length) tbody.append(h('tr', {}, h('td', { colspan: String(COLS.length + 2), class: 'muted' }, 'No runs yet. Start one from Run & Monitor.')));
    for (const r of sorted) {
      const cb = h('input', { type: 'checkbox', 'aria-label': 'Select run ' + r.runId, checked: picked.includes(r.runId) });
      cb.addEventListener('change', () => {
        if (cb.checked) { picked.push(r.runId); if (picked.length > 2) picked.shift(); } else picked.splice(picked.indexOf(r.runId), 1);
        cmpBtn.disabled = picked.length !== 2;
        draw();
      });
      tbody.append(h('tr', {}, h('td', {}, cb), COLS.map((c) => cell(r, c)),
        h('td', {}, h('div', { class: 'actions' },
          button('Summary', async () => {
            const s = await api('GET', `/api/runs/${enc(r.runId)}`);
            out.replaceChildren(renderSummary(s)); out.scrollIntoView({ block: 'start' });
          }, { small: true }),
          h('a', { class: 'btn small', href: `/api/runs/${enc(r.runId)}/report`, target: '_blank', rel: 'noopener' }, 'Report'),
          h('a', { class: 'btn small', href: `/api/runs/${enc(r.runId)}/summary.csv`, download: '' }, 'CSV'),
          h('a', { class: 'btn small', href: `/api/runs/${enc(r.runId)}/summary.json`, download: '' }, 'JSON')))));
    }
    cmpBtn.disabled = picked.length !== 2;
  }
  draw();
  root.replaceChildren(pageHead('Results', 'Finished and running tests. Select two runs to compare them.', cmpBtn,
    button('Refresh', async () => { runs = await api('GET', '/api/runs'); draw(); }, {})),
  h('div', { class: 'tw' }, h('table', {}, h('caption', { class: 'sr' }, 'Runs'), h('thead', {}, thead), tbody)), out);
}

export { downloads, note, toast };
