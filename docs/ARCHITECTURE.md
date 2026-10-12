# Architecture

Load Meter is one Go program built from the standard library only. The CLI, the web UI, the HTTP API and the MCP server are thin front ends over the same core.

## Package map

| Path | Role |
|---|---|
| `cmd/loadmeter` | The CLI. `main.go` has `run`, `validate`, `demo`, `mock`, `report`, `compare`, `runs`, `doctor`, `init`. `more.go` has `serve`, `mcp`, imports, `correlate`, `parameterize`, `record`, `ask`, `ai`, `explain`. Also OS-specific open-file-limit helpers |
| `web` | Embeds the static single-page UI (`web/static`: plain ES modules, no build step) with `go:embed`. See `web/UI_NOTES.md` |
| `examples` | Embeds the sample scenario `shop-login.json` and `users.csv` |
| `internal/model` | The scenario format (the intermediate representation, IR), its validation, and the compatibility report type |
| `internal/vars` | `${name}` substitution, built-in functions (`__uuid`, ...) and condition evaluation |
| `internal/extract` | Value extraction: JSONPath subset, regex, XPath subset, boundaries. XML parsing that rejects DOCTYPE |
| `internal/har` | HAR 1.2 import (noise filtering, grouping, credential removal) and the plain-HTTP recording proxy |
| `internal/jmx` | JMeter `.jmx` import and export, with a per-element compatibility report. See `SUPPORT_MATRIX.md` |
| `internal/loadrunner` | Static analysis and conversion of LoadRunner VuGen web scripts. See its `README.md` |
| `internal/correlate` | Finds dynamic values that flow from responses to later requests, proves them by replay, applies rules |
| `internal/param` | Finds business inputs that should come from test data and generates synthetic CSV data |
| `internal/engine` | Runs a scenario: executors, virtual users, HTTP requests, extraction, assertions, CSV feeds |
| `internal/metrics` | Collector and log-linear histograms for latency, error kinds, per-second windows |
| `internal/results` | Turns a finished run into a persisted summary, evaluates SLAs, builds findings, compares runs |
| `internal/report` | Renders the self-contained HTML report |
| `internal/sysres` | Samples the load generator's own CPU and memory use |
| `internal/safety` | Policy file, host allowlist, guarded dialer, rate limiter, audit log |
| `internal/redact` | Masks secrets and personal data for UI, logs and AI |
| `internal/app` | Workspace layout on disk, scenario load and save, run start and wait, workload overrides, target checks |
| `internal/service` | The application API shared by the HTTP server, MCP and the assistant: scenarios, history, imports, correlation, runs, policy |
| `internal/server` | The loopback HTTP server: UI files, JSON API, SSE live stream, token / CSRF / Host checks |
| `internal/tools` | Typed tools with JSON schemas. One registry for MCP and the assistant |
| `internal/mcp` | MCP server over stdio (JSON-RPC) |
| `internal/assistant` | Turns natural language into a tool plan (AI planner with a rule-based fallback) and explains runs |
| `internal/ai` | Provider adapters, routing, consent, budgets, key storage, activity log |
| `internal/mockserver` | Deterministic mock shop used by `demo`, `mock`, tests and `doctor` |

`research/` holds background notes from the design phase. `testdata/` holds importer fixtures.

## Data flow

```text
HAR / JMX / VuGen script / hand-written JSON
        |
        v   importers (har, jmx, loadrunner)
   model.Scenario  (the IR; loadmeter/v1 JSON on disk)
        |
        v   correlate + param (review and apply; validated by replay)
   improved Scenario
        |
        v   safety checks (allowlist, limits) in app / engine
   engine.Run  ->  virtual users  ->  HTTP requests
        |                               |
        |                        extractors, assertions
        v
   metrics.Collector  (histograms, per-second windows, raw.csv)
        |
        v
   results.Summary  (percentiles, SLAs, findings, comparison)
        |
        +--> summary.json / summary.csv / raw.csv
        +--> report.html
        +--> live SSE stream, CLI table, MCP tool replies
```

Everything the engine needs is in the Scenario. Importers never run anything. Exporters (`export-jmx`) read the same IR.

A run is stored in `<workspace>/runs/<run-id>/` with `scenario.json`, `summary.json`, `summary.csv`, `raw.csv` and `report.html`. Scenarios are in `<workspace>/scenarios/`, with earlier versions saved under the workspace history folder every time a scenario is overwritten or deleted.

## Concurrency model of the engine

The engine runs one goroutine per virtual user. It supports two families of executors.

### Closed model (`constant-vus`, `ramping-vus`, `per-vu-iterations`)

- Load Meter starts a pool of VU goroutines. For `ramping-vus` the pool size is the largest stage target.
- VU number `i` is active only while `i` is at most the current target. The target is computed from the elapsed time by linear interpolation between stages. VUs above the target idle and poll every 50 ms.
- Each active VU loops: run `setup` once, then iterations one after another, optionally paced by `workload.pacing`, until the duration ends. `teardown` runs when the VU finishes.
- The VU waits for each response before it sends the next request. When the system slows down, the offered load falls with it. This is how real users with a fixed crowd behave.

### Open model (`constant-arrival-rate`, `ramping-arrival-rate`)

- A scheduler loop starts one iteration every `1/rate` seconds, with the rate read from the stage interpolation.
- Each iteration takes an idle VU from a pool. If none is idle, it creates a new VU up to `maxVUs`.
- If the pool is full, the iteration is **dropped** and counted. It is not queued. The report shows dropped iterations.
- If the scheduler falls more than 1 second behind, it resets its clock instead of sending a burst to catch up.
- Because new work arrives whatever the system does, response times can grow without bound. That is what an open model is meant to show.

### Shared rules

- Each VU has its own HTTP client with its own cookie jar (unless `disableCookies` is set), its own variables, and its own random generator.
- All VUs share one HTTP transport. It has no system proxy, and its dialer is wrapped by the safety guard.
- A global token bucket enforces `maxRPS` from the policy.
- A **graceful stop** (first Ctrl+C or Stop click, or the end of the duration) closes a channel so no new iterations start. In-flight iterations get `gracefulStop` (default 5 s) to finish. A second stop cancels the context and aborts in-flight requests.
- Think time is cut short by a graceful stop.
- Transaction time excludes think time that happened inside the transaction.
- The load generator's own CPU and memory are sampled during the run and appear in the results, so you can see if the generator was the bottleneck. These values are measured on Linux only. On macOS and Windows they are reported as 0 (see KNOWN_LIMITATIONS.md).

## Metrics histogram

Latency samples go into a **log-linear histogram** (the same idea as HDR Histogram).

- Values are stored in microseconds.
- Small values are stored exactly. Larger values are grouped into buckets whose width grows with the value. Each power-of-two range is split into 128 sub-buckets (`7` sub-bucket bits).
- This gives a relative error below **0.8 %** for any recorded value.
- Memory per histogram is fixed (64 x 128 counters). Recording a sample is one array increment.
- Histograms **merge** by adding counters. Per-second windows, per-transaction and per-request totals, and the final run summary are all built by merging. A percentile is always read from the merged distribution.
- **Percentiles are never averaged.** Averaging per-second p95 values gives a wrong answer. Load Meter reads p50, p90, p95 and p99 from the full distribution of all samples in scope.
- The live dashboard and the final summary use the same code, so their numbers agree.
- Quantiles use nearest-rank, clamped to the exact minimum and maximum seen.
- Averages are computed from exact sums, not from the histogram.

Run comparison reads these full histograms for both runs. A latency regression is more than 10 % and more than 5 ms worse. An error regression is a rise of 1 percentage point or more.

## Design decisions

| Decision | Reason |
|---|---|
| One static binary, standard library only | Nothing to install or version-match. No third-party code to audit (see [SBOM.md](SBOM.md)) |
| One IR (`loadmeter/v1` JSON) for every importer, exporter and the engine | Each importer is only a translator. Human-readable and easy to diff |
| One service layer for UI, CLI, MCP and assistant | The same validation and safety checks apply everywhere. No front end has extra powers |
| Safety in the engine's dialer, not only in the UI | A check in one front end can be bypassed from another. The dialer sees every connection, including redirects |
| Allowlist set by the operator, never by the scenario | Imported scenarios are untrusted input |
| Imported scripts are read, never executed | Avoids running JSR223, BeanShell or LoadRunner C code from untrusted files |
| Compatibility reports for every import and export | Nothing is dropped silently. Each element is supported, partial, unsupported or ignored |
| Correlation proved by replay | A rule is kept only if the scenario still works after it |
| Open and closed models both available | They answer different questions. They are not interchangeable |
| Mergeable histogram with a stated error bound | Correct percentiles for live view, final report and comparison |
| Deterministic code never depends on AI | Parsing, execution, metrics and validation work with AI off |
| AI behind consent, local-only mode, redaction and budgets | Data leaves the machine only when you allow it |
| Local UI on loopback with a token | Safe on a shared computer, no remote attack surface |
| Plain JS modules in the UI, no build step | The UI is embedded with `go:embed` and has no external requests |
