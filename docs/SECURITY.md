# Security and safety model

Phoenix generates traffic. That can hurt a system that is not ready for it. The design goal is simple: the default settings can only hurt your own machine.

## Responsible use

**Only test systems you own or are explicitly authorized to test.** Do not point Phoenix at third-party sites, at shared services you do not run, or at production systems without written approval. You are responsible for the traffic you generate. Phoenix's safety features reduce risk. They do not give you permission.

## 1. Loopback only by default

- Targets on `127.0.0.1`, `::1`, `localhost` and `*.localhost` are always allowed.
- Every other host must be allowed by **you**, the operator. A scenario file cannot allow a host for itself, because imported scenarios are untrusted input.
- The check runs on every connection (including redirects) before DNS resolution. A redirect to a host that is not allowed is blocked, and the run records the error kind `blocked_by_policy`.
- Before a run, `phoenix validate` and the UI list the target hosts and whether each one is allowed.

### Allow a host

| Way | Scope |
|---|---|
| `--allow-host HOST` on `run`, `demo`, `correlate` or `serve` (repeatable, or comma-separated) | That command or UI session only. Logged to the audit log as `policy.allow_host_cli` |
| `allowedHosts` in `policy.json` | Permanent |
| UI: Settings, safety policy | Permanent. Writes `policy.json` |

An entry is an exact host (`staging.example.com`) or a wildcard suffix (`*.example.com`). Ports are not part of the entry.

## 2. The policy file

`policy.json` lives in `~/.phoenix/`, or in `$PHOENIX_HOME` if that variable is set. If the file does not exist, the defaults below apply.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `allowedHosts` | array of strings | `[]` | Non-loopback hosts you allow. Exact name or `*.suffix` |
| `maxVUs` | integer | `1000` | A run needing more VUs is refused before it starts |
| `maxRPS` | number | `0` (no limit) | Global request-rate cap shared by all VUs (token bucket) |
| `maxDuration` | string | `"4h"` | A planned duration above this is refused. Ramp stages are added up |
| `requireConfirmAbove` | integer | `50` | For a non-loopback target, tests above this many VUs (or this many iterations per second) need explicit confirmation |

Example:

```json
{
  "allowedHosts": ["staging.example.com", "*.test.example.com"],
  "maxVUs": 200,
  "maxRPS": 500,
  "maxDuration": "30m",
  "requireConfirmAbove": 25
}
```

The UI refuses to save a `maxVUs` below 1 or above 100000, or a negative `maxRPS`. `phoenix doctor` prints the path of the policy file, `maxVUs` and the number of allowlisted hosts.

## 3. Confirmation gates

| Where | Gate |
|---|---|
| CLI `run` / `demo` | A non-local target with more VUs than `requireConfirmAbove` stops with an error unless you pass `--yes`. A warning is printed for any non-local target |
| UI | The server answers 409 `needsConfirmation`. The UI shows a warning dialog. You confirm to resend |
| MCP tools and `phoenix ask` | `run_test` and `delete_scenario` need `confirm=true` (see [AI_AND_MCP.md](AI_AND_MCP.md)). `ask` needs `--confirm` |
| Correlation `validated` policy | Replays requests against the target. The same host allowlist applies |

## 4. Audit log

Phoenix appends one JSON line per event to `audit.log` in the config directory (`~/.phoenix` or `$PHOENIX_HOME`). The file is owner-only (mode 0600). Secrets are never written to it.

Events include: `run.start`, `run.end`, `run.stop_requested`, `run.stop`, `policy.changed`, `policy.allow_host_cli`, `import.har`, `import.jmx`, `import.loadrunner`, `import.keep_credentials`, `correlation.applied`, `correlation.rejected`, `scenario.deleted`, `tool.call` (every non-read-only MCP or assistant tool call, with the caller), `ai.call`, `ai.config_changed` and `ai.key_changed`.

The UI shows the last 200 events in Settings.

## 5. Secrets

- Put secrets in **environment variables**, not in scenario files. A scenario can read variables whose name starts with `PHOENIX_` through `${__env(PHOENIX_NAME)}`. Other variables cannot be read, so an imported scenario cannot steal your environment.
- AI API keys come from the environment variable named in the provider config (for example `ANTHROPIC_API_KEY`). The environment always wins and is never written to disk.
- If you store a key with `phoenix ai set-key <provider-id>`, it goes to the macOS Keychain, the Linux Secret Service (`secret-tool`, when a session bus is available), or an owner-only (0600) file `keys.json` in the config directory. `phoenix ai status` prints the backend (`keys=...`). The file backend is plain text on disk, so prefer environment variables on shared machines.
- Keys are never returned by the API and never logged.

## 6. Redaction

Phoenix masks secrets before they appear in the UI, in logs, in the debug inspector, or in AI requests.

- Header, field and key names that look sensitive (password, secret, token, api key, auth, session, cookie, credential, card number, cvv, otp, signature, bearer, jwt, refresh, and similar) are shown as `[REDACTED ...]` with a short masked hint.
- Free text is scanned for bearer/basic tokens, JWTs, `key=value` pairs such as `password=...`, and credit card numbers that pass the Luhn check.
- Everything sent to an AI provider also has e-mail addresses replaced with `[EMAIL]`. This happens even for local models.
- HAR import replaces recorded credentials with variables. Use `--keep-credentials` only with explicit authorization. That choice is audit-logged.
- Data sources marked `"sensitive": true` are masked in logs and reports.
- The raw HAR kept for correlation still holds the original values. It is stored with owner-only permissions.

Redaction is pattern-based. It is a safety net, not a guarantee. Do not put secrets in free text and expect them to be found.

## 7. AI consent, local-only mode and budgets

AI is **off by default**. Phoenix works fully without it.

- `enabled`: AI does nothing until you turn it on.
- `cloudConsent`: data may only go to a cloud provider after you grant consent (`phoenix ai enable ... --cloud-consent`, or the UI setting). Without it, only local providers (Ollama on loopback, or `mock`) can be used.
- `localOnly`: when on, cloud providers are refused even if consent was given.
- Limits: `maxTokensPerCall` (default 4096), `maxCallsPerHour` (default 120) and `monthlyBudgetUSD` (default 20). Calls stop when a limit is reached.
- Every AI call is logged in `ai-activity.jsonl` (time, task, provider, model, tokens, cost, number of redactions) and in the audit log. You can see recent calls in the UI.
- Text from imported files, HTTP responses and logs is wrapped in `<untrusted_data>` tags with an instruction to treat it as data (prompt-injection guard).
- AI cannot bypass any gate above. Plans run through the same tools as the UI. See [AI_AND_MCP.md](AI_AND_MCP.md).

## 8. Web UI protections

`phoenix serve` is built to be safe on a shared computer.

| Protection | How |
|---|---|
| Loopback only | `serve` refuses a non-loopback `--addr`. Use SSH port forwarding for remote access |
| Token | A random token is printed in the start URL. The first visit sets an `HttpOnly`, `SameSite=Strict` cookie. Every `/api/*` call needs the cookie or the `X-Phoenix-Token` header. Treat the URL like a password |
| CSRF | State-changing calls must also carry the `X-Phoenix-Token` header. A cookie alone is not enough |
| Host header check | Requests whose `Host` is not `127.0.0.1`, `localhost` or `[::1]` are rejected. This blocks DNS rebinding |
| Response headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and a strict Content-Security-Policy. Reports are served with a stricter policy |
| No remote content | The UI is embedded and makes no external requests |

If you set `PHOENIX_TOKEN_FILE`, `serve` writes the start URL to that file with mode 0600. Do not do this on a shared machine unless the path is private.

## 9. Safe parsing and safe paths

- **XML**: Any `<!DOCTYPE>` or entity declaration is rejected, in JMeter `.jmx` import and in XPath extraction. This blocks XXE and "billion laughs" attacks. `.jmx` input is limited to 50 MB, 256 levels of nesting and 2,000,000 elements. Nothing is fetched from the network while parsing.
- **Scripts are never run**: JSR223, BeanShell and BSF code in JMeter plans, and the C code in LoadRunner scripts, are only read and reported. LoadRunner import is static analysis.
- **Scenario file names** must match letters, digits, `_`, `-`, `.` and end in `.json`. Names containing `..` are refused.
- **Run IDs** may only contain letters, digits, `-` and `_`.
- **Paths inside scenarios** (data source files, multipart uploads) must be relative and may not leave the scenario's folder.
- **MCP and assistant imports** may only read files inside the workspace `imports` folder or the current directory tree. They cannot read `~/.ssh` or other places.
- **Recording proxy**: `phoenix record` only listens on loopback.
- **Config files** are written with owner-only permissions (directory 0700, files 0600).

## 10. What Phoenix does not protect you from

- It cannot know whether you are authorized. That is your job.
- An allowed host is trusted. If you allow `*.example.com`, any host under it can be targeted.
- Pattern-based redaction can miss unusual secrets.
- `insecureSkipVerify` in a scenario turns off TLS certificate checks for that scenario.

See [KNOWN_LIMITATIONS.md](../KNOWN_LIMITATIONS.md) for other gaps.

## Reporting a vulnerability

Report security problems privately to the maintainers of the repository you got Phoenix from. Do not post details in a public issue.
