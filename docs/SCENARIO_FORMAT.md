# Scenario format: `loadmeter/v1`

A scenario is one JSON file. The UI, the CLI, the importers and the MCP tools all read and write this format. The Go definition is in `internal/model/model.go`. Validation lives in `Scenario.Validate()`; run it with `loadmeter validate scenario.json`.

Scenarios live in `<workspace>/scenarios/`. A file name is letters, digits, `_`, `-` and `.`, and ends in `.json`.

## Top level

| Field | Type | Required | Meaning |
|---|---|---|---|
| `version` | string | no | Must be `"loadmeter/v1"` if present |
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
| `maxBodyBytes` | integer | How many response bytes are kept for extractors and assertions (default 1 MiB). Also the limit for decompressed bodies |
| `connectTimeout`, `tlsTimeout`, `responseHeaderTimeout` | duration | Split timeouts. See [HTTP layer](#http-layer) |
| `compression` | `auto` \| `off` | Response decompression. See [HTTP layer](#http-layer) |
| `connections`, `keepAlive`, `httpVersion`, `maxConnLifetime`, `hiddenRetries` | | Connection model. See [HTTP layer](#http-layer) |
| `auth` | array of rules | Basic, digest and bearer authentication by URL prefix. See [Authentication](#authentication) |
| `tls` | object | Client certificates and extra trusted CAs. See [TLS](#tls-client-certificates) |
| `dns` | `{hosts: {name: ip}}` | Static host overrides. See [DNS overrides and proxy](#dns-overrides-and-proxy) |
| `proxy` | object | Explicit HTTP proxy. See [DNS overrides and proxy](#dns-overrides-and-proxy) |

### HTTP layer

All fields are optional. A scenario without them behaves as before, except that connections are now per VU in the closed model (see `connections`).

**Timeouts.** `timeout` bounds the whole request (connect, send, wait, read the body; default 60s). Three phase timeouts can be set in `defaults` and overridden per request:

| Field | Default | Bounds | Error kind when exceeded |
|---|---|---|---|
| `connectTimeout` | 10s | DNS lookup and TCP connect | `connect_timeout` |
| `tlsTimeout` | 10s | The TLS handshake | `tls` |
| `responseHeaderTimeout` | none | From the request being sent to the first response byte (TTFB) | `ttfb_timeout` |

When the overall `timeout` fires, the error kind tells you where the request was: `connect_timeout` (still connecting), `tls` (in the handshake), `timeout` (waiting for the response) or `read_timeout` (headers received, body still arriving). Other network kinds: `connect` (refused, DNS failure), `tls` (handshake or certificate failure), `connection_closed` (see `hiddenRetries`), `decompression` (corrupt gzip/deflate body), `blocked_by_policy` (safety allowlist).

**Compression** (`compression`). `auto` (default): when a request has no `Accept-Encoding`, Load Meter asks for `gzip`. When the scenario sets one (JMX and HAR imports keep the recorded `gzip, deflate, br`), codings Load Meter cannot decode (`br`, `zstd`) are removed from it before sending, so the server answers with something readable. `gzip`, `x-gzip` and `deflate` (zlib or raw) responses are decoded whatever header was sent, so extractors and assertions always see plain text. Decoding stops at `maxBodyBytes`; the rest of the body is still read from the network but not decoded, which protects against compression bombs. `off`: headers are sent as written and bodies are kept raw. Byte counts: `bytes` in reports and the raw CSV are bytes on the wire (headers plus the compressed body), as in JMeter; `bodyBytes` in the raw CSV is the decoded size.

**Connections.**

| Field | Values | Default | Meaning |
|---|---|---|---|
| `connections` | `per-vu` \| `shared` | `per-vu` for `constant-vus`, `ramping-vus` and `per-vu-iterations`; `shared` for the arrival-rate executors | `per-vu`: each VU has its own connection pool, like a JMeter thread or a real browser, so 100 VUs open about 100 connections and pay their own TLS handshakes. `shared`: one pool for all VUs (HTTP/2 then multiplexes many VUs over a few connections) |
| `keepAlive` | boolean | `true` | `false` opens a new connection for every request |
| `httpVersion` | `auto` \| `1.1` \| `2` | `auto` | `auto`: HTTP/2 when the server offers it over TLS, else HTTP/1.1. `1.1`: never HTTP/2 (JMeter's behaviour). `2`: HTTP/2 only; `http://` URLs use cleartext h2c with prior knowledge |
| `maxConnLifetime` | duration | `60s` | A connection older than this is closed after its next use (HTTP/1.1 sends `Connection: close`; HTTP/2 stops opening streams on it). Same idea as JMeter's `httpclient4.time_to_live`. A negative value turns it off |
| `hiddenRetries` | `report` \| `fail` | `report` | Go resends an idempotent request (GET, HEAD, ...) on a new connection when a reused keep-alive connection turns out to be closed. `report` lets that happen and counts it (`retries` column, `hiddenRetries` in the summary). `fail` fails the request with `connection_closed` instead, like JMeter (retry count 0) |

### Authentication

`defaults.auth` is a list of rules. A request uses the rule whose `urlPrefix` is the longest prefix of its URL. A prefix must end on a path boundary: `/api` matches `/api` and `/api/x` but not `/apix`. A relative prefix (`/api`) is resolved against `baseUrl`; an empty prefix means the whole `baseUrl`. Rules are re-checked on every redirect: credentials are removed when the new URL matches no rule. An `Authorization` header you set yourself always wins.

| Field | Meaning |
|---|---|
| `urlPrefix` | Absolute URL prefix, or a path relative to `baseUrl` |
| `type` | `basic` (sent with every request), `bearer` (`Authorization: Bearer <token>`) or `digest` |
| `username` | For `basic` and `digest`. May be literal or `${var}` |
| `password` | For `basic` and `digest` |
| `token` | For `bearer` |
| `realm` | Optional. A `digest` rule only answers challenges for this realm |

`password` and `token` must be made only of `${secret_*}` variables or `${__env(LOADMETER_*)}`. Literal secrets are refused by validation. Supply values with `--set secret_pw=...`, a data source column named `secret_*` (mark the source `sensitive`), or the environment. Load Meter shows `Authorization` headers it added as `[REDACTED <type>]` in debug output.

`digest` follows RFC 7616: on a `401` with `WWW-Authenticate: Digest`, Load Meter answers once and records a single sample covering both legs (as JMeter does). Supported: `qop=auth` (and servers without `qop`), algorithms `MD5`, `SHA-256`, `SHA-512-256` and their `-sess` variants (the strongest offered is used), `opaque`, `userhash`. Each VU keeps the challenge per host and reuses it on later requests without a new `401`, counting `nc` up (`00000001`, `00000002`, ...). A stale or changed nonce is answered once more. `qop=auth-int` is not supported.

```json
"auth": [
  {"urlPrefix": "/api", "type": "digest", "username": "${user}", "password": "${secret_password}"},
  {"urlPrefix": "https://reports.example.test/v2", "type": "bearer", "token": "${__env(LOADMETER_REPORTS_TOKEN)}"}
]
```

### TLS client certificates

```json
"tls": {
  "caFile": "certs/ca.pem",
  "clientCert": "certs/client.pem", "clientKey": "certs/client.key",
  "hosts": [{"host": "*.payments.test", "clientCert": "certs/pay.pem", "clientKey": "certs/pay.key"}]
}
```

Files are PEM and relative to the scenario file (absolute paths and paths outside the scenario folder are refused). `hosts` picks a certificate by target host (exact name or `*.suffix`); other hosts use `clientCert`/`clientKey`. `caFile` adds trusted roots to the system pool. Encrypted keys and PKCS#12 are not supported: convert with `openssl pkcs12 -in c.p12 -nodes -out c.pem` or `openssl pkcs8 -in enc.key -nocrypt -out key.pem`, and protect the files with file permissions.

### DNS overrides and proxy

`"dns": {"hosts": {"api.example.test": "10.20.0.15"}}` connects to the given IP instead of resolving the name (like `/etc/hosts`), for example to test one node behind a load balancer. The URL, `Host` header and TLS server name keep the original name. The value must be an IP address.

`"proxy": {"url": "http://proxy.corp.test:3128", "username": "me", "password": "${secret_proxy}", "noProxy": ["*.internal.test"]}` sends requests through an explicit HTTP proxy (`https://` targets use `CONNECT`). Load Meter never uses a proxy from the environment. Loopback targets always connect directly.

Both features go through the safety allowlist twice. A DNS override needs the host name **and** the IP to be allowed (loopback is always allowed), so an allowed name cannot be pointed at a machine that is not allowed. With a proxy, every target host is checked before the request is handed to the proxy, and the proxy host itself must be allowed. Violations are refused before the run starts where possible, and at connect time otherwise (`blocked_by_policy`).

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
| `${__counter()}` | A counter that rises by 1 on every call, shared by all VUs of a run. It starts at 1 for every run |
| `${__vu()}` | The number of the current VU |
| `${__iteration()}` | The iteration number of the current VU |
| `${__env(LOADMETER_NAME)}` / `${__env(LOADMETER_NAME,default)}` | Value of an environment variable. Only names that start with `LOADMETER_` can be read. This stops an imported scenario from reading other secrets |
| `${__urlencode(value)}` | Percent-encoded value |

Use `${__env(LOADMETER_API_TOKEN)}` to keep secrets out of the file. See [SECURITY.md](SECURITY.md).

## Steps

Every step has a `type`. Common fields: `name`, `disabled` (skip this step), `comment`.

| `type` | Uses | Meaning |
|---|---|---|
| `request` | `request` | Send one HTTP request. `name` labels it in reports |
| `transaction` | `steps`, `name` (required) | A named group. Load Meter reports its time (think time is subtracted) and counts it failed if any inner request failed |
| `group` | `steps`, optional `select` | A group with no timing of its own. With `"select": "weighted"` it runs exactly one inner step per execution, picked at random in proportion to each inner step's `weight` (default 1; `0` or `disabled` = never). Used by access-log imports to reproduce an endpoint mix |
| `loop` | `steps`, `count` (at least 1) | Repeat the inner steps `count` times |
| `if` | `steps`, `condition` | Run the inner steps only when the condition is true |
| `think` | `think` | Pause. See below |
| `set` | `set` (array of `{name, value}`) | Set variables. Values are expanded |

`condition` forms: `${a} == b`, `${a} != b`, or a single `${a}`. A single value is true when it is not empty, not `false` and not `0`. Quotes around operands are ignored. An unresolved variable in a single-operand condition counts as false.

`think`: `{"min": "200ms", "max": "500ms"}`. With `max` larger than `min`, the pause is random between them. `workload.thinkTimeScale` multiplies all think times (`0` removes them, `1` keeps them).

`think.distribution` replaces the uniform draw with a statistical distribution (parameters in seconds); `min` and `max` then act as clamps (`max` 0 = no upper clamp): `{"min": "300ms", "max": "2m", "distribution": {"type": "lognormal", "mu": 2.3, "sigma": 1.1}}`. Types: `lognormal` (`mu`, `sigma` of ln seconds), `exponential` (`mean`), `gamma` (`shape`, `scale`), `uniform` (uses `min`/`max`). Sampling uses the run seed, so runs are reproducible. Exports to k6 and JMeter keep only `min`/`max` and say so in their report.

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
| `connectTimeout`, `tlsTimeout`, `responseHeaderTimeout` | duration | Override the defaults for this request. See [HTTP layer](#http-layer) |
| `followRedirects` | boolean | Overrides the default |
| `extract` | array of extractors | Save values from the response |
| `assert` | array of assertions | Check the response |

Only one body form is used. The order is `multipart`, then `form`, then `body`. Load Meter sets `Content-Type` for `form` and `multipart` if you did not. It sends `User-Agent: Load Meter-Performance/0.1` if you did not set one.

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
| `timing` | `total`, `ttfb`, `connect`, `dns` or `tls` | That time for this request in milliseconds, such as `12.345`. `connect` is `0` on a reused connection |

## Assertions

An assertion fails the request when it is not true.

| `type` | Uses | Passes when |
|---|---|---|
| `status` | `value` | The status matches. `"200"`, `"2xx"`, or a list `"200,201,3xx"`. Empty value means status below 400 |
| `contains` | `value` | The body contains the text |
| `not_contains` | `value` | The body does not contain the text |
| `regex` | `expr` | The body matches the expression |
| `jsonpath` | `expr`, optional `value` | The path exists. With `value`, one of the matches equals it |
| `max_latency` | `value` (duration), optional `expr` | The response time is at most the value. `expr` picks another time: `ttfb`, `connect`, `dns`, `tls` (default `total`) |
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

`loadmeter run` exits with code 3 if any SLA fails.

## Complete example

This is a small, valid scenario for the bundled mock server (`loadmeter mock`). It uses a variable, a data source, an extractor, assertions, a loop, a condition, a function and an SLA. For a larger sample see `examples/shop-login.json`.

```json
{
  "version": "loadmeter/v1",
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
loadmeter mock &
loadmeter validate shop.json
loadmeter run shop.json --vus 5 --duration 10s
```
