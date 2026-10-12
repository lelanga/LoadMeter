# Scenario format: `phoenix/v1`

A scenario is one JSON file. The UI, the CLI, the importers and the MCP tools all read and write this format. The Go definition is in `internal/model/model.go`. Validation lives in `Scenario.Validate()`; run it with `phoenix validate scenario.json`.

Scenarios live in `<workspace>/scenarios/`. A file name is letters, digits, `_`, `-` and `.`, and ends in `.json`.

## Top level

| Field | Type | Required | Meaning |
|---|---|---|---|
| `version` | string | no | Must be `"phoenix/v1"` if present |
| `name` | string | yes (by convention) | Display name |
| `description` | string | no | Free text |
| `defaults` | object | no | Settings that apply to every request. See [Defaults](#defaults) |
| `variables` | object (string to string) | no | Named values, read as `${name}` |
| `dataSources` | array | no | CSV or inline test data. See [Data sources](#data-sources) |
| `setup` | array of steps | no | Runs once per VU, before its first iteration |
| `steps` | array of steps | **yes** | The user journey. At least one step |
| `teardown` | array of steps | no | Runs when a VU finishes (10 second limit) |
| `workload` | object | **yes** | How much load. See [Workload](#workload-executors) |
| `slas` | array | no | Pass/fail goals. See [SLAs](#slas) |
| `metadata` | object (string to string) | no | Where the scenario came from (importer, source file, warnings). Never executed |

Durations are Go duration strings such as `"500ms"`, `"30s"`, `"5m"`, `"1h30m"`. A bare number is read as milliseconds.

## Defaults

| Field | Type | Meaning |
|---|---|---|
| `baseUrl` | string | Prefix for relative request URLs. May use `${var}` |
| `headers` | array of `{name, value}` | Sent on every request. A request header with the same name wins |
| `timeout` | duration | Per-request timeout |
| `followRedirects` | boolean | Follow HTTP redirects (a request can override) |
| `insecureSkipVerify` | boolean | Skip TLS certificate checks. Use only for test systems |
| `disableCookies` | boolean | Turn off the per-VU cookie jar. By default each VU has its own jar |
| `maxBodyBytes` | integer | How many response bytes are kept for extractors and assertions |

## Variables and functions

Write `${name}` anywhere in a URL, header value, body, form value, extractor expression, assertion value, `set` value or condition. Unknown variables stay as written and are reported as unresolved.

Variables come from, in order of use: `variables`, data source columns, extractors, `set` steps.

Functions look like `${__name(arg1,arg2)}`. Arguments may contain `${...}`.

| Function | Result |
|---|---|
| `${__uuid()}` | A random version 4 UUID |
| `${__randomInt(min,max)}` | Random integer from min to max, inclusive |
| `${__randomString(n)}` | Random letters and digits. Default length 8, maximum 4096 |
| `${__timestamp()}` | Unix time in milliseconds |
| `${__timestamp(yyyy-MM-dd'T'HH:mm:ss)}` | Formatted time. Supports the tokens `yyyy MM dd HH mm ss SSS` |
| `${__counter()}` | A counter that rises by 1 on every call, shared by all VUs |
| `${__vu()}` | The number of the current VU |
| `${__iteration()}` | The iteration number of the current VU |
| `${__env(PHOENIX_NAME)}` / `${__env(PHOENIX_NAME,default)}` | Value of an environment variable. Only names that start with `PHOENIX_` can be read. This stops an imported scenario from reading other secrets |
| `${__urlencode(value)}` | Percent-encoded value |

Use `${__env(PHOENIX_API_TOKEN)}` to keep secrets out of the file. See [SECURITY.md](SECURITY.md).

## Steps

Every step has a `type`. Common fields: `name`, `disabled` (skip this step), `comment`.

| `type` | Uses | Meaning |
|---|---|---|
| `request` | `request` | Send one HTTP request. `name` labels it in reports |
| `transaction` | `steps`, `name` (required) | A named group. Phoenix reports its time (think time is subtracted) and counts it failed if any inner request failed |
| `group` | `steps` | A group with no timing of its own |
| `loop` | `steps`, `count` (at least 1) | Repeat the inner steps `count` times |
| `if` | `steps`, `condition` | Run the inner steps only when the condition is true |
| `think` | `think` | Pause. See below |
| `set` | `set` (array of `{name, value}`) | Set variables. Values are expanded |

`condition` forms: `${a} == b`, `${a} != b`, or a single `${a}`. A single value is true when it is not empty, not `false` and not `0`. Quotes around operands are ignored. An unresolved variable in a single-operand condition counts as false.

`think`: `{"min": "200ms", "max": "500ms"}`. With `max` larger than `min`, the pause is random between them. `workload.thinkTimeScale` multiplies all think times (`0` removes them, `1` keeps them).

### Request

| Field | Type | Meaning |
|---|---|---|
| `method` | string | Required. `GET`, `POST`, ... |
| `url` | string | Required. Absolute (`http://` or `https://`) or relative to `defaults.baseUrl` |
| `headers` | array of `{name, value}` | A `Host` header sets the request host |
| `body` | string | Raw body |
| `form` | array of `{name, value}` | URL-encoded form (`application/x-www-form-urlencoded`) |
| `multipart` | array of parts | Multipart form. See below |
| `timeout` | duration | Overrides `defaults.timeout` |
| `followRedirects` | boolean | Overrides the default |
| `extract` | array of extractors | Save values from the response |
| `assert` | array of assertions | Check the response |

Only one body form is used. The order is `multipart`, then `form`, then `body`. Phoenix sets `Content-Type` for `form` and `multipart` if you did not. It sends `User-Agent: Phoenix-Performance/0.1` if you did not set one.

Multipart part: `{name, value}` for a text field, or `{name, file, contentType}` for a file. `file` is a path relative to the scenario file. Absolute paths and paths that leave the scenario folder are refused.

## Extractors

An extractor saves part of a response into a variable for later steps. This is how correlation works.

| Field | Meaning |
|---|---|
| `name` | Variable name (required) |
| `type` | One of the types below (required) |
| `expr` | The expression. Meaning depends on `type` |
| `left`, `right` | For `boundary`: the text before and after the value |
| `group` | For `regex`: capture group number |
| `match` | Which match to keep: `0` or `1` = first, `n` = nth (1-based), `-1` = random |
| `default` | Value to use when nothing is found |
| `required` | When true, the iteration is marked failed if nothing is extracted |

| `type` | `expr` is | Notes |
|---|---|---|
| `jsonpath` | A JSONPath such as `$.user.id` | Supported subset: `$`, `.key`, `['key']`, `[n]`, `[-n]`, `[*]`, `..key` |
| `regex` | A regular expression (Go RE2 syntax) | `group` picks the capture group |
| `xpath` | An XPath | Supports `/a/b`, `//b`, `a/b[2]`, a trailing `/text()` and `/@attr`. XML with a DOCTYPE is rejected |
| `header` | A response header name | |
| `cookie` | A cookie name | Looks at `Set-Cookie`, then at the cookie jar |
| `boundary` | not used (use `left` and `right`) | Text between two markers |
| `status` | not used | The HTTP status code as text |

## Assertions

An assertion fails the request when it is not true.

| `type` | Uses | Passes when |
|---|---|---|
| `status` | `value` | The status matches. `"200"`, `"2xx"`, or a list `"200,201,3xx"`. Empty value means status below 400 |
| `contains` | `value` | The body contains the text |
| `not_contains` | `value` | The body does not contain the text |
| `regex` | `expr` | The body matches the expression |
| `jsonpath` | `expr`, optional `value` | The path exists. With `value`, one of the matches equals it |
| `max_latency` | `value` (duration) | The response time is at most the value |
| `header` | `expr` (header name), optional `value` | The header is present. With `value`, the header contains it |

## Data sources

```json
{"name": "users", "file": "users.csv", "mode": "sequential", "onExhaust": "recycle"}
```

| Field | Meaning |
|---|---|
| `name` | Required |
| `file` | CSV path, relative to the scenario file. Absolute paths and `..` escapes are refused |
| `rows` | Inline rows (array of arrays of strings). Use instead of `file` for small data. One of `file` or `rows` is required |
| `columns` | Column names. If empty, the first CSV row is the header |
| `delimiter` | Field separator, default comma |
| `mode` | `sequential` (shared cursor, default), `random`, `unique` (each row used once), `per-vu` (row number = VU number) |
| `onExhaust` | When rows run out: `recycle` (default), `stop` (the VU stops), `error` (the iteration fails) |
| `scope` | `iteration` (new row each iteration, default) or `vu` (one row for the whole VU) |
| `sensitive` | Mask the values in logs and reports |

Each column becomes a variable with the column name.

## Workload (executors)

`workload.executor` is required. Pick one.

| Executor | Model | Needs | Behaviour |
|---|---|---|---|
| `constant-vus` | closed | `vus` >= 1, `duration` > 0 | A fixed number of VUs for the duration |
| `ramping-vus` | closed | `stages` (at least one); optional `vus` as the start | VUs move toward each stage `target` linearly over the stage `duration` |
| `per-vu-iterations` | closed | `vus` >= 1, `iterations` >= 1 | Each VU runs `iterations` iterations. `duration` is an optional time limit |
| `constant-arrival-rate` | open | `rate` > 0, `duration` > 0 | Starts `rate` iterations per second. Uses a VU pool up to `maxVUs` |
| `ramping-arrival-rate` | open | `stages` (at least one); optional `rate` as the start | The start rate moves toward each stage `target` (iterations per second) |

Other workload fields:

| Field | Meaning |
|---|---|
| `vus` | VU count (closed) or the number of VUs prepared at the start (open) |
| `maxVUs` | Upper limit of the VU pool for the open model |
| `stages` | Array of `{duration, target}`. Target is VUs (ramping-vus) or iterations per second (ramping-arrival-rate) |
| `pacing` | Minimum time between the starts of two iterations of one VU |
| `gracefulStop` | How long in-flight iterations may finish after the test ends (default 5 s) |
| `thinkTimeScale` | Multiplier for think times. `0` ignores them, `1` keeps them |
| `onError` | `continue` (default), `abort-iteration`, `stop-vu` |

If the open model cannot start an iteration because all `maxVUs` are busy, the iteration is counted as **dropped**. That means the system or the VU pool could not keep up with the rate.

The safety policy can refuse a workload that needs more VUs than `maxVUs` or runs longer than `maxDuration` (see [SECURITY.md](SECURITY.md)).

## SLAs

```json
{"metric": "p95", "scope": "03_order", "op": "<", "threshold": 800}
```

| Field | Meaning |
|---|---|
| `metric` | `p50`, `p90`, `p95`, `p99`, `avg`, `max` (milliseconds), `error_rate` (percent), `rps` |
| `scope` | Empty for the whole run, or the name of a transaction or request |
| `op` | `<`, `<=`, `>` or `>=` |
| `threshold` | A number |

`phoenix run` exits with code 3 if any SLA fails.

## Complete example

This is a small, valid scenario for the bundled mock server (`phoenix mock`). It uses a variable, a data source, an extractor, assertions, a loop, a condition, a function and an SLA. For a larger sample see `examples/shop-login.json`.

```json
{
  "version": "phoenix/v1",
  "name": "Mock shop quick check",
  "description": "Login, browse three times, place an order.",
  "defaults": {
    "baseUrl": "${baseUrl}",
    "timeout": "10s",
    "headers": [{"name": "Accept", "value": "application/json, text/html"}]
  },
  "variables": {"baseUrl": "http://127.0.0.1:8099"},
  "dataSources": [
    {"name": "users", "file": "users.csv", "mode": "sequential", "onExhaust": "recycle"}
  ],
  "steps": [
    {"type": "transaction", "name": "01_login", "steps": [
      {"type": "request", "name": "GET /login-page", "request": {
        "method": "GET", "url": "/login-page",
        "extract": [{"name": "rvt", "type": "regex",
                     "expr": "name=\"__RequestVerificationToken\" value=\"([^\"]+)\"",
                     "required": true}],
        "assert": [{"type": "status", "value": "200"}]
      }},
      {"type": "request", "name": "POST /login", "request": {
        "method": "POST", "url": "/login",
        "form": [
          {"name": "__RequestVerificationToken", "value": "${rvt}"},
          {"name": "username", "value": "${username}"},
          {"name": "password", "value": "${password}"}
        ],
        "extract": [
          {"name": "csrf", "type": "header", "expr": "X-CSRF-Token", "required": true},
          {"name": "token", "type": "jsonpath", "expr": "$.token", "required": true}
        ],
        "assert": [
          {"type": "status", "value": "200"},
          {"type": "jsonpath", "expr": "$.user.name", "value": "${username}"}
        ]
      }}
    ]},
    {"type": "think", "think": {"min": "200ms", "max": "500ms"}},
    {"type": "set", "name": "remember item", "set": [{"name": "wanted", "value": "${item}"}]},
    {"type": "loop", "name": "browse 3 times", "count": 3, "steps": [
      {"type": "request", "name": "GET /api/items", "request": {
        "method": "GET", "url": "/api/items?q=${__urlencode(${item})}",
        "assert": [{"type": "status", "value": "2xx"}, {"type": "max_latency", "value": "2s"}]
      }}
    ]},
    {"type": "if", "condition": "${wanted} != ", "steps": [
      {"type": "transaction", "name": "03_order", "steps": [
        {"type": "request", "name": "POST /api/orders", "request": {
          "method": "POST", "url": "/api/orders",
          "headers": [
            {"name": "Content-Type", "value": "application/json"},
            {"name": "X-CSRF-Token", "value": "${csrf}"},
            {"name": "X-Request-Id", "value": "${__uuid()}"}
          ],
          "body": "{\"item\": \"${item}\", \"qty\": 1}",
          "extract": [{"name": "orderId", "type": "jsonpath", "expr": "$.orderId", "required": true}],
          "assert": [{"type": "status", "value": "201"}]
        }}
      ]}
    ]}
  ],
  "workload": {
    "executor": "ramping-vus",
    "vus": 0,
    "stages": [
      {"duration": "5s", "target": 10},
      {"duration": "10s", "target": 10},
      {"duration": "3s", "target": 0}
    ],
    "thinkTimeScale": 1,
    "gracefulStop": "5s",
    "onError": "continue"
  },
  "slas": [
    {"metric": "p95", "op": "<", "threshold": 500},
    {"metric": "error_rate", "op": "<", "threshold": 1},
    {"metric": "p95", "scope": "03_order", "op": "<", "threshold": 800}
  ]
}
```

To try it, save it as `shop.json` next to a copy of `examples/users.csv` (columns `username,password,item`), then:

```bash
phoenix mock &
phoenix validate shop.json
phoenix run shop.json --vus 5 --duration 10s
```
