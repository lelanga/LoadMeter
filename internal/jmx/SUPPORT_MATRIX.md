# JMeter (.jmx) support matrix

Package `internal/jmx` converts JMeter 5.x test plans to the Load Meter scenario IR
(`Import`) and back (`Export`). Every source element produces exactly one
`CompatItem` in the report. JMeter functions that can't be mapped get their own
`function __X` item. Statuses:

- **supported**: mapped. Any semantic difference we know of is in the note.
- **partial**: mapped approximately. The note says what changes.
- **unsupported**: not executed. The element is reported, and children of
  controllers are kept in a *disabled* group so nothing is lost.
- **ignored**: intentionally skipped. These are UI-only, disabled or cache elements.

"Verified" in this document means "covered by `go test ./internal/jmx/`". The
tests check how elements map and that a .jmx → Load Meter → .jmx → Load Meter round
trip gives the same result. They do **not** run JMeter or the Load Meter engine
against a server. Runtime equivalence is therefore not proven. Notes that
mention Load Meter runtime behaviour come from reading `internal/engine` at the
time of writing, and that package is still changing.

## Input safety

| Concern | Handling |
|---|---|
| Size | Input is read through `io.LimitReader`, and anything over 50 MB (`MaxInputBytes`) is rejected. |
| DTD / entities | Any `<!DOCTYPE ...>` or other directive is an error. This blocks XXE and billion-laughs input. Undefined entities fail in strict mode. |
| External resolution | None. `encoding/xml` never fetches anything. |
| Depth / element count | Nesting deeper than 256 levels or more than 2,000,000 elements is rejected. |
| Encoding | UTF-8, ISO-8859-1 and US-ASCII are accepted. Other declared encodings are rejected. |
| Scripts | JSR223, BeanShell and BSF code is never executed. The report records the language and an 80-character excerpt. |

## Import: test plan structure

| JMeter element | Status | Mapping / notes |
|---|---|---|
| `TestPlan` | supported | `Name`, `Description` (comments), `user_defined_variables` → `Variables`. "Run thread groups consecutively" and functional mode are noted. |
| `Arguments` (User Defined Variables) | supported | Mapped to `Variables`. Like JMeter, these are global regardless of where they sit. |
| `ThreadGroup` (first enabled) | supported / partial | See "Workload mapping" below. |
| Additional `ThreadGroup`s | partial | Not loaded. Their children become a disabled group named after the thread group. Split them into a separate scenario to run them. |
| `SetupThreadGroup` | partial | Children are appended to `Setup`. JMeter runs setUp once with its own thread pool; Load Meter runs setup once per VU. |
| `PostThreadGroup` | partial | Children are appended to `Teardown`. Same difference as setUp. |
| `kg.apc.jmeter.threads.UltimateThreadGroup` | partial | Schedule rows are summed into a piecewise-linear `ramping-vus` profile. Zero-length ramps become 1 s. Loop settings are ignored. |
| `com.blazemeter...ConcurrencyThreadGroup` | partial | Mapped to `ramping-vus`: ramp to TargetLevel over RampUp, then hold. Stepped ramps become linear. The Iterations limit is not applied. A missing hold becomes 10 m. |
| Other `*ThreadGroup` plugins (Stepping, Arrivals, FreeForm…) | unsupported | Children are kept as a disabled group. |
| Elements directly under the Test Plan (samplers/controllers) | ignored | JMeter does not execute them either. |
| Disabled elements (`enabled="false"`) | ignored | Not imported. The note gives the number of nested elements skipped with them. |

### Workload mapping (`ThreadGroup`)

| JMeter settings | Load Meter workload | Status |
|---|---|---|
| scheduler + duration D, ramp R < D, loops −1 | `ramping-vus` stages `[R→N, (D−R) hold N]` | supported |
| scheduler + duration D, ramp 0 | `constant-vus` N, D | supported |
| scheduler + duration, ramp ≥ D | single stage D→N | partial |
| scheduler + duration + finite loops | duration only (JMeter stops on whichever comes first) | partial |
| loops L, no scheduler | `per-vu-iterations` N × L | supported (partial when ramp > 0, because the ramp is lost) |
| loops −1, no scheduler | 10 m bounded run (ramping or constant) | partial |
| startup delay | not represented | partial |
| on_sample_error ≠ continue | not represented | partial |
| `${__P(name,default)}` in numeric fields | default value used and added to `Variables` | partial |

## Import: HTTP

| JMeter element / property | Status | Mapping / notes |
|---|---|---|
| `HTTPSamplerProxy` (also legacy `HTTPSampler`, `HTTPSampler2`) | supported | Request step named after the sampler. |
| domain / port / protocol / path | supported | Gives a relative URL when the server comes from scenario-level defaults. Otherwise the URL is absolute (protocol defaults to http, or https on port 443; default ports are omitted). A full URL in `path` is used as-is. |
| method | supported | Defaults to GET. |
| GET/HEAD/OPTIONS/TRACE/DELETE arguments | supported | Appended to the query. `always_encode` arguments are URL-encoded with `${...}` kept intact. |
| POST/PUT/PATCH arguments | supported | Mapped to `Form`. Values without `always_encode` are URL-decoded because Load Meter re-encodes them. Unnamed arguments are concatenated into `Body`. |
| `postBodyRaw` | supported | Mapped to `Body`, the concatenation of argument values. |
| `HTTPsampler.Files` / `DO_MULTIPART_POST` | supported | Mapped to `Multipart`: values first, then files with the mime type. Relative file paths resolve against a different base directory. |
| follow_redirects / auto_redirects | supported | `FollowRedirects = follow OR auto`. JMeter's redirect sub-samples are not reproduced. |
| response_timeout / connect_timeout | supported / partial | Mapped to the single overall `Timeout`. When a connect timeout is set, the two are added together (partial). |
| `image_parser` (embedded resources), `ipSource`, proxy, `use_keepalive=false`, non-UTF-8 contentEncoding | partial | Not represented. Each one is named in the note. |
| `ConfigTestElement` / HttpDefaultsGui at Test Plan or main-thread-group level | supported | Mapped to `Defaults.BaseURL` and `Defaults.Timeout`. Default parameters and embedded resources make it partial. |
| HTTP Request Defaults nested in a controller or setUp/tearDown | supported | Values are filled into each request in scope as absolute URLs. |
| `HeaderManager` at Test Plan level | supported | Mapped to `Defaults.Headers`. |
| `HeaderManager` elsewhere | supported | Merged into every request in scope. A header closer to the sampler wins (case-insensitive). |
| `CookieManager` | supported / partial | Enables cookies; a per-VU Go cookiejar is used. Non-standard policy, clearEachIteration and predefined cookies make it partial. **Without any Cookie Manager, `Defaults.DisableCookies=true`**, which matches JMeter, and a warning is added. |
| `CacheManager` | ignored | Load Meter does not emulate a browser cache. |
| `AuthManager`, `DNSCacheManager`, `KeystoreConfig`, `CounterConfig`, `RandomVariableConfig`, `LoginConfig`, `JDBCDataSource`, other `ConfigTestElement`s | unsupported | See "Model gaps". |

## Import: data, timers, logic

| JMeter element | Status | Mapping / notes |
|---|---|---|
| `CSVDataSet` | supported | `filename`, `delimiter` (`\t` becomes a tab), and `variableNames` (split on `,` as in JMeter; names joined with the delimiter, as older Load Meter exports wrote them, are accepted as partial) map to `Columns`. `recycle` maps to `OnExhaust=recycle`, and `stopThread` to `stop`. |
| CSV shareMode.all / shareMode.group | supported | Mapped to `sequential` (only one thread group is imported). |
| CSV shareMode.thread | partial | Mapped to `per-vu`. In JMeter each thread reads the whole file; in Load Meter the row is chosen by VU index. |
| CSV recycle=false, stopThread=false | partial | Mapped to `stop`. JMeter would continue with `<EOF>` values. |
| CSV ignoreFirstLine + explicit names | partial | Columns are cleared so the file header row is used. The model cannot skip a header and also name the columns. |
| CSV inside a loop or controller / non-UTF-8 | partial / note | Load Meter data sources are scenario-wide and advance once per iteration. |
| `ConstantTimer` | supported | A think step is emitted before **each sampler in scope**, matching JMeter's timer scoping. |
| `UniformRandomTimer` | supported | Think `[delay, delay+range]`. |
| `GaussianRandomTimer` | partial | Approximated as uniform `[offset−dev, offset+dev]`. |
| `PoissonRandomTimer` | partial | Approximated as uniform with the same mean. |
| Timers whose values use `${var}` | unsupported | Think needs a fixed duration. |
| `ConstantThroughputTimer` | supported | A `throughput` step (`perMinute`, `key` = timer location) before **each sampler in scope**. calcMode 0 → `scope: vu`, 1/2 → `active`, 3/4 → `shared` (group modes are noted: one workload). Stringprop and doubleProp forms are read. |
| `SyncTimer` | supported / partial | A `rendezvous` step (`group` = groupSize, 0 = active VUs) before each sampler in scope. timeoutInMs 0 (wait forever) becomes the 30 s default (partial). |
| `PreciseThroughputTimer`, `kg.apc` timers | unsupported | Use an arrival-rate executor (optionally `arrivals: poisson`). |
| `TestAction` (Flow Control Action) with pause | supported | Mapped to a think step, plus any timers in scope. |
| `TestAction` stop / restart / break | unsupported | |
| `UserParameters` | supported | A `set` step is emitted before each sampler in scope. Multiple thread columns, or per-iteration with more than one sampler in scope, make it partial. |
| `TransactionController` | supported | Mapped to a transaction step. With includeTimers=true it is partial, because Load Meter leaves think time out of transaction time. |
| `LoopController` (nested) | supported | Loop with `Count`. `-1` becomes an enabled group (partial). A count that isn't a number becomes 1 (partial). |
| `GenericController`, `RecordingController` | supported | Mapped to a group. |
| `CriticalSectionController` | supported | `critical` step with `key` = lockName (default `global_lock`), a run-wide lock. A lock name containing `${}` is used literally (partial). |
| `IfController` with a simple condition | supported | Accepted forms: `"${x}" == "y"`, `"${x}" != "y"` (single quotes too), and the same wrapped in `${__jexl3(...)}`, `${__jexl2(...)}` or `${__groovy(...)}`. These become `${x} == y`. `${__groovy(vars.get("x") != null && vars.get("x") != "")}` becomes `${x}`. evaluateAll makes it partial. |
| `IfController` with any other condition | unsupported | Children are kept in a disabled group, and the original condition is in the note. |
| `OnceOnlyController` | partial | Children move to `Setup`, which Load Meter runs once per VU before iterations instead of at the controller's position. Inside setUp, tearDown or a disabled container it becomes a plain group. |
| `WhileController` | partial | `while` step. Conditions: empty (stop after a pass whose last request failed), `LAST`, `${var}` (→ `${var} != false`), and the If Controller forms plus unquoted `${__jexl3(${x} != 10)}` (compared as text). Always partial: Load Meter caps the loop at 10 000 passes and an unresolved variable ends it (JMeter loops forever). Other conditions: unsupported, children in a disabled group. |
| `ForeachController` | supported | `foreach` step: inputVal → `input`, returnVal → `var`, useSeparator=false → `noSeparator`, start/end index. `__jm__<name>__idx` is not set. |
| `SwitchController` | supported | `switch` step with JMeter's selection rules. Each child element is one switch child (a sampler with timers is wrapped in a group of its name). |
| `ThroughputController` | supported | `percent` step: percent style → `percent`, total executions → `executions`; per user unchecked → `shared`. 0 % / 0 executions → disabled group. |
| `InterleaveControl` | supported / partial | Group with `select: roundRobin`; interleave across threads → `shared`. Partial when a child is a controller (Load Meter runs the whole child on its turn). |
| `RandomController` | supported | Group with `select: weighted`, equal weights. |
| `RandomOrderController` | supported | Group with `select: shuffle`. |
| `RunTime` | supported | `runtime` step (`duration` = seconds), checked between children like JMeter. 0 s → unsupported (disabled group). |
| `ModuleController`, `IncludeController`, `TestFragmentController` | unsupported | Children are kept in a disabled group. Each child still gets its own item, noted "inside non-executed container". |
| ThreadGroup "same user on each iteration" off | supported | `workload.newUserEachIteration: true`. |

## Import: assertions and extractors

| JMeter element | Status | Mapping / notes |
|---|---|---|
| `ResponseAssertion` on the body, Substring (16) | supported | Mapped to `contains`. |
| …Contains (2) | supported | Mapped to `regex` (find). The pattern must compile as Go RE2; otherwise unsupported (no lookaround or backreferences). |
| …Matches (1) | supported | Mapped to `regex` `^(?:p)$`. |
| …Equals (8) | supported | Mapped to `regex` `^quoted$`, with `${vars}` kept. |
| …Equals (8) | supported | Mapped to `equals`. |
| …NOT (4) with Substring or a literal Contains | supported | Mapped to `not_contains`. |
| …NOT with a regex or Equals | supported | Mapped to `regex` / `equals` with `"not": true` (one per pattern, AND). |
| …OR (32) | supported | Mapped to one alternation regex. NOT+OR (passes when any pattern is absent) is unsupported. |
| `ResponseAssertion` on the response code | supported | A code gives `status "200"`; `2\d\d`, `2..` or `2[0-9][0-9]` gives `"2xx"`; OR of codes gives `"200,201"`. NOT, AND of different codes and other patterns map to `equals`/`regex`/`contains` with `"source": "status"`. |
| `ResponseAssertion` on response headers (Substring "Name: value") | partial | Mapped to a `header` assertion (name equals, value contains). JMeter matches the raw header block. |
| `ResponseAssertion` on response headers (other) or request headers | partial | `"source": "headers"` / `"requestHeaders"`. Load Meter renders headers as sorted `Name: value` lines with canonical name casing (the response block starts with the status line). |
| `ResponseAssertion` on the response message or URL (sample_label) | supported | `"source": "message"` / `"url"`. |
| `ResponseAssertion` on request data or the document | unsupported | |
| `Assertion.custom_message` | supported | `message` on each mapped assertion. |
| `Assertion.assume_success` (Ignore Status) | supported | `ignoreStatus: true` on every request in scope. A Response Assertion with no pattern that only sets Ignore Status is mapped too. |
| `JSONPathAssertion` | supported | Mapped to `jsonpath` (with expected value when validation is on). INVERT → `not`; EXPECT_NULL → `expectNull`; ISREGEX (default true) with a non-literal or empty value → `regex: true` (Go RE2, full match). |
| `SizeAssertion` | supported | `size` with `op` (`==` `!=` `>` `<` `>=` `<=`). Full response → `expr: "full"` (bytes on the wire), response data → `expr: "body"` (decoded size). Headers, code or message size is unsupported. |
| `MD5HexAssertion` | supported | `hash` with `expr: "md5"`, on the decoded body as kept (up to `defaults.maxBodyBytes`). |
| `DurationAssertion` | supported | Mapped to `max_latency` (e.g. `2s`). |
| `RegexExtractor` (body) | supported | refname, regex and default map across. Template `$N$` becomes `group`; any other template (`$1$-$2$`, `$0$`, literal text) becomes `template`. match_number 0 → `-1` (random), −1 → `"all"` (`name_1..N`, `name_matchNr`, `name_N_gK`), N stays N. |
| `RegexExtractor` with an empty template or on the unescaped body | partial | |
| `RegexExtractor` on headers | supported / partial | The exact patterns Load Meter exports for header and cookie extractors are mapped back to `header`/`cookie`. Other header regexes use `"source": "headers"` (partial: header rendering differs, see above). |
| `RegexExtractor` on the response code | supported | Maps to the `status` extractor when the pattern is `(.*)`, `(.+)`, `(\d+)` or similar; otherwise `"source": "status"`. |
| `RegexExtractor` on URL, message or request headers | supported / partial | `"source": "url"` / `"message"` / `"requestHeaders"` (request headers partial). "Body as a document" is unsupported. |
| `JSONPostProcessor` | supported | `;`-separated names, paths, match numbers and defaults become one extractor each. Match −1 → `"all"`; `name_ALL` (compute_concat) is always written in that mode. Jayway filters/functions make it partial. |
| `BoundaryExtractor` | supported | Mapped to a `boundary` extractor, with the same `source` mapping as the regex extractor. |
| `XPathExtractor` / `XPath2Extractor` | partial | Mapped to `xpath`. The XPath dialect, tolerant parsing and namespaces may differ. |
| "Apply to" (`Sample.scope`) all/children | partial | Applied to the final response only. A `variable` scope is unsupported. |
| Assertion/extractor/header/timer at controller or thread-group level | supported | Copied onto every sampler in scope. The note says how many samplers it was applied to, or "no sampler in scope". |
| `HtmlExtractor` (CSS), `JMESPath*`, `XPath/XPath2/HTML/XML/XMLSchema/Compare/SMIME` assertions | unsupported | |

## Import: non-HTTP, scripts, listeners

| JMeter element | Status | Notes |
|---|---|---|
| `JSR223*`, `BeanShell*`, `BSF*` (sampler, pre, post, assertion, timer, listener) | unsupported | Never executed. The note records the language, script file and an excerpt, and an entry is added to `Manual`. |
| `JavaSampler`, `JDBCSampler`, JMS, FTP, LDAP, TCP, SMTP, OS process, AJP, mail reader, Bolt samplers | unsupported | Load Meter only executes HTTP. |
| `DebugSampler`, `DebugPostProcessor` | ignored | |
| `ResultCollector` (all GUI listeners), `BackendListener`, `ResultSaver`, `Summariser`, `*ResultCollector`, `*Visualizer` | ignored | "UI/listener element; Load Meter reports replace it" |
| `URLRewritingModifier`, `AnchorModifier`, `RegExUserParameters`, `SampleTimeout`, `ResultAction`, JDBC pre/post | unsupported | |
| Any other testclass, including `kg.apc.*` and `com.blazemeter.*` plugins | unsupported | The note names the testclass and whether it comes from a plugin. Children are kept in a disabled group. |

## Import: JMeter functions

| JMeter | Load Meter | Status |
|---|---|---|
| `${var}` | `${var}` | supported |
| `${__UUID()}`, `${__UUID}` | `${__uuid()}` | supported |
| `${__Random(a,b[,var])}` | `${__randomInt(a,b[,var])}` | supported |
| `${__time()}`, `${__time(,)}` | `${__timestamp()}` | supported |
| `${__time(fmt,var)}`, `${__time(/1000,)}`, `${__time(YMD,)}` | `${__timestamp(fmt[,var])}` | supported (full SimpleDateFormat letter set, English names) |
| `${__counter(FALSE,var)}` / `${__counter(TRUE,var)}` | `${__counter([FALSE,var])}` / `${__counter(TRUE[,var])}` | supported (TRUE = per VU) |
| `${__V}`, `__eval`, `__evalVar`, `__isVarDefined`, `__split`, `__intSum`, `__longSum`, `__changeCase`, `__digest`, `__base64Encode`, `__base64Decode`, `__escapeHtml`, `__unescapeHtml`, `__escapeXml`, `__urldecode`, `__unescape`, `__char`, `__timeShift`, `__RandomDate`, `__dateTimeConvert`, `__RandomFromMultipleVars` | same arguments; `__V` → `__v`, `__RandomDate` → `__randomDate`, `__RandomFromMultipleVars` → `__randomFromVars`, others keep their name | supported. A non-English locale argument is partial (ignored). |
| `${__escapeOroRegexpChars(x,)}` | `${__escapeRegex(x,)}` | partial (Go RE2 quoting) |
| `${__threadNum}` | `${__vu()}` | supported |
| `${__RandomString(n,chars)}` | `${__randomString(n)}` | supported when chars is empty or `[a-zA-Z0-9]`; partial otherwise |
| `${__P(name,default)}`, `${__property(name,,default)}` | `${name}` + `Variables[name]=default` | partial (property becomes a variable) |
| `${__urlencode(x)}` | `${__urlencode(x)}` | supported |
| anything else | left as text | unsupported, reported as `function __X` |

## Export (Load Meter → JMeter 5.6)

| Load Meter construct | JMeter output | Status / notes |
|---|---|---|
| Scenario name, description, variables | `TestPlan` + User Defined Variables | supported |
| `Defaults.BaseURL` | HTTP Request Defaults (protocol, domain, port) | supported. A base path is prefixed to relative request paths, because JMeter's default path is not a prefix. |
| `Defaults.Timeout` / request `Timeout` | `response_timeout` (ms) | supported (JMeter treats it as a read timeout) |
| `Defaults.Headers` | Test Plan-level HeaderManager | supported |
| cookies enabled / disabled | CookieManager / none | supported |
| `Defaults.FollowRedirects` | per-sampler follow_redirects | supported |
| `InsecureSkipVerify` | n/a (JMeter does not verify certificates by default) | supported (note) |
| `MaxBodyBytes` | n/a | ignored |
| `DataSources` with a file | CSVDataSet | supported. Tab delimiter becomes `\t`; columns become variableNames, always joined with `,`. |
| `DataSources` with inline `Rows` | CSVDataSet pointing at `<name>.csv` | partial: you must write the companion CSV (added to `Manual`) |
| DataSource mode per-vu / random / unique; onExhaust error; scope vu | shareMode.thread / sequential / no-recycle+stop | partial |
| `constant-vus` | ThreadGroup N threads, scheduler duration | supported |
| `ramping-vus` `[ramp→N]` or `[ramp→N, hold N]` | ThreadGroup ramp_time + duration | supported |
| `ramping-vus` with any other shape | single ramp to the peak over the first stage, total duration | partial |
| `per-vu-iterations` | ThreadGroup loops | supported. A duration cap is not exported (partial). |
| `constant-arrival-rate`, `ramping-arrival-rate` | closed ThreadGroup (maxVUs threads) | partial: the rate is **not** enforced |
| pacing, thinkTimeScale ≠ 1, sub-second durations | n/a | partial. gracefulStop is ignored. |
| `Setup` steps | Once Only Controller at the top of the Thread Group | partial (once per thread, inside the first iteration) |
| `Teardown` steps | tearDown Thread Group (1 thread, 1 loop) | partial |
| request | HTTPSamplerProxy. Query stays in the path; `Form` becomes encoded arguments; `Body` becomes postBodyRaw; `Multipart` becomes arguments + files | supported. Body together with form or multipart is partial (one is dropped). |
| request headers | sampler-level HeaderManager | supported |
| think step before a request | timer inside that sampler's hashTree | supported |
| other think step | Flow Control Action (pause 0) + timer | supported |
| think min==max / min<max | ConstantTimer / UniformRandomTimer | supported |
| `set` step | Flow Control Action + UserParameters (per_iteration=false) | supported |
| transaction | TransactionController (includeTimers=false) | supported |
| loop | LoopController | supported |
| if `${a} == b` / `${a} != b` | IfController `${__jexl3("${a}" == "b")}` | supported |
| if `${a}` (non-empty) | IfController `${__groovy(vars.get("a") != null && vars.get("a") != "")}` | supported |
| other conditions | written as-is (JMeter would evaluate them as JavaScript) | unsupported |
| group / disabled step | GenericController / `enabled="false"` | supported |
| while / foreach / switch / percent / runtime / critical | WhileController / ForeachController / SwitchController / ThroughputController / RunTime / CriticalSectionController | supported. while: partial when the condition is `${x}` (JMeter stops only on the literal `false`) or a comparison (an unresolved variable stops Load Meter, not JMeter); unconvertible conditions are written verbatim (unsupported). A while `duration` is not exported; runtime rounds up to whole seconds. |
| group `select: shuffle` / `roundRobin` / `weighted` with equal weights | RandomOrderController / InterleaveControl (accrossThreads = `shared`) / RandomController | supported. Unequal weights stay partial (Simple Controller). |
| throughput / rendezvous step | Flow Control Action (pause 0) carrying a ConstantThroughputTimer / SyncTimer | supported |
| `workload.newUserEachIteration` / `arrivals: poisson` | same_user_on_next_iteration=false / none | supported / partial |
| extractor regex / jsonpath / boundary | RegexExtractor / JSONPostProcessor / BoundaryExtractor | supported. Regex is written as-is: RE2 and Perl5 syntax differ only in advanced features. |
| extractor xpath | XPath2Extractor | partial |
| extractor header | RegexExtractor on headers `(?im)^Name:\s*([^\r\n]*)` | supported |
| extractor cookie | RegexExtractor on Set-Cookie | partial (this response only, no cookie-jar fallback) |
| extractor status | RegexExtractor on the response code `(.*)` | supported |
| extractor `Required` | n/a | partial |
| match 0 / −1 / `"all"` / N | 1 / 0 (random) / −1 / N | supported. `"all"` on a JSON extractor also sets compute_concat. `maxMatches` is not exported. |
| extractor `template`, `source` | RegexExtractor template; useHeaders `true` / `request_headers` / `URL` / `code` / `message` | supported (headers sources partial) |
| assert status | ResponseAssertion on the response code: Equals, Matches for `Nxx`, OR for lists | supported |
| assert contains / not_contains / regex / equals | ResponseAssertion Substring / NOT Substring / Contains / Equals; `source` → test_field; `message` → custom_message; `not` → NOT bit | supported |
| assert jsonpath / max_latency | JSONPathAssertion (ISREGEX, INVERT, EXPECT_NULL from `regex`, `not`, `expectNull`) / DurationAssertion | supported. A negated max_latency is not exported. |
| assert size / hash | SizeAssertion (`not` inverts the operator) / MD5HexAssertion | supported. Only md5 hashes, without `not`, are exported. |
| request `ignoreStatus` | assume_success on the request's first Response Assertion, or a pass-through Response Assertion | supported |
| assert header | ResponseAssertion Substring on headers "Name: value" | partial |
| SLAs | none | unsupported (JMeter has no built-in pass/fail criteria) |
| Load Meter functions `__uuid`, `__randomInt`, `__timestamp`, `__counter`, `__vu`, `__randomString`, `__urlencode` | `__UUID`, `__Random`, `__time`, `__counter(FALSE,)` / `__counter(TRUE,)`, `__threadNum`, `__RandomString(n,[a-zA-Z0-9])`, `__urlencode` | supported |
| the JMeter-compatible functions listed under import | their JMeter names | supported (`__base64*` partial: needs jmeter-plugins) |
| other Load Meter functions (`__iteration`, `__env`, `__randomFromList`) | left as text | partial |

All text and attribute values are XML-escaped with `encoding/xml.EscapeText`.
Collection entries are named with Java `String.hashCode()`, as JMeter does.

## Model gaps

These JMeter features have no field in `internal/model`. They are mapped to
partial or unsupported items:

1. **Skipping a CSV header while naming columns.** `DataSource` cannot express "ignore the first line, and use these column names".
2. **NOT combined with OR** in a Response Assertion (passes when any one pattern is absent).
3. **Request-level auth and client certificates.** There is nothing for AuthManager (Basic, Digest or Kerberos) or KeystoreConfig.
4. **Predefined cookies and cookie policy.** There are no initial cookies. Per-iteration clearing exists only as `workload.newUserEachIteration`, which also resets auth and connections, so a Cookie Manager's "clear each iteration" is reported, not mapped.
5. **Embedded resource download.** There is no field for `image_parser` or concurrent downloads.
6. **Separate connect and read timeouts.** The model has one `Timeout`.
7. **Startup delay, on-sampler-error action and stepped or loop+duration thread schedules.** `Workload` has no delay, no error policy and no "loops OR duration".
8. **Raw header blocks.** Header sources are rebuilt from Go's parsed headers (sorted, canonical name casing), so a JMeter regex that depends on the server's exact header order or casing can behave differently.
9. **Extraction from a variable ("Apply to: JMeter variable") and body-as-document.**
10. **Infinite Loop Controller (-1)** has no unbounded loop step (While, ForEach, Throughput, Interleave, Random, Random Order, Switch and Runtime controllers are now mapped; see above).
11. **Precise Throughput Timer and plugin shaping timers.** The `throughput` step reproduces only the Constant Throughput Timer.
12. **JMeter properties.** Properties become plain variables.
13. **Scoped data sources.** Data sources are scenario-wide, so a CSV cannot advance on each pass of a loop controller.
