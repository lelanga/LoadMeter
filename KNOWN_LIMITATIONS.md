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
- Static analysis only; Load Meter does not execute VuGen scripts or use the LoadRunner runtime. Full details: `internal/loadrunner/README.md`.
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
- HTTPS is recorded through a local recording CA. Only Chromium-family browsers are launched pre-configured (`record --browser`); other clients need the CA installed manually (`loadmeter record ca`). Running Chrome as root needs `--no-sandbox` via `LOADMETER_BROWSER_ARGS`.
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

## Access-log replay
- Writes replay with an empty body; ids become random values so most will 404 on a real target (supply a data source of real ids).
- Real-time replay, one stage per minute (linear interpolation smooths the curve). `peak` window is fixed at 15 minutes.
- Session reconstruction (IP + user agent) is approximate; NAT merges users. Log timestamps have 1 s resolution.
- Only combined/common layouts for nginx/Apache (no custom `log_format`). Weighted groups and think distributions export to JMeter/k6 as `partial`.

## Failure triage and summaries
- **Verifier scope**: it checks numbers only. It cannot tell whether a true number is attached to the right thing. It does not check causal claims or spelled-out compounds ("twenty five"), and it allows a number that rounds from any fact, so a coincidentally close value passes.
- **One AI attempt**: there is one model call, no retry; any ungrounded number means the template is shown.
- **Quoted values are stripped**: two different extractor names in the same request merge into one cluster. The example text keeps the name and the explanation quotes it.
- **Drain order**: templates depend on row order, so they can differ slightly if the CSV is shuffled. The same file always gives the same result.
- **Transaction membership**: `raw.csv` has no transaction column, so it comes from `scenario.json` by request label. A transaction's own failures are counted but not clustered.
- **`raw.csv` missing**: this happens with `--no-raw` or distributed runs. Triage is then coarse: no timing, no messages, no change-point or "after extraction" logic.
- **Generator evidence** relies on the CPU timeline in the summary (above 85% of the machine's cores); it cannot detect sockets or file descriptors unless the error text says so.
- **Report hook**: it triages only when `Render` is given an `*os.File` next to `raw.csv`, which holds for all three callers (`internal/app/runner.go`, `cmdReport`, distributed). For a distributed run without `raw.csv`, the report gets the coarse section.
- **Classification order**: while testing, 429 and 5xx after an extraction failure were wrongly labelled "script". The final order puts them on the target side first; tests cover it.

## Capacity finder
- Knee detection only tested on synthetic curves; the mock has no load-sensitive latency.
- Each step is a fresh run; achieved throughput reads about 1% high from warm-up spillover.
- Generator CPU is this process's CPU (includes an in-process mock); not measured on Windows.
- Dropped iterations with latency within SLA mark a step generator-limited, which can hide a target limit with a tiny VU pool.
- Bisection steps are single measurements.
- Capacity search vs normal run exclusion is a check, not a lock.
- MCP `find_capacity` lacks the open-world annotation.
- Existing flaky test: `internal/service` `TestStartRunWithVarsOnScenarioWithoutVariables` (TempDir cleanup race, ~2-3/10).

## Statistics and gates
- Window samples test the typical per-second percentile, not the whole-run percentile; per-second p99 under ~100 requests approximates the max.
- `raw.csv` latency is whole milliseconds: transaction comparisons can't see sub-ms changes.
- Transactions without raw.csv in both run folders (summary-only compare, distributed, `compare_runs` MCP) are inconclusive/skipped.
- Autocorrelation is corrected for lag-1 only; bursty errors overstate confidence.
- MSER warm-up trim can cut a fast start too (capped at half the run).
- Diagnose uses the whole-run timeline only; memory drift is the generator's.
- Abort-on-breach ignores p90, rps and scoped SLAs; not in distributed runs.
- CUSUM and binary segmentation are implemented but not wired in.

## HTTP layer
- Brotli/zstd not decoded; digest `auth-int` unsupported.
- Hidden-retry count exact for HTTP/1.1, best effort for HTTP/2.
- Lifetime recycling sends a visible `Connection: close` on HTTP/1.1.
- Per-request TLS timeout fails on time, but the handshake is bounded only by the largest TLS timeout.
- DNS overrides don't apply to proxied targets. Client certs per host, not per VU.
- `hiddenRetries`/`newConnections` not merged in distributed aggregation.
- Flaky: `TestStartRunWithVarsOnScenarioWithoutVariables` (TempDir race); `TestPerformanceOneMillionRows` can time out under load.

## Control flow (second wave)
- Loops (`while`, `foreach`, `runtime`) are bounded by `maxIterations` (default 10 000, max 1 000 000), run duration and policy `maxDuration`. In `while`, an unresolved variable is false (JMeter would loop forever); imported While is always partial.
- `throughput` only slows VUs; no "target not reached" finding. Poisson arrivals use exponential gaps, not Precise Throughput Timer blocks (unsupported).
- `rendezvous` max wait is its timeout (default 30 s, max 1 h); `group: 0` = VUs active at arrival. `critical` waits count in transaction time.
- `newUserEachIteration` doesn't reset connections when `connections: shared`.
- JMX While conditions convert only when empty, `LAST`, `${var}` or simple ==/!=. Module/Include controllers, Test Fragments, infinite Loop (-1) stay disabled groups.
- k6 export: rendezvous skipped, critical sections unlocked, shared counters per-VU, while-on-last-result runs once.
- Not done: G14, G15, G18, G20.

## Extractors, assertions and functions
- Header sources are rebuilt from parsed headers (sorted, canonical casing); JMeter regexes depending on raw order/casing can differ.
- `match: "all"` sets `name` to the first match and always writes `name_ALL`; regex extractors always write `name_g*`.
- `hash` covers only the body kept up to `maxBodyBytes`; JMX hash export is md5 only.
- Unsupported: Response Assertion NOT+OR, "Not passes on empty", request-data/document fields.
- Date functions: English only, locale ignored; parsing lacks `G Y w W F u k K`; `__timeShift` D/H/M/S only.
- Behaviour changes: `${...}` inside scenario/CSV/`set` values now expands; `\\` in function args is one backslash; failed transaction rows have empty responseCode.
- k6 export ignores these features (each noted in the report).
- raw.csv `grpThreads` = `allThreads`; transaction counts exclude nested transactions.
