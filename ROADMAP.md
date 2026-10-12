# Roadmap

## 0.1 (this build)
Engine (closed + open model), metrics, HTML report, CLI, web UI, HAR import/record, JMeter import/export, LoadRunner analysis, correlation + replay validation, parameterization, AI providers, MCP server, NL assistant, safety policy.

## 0.2
- Live-verify AI providers with real keys; prompt evaluation set.
- WebSocket and gRPC samplers.
- HTTPS recording proxy with a locally generated, user-trusted CA.
- Signed/notarized binaries, GitHub Releases, Homebrew/Scoop/winget manifests.
- Verified Docker image.

## 0.3
- Distributed load (controller + agents over mTLS), merged histograms.
- Loops of unknown length, throughput/interleave/random controllers.
- Embedded-resource download.
- Prometheus/OpenTelemetry export, Grafana dashboard.

## Later
- Browser-level (Chromium) virtual users for front-end timings.
- CI integrations (GitHub Action), baseline gating.
- Additional VuGen protocols where public specs allow.
