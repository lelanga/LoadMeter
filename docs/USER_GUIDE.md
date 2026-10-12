# Load Meter User Guide

This guide is for people who test software but do not write code. Every step shows the web UI first, then the command line.

## Words you will see

| Word | Plain meaning |
|---|---|
| **Scenario** | A saved test. It lists the requests a user makes, the data they use, and how much load to apply. |
| **Request** | One call to the system under test, like opening a page or posting a form. |
| **Transaction** | A named group of requests, like "login". Load Meter reports numbers per transaction. |
| **Virtual user (VU)** | A simulated person. Each VU repeats the scenario again and again. 50 VUs means 50 people using the system at the same time. |
| **Iteration** | One full pass through the scenario by one VU. |
| **Duration** | How long the test runs. |
| **Ramp-up** | The time it takes to go from zero users to full load. A slow ramp-up is gentler and shows where problems begin. |
| **Think time** | A pause between steps, like a person reading a page. Without it, VUs hammer the system much harder than real people do. |
| **Latency / response time** | How long one request takes, in milliseconds (ms). |
| **Percentile (p50, p90, p95, p99)** | p95 = 250 ms means 95 out of 100 requests were at least that fast. p50 is the typical request. p99 shows the slow tail. Load Meter computes percentiles from all samples. It never averages percentiles. |
| **RPS** | Requests per second. |
| **Error rate** | The share of requests that failed (bad status code, timeout, failed check). |
| **Correlation** | Handling values the server creates for each visit, like session IDs and anti-forgery tokens. A recording contains the old values. They must be read from the new response and sent back. Without this, replayed logins fail. |
| **Parameterization** | Replacing recorded values (user name, search term) with data from a file, so each VU uses different values. |
| **Assertion** | A check on a response, like "status is 200" or "body contains SKU-001". |
| **SLA** | A pass/fail goal for the run, like "p95 under 500 ms" or "error rate under 1 %". |
| **Closed vs open model** | Closed: a fixed number of users, and each waits for its last answer. Open: new iterations start at a fixed rate, no matter how slow the system is. |

## Before you start: stay safe

- Load Meter only tests `127.0.0.1` (your own machine) by default.
- To test any other system you must allow its host. Only do this for systems you own or have permission to test. See [SECURITY.md](SECURITY.md).

## 1. Start the UI

```bash
loadmeter serve --open
```

The terminal prints a URL with a token. Open exactly that URL. The UI listens only on your computer. Press Ctrl+C in the terminal to stop it.

Useful options: `--workspace DIR` (where your files live) and `--addr 127.0.0.1:7878`.

The left menu has: **Projects / Scenarios**, **Import**, **Run & Monitor**, **Results**, **AI Assistant**, **Settings**.

### Beginner and Advanced mode

The switch at the top right sets your experience level. Beginner hides expert fields (for example regex patterns, arrival rate, max VUs, and the Raw JSON tab). Advanced shows everything. Your choice is remembered in the browser.

## 2. Run the sample test

1. Open **Projects / Scenarios**.
2. Click **Run sample test**. Load Meter starts a small mock shop on your machine and creates a sample scenario for it.
3. Open **Run & Monitor**, pick the scenario, and click **Start test**.

CLI equivalent (one command, includes the mock shop):

```bash
loadmeter demo
```

Shorter and gentler:

```bash
loadmeter demo --vus 5 --duration 10s --ramp-up 3s
```

## 3. Bring in your own test

You can import three kinds of files. Open **Import**.

| Source | Button | CLI |
|---|---|---|
| HAR capture from your browser or from `loadmeter record` | Import HAR | `loadmeter import-har capture.har` |
| JMeter test plan (`.jmx`) | Import JMX | `loadmeter import-jmx plan.jmx` |
| LoadRunner VuGen **web** script folder | Import LoadRunner | `loadmeter import-lr ./MyScript` |

After the import you see a **compatibility report**. Each item is `supported`, `partial`, `unsupported` or `ignored`. Read the partial and unsupported lines. They say what you must fix by hand. Load Meter never runs JMeter scripts or LoadRunner C code. It only reads them. Details: [JMeter support matrix](../internal/jmx/SUPPORT_MATRIX.md) and [LoadRunner notes](../internal/loadrunner/README.md).

### How to get a HAR file

- In Chrome, Edge or Firefox: open DevTools, go to the **Network** tab, perform your user journey, then export as HAR ("Save all as HAR").
- Or record plain-HTTP traffic with Load Meter (next section).

By default the HAR import:

- removes static files (images, scripts, styles) and analytics or third-party CDN calls. Use `--include-static` to keep static files.
- replaces recorded credentials with variables. Use `--keep-credentials` only if you are explicitly authorized to keep them.
- groups requests into transactions.

```bash
loadmeter import-har capture.har --name checkout-flow
```

The command prints the scenario path, a compatibility report, correlation candidates and parameter suggestions.

## 4. Record with `loadmeter record`

`loadmeter record` starts a local proxy and saves what passes through it as a HAR file. It works for **plain HTTP** only. For HTTPS sites, export a HAR from your browser DevTools instead.

```bash
loadmeter record --out rec.har
```

1. Set your browser (or client) to use the HTTP proxy `127.0.0.1:8888`. Use `--listen 127.0.0.1:9000` to change the port.
2. Do your user journey.
3. Press Ctrl+C in the terminal. Load Meter saves the file.
4. Import it: `loadmeter import-har rec.har`.

The proxy only accepts a loopback listen address.

## 5. Review correlations

After a HAR import, the Import page shows the correlation candidates. The UI also has a correlation page for each scenario. Load Meter looks for values that appear in a response and are later sent back in a request, such as session IDs, CSRF tokens, auth tokens and IDs.

For each candidate you see:

- the variable name, with the value masked,
- where it comes from and where it is used,
- a confidence score from 0 to 1,
- a type: secret, session, identifier or other.

Tick the ones you want and click **Apply selected**. You choose an approval policy:

| Policy | What it does |
|---|---|
| `validated` (default) | Applies your selection, replays the scenario against the target, and keeps the change only if the replay passes. Rolls back otherwise. |
| `manual` | Applies your selection as you chose it. |
| `auto` | Applies every candidate with confidence 0.7 or higher, then validates. |
| `suggest` | Only suggests. |

Validation replays requests against the target. The target host must be allowed (loopback is always allowed).

CLI:

```bash
loadmeter correlate scenario.json                      # list candidates
loadmeter correlate scenario.json --apply all          # apply and validate
loadmeter correlate scenario.json --apply c1,c2 --policy validated
```

Candidate IDs are printed by the first command. If your scenario points at a variable like `${baseUrl}`, give it a value for validation: `--set baseUrl=http://127.0.0.1:8099`. If validation fails, the change is rolled back and the command exits with code 4.

The correlation step needs the recording saved during HAR import. See [KNOWN_LIMITATIONS.md](../KNOWN_LIMITATIONS.md) for gaps.

## 6. Parameterize test data

Load Meter also finds business inputs (user names, search terms, product IDs, amounts, dates) that should differ per user. Applying a suggestion writes a synthetic CSV file and rewrites the requests to use it.

```bash
loadmeter parameterize scenario.json                   # list suggestions
loadmeter parameterize scenario.json --apply all --rows 200
```

You can replace the generated CSV with real data. The first row is the header. Each column becomes a variable. See [SCENARIO_FORMAT.md](SCENARIO_FORMAT.md#data-sources).

## 7. Check the scenario before you run

In the editor, **Validate** checks the file and lists the target hosts and whether each is allowed. **Debug** runs exactly one iteration with one user and shows every request and response (secrets are redacted), the values that were extracted, and the result of each assertion. Fix problems here, before you add load.

The editor tabs are **Steps**, **General & data**, **Workload**, **SLAs** and (Advanced only) **Raw JSON**.

CLI:

```bash
loadmeter validate scenario.json
```

## 8. Set users, duration and ramp-up

On **Run & Monitor** you can override the scenario's settings for one run:

| Field | Meaning |
|---|---|
| Virtual users | How many simulated users at full load |
| Duration | Total time, like `30s`, `5m`, `1h` |
| Ramp-up | Time to reach full load. Must be shorter than the duration |
| Rate (Advanced) | Start this many iterations per second instead of using a fixed number of users |
| Max VUs (Advanced) | Size of the user pool for rate tests |
| Compare with baseline | An earlier run to compare against |

Pick **users** (closed model) to ask "how does it behave with N people?". Pick **rate** (open model) to ask "how does it behave when N iterations arrive every second?". They are different questions. Do not set both.

CLI flags (they override the scenario file):

```bash
loadmeter run scenario.json --vus 50 --duration 5m --ramp-up 1m
loadmeter run scenario.json --rate 20 --duration 5m --max-vus 100
```

Notes:

- `--ramp-up` goes with `--vus` and must be shorter than `--duration`.
- A `--duration` alone works only for scenarios that already use a fixed-user or fixed-rate workload.
- `--set baseUrl=http://127.0.0.1:8080` changes a scenario variable for this run.
- `--quiet` hides live progress. `--json` prints the summary as JSON.

## 9. Run and watch live

Click **Start test** on **Run & Monitor**. The live dashboard updates every second: active users, requests per second, response-time percentiles, error rate, and charts. Click **STOP TEST** to stop gracefully. Click again to force a stop.

If a target is not on your computer, Load Meter shows a warning. A test with many users against a non-local host asks you to confirm first. A host that is not allowed is refused. See [SECURITY.md](SECURITY.md).

CLI:

```bash
loadmeter run scenario.json --vus 20 --duration 2m
```

While it runs you see a line every 2 seconds. Press Ctrl+C once to stop gracefully, twice to abort in-flight requests.

For a non-local target:

```bash
loadmeter run scenario.json --allow-host staging.example.com --yes
```

`--allow-host` allows the host for this command only. `--yes` confirms a high-impact test (see the policy field `requireConfirmAbove`).

Exit codes of `loadmeter run`: `0` success, `3` when at least one SLA failed, `1` for other errors.

## 10. Read the report

Results appear in **Results**, and every run writes files to `<workspace>/runs/<run-id>/`:

| File | Content |
|---|---|
| `report.html` | The readable report |
| `summary.json` / `summary.csv` | Totals, percentiles, per-transaction numbers, SLA results |
| `raw.csv` | Individual samples. JMeter JTL-compatible leading columns (`timeStamp` ... `Latency`, `URL`), then `kind`, `Connect` (ms from sample start to connection ready, 0 when reused), `dnsMs`, `tlsMs`, `connReused`, `retries` (hidden transport retries) and `bodyBytes` (decoded body; `bytes` is on the wire) |
| `scenario.json` | A copy of the scenario that was run |

The terminal summary and the report show:

- **requests, errors, error rate, rps, iterations, dropped.** "Dropped" counts iterations that could not start in an open-model test because all users were busy.
- **latency**: min, avg, p50, p90, p95, p99, max.
- **per transaction**: count, errors, p50, p95, p99.
- **SLA lines**: `PASS` or `FAIL`, with the actual value.
- **Findings**: each is marked `observation` (backed by measured data) or `hypothesis` (a guess to check).

List runs and re-create a report:

```bash
loadmeter runs
loadmeter report <run-id>
```

How to read it:

- Look at p95 and p99, not only the average. A good average can hide slow requests.
- Check the error rate first. Fast errors make latency look good.
- Compare transactions. One slow transaction is easier to fix than "the site is slow".
- A tiny test against the local mock server shows sub-millisecond numbers. These say nothing about a real system.

## 11. Compare runs

In **Results**, tick exactly two runs and click **Compare selected**. The older run is the baseline.

CLI:

```bash
loadmeter compare <baseline-run-id> <run-id>
```

Load Meter prints each metric (p50, p95, p99, error rate, rps) for the whole run and for each transaction, with the percent change. A latency regression is more than 10 % and more than 5 ms worse. An error regression is a rise of 1 percentage point or more. The command exits with code 3 if any regression is found.

You can also add a baseline when you run: `loadmeter run scenario.json --baseline <run-id>`, or when re-rendering: `loadmeter report <run-id> --baseline <other-run-id>`.

## 12. Export to JMeter

```bash
loadmeter export-jmx scenario.json -o plan.jmx
```

Load Meter prints a compatibility report listing what could not be preserved. The scenario must be in the workspace.

## 13. AI help (optional)

AI is off by default. See [AI_AND_MCP.md](AI_AND_MCP.md). Examples:

```bash
loadmeter ask "validate shop-login.json" --file shop-login.json
loadmeter explain <run-id>
```

## CLI cheat sheet

| Task | Command |
|---|---|
| Check install | `loadmeter doctor` |
| Make a workspace with the sample | `loadmeter init` |
| Start a mock target on 127.0.0.1:8099 | `loadmeter mock` |
| One-click demo | `loadmeter demo` |
| UI | `loadmeter serve --open` |
| Record plain HTTP | `loadmeter record --out rec.har` |
| Import | `loadmeter import-har f.har`, `loadmeter import-jmx f.jmx`, `loadmeter import-lr ./dir` |
| Correlate | `loadmeter correlate s.json --apply all` |
| Parameterize | `loadmeter parameterize s.json --apply all --rows 100` |
| Validate | `loadmeter validate s.json` |
| Run | `loadmeter run s.json --vus 50 --duration 5m --ramp-up 1m` |
| List runs | `loadmeter runs` |
| Report | `loadmeter report <run-id>` |
| Compare | `loadmeter compare <base-id> <run-id>` |
| Export | `loadmeter export-jmx s.json -o plan.jmx` |

Global option: `--workspace DIR` (default `./loadmeter-workspace`, or `$LOADMETER_WORKSPACE`).

## Try the whole flow locally

```bash
loadmeter init
loadmeter mock &                       # mock shop on 127.0.0.1:8099
loadmeter validate loadmeter-workspace/scenarios/shop-login.json
loadmeter run loadmeter-workspace/scenarios/shop-login.json --vus 5 --duration 10s --ramp-up 3s
loadmeter runs
```

Stop the mock server afterwards (`kill %1`).

## Troubleshooting

| Problem | Fix |
|---|---|
| `target host "x" is not in the allowlist` | Only add hosts you are authorized to test: `--allow-host x`, or edit the policy. See [SECURITY.md](SECURITY.md). |
| `workload needs N VUs but the safety policy allows M` | Lower the users, or raise `maxVUs` in `policy.json` if authorized. |
| `ramp-up must be shorter than the duration` | Use a shorter ramp-up or a longer duration. |
| Many errors right after login | A value probably needs correlation. Run **Debug** (UI) and look at the failing request. |
| UI says 403 / token error | Open the full URL printed by `loadmeter serve`, including `?token=`. |
| `cannot listen on 127.0.0.1:7878` | Another Load Meter is running. Use `--addr 127.0.0.1:0` to pick a free port. |
| Low open-file limit warning in `doctor` | Run `ulimit -n 65535` before large tests. |

## Converting between formats

### Import an OpenAPI spec

```
loadmeter import-openapi spec.json [--name N] [--tags a,b] [--exclude-tags x]
    [--operations opId1,opId2] [--group-by tag|path]
    [--include-deprecated] [--include-destructive] [--json]
```

Reads an OpenAPI 3.0/3.1 **JSON** document (max 20 MB; convert YAML first, for example with `yq -o=json`).

- One transaction per tag (or per path), one request per operation.
- Path parameters become `${param}` variables seeded from examples.
- JSON bodies are built from examples or schemas (uuid, date-time and email formats become dynamic functions).
- The status assertion comes from the first 2xx response; `servers[0].url` becomes the `baseUrl` variable.
- Security schemes become headers with `${secret_*}` placeholders. Set them before running, for example with `${__env(LOADMETER_TOKEN)}`.
- Deprecated and DELETE operations are skipped unless you include them.
- Only local `#/` references are resolved; cyclic references are truncated and reported.

MCP tool: `import_openapi`.

### Export to k6

```
loadmeter export-k6 scenario.json [-o out.js]
```

Writes a self-contained k6 script (no remote imports) with executors, thresholds from SLAs, extractors, checks and CSV data. The compatibility report lists anything k6 cannot express. Run it with `k6 run out.js`, keeping CSV files at the path the script opens. k6 does not apply this tool's host allowlist, so only test systems you are authorized to test. MCP tool: `export_k6`.

## Distributed testing

One machine is not always enough. Load Meter can split a test across several load agents running the same binary: no RMI, no extra ports, no Java.

**1. Start an agent on each load machine**

    loadmeter agent --token-file ~/.loadmeter/agent.token [--listen 0.0.0.0:7979] [--allow-host api.test.example]

- The first start creates a random token file (0600) and a self-signed TLS certificate in `~/.loadmeter/agent/`, and prints the certificate's SHA-256 fingerprint plus a ready-to-copy controller command. `loadmeter agent --fingerprint` prints it again.
- The agent refuses to start without a token. Agents are the only component meant to listen on non-loopback addresses; all traffic is HTTPS with the token.
- Each agent enforces its **own** safety policy (its `policy.json` and `--allow-host`). The controller cannot widen it.

**2. Copy the token file to the controller**, then run:

    loadmeter run scenario.json --vus 300 --duration 10m \
      --agents 10.0.0.11:7979#<fingerprint>,10.0.0.12:7979#<fingerprint> \
      --agent-token-file ./agent.token [--local-share 0.2]

or list agents in a file and use `--agents-file agents.json`:

    {"tokenFile": "agent.token",
     "agents": [{"id": "lg-1", "addr": "10.0.0.11:7979", "fingerprint": "<sha256>"},
                {"id": "lg-2", "addr": "10.0.0.12:7979", "fingerprint": "<sha256>"}]}

What happens:

- Certificate fingerprints are checked; a mismatch stops the run before any load.
- VUs, arrival rate and maxVUs are split across agents; `--local-share` runs a fraction on the controller.
- CSV data travels with the scenario; `mode: "unique"` gives each node its own slice.
- All nodes start together, corrected for measured clock offset (warning above 250 ms; keep NTP on).
- Per-second histograms are merged, so percentiles are exact across agents, not averages of averages. The summary and report include a per-node table.
- Ctrl+C stops every node (twice aborts in-flight requests). A lost agent is recorded as a finding and the run continues on the others; an agent that loses its controller stops immediately.

## Replay production traffic from an access log

`loadmeter import-log access.log --base-url http://127.0.0.1:8099` builds a scenario from an nginx or Apache combined/common log, an AWS ALB log or a CloudFront standard log (gzip works, format auto-detected).

- One weighted group with a request step per endpoint; the weight is the endpoint's share of traffic. Path ids (numbers, UUIDs, hashes) are folded into `{id}` and replaced with random values at run time.
- The load follows the logged requests per minute as a ramping arrival rate, scaled by `--scale` (default 0.1 = 10%).
- `--window peak` (busiest 15 minutes, default), `--window avg`, or `--window HH:MM-HH:MM`.
- Only GET/HEAD by default. `--include-writes` replays writes with an empty body. Query strings are dropped unless `--keep-query` (secrets redacted). Static assets and 4xx/5xx requests are skipped (`--include-static`, `--include-errors`). `--top N` keeps the N most frequent endpoints.
- `--base-url` is required. Loopback is accepted; any other host needs `--allow-host`, and the run-time allowlist still applies. Replaying real traffic patterns generates real load: only test systems you are authorized to test.
- Think time: gaps between a visitor's requests are fitted with exponential, log-normal and gamma distributions (maximum likelihood, best by AIC; needs at least 50 gaps). `--journey` turns each iteration into one session separated by that fitted think time.
- Weighted choice in a scenario file: `{"type":"group","select":"weighted","steps":[{"type":"request","weight":70,...},{"type":"request","weight":30,...}]}`. A think step can carry `"distribution":{"type":"lognormal","mu":2.3,"sigma":1.1}` with `min`/`max` clamps.

MCP tool: `import_access_log`.

## Why did my test fail? Failure triage
A single error rate often hides several different problems. `loadmeter triage <run-id>` groups the failed requests of a finished run by similar message, error kind, HTTP status and request, and ranks the groups.

For each group you see how many requests failed and what share of all failures that is. You also see when it first and last appeared, an example message, a priority score from 0 to 100, which side it most likely comes from, a short explanation and a suggested next check.

Possible sides:
- **Test script**: a value that was never found, an assertion on something that never appeared, an unfilled variable, a rejected login or token.
- **Target system**: server errors, timeouts, resets, rate limiting.
- **Load generator**: the test machine ran out of sockets or CPU.
- **Safety policy**: Load Meter's allowlist refused the request.

The priority score rewards groups that cover many failures, started early, started when errors or latency jumped, or hit a request or transaction that has an SLA. The side is a rule-based hint about where to look first, not proof. The same data always gives the same result, and no AI is involved. Use `--top N` to show more or fewer groups and `--json` for machine-readable output.

The HTML report, the run summary page in the web UI ("Failures" card), the API (`GET /api/runs/{id}/triage`) and the MCP tool `triage_run` show the same result. Runs started without raw samples (no `raw.csv`) get only a coarse grouping by request and error kind.

## Plain-English run summary
`loadmeter summarize <run-id> [--baseline <run-id>]` prints a summary of at most 12 lines for people who do not read latency tables. Every number comes from the run's recorded results. It works without AI.

Add `--ai` to let your configured AI provider reword the summary. Only a small facts document (aggregated numbers and failure group descriptions, no raw requests or responses) is sent, and only if AI is enabled and, for cloud providers, cloud consent is given; local-only mode, call limits and the monthly budget apply as for every other AI feature. The reworded text is checked automatically: if it contains any number that is not in the facts (allowing for rounding), it is discarded, the built-in summary is shown and the reason is printed. The web UI offers the same through "Reword with AI" on the Summary card, and the MCP tool `summarize_run` returns the built-in summary and the facts.

## Find the capacity (breaking point)

`loadmeter capacity` answers "how much load can this take while still meeting my SLAs?". It runs the scenario as an open-model load at increasing rates (a staircase), judges each step on its own measurements, and narrows the answer by bisection.

    loadmeter capacity scenario.json --p95 300 --max-error-pct 1 --max-rate 200

- Rates are scenario iterations per second; requests/s is shown next to them.
- Criteria come from the scenario's `slas` plus `--p95 <ms>` and `--max-error-pct <pct>`. With none, the command refuses to run. `rps` SLAs are ignored.
- Flags: `--start-rate 10`, `--step-pct 25` (or `--step-add N`), `--warmup 5s` (discarded), `--hold 20s` (measured), `--max-rate N`, `--breach-steps 2`, `--refine 3`, `--budget 30m`, `--max-vus`, `--json`. Ctrl+C saves the partial result.
- Step verdicts: pass; breach; error-storm (50%+ errors, stops at once); generator-limited (Load Meter CPU above 85% or VU pool exhausted while latency fine; result is then a lower bound).
- Output: max sustainable rate ("measured" or "lower-bound"), first breached criterion, knee (Kneedle) and plateau, confidence, limitations, per-step table. Saved to `<workspace>/capacity/<id>/capacity.json` and `report.html`.
- Sizing estimate: `--target-rate N --headroom 30 --instances-tested 1`. Estimates under printed assumptions; no prices.
- Safety: same policy as `run`. Non-local targets need `--allow-host`, `--max-rate` and `--yes`. Loopback without `--max-rate` stops at 500 it/s.
- Also: MCP `find_capacity` (needs confirm), `capacity_status`, `stop_capacity`; API `POST /api/capacity`, `GET /api/capacity/active`.

## Comparing runs statistically

`loadmeter compare <baseline-run-id> <run-id> [--alpha 0.05] [--min-effect-pct 5] [--min-effect-ms 2] [--json]`

Compares two runs per metric, for the whole run and each transaction, using per-second windows (p50/p95/p99). Partial first/last seconds and the warm-up are dropped. Transactions are rebuilt from each run's `raw.csv`.

Verdicts:
- **regression / improvement**: significant, 95% CI excludes zero, and at least `--min-effect-pct` and `--min-effect-ms` (error rate: +1 percentage point).
- **no-change**: not significant, or too small to matter (the reason says which).
- **inconclusive**: fewer than 10 usable seconds per run.

Tests: Mann-Whitney U (corrected for correlation between seconds), block-bootstrap CI of the median shift, Cliff's delta; two-proportion test for error rate; Holm-adjusted p-values. Throughput is informational only. A warning is printed when workloads differ. `compare` exits 3 on any regression. `run --baseline` accepts the same flags.

## CI quality gate

`loadmeter gate <run-id> [--baseline <run-id>] [--junit out.xml] [--markdown out.md] [--json]`

Exit codes: `0` pass; `3` fail (SLA failed, run failed/aborted, or regression); `4` inconclusive; `1` error. Note `run --baseline` still exits 0 on a regression if all SLAs pass; use `gate` in CI.

## Suggest SLAs from baseline runs

`loadmeter suggest-slas <run-id> [--runs id2,id3] [--margin 20] [--tx] [--json] [--write scenario.json [--replace] [--yes]]`

Proposes p95/p99 limits (max of whole-run percentile and per-second median + 3 x MAD, plus margin, rounded up), an error ceiling (2 x Wilson 95% upper bound, min 0.1%), and with `--tx` per-transaction p95. Low confidence for short, error-heavy or generator-limited runs. `--write` shows a diff and asks first; non-interactive needs `--yes`; existing SLAs kept unless `--replace`.

## Stop a run on SLA breach

`loadmeter run scenario.json --abort-on-breach [--abort-after 10] [--abort-min-requests 20]`

Gracefully stops when an all-requests SLA is breached for N consecutive complete seconds. Status `aborted-sla`, exit 3. Not wired into distributed runs.

## Timeline findings

Diagnose now reports p95/error-rate level shifts (PELT) with before/after levels and active VUs, slow drift under constant load, and generator memory growth.

## HTTP behaviour and JMeter parity
- **Connections.** Closed-model tests (`constant-vus`, `ramping-vus`, `per-vu-iterations`) give each user its own connections, like JMeter threads: 100 users open ~100 connections, each with its own TLS handshake. Arrival-rate tests share one pool. Set `defaults.connections` to `shared` or `per-vu`, `httpVersion: "1.1"` to match JMeter, `keepAlive: false` for a connection per request. Connections recycle after 60 s (`maxConnLifetime`).
- **Compressed responses.** gzip/deflate are always decoded, even with an imported `Accept-Encoding`; `br`/`zstd` are removed from that header (not decodable). Bytes are wire bytes.
- **Timing breakdown.** Raw samples include connect, DNS, TLS and connection reuse. `responseHeaderTimeout` fails slow first bytes (`ttfb_timeout`); `{"type":"max_latency","expr":"ttfb","value":"500ms"}` asserts on TTFB.
- **Hidden retries.** Go may silently resend a GET on a closed keep-alive connection. They are counted (`retries` column). `hiddenRetries: "fail"` reports them as failures, like JMeter.
- **Auth, certificates, proxies.** `defaults.auth` (basic, digest, bearer) with secrets only as `${secret_*}` or `${__env(...)}`; client certs in `defaults.tls`; static hosts in `defaults.dns.hosts`; proxy in `defaults.proxy`. The allowlist checks host names, DNS-override IPs, the proxy and every target behind it.
