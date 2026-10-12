# Known limitations (0.1.0-dev)

Honest list. Anything not listed here and not in the support matrices should be treated as untested.

## Engine and protocols
- HTTP/1.1 and HTTP/2 (via Go net/http) only. No WebSocket, gRPC, browser (TruClient-style), SAP, Citrix, database or socket load.
- Single generator process. No distributed / multi-machine load yet.
- No embedded-resource download (JMeter "retrieve embedded resources", LoadRunner HTML mode). HAR import drops static assets by default (`--include-static` keeps them).
- One timeout per request (no separate connect/read timeouts). No client certificates or NTLM/Kerberos auth.
- `__counter` is global only; there is no loop of unknown length (while/for-each).
- Generator CPU/RSS is measured on Linux; on macOS/Windows these values are reported as 0.

## JMeter (.jmx)
- Full matrix: `internal/jmx/SUPPORT_MATRIX.md`.
- One load thread group per scenario; extra thread groups are imported disabled.
- While, ForEach, Throughput, Interleave, Random, Runtime, Switch, Module/Include controllers and complex If conditions are kept as disabled groups and listed as manual changes.
- JSR223/BeanShell/Groovy scripts never run. Listeners are ignored.
- Export: SLAs have no JMeter equivalent; arrival-rate workloads export as a fixed thread count; `required` extractors and header assertions are partial.
- Model gaps reported by the importer: CSV skip-header with named columns, negated assertions, auth managers, cookie policies, all-matches extractors, throughput timers, per-loop CSV advance.

## LoadRunner (VuGen)
- Static analysis only; Phoenix does not execute VuGen scripts or use the LoadRunner runtime. Full details: `internal/loadrunner/README.md`.
- Web (HTTP/HTML) protocol subset only. Other protocols are reported as unsupported.
- No C evaluation: `for`/`while`, `sprintf`-built arguments, macros and `globals.h` logic are reported, not converted.
- `.prm` key names differ between VuGen versions and were not verified against a licensed install.
- `.dat` files are read as UTF-8 only.

## Correlation and parameterization
- Correlation is found by matching recorded values. Values that are transformed (hashed, re-encoded, split) between response and request are not found and are listed as "dynamic unexplained".
- Replay validation needs a reachable target that the safety policy allows.

## AI
- AI features were tested only with the built-in mock provider. Live calls to Anthropic, OpenAI, Gemini, OpenRouter and Ollama were **not** made in this environment (no keys). Request formats follow public docs but are unverified end-to-end.
- AI is never in the request hot path; it only plans, explains and suggests.

## Packaging
- Dockerfile is unverified (no Docker daemon in the build environment).
- macOS and Windows binaries were cross-compiled but not run.
- Binaries are unsigned (no macOS notarization, no Windows Authenticode).
- No published release or download URL yet; no Git remote yet.

## OpenAPI import and k6 export
- OpenAPI: JSON only (no YAML); first `servers` entry only; callbacks/webhooks ignored; schema `pattern` ignored; `oneOf` uses the first variant; XML/text/file bodies need manual content; external `$ref` unsupported.
- k6: JSONPath filters/wildcards/recursive descent and XPath unsupported; error-rate thresholds count HTTP failures only; teardown runs once globally; CSV rows follow the k6 iteration counter, not a shared cursor.

## Recording
- HTTPS is recorded through a local recording CA. Only Chromium-family browsers are launched pre-configured (`record --browser`); other clients need the CA installed manually (`phoenix record ca`). Running Chrome as root needs `--no-sandbox` via `PHOENIX_BROWSER_ARGS`.
- Browser-to-proxy is HTTP/1.1 only; WebSocket upgrades fail while recording. QUIC is disabled only in the launched browser.
- Recorded body copies are capped at 2 MB; `Accept-Encoding` is replaced so bodies are readable.
- Recording state is in memory (one per process); the HAR is written at stop. In the UI, closing the browser does not auto-stop.
- Recording ignores the target allowlist; recorded hosts must be allowlisted before a load test. `--insecure-upstream` is CLI-only.
- The Record web page has not been clicked through in a browser yet.

## Distributed runs
- CLI only (`run --agents`); not yet in the web UI, HTTP API or MCP.
- Verified only over loopback on one host; real LAN runs and clock skew > 250 ms are untested. Ramping and arrival-rate executors are unit-tested for splitting only.
- `raw.csv` holds only the local share's samples.
- An agent unreachable before start aborts the run; no reconnect after a disconnect.
- Equal split (plus optional local share), no capacity weighting; ramp targets may be off by ±1 VU between stages.
- `sequential`/`random` data gives every node the full data set (values repeat across nodes); `unique` exhaustion is per node.
- The controller does not cap total VUs across agents; each agent caps its own share.
