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
| `CSVDataSet` | supported | `filename`, `delimiter` (`\t` becomes a tab), and `variableNames` (split on the delimiter) map to `Columns`. `recycle` maps to `OnExhaust=recycle`, and `stopThread` to `stop`. |
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
| `ConstantThroughputTimer` | partial | Not converted. Use an arrival-rate executor instead (this is added to the manual list). |
| `PreciseThroughputTimer`, `SyncTimer`, `kg.apc` timers | unsupported | |
| `TestAction` (Flow Control Action) with pause | supported | Mapped to a think step, plus any timers in scope. |
| `TestAction` stop / restart / break | unsupported | |
| `UserParameters` | supported | A `set` step is emitted before each sampler in scope. Multiple thread columns, or per-iteration with more than one sampler in scope, make it partial. |
| `TransactionController` | supported | Mapped to a transaction step. With includeTimers=true it is partial, because Load Meter leaves think time out of transaction time. |
| `LoopController` (nested) | supported | Loop with `Count`. `-1` becomes an enabled group (partial). A count that isn't a number becomes 1 (partial). |
| `GenericController`, `RecordingController` | supported | Mapped to a group. |
| `CriticalSectionController` | partial | Plain group: Load Meter has no locking across VUs. |
| `IfController` with a simple condition | supported | Accepted forms: `"${x}" == "y"`, `"${x}" != "y"` (single quotes too), and the same wrapped in `${__jexl3(...)}`, `${__jexl2(...)}` or `${__groovy(...)}`. These become `${x} == y`. `${__groovy(vars.get("x") != null && vars.get("x") != "")}` becomes `${x}`. evaluateAll makes it partial. |
| `IfController` with any other condition | unsupported | Children are kept in a disabled group, and the original condition is in the note. |
| `OnceOnlyController` | partial | Children move to `Setup`, which Load Meter runs once per VU before iterations instead of at the controller's position. Inside setUp, tearDown or a disabled container it becomes a plain group. |
| `WhileController`, `ForeachController`, `ThroughputController`, `InterleaveControl`, `RandomController`, `RandomOrderController`, `RunTime`, `SwitchController`, `ModuleController`, `IncludeController`, `TestFragmentController` | unsupported | Children are kept in a disabled group. Each child still gets its own item, noted "inside non-executed container". |

## Import: assertions and extractors

| JMeter element | Status | Mapping / notes |
|---|---|---|
| `ResponseAssertion` on the body, Substring (16) | supported | Mapped to `contains`. |
| …Contains (2) | supported | Mapped to `regex` (find). The pattern must compile as Go RE2; otherwise unsupported (no lookaround or backreferences). |
| …Matches (1) | supported | Mapped to `regex` `^(?:p)$`. |
| …Equals (8) | supported | Mapped to `regex` `^quoted$`, with `${vars}` kept. |
| …NOT (4) with Substring or a literal Contains | supported | Mapped to `not_contains`. |
| …NOT with a regex or Equals | unsupported | The model has no not-regex assertion. |
| …OR (32) | supported | Mapped to one alternation regex. NOT+OR is unsupported. |
| `ResponseAssertion` on the response code | supported | A code gives `status "200"`; `2\d\d`, `2..` or `2[0-9][0-9]` gives `"2xx"`; OR of codes gives `"200,201"`. AND of different codes, or NOT, is unsupported. |
| `ResponseAssertion` on response headers (Substring "Name: value") | partial | Mapped to a `header` assertion (name equals, value contains). JMeter matches the raw header block. |
| `ResponseAssertion` on message, URL, request data or document | unsupported | |
| `Assertion.assume_success` (Ignore Status) | partial | The status override is not modelled. |
| `JSONPathAssertion` | supported | Mapped to `jsonpath` (with expected value when validation is on). A regex expected value, INVERT or EXPECT_NULL is unsupported. |
| `DurationAssertion` | supported | Mapped to `max_latency` (e.g. `2s`). |
| `RegexExtractor` (body) | supported | refname, regex and default map across. Template `$N$` becomes `Group`. match_number 0 becomes random (−1), N stays N, and −1 (all) becomes first (partial). |
| `RegexExtractor` with a combined template, `$0$` plus groups, or the unescaped body | partial | |
| `RegexExtractor` on headers | supported / unsupported | The exact patterns Load Meter exports for header and cookie extractors are recognised and mapped back. Other header regexes are unsupported. |
| `RegexExtractor` on the response code | supported | Maps to the `status` extractor when the pattern is `(.*)`, `(.+)`, `(\d+)` or similar. |
| `RegexExtractor` on URL, message or request headers | unsupported | |
| `JSONPostProcessor` | supported | `;`-separated names, paths, match numbers and defaults become one extractor each. compute_concat or Jayway filters/functions make it partial. |
| `BoundaryExtractor` | supported | Mapped to a `boundary` extractor. Header and other fields are unsupported. |
| `XPathExtractor` / `XPath2Extractor` | partial | Mapped to `xpath`. The XPath dialect, tolerant parsing and namespaces may differ. |
| "Apply to" (`Sample.scope`) all/children | partial | Applied to the final response only. A `variable` scope is unsupported. |
| Assertion/extractor/header/timer at controller or thread-group level | supported | Copied onto every sampler in scope. The note says how many samplers it was applied to, or "no sampler in scope". |
| `HtmlExtractor` (CSS), `JMESPath*`, `Size/XPath/XPath2/HTML/MD5Hex/XML/XMLSchema/Compare/SMIME` assertions | unsupported | |

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
| `${__Random(a,b)}` | `${__randomInt(a,b)}` | supported (partial when a result variable is given) |
| `${__time()}`, `${__time(,)}` | `${__timestamp()}` | supported |
| `${__time(fmt,)}` | `${__timestamp(fmt)}` | partial (only a SimpleDateFormat subset) |
| `${__counter(FALSE,)}` | `${__counter()}` | supported. `TRUE` (per-thread) is partial because the Load Meter counter is global. |
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
| `DataSources` with a file | CSVDataSet | supported. Tab delimiter becomes `\t`; columns become variableNames. |
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
| extractor regex / jsonpath / boundary | RegexExtractor / JSONPostProcessor / BoundaryExtractor | supported. Regex is written as-is: RE2 and Perl5 syntax differ only in advanced features. |
| extractor xpath | XPath2Extractor | partial |
| extractor header | RegexExtractor on headers `(?im)^Name:\s*([^\r\n]*)` | supported |
| extractor cookie | RegexExtractor on Set-Cookie | partial (this response only, no cookie-jar fallback) |
| extractor status | RegexExtractor on the response code `(.*)` | supported |
| extractor `Required` | n/a | partial |
| match 0 / −1 / N | 1 / 0 (random) / N | supported |
| assert status | ResponseAssertion on the response code: Equals, Matches for `Nxx`, OR for lists | supported |
| assert contains / not_contains / regex | ResponseAssertion Substring / NOT Substring / Contains | supported |
| assert jsonpath / max_latency | JSONPathAssertion (ISREGEX=false) / DurationAssertion | supported |
| assert header | ResponseAssertion Substring on headers "Name: value" | partial |
| SLAs | none | unsupported (JMeter has no built-in pass/fail criteria) |
| Load Meter functions `__uuid`, `__randomInt`, `__timestamp`, `__counter`, `__vu`, `__randomString`, `__urlencode` | `__UUID`, `__Random`, `__time`, `__counter(FALSE,)`, `__threadNum`, `__RandomString(n,[a-zA-Z0-9])`, `__urlencode` | supported |
| other Load Meter functions (`__iteration`, `__env`) | left as text | partial |

All text and attribute values are XML-escaped with `encoding/xml.EscapeText`.
Collection entries are named with Java `String.hashCode()`, as JMeter does.

## Model gaps

These JMeter features have no field in `internal/model`. They are mapped to
partial or unsupported items:

1. **Skipping a CSV header while naming columns.** `DataSource` cannot express "ignore the first line, and use these column names".
2. **Inverted assertions.** There is no negated regex, negated equals or negated status assertion, and no "Ignore Status" flag on a request.
3. **Request-level auth and client certificates.** There is nothing for AuthManager (Basic, Digest or Kerberos) or KeystoreConfig.
4. **Predefined cookies and cookie policy.** There are no initial cookies and no per-iteration cookie clearing.
5. **Embedded resource download.** There is no field for `image_parser` or concurrent downloads.
6. **Separate connect and read timeouts.** The model has one `Timeout`.
7. **Startup delay, on-sampler-error action and stepped or loop+duration thread schedules.** `Workload` has no delay, no error policy and no "loops OR duration".
8. **"All matches" extraction (match −1)** and template concatenation (`$1$-$2$`). An extractor yields one value from one group.
9. **Extractors on the raw header block, URL or response message.** Regex extractors read the body only.
10. **Loops of unknown length** (While, ForEach, infinite Loop) and the Throughput, Interleave, Random, Switch and Runtime controllers. The `Step` types are only request, transaction, think, loop with a count, if, group and set.
11. **Throughput shaping inside a closed workload.** There is no per-sampler throughput timer.
12. **Per-thread counters and JMeter properties.** `__counter` is global, and properties become plain variables.
13. **Scoped data sources.** Data sources are scenario-wide, so a CSV cannot advance on each pass of a loop controller.
