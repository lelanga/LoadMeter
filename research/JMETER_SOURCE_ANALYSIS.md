# Apache JMeter source analysis for Load Meter

This study covers the Apache JMeter source (Apache-2.0) at commit `762310f4` (2026-10-01). It reads JMeter's algorithms and edge cases from the code, compares them with Load Meter as it is in this repository, and ends with a ranked list of gaps. All descriptions are written for this document. Quotes are kept to a line or two and carry a `file:line` reference. No JMeter code is copied, and none should be. Each port listed here is a fresh implementation of the documented behaviour.

**Path abbreviations** (all relative to `/home/claude/apache/jmeter`):

| Prefix | Directory |
|---|---|
| `C/` | `src/core/src/main/java/org/apache/jmeter/` |
| `CK/` | `src/core/src/main/kotlin/org/apache/jmeter/` |
| `K/` | `src/components/src/main/java/org/apache/jmeter/` |
| `H/` | `src/protocol/http/src/main/java/org/apache/jmeter/protocol/http/` |
| `F/` | `src/functions/src/main/java/org/apache/jmeter/functions/` |
| `J/` | `src/jorphan/src/main/java/org/apache/jorphan/` |
| `bin/` | `bin/` (default `jmeter.properties`, `reportgenerator.properties`) |

Load Meter paths are relative to `/home/claude/loadmeter`.

**Uncommitted work in progress.** While this was being written, uncommitted changes appeared in the working tree:

- `group.select: "weighted"` with a per-step `weight` (`internal/model/model.go`, `internal/engine/vu.go pickWeighted`).
- `think.distribution` with lognormal, exponential, gamma and uniform (`internal/model/dist.go`).

Where they matter, the tables below say "(WIP)". Everything else describes `HEAD`.

**What was read in Load Meter:** `docs/SCENARIO_FORMAT.md`, `internal/model/model.go`, `internal/engine/{engine,vu,data}.go`, `internal/vars/vars.go`, `internal/extract/extract.go`, `internal/metrics/{collector,histogram}.go`, `internal/report/report.go`, `internal/jmx/SUPPORT_MATRIX.md` and parts of `internal/jmx/{import,export}.go`, `internal/safety/safety.go` and `internal/distributed/protocol.go`.

---

## 1. Component inventory

Status describes **Load Meter runtime behaviour**, not JMX import mapping:

- **have**: equivalent behaviour.
- **partial**: a subset, or different semantics.
- **missing**: no equivalent.
- **n/a**: out of scope by design.

### 1.1 Threads, workload, lifecycle

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| Thread Group | `C/threads/ThreadGroup.java` | N threads. Thread *i* starts at about `i·rampUp/N` (`:241`). Loops or a scheduler (duration, startup delay `:199-216`). "Same user on each iteration" defaults to true (`CK/threads/AbstractThreadGroupSchema.kt:43-44`) | **partial**: `constant-vus`, `ramping-vus` and `per-vu-iterations` (loops or duration). No startup delay and no "new user each iteration" |
| Scheduler end | `C/threads/JMeterThread.java:239-245, 497-500` | After each sampler the thread checks `now >= endTime` and stops in the middle of the iteration. An in-flight sample finishes | **have** (different): Load Meter stops new iterations and gives in-flight ones `gracefulStop` |
| On sampler error | `JMeterThread.java:268-295, 599-607` | continue / start next thread loop / stop thread / stop test / stop test now | **partial**: `onError` is continue / abort-iteration / stop-vu. There is no "stop test" |
| setUp / tearDown Thread Group | `C/threads/SetupThreadGroup.java`, `PostThreadGroup.java` | Their own thread pool, run once before or after all groups | **partial**: `setup`/`teardown` run once *per VU* |
| Multiple thread groups | `C/engine/StandardJMeterEngine` | Run in parallel or one after another | **missing**: one workload per scenario |
| Open Model Thread Group | `CK/threads/openmodel/*.kt` | Schedule DSL (`rate() random_arrivals() even_arrivals() pause()`). **A fresh thread for every arrival**, and it runs one iteration (`OpenModelThreadGroupController.kt:34`). Thread count is unbounded | **partial**: `constant-` and `ramping-arrival-rate` use even spacing only. VUs are reused from a pool capped by `maxVUs`. Iterations that can't start are counted as dropped |
| Concurrency / Ultimate / Arrivals TG | jmeter-plugins (**not in this repo**, verified by grep) | Target concurrency, piecewise schedules | **partial**: the JMX importer maps them to `ramping-vus` |
| Flow Control Action | `K/sampler/TestAction.java` | Pause, stop thread or test, go to next iteration, break loop | **partial**: pause only (a `think` step) |
| Distributed mode | `C/samplers/SampleSenderFactory.java:52` (default `StrippedBatch`) | RMI. **Every server runs the full thread count**, and samples go back to the client | **have** (better): HTTPS agents, the workload is split, histogram deltas are merged (`internal/distributed`) |
| Non-GUI run / summariser | `C/reporters/Summariser.java`, `bin/jmeter.properties:854` | Console summary every 30 s | **have**: CLI live output |
| Timer factor | `JMeterThread.java:83-89, 991-997` | Multiplies every "modifiable" timer delay | **have**: `workload.thinkTimeScale` |

### 1.2 HTTP sampler and HTTP config

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| HTTP sampler (HC4) | `H/sampler/HTTPSamplerBase.java`, `HTTPHC4Impl.java` | Request building, timing, sub-results | **have** (`internal/engine/vu.go`) |
| Success code rule | `HTTPSamplerBase.java:1821` → `MetricUtils.isSuccessCode` | Codes 200–399 are OK | **have**: `<400` is OK unless a `status` assertion exists |
| Redirects | `HTTPSamplerBase.java:1654-1734, 1749-1760, :295` | Max 20 hops (`httpsampler.max_redirects`). 301/302/303 change to GET (HEAD stays HEAD); 307/308 keep the method. `/..` is removed and spaces are encoded. Each hop is a sub-result, and the label is `a->b` | **partial**: Go client, max 10 hops, same method rules. Only the final response is recorded |
| Connect / response timeouts | `HTTPHC4Impl.java:1275-1283` | Separate connect and socket-read timeouts. The read timeout applies **between packets**, not to the whole response | **partial**: one overall deadline. Dial is fixed at 10 s and TLS at 10 s (`engine.go:128,136`) |
| Keep-alive switch | `HTTPHC4Impl.java:1288-1296` | `use_keepalive=false` sends `Connection: close` | **missing** |
| Connection pool per thread | `HTTPHC4Impl.java:520` (thread-local client map) | **Each JMeter thread has its own connections**. TTL is 60 s (`:424`) and stale connections are re-checked after 4.9 s idle (`:422`) | **missing**: one `http.Transport` is shared by all VUs (`engine.go:130`). Over TLS with HTTP/2 every VU multiplexes over one connection per host |
| Retries | `HTTPHC4Impl.java:413`, `bin/jmeter.properties:429` | 0 by default | **different**: Go's Transport silently retries idempotent requests when a reused connection fails |
| Reset state per iteration | `HTTPHC4Impl.java:1222-1231, 1824-1840`, `HTTPHCAbstractImpl.java:88` | Without "same user", idle connections, auth state and the SSL context are dropped at each iteration | **missing** |
| HTTP/2 | none in core (HC4 is HTTP/1.1 only, verified by grep) | — | **different**: Load Meter negotiates h2 over TLS (`ForceAttemptHTTP2`), and this can't be switched off |
| Compression | `HTTPHC4Impl.java:479-562` | Decodes gzip, x-gzip and deflate whatever request header the user set. Byte counts are wire bytes | **partial**: Go only decompresses gzip **when the user did not set `Accept-Encoding`**. Imported JMX headers pass through, so bodies stay compressed |
| Multipart | `HTTPHC4Impl.java:1532-1560` | multipart/form-data, browser-compatible headers, per-file MIME type | **have** (files are read into memory) |
| Source IP | `HTTPHC4Impl.java:1268-1272` | Bind to a local address (`ipSource`) | **missing** |
| Proxy | `HTTPHCAbstractImpl.java:46-52` + per-sampler fields | HTTP proxy with auth and non-proxy hosts | **missing**: `Proxy: nil` on purpose |
| Embedded resources | `HTTPSamplerBase.java:1353-1460, :190-192` | Parses HTML, applies allow and exclude URL regexes, downloads with a pool of 6 in parallel. A failure fails the parent unless `httpsampler.ignore_failed_embedded_resources` is set (`:1576-1600`) | **missing** |
| HTTP Request Defaults | `C/config/ConfigTestElement` merged into the sampler | Empty sampler fields inherit the defaults, including default parameters | **partial**: `baseUrl`, `timeout`, `headers`, `followRedirects` |
| Header Manager | `H/control/HeaderManager.java` | Merged by scope; the nearest one wins | **have** (`defaults.headers` + request headers) |
| Cookie Manager | `H/control/CookieManager.java` | RFC 6265 "standard" policy (`:103`), predefined cookies, clear each iteration (`:427-435`), drop empty-value cookies (`:69`), optional `COOKIE_<name>` vars (`:80, 302-306`) | **partial**: a Go cookiejar per VU. No clearing, no predefined cookies, no policy choice |
| Cache Manager | `H/control/CacheManager.java` | Per-thread browser cache emulation (see §2.13) | **missing** |
| DNS Cache Manager | `H/control/DNSCacheManager.java:227-247, 388` | Static host→IP entries, a custom resolver, a per-thread cache, clear each iteration | **missing**: only the `Host` header override |
| Auth Manager | `H/control/AuthManager.java:225-275` | Basic (preemptive by default, `HTTPHC4Impl.java:427`), Digest, Kerberos. Entries are matched by **URL prefix, first match wins** | **missing**: a manual `Authorization` header only |
| Keystore Config (client certs) | `K/config/KeystoreConfig.java` | Client certificate per thread, alias from a variable, index range | **missing** |
| Recorder / mirror | `H/proxy`, `H/control/HttpMirror*` | Recording proxy and an echo server | **have** (`internal/har/recorder.go`, `internal/mockserver`) |

### 1.3 Config elements, pre-processors and data

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| CSV Data Set | `K/config/CSVDataSet.java:153-226` | Share modes all/group/thread. Recycle, stopThread or `<EOF>` values. Advances **on each iteration of its parent controller** | **partial**: sequential/random/unique/per-vu modes, advances once per scenario iteration, no EOF-value mode (§2.8) |
| Random Variable | `K/config/RandomVariableConfig.java` | Random number with a format, per thread or shared, optional seed | **missing** (`${__randomInt}` only) |
| Counter | `K/modifiers/CounterConfig.java:93-120` | start/incr/end, wraps around, DecimalFormat, per user, reset on each TG iteration | **partial**: `${__counter()}` is global and not reset between runs (§4) |
| User Defined Variables | `C/config/Arguments` | Evaluated in order at test start, so later ones can use earlier ones | **partial**: a `variables` map. Values are **not re-expanded** (`vars.go:73-75`), so `{"b":"${a}/x"}` yields the literal `${a}/x` |
| User Parameters | `K/modifiers/UserParameters.java` | Per-thread value columns | **partial**: `set` step |
| JSR223 / BeanShell pre- and post-processors | `K/modifiers/JSR223PreProcessor.java`… | Scripting | **n/a** (never executed, by design) |
| URL Rewriting / RegEx User Parameters | `H/modifier/*` | Session-ID rewriting, regex-driven parameters | **missing** |
| Sample Timeout | `K/modifiers/SampleTimeout.java` | Interrupts a sampler after N ms | **have** (request `timeout`) |

### 1.4 Post-processors (extractors)

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| Regular Expression Extractor | `K/extractor/RegexExtractor.java` | Fields: body, unescaped body, document text, response or request headers, URL, code, message. Template `$n$`. Match 0 = random, N = Nth, −1 = all. Sets `_gN` and `_matchNr` | **partial**: body only, one group, no template, no "all". **Numbering differs**: Load Meter 0 = first, −1 = random |
| JSON Extractor | `K/extractor/json/jsonpath/JSONPostProcessor.java:147-202` | Jayway JSONPath. −1 gives `_1.._N`, `_matchNr`, optional `_ALL` concatenation | **partial**: a JSONPath subset (`extract.go`), no `_matchNr` or `_ALL` |
| Boundary Extractor | `K/extractor/BoundaryExtractor.java` | Left/right markers, same match rules as the regex extractor | **have** (body only) |
| XPath / XPath2 Extractor | `K/extractor/XPath*.java` | Full XPath 1/2, namespaces, tolerant parser | **partial**: small subset |
| CSS/JQuery Extractor | `K/extractor/HtmlExtractor.java` | CSS selector with attribute | **missing** |
| JMESPath Extractor | `K/extractor/json/jmespath` | JMESPath | **missing** |
| Debug Sampler / PostProcessor | `K/sampler/DebugSampler.java` | Dumps variables | **have** (Inspect / `Exchange`) |

### 1.5 Assertions

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| Response Assertion | `K/assertions/ResponseAssertion.java:225-399` | Fields × {contains (regex find), matches (whole), equals, substring} × NOT × OR. "Ignore status" sets success first (`:298-299`). Custom failure message | **partial**: contains / not_contains / regex / status / header. No NOT on regex, equals or status, no ignore-status, no URL or message fields |
| JSON Assertion | `K/assertions/JSONPathAssertion.java:112-180` | Existence, or a value compared **as a regex by default** (`ISREGEX` defaults to true, `:112-114`). Expect-null, invert | **partial**: existence and equality |
| Duration Assertion | `K/assertions/DurationAssertion.java` | elapsed > limit fails | **have** (`max_latency`) |
| Size Assertion | `K/assertions/SizeAssertion.java` | Compares size with an operator, optionally network size | **missing** |
| MD5Hex / XML / XMLSchema / HTML / XPath / Compare / SMIME | `K/assertions/*` | | **missing** |
| Assertion scope | `JMeterThread.java:893-934` | parent / children / all (3 levels deep) / variable | **partial**: final response only |

### 1.6 Timers

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| Constant / Uniform Random | `K/timers/ConstantTimer.java`, `UniformRandomTimer.java:35` | Fixed delay, or `|rand·range + offset|` | **have** (`think` min/max) |
| Gaussian Random | `K/timers/GaussianRandomTimer.java:35` | `|N(0,1)·dev + offset|` | **partial**: the importer approximates it as uniform. (WIP) `think.distribution` adds lognormal, exponential and gamma, but not a reflected Gaussian |
| Poisson Random | `K/timers/PoissonRandomTimer.java:304-385` | `|Poisson(λ) + offset|`: a Poisson-distributed **count** used as milliseconds, not exponential gaps | **partial**: approximated as uniform. (WIP) exponential is a different distribution |
| Constant Throughput Timer | `K/timers/ConstantThroughputTimer.java:181-264` | Per-thread or shared pacing to N samples per minute | **missing**: per-VU `pacing` only |
| Precise Throughput Timer | `K/timers/poissonarrivals/*` | Pre-generated Poisson schedule per thread group | **missing** (arrival-rate is the alternative) |
| Synchronizing Timer | `K/timers/SyncTimer.java:172-265` | Barrier across threads with a timeout | **missing** |
| Timer placement | `JMeterThread.java:555-560, 986-1015` | **All timers in scope are summed and slept before each sampler**, after pre-processors. They are not part of sample time | **different**: explicit `think` steps (the importer expands them per sampler) |

### 1.7 Logic controllers

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| Simple / Generic | `C/control/GenericController.java` | Grouping | **have** (`group`) |
| Loop | `C/control/LoopController.java` | N times or forever (−1) | **partial**: count ≥ 1 only |
| If | `C/control/IfController.java:179-230` | JS/JEXL expression, or a variable equal to `"true"` with "interpret as variable expression". evaluateAll | **partial**: `==`, `!=` and truthy |
| While | `C/control/WhileController.java:66-82` | Loops until the condition is the literal `false` (§2.10) | **missing** |
| ForEach | `K/control/ForeachController.java:154-258` | Iterates `in_1..in_N` | **missing** |
| Transaction | `C/control/TransactionController.java`, `TransactionSampler.java` | Parent sample or extra sample, include/exclude timers | **have**: always excludes think time, extra-sample style |
| Once Only | `K/control/OnceOnlyController.java:44-50` | First iteration of the thread only | **partial**: maps to setup |
| Throughput | `K/control/ThroughputController.java:186-241` | Deterministic percent or count, per thread or shared | **missing** |
| Interleave | `K/control/InterleaveControl.java` | One child per pass, round-robin, optionally across threads | **missing** |
| Random / Random Order | `K/control/RandomController.java:36-50`, `RandomOrderController.java:55` | One random child / all children shuffled | **partial (WIP)**: a weighted group picks one child (a superset of Random). Random Order is missing |
| Switch | `K/control/SwitchController.java:88-128` | Child by index or name (§2.10) | **missing** |
| Runtime | `C/control/RunTime.java:68-81` | Repeats children for N seconds | **missing** |
| Module / Include / Test Fragment | `K/control/ModuleController.java`, `IncludeController.java` | Reuse a fragment | **missing** (no fragments in the IR) |
| Critical Section | `K/control/CriticalSectionController.java` | Named global lock | **missing** |

### 1.8 Functions (`F/`, parser `C/engine/util/FunctionParser.java`)

JMeter has about 50 functions. Load Meter has 9: `uuid`, `randomInt`, `randomString`, `timestamp`, `counter`, `vu`, `iteration`, `env`, `urlencode`.

| Load Meter equivalent | JMeter functions |
|---|---|
| **have / partial** | `__UUID`, `__Random(min,max[,var])` (`Random.java:69`, inclusive), `__RandomString` (Load Meter has no charset argument), `__time` (Load Meter has no `/1000` divisor form, no `YMD`/`HMS`/`USER1` aliases and only a few format tokens, see `TimeFunction.java:57-114`), `__counter(TRUE|FALSE)` (Load Meter is global only, `IterationCounter.java:64-76`), `__threadNum`, `__urlencode`, `__P` (via `--set`) |
| **missing, cheap** | `__V` (nested variable names), `__eval`, `__evalVar`, `__split`, `__intSum`, `__longSum`, `__changeCase`, `__digest` (MD5/SHA-*), `__escapeHtml`, `__unescapeHtml`, `__escapeXml`, `__unescape`, `__urldecode`, `__char`, `__RandomDate`, `__timeShift` (ISO-8601 duration), `__dateTimeConvert`, `__isVarDefined`, `__isPropDefined`, `__RandomFromMultipleVars`, `__threadGroupName`, `__samplerName`, `__TestPlanName`, `__machineName`, `__machineIP`, `__log`, `__setProperty`, `__property` |
| **missing, needs design** | `__CSVRead`, `__StringFromFile`, `__FileToString`, `__StringToFile` (file I/O must go through `SafePath`), `__regexFunction`, `__XPath` |
| **n/a** | `__groovy`, `__javaScript`, `__jexl2`, `__jexl3`, `__BeanShell` (scripting) |

Escaping: JMeter function arguments accept `\,`, `\$` and `\\` (`FunctionParser.java:61-69`). Load Meter splits on every comma (`vars.go:119`), so a comma can't appear inside an argument.

### 1.9 Results, reporting and outputs

| Component | JMeter source | What it does | Load Meter |
|---|---|---|---|
| SampleResult timing | `C/samplers/SampleResult.java:1146-1163` | `elapsed = end − start − idle`. The timestamp is start (`bin/jmeter.properties:584`). Latency is time to first byte. Connect time is recorded separately | **partial**: elapsed and TTFB. No connect, DNS or TLS times |
| JTL CSV | `C/save/CSVSaveService.java` | Fixed column set including `grpThreads` and `allThreads` (`:98-99`), `Connect` and `IdleTime` | **partial**: `metrics/collector.go:107` has no thread counts, Connect or IdleTime. Transaction rows aren't marked |
| Aggregate Report percentiles | `J/math/StatCalculator.java:152-173` | Exact nearest-rank over all values | **have** (histogram, <0.8% error) |
| Dashboard statistics | `C/report/processor/PercentileAggregator.java:30-33` | Percentiles over a **sliding window of the last 20 000 samples** | **have** (better: full histogram) |
| APDEX | `C/report/processor/ApdexSummaryConsumer.java:74-128` | §2.15 | **missing** |
| Errors / Top-5 errors by sampler | `ErrorsSummaryConsumer.java:76-97` | Error key is `code/message`, or "Assertion failed" | **partial**: errors by `ErrKind`, no per-label top-N |
| Over-time graphs | `bin/reportgenerator.properties:81-136` | 60 s granularity by default: RT percentiles, latency, connect time, active threads, bytes, codes/s, TPS by label | **partial**: overall per-second percentiles, throughput, errors, VUs, CPU |
| Response-time distribution / synthetic distribution | `ResponseTimeDistributionGraphConsumer`, `bin/reportgenerator.properties:125-126` | Histogram, plus counts for ≤T, T..F, >F and failed | **missing** |
| Backend Listener (InfluxDB, Graphite) | `K/visualizers/backend/**` | Pushes aggregates every 5 s (`InfluxdbBackendListenerClient.java:99`) | **missing** |
| SLAs / pass-fail | none in core | — | **have** (Load Meter only) |

---

## 2. Algorithms worth porting (semantics as read from source)

### 2.1 Sampler execution order and think time on error (`JMeterThread.java:547-610`)

```
for each sampler reached by the controller tree:
  runPreProcessors(scope)                     # in scope order
  sleep(sum(timer.delay() for timers in scope))  # see 2.2; skipped if !running
  result = sample()
  if !result.ignore:
     fill grpThreads/allThreads (active counts at sample end)
     runPostProcessors(scope)                 # extractors run BEFORE assertions
     checkAssertions(scope)                   # sets JMeterThread.last_sample_ok
     notify listeners (sub-samples of a transaction go only to the TC's listeners)
  on failure: stopThread / shutdownTest / stopTestNow, or start the next loop
              (stops the current iteration, :276-282)
```

**Think time on error.** Timers belong to the *next* sampler, not the failed one, so they still run after a failure unless the action is "start next thread loop", which skips the rest of the iteration and its timers. Load Meter does the same with `onError: abort-iteration`. With `continue`, its explicit `think` steps still run.

**Extractors before assertions.** A failed assertion never stops an extraction. Load Meter has the same order (`vu.go:484-513`). Keep it.

### 2.2 Timer sum and the end-of-test clamp (`JMeterThread.java:986-1015`, `C/timers/TimerService.java:95-107`)

```
total = Σ delay_i            # each multiplied by timer.factor if the timer is modifiable
if scheduler and total > endTime − now: stop the thread (runTillEnd=false)
sleep(total)
```

A pause that would run past the scheduled end ends the thread instead of sleeping. Load Meter's `think` returns as soon as `stopIters` closes (`vu.go:232-237`), which gives the same result.

### 2.3 Random timers (`K/timers/*RandomTimer.java`)

- Uniform: `|U[0,1)·range + offset|` (`UniformRandomTimer.java:35`).
- Gaussian: `|N(0,1)·deviation + offset|` (`GaussianRandomTimer.java:35`). Negative draws are **reflected** by `abs`, not clipped. With `offset < 3·dev` the distribution folds at zero.
- Poisson: `|Poisson(λ=range) + offset|` in ms (`PoissonRandomTimer.java:304-305`). λ ≤ 30 uses Knuth's product-of-uniforms method (`:333-345`). λ > 30 uses the "PA" rejection method with a log-factorial approximation (`:350-385`).
- Go port: add `gaussian{offset, dev}` and `poisson{offset, lambda}` to the WIP `model.Distribution` types. Use `rand.NormFloat64()` and Knuth or PTRS sampling. Keep the `abs`, because imported plans depend on it. Then map the JMX timers to these types instead of to uniform.

### 2.4 Constant Throughput Timer (`K/timers/ConstantThroughputTimer.java:181-264`)

```
msPerSample = 60000 / throughputPerMinute          # re-read each call (may be ${var}/property)
mode ThisThreadOnly:             d = msPerSample
mode AllActiveThreads:           d = activeThreadsInTest  × msPerSample
mode AllActiveThreadsInGroup:    d = activeThreadsInGroup × msPerSample
mode *_Shared (all / group):     shared.next = max(now, shared.last + msPerSample)   # under a mutex
                                 delay = shared.next − now                            # (:253-264)
non-shared delay():
    target = previousTime + d          # previousTime=0 at start → first sample immediate
    if now > target: previousTime = now; return 0     # behind: NO catch-up burst, deficit is lost
    previousTime = target; return target − now
reset all state when a new test start time is seen (:211-216)
```

Semantics that matter:

1. It only **slows threads down**. If threads × latency can't reach the target, the shortfall is silent.
2. It counts **samples, not iterations**. A CTT at thread-group scope runs before every sampler in scope.
3. The non-shared "all threads" modes use the **current active count**, so during ramp-up each thread waits longer.
4. The shared modes give an even global spacing with no bursts.

Go port: a `throughput` step or `workload.throughput` limiter. Use a mutex-protected `next` timestamp per scope (scenario, group or VU) with the exact `max(now, last+interval)` rule. It costs about 30 lines and gives JMeter users a closed-model rate cap without moving to arrival-rate.

### 2.5 Precise Throughput Timer (`K/timers/poissonarrivals/PreciseThroughputTimer.java:95-138`, `ConstantPoissonProcessGenerator.java:73-146`)

```
per thread group (shared EventProducer, synchronized):
  generate block: n = ceil(rate·blockDuration)   # rate = throughput/period per sec, divided by batchSize
                  times = sort(n × (blockStart + U·blockDuration))   # fixed count, uniform → conditional Poisson
                  blockStart += blockDuration
  regenerate when exhausted, or when throughput changes (|Δ| < PRECISION check)
  batchSize>1: each event is reused batchSize times (same timestamp)
delay = groupStart + event·1000 − now; clamp ≥ 0     # late threads fire immediately → catch-up bursts
if now + delay > thread.endTime: throw StopThread
```

Notes from the source:

- `batchThreadDelay` is stored and passed to the generator, but the generator **never uses it** (`ConstantPoissonProcessGenerator.java:51-58,127-146`). Batches fire at the same instant.
- Each block has an exact count, so the long-run rate is exact, unlike a true Poisson process.
- Unlike CTT, a late thread catches up, because events in the past fire at once.

Go port: the arrival-rate executor could take `arrivals: "poisson"` and generate exactly this way (see 2.6). A closed-model "precise throughput" step could share the same generator.

### 2.6 Open Model Thread Group (`CK/threads/openmodel/`)

Schedule language: `rate(N/unit)`, `random_arrivals(dur)`, `even_arrivals(dur)` and `pause(dur)`. Two rates around an arrivals step give a linear ramp (`scheduleParser.kt:31-38`). `pause(d)` expands to `[lastRate, 0, even(d), 0, lastRate]` (`:188-202`).

```
for each arrivals step with beginRate b, endRate e (e = next rate step or b), duration D:
  n = floor((b+e)/2 · D)                                  # PoissonArrivalsRamp.kt:81
  random: flat = floor(min(b,e)·D) events at U·D;
          remaining n−flat at sqrt(U)·D (increasing) or (1−sqrt(U))·D (decreasing); sort
          (inverse-transform sampling of a linear density, :104-110)
  even:   t_k = k/mean  if b≈e, else (sqrt(b² + 2·a·k) − b)/a with a=(e−b)/D   # EvenArrivalsRamp.kt:60-81
starter: sleep until testStart + t; new JMeterThread(fresh clone, fresh vars); endTime = schedule end
at schedule end: wait for trailing pause, then interrupt still-running threads (OpenModelThreadGroup.kt:150-183)
```

Compared with Load Meter (`engine.go:361-433`):

- JMeter has **no thread cap and no dropped-iteration concept**. Saturation shows up as unbounded thread growth.
- Each arrival is a **brand-new user** with no cookies or connections carried over.
- Load Meter reuses VUs, and their cookie jars, across arrivals. It also only does even spacing, and it truncates the starting rate of `ramping-arrival-rate` to an integer (`engine.go:268`, `int(w.Rate)`).

Port the even/Poisson generator unchanged. It is closed-form, deterministic and seedable. Add `workload.freshVU: true` for JMeter OMTG parity.

### 2.7 Synchronizing Timer (`K/timers/SyncTimer.java:172-265`)

```
groupSize == 0 → barrier size = threads in group (set lazily at threadStarted)
delay(): arrival = barrier.await(timeout or ∞, clamped to thread endTime)
         on timeout/broken: log, return 0 (release whoever is waiting)
         if arrival==0 (last to arrive or failure): barrier.reset()
returns 0 always (the wait IS the delay)
```

Go port: a `sync` step `{"group": N, "timeout": "5s"}` built on a generation-counted barrier (mutex + cond, or a channel per generation). On timeout, release the waiters and start a new generation.

### 2.8 CSV Data Set (`K/config/CSVDataSet.java:153-226`, `FileServer`)

```
alias = file              (shareMode.all)
      | file@TG-identity  (shareMode.group)
      | file@thread-id    (shareMode.thread: EACH THREAD READS THE WHOLE FILE FROM LINE 1)
      | file@<custom>     (any other string = named sharing key)
names empty → first line is the header; else names split on "," ALWAYS (not on the delimiter, :210)
on iterationStart of the PARENT controller:
   line = readLine(alias, recycle, skipFirst = header||ignoreFirstLine)
   EOF: recycle → reopen (header skipped again)
        else if stopThread → JMeterStopThreadException
        else vars = "<EOF>" (csvdataset.eofstring, :94-95)
   quotedData=false (default) → plain split; true → RFC-4180-ish parse
```

Differences that matter in Load Meter:

1. `per-vu` picks row #VU, while JMeter's `thread` mode gives each VU a private cursor over the whole file.
2. There is no `<EOF>` continue mode. The importer maps it to `stop`.
3. A CSV can't advance per pass of a loop.
4. **Exporter bug**: `internal/jmx/export.go:452` joins `variableNames` with the delimiter, but JMeter splits names on comma. A `;` or tab CSV exported to JMeter gets one variable named `a;b;c`.

Go port: add `mode: "per-vu-cursor"` with a cursor per VU, `onExhaust: "eof"` with a configurable value, and `advance: "iteration"|"loop:<step-name>"`.

### 2.9 Regular Expression Extractor (`K/extractor/RegexExtractor.java:96-235, 465-542`)

```
template parsed once: literal text + $n$ group refs (e.g. "$1$-$2$")
if default != "" or useEmptyDefault: vars[ref] = default        # BEFORE matching (:110-113)
input = body | unescaped body | document text | resp headers | req headers | URL | code | message
matches = all find() results; if matchNumber>0 stop after N (:305)
matchNumber  > 0 → matches[N−1] (none if N > count)
matchNumber == 0 → matches[random]                              (:536-539)
matchNumber  < 0 → ref_matchNr=count, ref_1..ref_count = template(match_i), each with _gN;
                   remove stale ref_{count+1..prevCount}       (:208-229)
single match: ref = template(m); ref_g0..ref_gN, ref_g = group count; on no match remove _g vars
```

Edge cases:

1. **No default and no "use empty default" means a miss keeps the previous iteration's value.** This is a classic source of false-positive correlation.
2. With −1, the bare `ref` is *not* set unless a default exists.
3. A random pick uses the shared JMeter RNG.

The Boundary and JSON extractors follow the same 0/−1/N convention (`JSONPostProcessor.java:147-202`, which also has `_ALL` when compute-concat is on).

Load Meter (`extract.Pick`, `vu.go:484-503`) uses 0 = first and −1 = random. It writes the default (or "") on a miss, which avoids pitfall 1, except when `required:true` and the default is empty: then the stale value is kept (`vu.go:487`). Port: keep the JSON field `match`, but add `match: "all"` (`name_1..N`, `name_matchNr`) and `template`. Add `source: body|headers|url|status|message|requestHeaders` to regex and boundary extractors. Document the numbering difference from JMeter loudly.

### 2.10 Controller decision rules

- **Throughput Controller** (`ThroughputController.java:186-192, 225-241`). It runs on this pass if `executions < N` (count style), or if `(100·executions + 50)/(passes + 1) < percent` (percent style). The rule is **deterministic**, so 25% runs exactly on every 4th pass. The counters are per thread or shared under a lock.
- **Switch** (`SwitchController.java:88-128`). An empty value gives child 0. A number out of range also gives **child 0**. A name is matched exactly. An unmatched name gives the child named `default` (case-insensitive) if there is one, **otherwise nothing runs**.
- **While** (`WhileController.java:66-82`). It continues unless the condition is literally `false` (case-insensitive). An empty condition means "stop after a pass whose last sample failed". `LAST` also checks the last sample before the first pass. **An unresolved `${x}` is never `false`, so the loop never ends.**
- **ForEach** (`ForeachController.java:154-258`). It reads `in_<i>` (with `_` when "use separator" is set) for i from start+1 to end, and stops at the first missing variable. This is designed to consume regex −1 output.
- **Interleave**: one child per pass in rotation, per thread or across threads. **Random**: one uniformly random child. **Random Order**: `Collections.shuffle` of all children (`:55`). **Runtime**: repeats children until `(now − start)/1e9 ≥ seconds` (`RunTime.java:75`). The check runs between children, so overrun is up to one child's duration. **Once Only**: runs on the first iteration of the thread only (`OnceOnlyController.java:44-50`).

Go port: add step types `while{cond, max}`, `foreach{in, var}`, `switch{value, cases}`, `random`, `randomOrder`, `interleave`, `percent{p, perVU}` and `runtime{for}` in `model.go` and `vu.go execSteps`. Give `while` a mandatory `maxIterations` guard, default 10 000, to avoid the infinite-loop trap.

### 2.11 Transaction Controller timing (`TransactionController.java:199-318`, `TransactionSampler.java:106-137`)

- Parent-sample mode, excluding timers: `idle = wall − Σ child elapsed`, so pre/post-processor and timer time are both removed.
- Default mode, excluding timers: `pause += child.start − prevChild.end` (the gaps between children).
- Success: all children must succeed. The message is `"Number of samples in transaction : N, number of failing samples : F"`. The report generator **recognises transaction rows by that prefix** (`C/report/core/Sample.java:319-322`).
- Load Meter subtracts only `think` time (`vu.go:137-145`), which is close to default mode. Add `includeThink` and write the JMeter message prefix in JTL so the two tools agree.

### 2.12 Cookie handling (`H/control/CookieManager.java`, `HC4CookieHandler.java`)

- Policy: `standard` (RFC 6265) by default. `standard-strict`, `netscape` and others are selectable (`HC4CookieHandler.java:57-90`).
- Storing a cookie removes the matching (name, domain, path) entry first. An empty value deletes it (`delete_null_cookies=true`, `:69, 290-299`).
- `check.cookies=true` (`:83`) rejects cookies whose domain doesn't match.
- `testIterationStart` (`:427-435`) restores the predefined cookie list when "clear each iteration" is on, or when the thread group is set to "new user each iteration".
- Go: `net/http/cookiejar` already does RFC 6265 matching (without a public-suffix list, so `example.co.uk` supercookies are allowed). Add `defaults.cookies: {clearEachIteration, initial: [{name,value,domain,path,secure}]}` and implement it by swapping in a new jar per iteration in `vu.iteration`.

### 2.13 Cache Manager (`H/control/CacheManager.java`)

```
cacheable: method in cacheable_methods (default GET, :69) AND (2xx or 304) AND Vary != "*"  (:365-374)
store per thread (LRU, maxSize 5000 :67): Last-Modified, ETag, expires, Vary (header name+request value)
expires (only if useExpires):
   no-store → don't cache expiry; "no-cache" → expires=null (always revalidate)
   max-age=N → now+N s (overrides Expires, :307-314)
   else Expires header if present
   else neither: now + 10% × (Date − Last-Modified) heuristic, or +1 year if unparsable/missing (:324-352)
before request: if entry valid (expires > now) → don't send; default result = NO SAMPLE AT ALL
                (cache_manager.cached_resource_mode=RETURN_NO_SAMPLE, HTTPAbstractImpl.java:67-70, 578-593)
                else add If-Modified-Since / If-None-Match
clear on each iteration / with "new user"
```

Go port: per-VU `map[url+varyKey]entry` with an LRU list. Run it in `doRequest` before the limiter. When a fresh hit occurs, record nothing (like JMeter), or emit a "cached" sample marked as such. This matters only for page-level (embedded resource) scripts, so it ranks below the API-centric gaps.

### 2.14 DNS Cache Manager and Auth Manager

- **DNS** (`DNSCacheManager.java:227-247`). Static hosts are checked first. Then the per-thread cache. Then the system or custom resolver, which returns all A records, and the client uses them in order. Entries are cleared each iteration if configured. Go: `defaults.dns: {hosts: {"api.x":"10.0.0.5"}, perVU: true}` via a custom `DialContext` that rewrites the address **after** `safety.HostAllowed(logicalHost)` and also checks the resulting IP.
- **Auth** (`AuthManager.java:225-275`). The URL is compared as a string prefix in both default-port and explicit-port forms. The **first** entry in list order wins (not the longest). Basic is sent pre-emptively by default (`HTTPHC4Impl.java:427`). Digest and Kerberos use challenge and response. Go: `defaults.auth: [{urlPrefix, type: basic|digest|bearer, user, password, token}]`. Prefer longest-prefix matching, but keep JMeter's first-match order when importing.

### 2.15 Percentiles and APDEX (report generator)

- **Aggregate Report / Summary (GUI):** exact nearest rank. `target = round(count·p)`, then walk a sorted TreeMap until the cumulative count reaches the target. `p ≥ 1` gives the max (`StatCalculator.java:152-173`).
- **HTML dashboard:** commons-math `DescriptiveStatistics` with **window = 20 000** (`PercentileAggregator.java:30-33`) and the `LEGACY` estimator (`DescriptiveStatisticsFactory.java:26-28,40`). The position is `p·(n+1)` with linear interpolation, clamped to min/max. On long runs the dashboard "90th pct" covers only the **last 20 000 samples per label**.
- **Backend Listener:** the "fixed" window mode is the default (`bin/jmeter.properties:885-887`), and percentile stats keep only the **last 100 samples** (`SamplerMetric.java:37, 61, 74-78`). "timed" mode clears every interval and uses a 5 000-sample window.
- **APDEX** (`ApdexSummaryConsumer.java:74-128`): `apdex = (satisfied + tolerated/2) / total`. Failed samples count in `total` only (frustrated). The bounds are inclusive (`elapsed ≤ T`). The defaults are T = 500 ms and F = **1500 ms**, absolute rather than 4T (`bin/jmeter.properties:1357-1360`). Per-label thresholds come from a regex map (`reportgenerator.properties:49`). Empty transaction samples are skipped.
- **Error key** (`ErrorsSummaryConsumer.java:76-97`): `code + "/" + message`. If the code is a success code (or empty with a failure message), the key is `"Assertion failed"` or the assertion message.
- Load Meter computes percentiles from a full log-linear histogram (`metrics/histogram.go`), mergeable across agents. Keep that, and document that it is more accurate than JMeter's dashboard. Add APDEX from the same histogram, counting failures separately: `ok_le_T + ok_in_(T,F]/2` over `total`.

### 2.16 Backend Listener / InfluxDB (`K/visualizers/backend/influxdb/InfluxdbBackendListenerClient.java`)

- Sends every `backend_influxdb.send_interval=5` s (`:99`). Per transaction and per status (`all`, `ok`, `ko`): count, avg, min, max and the percentiles `99;95;90` (`:109-114`). Also `internal` user metrics (active, started and ended threads) and annotation events at start and end. Tags are `application`, `transaction` and `statut`.
- A Go port needs only `net/http` POSTs of line protocol (Influx v1/v2), plain-text Graphite (`name value ts\n` over TCP) and a Prometheus text `/metrics` endpoint. Feed it from `metrics.Collector` deltas, which already exist for distributed mode (`internal/metrics/delta.go`), and compute interval percentiles from the per-second sparse histograms, **not** from a 100-sample window.

---

## 3. Top-20 gaps (ranked)

Ranking weighs how often JMeter users depend on a feature, how cheap it is in Go stdlib, and the risk of a wrong measurement. Size: S ≤ 1 day, M ≤ 1 week, L > 1 week.

| # | Id | Feature | User value | Size |
|---|---|---|---|---|
| 1 | G01 | Correct compression handling | Correctness of imported JMX scripts | S |
| 2 | G02 | Extractor parity (all/template/sources) | Correlation in real scripts | M |
| 3 | G03 | HTTP auth: basic, digest, bearer | Very common config element | S–M |
| 4 | G04 | Split timeouts + timing breakdown | Diagnosis, JMeter parity | S |
| 5 | G05 | Connection model: per-VU pool, keep-alive, HTTP version, TTL | Measurement fidelity | M |
| 6 | G06 | while / foreach / switch | Data-driven flows | M |
| 7 | G07 | Assertion parity | Fewer "unsupported" imports | S |
| 8 | G08 | Throughput shaping in the closed model + Poisson arrivals | Most-used JMeter timer | M |
| 9 | G09 | Function and variable batch | Script portability | S–M |
| 10 | G10 | Mix controllers: random, order, interleave, percent, runtime | Workload mixes | M |
| 11 | G11 | Client certificates (mTLS) | Enterprise/banking APIs | S–M |
| 12 | G12 | DNS override / static hosts | Testing behind load balancers, blue/green | S |
| 13 | G13 | New user per iteration + cookie controls | Realistic sessions | S |
| 14 | G14 | CSV parity (cursor per VU, EOF, loop scope) | Data handling | S–M |
| 15 | G15 | Report: APDEX, distribution, top errors, JTL compatibility | Familiar outputs | M |
| 16 | G16 | Explicit HTTP proxy | Corporate networks | S |
| 17 | G17 | Redirect policy and per-hop samples | Login flows (SSO) | S–M |
| 18 | G18 | Streaming metrics (Influx/Graphite/Prometheus) | Grafana dashboards | M |
| 19 | G19 | Rendezvous (sync) and critical section | Spike tests, ordered access | S |
| 20 | G20 | HTTP cache emulation (+ later, embedded resources) | Browser-like page tests | M (L with resources) |

### G01: Correct compression handling (S)

- **Problem.** Go's Transport transparently gunzips only when *it* added `Accept-Encoding`. Plans imported from JMX commonly carry `Accept-Encoding: gzip, deflate, br` from a recorded Header Manager. The HAR importer strips that header (`internal/har/har.go:90`); the JMX importer does not. The result is that bodies stay compressed, regex and JSON extractors silently fail, and `BytesIn` counts compressed or decompressed bytes inconsistently.
- **Sketch.**
  1. In `vu.go doRequest`, when `Content-Encoding` is `gzip`/`x-gzip`/`deflate` and the Transport did not decode, wrap the body in `gzip.NewReader` or `flate`/`zlib` (stdlib).
  2. Leave `br` and `zstd` (not in stdlib) raw, and record a warning in `Exchange`.
  3. Count wire bytes with a counting reader placed under the decoder.
  4. Add `defaults.compression: auto|off`.
  5. In `internal/jmx/import.go`, drop `br` from imported `Accept-Encoding` values with a compatibility note.
- **Risks.** Double decoding if both layers decode. Gate it on `resp.Uncompressed`.

### G02: Extractor parity (M)

- **Sketch.** In `model.Extractor`, add:
  - `Match` as a string, `"all"`, or a number. Keep the numeric form for back-compatibility.
  - `Template` (`$1$-$2$`).
  - `Source` (`body|headers|requestHeaders|url|status|message`).
  - `KeepPrevious` for strict JMeter semantics.

  In `extract.Regex`, return all submatches. In `vu.extractOne`, write `name_1..N`, `name_matchNr` and `name_gK`, and remove stale `name_k` above the new count (§2.9). Add the same to boundary and jsonpath (`_ALL` concatenation). Update `jmx/import.go` so −1 maps to "all" instead of first.
- **Risks.** Variable-map growth with large `all` results. Cap at `maxMatches` (default 1000).

### G03: HTTP authentication (S–M)

- **Sketch.** Add `Defaults.Auth []AuthRule{URLPrefix, Type, User, Password, Token, Realm}`:
  - **basic**: set the header pre-emptively.
  - **bearer**: `Authorization: Bearer ${…}`.
  - **digest**: on a 401 with `WWW-Authenticate: Digest`, compute the RFC 7616 response (MD5, MD5-sess, SHA-256, qop=auth, client nonce counter per VU) with `crypto/md5` and `crypto/sha256`, then retry once. Cache the challenge per VU and host.

  Put the code in a new `internal/engine/auth.go`. Match rules by longest prefix, with an import flag for JMeter's first-match order. Values expand `${}` so they work with `__env`.
- **Risks.** The digest retry doubles the sample unless it is counted as a sub-request. Record a single sample covering both legs, as JMeter does inside HC4. Never log credentials: route them through `internal/redact`.

### G04: Timeouts and timing breakdown (S)

- **Sketch.** Add `Defaults`/`Request` fields `connectTimeout`, `tlsTimeout` and `responseHeaderTimeout` (time to first byte), plus `idleReadTimeout` (gap between reads, which is JMeter's semantics), alongside the existing overall `timeout`.
  - Apply connect and TLS through `net.Dialer.Timeout` and `TLSHandshakeTimeout` on the per-VU Transport (G05). Response headers use `Transport.ResponseHeaderTimeout`.
  - For the idle-read timeout, wrap the body reader in a deadline that resets on each `Read`.
  - Extend `httptrace` in `vu.go` with `DNSStart/Done`, `ConnectStart/Done`, `TLSHandshakeStart/Done` and `GotConn(Reused)`. Add `Sample.DNS`, `Connect`, `TLS` and `Reused`.
  - Write the `Connect` column to the JTL. Classify the timeout error kinds `connect_timeout`, `ttfb_timeout` and `read_timeout`.
- **Risks.** Per-request Transport settings require per-VU or per-profile Transports (G05).

### G05: Connection model fidelity (M)

- **Problem.** One shared Transport (`engine.go:130`) means VUs share pooled connections, so N VUs ≠ N connections. With HTTP/2 over TLS they multiplex over about one connection per host. TLS handshakes and per-connection server limits are under-exercised compared with JMeter, where each thread has its own HC4 connections (`HTTPHC4Impl.java:520`) and HTTP is always 1.1.
- **Sketch.** Add `defaults.connections: {mode: "per-vu"|"shared", keepAlive: bool, httpVersion: "1.1"|"2"|"auto", maxLifetime: "60s"}`.
  - **per-vu** (default for imported JMX): each `vuState` gets its own `http.Transport` clone with `MaxConnsPerHost` small.
  - **HTTP/1.1 only**: set `TLSNextProto` to an empty map and `ForceAttemptHTTP2=false`.
  - **keepAlive=false**: `DisableKeepAlives`.
  - **maxLifetime**: wrap the `net.Conn` with a creation time and close it on reuse after the TTL (JMeter TTL is 60 s).
  - **Retries**: disable Go's hidden idempotent retry by setting `req.GetBody=nil` where safe, or detect retries via `httptrace.GotConn` counts and flag them in the sample.
- **Risks.** Memory with 10k VUs × Transport (about 10 KB each, acceptable). Call `CloseIdleConnections` when VUs stop. Keep `GuardDialer` on every clone.

### G06: Data-driven loops: while, foreach, switch (M)

- **Sketch.** In `model.go`, add step types `while{condition, maxIterations}`, `foreach{input, var, start, end}` and `switch{value, cases:[{name, steps}], default}`.
  - **while**: stop when the condition is false. An empty condition means "until the last request failed" (§2.10), and an unresolved variable counts as **false**, which deliberately differs from JMeter and must be documented.
  - **foreach**: read `input_1..` until missing. This pairs with G02 "all".
  - **switch**: JMeter rules (an out-of-range index gives the first case, an unknown name gives `default` or nothing).

  Implement in `vu.execSteps`. Add JMX mappings to `jmx/import.go` and `export.go`.
- **Risks.** Runaway loops: enforce `maxIterations` and respect `ctx`/`stopIters` every pass.

### G07: Assertion parity (S)

- **Sketch.** In `model.Assertion`, add:
  - `Not bool`, applied to every type.
  - `IgnoreStatus` on `Request` (JMeter `assume_success`).
  - New types `equals`, `size` (`op`, `bytes`, body or network), `jsonpath` with `regex: true`, `expectNull`, and `hash` (md5/sha256 of the body).
  - `message` for a custom failure text.

  Put the code in `vu.assert` and `statusMatches`. Remove the "unsupported" rows from `SUPPORT_MATRIX.md`.
- **Risks.** JSON regex defaults: JMeter's `ISREGEX` defaults to true, and the importer already handles this (`import.go:1135`). Keep the Load Meter default as literal.

### G08: Throughput shaping and Poisson arrivals (M)

- **Sketch.**
  1. Add a `throughput` step or `workload.throughputCap {perMinute, scope: vu|scenario}` that implements the CTT shared rule (§2.4) with a mutex-guarded `next` timestamp, placed before the step's children or before every request.
  2. Add `workload.arrivals: "even"|"poisson"`, plus `seed`, to the arrival executors, using the block generator from §2.5 or §2.6 for exact counts and seedable reproducibility.
  3. Fix `engine.go:268`, which truncates the starting rate with `int(w.Rate)`, by passing a float.
- **Risks.** Users may expect CTT to *speed up* VUs. Add a finding when the achieved rate is below 95% of the target, which JMeter never reports.

### G09: Functions and variables (S–M)

- **Sketch.** In `vars.go`:
  - Recursive expansion of variable values (bounded depth 8), which also fixes the UDV chaining in §1.3.
  - Backslash escapes `\,`, `\$` and `\\` in function arguments, and parenthesis-aware argument splitting.
  - New functions: `__V`, `__split`, `__intSum`/`__longSum`, `__changeCase`, `__digest(alg,value[,salt])`, `__base64Encode`/`Decode`, `__escapeHtml`/`__unescapeHtml`/`__escapeXml`, `__urldecode`, `__timeShift(fmt,date,ISO8601dur)`, `__RandomDate`, `__dateTimeConvert`, `__isVarDefined`, `__randomFromList` (Load Meter naming), `__counter(true)` per VU, `__timestamp(/1000)` divisor and named aliases.
  - A complete SimpleDateFormat→Go layout converter (`yy`, `M`, `d`, `H`, `h`, `a`, `E`, `Z`, `X`), with an import mapping for each new function.
- **Risks.** Ambiguous date-token conversion; keep a table-driven test.

### G10: Mix controllers (M)

- **Sketch.** The WIP `group.select: "weighted"` already covers JMeter's Random Controller. Extend `select` with `"shuffle"` (Random Order) and `"roundRobin"` (Interleave, with `acrossVUs`), and add step types `percent{percent, perVU}` (the deterministic §2.10 rule) and `runtime{for}`. Put per-VU state in `vuState` (a map from step pointer to counter) and shared state in `Run` (mutex). All of these are about 20 lines each in `vu.execSteps`.
- **Risks.** Steps need stable identity for their state. Use the step's path index, not a pointer, because of `Clone()`.

### G11: Client certificates (S–M)

- **Sketch.** Add `defaults.tls: {clientCert, clientKey, pkcs12?, perVU: {source: "dataSource.column"}, caFile, serverName, minVersion}`. PEM loads with `tls.LoadX509KeyPair`. PKCS#12 is not in stdlib, so support PEM only and document a conversion command. A per-VU certificate works through `GetClientCertificate`, reading a VU variable; this needs per-VU Transports (G05). Resolve paths with `SafePath`.
- **Risks.** Key material in memory and in logs. Mark it `Sensitive` and redact. Encrypted PEM is not supported by stdlib, so refuse it with a clear error.

### G12: DNS override and static hosts (S)

- **Sketch.** Add `defaults.dns: {hosts: {name: ip|[ips]}, cacheTTL, perVU, clearEachIteration, roundRobin}`. Implement with a custom `DialContext` that maps `host:port` to `ip:port` after `policy.HostAllowed(host)`, then checks `policy.HostAllowed(ip)` as well. Keep `ServerName` as the logical host for TLS SNI. Round-robin across multiple IPs per VU.
- **Risks.** It can bypass the allowlist if only one side is checked. Test both.

### G13: New user per iteration and cookie controls (S)

- **Sketch.** Add `workload.newUserEachIteration` (JMeter's "same user" unchecked) and `defaults.cookies.{clearEachIteration, initial[]}`. In `vu.iteration`, replace the jar, clear the auth and cache state (G03, G20) and call `CloseIdleConnections` on the per-VU Transport (G05). Add an option to expose received cookies as `COOKIE_<name>`.
- **Risks.** It is meaningless on a shared Transport. Depends on G05 for connection reset.

### G14: CSV parity (S–M)

- **Sketch.** In `data.go`, add:
  - `mode: "per-vu-cursor"`, where each VU reads the whole file in order (JMeter `shareMode.thread`).
  - `onExhaust: "eof"` with `eofValue` (default `<EOF>`).
  - `skipHeader` together with `columns`.
  - `quoted: false` for a literal split.
  - `advance: "loop:<name>"`, so a data source can advance on each pass of a named loop step.
  - A named `shareKey` (JMeter custom share-mode strings).

  Fix `jmx/export.go:452` so `variableNames` is always comma-joined.
- **Risks.** Loop-scoped advance interacts with `scope: vu`. Reject that combination in validation.

### G15: Report and JTL compatibility (M)

- **Sketch.** In `internal/report` and `internal/metrics`:
  - APDEX overall and per label, with T/F thresholds and per-label regex overrides (§2.15).
  - Response-time distribution: buckets from the existing histogram, plus the "≤T / T–F / >F / failed" synthetic chart.
  - Top-5 errors per label with JMeter error keys (status/message).
  - Per-label over-time p90/p95.
  - A connect-time chart (after G04).

  In the JTL, add `grpThreads`, `allThreads`, `Connect`, `IdleTime`, `dataType`, `Encoding`, `SampleCount`, `ErrorCount` and `Hostname`, and write `"Number of samples in transaction : N, number of failing samples : F"` as the transaction `responseMessage`. With those, `jmeter -g results.jtl -o dir` can consume Load Meter output: JMeter reads `grpThreads`/`allThreads` without a presence check (`C/report/core/Sample.java:292-301`).
- **Risks.** The CSV column order must stay stable for existing consumers. Append columns at the end.

### G16: Explicit HTTP proxy (S)

- **Sketch.** Add `defaults.proxy: {url, user, password, noProxy[]}` (never from the environment implicitly). Set `Transport.Proxy` to a func, and `Proxy-Authorization` via the URL's userinfo.
- **Safety.** `GuardDialer` would then see only the proxy host. Check `policy.HostAllowed(target)` in the Proxy func **and** require the proxy host itself to be allowlisted.
- **Risks.** With CONNECT tunnelling, timing includes proxy setup. Report that in the Connect time.

### G17: Redirect policy and per-hop samples (S–M)

- **Sketch.** Add `defaults.maxRedirects` (JMeter default 20; Load Meter is hard-coded to 10 at `vu.go:58`). Add `redirectSamples: "final"|"hops"`. For `hops`, follow redirects manually: `CheckRedirect` returns `ErrUseLastResponse` and the loop runs in `doRequest`, applying RFC 9110 method rules (301/302/303 → GET except HEAD; 307/308 keep the method and body) and recording each hop as a sample `name#hopN`, with the aggregate named `a->b` like JMeter. Extractors run on the final response. Add an option to run them on all hops.
- **Risks.** Cookies must be applied per hop, which the jar handles. Go strips `Authorization` on cross-host redirects; JMeter's AuthManager re-applies it by URL prefix (G03).

### G18: Streaming metrics output (M)

- **Sketch.** Add a new `internal/sink` package with InfluxDB line protocol (HTTP v1/v2), Graphite plaintext over TCP, and a Prometheus text endpoint on `serve`. Feed it from `Collector` per-second deltas (`metrics/delta.go`). Interval percentiles come from merged sparse histograms. Tags: scenario, label, status (ok/ko/all) and agent. Use JMeter-compatible measurement and field names (`jmeter`, `count`, `avg`, `pct90.0`…) so existing Grafana dashboards keep working.
- **Risks.** Never block sample recording. Use a bounded channel and drop with a counter. Sink credentials go through `__env`.

### G19: Rendezvous and critical section (S)

- **Sketch.** Add step `rendezvous{group, timeout}` (barrier with generations, §2.7; `group: 0` means current active VUs) and `critical{name, steps}` (a named `sync.Mutex` in `Run`). Report the rendezvous wait as think time, so it is excluded from transactions.
- **Risks.** Deadlock when VUs ramp down. The timeout is required, with a default of 30 s.

### G20: HTTP cache emulation (M), then embedded resources (L)

- **Sketch.** A per-VU cache implementing §2.13 in a new `internal/engine/cache.go`, with `defaults.cache: {enabled, useExpires, maxEntries, onHit: "skip"|"sample"}`.
- **Embedded resources (later).** Stdlib has no HTML parser (`golang.org/x/net/html` would break the stdlib-only rule), so write a small tokenizer for `src`/`href` of `img`, `script`, `link[rel=stylesheet]`, `iframe` and `background`, plus CSS `url()`. Add allow and exclude regexes and a 6-wide concurrent pool per VU, with the parent elapsed time running from the first start to the last end.
- **Risks.** Tokenizer edge cases. Keep it opt-in and clearly labelled "approximate".

**Next candidates (beyond the top 20):** multiple workloads per scenario (thread-group parity); stop-test on error; module/fragment reuse; source-IP binding; CSS/JMESPath extractors; `onError: stop-test`; a startup delay per workload.

---

## 4. Pitfalls JMeter users hit, and how Load Meter should avoid them

1. **Coordinated omission in closed models.** A JMeter thread waits for the response before its next sample. When the system slows down, the sample count falls and the slow period is under-represented. CTT makes this worse because it *drops* the deficit (`ConstantThroughputTimer.java:187-190`). PTT bursts to catch up (`PreciseThroughputTimer.java:103-107`). **Load Meter:** keep arrival-rate executors prominent. For the open model, record each iteration's *intended* start and report "schedule lag" (`actualStart − intendedStart`). Today `runArrival` silently starts late iterations immediately (`engine.go:386-396`). For CTT-like caps (G08), report the achieved rate against the target.
2. **Percentiles over truncated windows.** Dashboard p90/p95/p99 use only the last 20 000 samples per label (`PercentileAggregator.java:30-33`). Backend-listener percentiles use the last 100 by default (`SamplerMetric.java:37`). The GUI Aggregate Report uses exact nearest rank, so the three disagree. **Load Meter:** never window. Keep the mergeable histograms, and state the method and error bound in the report.
3. **Listener overhead.** GUI listeners, View Results Tree, saving response data and many JTL columns all compete with load generation. **Load Meter:** `Collector.Add` takes one global mutex and writes the raw CSV under it (`metrics/collector.go:132-161`). At high RPS that serialises every VU. Shard the stats per VU and merge per second, and move CSV writing to a buffered channel and goroutine. Keep `Inspect` off in load runs.
4. **Stale correlation values.** A regex or JSON extractor with no default keeps the previous iteration's value on a miss (`RegexExtractor.java:110-113`), so the next request "works" with a stale token. **Load Meter** resets to the default or "". Keep that, and also clear the variable when `required` fails (`vu.go:487` currently keeps the stale value).
5. **Match-number confusion.** JMeter 0 = random and −1 = all; Load Meter 0 = first and −1 = random. Hand-ported scripts will misbehave. Use a string form (`"random"`, `"all"`) in docs and the UI, and warn when a scenario uses `0`/`-1` numerically.
6. **Timer scope surprises.** A timer at thread-group level runs before **every** sampler (`JMeterThread.java:560`), and CTT throughput counts samplers rather than iterations. Load Meter's explicit `think` steps avoid this. Keep it, and make the importer's expansion visible in the compatibility report (it already is).
7. **Infinite While loops.** An unresolved `${var}` is never `false` (`WhileController.java:78-79`). Load Meter's `while` (G06) must treat unresolved as false and enforce `maxIterations`.
8. **HTTP/2 and connection sharing change the system under test.** JMeter core speaks HTTP/1.1 with connections per thread. Load Meter shares one Transport and negotiates h2, so the same "100 users" opens about 1 TCP connection instead of 100. Server tuning such as connection limits, TLS cost and LB distribution is not exercised. Make this explicit (G05) and default imported JMX to `per-vu` + HTTP/1.1.
9. **Hidden retries.** HC4 retry count is 0 (`HTTPHC4Impl.java:413`). Go may resend idempotent requests on a broken keep-alive connection without telling you, which hides server-side resets. Detect this through `httptrace` and count it (G05).
10. **Compressed bodies and byte counts.** See G01. JMeter decodes gzip and deflate whatever header was set and counts wire bytes. Load Meter currently counts decompressed bytes when Go decodes and raw bytes otherwise, so bandwidth figures are inconsistent between scenarios.
11. **Distributed mode multiplies load.** In JMeter, each remote server runs the *full* thread count, so 3 servers × 100 threads = 300. Load Meter splits the workload. Keep it that way, but print the per-agent share before the start so JMeter users aren't surprised.
12. **Cookie Manager absent = no cookies.** JMeter sends no cookies without a Cookie Manager. Load Meter enables them by default. The importer already sets `disableCookies` when no Cookie Manager is present. Keep that warning.
13. **Process-global state across runs.** JMeter had to reset timer state when a new test starts (`ConstantThroughputTimer.java:211-216`, issue 6165). In Load Meter, `vars.globalCounter` is a package global (`vars.go:20`) that is never reset, so in a long-lived `serve` process the second run's `${__counter()}` continues from the first. Reset it in `engine.Prepare`, or hold it on `Run`.
14. **Out-of-range Switch values silently pick the first child** (`SwitchController.java:97-99`), while an unknown *name* picks nothing. If Load Meter adds `switch`, make out-of-range an error, or `default`, consistently.
15. **APDEX F is not 4T by default** in JMeter (500/1500 ms). When Load Meter adds APDEX, show both thresholds explicitly.
16. **Scheduler stops mid-iteration.** At the end of a JMeter duration, threads stop after the current sampler, leaving partial transactions that are never reported. Load Meter's `gracefulStop` is better. Keep reporting interrupted iterations separately.

---

## 5. Load Meter defects found during the comparison

These are small and independent of the gap list. Each was verified by reading the code.

| Where | Issue | Fix |
|---|---|---|
| `internal/jmx/export.go:452` | `variableNames` joined with the CSV delimiter. JMeter always splits on `,` (`CSVDataSet.java:210`) | Always join with `,` |
| `internal/engine/engine.go:268` | Ramping-arrival start rate is truncated with `int(w.Rate)` (0.5/s becomes 0) | Make `stageTarget` take a float64 start |
| `internal/engine/engine.go:420-424` | In `runArrival`, a VU whose setup fails is never returned to `idle` but still counts toward `allocated`, so the pool shrinks for good | Return it, or decrement `allocated` |
| `internal/vars/vars.go:20,163` | `__counter` is global across runs in one process | Reset it per run |
| `internal/vars/vars.go:73-75` | Variable values aren't re-expanded, so chained `variables` don't resolve | Bounded recursive expansion (G09) |
| `internal/vars/vars.go:119` | Function arguments split on every comma with no escape | Escape-aware splitter (G09) |
| `internal/engine/vu.go:487` | A `required` extractor with an empty default leaves the previous value in place | Always overwrite |
| `internal/metrics/collector.go:107-160` | The JTL lacks `grpThreads`/`allThreads`, and transaction rows can't be told apart, so JMeter's report generator can't read it correctly | G15 |
| `internal/jmx/import.go` | `Accept-Encoding` from a Header Manager is kept, so responses arrive compressed (Go won't decode them) | G01 |
