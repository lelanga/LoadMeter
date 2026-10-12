# SMART / AI-ERA FEATURES: SURVEY AND RECOMMENDATIONS FOR LOAD METER

Researched 2026-10-12 from public vendor documentation and blogs only. No binaries were downloaded or analysed.

Evidence grades: **[D]** = fetched vendor documentation page; **[B]** = vendor blog or marketing page (self-promotional); **[S]** = search-result snippet only, page not fetched. "Not documented" means the page I read did not say, not that the feature lacks it.

## 0. Scope and what Load Meter already has (so it is not repeated)

Read first: README, PROJECT_STATUS, KNOWN_LIMITATIONS, COMPETITIVE_GAPS (F001-F033), USER_FEEDBACK, docs/AI_AND_MCP.md, plus `internal/results/results.go` and `internal/metrics/collector.go`.

Already in the tree (verified by reading code or docs):
- Full-histogram percentiles, per-second `Timeline` (`metrics.WindowOut`: rps, errors, p50/p95/p99, max, VUs, generator CPU), per-request and per-transaction breakdowns.
- Open and closed models, five executors including ramping and arrival-rate, dropped-iteration counting.
- SLAs evaluated after the run (`results.EvaluateSLAs`), exit codes for SLA failure and regression (`cmd/loadmeter/main.go`).
- `results.Diagnose`: deterministic findings (slowest request, error mix, 401/403 hint, error rise between run halves, generator CPU saturation, dropped iterations).
- `results.Compare`: fixed-rule regression (latency >10% and >5 ms; error +1 pp). No statistics.
- Correlation and parameter suggestion with replay validation; synthetic CSV generation (`apply_parameters`); HAR, JMX, VuGen, OpenAPI (JSON) import; k6 and JMX export.
- HAR think-time inference from inter-request gaps (`internal/har`, `ThinkThreshold` 800 ms): crude, per-recording only.
- `loadmeter explain`, `ask`, MCP server (21 tools), redaction, consent, budget limits, local Ollama.
- Distributed runs (CLI only), safety allowlist, confirm gates, audit log.

Not in the tree (grep for prometheus, nginx, access log, knee, breakpoint returned nothing relevant): any breakpoint/capacity finder, statistical regression test, baseline store beyond "pick a run", server-side metric ingestion, access-log import, error clustering beyond status/kind, auto-thresholds.

## 1. Survey table

Load Meter status: **have** / **partial** / **missing**.

| # | Feature | Tool(s) | How it works (as publicly documented) | Evidence | LM status |
|---|---|---|---|---|---|
| 1 | NL script authoring | Grafana Assistant "k6 Script Authoring" mode | Generates k6 script from description, OpenAPI/Swagger, GitHub repo, Postman collection or curl; can also query the user's own metrics/logs/traces to discover real endpoints and build a load profile. Adds checks, thresholds, URL grouping. Marks unknowns with `// TODO`. Output is "meant for review". Needs Grafana Assistant in Grafana Cloud. | [D] https://grafana.com/docs/grafana-cloud/observe-and-act/testing/k6/author-run/k6-script-authoring-mode/ | partial (`ask` + OpenAPI import; no traffic-mix discovery) |
| 2 | Script lint / review by AI | Grafana k6 Script Authoring | Analyses an existing script for high metric cardinality, missing checks/thresholds, wrong lifecycle use, and applies fixes | [D] same URL | missing (a deterministic scenario linter is a cheaper substitute) |
| 3 | AI autocorrelation | k6 Studio 2.0 (GA, "Powered by Grafana Assistant"); LoadRunner; NeoLoad | Detects dynamic values (session, CSRF, resource IDs), creates extract/reuse rules | [D] https://newreleases.io/project/github/grafana/k6-studio/release/v2.0.0 ; [B] Gatling comparison page said earlier preview needed own OpenAI key (https://gatling.io/blog/best-ai-load-testing-tools) | have (deterministic + replay proof, which is stronger than any of these as documented) |
| 4 | AI setup wizard from a recording | k6 Studio 2.0 (experimental) | Analyses a recording step by step and proposes hosts, correlation, parameterization, thresholds for review, then runs in Grafana Cloud k6 | [D] k6 Studio 2.0 release page above | partial (suggest_* tools, but no single "propose thresholds" step) |
| 5 | Agent/MCP integration | k6 2.0 (`k6 x mcp`, `k6 x agent`, `k6 x docs`, JSON summary); NeoLoad MCP; BlazeMeter MCP; Gatling MCP; AWS DLT MCP; LoadRunner MCP (CE 26.1, per Gatling's article) | MCP server lets an agent validate/run scripts, read results, compare runs, generate reports | [D] https://grafana.com/blog/k6-2-0-release/ ; [D] https://www.tricentis.com/blog/new-neoload-2025-3-core-web-vitals-expanding-ai-mcp ; [D] https://docs.aws.amazon.com/solutions/latest/distributed-load-testing-on-aws/features.html ; [B] https://gatling.io/blog/best-ai-load-testing-tools | have |
| 6 | AI run summary | Gatling (AI Run Summary); LoadRunner Aviator for Analysis; NeoLoad MCP; Azure Load Testing | Plain-language analysis of one run. Azure: on-demand "Generate insights", preview, shows evidence and next steps, warns output may be inaccurate | [D] https://docs.gatling.io/ai-for-analysis/ ; [D] https://learn.microsoft.com/ga-ie/azure/app-testing/load-testing/how-to-analyze-test-results-using-actionable-insights ; [D] https://admhelp.microfocus.com/lrc/en/26.3/Content/Storm/Whats_New_25.3.htm | have (`explain`) but not grounded/validated (see rec 12) |
| 7 | AI run comparison | Gatling (2-5 runs) | Structured report: what changed/regressed, where to look next, consistency verdict and confidence level | [D] https://docs.gatling.io/ai-for-analysis/ ; [D] https://docs.gatling.io/ai-for-analysis/ai-trend-analysis/ | partial (`compare_runs` numbers; fixed rule only; 2 runs) |
| 8 | AI trend analysis across runs | Gatling (last 10 runs) | Verdict Stable / SomeIssues / Degrading with Low/Medium/High confidence, findings, 2-4 recommendations; only reasons over the last 10 valid runs, no external baselines | [D] https://docs.gatling.io/ai-for-analysis/ai-trend-analysis/ | missing |
| 9 | Baseline designation and trend | AWS Distributed Load Testing; NeoLoad (baseline history) | Mark a run as baseline to see improvement/regression over time; NeoLoad MCP does "trend-against-baseline analysis" | [D] AWS features URL above ; [D] https://www.tricentis.com/blog/neoload-mcp-practical-use-cases | partial |
| 10 | Anomaly intervals and likely root cause | NeoLoad Augmented Analysis (2025.1) | Marks anomalous periods as intervals, proposes "likely root causes". Method not explained in the article. Gatling's article claims an in-house ML engine segmenting runs into stability intervals against RED metrics (vendor claim) | [D] https://www.tricentis.com/blog/evolving-performance-analysis-neoload-ai ; [B] Gatling comparison page | partial (halves comparison only) |
| 11 | Interval/phase analysis | NeoLoad MCP | Splits execution into phases to show how performance changes; find spikes and degradations in windows | [D] NeoLoad evolving-analysis URL above | missing |
| 12 | AI anomaly analysis with grouping | BlazeMeter AI Anomaly Analysis | After anomalies are detected, "Analyze with AI" groups related anomalies, explains and suggests fix; max 50 anomalies per test; Enterprise plan and AI consent required; optional Dynatrace APM correlation | [D] https://help.blazemeter.com/docs/guide/performance-ai-anomalies.htm | missing |
| 13 | AI log/error triage (clustering) | BlazeMeter AI Log Analysis | Takes engine logs of a finished run, groups errors/warnings, priority 0-100 weighted 40% impact on test success, 25% frequency, 20% origin category, 15% scope; separates system-under-test issues from script issues; grouping method not explained | [D] https://help.blazemeter.com/docs/guide/performance-ai-log-analysis-report.html | partial (`Diagnose` counts by kind/status only) |
| 14 | Client+server correlation for bottleneck hints | Azure Load Testing actionable insights; BlazeMeter (Dynatrace); NeoLoad MCP via Datadog MCP | Azure: correlates client and server-side metrics, detects latency spikes, throughput drops, backend resource bottlenecks; server-side metrics must be enabled. NeoLoad: adds Kubernetes pod status and alerts through Datadog MCP | [D] Azure URL above ; [D] NeoLoad practical use cases URL | missing (F029 backlog only) |
| 15 | Fail criteria and auto-stop | Azure (up to 50 criteria; per-request; server-metric criteria; auto-stop default 90% errors in 60 s); k6 (`abortOnFail`, `delayAbortEval`); Gatling Run Stop Criteria (events, thresholds, time limits) | Static, user-defined. None documents automatic threshold derivation. k6 cloud evaluates thresholds every 60 s | [D] https://learn.microsoft.com/en-my/azUre/app-testing/load-testing/how-to-define-test-criteria ; [D] https://grafana.com/docs/k6/latest/using-k6/thresholds/ ; [D] https://gatling.io/features/run-stop-criteria | partial (SLAs post-run; no abort-on-breach, no auto-stop) |
| 16 | Breaking-point / capacity finding | k6 (manual pattern); LoadRunner goal-oriented scenarios; Locust (custom shape) | k6: recommends ramping-arrival-rate and stopping via Ctrl+C or abortOnFail; no automatic detection documented. LoadRunner Controller: goal types (Vusers, pages/min, hits/s, TPS, transaction response time) with min-max Vuser range, batches every 2 min by default, marks scenario Failed if throughput stops increasing. Locust: user code in `tick()`, no built-in breakpoint detection | [D] https://grafana.com/docs/k6/latest/testing-guides/test-types/breakpoint-testing/ ; [D] https://admhelp.microfocus.com/lr/en/26.1/help/WebHelp/Content/Controller/c_goal_oriented_scenarios.htm ; [D] https://docs.locust.io/en/2.43.1/custom-load-shape.html | missing |
| 17 | AI test data generation | BlazeMeter Test Data Pro (Enterprise) | LLM classifies CSV columns, builds generator function from description, `anySeedlist("car brands")`, `similarValues(...)`. Adds 50% to VUh when active (per Gatling's article, vendor-sourced) | [D] https://help.blazemeter.com/docs/guide/test-data-pro.html ; [B] Gatling comparison page | partial (heuristic synthetic CSV; no semantic generators) |
| 18 | Traffic capture and replay | Speedscale (eBPF/sidecar capture in Kubernetes; replay against candidate; mocks of outbound deps; DLP redaction at ingest; local proxymock) | Captured inbound requests become replayable tests. Docs page I read did not describe synthetic load generation | [D] https://docs.speedscale.com/ | missing (HAR only) |
| 19 | Telemetry-driven profile discovery | Grafana k6 Script Authoring | Queries metrics/logs/traces for a service to find real endpoints and traffic patterns, then builds a load profile | [D] k6 Script Authoring URL above | missing |
| 20 | Script migration assistants | Gatling (VuGen, JMX; "experimental" per article) | LLM-assisted conversion | [B] Gatling comparison page | have (deterministic JMX/VuGen import with report) |
| 21 | Conversational analysis | LoadRunner Aviator for Analysis; NeoLoad AI Chat / Agentic Performance Testing (Mar 2026, per article) | Chat over dashboards: summaries, trends, anomalies | [D] LoadRunner 25.3 what's-new URL ; [B] Gatling comparison page for NeoLoad dates | have (`ask`, UI assistant) |
| 22 | Stakeholder report generation | NeoLoad MCP | Agent-generated reports for one or several tests covering trends, flagged issues, notable intervals | [D] NeoLoad evolving-analysis URL | partial (HTML report, no narrative) |
| 23 | Scheduled/recurring regression runs | AWS DLT (cron schedule); NeoLoad via Jenkins | Recurring tests for regression detection | [D] AWS features URL ; [D] NeoLoad practical use cases | missing (CLI + cron works, nothing built in) |
| 24 | Browser/Web Vitals in load | NeoLoad 2025.3 (Core Web Vitals); Artillery (Playwright) | Real-browser measurement | [D] NeoLoad 2025.3 URL ; [S] Artillery search results | missing (out of scope for now) |
| 25 | Assertions API for agents | k6 2.0 `expect()`, JSON summary | Structured summary so CI and agents need not parse text | [D] k6 2.0 blog above | have (JSON export) |

Not found / could not verify:
- Public docs describing automatic threshold suggestion from a baseline in any of these tools: none found. The k6 thresholds page explicitly says thresholds are manual; Azure's page says it does not cover automatic or baseline thresholds. This is an apparent open niche, but absence of evidence only.
- Think-time or pacing derived from recorded sessions as a named "smart" feature: none found in the pages read.
- Production-log (nginx/ALB) replay in a load tool: not found in vendor docs I read (Speedscale captures live traffic rather than parsing logs). Not verified for Octoperf, Tricentis other products, Checkly.
- Octoperf, Checkly, Artillery, Locust AI features: searches returned no first-party AI documentation. Artillery and Locust have no AI claims in Gatling's article (a competitor source). The `Locust + Azure` and "AI-assisted load testing" results were third-party blogs, not read.
- LoadRunner Aviator (separately licensed, Vertex/Gemini) and GA date in CE 26.1: stated only in Gatling's competitor article [B]; LoadRunner's own 25.3 page confirms Aviator for Scripting and Analysis, beta wording for the PE assistant.
- k6 Studio's earlier "requires your own OpenAI key" claim is from a competitor article; the 2.0 notes say GA "Powered by Grafana Assistant". Treat as changed.
- Accuracy of any vendor AI output: nobody I read publishes accuracy numbers. Azure and Gatling both say output may be wrong and must be verified.

Observations on the market:
1. Every AI-analysis feature found is narrative over metrics the platform already holds (summaries, comparisons, anomaly grouping). None documents a statistical test or states the numbers are checked against the narrative.
2. Detection of breakpoints is manual (k6, Locust) or batch-goal based (LoadRunner), and nothing documented does knee detection.
3. Server-side correlation exists only in cloud/APM-bound products (Azure, BlazeMeter+Dynatrace, NeoLoad+Datadog).
4. AI is vendor-cloud-bound and mostly Enterprise-gated (BlazeMeter, Gatling, k6 Assistant). A local-first tool whose numbers are deterministic is a coherent differentiator, matching COMPETITIVE_GAPS G11/G12.

## 2. Principles used for the ranking

1. **Numbers are deterministic.** Anything stored, gated on, or shown as a metric comes from Go code with tests. The model can only (a) choose among candidates the code produced, (b) phrase explanations that cite computed fields, (c) draft scenario text that the validator then checks.
2. **Every AI output carries its proof.** An explanation must reference finding IDs; a post-check rejects any number in the text not present in the computed facts (extends F027's acceptance criterion).
3. **Safety unchanged.** New features that generate load go through `internal/safety` (allowlist, VU/rate/duration caps, confirm gate). New features that read external systems (Prometheus, logs) are read-only, allowlisted, and credentials never reach a model.
4. **Stdlib only.** All statistical code (Mann-Whitney, bootstrap, change-point, robust regression) is small enough to hand-write and fuzz/test against reference values.
5. **Size**: S = under about 3 days, M = about 1-2 weeks, L = more than 2 weeks, for one developer.

## 3. Ranked top 12

Ranking weighs user value, differentiation vs the survey, fit with existing code, and risk. Items 1-4 are the recommended 0.2 "smart" milestone.

---

### 1. Automatic capacity / breaking-point finder (`loadmeter capacity`)

- **User value.** Answers "how much can this take?" without hand-tuned ramps. Documented tools leave this manual (k6) or batch-goal based (LoadRunner). Output: max sustainable rate at SLA, knee rate, first-failing resource-agnostic symptom.
- **Deterministic vs AI.** Fully deterministic: step schedule, SLA breach test, knee detection, result. AI only explains the curve and proposes the next experiment (for example "test with warm caches"); it never picks the number.
- **Implementation (Go).**
  - New package `internal/capacity`: `Plan` (start rate, step size or multiplicative factor, step duration, warm-up discard, max rate cap), `Runner` that drives the existing arrival-rate executor one step at a time via `internal/engine` (new `engine.RunStep(ctx, sc, rate)` wrapper reusing the collector, one fresh `metrics.Collector` per step), `Detect` for stop rules and knee.
  - Stop rules per step, evaluated over the last N seconds of the step (not the first, to avoid warm-up): SLA breach (reuse `results.EvaluateSLAs`), error rate over limit, `Dropped>0` (VU pool exhausted means the generator or target can't keep up), achieved RPS < 95% of offered RPS (saturation), generator CPU > 85% (invalid step, flagged "generator-bound", never reported as target capacity).
  - Knee: on the (offered rate, p95) series use the Kneedle algorithm (normalise, difference curve, max) and cross-check with throughput-plateau detection (achieved RPS stops increasing, slope < epsilon over 2 steps). Report both and the agreement; if they disagree, say so. Optionally Universal Scalability Law fit (least squares on throughput vs concurrency, `internal/capacity/usl.go`) as a labelled model estimate, not a measurement.
  - Binary-search refinement between last-pass and first-fail step (S/M variant).
  - Output `capacity.json` (steps, per-step summary, knee, max sustainable rate) plus a report section; CLI `loadmeter capacity <scenario> --sla p95<400ms,err<1% --from 50 --step 25 --step-dur 60s --max-rate 1000`; MCP tool `find_capacity` (needs confirm like `run_test`).
- **Data needed.** Existing per-second `Timeline`, SLA definitions, engine counters (`Dropped`, achieved RPS). No new data.
- **Size.** M (L if USL and binary refinement are included).
- **Safety.** Ramps load by design, so: hard `--max-rate`/`--max-vus` required and bounded by policy caps; confirm gate for non-loopback; auto-abort on error storm (default 50% errors over 10 s) and on target unreachable; never auto-escalate above policy caps even if no breach; clear report line "capacity measured from this generator; generator-bound steps excluded". Autoscaling targets can hide the limit (k6 docs say this too).

### 2. Statistical regression detection and auto-baselining

- **User value.** The current `Compare` uses a fixed 10%/5 ms rule that flags noise or misses real shifts. Teams need a defensible "is this slower than normal?" in CI. Gatling/AWS/NeoLoad document baseline comparison; none documents the statistics used.
- **Deterministic vs AI.** Fully deterministic verdict (regression / improvement / inconclusive, effect size, confidence interval). AI optional: explain in prose using only the computed verdict.
- **Implementation.**
  - New package `internal/baseline`: store of runs tagged by scenario and workload fingerprint (hash of scenario, workload, target host) under `workspace/baselines/`; `Select` picks the last K passing runs (default 5) as the baseline pool, or a pinned run.
  - `internal/stats`: Mann-Whitney U on per-second window percentiles (or on per-request latencies when `raw.csv` exists), bootstrap CI on p95/p99 difference (resample windows, block bootstrap to respect autocorrelation), Cliff's delta effect size, and a minimum practical effect (default 5% and 5 ms) so tiny but "significant" shifts don't alert. Error-rate difference via two-proportion test with continuity correction. Multiple-comparison control (Holm) across transactions.
  - Needs histograms to be mergeable: `metrics.Histogram` already exists; add JSON-serialised bucket counts to `Summary` (check what `results.Save` stores; if only percentiles, add buckets).
  - Replace `Comparison.Regressions` counting with `Verdict` records; keep old numbers for backward compatibility. `loadmeter compare --baseline auto --fail-on regression --alpha 0.01`.
  - Warn on invalid comparisons: different workload, different generator CPU class, fewer than N windows, high baseline variance (coefficient of variation > 30%): verdict "inconclusive".
- **Data needed.** Stored histograms or raw samples from previous runs; run metadata fingerprint.
- **Size.** M.
- **Safety.** Read-only. Never auto-promote a failing run into the baseline pool; baseline pool updates are explicit or only from passing runs. State the sample counts used.

### 3. CI quality gates with automatic SLA suggestion from a baseline run

- **User value.** Turns one good run into thresholds (nobody documents this; k6 and Azure make users guess). Removes the "what SLA do I write?" blank page and gives a one-command pipeline gate.
- **Deterministic vs AI.** Thresholds computed deterministically: for each transaction, baseline p95/p99 plus headroom based on measured variability (for example `median(p95 across windows) + max(10%, 3 x MAD)`, rounded up to a sensible step), error-rate ceiling = max(0.1%, 2 x baseline). AI only drafts human-readable names and rationale text; user accepts or edits.
- **Implementation.**
  - `internal/slasuggest`: `Suggest(summary, opts) []model.SLA` with per-metric rationale and a "tight / normal / loose" profile. Uses windows from `Snapshot.Timeline` for variability; refuses to suggest when under 30 windows or when the baseline run itself had errors or generator-bound CPU.
  - CLI `loadmeter sla suggest <run-id> --profile normal --write scenario.json`; MCP `suggest_slas` (read-only) and existing `set_workload`-style `apply_slas` (writes scenario).
  - Gate command `loadmeter gate <run-id|scenario> --baseline auto` that combines SLA results and the regression verdict from item 2 into JUnit XML + JSON + stable exit codes (extend the existing exit-code scheme: 0 pass, 10 SLA fail, 11 regression, 12 invalid test). Ship GitHub Actions/GitLab snippets in `examples/`.
  - Abort-on-breach during a run (k6 `abortOnFail`/`delayAbortEval` analogue): `slas[].abortOnFail` plus `delayAbortEval`, checked once per second against the live window. Note k6 cloud only evaluates every 60 s; local evaluation is finer.
- **Data needed.** A baseline run with window data; existing SLA model (`model.SLA`).
- **Size.** M (S for suggestion alone).
- **Safety.** Suggested SLAs are proposals written only on explicit apply. Mark suggestions derived from a noisy or generator-bound run as unreliable. Abort-on-breach only ever reduces load.

### 4. Anomaly and change-point detection on run timelines

- **User value.** "Latency stepped up at t=214 s when VUs hit 80" instead of two-halves comparison. Matches NeoLoad Augmented Analysis and BlazeMeter anomaly features, but explainable and testable.
- **Deterministic vs AI.** Detection is deterministic (robust statistics). AI optionally narrates and groups, always quoting the detector's interval, magnitude and VU/RPS context. Anomaly list is capped (BlazeMeter caps at 50).
- **Implementation.**
  - `internal/anomaly`: operate on `WindowOut` series (p95, error rate, RPS, achieved/offered ratio, generator CPU). Methods: rolling median/MAD z-score for spikes; CUSUM or PELT-style change-point (O(n) penalised cost with a fixed BIC-like penalty) for level shifts; trend detection via Theil-Sen slope with Mann-Kendall test for drift (memory leak style); segment the run into stability intervals (stable / ramping / degraded) like NeoLoad's interval view.
  - Each anomaly: `{start,end,metric,baselineValue,observedValue,score,coincidingVUs,coincidingRPS,kind}`. Exclude ramp-up windows via the workload definition so ramping is not reported as anomaly.
  - Feeds `Diagnose` (replaces the "first half vs second half" heuristic, which stays as fallback), the HTML report timeline overlay, and `explain`.
  - Tests with synthetic series (known step, spike, drift, noise) and golden files.
- **Data needed.** Existing per-second timeline (already stored in `Summary`). Optional: server metrics (item 8) as extra series.
- **Size.** M.
- **Safety.** Read-only. Fix random seeds (none needed) and publish thresholds in output so verdicts are reproducible.

### 5. Scenario generation from OpenAPI, HAR and access logs with a realistic traffic mix

- **User value.** Fastest route from "I have an API" to a believable test. Grafana's Script Authoring does OpenAPI plus telemetry discovery (cloud-bound). Load Meter's OpenAPI import is JSON-only, takes the first server, one flat scenario.
- **Deterministic vs AI.** Deterministic: parsing, endpoint extraction, weights from log frequency, payload shapes from schema, validation by running `debug_scenario`. AI-assisted: naming flows, choosing dependency order (create before get), writing sensible example values where the schema is silent, always validated by a single-VU debug run and flagged with `// TODO`-style notes (as Grafana does).
- **Implementation.**
  - `internal/openapi`: add YAML (a minimal stdlib YAML subset parser is a risk; alternative: accept JSON, and convert YAML via a small restricted-subset reader with explicit "unsupported YAML feature" errors), multi-server, `$ref` within document, `oneOf` enumeration, auth schemes to header injection.
  - `internal/accesslog` (see item 6) supplies per-endpoint frequency; `internal/mixgen`: builds a weighted scenario set (`model` weights per request group or multiple scenarios with executor shares) so that the mix matches, for example, 62% GET /products, 9% POST /cart. Dependency inference: path parameter producers (`POST /orders` response `id` used by `GET /orders/{id}`) found via response schema and extractor suggestion; verified by replay with the existing `internal/correlate/validate.go`.
  - MCP tool `generate_scenario` (inputs: spec path, optional log path, target, VUs). Output is a normal scenario JSON, never run automatically.
- **Data needed.** OpenAPI doc, optional access logs or HAR, optional sample credentials in env.
- **Size.** L (S for "weights from logs onto existing HAR scenario").
- **Safety.** Generation never fires requests except the explicit single-VU validation, which goes through the allowlist. Do not guess destructive operations: default-exclude DELETE/PUT/PATCH on unknown paths and require opt-in (`--include-writes`). Strip credentials found in logs.

### 6. Production-traffic replay from access logs (nginx, Apache, ALB/ELB, CloudFront)

- **User value.** Real URL distribution, real arrival-rate curve, no recording session. Fits the OSS/stdlib niche; Speedscale captures live traffic via Kubernetes (heavy), vendors did not document log-based replay in the pages I read.
- **Deterministic vs AI.** Fully deterministic parse and shape extraction. AI at most suggests which query parameters look like IDs to parameterize (then validated).
- **Implementation.**
  - New package `internal/accesslog`: parsers for nginx combined and `log_format` custom (user supplies format string), Apache CLF/combined, AWS ALB (space-separated with quoted fields), CloudFront tab-separated; stream with `bufio.Scanner` enlarged buffer; skip static assets; normalise paths (`/orders/123` to `/orders/{id}` by numeric/UUID/hex heuristics, with a user override list); sessionisation by client IP + user agent + gap > 30 min (approximate; mark as such).
  - Outputs: `Profile{Endpoints[], Weights, ArrivalCurve[]}`; modes: (a) "open-model replay": an arrival-rate executor following the per-minute curve, scaled by `--scale 0.1`; (b) "journey replay": reconstructed sessions replayed as iterations with original inter-request gaps (item 7); (c) "mix only": weights onto an existing scenario.
  - Methods restricted to GET/HEAD by default; request bodies are not in access logs, so writes need templates from OpenAPI/HAR and are excluded otherwise.
  - Redact query strings matching secret patterns (tokens, emails) with the existing `internal/redact`; hash client IPs before they touch a model or report.
- **Data needed.** Access log files, host mapping (log host to test target), optional time window.
- **Size.** M.
- **Safety.** Replaying production URLs against production is dangerous: target must be allowlisted and the tool refuses when the log's host equals the target unless explicitly confirmed; default scale 10%; GET-only default; never replays logged `Authorization`/cookies (not in logs anyway); PII in URLs masked in reports.

### 7. Smart think time and pacing derived from recorded sessions and logs

- **User value.** Today think time comes from a single threshold on one HAR. Realistic tests need a fitted distribution and pacing so iteration rate matches target (backlog F024, P2). LoadRunner users value pacing.
- **Deterministic vs AI.** Deterministic: fit and sampling. AI not needed (state this plainly in docs; do not add an LLM where statistics suffice).
- **Implementation.**
  - `internal/think`: from inter-request gaps within a session (HAR timings, or reconstructed sessions from item 6), separate machine gaps (below 300 ms, subresource bursts) from human gaps; fit log-normal and gamma by maximum likelihood on the human gaps; select with AIC and a KS check; store as `thinkTime: {dist:"lognormal", mu, sigma, min, max}` in the scenario (extend `model` with a distribution type next to fixed/uniform). Per-step medians when enough samples (>= 20), otherwise global.
  - Pacing: `pacing: {targetIterationsPerSec}`; engine sleeps per iteration to hold cadence (LoadRunner-style), reporting when iterations overrun pacing (backpressure signal).
  - Clamp tails (default p99.5) so a rare 30-minute gap does not stall a VU.
- **Data needed.** Timed HAR entries or session-reconstructed logs. Needs at least about 50 gaps to fit; otherwise fall back to uniform around median.
- **Size.** S-M.
- **Safety.** None beyond engine caps. Report the fitted parameters and sample size so users can override.

### 8. Server-side metric correlation and bottleneck hints (Prometheus / OTel scraping)

- **User value.** Client latency says "slow"; server CPU, GC, pool saturation, DB time say "why". Azure, BlazeMeter+Dynatrace and NeoLoad+Datadog gate this on their platforms; a local tool that can read a user's own Prometheus fills G11/F029.
- **Deterministic vs AI.** Deterministic: scraping, alignment, correlation coefficients, saturation rules. AI only turns ranked evidence into a sentence ("p95 rose when `db_pool_in_use` hit its max at t=190 s"); hypotheses always labelled as such, like the existing `hypothesis` finding kind.
- **Implementation.**
  - `internal/telemetry`: `PromClient` using `net/http` against `/api/v1/query_range` (user supplies base URL, optional bearer, list of named PromQL queries, step = 1 s to 5 s); `OTLP` receiving is out of scope (L), instead support Prometheus text endpoints (`/metrics`) scraped by a goroutine during the run (a stdlib parser for the exposition format is about 150 lines).
  - `internal/rca`: align series to load timeline; per series compute lagged Pearson/Spearman (lags 0-10 s) against p95 and error rate, plus saturation heuristics (value stuck near its observed max or a configured limit while latency climbs; USE/RED rules from a small built-in rule file: CPU > 85%, memory growth slope, GC pause share, connection pool utilisation, goroutines/threads growth, disk queue). Output ranked "signals" with correlation, lag, and plot data; correlation is never reported as causation, wording enforced by template.
  - Config `telemetry.json` per scenario or CLI flag; MCP tool `get_bottleneck_hints`.
  - Optional later: Kubernetes pod restarts/OOM via the same PromQL (kube-state-metrics queries shipped as presets).
- **Data needed.** Reachable Prometheus-compatible endpoint and a metric allowlist from the user; run timeline.
- **Size.** M-L.
- **Safety.** Read-only queries; scrape URL must pass the same allowlist (SSRF guard: no link-local/metadata IPs by default); credentials stored in the existing keystore and never included in AI prompts or reports; query results sent to a cloud model only as aggregated, redacted facts and only with `cloudConsent`.

### 9. Failure triage: cluster similar errors and separate script faults from target faults

- **User value.** One error rate hides 12 different causes. BlazeMeter's log clustering with 0-100 priority is documented as valuable but Enterprise-only and its grouping method is not published.
- **Deterministic vs AI.** Clustering and ranking deterministic (reproducible given the same input). AI optional for cluster titles and fix suggestions, input limited to cluster signature plus 2-3 redacted exemplars.
- **Implementation.**
  - `internal/triage`: normalise failure records (status, error kind, assertion name, extractor failure, transport error class, response snippet): tokenise snippets, replace numbers/UUIDs/hex/timestamps/IDs with placeholders (log template mining in the Drain style, fixed-depth prefix tree), cluster by template + request + status. Collector must retain a bounded sample (reservoir, for example 200 exemplars per cluster, 10 KB cap each) rather than only counts; add to `metrics` a `FailureSink` interface fed by `engine`.
  - Classification rules: `script` (extractor missing, assertion on dynamic value, 401/403 early in iteration after login, 400 with validation text, `blocked_by_policy`), `target` (5xx, timeouts, resets, 429), `generator` (dial errors with ephemeral-port exhaustion, too many open files, CPU saturation). Priority score using a published formula analogous to BlazeMeter's idea but with our weights: share of failures, first-seen time, whether it precedes the knee.
  - Show timeline of each cluster so "errors began at 190 s" is visible; integrate into `Diagnose` and report.
- **Data needed.** Failure samples captured during run (new, bounded); status and error kind already exist.
- **Size.** M.
- **Safety.** Response snippets can contain PII/secrets: redact on capture (`internal/redact`), bound memory, drop bodies for text above the cap, never persist to history unless the user opts in.

### 10. Cost / capacity estimation (generator sizing and "what will production need")

- **User value.** "How many agents do I need for 20k RPS?" and "how many instances for 5k RPS on this service?" Common planning questions; cloud tools bill by VU-hours, so a pre-run estimate prevents surprises. Azure's docs also warn misconfigured tests incur cost.
- **Deterministic vs AI.** All numbers deterministic from measured data; AI can only draft the narrative and assumptions list.
- **Implementation.**
  - `internal/sizing`: (a) generator side: from `sysres` CPU/RSS per VU and per RPS measured in a calibration run (`loadmeter calibrate` runs 30-60 s against the mock server and records cost per request and per VU for this scenario, including TLS cost), then predicts VUs/RPS per core and how many `--agents` are needed with 30% headroom; (b) target side: from item 1's capacity result, instances needed = ceil(target RPS / (per-instance sustainable RPS x 0.7)); optional USL extrapolation with a stated confidence band, labelled "model estimate"; (c) optional cost: user supplies $/hour per generator and instance, computes run cost and monthly cost.
  - CLI `loadmeter plan --target-rps 5000`. Assumptions printed with every figure.
- **Data needed.** Capacity run (item 1) or any run with per-instance information; user-supplied prices and utilisation target.
- **Size.** S-M after item 1.
- **Safety.** Read-only. Linear scaling assumption is wrong for shared databases; the tool must say what it assumes and never present the output as a guarantee.

### 11. Flaky-script self-healing: re-correlation and drift detection after an app change

- **User value.** After a deploy, recorded scripts break (renamed token field, new header). LoadRunner and k6 Studio rerun correlation by hand; no tool in the survey documents unattended healing. Load Meter already has the hard parts (candidate detection, replay validation).
- **Deterministic vs AI.** Detection and proof deterministic: run the single-VU debug iteration, find the first failing step (status/assertion), diff against the recorded response shape (JSON key paths, HTML form fields, header names), search new response for the old value's replacement via the same value-flow analysis, and re-validate. AI optionally ranks candidate fixes and explains. Changes are written as a proposal diff, never applied silently.
- **Implementation.**
  - `internal/heal`: `Drift(scenario, run) []DriftFinding` (schema-key diff of each response vs stored fingerprints; need to store response fingerprints at import/validation time, stripped of values), `Propose(finding) []Patch` reusing `internal/correlate` candidate search with the new response as input, `Verify(patch)` using the existing `validate.go` replay proving that the extracted value is non-empty and changes between iterations and the step now succeeds.
  - CLI `loadmeter heal <scenario> --report`, MCP `suggest_heals` (read-only) and `apply_heals` (writes, policy "validated" only).
  - Add `scenario` file section `fingerprints` (hashes of key paths and status per step) written by `debug_scenario`.
- **Data needed.** Stored fingerprints from a known-good validation; live target reachable (allowlisted).
- **Size.** L.
- **Safety.** Healing makes requests, so it uses the debug path (one VU, allowlist, rate limit). Never auto-applies; patches that would replace an extractor with a literal are rejected; log the proof (before/after values masked) as the audit record.

### 12. Grounded natural-language run summaries (verified narrative)

- **User value.** Everyone ships "AI run summary" (Gatling, Aviator, NeoLoad, Azure); all warn the text may be wrong. A summary where every number is machine-checked is a trust differentiator and cheap given `explain` exists.
- **Deterministic vs AI.** Deterministic: a `Facts` document (all numbers, anomalies from item 4, regression verdict from item 2, triage clusters from item 9, capacity result from item 1, hints from item 8). AI: wording only. A deterministic checker rejects the output if it contains a number, unit, or run id not in `Facts` (within rounding), or an unlabelled causal claim; on failure retry once then fall back to a template-based summary (also useful with AI off).
- **Implementation.**
  - `internal/narrate`: `BuildFacts(summary, extras) Facts` (stable JSON with IDs per fact), `Template(Facts) string` (no-AI fallback with Go `text/template`), `Prompt(Facts)` instructing the model to cite fact IDs inline (`[f12]`), `Verify(text, Facts)` regex-extracts numbers/percents/durations/ids and matches them; claims language: observation vs hypothesis tags mirror `results.Finding.Kind`.
  - Wire into `explain`, HTML report ("Summary" box with an "AI-written, numbers verified" badge or "template" badge), and MCP `get_results` summary field.
  - Test: golden Facts, injected hallucinated number must fail `Verify`.
- **Data needed.** Outputs of earlier items; existing `Summary`.
- **Size.** S-M.
- **Safety.** Facts are aggregated and redacted before any cloud call (existing consent flow). Prompt-injection: exemplars (from item 9) are treated as data, wrapped and length-capped; verification means an injected instruction cannot add numbers or actions. No tool calls from this path.

---

## 4. Considered but not in the top 12

| Idea | Why it ranks lower |
|---|---|
| Test data synthesis with LLM (BlazeMeter style) | Existing heuristic generator covers common cases; semantic generators (names, addresses, cards) can be shipped as deterministic seed lists without any LLM; valuable but lower than the above. Do the deterministic part (S) first: Luhn-valid test card numbers, locale names, realistic emails on reserved domains. Never produce data that looks like real PII. |
| AI scenario linting (Grafana lint) | A deterministic scenario linter (missing assertions, no think time, hardcoded tokens, unbounded loop) is S and should be done as a quick win, no AI. |
| Scheduled/recurring baselined runs | `loadmeter` + cron/CI already works; a built-in scheduler adds a daemon and safety questions. Revisit after items 2 and 3. |
| Core Web Vitals / browser load | Needs a browser engine; out of the stdlib-only constraint (see F031). |
| OTLP receiving | L, heavy protobuf without dependencies; Prometheus text/range query is enough first. |
| Autonomous "agent runs the whole test" | Competitors are moving there (NeoLoad Agentic Performance Testing, k6 `x agent`); Load Meter's MCP already exposes the tools. Keep human confirm on `run_test`; add only the finding/verify tools above. |

## 5. Suggested build order and dependencies

1. Item 2 (stats + baseline store) and item 4 (anomaly) in parallel; both need only existing data. They feed everything else.
2. Item 3 (SLA suggestion + gate + abort-on-breach) after 2.
3. Item 1 (capacity finder) reuses 3's abort rules and 4's saturation checks.
4. Items 9, 12 (triage, grounded summary) once 2, 4 exist.
5. Items 6, 7, 5 as a "realistic traffic" milestone (log parser first, then think time, then OpenAPI/mix).
6. Item 8 then 10 then 11 as the "diagnosis" milestone.

## 6. Open questions and caveats

- Several vendor pages are marketing or preview-status (Azure insights is "Preview"; k6 Studio AI wizard "experimental"; Gatling migration assistants "experimental" per a competitor). Treat capability claims as claims.
- Dates for NeoLoad AI Chat (March 2026), Aviator GA (CE 26.1) and BlazeMeter AI Anomaly Analysis v1.1 (January 2026) come from Gatling's own competitor blog [B]; I did not verify them against vendor release notes.
- Some search results (Octoperf, Checkly, Artillery AI, Locust AI plugins, Tricentis other tools) were not fetched; absence from the table is not proof of absence.
- Effort sizes are my estimates from reading the code, not measured. Item 1 assumes the engine can start and stop an arrival-rate step cleanly; this needs a short spike before committing.
- Statistical methods (Mann-Whitney, Kneedle, PELT, Theil-Sen, Drain-style templates) are standard published techniques from my own knowledge; I did not fetch papers for this document. Implement against reference datasets and keep unit tests with known answers.
