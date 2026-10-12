# Decisions

| # | Decision | Why |
|---|---|---|
| 1 | Go, standard library only, single static binary with the UI embedded | Plug-and-play install like JMeter, no JVM, tiny SBOM, easy cross-compile |
| 2 | Canonical JSON IR `phoenix/v1`; all importers/exporters convert to/from it | One engine, many formats; every conversion produces a compatibility report |
| 3 | Log-linear histogram (7 sub-bucket bits, <0.8% relative error); percentiles always computed from merged histograms | Averaging percentiles is wrong; HDR-style buckets are cheap and mergeable |
| 4 | Both closed (VU) and open (arrival-rate) executors, dropped iterations counted | Open model avoids coordinated omission; matches k6/LoadRunner goal-oriented usage |
| 5 | Loopback-only targets by default; other hosts need an explicit allowlist; checked at dial time before DNS | Prevents accidental or abusive load against third parties, including via redirects |
| 6 | Policy limits (max VUs, RPS, duration) and confirmation above a threshold; audit log | Safe defaults for AI/MCP-driven runs |
| 7 | Secrets only from env vars or the OS keystore; redaction in logs, reports, AI payloads | Reports and AI prompts are shareable artifacts |
| 8 | Correlation candidates must reproduce the recorded value, then be replay-validated before auto-apply | No silent wrong correlations |
| 9 | LoadRunner support is static analysis + conversion, never runtime emulation | The runtime is proprietary; claims stay honest |
| 10 | AI is optional, off the hot path, consent-gated for cloud providers, local Ollama default | Works offline; no data leaves without consent |
| 11 | One tool registry shared by MCP server and the NL assistant; destructive/run tools need `confirm=true` | Same behaviour for every AI client |
| 12 | Web UI is vanilla JS modules served from the binary; token cookie + header CSRF + Host check | No build step; safe against DNS rebinding and CSRF |
| 13 | JMeter target version 5.6.3 for export | Current stable at research time |
| 14 | Security safeguards stay on even though full relaxation was requested | The brief itself requires secure defaults; relaxing them would allow unsafe load generation |
