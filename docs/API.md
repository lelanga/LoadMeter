# Load Meter local HTTP API

`loadmeter serve` starts the web UI and this JSON API on `127.0.0.1:7878` (loopback only by default).

## Authentication and browser safety
- On start the server prints a URL with a one-time token: `http://127.0.0.1:7878/?token=XYZ`.
- Opening it sets an `HttpOnly; SameSite=Strict` cookie `loadmeter_session`. Every `/api/*` call needs that cookie or an `X-LoadMeter-Token: XYZ` header.
- Requests whose `Host` header is not `127.0.0.1`, `localhost` or `[::1]` (with port) are rejected. This blocks DNS rebinding.
- State-changing calls must send `Content-Type: application/json` (or multipart for uploads) and the `X-LoadMeter-Token` header. The UI reads the token from `GET /api/session`, which works with the cookie.
- Error responses are always `{"error": "message"}` with a 4xx/5xx status.

## Session
| Method | Path | Body | Response |
|---|---|---|---|
| GET | /api/session | – | `{token, version, workspace}` |
| GET | /api/health | – | `{status:"ok"}` |

## Scenarios (stored as `<workspace>/scenarios/<file>.json`)
`{file}` is a bare file name like `shop-login.json`: letters, digits, `-`, `_` and `.json` only.

| Method | Path | Body | Response |
|---|---|---|---|
| GET | /api/scenarios | – | `[{file, name, description, requests, executor, maxVUs, duration, modified}]` |
| GET | /api/scenarios/{file} | – | Scenario JSON (see `internal/model/model.go`) |
| PUT | /api/scenarios/{file} | Scenario JSON | `{file, problems:[]}`; a history snapshot is saved before overwriting |
| DELETE | /api/scenarios/{file} | – | `{deleted:true}` (the file moves to history and can be restored) |
| POST | /api/scenarios/{file}/validate | – | `{problems:[], targets:[hosts], blocked:[hosts]}` |
| POST | /api/scenarios/{file}/debug | `{vars?:{}}` | Runs ONE iteration with 1 VU: `{exchanges:[Exchange], ok}`. Sensitive headers and fields are redacted. |
| GET | /api/scenarios/{file}/history | – | `[{id, time, reason}]` |
| POST | /api/scenarios/{file}/history/{id}/restore | – | `{restored:true}` |
| GET | /api/scenarios/{file}/diff/{id} | – | `{changes:[{path, before, after}]}` (snapshot vs current) |

`Exchange`: `{vu, iteration, step, method, url, requestHeaders, requestBody, status, responseHeaders, responseBody, latencyMs, extracted, missingExtractions, assertions:[{type,expr,value,passed,message}], varsUsed, unresolved, ok, error}`

## Import / export
| Method | Path | Body | Response |
|---|---|---|---|
| POST | /api/import/har | multipart: `file`, `name?`, `includeStatic?`, `keepCredentials?` | `{file, report:CompatReport, correlations:[Candidate], parameters:[ParamSuggestion]}` |
| POST | /api/import/jmx | multipart: `file`, `name?` | `{file, report:CompatReport}` |
| POST | /api/import/loadrunner | multipart: `files` (repeat; all files of the script folder), `name?` | `{file, report:CompatReport, analysis}` |
| GET | /api/export/jmx/{file} | – | `.jmx` download; header `X-Load Meter-Compat` carries a short summary |
| GET | /api/export/jmx/{file}/report | – | CompatReport JSON |

`CompatReport`: `{source, format, items:[{element,name,location,status,note}], warnings, manualChanges, confidence, validation}`. `status` is one of supported, partial, unsupported or ignored.

## Correlation and parameterization
| Method | Path | Body | Response |
|---|---|---|---|
| POST | /api/scenarios/{file}/correlate | – | `{candidates:[Candidate], dynamicUnexplained:[...]}` (needs the recording saved during HAR import) |
| POST | /api/scenarios/{file}/correlate/apply | `{ids:[...], validate:true, policy:"suggest"|"validated"|"manual"|"auto"}` | `{applied:[ids], validation:{ok, details:[...]}, file}` |
| POST | /api/scenarios/{file}/parameterize | – | `{suggestions:[ParamSuggestion]}` |
| POST | /api/scenarios/{file}/parameterize/apply | `{ids:[...], rows?:int}` | `{applied:[ids], dataFile}` |

`Candidate`: `{id, variable, value(masked), source:{step, location, extractor}, destinations:[{step, location}], evidence:[...], confidence (0–1), security:"secret"|"session"|"identifier"|"other", suggestedReplacement, validation:"not-run"|"passed"|"failed", explanation:{what, why, risk, howValidated, manualReview}}`

## Runs
| Method | Path | Body | Response |
|---|---|---|---|
| POST | /api/runs | `{file, overrides?:{vus,duration,rampUp,rate,maxVUs}, baseline?, confirm?:bool}` | `{runId}`. Returns 409 `{error, needsConfirmation:true}` when a non-loopback target needs confirmation, 403 when a host is not allowlisted. |
| GET | /api/runs | – | `[{runId, scenario, startedAt, durationSec, requests, p95Ms, errorRate, slaPassed, hasSLAs, status}]` |
| GET | /api/runs/active | – | `{runId, state, scenario}` or `{}` |
| POST | /api/runs/{id}/stop | – | `{stopping:true}` (call again to force) |
| GET | /api/runs/{id}/live | – | **SSE**. `event: snapshot` every second with `data: Snapshot`; `event: done` with `data: Summary` at the end. |
| GET | /api/runs/{id} | – | Summary JSON |
| GET | /api/runs/{id}/report | – | HTML report |
| GET | /api/runs/{id}/raw.csv, /summary.csv, /summary.json | – | downloads |
| GET | /api/compare?base={id}&run={id} | – | Comparison |
| POST | /api/demo | – | Starts the built-in mock server and creates `demo-shop.json`: `{file, mockUrl}` |

`Snapshot` (live and in summary.metrics): `{elapsedSec, activeVUs, iterations, iterationFailures, droppedIterations, requests:MetricSet, transactions:MetricSet, byRequest:[Named], byTransaction:[Named], timeline:[{t,rps,tps,errors,errorRate,avgMs,p50Ms,p95Ms,p99Ms,maxMs,vus,bytesIn,cpuPercent,memMB}]}`.
Live snapshots send only the last 60 timeline points.
`MetricSet`: `{count, errors, errorRate, rps, minMs, avgMs, p50Ms, p90Ms, p95Ms, p99Ms, maxMs, bytesIn, bytesOut, status:{code:n}, errorKinds:{kind:n}}`.
`Summary`: `{runId, scenario, status, startedAt, endedAt, durationSec, workload, metrics:Snapshot, slas:[{metric,scope,op,threshold,actual,passed,note}], slaPassed, findings:[{kind:"observation"|"hypothesis", severity, title, evidence}], environment, baseline?}`.

## Settings, AI and audit
| Method | Path | Body | Response |
|---|---|---|---|
| GET/PUT | /api/settings/policy | `{allowedHosts:[], maxVUs, maxRPS, maxDuration, requireConfirmAbove}` | same |
| GET | /api/settings/ai | – | `{enabled, cloudConsent, localOnly, providers:[{id, kind, baseUrl, model, hasKey}], routing:{light, strong}, limits:{maxTokensPerCall, maxCallsPerHour, monthlyBudgetUSD}}` (keys are never returned) |
| PUT | /api/settings/ai | same shape, plus `apiKey` per provider (write-only) | same as GET |
| POST | /api/ai/test | `{provider}` | `{ok, model, latencyMs, error?}` |
| POST | /api/ai/ask | `{prompt, file?, confirm?:bool}` | `{plan:{steps:[{tool,args,why,needsConfirmation}]}, executed:[{tool, ok, result}], needsConfirmation:bool, message}` |
| POST | /api/ai/explain | `{file?, runId?, topic:"scenario"|"run"|"error"}` | `{text, provider, model, sentBytes}` |
| GET | /api/ai/activity | – | recent AI calls `[{time, task, provider, model, inputTokens, outputTokens, costUSD, redactions}]` |
| GET | /api/audit | – | last 200 audit events |
