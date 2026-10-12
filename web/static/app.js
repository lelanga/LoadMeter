// Phoenix web UI entry point: session bootstrap, mode toggle and hash router.
import { initSession } from './api.js';
import { h, initMode, setMode, getMode, toast, errorBox, loading, confirmDialog } from './ui.js';
import { renderScenarios } from './scenarios.js';
import { renderEditor, isDirty, discard } from './editor.js';
import { renderCorrelate } from './correlate.js';
import { renderImport } from './import.js';
import { renderRun } from './run.js';
import { renderResults } from './results.js';
import { renderAI } from './ai.js';
import { renderSettings } from './settings.js';

const main = document.getElementById('main');
const title = document.getElementById('top-title');
const session = { token: '', version: '', workspace: '' };
let cleanup = null;
let lastHash = '';
let ignoreNext = false;
let renderSeq = 0;

const ROUTES = {
  scenarios: { nav: 'scenarios', title: 'Projects / Scenarios', view: (m, p, c) => renderScenarios(m, p, c) },
  editor: { nav: 'scenarios', title: 'Scenario editor', view: (m, p, c) => renderEditor(m, { file: p[0] }, c) },
  correlate: { nav: 'scenarios', title: 'Correlation review', view: (m, p, c) => renderCorrelate(m, { file: p[0] }, c) },
  import: { nav: 'import', title: 'Import', view: (m, p, c) => renderImport(m, {}, c) },
  run: { nav: 'run', title: 'Run & Monitor', view: (m, p, c) => renderRun(m, { id: p[0] }, c) },
  results: { nav: 'results', title: 'Results', view: (m, p, c) => renderResults(m, {}, c) },
  ai: { nav: 'ai', title: 'AI Assistant', view: (m, p, c) => renderAI(m, {}, c) },
  settings: { nav: 'settings', title: 'Settings', view: (m, p, c) => renderSettings(m, {}, c) },
};

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [pathPart, qs = ''] = raw.split('?');
  const segs = pathPart.split('/').filter(Boolean).map((s) => { try { return decodeURIComponent(s); } catch { return s; } });
  const query = Object.fromEntries(new URLSearchParams(qs));
  return { name: segs[0] || 'scenarios', params: segs.slice(1), query };
}

async function route(keep) {
  const seq = ++renderSeq;
  if (cleanup) { try { cleanup(); } catch (e) { console.warn(e); } cleanup = null; }
  const { name, params, query } = parseHash();
  const r = ROUTES[name] || ROUTES.scenarios;
  document.querySelectorAll('nav a').forEach((a) => {
    if (a.dataset.nav === r.nav) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  title.textContent = r.title;
  document.title = r.title + ' - Phoenix Performance';
  main.replaceChildren(loading());
  try {
    const c = await r.view(main, params, { keep, query, session });
    if (seq !== renderSeq) { if (typeof c === 'function') c(); return; }
    cleanup = typeof c === 'function' ? c : null;
  } catch (e) {
    if (seq !== renderSeq) return;
    main.replaceChildren(errorBox(e));
    toast(e.message || String(e), 'error');
  }
  if (seq === renderSeq && !keep) {
    const hd = main.querySelector('h1');
    if (hd) hd.focus({ preventScroll: true });
    window.scrollTo(0, 0);
  }
}

function setupMode() {
  const b = document.getElementById('mode-beginner'), a = document.getElementById('mode-advanced');
  const sync = () => {
    const m = getMode();
    b.setAttribute('aria-pressed', String(m === 'beginner'));
    a.setAttribute('aria-pressed', String(m === 'advanced'));
  };
  b.addEventListener('click', () => { setMode('beginner'); sync(); route(true); });
  a.addEventListener('click', () => { setMode('advanced'); sync(); route(true); });
  sync();
}

window.addEventListener('beforeunload', (e) => { if (isDirty()) { e.preventDefault(); e.returnValue = ''; } });

window.addEventListener('hashchange', async () => {
  if (ignoreNext) { ignoreNext = false; return; }
  const next = parseHash();
  const leavingEditor = lastHash.startsWith('#/editor/') && next.name !== 'editor';
  const otherFile = lastHash.startsWith('#/editor/') && next.name === 'editor' && ('#/editor/' + encodeURIComponent(next.params[0] || '')) !== lastHash.split('?')[0];
  if (isDirty() && (leavingEditor || otherFile)) {
    const ok = await confirmDialog('Discard unsaved changes?', 'You have unsaved changes in the scenario editor. Leaving now will discard them.', 'Discard', 'danger solid');
    if (!ok) { ignoreNext = true; location.hash = lastHash; return; }
    discard();
  }
  lastHash = location.hash;
  route(false);
});

async function boot() {
  initMode();
  setupMode();
  try {
    Object.assign(session, await initSession());
  } catch (e) {
    main.replaceChildren(h('div', { class: 'err-box', role: 'alert' }, h('h2', {}, 'Not signed in'), h('p', {}, e.message),
      h('p', {}, 'Open the link printed in the terminal by ', h('code', {}, 'phoenix serve'), ' (it looks like http://127.0.0.1:7878/?token=...).')));
    return;
  }
  document.getElementById('side-foot').replaceChildren(h('div', {}, 'Phoenix ' + (session.version || '')), h('div', { title: session.workspace }, session.workspace || ''));
  lastHash = location.hash;
  await route(false);
}
boot();
