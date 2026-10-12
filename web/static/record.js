// Record page: start a recording (optionally launching a browser), mark
// transactions, watch captured requests live, then stop and create a scenario
// that opens in the correlation review.
import { api } from './api.js';
import { h, button, field, check, badge, note, pageHead, enc, toast, confirmDialog } from './ui.js';

const splitHosts = (s) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);

export async function renderRecord(root) {
  let timer = null;
  let stopped = false;
  const cleanup = () => { stopped = true; if (timer) clearTimeout(timer); timer = null; };

  const head = pageHead('Record', 'Record a user journey through a local proxy (HTTP and HTTPS) and turn it into a scenario.');
  const body = h('div', {});
  root.replaceChildren(head, body);

  const st = await api('GET', '/api/recordings/active');
  if (st.active) live(st.recording); else startForm();

  function startForm() {
    cleanup(); stopped = false;
    const launch = h('input', { type: 'checkbox', checked: true });
    const url = h('input', { type: 'url', placeholder: 'https://test.example.com/ (optional)' });
    const inc = h('input', { type: 'text', placeholder: 'e.g. shop.example.com, *.api.example.com' });
    const exc = h('input', { type: 'text', placeholder: 'e.g. cdn.example.com' });
    body.replaceChildren(h('div', { class: 'card' }, h('h2', {}, 'Start a recording'),
      check('Launch a browser for recording (Chrome, Chromium or Edge with a temporary profile; no certificate install needed)', launch),
      field('Start URL', url, 'Opened in the launched browser.'),
      field('Only record these hosts', inc, 'Optional. Exact host names or *.suffix, comma separated.'),
      field('Do not record these hosts', exc, 'Analytics and telemetry hosts are excluded automatically.'),
      note('info', 'Recording is not limited by the target allowlist because you browse manually. Before you load test the recorded scenario, add the recorded hosts to the allowlist (only systems you are authorized to test).'),
      note('warn', 'Recordings contain everything you send, including passwords and session cookies. They are stored in the workspace with owner-only permissions; the imported scenario replaces credentials with variables.'),
      h('div', { class: 'actions' }, button('Start recording', async () => {
        const rec = await api('POST', '/api/recordings', { browser: launch.checked, startUrl: url.value.trim(), includeHosts: splitHosts(inc.value), excludeHosts: splitHosts(exc.value) });
        toast(rec.browser ? 'Recording started; a browser window opened.' : 'Recording proxy started.', 'ok');
        live(rec);
      }, { kind: 'primary' }))));
  }

  function live(rec) {
    cleanup(); stopped = false;
    let since = 0;
    const count = h('strong', {}, String(rec.captured || 0));
    const txBadge = h('span', {});
    const exited = h('div', {});
    const tbody = h('tbody');
    const empty = h('tr', {}, h('td', { colspan: '5', class: 'muted' }, 'No requests captured yet. Use the browser to walk through the journey.'));
    tbody.append(empty);
    const setTx = (name) => txBadge.replaceChildren(name ? badge('Transaction: ' + name, 'ok') : badge('No transaction open', 'neutral'));
    setTx(rec.transaction);

    const txName = h('input', { type: 'text', placeholder: 'e.g. Login', maxlength: '100' });
    const mark = async (name) => {
      const r = await api('POST', `/api/recordings/${enc(rec.id)}/transaction`, { name });
      setTx(r.transaction);
      if (name) txName.value = '';
    };
    txName.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); startTx.click(); } });
    const startTx = button('Start transaction', async () => {
      const n = txName.value.trim();
      if (!n) throw new Error('Enter a transaction name first.');
      await mark(n);
    }, { kind: 'primary' });
    const endTx = button('End transaction', () => mark(''));

    const scName = h('input', { type: 'text', placeholder: 'e.g. checkout-journey' });
    const stopTo = button('Stop & create scenario', async () => {
      const name = scName.value.trim();
      if (!name) throw new Error('Enter a scenario name first.');
      const res = await api('POST', `/api/recordings/${enc(rec.id)}/stop`, { toScenario: name });
      cleanup();
      if (res.note) toast(res.note, 'info', 15000);
      toast(`Captured ${res.captured} requests. Scenario ${res.import.file} created.`, 'ok');
      location.hash = '#/correlate/' + enc(res.import.file);
    }, { kind: 'primary' });
    const stopOnly = button('Stop and keep HAR only', async () => {
      if (!(await confirmDialog('Stop recording?', 'The recording is saved as a HAR file in the workspace; no scenario is created.', 'Stop'))) return;
      const res = await api('POST', `/api/recordings/${enc(rec.id)}/stop`, {});
      cleanup();
      toast(`Saved ${res.captured} requests to ${res.harFile}`, 'ok', 10000);
      startForm();
    });

    body.replaceChildren(
      h('div', { class: 'card' }, h('h2', {}, 'Recording'),
        h('div', { class: 'tools' }, badge('Recording', 'warn'), h('span', {}, 'Captured requests: ', count), txBadge),
        h('p', { class: 'small muted' }, 'Proxy ', h('code', {}, 'http://' + rec.proxyAddr), ' (loopback only). Auto-stops at ', new Date(rec.autoStopAt).toLocaleTimeString(), '.'),
        rec.browser ? null : note('info', 'Set ', h('code', {}, 'http://' + rec.proxyAddr), ' as the HTTP and HTTPS proxy of your browser. For HTTPS sites the browser must trust the recording CA ', h('code', {}, rec.caCertPath), ' (run ', h('code', {}, 'phoenix record ca'), ' for instructions).'),
        exited),
      h('div', { class: 'cols' },
        h('div', { class: 'card' }, h('h2', {}, 'Transactions'),
          h('p', { class: 'hint' }, 'Name each business step before you perform it (for example Login, Search, Checkout). Requests captured while a transaction is open are grouped under its name.'),
          field('Transaction name', txName), h('div', { class: 'actions' }, startTx, endTx)),
        h('div', { class: 'card' }, h('h2', {}, 'Finish'),
          field('Scenario name', scName),
          h('div', { class: 'actions' }, stopTo, stopOnly),
          h('p', { class: 'hint' }, 'Creates the scenario and opens the correlation review for dynamic values such as tokens and session ids.'))),
      h('div', { class: 'card' }, h('h2', {}, 'Captured requests'),
        h('div', { class: 'tw' }, h('table', {}, h('thead', {}, h('tr', {}, ['Time', 'Transaction', 'Method', 'Status', 'URL'].map((t) => h('th', {}, t)))), tbody)),
        h('p', { class: 'small muted' }, 'Query strings are hidden here. Static files are kept in the recording and filtered when the scenario is created.')));

    async function poll() {
      if (stopped) return;
      try {
        const r = await api('GET', '/api/recordings/active?since=' + since);
        if (stopped) return;
        if (!r.active) { toast('The recording has ended.', 'info'); startForm(); return; }
        const a = r.recording;
        count.textContent = String(a.captured);
        setTx(a.transaction);
        if (a.browserExited && !exited.childElementCount) exited.append(note('warn', 'The recording browser was closed. Stop the recording to save it.'));
        for (const q of a.requests || []) {
          if (empty.parentNode) empty.remove();
          since = Math.max(since, q.seq);
          tbody.prepend(h('tr', {}, h('td', {}, q.time), h('td', {}, q.transaction || ''), h('td', {}, q.method),
            h('td', {}, badge(String(q.status), q.status >= 400 ? 'bad' : q.status >= 300 ? 'neutral' : 'ok')), h('td', { style: { wordBreak: 'break-all' } }, q.url)));
        }
        while (tbody.childElementCount > 500) tbody.lastElementChild.remove();
      } catch (e) {
        if (!stopped) toast(e.message || String(e), 'error');
      }
      if (!stopped) timer = setTimeout(poll, 1000);
    }
    poll();
  }

  return cleanup;
}
