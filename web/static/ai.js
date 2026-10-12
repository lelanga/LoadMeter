// AI Assistant: plan/confirm/execute chat and activity log.
import { api } from './api.js';
import { h, button, field, badge, note, loading, pageHead, enc, renderValue, jsonPre, dataTable, fmtNum, toast } from './ui.js';

function planView(res) {
  const box = h('div', {});
  if (res.message) box.append(h('p', {}, res.message));
  const steps = (res.plan && res.plan.steps) || [];
  if (steps.length) {
    box.append(h('h3', {}, 'Plan'), h('ol', {}, steps.map((s) => h('li', {},
      h('code', {}, s.tool || ''), ' ', s.needsConfirmation ? badge('needs confirmation', 'warn') : badge('read-only / safe', 'ok'),
      s.why ? h('div', { class: 'muted' }, s.why) : null,
      s.args && Object.keys(s.args).length ? h('details', {}, h('summary', {}, 'Arguments'), jsonPre(s.args)) : null))));
  }
  const ex = res.executed || [];
  if (ex.length) {
    box.append(h('h3', {}, 'Executed'), ex.map((e) => {
      const links = [];
      const r = e.result;
      if (r && typeof r === 'object') {
        if (r.runId) links.push(h('a', { class: 'btn small', href: '#/run/' + enc(r.runId) }, 'Open run ' + r.runId));
        if (r.file) links.push(h('a', { class: 'btn small', href: '#/editor/' + enc(r.file) }, 'Open ' + r.file));
      }
      return h('div', { class: 'note ' + (e.ok ? 'ok' : 'bad') }, h('strong', {}, e.tool || ''), ' ', badge(e.ok ? 'ok' : 'failed', e.ok ? 'ok' : 'bad'),
        h('details', {}, h('summary', {}, 'Result'), renderValue(e.result)), links.length ? h('div', { class: 'actions' }, links) : null);
    }));
  }
  return box;
}

export async function renderAI(root) {
  root.replaceChildren(loading());
  const [ai, scenarios] = await Promise.all([api('GET', '/api/settings/ai').catch(() => null), api('GET', '/api/scenarios').catch(() => [])]);
  const configured = ai && ai.enabled && (ai.providers || []).length > 0;
  const log = h('div', { class: 'chat', role: 'log', 'aria-live': 'polite', 'aria-label': 'Conversation' });
  const prompt = h('textarea', { rows: '3', placeholder: 'e.g. "Import my last HAR, fix correlations and run a 2 minute test with 20 users"', 'aria-label': 'Message to the assistant' });
  const file = h('select', { 'aria-label': 'Scenario context' }, h('option', { value: '' }, 'No scenario selected'), scenarios.map((s) => h('option', { value: s.file }, s.name || s.file)));
  const activity = h('div', {});

  async function ask(text, confirm, slot) {
    const body = { prompt: text };
    if (file.value) body.file = file.value;
    if (confirm) body.confirm = true;
    const res = await api('POST', '/api/ai/ask', body);
    const bubble = slot || h('div', { class: 'bubble ai' });
    bubble.replaceChildren(planView(res));
    if (res.needsConfirmation) {
      bubble.append(note('warn', 'Some steps change things or start a test and need your confirmation before they run.'),
        button('Confirm and run', async () => { await ask(text, true, bubble); }, { kind: 'primary' }));
    }
    if (!slot) log.append(bubble);
    bubble.scrollIntoView({ block: 'nearest' });
    loadActivity();
  }
  const send = button('Ask', async () => {
    const t = prompt.value.trim();
    if (!t) throw new Error('Type a question or task first.');
    log.append(h('div', { class: 'bubble user' }, t));
    prompt.value = '';
    await ask(t, false);
  }, { kind: 'primary' });
  prompt.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send.click(); } });

  async function loadActivity() {
    try {
      const rows = await api('GET', '/api/ai/activity');
      const tin = rows.reduce((a, r) => a + (r.inputTokens || 0), 0), tout = rows.reduce((a, r) => a + (r.outputTokens || 0), 0), cost = rows.reduce((a, r) => a + (r.costUSD || 0), 0);
      activity.replaceChildren(rows.length
        ? h('div', {}, h('p', {}, `${rows.length} recent call(s): ${fmtNum(tin)} input tokens, ${fmtNum(tout)} output tokens, about $${cost.toFixed(4)}.`),
          dataTable(rows, { columns: ['time', 'task', 'provider', 'model', 'inputTokens', 'outputTokens', 'costUSD', 'redactions'] }))
        : h('p', { class: 'muted' }, 'No AI calls yet.'));
    } catch (e) { activity.replaceChildren(h('p', { class: 'muted' }, 'Activity unavailable: ' + e.message)); }
  }

  root.append(pageHead('AI Assistant', 'Describe what you want; the assistant proposes a plan. Nothing risky runs until you confirm.'),
    configured ? null : note('warn', h('strong', {}, 'AI is not configured. '), 'Load Meter works fully without AI: you can import, edit, validate and run tests yourself. To use the assistant, ',
      h('a', { href: '#/settings' }, 'turn on AI in Settings'), ' and add a provider (a local Ollama model keeps all data on your computer).'),
    h('div', { class: 'card' }, log, h('div', { class: 'form-grid' }, field('Message', prompt, 'Ctrl+Enter to send', 'wide'), field('Scenario context (optional)', file)),
      h('div', { class: 'actions', style: { marginTop: '.6rem' } }, send)),
    h('div', { class: 'card' }, h('h2', {}, 'AI activity'), h('p', { class: 'hint' }, 'Every call is logged. "Redactions" counts secrets and personal values removed before text was sent to a provider.'), activity));
  loadActivity();
}

export { toast };
