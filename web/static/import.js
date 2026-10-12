// Import page: HAR, JMX, LoadRunner.
import { api } from './api.js';
import { h, button, field, check, badge, note, pageHead, enc, statusKind, renderValue, bar, toast } from './ui.js';
import { correlationPanel } from './correlate.js';

const STATUSES = ['supported', 'partial', 'unsupported', 'ignored'];

export function reportView(report) {
  const root = h('div', { class: 'card' }, h('h2', {}, 'Compatibility report'));
  const items = report.items || [];
  const counts = {};
  for (const i of items) counts[i.status] = (counts[i.status] || 0) + 1;
  let filter = '';
  let q = '';
  const tbody = h('tbody');
  const more = h('div', {});
  const LIMIT = 300;
  let all = false;
  function draw() {
    tbody.replaceChildren(); more.replaceChildren();
    const rows = items.filter((i) => (!filter || i.status === filter) && (!q || JSON.stringify(i).toLowerCase().includes(q)));
    (all ? rows : rows.slice(0, LIMIT)).forEach((i) => tbody.append(h('tr', {},
      h('td', {}, i.element || ''), h('td', {}, i.name || ''), h('td', {}, i.location || ''), h('td', {}, badge(i.status || '?', statusKind(i.status))), h('td', {}, i.note || ''))));
    if (!rows.length) tbody.append(h('tr', {}, h('td', { colspan: '5', class: 'muted' }, 'No items match.')));
    if (!all && rows.length > LIMIT) more.append(h('p', { class: 'muted small' }, `Showing ${LIMIT} of ${rows.length}. `, h('button', { type: 'button', class: 'btn small', onClick: () => { all = true; draw(); } }, 'Show all')));
  }
  const chips = h('div', { class: 'actions', role: 'group', 'aria-label': 'Filter by status' });
  const mkChip = (st, label) => {
    const b = h('button', { type: 'button', class: 'btn small', 'aria-pressed': String(filter === st) }, label);
    b.addEventListener('click', () => { filter = st; chips.querySelectorAll('button').forEach((x) => x.setAttribute('aria-pressed', String(x === b))); draw(); });
    return b;
  };
  chips.append(mkChip('', `All (${items.length})`));
  for (const s of STATUSES) chips.append(mkChip(s, `${s} (${counts[s] || 0})`));
  const search = h('input', { type: 'search', placeholder: 'Filter text...', 'aria-label': 'Filter report items', onInput: (e) => { q = e.target.value.toLowerCase(); draw(); } });
  const conf = report.confidence;
  root.append(h('div', { class: 'tools' },
    h('span', {}, `Source: ${report.source || ''} (${report.format || ''})`),
    conf !== undefined && conf !== null ? h('span', {}, 'Confidence: ', typeof conf === 'number' && conf <= 1 ? bar(conf) : String(conf)) : null),
  h('div', { class: 'tools' }, STATUSES.map((s) => badge(`${s}: ${counts[s] || 0}`, statusKind(s)))),
  h('div', { class: 'tools' }, chips, h('div', { style: { minWidth: '200px' } }, search)),
  h('div', { class: 'tw' }, h('table', {}, h('thead', {}, h('tr', {}, ['Element', 'Name', 'Location', 'Status', 'Note'].map((t) => h('th', {}, t)))), tbody)), more);
  draw();
  if ((report.warnings || []).length) root.append(note('warn', h('strong', {}, 'Warnings'), h('ul', {}, report.warnings.map((w) => h('li', {}, String(w))))));
  if ((report.manualChanges || []).length) root.append(note('info', h('strong', {}, 'Manual changes needed'), h('ul', {}, report.manualChanges.map((w) => h('li', {}, String(w))))));
  if (report.validation) root.append(h('details', {}, h('summary', {}, 'Validation'), renderValue(report.validation)));
  return root;
}

function showResult(box, res, kind) {
  box.replaceChildren();
  box.append(h('div', { class: 'card' }, h('h2', {}, 'Imported'),
    h('p', {}, 'Saved as ', h('code', {}, res.file)),
    h('div', { class: 'actions' }, h('a', { class: 'btn primary', href: '#/editor/' + enc(res.file) }, 'Open scenario'),
      h('a', { class: 'btn', href: '#/run?file=' + enc(res.file) }, 'Go to Run & Monitor'))),
  res.report ? reportView(res.report) : null);
  if (kind === 'har') {
    box.append(h('h2', {}, 'Next: review correlations and parameters'),
      note('info', 'Dynamic values from the recording (tokens, session ids) must be replaced by extracted values, or the test will fail with a different user. Review each candidate; the explanation shows why it was suggested.'),
      correlationPanel(res.file, { candidates: res.correlations || [], parameters: res.parameters || [] }));
  }
  if (kind === 'lr' && res.analysis) box.append(h('div', { class: 'card' }, h('h2', {}, 'LoadRunner analysis'), renderValue(res.analysis)));
  box.scrollIntoView({ block: 'start' });
}

function fileInput(label, accept, extra = {}) {
  const i = h('input', { type: 'file', accept, ...extra });
  return { i, f: field(label, i) };
}

export async function renderImport(root) {
  const result = h('div', { 'aria-live': 'polite' });
  const nameFor = () => { const n = h('input', { type: 'text', placeholder: 'optional scenario name' }); return { n, f: field('Scenario name', n) }; };

  // HAR
  const har = fileInput('HAR file (.har, .json)', '.har,.json,application/json');
  const harName = nameFor();
  const stat = h('input', { type: 'checkbox' }), creds = h('input', { type: 'checkbox' });
  const harCard = h('div', { class: 'card' }, h('h2', {}, 'HAR recording'),
    h('p', { class: 'hint' }, 'Export a HAR from your browser developer tools (Network tab). Phoenix turns it into a scenario and finds dynamic values.'),
    har.f, harName.f, check('Include static files (images, CSS, scripts)', stat), h('br'), check('Keep credentials from the recording', creds),
    note('warn', 'By default, passwords, tokens, cookies and authorization headers are replaced with placeholders such as ${password}. Keep credentials only if you understand that they will be stored in plain text in the scenario file.'),
    button('Import HAR', async () => {
      if (!har.i.files[0]) throw new Error('Choose a HAR file first.');
      const fd = new FormData();
      fd.append('file', har.i.files[0]);
      if (harName.n.value) fd.append('name', harName.n.value);
      fd.append('includeStatic', String(stat.checked)); fd.append('keepCredentials', String(creds.checked));
      showResult(result, await api('POST', '/api/import/har', fd), 'har');
      toast('HAR imported', 'ok');
    }, { kind: 'primary' }));

  // JMX
  const jmx = fileInput('JMeter test plan (.jmx)', '.jmx,.xml');
  const jmxName = nameFor();
  const jmxCard = h('div', { class: 'card' }, h('h2', {}, 'JMeter JMX'),
    h('p', { class: 'hint' }, 'Imports the supported parts of a JMeter plan. Unsupported elements are listed in the report so nothing is silently dropped.'),
    jmx.f, jmxName.f,
    button('Import JMX', async () => {
      if (!jmx.i.files[0]) throw new Error('Choose a .jmx file first.');
      const fd = new FormData(); fd.append('file', jmx.i.files[0]); if (jmxName.n.value) fd.append('name', jmxName.n.value);
      showResult(result, await api('POST', '/api/import/jmx', fd), 'jmx'); toast('JMX imported', 'ok');
    }, { kind: 'primary' }));

  // LoadRunner
  const lrDir = h('input', { type: 'file', multiple: true, webkitdirectory: true, directory: true });
  const lrFiles = h('input', { type: 'file', multiple: true });
  const lrName = nameFor();
  const lrCard = h('div', { class: 'card' }, h('h2', {}, 'LoadRunner script'),
    h('p', { class: 'hint' }, 'Choose the whole script folder (Vuser script, .c, .h, .usr, .cfg, data files) or select several files.'),
    field('Script folder', lrDir), field('Or select files', lrFiles), lrName.f,
    button('Import LoadRunner', async () => {
      const files = [...(lrDir.files || []), ...(lrFiles.files || [])];
      if (!files.length) throw new Error('Choose a folder or files first.');
      const fd = new FormData(); files.forEach((f) => fd.append('files', f, f.name)); if (lrName.n.value) fd.append('name', lrName.n.value);
      showResult(result, await api('POST', '/api/import/loadrunner', fd), 'lr'); toast('LoadRunner script imported', 'ok');
    }, { kind: 'primary' }));

  root.append(pageHead('Import', 'Bring in an existing recording or test plan. Imported files are untrusted data; Phoenix shows everything as plain text.'),
    h('div', { class: 'cols' }, harCard, jmxCard, lrCard), result);
}
