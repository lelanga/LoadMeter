# Load Meter web UI notes

Plain static files embedded with `//go:embed static` (`web.Static`). No build step, no npm, no external requests.
ES2020 modules: `app.js` (router), `api.js`, `ui.js` (DOM helpers, dialogs, toasts, formatting), `charts.js` (canvas), `summary.js`, `inspector.js`,
`editor.js`, `correlate.js`, `import.js`, `run.js`, `results.js`, `ai.js`, `settings.js`, `scenarios.js`, `style.css`.
All server text is inserted with `textContent`/text nodes (no `innerHTML` anywhere).
Server must serve `static/` at `/` (so `/app.js`, `/style.css`, `/ui.js`... resolve) with correct JS MIME type (`text/javascript`) for modules.

## Views and endpoints (hash routes)
| Route | View | Endpoints |
|---|---|---|
| `#/scenarios` | Scenario list, welcome/first-run, sample test, new scenario | GET /api/session, GET /api/scenarios, DELETE /api/scenarios/{f}, POST .../validate, POST /api/demo, POST /api/runs |
| `#/editor/{file}` | Step tree, request form, variables/data, workload, SLAs, raw JSON (Advanced), debug inspector | GET/PUT /api/scenarios/{f}, POST .../validate, POST .../debug |
| `#/correlate/{file}` | Correlation + parameterization review | POST .../correlate, .../correlate/apply, .../parameterize, .../parameterize/apply |
| `#/import` | HAR / JMX / LoadRunner upload, compat report, (HAR) correlation panel | POST /api/import/har, /jmx, /loadrunner (+ correlate apply endpoints) |
| `#/run`, `#/run/{id}`, `#/run?file=x` | Start form, live dashboard, stop, final summary | GET /api/scenarios, GET /api/runs, GET /api/runs/active, POST /api/runs, POST /api/runs/{id}/stop, SSE /api/runs/{id}/live, GET /api/runs/{id}, POST .../validate |
| `#/results` | Sortable run list, summary, compare, downloads | GET /api/runs, GET /api/runs/{id}, GET /api/compare, links to /report, /summary.csv, /raw.csv, /summary.json |
| `#/ai` | Chat-like planner with confirm step, activity | GET /api/settings/ai, POST /api/ai/ask, GET /api/ai/activity, GET /api/scenarios |
| `#/settings` | Safety policy, AI settings (+Test), audit log | GET/PUT /api/settings/policy, GET/PUT /api/settings/ai, POST /api/ai/test, GET /api/audit |

Not used by the UI (yet): scenario history/diff/restore, `/api/export/jmx/*`, `/api/ai/explain`, `/api/health`.

## Behaviour notes
- Token: `GET /api/session` (cookie), then `X-LoadMeter-Token` on every fetch. `?token=` is removed from the address bar after load.
- Beginner/Advanced is `localStorage["loadmeter.mode"]` (try/catch). Switching re-renders the current view; editor edits are kept in memory.
- Editor leaves with unsaved changes ask for confirmation. "Validate" and "Debug" save first when the editor is dirty (both act on the saved file).
- 409 `needsConfirmation` shows a warning dialog and resends with `confirm:true`; 403 shows allowlist guidance linking to Settings.
- Closed (VUs) vs open (rate) model explanation is on the Workload tab.

## Assumptions beyond API.md
- Error rates (`errorRate`) are fractions 0..1 (shown as %). SLA `error_rate` is compared in percent.
- `byRequest` / `byTransaction` entries are `{name, ...MetricSet}` (or `{name, metrics:{...}}`).
- `GET /api/runs/{id}` while running either 404s or returns `status:"running"`; both are handled. Final statuses `completed`, `done`, `passed` count as success, `failed` as failure.
- `Comparison` shape is unknown: rendered generically (arrays of objects become tables, objects become key/value lists). A row is highlighted red as a regression when a field whose name contains regress/worse/degrad/slower/breach is true or a non-"no" string, or when `verdict|status|change|result|direction` contains such a word. Base = the older of the two selected runs.
- `ParamSuggestion` shape is unknown: displays `id`, `variable|name`, `value|sample`, `reason|why|description`, `confidence`; other fields appear under "more" in Advanced.
- `Candidate.source.extractor` may be a string or `{type, expr}`; destinations/source `location` may be strings or `{step, location}`.
- `correlate/apply` `validation.details` and audit events are rendered generically (arrays of objects as tables).
- `policy.maxDuration` keeps its original JSON type (string like "30m" or number) on save; other policy fields are numbers; `allowedHosts` is a string array.
- AI settings PUT sends `apiKey` only for providers where a key was typed; routing is `{light, strong}` provider ids. The "Test" button saves AI settings first because `/api/ai/test` takes only a provider id.
- LoadRunner folder upload sends each file as `files` with its bare file name (no relative path).
- Server-side `scenario` in `/api/runs/active` is matched against scenario file or name to find SLAs/planned duration for the live view; if unmatched, elapsed time is shown without remaining time.
- Live dashboard merges the last-60 timeline points of each snapshot by `t` to build full-run charts; planned duration is computed from the scenario workload (duration override honoured for non-ramping executors).
