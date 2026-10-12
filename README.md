# Phoenix Performance

Phoenix Performance is a load-testing tool in one file. You give it a recording of a user journey (or a JMeter plan, or a LoadRunner script). It replays that journey with many virtual users and shows you how fast and how stable your system is.

- One static binary called `phoenix`. Go standard library only. No runtime to install.
- A web UI is built into the binary. It runs on your own computer.
- A command line for everything the UI does.
- Optional AI help (off by default). Core features work without it.
- Safe by default: it only tests `127.0.0.1` until you allow other hosts.

> **Status: 0.1.0-dev.** This is early software. Read [KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md) before you rely on it.

## 60-second quickstart

**1. Get the binary.** Pick one:

```bash
# Build from source (needs Go 1.24)
cd phoenix
go build -o bin/phoenix ./cmd/phoenix

# or use a prebuilt file from dist/ (see docs/INSTALL.md)
```

**2. Check it works.**

```bash
bin/phoenix doctor
```

**3. Run the built-in demo.** This starts a mock shop on `127.0.0.1` and load-tests it. Nothing leaves your machine.

```bash
bin/phoenix demo --vus 5 --duration 10s --ramp-up 3s
```

You get a summary in the terminal and an HTML report path at the end.

**4. Open the web UI.**

```bash
bin/phoenix serve --open
```

`serve` prints a URL with a one-time token, for example `http://127.0.0.1:7878/?token=...`. Open that exact URL if the browser does not open by itself. Click **Run sample test**.

## What it can do

| Area | Feature |
|---|---|
| Import | HAR captures, JMeter `.jmx` plans (with a compatibility report), LoadRunner VuGen Web scripts (static analysis only) |
| Record | `phoenix record` runs a local proxy for plain-HTTP traffic and saves a HAR |
| Fix-up | Suggests correlation rules (dynamic tokens and IDs) and test-data parameters. You review and apply them. |
| Workloads | Closed model (fixed users) and open model (fixed arrival rate). Five executors, including ramping. |
| Scenario format | A readable JSON format, `phoenix/v1`. Extractors, assertions, CSV data, loops, conditions, think times |
| Results | Live dashboard, p50/p90/p95/p99 from a full histogram, per-transaction numbers, SLA pass/fail, HTML report, CSV and JSON exports |
| Compare | Compare two runs and flag regressions |
| Export | Export a scenario to a JMeter `.jmx` file, with a report of what could not be kept |
| Safety | Host allowlist, VU / rate / duration limits, confirmation gates, audit log, secret redaction |
| AI (optional) | Providers: anthropic, openai, gemini, openrouter, ollama (local), mock. `phoenix ask`, `phoenix explain`, and an MCP server for AI assistants |

## Documentation

| Document | Read it to |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | Build, install, run in Docker, run `doctor` |
| [docs/USER_GUIDE.md](docs/USER_GUIDE.md) | Learn the UI and CLI, step by step. Written for non-programmers |
| [docs/SCENARIO_FORMAT.md](docs/SCENARIO_FORMAT.md) | Write or edit `phoenix/v1` scenario JSON by hand |
| [docs/SECURITY.md](docs/SECURITY.md) | Understand the safety model and policy file |
| [docs/AI_AND_MCP.md](docs/AI_AND_MCP.md) | Set up AI providers and connect Claude, Gemini CLI, Cursor, VS Code |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | See how the code is organised and how the engine works |
| [docs/API.md](docs/API.md) | Use the local HTTP API behind the UI |
| [docs/SBOM.md](docs/SBOM.md) | Check dependencies |
| [internal/jmx/SUPPORT_MATRIX.md](internal/jmx/SUPPORT_MATRIX.md) | See exactly which JMeter elements are supported |
| [internal/loadrunner/README.md](internal/loadrunner/README.md) | See exactly which LoadRunner constructs are supported |
| [KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md) | Know what does not work yet |

## Command overview

Run `phoenix --help` for the full list, and `phoenix <command> -h` for flags.

```
ai  ask  compare  correlate  demo  doctor  explain  export-jmx  import-har
import-jmx  import-lr  init  mcp  mock  parameterize  record  report  run
runs  serve  validate  version
```

## Responsible use

Only test systems you own or are authorized to test. Load testing can slow down or take down a service. Avoid production unless you have explicit approval. See [docs/SECURITY.md](docs/SECURITY.md).
