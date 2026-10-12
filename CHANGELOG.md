# Changelog

## 0.1.0-dev — 2026-10-12
### Added
- Canonical scenario IR `phoenix/v1` with validation.
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
### Fixed during integration
- Web server panicked at start-up because of a route-pattern conflict (`/api/` vs `GET /`); covered by a test now.
- CLI flag reordering mis-parsed value flags such as `--apply all`; it now reads flag kinds from each command's flag set.
- UI read error rates as fractions (API returns percent) and read live events without their wrapper.
