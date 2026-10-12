# Formats & APIs (first pass)

Accessed 2026-10-12. Sources are official docs or official source code. Confidence: H = read directly, M = partial/inferred, U = unverified.
Researcher: Formats agent (Haiku tier). The lead engineer condensed this file from the agent's report.

## Sources

| ID | URL | Version stated | Used for |
|---|---|---|---|
| F001 | https://jmeter.apache.org/download_jmeter.cgi | 5.6.3 (Java 8+) | JMeter version |
| F003 | https://jmeter.apache.org/usermanual/component_reference.html | – | HTTP, JSR223, BeanShell |
| F004 | https://github.com/apache/jmeter/blob/master/src/core/src/main/java/org/apache/jmeter/threads/ThreadGroup.java | master | ThreadGroup keys |
| F006 | https://github.com/apache/jmeter/blob/master/src/components/src/main/java/org/apache/jmeter/assertions/ResponseAssertion.java | master | test_field / test_type |
| F007 | https://github.com/apache/jmeter/blob/master/src/protocol/http/src/main/java/org/apache/jmeter/protocol/http/sampler/HTTPSamplerBase.java | master | HTTPSampler.* keys |
| F008 | https://github.com/apache/jmeter/blob/master/src/components/src/main/java/org/apache/jmeter/timers/ConstantThroughputTimer.java | master | throughput, calcMode |
| F010 | https://github.com/apache/jmeter/blob/master/src/core/src/main/java/org/apache/jmeter/save/CSVSaveService.java | master | JTL columns |
| F011 | https://jmeter.apache.org/usermanual/functions.html | – | functions |
| F012 | https://github.com/apache/jmeter/blob/master/bin/testfiles/HTMLParserTestFile_2.jmx | master | JMX structure |
| F018 | https://admhelp.microfocus.com/vugen/en/26.1-26.3/help/WebHelp/Content/VuGen/104400_r_files_generated_during_recording.htm | 26.1-26.3 | VuGen file set |
| F019–F032 | admhelp.microfocus.com VuGen 26.1-26.3 function reference pages (web_url, web_submit_data, web_custom_request, web_add_header, web_add_auto_header, web_reg_save_param_ex/_json/_regexp, web_reg_find, lr_start_transaction, lr_save_string, parameter types, lr_next_row) | 26.1-26.3 | VuGen API |
| F033 | https://admhelp.microfocus.com/lr/en/26.1-26.3/help/WebHelp/Content/License/R_License_Utility.htm | 26.1-26.3 | licensing |
| F034–F036 | https://developers.openai.com/api/reference/overview (Responses, Chat) | current | OpenAI |
| F037–F039 | https://platform.claude.com/docs/en/api/messages, /overview, /versioning | 2023-06-01 | Anthropic |
| F040–F041 | https://ai.google.dev/api/generate-content, https://ai.google.dev/gemini-api/docs/api-key | current | Gemini |
| F042 | https://openrouter.ai/docs/api/reference/overview | current | OpenRouter |
| F043–F044 | https://docs.ollama.com/api/chat, https://github.com/ollama/ollama/blob/main/docs/api.md | current | Ollama |
| F045 | https://docs.github.com/en/github-models/use-github-models/prototyping-with-ai-models | public preview | GitHub Models |
| F046–F051 | https://modelcontextprotocol.io/specification/latest (versioning, stdio, tools, 2025-11-25 lifecycle) | 2026-07-28 | MCP |
| F053–F057 | Claude Code, OpenAI, Gemini CLI, Cursor and VS Code MCP docs | current | client MCP support |

## A. JMeter .jmx (H unless noted)
- Current stable: **5.6.3** (F001).
- Structure: each element is followed by a sibling `<hashTree>` holding its children (F012).
- ThreadGroup: `ThreadGroup.num_threads`, `ramp_time`, `scheduler`, `duration`, `delay`, `main_controller` → `LoopController.loops`, `LoopController.continue_forever` (M), `ThreadGroup.on_sample_error`.
- HTTPSamplerProxy: `HTTPSampler.domain|port|protocol|path|method|follow_redirects|auto_redirects|use_keepalive|DO_MULTIPART_POST|postBodyRaw|connect_timeout|response_timeout`; params in elementProp `HTTPsampler.Arguments` (lowercase "s").
- ResponseAssertion: `Assertion.test_field` ∈ response_data, response_code, response_headers, …; `Assertion.test_type` bitmask MATCH=1, CONTAINS=2, NOT=4, EQUALS=8, SUBSTRING=16, OR=32.
- ConstantTimer `ConstantTimer.delay`; ConstantThroughputTimer `throughput`, `calcMode` 1–5.
- DurationAssertion `DurationAssertion.duration`; JSONPathAssertion `JSON_PATH`, `EXPECTED_VALUE`, `JSONVALIDATION`, `EXPECT_NULL`, `INVERT`, `ISREGEX`.
- Functions: `__UUID`, `__Random`, `__time`, `__counter`, `__P`, `__property`, `__threadNum`, `__CSVRead`, `__RandomString`, `__V` (F011).
- JTL CSV default columns: timeStamp, elapsed, label, responseCode, responseMessage, threadName, dataType, success, failureMessage, bytes, sentBytes, grpThreads, allThreads, URL, Filename, Latency, Encoding, SampleCount, ErrorCount, Hostname, IdleTime, Connect (F010).
- **U:** exact property keys for CSVDataSet (except `shareMode`), Random timers, Regex/JSON/Boundary/XPath2 extractors, Transaction/If/While/OnceOnly controllers, Header/Cookie/Cache managers. The importer uses keys commonly seen in JMeter-saved files and is tested against hand-written fixtures.

## B. LoadRunner / VuGen (web HTTP/HTML)
- The official recording file set (F018) is `.usr`, `.bak`, `default.cfg`, `.asc`, `.grd`, `default.usp`, `init.c`, `run.c`, `end.c`, `vdf.h`, `Data\`. Real projects also commonly use `vuser_init.c`, `Action.c`, `vuser_end.c` and `globals.h`, so the importer accepts both naming sets.
- Function arguments are documented for web_url, web_submit_data, web_custom_request, web_add_header, web_add_auto_header, web_reg_save_param_ex, web_reg_save_param_regexp (Group 0–10, default 1), web_reg_save_param_json (QueryString JSONPath, SelectAll), web_reg_find (Text, Search, Fail, SaveCount), lr_start_transaction, lr_save_string and lr_next_row (F019–F032).
- Parameter types (F031): File, Table, Internal (Date/Time, Group Name, Iteration Number, LG Name, Random Number, Unique Number, Vuser ID), Custom, XML, User-defined function.
- **U:** `.prm` key names (Type, Table, ColumnName, SelectNextRow, UpdateValueOn, OutOfRangePolicy), web_reg_save_param (plain), web_submit_form, lr_think_time, lr_end_transaction.
- Licensing (F033): Controller runs need an OpenText licence. The Community licence allows 50 Vusers. Licences are per Vuser per protocol. Load Meter never needs or bundles the vendor runtime; it only reads script text statically.

## C. AI providers
| Provider | Endpoint | Auth | Notes |
|---|---|---|---|
| Anthropic | `POST https://api.anthropic.com/v1/messages` | `x-api-key` (still supported) or `Authorization: Bearer`; `anthropic-version: 2023-06-01` | tools; `output_config.format` json_schema |
| OpenAI | `POST https://api.openai.com/v1/chat/completions` and `/v1/responses` | Bearer | Chat Completions not stated as deprecated |
| Gemini | `POST https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent` | `x-goog-api-key` | `generationConfig.responseMimeType/responseSchema` |
| OpenRouter | `https://openrouter.ai/api/v1/chat/completions` | Bearer | OpenAI-Chat-like schema |
| Ollama | `POST http://localhost:11434/api/chat` | none (local) | `/v1` OpenAI-compat path **U** |
| GitHub Models | REST, token with `models:read`, public preview | GitHub token | endpoint URL **U** |
| GitHub Copilot | **No documented public API for third-party apps found** | – | not integrated |

## D. MCP
- Latest spec **2026-07-28**: no `initialize` handshake, protocol version sent in `_meta`, servers MUST implement `server/discover`. The legacy flow (2025-11-25 and earlier) uses `initialize` and `notifications/initialized`. Load Meter supports both.
- stdio transport: newline-delimited UTF-8 JSON-RPC. Nothing but MCP messages may go to stdout; logs go to stderr.
- `tools/list` returns {tools, nextCursor}. `tools/call` takes {name, arguments} and returns {content[], isError, structuredContent?}.
- Clients with MCP support: Claude Code / Desktop, ChatGPT (remote servers), Gemini CLI, Cursor, VS Code Copilot Chat (F053–F057).
