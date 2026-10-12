# Test results

All numbers below are actual outputs from the build environment on 2026-10-12 (Linux, 4 vCPU @ 2.1 GHz, 16 GB RAM, Go 1.24.7). Load generator and target ran on the same machine, so throughput figures are lower bounds for a dedicated generator.

## Unit and integration tests
`go test -count=1 ./...`: **23 packages ok, 0 failed, 0 skipped.** 285 top-level tests and 348 subtests passed.

| Package | Coverage |
|---|---|
| vars | 100.0% |
| extract | 99.6% |
| results | 98.8% |
| redact | 97.6% |
| engine | 93.7% |
| metrics | 93.4% |
| app | 92.5% |
| assistant | 90.6% |
| safety | 90.2% |
| ai | 88.2% |
| param | 85.1% |
| jmx | 82.9% |
| loadrunner | 78.8% |
| har | 76.9% |
| server | 75.2% |
| distributed | 73.1% |
| mcp | 73.1% |
| service | 72.3% |
| k6 | 71.6% |
| openapi | 70.1% |
| tools | 58.2% |
| browser | 43.0% |
| cmd/loadmeter | 10.2% (CLI paths are covered by the end-to-end runs below) |

`go vet ./...` and `gofmt -l .` are clean. The `distributed` and `metrics` packages and the 14 QA-tested packages also pass under `-race`.

The QA pass found 10 bugs (rate limiter below 1 rps hung, nested variable tracking, nil-map panic, history path traversal, run-ID collisions within one second, credentials in URLs breaking host checks, "rps" shorthand ignored, token limit floor, case-insensitive untrusted-tag escaping, symlink escape in import paths). All are fixed and covered by tests.

## End-to-end checks
| Check | Result |
|---|---|
| Browser smoke (Playwright Chromium): every page loads, "Run sample test" runs 18 s live, summary renders | 0 console errors, 0 failed HTTP calls; 1,194 requests, 0 errors, 3/3 SLAs pass |
| Record page in the browser: start recording with a launched headless Chromium, mark a transaction, stop and create scenario | Scenario created, navigates to correlation review, 0 errors |
| CLI: HAR import → correlate → run against the mock | 5/5 correlation rules validated (original replay 4 failing requests, with rules 0); 174,632 requests in 8 s, 0 errors |
| HTTPS recording through the local CA with a launched browser (recording agent) | 4 decrypted requests recorded, scenario created |
| Distributed: 2 agent processes + local share, 10 s | 516 requests = 84 + 264 + 168 per node, 0 errors, merged p95 1.0 ms |
| Distributed: agent killed mid-run | Run continued, disconnect recorded as a finding |
| JMeter import of the fixture plan; LoadRunner import of the WebTours-style fixture | Itemised reports; unsupported items listed with manual changes |
| MCP stdio: initialize, tools/list, tools/call; run_test without confirm | 22+ tools listed; run_test refused without confirmation |
| MCP HTTP transport | 401 without token, 403 bad Origin, 200 with token |
| OpenAPI import → validate; k6 export → `node --check` | Valid scenario; generated JS passes syntax check |

## Benchmarks
| Metric | Result |
|---|---|
| Binary size (stripped, linux/amd64) | 11.1 MB (darwin 10.7–11.4 MB, windows 10.5–11.4 MB) |
| Start-up (`version`) | 6 ms median of 5 |
| Idle memory (`serve`) | 11.8 MB RSS |
| Throughput, 50 VUs, GET /health, 15 s | 41,106 req/s, p95 3.5 ms, 0 errors; generator ≈ 1.8 cores, ≤ 19 MB RSS |
| Open model accuracy, target 1,000 req/s for 10 s against a 20 ms endpoint | 997.8 req/s achieved, 0 dropped iterations |
| AI on vs off overhead during a run | None by design: AI is never called in the request path |

## Not verified
- Docker image build (no daemon here); macOS and Windows binaries (cross-compiled, not run).
- Live AI providers (no API keys; only the mock provider was exercised).
- Real MCP clients (Claude Desktop, Cursor, VS Code, Gemini CLI); config files are tested against their documented formats.
- Distributed runs across real machines.
