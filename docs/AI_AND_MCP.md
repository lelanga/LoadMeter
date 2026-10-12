# AI providers and the MCP server

Phoenix works fully without AI. AI is optional and **off by default**. You can use it in two ways:

1. Phoenix calls an AI provider for you (`phoenix ask`, `phoenix explain`, the UI's AI Assistant).
2. An AI assistant (Claude, Gemini CLI, Cursor, VS Code, ...) calls Phoenix through the MCP server (`phoenix mcp`).

Either way, the AI goes through the same tools, validation and safety gates as the UI. It cannot test a host that is not allowlisted. See [SECURITY.md](SECURITY.md).

## Providers

| Kind | Default provider id | Default model | Key variable | Where it runs |
|---|---|---|---|---|
| `anthropic` | `anthropic` | `claude-haiku-4-5` | `ANTHROPIC_API_KEY` | Cloud |
| `anthropic` | `anthropic-strong` | `claude-opus-5-5` | `ANTHROPIC_API_KEY` | Cloud |
| `openai` | `openai` | `gpt-4o-mini` | `OPENAI_API_KEY` | Cloud |
| `gemini` | `gemini` | `gemini-2.5-flash` | `GEMINI_API_KEY` | Cloud |
| `openrouter` | `openrouter` | `openrouter/auto` | `OPENROUTER_API_KEY` | Cloud |
| `ollama` | `ollama` | `llama3.1` at `http://127.0.0.1:11434` | none | Local |
| `mock` | not listed by default | n/a | none | Local, built in, for tests. Add it to `ai.json` yourself |

A provider counts as **local** when it is `mock`, or when its base URL points to a loopback address. Everything else is cloud.

The settings live in `ai.json` in the config directory (`~/.phoenix` or `$PHOENIX_HOME`). You can edit it by hand or use the UI (Settings) or `phoenix ai`. Fields:

| Field | Meaning |
|---|---|
| `enabled` | Master switch |
| `cloudConsent` | You agree that redacted data may be sent to cloud providers |
| `localOnly` | Refuse all cloud providers |
| `providers[]` | `id`, `kind`, `baseUrl`, `model`, `keyEnv`, `priceInPerMTok`, `priceOutPerMTok` |
| `routing.light`, `routing.strong` | Provider id for light tasks and for strong tasks |
| `limits` | `maxTokensPerCall` (4096), `maxCallsPerHour` (120), `monthlyBudgetUSD` (20) |

### Routing

Phoenix sorts AI tasks into two tiers. The **strong** tier is used for `convert`, `correlate`, `root-cause`, `plan`, `report` and `scenario-generation`. Everything else is **light**. `routing.strong` and `routing.light` name which provider handles each tier. If the first provider fails, Phoenix tries the other tier's provider. Temporary errors (HTTP 429 and 5xx) are retried up to 3 times with backoff.

The default routing is `ollama` for both tiers, so nothing leaves your machine until you change it.

## `phoenix ai` commands

```text
phoenix ai status
phoenix ai enable <light-provider-id> <strong-provider-id> [--cloud-consent]
phoenix ai disable
phoenix ai set-key <provider-id>
phoenix ai test <provider-id>
```

| Command | What it does |
|---|---|
| `phoenix ai` or `phoenix ai status` | Shows `enabled`, `cloudConsent`, `localOnly`, the routing, the key storage backend, and one line per provider (kind, model, local, key present) |
| `phoenix ai enable L S` | Turns AI on and routes light tasks to provider `L` and strong tasks to provider `S`. Add `--cloud-consent` to allow cloud providers. Both ids must exist |
| `phoenix ai disable` | Turns AI off |
| `phoenix ai set-key ID` | Prompts for the key on stdin and stores it (Keychain, Secret Service, or a 0600 file). An environment variable still takes precedence |
| `phoenix ai test ID` | Sends one tiny prompt to that provider and prints the reply and latency. It respects consent and local-only |

`phoenix ai -h` is not a help screen. It reports `unknown ai subcommand "-h"`. Run `phoenix --help` for the summary line instead.

### Examples

Local only, with Ollama:

```bash
ollama serve &                     # your own Ollama install
phoenix ai enable ollama ollama
phoenix ai test ollama
```

Cloud, with consent and a key from the environment:

```bash
export ANTHROPIC_API_KEY=...      # your own key
phoenix ai enable anthropic anthropic-strong --cloud-consent
phoenix ai test anthropic
```

## What is sent to the AI, and consent

Before any text leaves Phoenix, secrets in it are masked and e-mail addresses become `[EMAIL]` (see [SECURITY.md](SECURITY.md#6-redaction)). This also applies to local models.

| Feature | Sent to the provider |
|---|---|
| `phoenix ask` / UI assistant | Your request text, the tool list, scenario file names and titles, the 5 newest run ids, and the name of the open scenario. Not request bodies or response bodies |
| `phoenix explain <run-id>` | The run's aggregated summary (percentiles, per-transaction numbers, SLA results, findings), redacted. The command prints how many bytes were sent |

Rules:

- Nothing is sent unless `enabled` is true.
- A cloud provider needs `cloudConsent`. With `localOnly` on, a cloud provider is refused even with consent.
- Calls stop at the hourly call limit or the monthly budget.
- Every call is recorded in `ai-activity.jsonl` and the audit log.
- Imported files, responses and logs are sent inside `<untrusted_data>` tags, and the model is told to treat them as data.

## Natural language: `phoenix ask`

```bash
phoenix ask "create a 100-user 10-minute test with ramp-up" --file shop-login.json
phoenix ask "find dynamic tokens and suggest correlation rules" --file shop-login.json
phoenix ask "validate shop-login.json" --file shop-login.json
```

`ask` builds a plan, a list of tool calls, and runs it. Output shows the planner (`ai` or `rules`), each step, and each result.

- With AI **disabled**, a built-in rule planner handles common requests (import a path ending in `.har` or `.jmx`, correlate, parameterize, set workload, validate, debug, run, compare, export). It works offline.
- With AI **enabled**, the AI proposes the plan. If the AI fails or its answer cannot be parsed, Phoenix falls back to the rules.
- Steps that generate load or delete data (`run_test`, `delete_scenario`) run only when you pass `--confirm`. Without it they are listed as `[needs --confirm]`.
- Other steps run immediately. That includes `set_workload`, which saves a change to the scenario file. A copy of the old version is kept in the workspace history.

Flags: `--file` (the scenario the request refers to), `--confirm`, `--workspace`.

Explain a run:

```bash
phoenix explain <run-id>
```

## MCP server: `phoenix mcp`

`phoenix mcp` speaks the Model Context Protocol over **stdio**. The AI client starts it as a child process. It prints one line on stderr, then waits for JSON-RPC on stdin.

```bash
phoenix mcp --workspace /absolute/path/to/workspace
```

Use an **absolute** path for the binary and the workspace. Phoenix resolves relative paths against the working directory of the client, which is often not what you expect.

Protocol versions answered: 2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05. Phoenix offers tools only (no resources or prompts).

The server does not use the AI providers above. The AI client brings its own model. Hosts that Phoenix may test follow your `policy.json` (loopback always allowed). The MCP server has no `--allow-host` flag. Add hosts to `policy.json` or the UI before you ask an assistant to test them.

### Client configuration

Replace `/path/to/phoenix` and `/path/to/workspace` with real absolute paths.

**Claude Desktop** (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "phoenix": {
      "command": "/path/to/phoenix",
      "args": ["mcp", "--workspace", "/path/to/workspace"]
    }
  }
}
```

**Claude Code**:

```bash
claude mcp add phoenix -- /path/to/phoenix mcp --workspace /path/to/workspace
```

**Gemini CLI** (`~/.gemini/settings.json`):

```json
{
  "mcpServers": {
    "phoenix": {
      "command": "/path/to/phoenix",
      "args": ["mcp", "--workspace", "/path/to/workspace"]
    }
  }
}
```

**Cursor** (`.cursor/mcp.json` in your project, or the global Cursor MCP file):

```json
{
  "mcpServers": {
    "phoenix": {
      "command": "/path/to/phoenix",
      "args": ["mcp", "--workspace", "/path/to/workspace"]
    }
  }
}
```

**VS Code** (`.vscode/mcp.json`; note the key is `servers` and it needs `type`):

```json
{
  "servers": {
    "phoenix": {
      "type": "stdio",
      "command": "/path/to/phoenix",
      "args": ["mcp", "--workspace", "/path/to/workspace"]
    }
  }
}
```

Restart the client after you edit its config. Then ask: "List my Phoenix scenarios."

### Tools

Tools marked **confirm** refuse to run unless the call has `confirm=true`. The server instructions tell the AI to set it only after the human approves. Tools that are not read-only are written to the audit log as `tool.call`.

| Tool | Read-only | Needs confirm | What it does |
|---|---|---|---|
| `list_scenarios` | yes | no | List scenarios with request counts and workload settings |
| `get_scenario` | yes | no | Return the full scenario JSON |
| `validate_scenario` | yes | no | Validate and list target hosts and which are blocked |
| `import_har` | no | no | Import a HAR. Returns correlation and parameter suggestions. Args: `path`, `name`, `includeStatic` |
| `import_jmeter` | no | no | Import a `.jmx` with a compatibility report |
| `import_loadrunner` | no | no | Statically analyze and convert a VuGen web script folder |
| `export_jmeter` | no | no | Write a `.jmx` into the workspace `exports` folder |
| `suggest_correlations` | yes | no | Find dynamic values (values are masked) |
| `apply_correlations` | no | no | Apply chosen rules. Policy `validated` (default), `manual`, `auto` or `suggest`. Replays against the target to validate |
| `suggest_parameters` | yes | no | Find inputs that should come from test data |
| `apply_parameters` | no | no | Generate a synthetic CSV and rewrite requests |
| `set_workload` | no | no | Set `vus`, `rampUp`, `duration`, `rate`, `maxVUs` |
| `debug_scenario` | yes | no | Run one iteration with one VU and return redacted exchanges |
| `run_test` | no | **yes** | Start a load test. Optional `wait` returns the summary (up to 15 minutes) |
| `run_status` | yes | no | Live metrics of a running test |
| `stop_test` | no | no | Gracefully stop a test (call twice to force) |
| `list_runs` | yes | no | List finished runs |
| `get_results` | yes | no | Full summary of a run |
| `compare_runs` | yes | no | Compare a run with a baseline |
| `get_report_path` | yes | no | Paths of the HTML report and the CSV and JSON exports |
| `delete_scenario` | no | **yes** | Delete a scenario (a copy stays in history) |
| `create_demo` | no | no | Start the local mock shop and create `demo-shop.json` |

Only `run_test` and `delete_scenario` need confirmation. Tools with `readOnlyHint` true change nothing. `import_*` tools read files only from the workspace `imports` folder or the current directory tree.

`run_test` still obeys the safety policy: a host that is not allowlisted is refused, and the limits for VUs, rate and duration apply.

### Test the server by hand

```bash
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
              '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | phoenix mcp --workspace /tmp/phoenix-ws
```

## UI AI Assistant

The **AI Assistant** page in the UI is the same planner as `phoenix ask`. It shows the plan, asks for confirmation before steps that generate load, and shows recent AI activity. AI settings are in **Settings**.
