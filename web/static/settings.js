// Settings: safety policy, AI configuration, audit log.
import { api } from './api.js';
import { h, button, sbtn, field, check, badge, note, loading, pageHead, tabs, toast, dataTable, renderValue, fmtTime } from './ui.js';

const KINDS = ['anthropic', 'openai', 'gemini', 'openrouter', 'ollama'];
const URL_HINT = {
  anthropic: 'https://api.anthropic.com', openai: 'https://api.openai.com/v1', gemini: 'https://generativelanguage.googleapis.com',
  openrouter: 'https://openrouter.ai/api/v1', ollama: 'http://127.0.0.1:11434',
};

// keep the original type (number or duration string) when saving
const typed = (orig, v) => (typeof orig === 'number' ? Number(v) : v);

async function policyTab(panel) {
  const p = await api('GET', '/api/settings/policy');
  const hosts = h('textarea', { rows: '5', class: 'mono', spellcheck: 'false', value: (p.allowedHosts || []).join('\n'), 'aria-label': 'Allowed hosts' });
  const maxVUs = h('input', { type: 'number', min: 0, value: p.maxVUs ?? '' });
  const maxRPS = h('input', { type: 'number', min: 0, value: p.maxRPS ?? '' });
  const maxDur = h('input', { type: 'text', value: p.maxDuration ?? '', placeholder: 'e.g. 30m' });
  const above = h('input', { type: 'number', min: 0, value: p.requireConfirmAbove ?? '' });
  panel.append(h('div', { class: 'card' }, h('h2', {}, 'Safety policy'),
    note('warn', h('strong', {}, 'Only add hosts you own or have written permission to test. '), 'Load testing a system you do not control can disrupt it, harm other people and may be illegal. Local addresses (127.0.0.1, localhost) are always safe to test.'),
    field('Allowed target hosts (one per line)', hosts, 'Scenarios may only send traffic to these hosts (plus local addresses).'),
    h('div', { class: 'form-grid', style: { marginTop: '.8rem' } },
      field('Max virtual users', maxVUs, 'Runs above this are refused.'), field('Max requests per second', maxRPS, 'Rate cap, 0 = unlimited if the server allows it.'),
      field('Max duration', maxDur, 'Longest allowed test, e.g. 30m.'), field('Ask for confirmation above (VUs)', above, 'A confirmation dialog is shown for larger runs.')),
    h('div', { class: 'actions', style: { marginTop: '.8rem' } }, button('Save safety policy', async () => {
      const body = {
        allowedHosts: hosts.value.split('\n').map((x) => x.trim()).filter(Boolean),
        maxVUs: Number(maxVUs.value || 0), maxRPS: Number(maxRPS.value || 0),
        maxDuration: typed(p.maxDuration, maxDur.value.trim()), requireConfirmAbove: Number(above.value || 0),
      };
      const res = await api('PUT', '/api/settings/policy', body);
      hosts.value = (res.allowedHosts || []).join('\n');
      toast('Safety policy saved', 'ok');
    }, { kind: 'primary' }))));
}

async function aiTab(panel) {
  let cfg = await api('GET', '/api/settings/ai');
  const draw = () => {
    panel.replaceChildren();
    const enabled = h('input', { type: 'checkbox', checked: !!cfg.enabled });
    const consent = h('input', { type: 'checkbox', checked: !!cfg.cloudConsent });
    const local = h('input', { type: 'checkbox', checked: !!cfg.localOnly });
    const providers = (cfg.providers = cfg.providers || []);
    const keyInputs = new Map();

    const provBox = h('div', {});
    const drawProviders = () => {
      provBox.replaceChildren();
      providers.forEach((pr, idx) => {
        const idIn = h('input', { type: 'text', value: pr.id || '', 'aria-label': 'Provider id', onInput: (e) => { pr.id = e.target.value; } });
        const kind = h('select', { 'aria-label': 'Provider kind' }, KINDS.map((k) => h('option', { value: k }, k)));
        kind.value = pr.kind || 'anthropic';
        const base = h('input', { type: 'text', value: pr.baseUrl || '', placeholder: URL_HINT[kind.value], 'aria-label': 'Base URL', onInput: (e) => { pr.baseUrl = e.target.value; } });
        kind.addEventListener('change', () => { pr.kind = kind.value; base.placeholder = URL_HINT[kind.value]; });
        const model = h('input', { type: 'text', value: pr.model || '', placeholder: 'model name', 'aria-label': 'Model', onInput: (e) => { pr.model = e.target.value; } });
        const key = h('input', { type: 'password', autocomplete: 'off', value: '', placeholder: pr.hasKey ? 'key stored (type to replace)' : 'paste API key', 'aria-label': 'API key (write-only)' });
        keyInputs.set(pr, key);
        provBox.append(h('div', { class: 'card' }, h('div', { class: 'form-grid' },
          field('Id', idIn), field('Kind', kind), field('Base URL', base, 'Leave empty for the default.'), field('Model', model),
          field('API key', key, pr.kind === 'ollama' ? 'Local models need no key.' : 'Write-only: stored by the Phoenix server, never shown again.')),
        h('div', { class: 'actions', style: { marginTop: '.5rem' } },
          pr.hasKey ? badge('key stored', 'ok') : badge('no key', 'neutral'),
          button('Test', async () => {
            await save(true);
            const r = await api('POST', '/api/ai/test', { provider: pr.id });
            if (r.ok) toast(`Provider ${pr.id} works: ${r.model || ''} (${r.latencyMs} ms)`, 'ok');
            else toast(`Provider ${pr.id} failed: ${r.error || 'unknown error'}`, 'error');
          }, { small: true }),
          sbtn('Remove', () => { providers.splice(idx, 1); drawProviders(); refreshRouting(); }, { kind: 'danger' }))));
      });
      provBox.append(sbtn('+ Add provider', () => { providers.push({ id: 'provider-' + (providers.length + 1), kind: 'anthropic', baseUrl: '', model: '' }); drawProviders(); refreshRouting(); }));
    };

    const light = h('select', { 'aria-label': 'Light-task provider' }), strong = h('select', { 'aria-label': 'Strong-task provider' });
    const refreshRouting = () => {
      for (const [s, cur] of [[light, light.value || (cfg.routing || {}).light], [strong, strong.value || (cfg.routing || {}).strong]]) {
        s.replaceChildren(h('option', { value: '' }, '(none)'), ...providers.map((p) => h('option', { value: p.id }, p.id)));
        s.value = cur || '';
      }
    };
    const lim = cfg.limits || {};
    const maxTok = h('input', { type: 'number', min: 0, value: lim.maxTokensPerCall ?? '' });
    const maxCalls = h('input', { type: 'number', min: 0, value: lim.maxCallsPerHour ?? '' });
    const budget = h('input', { type: 'number', min: 0, step: 'any', value: lim.monthlyBudgetUSD ?? '' });

    async function save(quiet) {
      const body = {
        enabled: enabled.checked, cloudConsent: consent.checked, localOnly: local.checked,
        providers: providers.map((p) => { const o = { id: p.id, kind: p.kind, baseUrl: p.baseUrl || '', model: p.model || '' }; const k = keyInputs.get(p)?.value; if (k) o.apiKey = k; return o; }),
        routing: { light: light.value, strong: strong.value },
        limits: { maxTokensPerCall: Number(maxTok.value || 0), maxCallsPerHour: Number(maxCalls.value || 0), monthlyBudgetUSD: Number(budget.value || 0) },
      };
      cfg = await api('PUT', '/api/settings/ai', body);
      if (!quiet) toast('AI settings saved', 'ok');
      if (!quiet) draw(); else { providers.length = 0; providers.push(...(cfg.providers || [])); }
    }

    panel.append(h('div', { class: 'card' }, h('h2', {}, 'AI assistant'),
      note('info', 'AI is optional. Phoenix imports, edits, runs and reports without it. The assistant only suggests plans; actions that change things need your confirmation.'),
      check('Enable the AI assistant', enabled), h('br'),
      check('I consent to sending redacted data to cloud providers', consent),
      h('p', { class: 'hint' }, 'What is sent: prompts and short excerpts of scenarios or results. Before sending, Phoenix redacts secrets, tokens, cookies, passwords and recognizable personal values. The cloud provider you configure receives the redacted text and handles it under its own terms. Without this consent only local providers can be used.'),
      check('Local-only mode (never contact cloud providers)', local),
      h('p', { class: 'hint' }, 'Use a local model such as Ollama to keep everything on this computer.')),
    h('div', { class: 'card' }, h('h2', {}, 'Providers'), provBox),
    h('div', { class: 'card' }, h('h2', {}, 'Routing'), h('p', { class: 'hint' }, 'Light tasks (short explanations) can use a cheaper model; strong tasks (planning, analysis) a more capable one.'),
      h('div', { class: 'form-grid' }, field('Light tasks use', light), field('Strong tasks use', strong))),
    h('div', { class: 'card' }, h('h2', {}, 'Limits'),
      h('div', { class: 'form-grid' }, field('Max tokens per call', maxTok), field('Max calls per hour', maxCalls), field('Monthly budget (USD)', budget))),
    h('div', { class: 'actions' }, button('Save AI settings', () => save(false), { kind: 'primary' })));
    drawProviders();
    light.value = (cfg.routing || {}).light || ''; strong.value = (cfg.routing || {}).strong || '';
    refreshRouting();
  };
  draw();
}

async function auditTab(panel) {
  const box = h('div', {});
  async function load() {
    box.replaceChildren(loading());
    const rows = await api('GET', '/api/audit');
    const list = Array.isArray(rows) ? [...rows] : [];
    if (list.length && list[0].time) list.sort((a, b) => new Date(b.time) - new Date(a.time));
    box.replaceChildren(list.length ? dataTable(list) : h('p', { class: 'muted' }, 'No audit events yet.'));
  }
  panel.append(h('div', { class: 'card' }, h('div', { class: 'page-head' }, h('div', {}, h('h2', {}, 'Audit log'), h('p', { class: 'muted' }, 'The last 200 security-relevant events (runs started, settings changed, hosts allowed).')),
    button('Refresh', load)), box));
  await load();
}

export async function renderSettings(root) {
  root.replaceChildren(pageHead('Settings', 'Safety limits, AI, and the audit log.'));
  const t = tabs([
    { id: 'policy', label: 'Safety policy', render: (p) => { p.append(loading()); policyTab(p).then(() => p.querySelector('.spinner-line')?.remove()).catch((e) => p.replaceChildren(note('bad', e.message))); } },
    { id: 'ai', label: 'AI', render: (p) => { p.append(loading()); aiTab(p).catch((e) => p.replaceChildren(note('bad', e.message))); } },
    { id: 'audit', label: 'Audit log', render: (p) => { auditTab(p).catch((e) => p.replaceChildren(note('bad', e.message))); } },
  ], 'policy');
  root.append(t.el);
}

export { renderValue, fmtTime };
