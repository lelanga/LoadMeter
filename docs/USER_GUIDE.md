# Phoenix Performance User Guide

This guide is for people who test software but do not write code. Every step shows the web UI first, then the command line.

## Words you will see

| Word | Plain meaning |
|---|---|
| **Scenario** | A saved test. It lists the requests a user makes, the data they use, and how much load to apply. |
| **Request** | One call to the system under test, like opening a page or posting a form. |
| **Transaction** | A named group of requests, like "login". Phoenix reports numbers per transaction. |
| **Virtual user (VU)** | A simulated person. Each VU repeats the scenario again and again. 50 VUs means 50 people using the system at the same time. |
| **Iteration** | One full pass through the scenario by one VU. |
| **Duration** | How long the test runs. |
| **Ramp-up** | The time it takes to go from zero users to full load. A slow ramp-up is gentler and shows where problems begin. |
| **Think time** | A pause between steps, like a person reading a page. Without it, VUs hammer the system much harder than real people do. |
| **Latency / response time** | How long one request takes, in milliseconds (ms). |
| **Percentile (p50, p90, p95, p99)** | p95 = 250 ms means 95 out of 100 requests were at least that fast. p50 is the typical request. p99 shows the slow tail. Phoenix computes percentiles from all samples. It never averages percentiles. |
| **RPS** | Requests per second. |
| **Error rate** | The share of requests that failed (bad status code, timeout, failed check). |
| **Correlation** | Handling values the server creates for each visit, like session IDs and anti-forgery tokens. A recording contains the old values. They must be read from the new response and sent back. Without this, replayed logins fail. |
| **Parameterization** | Replacing recorded values (user name, search term) with data from a file, so each VU uses different values. |
| **Assertion** | A check on a response, like "status is 200" or "body contains SKU-001". |
| **SLA** | A pass/fail goal for the run, like "p95 under 500 ms" or "error rate under 1 %". |
| **Closed vs open model** | Closed: a fixed number of users, and each waits for its last answer. Open: new iterations start at a fixed rate, no matter how slow the system is. |

## Before you start: stay safe

- Phoenix only tests `127.0.0.1` (your own machine) by default.
- To test any other system you must allow its host. Only do this for systems you own or have permission to test. See [SECURITY.md](SECURITY.md).

## 1. Start the UI

```bash
phoenix serve --open
```

The terminal prints a URL with a token. Open exactly that URL. The UI listens only on your computer. Press Ctrl+C in the terminal to stop it.

Useful options: `--workspace DIR` (where your files live) and `--addr 127.0.0.1:7878`.

The left menu has: **Projects / Scenarios**, **Import**, **Run & Monitor**, **Results**, **AI Assistant**, **Settings**.

### Beginner and Advanced mode

The switch at the top right sets your experience level. Beginner hides expert fields (for example regex patterns, arrival rate, max VUs, and the Raw JSON tab). Advanced shows everything. Your choice is remembered in the browser.

## 2. Run the sample test

1. Open **Projects / Scenarios**.
2. Click **Run sample test**. Phoenix starts a small mock shop on your machine and creates a sample scenario for it.
3. Open **Run & Monitor**, pick the scenario, and click **Start test**.

CLI equivalent (one command, includes the mock shop):

```bash
phoenix demo
```

Shorter and gentler:

```bash
phoenix demo --vus 5 --duration 10s --ramp-up 3s
```

## 3. Bring in your own test

You can import three kinds of files. Open **Import**.

| Source | Button | CLI |
|---|---|---|
| HAR capture from your browser or from `phoenix record` | Import HAR | `phoenix import-har capture.har` |
| JMeter test plan (`.jmx`) | Import JMX | `phoenix import-jmx plan.jmx` |
| LoadRunner VuGen **web** script folder | Import LoadRunner | `phoenix import-lr ./MyScript` |

After the import you see a **compatibility report**. Each item is `supported`, `partial`, `unsupported` or `ignored`. Read the partial and unsupported lines. They say what you must fix by hand. Phoenix never runs JMeter scripts or LoadRunner C code. It only reads them. Details: [JMeter support matrix](../internal/jmx/SUPPORT_MATRIX.md) and [LoadRunner notes](../internal/loadrunner/README.md).

### How to get a HAR file

- In Chrome, Edge or Firefox: open DevTools, go to the **Network** tab, perform your user journey, then export as HAR ("Save all as HAR").
- Or record plain-HTTP traffic with Phoenix (next section).

By default the HAR import:

- removes static files (images, scripts, styles) and analytics or third-party CDN calls. Use `--include-static` to keep static files.
- replaces recorded credentials with variables. Use `--keep-credentials` only if you are explicitly authorized to keep them.
- groups requests into transactions.

```bash
phoenix import-har capture.har --name checkout-flow
```

The command prints the scenario path, a compatibility report, correlation candidates and parameter suggestions.

## 4. Record with `phoenix record`

`phoenix record` starts a local proxy and saves what passes through it as a HAR file. It works for **plain HTTP** only. For HTTPS sites, export a HAR from your browser DevTools instead.

```bash
phoenix record --out rec.har
```

1. Set your browser (or client) to use the HTTP proxy `127.0.0.1:8888`. Use `--listen 127.0.0.1:9000` to change the port.
2. Do your user journey.
3. Press Ctrl+C in the terminal. Phoenix saves the file.
4. Import it: `phoenix import-har rec.har`.

The proxy only accepts a loopback listen address.

## 5. Review correlations

After a HAR import, the Import page shows the correlation candidates. The UI also has a correlation page for each scenario. Phoenix looks for values that appear in a response and are later sent back in a request, such as session IDs, CSRF tokens, auth tokens and IDs.

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
phoenix correlate scenario.json                      # list candidates
phoenix correlate scenario.json --apply all          # apply and validate
phoenix correlate scenario.json --apply c1,c2 --policy validated
```

Candidate IDs are printed by the first command. If your scenario points at a variable like `${baseUrl}`, give it a value for validation: `--set baseUrl=http://127.0.0.1:8099`. If validation fails, the change is rolled back and the command exits with code 4.

The correlation step needs the recording saved during HAR import. See [KNOWN_LIMITATIONS.md](../KNOWN_LIMITATIONS.md) for gaps.

## 6. Parameterize test data

Phoenix also finds business inputs (user names, search terms, product IDs, amounts, dates) that should differ per user. Applying a suggestion writes a synthetic CSV file and rewrites the requests to use it.

```bash
phoenix parameterize scenario.json                   # list suggestions
phoenix parameterize scenario.json --apply all --rows 200
```

You can replace the generated CSV with real data. The first row is the header. Each column becomes a variable. See [SCENARIO_FORMAT.md](SCENARIO_FORMAT.md#data-sources).

## 7. Check the scenario before you run

In the editor, **Validate** checks the file and lists the target hosts and whether each is allowed. **Debug** runs exactly one iteration with one user and shows every request and response (secrets are redacted), the values that were extracted, and the result of each assertion. Fix problems here, before you add load.

The editor tabs are **Steps**, **General & data**, **Workload**, **SLAs** and (Advanced only) **Raw JSON**.

CLI:

```bash
phoenix validate scenario.json
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
phoenix run scenario.json --vus 50 --duration 5m --ramp-up 1m
phoenix run scenario.json --rate 20 --duration 5m --max-vus 100
```

Notes:

- `--ramp-up` goes with `--vus` and must be shorter than `--duration`.
- A `--duration` alone works only for scenarios that already use a fixed-user or fixed-rate workload.
- `--set baseUrl=http://127.0.0.1:8080` changes a scenario variable for this run.
- `--quiet` hides live progress. `--json` prints the summary as JSON.

## 9. Run and watch live

Click **Start test** on **Run & Monitor**. The live dashboard updates every second: active users, requests per second, response-time percentiles, error rate, and charts. Click **STOP TEST** to stop gracefully. Click again to force a stop.

If a target is not on your computer, Phoenix shows a warning. A test with many users against a non-local host asks you to confirm first. A host that is not allowed is refused. See [SECURITY.md](SECURITY.md).

CLI:

```bash
phoenix run scenario.json --vus 20 --duration 2m
```

While it runs you see a line every 2 seconds. Press Ctrl+C once to stop gracefully, twice to abort in-flight requests.

For a non-local target:

```bash
phoenix run scenario.json --allow-host staging.example.com --yes
```

`--allow-host` allows the host for this command only. `--yes` confirms a high-impact test (see the policy field `requireConfirmAbove`).

Exit codes of `phoenix run`: `0` success, `3` when at least one SLA failed, `1` for other errors.

## 10. Read the report

Results appear in **Results**, and every run writes files to `<workspace>/runs/<run-id>/`:

| File | Content |
|---|---|
| `report.html` | The readable report |
| `summary.json` / `summary.csv` | Totals, percentiles, per-transaction numbers, SLA results |
| `raw.csv` | Individual samples |
| `scenario.json` | A copy of the scenario that was run |

The terminal summary and the report show:

- **requests, errors, error rate, rps, iterations, dropped.** "Dropped" counts iterations that could not start in an open-model test because all users were busy.
- **latency**: min, avg, p50, p90, p95, p99, max.
- **per transaction**: count, errors, p50, p95, p99.
- **SLA lines**: `PASS` or `FAIL`, with the actual value.
- **Findings**: each is marked `observation` (backed by measured data) or `hypothesis` (a guess to check).

List runs and re-create a report:

```bash
phoenix runs
phoenix report <run-id>
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
phoenix compare <baseline-run-id> <run-id>
```

Phoenix prints each metric (p50, p95, p99, error rate, rps) for the whole run and for each transaction, with the percent change. A latency regression is more than 10 % and more than 5 ms worse. An error regression is a rise of 1 percentage point or more. The command exits with code 3 if any regression is found.

You can also add a baseline when you run: `phoenix run scenario.json --baseline <run-id>`, or when re-rendering: `phoenix report <run-id> --baseline <other-run-id>`.

## 12. Export to JMeter

```bash
phoenix export-jmx scenario.json -o plan.jmx
```

Phoenix prints a compatibility report listing what could not be preserved. The scenario must be in the workspace.

## 13. AI help (optional)

AI is off by default. See [AI_AND_MCP.md](AI_AND_MCP.md). Examples:

```bash
phoenix ask "validate shop-login.json" --file shop-login.json
phoenix explain <run-id>
```

## CLI cheat sheet

| Task | Command |
|---|---|
| Check install | `phoenix doctor` |
| Make a workspace with the sample | `phoenix init` |
| Start a mock target on 127.0.0.1:8099 | `phoenix mock` |
| One-click demo | `phoenix demo` |
| UI | `phoenix serve --open` |
| Record plain HTTP | `phoenix record --out rec.har` |
| Import | `phoenix import-har f.har`, `phoenix import-jmx f.jmx`, `phoenix import-lr ./dir` |
| Correlate | `phoenix correlate s.json --apply all` |
| Parameterize | `phoenix parameterize s.json --apply all --rows 100` |
| Validate | `phoenix validate s.json` |
| Run | `phoenix run s.json --vus 50 --duration 5m --ramp-up 1m` |
| List runs | `phoenix runs` |
| Report | `phoenix report <run-id>` |
| Compare | `phoenix compare <base-id> <run-id>` |
| Export | `phoenix export-jmx s.json -o plan.jmx` |

Global option: `--workspace DIR` (default `./phoenix-workspace`, or `$PHOENIX_WORKSPACE`).

## Try the whole flow locally

```bash
phoenix init
phoenix mock &                       # mock shop on 127.0.0.1:8099
phoenix validate phoenix-workspace/scenarios/shop-login.json
phoenix run phoenix-workspace/scenarios/shop-login.json --vus 5 --duration 10s --ramp-up 3s
phoenix runs
```

Stop the mock server afterwards (`kill %1`).

## Troubleshooting

| Problem | Fix |
|---|---|
| `target host "x" is not in the allowlist` | Only add hosts you are authorized to test: `--allow-host x`, or edit the policy. See [SECURITY.md](SECURITY.md). |
| `workload needs N VUs but the safety policy allows M` | Lower the users, or raise `maxVUs` in `policy.json` if authorized. |
| `ramp-up must be shorter than the duration` | Use a shorter ramp-up or a longer duration. |
| Many errors right after login | A value probably needs correlation. Run **Debug** (UI) and look at the failing request. |
| UI says 403 / token error | Open the full URL printed by `phoenix serve`, including `?token=`. |
| `cannot listen on 127.0.0.1:7878` | Another Phoenix is running. Use `--addr 127.0.0.1:0` to pick a free port. |
| Low open-file limit warning in `doctor` | Run `ulimit -n 65535` before large tests. |

## Converting between formats

### Import an OpenAPI spec

```
phoenix import-openapi spec.json [--name N] [--tags a,b] [--exclude-tags x]
    [--operations opId1,opId2] [--group-by tag|path]
    [--include-deprecated] [--include-destructive] [--json]
```

Reads an OpenAPI 3.0/3.1 **JSON** document (max 20 MB; convert YAML first, for example with `yq -o=json`).

- One transaction per tag (or per path), one request per operation.
- Path parameters become `${param}` variables seeded from examples.
- JSON bodies are built from examples or schemas (uuid, date-time and email formats become dynamic functions).
- The status assertion comes from the first 2xx response; `servers[0].url` becomes the `baseUrl` variable.
- Security schemes become headers with `${secret_*}` placeholders. Set them before running, for example with `${__env(PHOENIX_TOKEN)}`.
- Deprecated and DELETE operations are skipped unless you include them.
- Only local `#/` references are resolved; cyclic references are truncated and reported.

MCP tool: `import_openapi`.

### Export to k6

```
phoenix export-k6 scenario.json [-o out.js]
```

Writes a self-contained k6 script (no remote imports) with executors, thresholds from SLAs, extractors, checks and CSV data. The compatibility report lists anything k6 cannot express. Run it with `k6 run out.js`, keeping CSV files at the path the script opens. k6 does not apply this tool's host allowlist, so only test systems you are authorized to test. MCP tool: `export_k6`.

## Distributed testing

One machine is not always enough. Phoenix can split a test across several load agents running the same binary: no RMI, no extra ports, no Java.

**1. Start an agent on each load machine**

    phoenix agent --token-file ~/.phoenix/agent.token [--listen 0.0.0.0:7979] [--allow-host api.test.example]

- The first start creates a random token file (0600) and a self-signed TLS certificate in `~/.phoenix/agent/`, and prints the certificate's SHA-256 fingerprint plus a ready-to-copy controller command. `phoenix agent --fingerprint` prints it again.
- The agent refuses to start without a token. Agents are the only component meant to listen on non-loopback addresses; all traffic is HTTPS with the token.
- Each agent enforces its **own** safety policy (its `policy.json` and `--allow-host`). The controller cannot widen it.

**2. Copy the token file to the controller**, then run:

    phoenix run scenario.json --vus 300 --duration 10m \
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
