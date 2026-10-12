# Roadmap

## 0.1 (this build)
Also in 0.1: HTTPS + browser recording, distributed agents, OpenAPI import, k6 export, MCP resources/prompts/HTTP/auto-install.
Engine (closed + open model), metrics, HTML report, CLI, web UI, HAR import/record, JMeter import/export, LoadRunner analysis, correlation + replay validation, parameterization, AI providers, MCP server, NL assistant, safety policy.

## 0.2
- Live-verify AI providers with real keys; prompt evaluation set.
- WebSocket and gRPC samplers.
- Signed/notarized binaries, GitHub Releases, Homebrew/Scoop/winget manifests.
- Verified Docker image.

## 0.3
- Distributed runs from the web UI and MCP; capacity-weighted splitting.
- Loops of unknown length, throughput/interleave/random controllers.
- Embedded-resource download.
- Prometheus/OpenTelemetry export, Grafana dashboard.

## Later
- Browser-level (Chromium) virtual users for front-end timings.
- CI integrations (GitHub Action), baseline gating.
- Additional VuGen protocols where public specs allow.
