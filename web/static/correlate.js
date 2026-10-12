// Correlation review and parameterization suggestions.
import { api } from './api.js';
import { h, button, field, check, badge, note, loading, pageHead, enc, bar, renderValue, muted, isAdv, toast } from './ui.js';

const secKind = { secret: 'bad', session: 'warn', identifier: 'info', other: 'neutral' };
const secHelp = {
  secret: 'Secret: a credential or key. Never print or share it.',
  session: 'Session: identifies a logged-in session.',
  identifier: 'Identifier: an object id or similar.',
  other: 'Other dynamic value.',
};

function describeExtractor(x) {
  if (!x) return '';
  if (typeof x === 'string') return x;
  return [x.type, x.expr || x.name || x.left].filter(Boolean).join(' ');
}
const where = (l) => (l && typeof l === 'object' ? [l.step, l.location].filter(Boolean).join(' · ') : String(l ?? ''));

export const POLICIES = [
  ['suggest', 'Suggest only (apply nothing automatically)'],
  ['validated', 'Apply only changes that pass validation'],
  ['manual', 'Manual: apply exactly the ones I tick'],
  ['auto', 'Automatic: apply all high-confidence changes'],
];

function candidatesCard(file, candidates, dynamicUnexplained) {
  const card = h('div', { class: 'card' }, h('h2', {}, 'Correlation candidates'),
    h('p', { class: 'hint' }, 'A correlation replaces a recorded dynamic value (token, id, session) with a value extracted from an earlier response, so the test works for every virtual user instead of replaying one recorded value.'));
  if (!candidates.length) {
    card.append(note('info', 'No correlation candidates found.'));
    return card;
  }
  const checks = new Map();
  const body = h('tbody');
  for (const c of candidates) {
    const cb = h('input', { type: 'checkbox', 'aria-label': 'Select ' + c.variable, checked: (c.confidence || 0) >= 0.8 && c.validation !== 'failed' && c.security !== 'secret' });
    checks.set(c.id, cb);
    const ex = c.explanation || {};
    const det = h('tr', { hidden: true }, h('td', { colspan: '8' },
      h('dl', { class: 'kv' },
        h('dt', {}, 'What'), h('dd', {}, ex.what || '–'), h('dt', {}, 'Why'), h('dd', {}, ex.why || '–'),
        h('dt', {}, 'Risk'), h('dd', {}, ex.risk || '–'), h('dt', {}, 'How validated'), h('dd', {}, ex.howValidated || '–'),
        h('dt', {}, 'Manual review'), h('dd', {}, ex.manualReview || '–'),
        h('dt', {}, 'Replacement'), h('dd', {}, c.suggestedReplacement ? h('code', {}, c.suggestedReplacement) : '–'),
        isAdv() ? [h('dt', {}, 'Evidence'), h('dd', {}, renderValue(c.evidence))] : null)));
    const tog = h('button', { type: 'button', class: 'btn small', 'aria-expanded': 'false' }, 'Explain');
    tog.addEventListener('click', () => { det.hidden = !det.hidden; tog.setAttribute('aria-expanded', String(!det.hidden)); });
    body.append(h('tr', {},
      h('td', {}, cb), h('td', {}, h('code', {}, c.variable || '')), h('td', {}, h('code', {}, String(c.value ?? ''))),
      h('td', {}, where(c.source), ' → ', (c.destinations || []).length ? (c.destinations).map((d) => where(d)).join('; ') : 'nowhere'),
      h('td', { class: 'adv-only' }, describeExtractor(c.source && c.source.extractor)),
      h('td', {}, bar(c.confidence)),
      h('td', {}, h('span', { title: secHelp[c.security] || '' }, badge(c.security || 'other', secKind[c.security] || 'neutral')), ' ',
        badge('validation: ' + (c.validation || 'not-run'), c.validation === 'passed' ? 'ok' : c.validation === 'failed' ? 'bad' : 'neutral')),
      h('td', {}, tog)), det);
  }
  card.append(h('div', { class: 'tw' }, h('table', {},
    h('thead', {}, h('tr', {}, h('th', {}, 'Use'), h('th', {}, 'Variable'), h('th', {}, 'Value (masked)'), h('th', {}, 'Source → destinations'),
      h('th', { class: 'adv-only' }, 'Extraction'), h('th', {}, 'Confidence'), h('th', {}, 'Security / validation'), h('th', {}, 'Details'))),
    body)));
  if (dynamicUnexplained && dynamicUnexplained.length) {
    card.append(h('details', {}, h('summary', {}, `Dynamic values without a known source (${dynamicUnexplained.length})`),
      h('p', { class: 'hint' }, 'These look dynamic but no earlier response provides them. They may need manual handling.'), renderValue(dynamicUnexplained)));
  }
  const policy = h('select', { 'aria-label': 'Approval policy' }, POLICIES.map(([v, l]) => h('option', { value: v }, l)));
  policy.value = 'validated';
  const validate = h('input', { type: 'checkbox', checked: true });
  const results = h('div', { 'aria-live': 'polite' });
  const apply = button('Apply selected', async () => {
    const ids = candidates.filter((c) => checks.get(c.id).checked).map((c) => c.id);
    if (!ids.length) { toast('Select at least one candidate.', 'info'); return; }
    const res = await api('POST', `/api/scenarios/${enc(file)}/correlate/apply`, { ids, validate: validate.checked, policy: policy.value });
    results.replaceChildren(note('ok', `Applied ${(res.applied || []).length} of ${ids.length} selected correlation(s).`),
      res.validation ? h('div', {}, h('h3', {}, 'Validation'),
        note(res.validation.ok ? 'ok' : 'bad', res.validation.ok ? 'Validation passed: the corrected scenario runs end to end.' : 'Validation failed. Review the details below before running.'),
        renderValue(res.validation.details)) : null,
      h('p', {}, h('a', { class: 'btn primary', href: '#/editor/' + enc(res.file || file) }, 'Open scenario in editor')));
  }, { kind: 'primary' });
  card.append(h('div', { class: 'tools', style: { marginTop: '.8rem' } },
    field('Approval policy', policy), check('Validate by running one iteration (recommended)', validate), apply), results);
  return card;
}

function paramsCard(file, suggestions) {
  const card = h('div', { class: 'card' }, h('h2', {}, 'Parameterization suggestions'),
    h('p', { class: 'hint' }, 'Parameterization replaces recorded constants (such as one username) with values from a data file so each virtual user can use different data.'));
  if (!suggestions.length) { card.append(note('info', 'No parameterization suggestions.')); return card; }
  const checks = new Map();
  const body = h('tbody');
  for (const s of suggestions) {
    const cb = h('input', { type: 'checkbox', 'aria-label': 'Select ' + (s.column || s.id), checked: (s.confidence ?? 1) >= 0.8 });
    checks.set(s.id, cb);
    const extra = { ...s };
    for (const k of ['id', 'column', 'recordedValue', 'confidence', 'why']) delete extra[k];
    body.append(h('tr', {}, h('td', {}, cb), h('td', {}, h('code', {}, String(s.column ?? s.id ?? ''))),
      h('td', {}, h('code', {}, String(s.recordedValue ?? ''))), h('td', {}, String(s.why ?? '')),
      h('td', {}, s.confidence !== undefined ? bar(s.confidence) : ''),
      h('td', { class: 'adv-only' }, Object.keys(extra).length ? h('details', {}, h('summary', {}, 'more'), renderValue(extra)) : '')));
  }
  card.append(h('div', { class: 'tw' }, h('table', {}, h('thead', {}, h('tr', {}, ['Use', 'Variable', 'Recorded value', 'Why', 'Confidence'].map((t) => h('th', {}, t)), h('th', { class: 'adv-only' }, 'Details'))), body)));
  const rows = h('input', { type: 'number', min: 1, placeholder: 'e.g. 100' });
  const results = h('div', { 'aria-live': 'polite' });
  card.append(h('div', { class: 'tools', style: { marginTop: '.8rem' } }, field('Generated data rows (optional)', rows),
    button('Apply selected', async () => {
      const ids = suggestions.filter((s) => checks.get(s.id).checked).map((s) => s.id);
      if (!ids.length) { toast('Select at least one suggestion.', 'info'); return; }
      const body2 = { ids }; if (rows.value) body2.rows = Number(rows.value);
      const res = await api('POST', `/api/scenarios/${enc(file)}/parameterize/apply`, body2);
      results.replaceChildren(note('ok', `Applied ${(res.applied || []).length} parameter(s). Data file: ${res.dataFile || 'n/a'}.`),
        h('p', {}, h('a', { class: 'btn primary', href: '#/editor/' + enc(file) }, 'Open scenario in editor')));
    }, { kind: 'primary' })), results);
  return card;
}

// Embedded panel (used after HAR import with already-known data)
export function correlationPanel(file, data) {
  return h('div', {}, candidatesCard(file, data.candidates || [], data.dynamicUnexplained), paramsCard(file, data.parameters || []));
}

// Full view: #/correlate/<file>
export async function renderCorrelate(root, params) {
  const file = params.file;
  root.replaceChildren(pageHead('Review correlations', file, h('a', { class: 'btn', href: '#/editor/' + enc(file) }, 'Back to editor')), loading('Analyzing recording...'));
  const out = h('div', {});
  let corr = { candidates: [] }, par = { suggestions: [] };
  const errs = [];
  try { corr = await api('POST', `/api/scenarios/${enc(file)}/correlate`); } catch (e) { errs.push('Correlation analysis: ' + e.message); }
  try { par = await api('POST', `/api/scenarios/${enc(file)}/parameterize`); } catch (e) { errs.push('Parameterization analysis: ' + e.message); }
  root.replaceChildren(pageHead('Review correlations', file, h('a', { class: 'btn', href: '#/editor/' + enc(file) }, 'Back to editor')));
  if (errs.length) root.append(note('warn', h('strong', {}, 'Some analysis was not possible. '), errs.join(' '), ' Correlation analysis needs the recording that is saved when a HAR file is imported.'));
  out.append(correlationPanel(file, { candidates: corr.candidates, dynamicUnexplained: corr.dynamicUnexplained, parameters: par.suggestions }));
  root.append(out);
}

export { muted };
