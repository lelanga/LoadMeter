# loadrunner: VuGen Web (HTTP/HTML) script importer

This package converts a LoadRunner VuGen **Web - HTTP/HTML** script into a
Phoenix scenario (`model.Scenario`) and a `model.CompatReport`. It works by
**static analysis of the script text only**:

* it never compiles or runs the C code in a script;
* it never needs, loads or bundles any OpenText / Micro Focus runtime, library
  or header (`lrun.h`, `web_api.h` and so on are only recognized by name);
* the test fixtures in `testdata/loadrunner/` are original and hand-written,
  based on the function syntax in the public documentation listed at the end.

## API

| Function | Purpose |
|---|---|
| `ImportDir(dir) (*model.Scenario, *model.CompatReport, error)` | Imports the script folder (the folder that holds the `.usr`). It reads only top-level regular files: subfolders such as `data/` and symlinks are skipped. |
| `ImportFiles(files map[string][]byte, name)` | Does the same for uploads. Path components in keys are ignored and file names match case-insensitively. |
| `Analyze(files, name) (*Analysis, error)` / `AnalyzeDir(dir)` | Standalone portability assessment, which also works for non-web scripts. |
| `ErrUnsupportedProtocol` | Returned (wrapped) when the `.usr` declares no web protocol. The scenario is `nil`, but the report is still returned. |
| `ErrNoScript` | Returned when the input has no `.usr` and no `.c` files. |

Nothing is dropped silently. Every construct shows up in the report as
`supported`, `partial`, `ignored` or `unsupported`, with a `file:line`
location wherever one exists. Recognized but meaningless attributes (for
example `Snapshot=`) are reported once per attribute type.

## Script files

| File | Handling |
|---|---|
| `*.usr` | INI file. The protocol comes from `[General] Type / AdditionalTypes / ActiveTypes`. `[Actions]` gives the order (init first, then the actions in listed order, then end). `ParameterFile=` is honoured, and so are `ParamLeftBrace` / `ParamRightBrace`. |
| `vuser_init.c` or `init.c` | Becomes `Setup`. |
| `Action*.c`, `run.c`, or any action listed in `[Actions]` | Each action becomes one `group` step in `Steps`. |
| `vuser_end.c` or `end.c` | Becomes `Teardown`. |
| `globals.h`, `vdf.h`, other `.h` files | Preprocessor lines are ignored. Global declarations are reported as `partial`, and function bodies as `unsupported`. |
| `*.prm` + `*.dat` | Parameters and data sources (see below). |
| `default.cfg`, `default.usp` | Run-time settings (see below). |
| `.c` files not referenced by the `.usr` | Reported as `unsupported` and not imported. |

Without a `.usr`, conventional names are used: `vuser_init.c` or `init.c`,
then `Action*.c` (natural order) or `run.c`, then `vuser_end.c` or `end.c`.
A warning is added when this happens.

### Protocols

* **Supported:** `QTWeb`, `Web (HTTP/HTML)` and their spelling variants.
* **Partial (web-based):** Web Services, SAP Web, Siebel Web, Flex and
  WebSocket. Their `web_*` calls are imported, but their own APIs are not.
* **Any other protocol:** reported `unsupported`, the import stops with
  `ErrUnsupportedProtocol`, and `Analyze` returns `not-portable`. Examples are
  Citrix ICA, SAP GUI, Windows Sockets, ODBC, Oracle NCA, TruClient, RDP,
  DevWeb and Java.

In a multi-protocol script that also declares web, the non-web protocol is
reported as `partial`, and calls to its APIs (`lrs_*`, `ctrx_*` and so on)
are reported `unsupported` at their `file:line`.

## Supported subset

### Requests and registrations

| VuGen construct | Phoenix IR | Status |
|---|---|---|
| `web_url` (`URL=`, `Referer=`) | `GET` request; `Referer` becomes a header. Step name = first argument. | supported |
| `Resource=`, `RecContentType=`, `Snapshot=`, `Mode=`, `TargetFrame=`, `EncodeAtSign=` | none | ignored (reported once per attribute) |
| `EXTRARES ... ENDITEM` | Resources are not replayed. | partial |
| `web_submit_data` (`Action=`, `Method=` defaulting to POST, `ITEMDATA "Name=","Value=",ENDITEM`) | `form`. With `EncType=multipart/form-data` it becomes `multipart`, and `File=Yes` maps to `Part.File`. With `Method=GET` the fields go into the URL query string. | supported (file uploads partial) |
| `web_custom_request` (`URL`, `Method` defaulting to GET, `Body`, `EncType`) | `body`; `EncType` becomes a `Content-Type` header. Adjacent C string literals are concatenated. | supported |
| `BodyBinary=` | `\xNN` escapes are decoded into bytes. | supported (partial if the result is not UTF-8) |
| `BodyFilePath=`, `BodyUnicode=` | Recorded in the report as a manual change. | partial |
| `web_submit_form` | A **disabled** POST to the previous URL with the listed fields. The real form action and hidden fields must be added by hand. | partial |
| `web_add_header` | Header on the next request only. | supported |
| `web_add_auto_header`, `web_remove_auto_header`, `web_revert_auto_header`, `web_cleanup_auto_headers` | Headers on every following request, in script order. | supported |
| `web_reg_save_param` (`LB`, `RB`, `Ord`, `NotFound`, `Search`) | `boundary` extractor on the next request. `NotFound=warning` sets `required=false`. | supported |
| `Ord=ALL` | Only the first match is saved, as `Name_1`. References to `{Name_N}` and `{Name_count}` are rewritten, but `_count` is never filled. | partial |
| `LB/IC`, `SaveLen`, `SaveOffset`, `Convert`, `Search=Headers` | none | partial |
| `web_reg_save_param_ex` (`ParamName`, `LB`, `RB`, `Ordinal` with `n`/`ALL`/`RANDOM`, `SEARCH_FILTERS Scope=`) | `boundary` extractor. `Scope=Headers` with a single `HeaderNames=` and no boundaries becomes a `header` extractor; other header scopes are partial. | supported |
| `web_reg_save_param_json` (`QueryString=`) | `jsonpath` extractor | supported (`SelectAll=Yes` partial) |
| `web_reg_save_param_regexp` (`RegExp=`, `Group=`, `Ordinal=`) | `regex` extractor. The expression is checked against Go RE2 syntax. | supported |
| `web_reg_save_param_xpath` (`QueryString=`) | `xpath` extractor | partial (XPath engines differ) |
| `web_reg_find` `Text=` (`Fail=NotFound`, which is the default) | `contains` assertion on the **next** request | supported |
| `web_reg_find` `Fail=Found` | `not_contains` | supported |
| `Text/IC=` | `regex` with `(?i)` | supported |
| `TextPfx=` + `TextSfx=` | `regex` | partial |
| `SaveCount=` | Without `Fail=`, no assertion is added, because LoadRunner does not fail the step in that case. | partial |

Registration functions (`web_reg_*`) attach to the next action function, even
across file boundaries. Any left pending at the end of the script produce a
warning.

### Flow, timing and variables

| VuGen construct | Phoenix IR | Status |
|---|---|---|
| `lr_start_transaction` / `lr_end_transaction` | `transaction` step wrapping the enclosed steps | supported |
| Unbalanced transactions, `LR_FAIL` / `LR_STOP` status | Closed automatically, with a warning. | partial |
| `lr_start_sub_transaction` | Nested transaction | partial |
| `lr_think_time(n)` | `think` step, adjusted by the think-time run-time settings. | supported |
| `lr_save_string("v","n")`, `lr_save_int`, `web_save_timestamp_param` | `set` step | supported |
| `lr_eval_string("...")` used as an argument | Parameter substitution | supported |
| `{Name}` inside any string | `${Name}`, but only for names declared in the `.prm` or registered by the script. Other identifier-like braces are left as they are and warned about once. JSON braces are never touched. | supported |
| `web_set_timeout(STEP/RECEIVE, s)` | `defaults.timeout` (largest value wins) | supported |
| `web_set_timeout(CONNECT, s)` | `defaults.timeout` | partial |
| `web_set_user("u","p",host)` with literal values | Preemptive `Authorization: Basic` default header | partial |
| `web_set_user` with parameters | none | unsupported |
| `web_set_cookie` | Recorded as a manual change. | partial |
| `web_cleanup_cookies` | Ignored in `vuser_init`, because the cookie jar starts empty. | ignored or partial |
| `web_set_sockets_option` | none | partial or ignored |
| `web_set_max_html_param_len`, `web_cache_cleanup`, `lr_output_message`, `lr_log_message`, `lr_error_message` and similar | none | ignored |
| `for (i = A; i < B; i++) { ... }` with literal bounds, where the body does not use `i` | `loop` step with `count` | partial |
| `return 0;` | none | silent |
| Non-zero `return` | none | partial |
| `rc = web_url(...)` | The call is imported; the assigned value is lost. | partial |
| Other C control flow (`if`, `while`, `do`, `switch`, other `for` loops), declarations, expressions, labels | Not imported. The report gives the `file:line`, an excerpt, and the number of HTTP calls lost inside. | unsupported |
| Custom C functions and calls to them; C library calls (`strcpy`, `sprintf`, ...) | none | unsupported |
| Any other `lr_*` / `web_*` call (for example `web_link`, `web_global_verification`, `lr_param_sprintf`, `web_convert_param`, `lr_rendezvous`) | none | unsupported |
| Non-web protocol APIs (`ctrx_`, `sapgui_`, `lrs_`, `lrd_`, `nca_`, `tc_`, `rdp_`, `jms_`, `web_service_call`, ...) | none | unsupported (needs the licensed runtime) |

### Parameters (`.prm`) and run-time settings

> **Based on commonly observed files, not confirmed against official docs.**
> The `.prm` key names, and the `default.cfg` / `default.usp` keys below, come
> from files VuGen commonly generates. They could not be confirmed in reachable
> official documentation. The parser is tolerant: it compares keys
> case-insensitively and ignores spaces and underscores in values, so
> `Each iteration` and `EachIteration` are the same. Anything it does not
> understand is reported.

**Table parameters.** A parameter declared as `[parameter:Name]` with
`Type="Table"` (or `File`), a `Table="x.dat"` and a `ColumnName` (either
`"Col N"` or a header name) becomes a `DataSource`.

* Parameters that share one `.dat` file share one data source.
* The `.dat` file is read with its first row as the header. Rows are **inlined**
  and the columns are renamed to the parameter names.
* The data source is marked `sensitive` when a column name suggests a
  credential.

`Delimiter` accepts a literal character or `comma`, `tab`, `space` or
`semicolon`. The other table settings map as follows:

| `.prm` setting | Data source setting | Status |
|---|---|---|
| `SelectNextRow="Sequential"` | `sequential` | partial: LoadRunner gives each Vuser its own cursor, while Phoenix shares one |
| `SelectNextRow="Random"` | `random` | supported |
| `SelectNextRow="Unique"` | `unique` | supported |
| `SelectNextRow="Same line as X"` | Same row as X | supported |
| `UpdateValueOn` / `GenerateNewVal` = `Each iteration` | `scope: iteration` | supported |
| `UpdateValueOn` / `GenerateNewVal` = `Once` | `scope: vu`; combined with `Unique` it becomes `per-vu` | supported |
| `UpdateValueOn` / `GenerateNewVal` = `Each occurrence` | none | partial |
| `OutOfRangePolicy` containing `Abort` | `onExhaust: stop` | supported |
| `OutOfRangePolicy` containing `Cyclic` | `recycle` | supported |
| `OutOfRangePolicy` = continue with last value | `recycle` | partial |

**Other parameter types:**

| Type | Phoenix mapping | Status |
|---|---|---|
| Date/Time (`Format=` strftime) | `${__timestamp(fmt)}`. Only `%Y %m %d %H %M %S` convert; other formats fall back to epoch milliseconds. | supported, or partial for unconvertible formats |
| Random Number (`MinValue` / `MaxValue`) | `${__randomInt(a,b)}` | supported |
| Unique Number | `${__counter()}` (Start and Block are lost) | partial |
| Iteration Number | `${__iteration()}` | supported |
| VuserID | `${__vu()}` | supported |
| Group Name, Host, Load Generator | Constant variable (`OriginalValue`) | partial |
| User-defined function, XML, database and others | Empty variable plus a manual change | unsupported |

**Run-time settings:**

| Setting | Phoenix mapping | Status |
|---|---|---|
| `default.usp [RunLogicRunRoot] RunLogicNumOfIterations`, or `default.cfg [Iterations] NumOfIterations` | Executor `per-vu-iterations` with `vus=1`. The VU count belongs to the Controller scenario (`.lrs`), which is not part of a script. | supported |
| `RunLogicPaceType="Const"` + `RunLogicPaceConstTime` | `workload.pacing` | supported |
| `RunLogicPaceType="Random"` (`RunLogicRandomPaceMin` / `Max`) | Pacing at the lower bound | partial |
| `RunLogicPaceType="ConstAfter"` / `"RandomAfter"` | A trailing `think` step | partial |
| `RunLogicRunMode` other than sequential; nested run-logic blocks | Flattened | partial |
| `default.cfg [ThinkTime] Options=NOTHINK` / `RECORDED` / `MULTIPLY` (`Factor`) | `thinkTimeScale` 0, 1 or the factor | supported |
| `[ThinkTime] Options=RANDOM` (`ThinkTimeRandomLow` / `High` percentages) | Each think step gets a `min` / `max` range. | supported |
| `[ThinkTime] LimitFlag=1` + `Limit` | Think times are clamped at import. | supported |
| Other `default.cfg` sections (log, browser, proxy, speed simulation, ...) | none | ignored, reported per section |

## Portability assessment (`Analyze`)

`Analyze` returns an `Analysis` covering:

* the declared protocols;
* per-file findings: role, defined C functions, `lr_*` / `web_*` / other
  protocol calls counted by name (including calls nested inside expressions
  and control flow), custom C code findings (`file:line` and excerpt),
  includes, correlation rules, transactions and parameter references;
* aggregate runtime-function counts, protocol API counts, external files
  (`.dat`, `BodyFilePath`, uploads, non-standard includes), parameters and
  their mapping, request count, compatibility counts and confidence.

The verdict is one of:

* **not-portable:** the protocol is not web, no HTTP request could be
  imported, or non-web protocol calls outnumber the HTTP requests.
* **portable-with-changes:** at least one element is `unsupported` or
  `partial`.
* **portable:** everything is `supported` or `ignored`.

## What requires the licensed LoadRunner runtime

The following cannot be reproduced without the licensed runtime, so they
always show up in the report:

* **Non-web protocols** (Citrix, SAP GUI, Windows Sockets, database,
  TruClient/browser, RDP, Java, .NET, mobile and so on), whose replay engines
  are proprietary.
* **Arbitrary C logic** in action files or `globals.h`, and the C runtime it
  links against. Phoenix only models request, transaction, think, set, loop,
  if and group steps.
* **HTML-mode browser emulation:** automatic download of embedded resources
  (`Mode=HTML`, `EXTRARES`), `web_submit_form` / `web_link` / `web_image`,
  which navigate using the previous page's DOM, and the browser cache.
* **Controller-side settings:** the Vuser count, schedule, rendezvous points,
  IP spoofing, network speed simulation and goals live in the `.lrs` scenario
  and the Controller, not in the script.
* **Runtime parameter features:** user-defined function parameters, XML and
  database parameters, and per-Vuser block allocation for Unique Number.

## Limitations

* The parser is a tokenizer plus a statement recognizer, not a C compiler.
  * It does not expand macros, so `#define`'d attribute strings are unknown.
  * It does not evaluate expressions: an argument built from `sprintf` or
    variables makes the whole call `unsupported`.
  * It handles comments, escapes (`\n`, `\x`, octal, line continuations),
    adjacent-literal concatenation, wide-string prefixes and nested calls.
  * It recovers from unbalanced braces and unterminated literals. The fuzz
    target `FuzzParseC` covers this.
* Auto headers are applied in linear script order. Headers that a later
  action adds do not reach requests earlier in the next iteration, which
  LoadRunner would send them on.
* Only one `.prm` is read per script.
* `.dat` rows are inlined into the scenario, up to 1,000,000 rows.
* `.dat` files are decoded as UTF-8; legacy ANSI (Windows-1252) data is not
  transcoded.
* Run-time settings keys that VuGen versions name differently are not
  guaranteed to be recognized. Anything unrecognized is reported, never
  applied silently.

## Public documentation relied on

<!--
Public documentation consulted for function syntax and file roles. Only
publicly documented syntax was used; no proprietary code was copied, and the
fixtures are original and hand-written.

- VuGen Function Reference (index):
  https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/FuncRef.htm
- web_reg_save_param_ex:
  https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFr_web_reg_save_param_ex.htm
- web_reg_find:
  https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFr_web_reg_find.htm
- web_submit_data:
  https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFr_web_submit_data.htm
- web_submit_form:
  https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFR_web_submit_form.htm
- Other web_* / lr_* pages, same tree, pattern .../FuncRef/web/lrFr_<function>.htm
  (web_url, web_custom_request, web_add_header, web_add_auto_header,
  web_reg_save_param, web_reg_save_param_json, web_reg_save_param_regexp,
  web_reg_save_param_xpath, web_set_timeout, web_set_user) and
  .../FuncRef/lr/... for lr_start_transaction, lr_think_time, lr_save_string,
  lr_eval_string.
- Files generated during recording (lists .usr, default.cfg, default.usp,
  init.c / run.c / end.c, vdf.h):
  https://admhelp.microfocus.com/vugen/en/26.1-26.3/help/WebHelp/Content/VuGen/104400_r_files_generated_during_recording.htm
- Parameter Properties dialog box (select next row / update value on / out of range):
  https://admhelp.microfocus.com/vugen/en/26.1/help/WebHelp/Content/VuGen/UI/116100_ui_parameters_property_db.htm
- Data assignment methods for parameters:
  https://admhelp.microfocus.com/vugen/en/26.1/help/WebHelp/Content/VuGen/114350_data_assignment_methods_for_parameters.htm
- Configure runtime settings manually (pacing, think time, iterations):
  https://admhelp.microfocus.com/vugen/en/26.1/help/WebHelp/Content/VuGen/166950_t_how_to_confugre_rts_manually.htm
-->

These are the same links as in the comment block above.

* [VuGen Function Reference (index)](https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/FuncRef.htm)
* [web_reg_save_param_ex](https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFr_web_reg_save_param_ex.htm)
* [web_reg_find](https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFr_web_reg_find.htm)
* [web_submit_data](https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFr_web_submit_data.htm)
* [web_submit_form](https://admhelp.microfocus.com/vugen/en/26.1/help/function_reference/Content/FuncRef/web/lrFR_web_submit_form.htm)
* [Files generated during recording](https://admhelp.microfocus.com/vugen/en/26.1-26.3/help/WebHelp/Content/VuGen/104400_r_files_generated_during_recording.htm)
* [Parameter Properties dialog box](https://admhelp.microfocus.com/vugen/en/26.1/help/WebHelp/Content/VuGen/UI/116100_ui_parameters_property_db.htm)
* [Data assignment methods for parameters](https://admhelp.microfocus.com/vugen/en/26.1/help/WebHelp/Content/VuGen/114350_data_assignment_methods_for_parameters.htm)
* [Configure runtime settings manually](https://admhelp.microfocus.com/vugen/en/26.1/help/WebHelp/Content/VuGen/166950_t_how_to_confugre_rts_manually.htm)
