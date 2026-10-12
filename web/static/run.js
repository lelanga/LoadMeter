// Run & Monitor: start form, live dashboard over SSE, stop button, final summary.
import { api } from './api.js';
import {
  h, button, field, badge, note, loading, pageHead, enc, modal, confirmDialog, fmtMs, fmtNum, fmtRate, fmtClock, isAdv, plannedSeconds, parseDur, muted, toast,
} from './ui.js';
import { latencyChart, throughputChart, errorsChart } from './charts.js';
import { namedTable, statusBars, slaChips, renderSummary } from './summary.js';

// POST /api/runs with the 409 / 403 flows. Returns runId.
export async function startRun(file, overrides, baseline) {
  const body = { file };
  if (overrides && Object.keys(overrides).length) body.overrides = overrides;
  if (baseline) body.baseline = baseline;
  try {
    return (await api('POST', '/api/runs', body)).runId;
  } catch (e) {
    if (e.status === 409 && e.data && e.data.needsConfirmation) {
      const ok = await confirmDialog('This test targets a non-local system',
        h('div', {}, h('p', {}, e.message),
          note('warn', 'Load testing generates heavy traffic. Only continue if you own this system or have explicit written permission to test it. Testing systems you do not control can disrupt services and may be illegal.')),
        'I am authorized, start the test', 'danger solid');
      if (!ok) { e.handled = true; throw e; }
      body.confirm = true;
      return (await api('POST', '/api/runs', body)).runId;
    }
    if (e.status === 403) {
      await modal('Host not allowlisted', h('div', {}, h('p', {}, e.message),
        note('info', 'The safety policy only allows tests against hosts on the allowlist (local hosts are allowed by default). If, and only if, you are authorized to test this host, add it under Settings, Safety policy, and try again. Otherwise change the target of the scenario.'),
        h('p', {}, h('a', { href: '#/settings' }, 'Open Settings'))));
      e.handled = true; throw e;
    }
    throw e;
  }
}

let preselect = null;
export const setPreselect = (f) => { preselect = f; };

function kpi(label) {
  const v = h('div', { class: 'v' }, '–');
  const el = h('div', { class: 'kpi' }, v, h('div', { class: 'l' }, label));
  return { el, set: (t, bad) => { v.textContent = t; el.classList.toggle('bad', !!bad); } };
}

// Live dashboard. Returns {el, stop()}
function dashboard(runId, ctx) {
  const state = badge('connecting', 'info');
  const clock = h('span', { class: 'mono' }, '0:00');
  const prog = h('span', {});
  const progWrap = h('div', { class: 'progress', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': '0', 'aria-label': 'Run progress' }, prog);
  const tiles = {
    vus: kpi('Active VUs'), rps: kpi('Requests / s'), p50: kpi('p50'), p95: kpi('p95'), p99: kpi('p99'), err: kpi('Error rate'), ftx: kpi('Failed transactions'),
    fit: kpi('Failed iterations'), drop: kpi('Dropped iterations'), cpu: kpi('Generator CPU'), mem: kpi('Generator RSS'), total: kpi('Total requests'),
  };
  const slaBox = h('div', {});
  const codes = h('div', {});
  const txT = h('div', {}), rqT = h('div', {});
  const c1 = latencyChart(), c2 = throughputChart(), c3 = errorsChart();
  const result = h('div', { 'aria-live': 'polite' });
  let forced = false, es = null, finished = false;
  const history = new Map();

  const stopBtn = button('STOP TEST', async (b) => {
    await api('POST', `/api/runs/${enc(runId)}/stop`);
    if (!forced) {
      forced = true; b.textContent = 'FORCE STOP';
      toast('Stopping gracefully. Click again to force stop.', 'info');
    } else toast('Force stop requested.', 'info');
  }, { kind: 'danger solid big', aria: 'Stop the running test' });

  const el = h('div', {},
    h('div', { class: 'card' }, h('div', { class: 'stop-row' }, stopBtn,
      h('div', { class: 'grow' }, h('div', { class: 'tools' }, state, h('span', {}, 'Run ', h('code', {}, runId)), h('span', {}, 'Elapsed ', clock)), progWrap)),
    slaBox),
    h('div', { class: 'kpis' }, Object.values(tiles).map((t) => t.el)),
    h('div', { class: 'charts' }, h('div', { class: 'card' }, c1.el), h('div', { class: 'card' }, c2.el), h('div', { class: 'card' }, c3.el)),
    h('div', { class: 'cols' }, h('div', { class: 'card' }, h('h2', {}, 'Status codes'), codes)),
    h('div', { class: 'card' }, h('h2', {}, 'Transactions'), txT, h('h2', { style: { marginTop: '1rem' } }, 'Requests'), rqT),
    result);

  function update(s) {
    const r = s.requests || {};
    const planned = ctx.planned;
    state.textContent = 'running'; state.className = 'badge ok';
    clock.textContent = planned ? `${fmtClock(s.elapsedSec)} / ${fmtClock(planned)} (${fmtClock(Math.max(0, planned - s.elapsedSec))} left)` : fmtClock(s.elapsedSec);
    const pct = planned ? Math.min(100, (s.elapsedSec / planned) * 100) : 0;
    prog.style.width = pct.toFixed(0) + '%';
    progWrap.setAttribute('aria-valuenow', pct.toFixed(0));
    tiles.vus.set(fmtNum(s.activeVUs)); tiles.rps.set(fmtNum(r.rps, 1));
    tiles.p50.set(fmtMs(r.p50Ms)); tiles.p95.set(fmtMs(r.p95Ms)); tiles.p99.set(fmtMs(r.p99Ms));
    tiles.err.set(fmtRate(r.errorRate), r.errors > 0);
    const tx = s.transactions || {};
    tiles.ftx.set(fmtNum(tx.errors), tx.errors > 0); tiles.fit.set(fmtNum(s.iterationFailures), s.iterationFailures > 0);
    tiles.drop.set(fmtNum(s.droppedIterations), s.droppedIterations > 0); tiles.total.set(fmtNum(r.count));
    for (const p of s.timeline || []) history.set(p.t, p);
    const pts = [...history.values()].sort((a, b) => a.t - b.t);
    const last = pts[pts.length - 1];
    tiles.cpu.set(last ? fmtNum(last.cpuPercent, 0) + ' %' : '–'); tiles.mem.set(last ? fmtNum(last.memMB, 0) + ' MB' : '–');
    c1.setData(pts); c2.setData(pts); c3.setData(pts);
    slaBox.replaceChildren(h('h3', {}, 'SLA status'), slaChips(ctx.slas, s));
    codes.replaceChildren(statusBars(r.status));
    txT.replaceChildren(namedTable(s.byTransaction, 'Transaction'));
    rqT.replaceChildren(namedTable(s.byRequest, 'Request'));
  }

  async function finish(summary) {
    if (finished) return;
    finished = true;
    if (es) es.close();
    const ok = summary && (summary.status === 'completed' || summary.status === 'done' || summary.status === 'passed');
    state.textContent = summary ? String(summary.status || 'finished') : 'finished';
    state.className = 'badge ' + (ok ? 'ok' : summary && summary.status === 'failed' ? 'bad' : 'warn');
    stopBtn.hidden = true;
    prog.style.width = '100%';
    result.replaceChildren(summary ? renderSummary(summary) : muted('Run finished.'));
    result.scrollIntoView({ block: 'start', behavior: 'smooth' });
  }

  function connect() {
    es = new EventSource(`/api/runs/${enc(runId)}/live`);
    es.addEventListener('snapshot', (ev) => { try { const d = JSON.parse(ev.data); if (d.plannedSec && !ctx.planned) ctx.planned = d.plannedSec; update(d.metrics || d); } catch (e) { console.warn('bad snapshot', e); } });
    es.addEventListener('done', (ev) => { let s = null; try { s = JSON.parse(ev.data); } catch { /* ignore */ } finish(s); });
    es.onerror = async () => {
      if (finished) return;
      state.textContent = 'reconnecting'; state.className = 'badge warn';
      // maybe the run is already over: check the stored summary
      try {
        const s = await api('GET', `/api/runs/${enc(runId)}`);
        if (s && s.status && s.status !== 'running') finish(s);
      } catch { /* keep retrying */ }
    };
  }
  connect();
  return { el, stop() { finished = true; if (es) es.close(); } };
}

async function plannedFor(file, overrides) {
  try {
    const sc = await api('GET', '/api/scenarios/' + enc(file));
    const w = JSON.parse(JSON.stringify(sc.workload || {}));
    if (overrides && overrides.duration && w.executor !== 'ramping-vus' && w.executor !== 'ramping-arrival-rate') w.duration = overrides.duration;
    return { planned: plannedSeconds(w), slas: sc.slas || [], name: sc.name };
  } catch { return { planned: 0, slas: [] }; }
}

export async function renderRun(root, params, ctx = {}) {
  root.replaceChildren(loading());
  const [scenarios, runs, active] = await Promise.all([
    api('GET', '/api/scenarios'), api('GET', '/api/runs').catch(() => []), api('GET', '/api/runs/active').catch(() => ({})),
  ]);
  let cleanup = null;
  const dash = h('div', { 'aria-live': 'off' });

  const wantFile = (ctx.query && ctx.query.file) || preselect;
  preselect = null;
  const sc = h('select', { 'aria-label': 'Scenario' }, scenarios.map((s) => h('option', { value: s.file }, `${s.name || s.file} (${s.file})`)));
  if (wantFile && scenarios.some((s) => s.file === wantFile)) sc.value = wantFile;
  const vus = h('input', { type: 'number', min: 1, placeholder: 'scenario default' });
  const dur = h('input', { type: 'text', placeholder: 'e.g. 2m' });
  const ramp = h('input', { type: 'text', placeholder: 'e.g. 30s' });
  const rate = h('input', { type: 'number', min: 0, step: 'any', placeholder: 'iterations/s' });
  const maxv = h('input', { type: 'number', min: 1, placeholder: 'pool size' });
  const base = h('select', { 'aria-label': 'Baseline run' }, h('option', { value: '' }, 'No baseline'),
    runs.map((r) => h('option', { value: r.runId }, `${r.runId} • ${r.scenario || ''} • p95 ${fmtMs(r.p95Ms)}`)));

  function overrides() {
    const o = {};
    if (vus.value) o.vus = Number(vus.value);
    if (dur.value.trim()) o.duration = dur.value.trim();
    if (ramp.value.trim()) o.rampUp = ramp.value.trim();
    if (rate.value) o.rate = Number(rate.value);
    if (maxv.value) o.maxVUs = Number(maxv.value);
    return o;
  }

  async function attach(runId, file, ov) {
    if (cleanup) cleanup();
    dash.replaceChildren(loading('Starting live view...'));
    const info = await plannedFor(file, ov);
    const d = dashboard(runId, info);
    cleanup = d.stop;
    dash.replaceChildren(h('h2', {}, info.name ? `Live: ${info.name}` : 'Live run'), d.el);
    if (location.hash !== '#/run/' + enc(runId)) history.replaceState(null, '', '#/run/' + enc(runId));
  }

  const form = h('div', { class: 'card' }, h('h2', {}, 'Start a test'),
    scenarios.length ? null : note('warn', 'There are no scenarios yet. Run the sample test or import a recording first.'),
    h('div', { class: 'form-grid' }, field('Scenario', sc, null, 'wide'),
      field('Virtual users', vus, 'Leave blank to use the scenario value.'),
      field('Duration', dur), field('Ramp-up', ramp, 'Time to reach full load.'),
      field('Rate (iterations/s)', rate, 'Only for arrival-rate scenarios.', 'adv-only'),
      field('Max VUs', maxv, null, 'adv-only'),
      field('Compare with baseline', base, 'The new run is checked against this earlier run.')),
    h('p', { class: 'hint beg-only' }, 'Tip: validate first (Scenarios, Validate) and run a short test before a long one.'),
    h('div', { class: 'actions', style: { marginTop: '.8rem' } },
      button('Validate', async () => {
        const r = await api('POST', `/api/scenarios/${enc(sc.value)}/validate`);
        const body = h('div', {}, (r.problems || []).length ? note('warn', h('ul', {}, r.problems.map((p) => h('li', {}, String(p))))) : note('ok', 'No problems found.'),
          (r.targets || []).length ? h('p', {}, 'Targets: ', r.targets.join(', ')) : null,
          (r.blocked || []).length ? note('bad', 'Blocked hosts: ' + r.blocked.join(', ') + '. Add a host in Settings only if you are authorized to test it.') : null);
        await modal('Validation', body);
      }),
      button('Start test', async () => {
        if (!sc.value) throw new Error('Choose a scenario first.');
        const ov = overrides();
        const id = await startRun(sc.value, ov, base.value);
        await attach(id, sc.value, ov);
      }, { kind: 'primary' })));

  root.replaceChildren(pageHead('Run & Monitor', 'Start a test and watch it live.'), form, dash);

  if (params.id) {
    const s = await api('GET', `/api/runs/${enc(params.id)}`).catch(() => null);
    if (s && s.status && s.status !== 'running') {
      dash.replaceChildren(renderSummary(s));
    } else {
      await attach(params.id, wantFile || (s && s.scenarioFile) || sc.value, {});
    }
  } else if (active && active.runId) {
    dash.replaceChildren(note('info', `A run is already active (${active.runId}${active.scenario ? ', ' + active.scenario : ''}). Showing it live.`));
    const f = scenarios.find((s) => s.file === active.scenario || s.name === active.scenario);
    await attach(active.runId, f ? f.file : sc.value, {});
  }
  return () => { if (cleanup) cleanup(); };
}

export { parseDur, isAdv };
