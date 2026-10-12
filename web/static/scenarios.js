// Projects / Scenarios: list and welcome (first-run) state.
import { api } from './api.js';
import { h, button, field, badge, note, loading, pageHead, enc, modal, confirmDialog, toast, fmtTime, muted } from './ui.js';
import { startRun, setPreselect } from './run.js';

export async function runSample() {
  const demo = await api('POST', '/api/demo');
  toast('Sample shop is ready: ' + (demo.mockUrl || ''), 'ok');
  const runId = await startRun(demo.file);
  setPreselect(demo.file);
  location.hash = '#/run/' + enc(runId);
}

async function newScenario(reload) {
  const name = h('input', { type: 'text', placeholder: 'my-test', 'aria-label': 'File name' });
  const v = await modal('New scenario', h('div', {}, field('File name (letters, digits, - and _)', name, 'Saved as <name>.json'),
    h('p', { class: 'hint' }, 'A starter scenario with one request is created; edit it afterwards.')),
  [{ label: 'Cancel', value: 'cancel' }, { label: 'Create', value: 'ok', kind: 'primary' }]);
  if (v !== 'ok') return;
  const base = name.value.trim().replace(/\.json$/, '');
  if (!/^[A-Za-z0-9_-]+$/.test(base)) { toast('Use letters, digits, - and _ only.', 'error'); return; }
  const file = base + '.json';
  const sc = { version: 'phoenix/v1', name: base, variables: { baseUrl: 'http://127.0.0.1:8080' }, defaults: { baseUrl: '${baseUrl}' },
    steps: [{ type: 'request', name: 'GET /', request: { method: 'GET', url: '/', assert: [{ type: 'status', value: '200' }] } }],
    workload: { executor: 'constant-vus', vus: 1, duration: '30s' } };
  await api('PUT', '/api/scenarios/' + enc(file), sc);
  location.hash = '#/editor/' + enc(file);
  if (reload) reload();
}

async function validateDialog(file) {
  const r = await api('POST', `/api/scenarios/${enc(file)}/validate`);
  const body = h('div', {},
    (r.problems || []).length ? note('warn', h('strong', {}, 'Problems'), h('ul', {}, r.problems.map((p) => h('li', {}, String(p))))) : note('ok', 'No problems found. The scenario is executable.'),
    (r.targets || []).length ? h('p', {}, 'Target hosts: ', (r.targets).map((t) => h('code', {}, t)).flatMap((c) => [c, ' '])) : null,
    (r.blocked || []).length ? note('bad', 'Blocked by the safety policy: ' + r.blocked.join(', ') + '. Add a host in Settings only if you are authorized to test it.') : null);
  await modal('Validation: ' + file, body);
}

const STEPS = ['Open sample', 'Import', 'Review correlations', 'Configure VUs', 'Validate', 'Run', 'Watch live metrics', 'Read the report'];

export async function renderScenarios(root, params, ctx) {
  root.replaceChildren(loading());
  const list = await api('GET', '/api/scenarios');
  const draw = () => renderScenarios(root, params, ctx);
  root.replaceChildren();
  const sampleBtn = () => button('Run sample test', runSample, { kind: 'primary', title: 'Starts a built-in mock shop and runs a test against it' });
  const importLink = () => h('a', { class: 'btn', href: '#/import' }, 'Import HAR / JMX / LoadRunner');

  if (!list.length) {
    root.append(pageHead('Welcome to Phoenix Performance', 'Lightweight load testing that runs on your computer.'),
      h('div', { class: 'card welcome' }, h('h2', {}, 'Start with a sample test'),
        h('p', {}, 'No scenarios yet. The sample starts a small mock shop on your machine and runs a ramping load against it. Nothing leaves your computer.'),
        h('div', { class: 'actions' }, sampleBtn(), importLink(), button('New empty scenario', () => newScenario(draw))),
        h('ol', { class: 'step-list', 'aria-label': 'First-run workflow' }, STEPS.map((s) => h('li', {}, s)))),
      note('info', 'Your first test: run the sample, then import your own recording (a HAR file from your browser works best), check the suggested correlations, set the number of users, validate, run, and read the report.'));
    return;
  }

  root.append(pageHead('Scenarios', ctx.session && ctx.session.workspace ? 'Project workspace: ' + ctx.session.workspace : 'Your saved test scenarios',
    sampleBtn(), importLink(), button('New scenario', () => newScenario(draw))));
  const body = h('tbody');
  for (const s of list) {
    body.append(h('tr', {},
      h('td', {}, h('a', { href: '#/editor/' + enc(s.file) }, s.name || s.file), h('div', { class: 'muted small' }, s.file),
        s.description ? h('div', { class: 'small beg-only' }, s.description) : null),
      h('td', { class: 'num' }, String(s.requests ?? '')), h('td', {}, badge(s.executor || '?', 'info')),
      h('td', { class: 'num' }, String(s.maxVUs ?? '')), h('td', {}, String(s.duration ?? '')),
      h('td', { class: 'adv-only' }, fmtTime(s.modified)),
      h('td', {}, h('div', { class: 'actions' },
        h('a', { class: 'btn small', href: '#/editor/' + enc(s.file) }, 'Open'),
        button('Validate', () => validateDialog(s.file), { small: true }),
        h('a', { class: 'btn small', href: '#/run?file=' + enc(s.file) }, 'Run'),
        button('Delete', async () => {
          if (!(await confirmDialog('Delete scenario?', `Delete "${s.name || s.file}" (${s.file})? A copy is kept in its history and can be restored.`, 'Delete', 'danger solid'))) return;
          await api('DELETE', '/api/scenarios/' + enc(s.file));
          toast('Deleted ' + s.file, 'ok');
          draw();
        }, { small: true, kind: 'danger' })))));
  }
  root.append(h('div', { class: 'tw' }, h('table', {},
    h('caption', { class: 'sr' }, 'Scenarios'),
    h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', { class: 'num' }, 'Requests'), h('th', {}, 'Executor'), h('th', { class: 'num' }, 'VUs'), h('th', {}, 'Duration'), h('th', { class: 'adv-only' }, 'Modified'), h('th', {}, 'Actions'))), body)),
  h('p', { class: 'hint beg-only', style: { marginTop: '.8rem' } }, 'VUs are virtual users: simulated people using your application at the same time.'));
}

export { muted };
