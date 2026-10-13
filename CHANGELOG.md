# Changelog

## 0.1.0-dev — 2026-10-12
### Added
- Canonical scenario IR `loadmeter/v1` with validation.
- Load engine: constant-vus, ramping-vus, per-vu-iterations, constant- and ramping-arrival-rate; transactions, think time, loops, conditions, extractors, assertions, CSV data feeds.
- Metrics: mergeable log-linear histograms, per-second timeline, JTL-compatible raw CSV, generator CPU/RSS.
- Self-contained HTML report, run comparison with regression detection, SLA evaluation, findings (observation vs hypothesis).
- CLI: run, validate, demo, mock, init, report, compare, runs, doctor, serve, mcp, import-har, import-jmx, import-lr, export-jmx, correlate, parameterize, record, ask, ai, explain.
- Web UI (embedded): scenarios, editor, import, correlation review, run & live monitor, results, compare, AI assistant, settings.
- HAR import and HTTP recording proxy; correlation detection with replay validation; parameterization with synthetic data.
- JMeter .jmx import/export; LoadRunner VuGen Web static analysis and conversion.
- AI providers (Anthropic, OpenAI, Gemini, OpenRouter, Ollama, mock) with routing, consent, budgets and redaction; MCP stdio server; natural-language assistant.
- Safety policy: loopback-only by default, host allowlist, VU/RPS/duration caps, confirmation gate, audit log.
- Cross-platform binaries, install script, Dockerfile, Makefile.
- HTTPS recording via a local CA, browser-launched recording, transaction markers (CLI, UI, API, MCP).
- Distributed runs: `agent` command, pinned TLS, token auth, merged histograms, per-node results.
- OpenAPI 3 import and k6 export.
- MCP resources, prompts, progress, HTTP transport and `mcp install` for six clients.
### Changed
- Product renamed from Phoenix to Load Meter (binary `loadmeter`, env vars `LOADMETER_*`, config dir `~/.loadmeter`, format tag `loadmeter/v1`).
### Fixed during integration
- Ten bugs found by the QA pass (see TEST_RESULTS.md).
- Web server panicked at start-up because of a route-pattern conflict (`/api/` vs `GET /`); covered by a test now.
- CLI flag reordering mis-parsed value flags such as `--apply all`; it now reads flag kinds from each command's flag set.
- UI read error rates as fractions (API returns percent) and read live events without their wrapper.

## Unreleased: JMeter-informed upgrade
- Research: JMeter source analysis (G01-G20 gaps), smart-feature survey.
- Access-log replay (`import-log`), weighted groups, think-time distributions.
- Failure triage (`triage`) and grounded plain-English summaries (`summarize`).
- Capacity/breaking-point finder (`capacity`, MCP `find_capacity`).
- Statistical compare, `gate` (JUnit, exit codes 0/3/4), `suggest-slas`, `--abort-on-breach`, change-point/drift findings.
- HTTP parity: compression decoding, split timeouts and timings, per-VU connections, HTTP/2, hidden-retry accounting, basic/digest/bearer auth, mTLS, DNS override, proxy (all inside the safety allowlist).
- Fixes: fractional ramp start, arrival-pool VU leak, per-run `__counter`.

## Unreleased: second wave
- Control flow: while/foreach/switch/percent/runtime, shuffle/roundRobin groups, throughput, rendezvous, critical, Poisson arrivals, newUserEachIteration; JMX import/export and k6 export mappings.
- Extractor parity (all matches, templates, sources), assertion parity (not, equals, size, hash, ignoreStatus), ~20 functions, nested variable expansion, comma escaping.
- Fixes: JMX CSV variableNames export, JTL grpThreads/allThreads and transaction rows, policy maxDuration hard-stops every run.
