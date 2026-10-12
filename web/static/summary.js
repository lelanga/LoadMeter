// Shared renderers for run metrics: tables, status distribution, SLA checks, summary view.
import { h, badge, fmtMs, fmtNum, fmtRate, fmtTime, fmtClock, renderValue, enc, isAdv, jsonPre, dataTable, muted } from './ui.js';
import { latencyChart, throughputChart, errorsChart } from './charts.js';

// Named entries may be {name, ...MetricSet} or {name, metrics:{...}}
const metricsOf = (n) => (n && n.metrics && typeof n.metrics === 'object' ? n.metrics : n) || {};
const nameOf = (n) => n.name ?? n.key ?? n.id ?? '';

export function namedTable(list, label) {
  const rows = Array.isArray(list) ? list : Object.entries(list || {}).map(([k, v]) => ({ name: k, ...v }));
  if (!rows.length) return h('p', { class: 'muted small' }, `No ${label.toLowerCase()} data yet.`);
  const cols = ['count', 'errors', 'errorRate', 'rps', 'avgMs', 'p50Ms', 'p95Ms', 'p99Ms', 'maxMs'];
  const head = [label, 'Count', 'Errors', 'Error %', 'Per s', 'Avg', 'p50', 'p95', 'p99', 'Max'];
  const body = h('tbody', {}, rows.map((n) => {
    const m = metricsOf(n);
    return h('tr', { class: m.errors > 0 && (m.errorRate || 0) > 5 ? 'bad-row' : null },
      h('td', {}, String(nameOf(n))),
      cols.map((c) => {
        const v = m[c];
        let t;
        if (c === 'errorRate') t = fmtRate(v);
        else if (c.endsWith('Ms')) t = fmtMs(v);
        else t = fmtNum(v, c === 'rps' ? 1 : 0);
        return h('td', { class: 'num' }, t);
      }));
  }));
  return h('div', { class: 'tw' }, h('table', {},
    h('caption', { class: 'sr' }, label + ' metrics'),
    h('thead', {}, h('tr', {}, head.map((t, i) => h('th', { class: i ? 'num' : '' }, t)))), body));
}

export function statusBars(statusMap) {
  const entries = Object.entries(statusMap || {}).sort((a, b) => a[0].localeCompare(b[0]));
  if (!entries.length) return h('p', { class: 'muted small' }, 'No responses yet.');
  const total = entries.reduce((a, [, n]) => a + Number(n), 0) || 1;
  return h('div', {}, entries.map(([code, n]) => {
    const cls = 's' + (/^[2-5]/.test(code) ? code[0] : '0');
    const fill = h('span', { class: cls });
    fill.style.width = ((Number(n) / total) * 100).toFixed(1) + '%';
    return h('div', { class: 'hbar' }, h('strong', {}, code), h('div', { class: 'track' }, fill), h('span', { class: 'num' }, fmtNum(n)));
  }));
}

// Evaluate a scenario SLA against a snapshot. Returns {ok:boolean|null, actual}.
export function evalSLA(sla, snap) {
  if (!snap || !snap.requests) return { ok: null, actual: null };
  let m = snap.requests;
  if (sla.scope) {
    const pool = [].concat(snap.byTransaction || [], snap.byRequest || []);
    const f = pool.find((n) => nameOf(n) === sla.scope);
    if (!f) return { ok: null, actual: null };
    m = metricsOf(f);
  }
  let actual;
  switch (sla.metric) {
    case 'p50': actual = m.p50Ms; break;
    case 'p90': actual = m.p90Ms; break;
    case 'p95': actual = m.p95Ms; break;
    case 'p99': actual = m.p99Ms; break;
    case 'avg': actual = m.avgMs; break;
    case 'max': actual = m.maxMs; break;
    case 'error_rate': actual = m.errorRate || 0; break; // both in percent
    case 'rps': actual = m.rps; break;
    default: return { ok: null, actual: null };
  }
  if (actual === undefined || actual === null) return { ok: null, actual: null };
  const t = Number(sla.threshold);
  const ok = { '<': actual < t, '<=': actual <= t, '>': actual > t, '>=': actual >= t }[sla.op];
  return { ok: ok === undefined ? null : ok, actual };
}

export const slaLabel = (s) => `${s.scope ? s.scope + ' ' : ''}${s.metric} ${s.op} ${s.threshold}${s.metric === 'error_rate' ? '%' : s.metric === 'rps' ? '/s' : ' ms'}`;

export function slaChips(slas, snap) {
  const box = h('div', { class: 'sla-row', 'aria-label': 'SLA status' });
  if (!slas || !slas.length) { box.append(h('span', { class: 'muted small' }, 'No SLAs defined for this scenario.')); return box; }
  for (const s of slas) {
    const r = evalSLA(s, snap);
    const kind = r.ok === null ? 'neutral' : r.ok ? 'ok' : 'bad';
    const state = r.ok === null ? 'no data' : r.ok ? 'OK' : 'BREACH';
    box.append(badge(`${slaLabel(s)}  •  ${r.actual === null ? 'n/a' : fmtNum(r.actual, 1)}  •  ${state}`, kind));
  }
  return box;
}

const kpiTile = (label, value, bad) => h('div', { class: 'kpi' + (bad ? ' bad' : '') }, h('div', { class: 'v' }, value), h('div', { class: 'l' }, label));

export function metricTiles(m) {
  const r = m.requests || {};
  return h('div', { class: 'kpis' },
    kpiTile('Requests', fmtNum(r.count)), kpiTile('Req/s', fmtNum(r.rps, 1)),
    kpiTile('Avg', fmtMs(r.avgMs)), kpiTile('p50', fmtMs(r.p50Ms)), kpiTile('p95', fmtMs(r.p95Ms)), kpiTile('p99', fmtMs(r.p99Ms)),
    kpiTile('Max', fmtMs(r.maxMs)), kpiTile('Error rate', fmtRate(r.errorRate), r.errors > 0),
    kpiTile('Failed iterations', fmtNum(m.iterationFailures), m.iterationFailures > 0));
}

export function downloads(runId) {
  const base = `/api/runs/${enc(runId)}`;
  const a = (href, label, extra = {}) => h('a', { class: 'btn', href, ...extra }, label);
  return h('div', { class: 'actions' },
    a(`${base}/report`, 'Open HTML report', { target: '_blank', rel: 'noopener' }),
    a(`${base}/summary.csv`, 'Summary CSV', { download: '' }),
    a(`${base}/raw.csv`, 'Raw CSV', { download: '' }),
    a(`${base}/summary.json`, 'Summary JSON', { download: '' }));
}

export function renderSummary(s) {
  const m = s.metrics || {};
  const root = h('div', {});
  const passed = s.slaPassed;
  const hasSLAs = (s.slas || []).length > 0;
  root.append(h('div', { class: 'card' },
    h('div', { class: 'page-head' },
      h('div', {}, h('h2', {}, `Run ${s.runId}`), h('p', { class: 'muted' }, `${s.scenario || ''} • started ${fmtTime(s.startedAt)} • lasted ${fmtClock(s.durationSec)}`)),
      h('div', { class: 'actions' }, badge(String(s.status || 'unknown'), s.status === 'completed' || s.status === 'done' ? 'ok' : s.status === 'failed' ? 'bad' : 'info'),
        hasSLAs ? badge(passed ? 'SLAs passed' : 'SLAs breached', passed ? 'ok' : 'bad') : badge('no SLAs', 'neutral'))),
    metricTiles(m), downloads(s.runId)));

  if (hasSLAs) {
    root.append(h('div', { class: 'card' }, h('h2', {}, 'SLA results'),
      h('div', { class: 'tw' }, h('table', {},
        h('thead', {}, h('tr', {}, ['Result', 'Metric', 'Scope', 'Condition', 'Actual', 'Note'].map((t) => h('th', {}, t)))),
        h('tbody', {}, s.slas.map((x) => h('tr', { class: x.passed ? null : 'bad-row' },
          h('td', {}, badge(x.passed ? 'PASS' : 'FAIL', x.passed ? 'ok' : 'bad')), h('td', {}, x.metric), h('td', {}, x.scope || 'all'),
          h('td', {}, `${x.op} ${x.threshold}`), h('td', { class: 'num' }, fmtNum(x.actual, 2)), h('td', {}, x.note || ''))))))));
  }
  if ((s.findings || []).length) {
    root.append(h('div', { class: 'card' }, h('h2', {}, 'Findings'),
      h('p', { class: 'muted small' }, 'Observations are measured facts. Hypotheses are possible explanations that still need checking.'),
      s.findings.map((f) => h('div', { class: 'note ' + (f.severity === 'high' || f.severity === 'critical' ? 'bad' : f.severity === 'medium' || f.severity === 'warning' ? 'warn' : '') },
        badge(f.kind || 'observation', f.kind === 'hypothesis' ? 'warn' : 'info'), ' ', h('strong', {}, f.title || ''),
        f.severity ? h('span', { class: 'muted' }, ` (${f.severity})`) : null,
        f.evidence ? h('div', {}, renderValue(f.evidence)) : null))));
  }
  const tl = (m.timeline || []);
  if (tl.length) {
    const c1 = latencyChart(), c2 = throughputChart(), c3 = errorsChart();
    root.append(h('div', { class: 'charts' }, h('div', { class: 'card' }, c1.el), h('div', { class: 'card' }, c2.el), h('div', { class: 'card' }, c3.el)));
    c1.setData(tl); c2.setData(tl); c3.setData(tl);
  }
  root.append(h('div', { class: 'cols' },
    h('div', { class: 'card' }, h('h2', {}, 'Status codes'), statusBars((m.requests || {}).status),
      Object.keys((m.requests || {}).errorKinds || {}).length ? [h('h3', {}, 'Error kinds'), renderValue(m.requests.errorKinds)] : null),
    h('div', { class: 'card' }, h('h2', {}, 'Run details'),
      renderValue({ iterations: m.iterations, 'dropped iterations': m.droppedIterations, 'bytes in': (m.requests || {}).bytesIn, 'bytes out': (m.requests || {}).bytesOut }),
      isAdv() ? [h('h3', {}, 'Workload'), jsonPre(s.workload || {})] : null,
      s.environment ? [h('h3', {}, 'Environment'), renderValue(s.environment)] : null)));
  root.append(h('div', { class: 'card' }, h('h2', {}, 'Transactions'), namedTable(m.byTransaction, 'Transaction'),
    h('h2', { style: { marginTop: '1rem' } }, 'Requests'), namedTable(m.byRequest, 'Request')));
  if (s.baseline) root.append(h('div', { class: 'card' }, h('h2', {}, 'Baseline'), renderValue(s.baseline)));
  return root;
}

export { dataTable, muted };
