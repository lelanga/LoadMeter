# USER FEEDBACK AND THEMES

Conventions: every finding cites source IDs (see SOURCE_REGISTER.md). **"Quote"** = text returned by the fetch tool from the cited page (reviewer's own words as relayed by the fetch summariser). "(paraphrased)" = our summary. Review-site data is self-selected and small-sample; no Reddit/Stack Overflow text was obtainable (blocked), so nothing here claims Reddit consensus.

---
## Part A. DIRECT USER FEEDBACK (reviewers, forum posters)

### A1. LoadRunner / OpenText Professional Performance Engineering
What experienced users value
- Protocol breadth: "They also support many protocols to perform load testing." (CTO, Marco Technology, 2023-10) [S005]; "the wide range of protocols it supports" (VP, tech vendor, 2024-09) (paraphrased wording from fetch) [S005].
- Record and playback: "the ability to record and play back to produce a script" [S042].
- Correlation: "impressed with the tool's correlation function" (Gary Chen, 2023-06, paraphrased) [S042]; "Paramterization [sic] and correlation are important features." (NYC DoT specialist, 2020-09) [S042]; "Correlation can be done within five to ten minutes in LoadRunner." (Performance Test Engineer, CEI) [S043].
- Scenario design: "a good range of possibilities for creating goal-oriented scenarios" (EPAM, 2024-04) [S005, S042].
- Reporting/analysis and scale: reporting and "front loader" named most valuable (PCS Systemtechnik, 2023-10, paraphrased) [S005]; "quite scalable", "agents in different geographic locations" [S042].
- Legacy and enterprise protocols JMeter cannot cover: "JMeter cannot handle a lot of cases the way LoadRunner can." (Canadian Pacific Railway, re mainframe/VB) [S043]; a VP recommends it for SAP and newer stacks, BlazeMeter for web-only (paraphrased) [S043].
Frustrations
- Cost: "pricing should be more competitive" (VP, 2024-09) [S005]; NeoLoad users call LoadRunner pricier (paraphrased) [S043, S041]; page summary says many find it expensive [S005].
- Resource use / hangs on heavy recordings: "LoadRunner experiences high resource utilization." (EPAM) [S005].
- SPA/JavaScript-heavy apps: complexity "where JavaScript is talking to the server" (Amtrak, 2023) [S005]. TruClient "a bit difficult to use" (Happiest Minds, 2024) [S005].
- Monitoring depends on SiteScope, partial coverage (paraphrased) [S005]. Reporting and GUI "have room for improvement" (PCS, 2023) [S005].
- Specific protocol effort: IBM MQ needs Java users and many libraries (Canadian Pacific Railway, 2025) [S005].
- Support: rated 1/10 by one architect (2023); another says support "needs to be faster" [S005].
- AI: "should be improved and more accessible" (TCS consultant, 2025-05) [S005]. Gartner reviewers: scenario creation harder than alternatives; scheduled tests did not stop on time (n=9 ratings, low weight) [S016].
- Not all users are leaving: some keep LR for some scenarios and JMeter for others; one "continuously looking at other vendors to complement it" [S043].

### A2. Apache JMeter
What users value
- Free and open source: "Because it's open source, there's no licensing cost" (G2, 2026-07) [S015]; "compared to LoadRunner, you save a significant amount of money" (Tecnotree, 2025-09) [S017].
- Reporting/integrations: HTML report "gives me the exact transactions, response times, and graphs" (Capgemini, 2025-06) [S017]; integrates with Grafana/Prometheus (HCL, 2024-11, paraphrased) [S017]; CI/CD automation (paraphrased) [S015].
- Learnable once past ramp-up (paraphrased) [S015]; support/community helpful [S015].
Frustrations
- Learning curve: "Expect a steep learning curve" (G2, 2026-01) [S015].
- Resource-heavy GUI: "the GUI is a resource hog" (G2, 2026-01); "It requires a large amount of system memory" (2022-09) [S015]; "the amount of cache that it uses, the memory that it uses" (Synechron, 2025-12) [S017].
- Dated/unfriendly UI: "it can feel a bit dated compared to many modern testing tools" (G2, 2026-07) [S015]; "The GUI of Apache JMeter is not that user-friendly" (Capgemini) [S017].
- Docs: "Documentation is not comprehensive." (manufacturing test manager, 2024-10) [S017]; "There is no use case guide for handholding" [S015].
- Distributed/load generators too complex, should be simplified (Verizon, 2024-11, paraphrased) [S017].
- Live graphs unusable while running: "unable to view the graph while the test is running" (CEI, 2024-11) [S017].
- Cannot reduce load mid-test without separate instances (paraphrased) [S017]. Limited JS/AJAX support, weak real-time monitoring, hard debugging of complex scripts (G2 summary) [S015].
- Correlation clumsy at scale: JMeter gets "a little clumsy with many request correlations" (consultancy team lead, 2024-03, in a NeoLoad review) [S041].
- Possible ROI doubt: "I have not seen a return on investment right now." (KiwiTech, 2026-04) [S017].

### A3. k6 (Grafana)
- A Ministry of Testing poster on a Grafana-based team: "The easiest way for us is to use k6." and "From observability perspective, k6 is better then JMeter." (2024-11; one team, low weight) [S014].
- Coordinated omission concern originated from JMeter experience: user reported JMeter needed manual calibration of threads, think time and heap, and showed response times "several times higher than real" (paraphrased; 2021 forum question) [S039]. k6 maintainer answer: avoid only via arrival-rate executors; the `rps` option is "somewhat misleading and difficult to use correctly." [S039]
- Gap: no PeerSpot/G2 k6 review text could be retrieved. k6 sentiment evidence is thin.

### A4. Gatling
- Community thread (2025-11): a user comparing with JMeter believed Gatling had scenario time limits; staff clarified that core SDK has none, but gRPC/MQTT/Postman Enterprise-licensed plugins have a 5-minute duration limit without a subscription [S025]. Illustrates confusion about what is free vs paid.

### A5. Locust, Taurus, Artillery
- No direct user feedback retrieved for Locust, Taurus or Artillery beyond project docs. Marked as gap. (Artillery: a forum poster said nobody in the thread had hands-on Artillery experience [S014].)

### A6. NeoLoad (comparison tool)
- Auto-correlation praised: "The scripting and auto-correlation features of Tricentis NeoLoad have helped our organization improve testing efficiency." (Hexaware, 2024-04) [S041]. Cons: smallest licence "very expensive" with no starter package (2025-01, paraphrased); cost for global loads (2026-06, paraphrased) [S041].

### A7. Practitioner/blog accounts of correlation failure (not user reviews; practitioner blogs)
- JMeter correlation failure causes listed by one author: extractor scope too broad, regex on JSON/HTML, silent extraction failure (sentinel value like "TOKEN_NOT_FOUND" propagates), per-user values stored in shared properties, wrong match selected, rotated/expired tokens, manual cookie handling, literal `${var}` unresolved, environment mismatch between GUI and distributed engines, misdiagnosing downstream 404 caused by earlier failed handoff [S047].
- AI correlation: a vendor-affiliated author argues large recordings exceed context, general LLMs produce plausible-but-wrong JSONPath/regex, and "looks correct but sends hardcoded tokens is worse than no test at all" (vendor-biased, n=1 measurement) [S011].
- JMeter AI usage: an AI-plugin author reports agents are good for a single well-understood token correlation, but recommends verifying suggestions, backing up the .jmx, and not pasting secrets (author conflict) [S032]. Readers of the JMeter MCP post reported client freezes after GUI launch and asked whether a free plan works (reader comments, unanswered) [S033].

---
## Part B. OUR TECHNICAL ANALYSIS (inference, not user quotes)

### B1. What experienced LoadRunner users value (synthesis from A1)
Record/replay with auto-correlation suggestions and a replay-compare loop [S030, S042, S043]; scenario/goal modelling, central controller and geographic load generators [S042]; integrated analysis [S005]; protocol breadth [S005]. Phoenix implication: replicate the *workflow* (record, scan, suggest, replay-validate, repeat), not the protocol catalogue.

### B2. What makes JMeter accessible and extensible
GUI test-plan tree, built-in HTTP(S) recorder with templates [S019]; CSV Data Set and extractors as first-class elements [S018]; JSR223/Groovy extensibility [S018]; HTML dashboard from CSV (`-g`/`-e -o`) [S035]; plugin ecosystem and plugin manager [S029, S044]; free [S015, S017]; broad built-in protocols (JDBC, JMS, LDAP, FTP, TCP) [S044]. JMX is a de-facto interchange format (Taurus converts JMX/Swagger to YAML [S028]).

### B3. Tasks requiring excessive manual scripting
- Correlation in JMeter: recorder does not auto-correlate; user uses View Results Tree search and adds extractors by hand [S019]. k6 HAR converter does not correlate: dynamic values converted "as recorded" [S046]. Hence correlation is manual in OSS code-first tools except k6 Studio rules [S023].
- Filtering third-party noise from HAR/recordings (analytics, CDNs) is manual [S046]; JMeter recorder filtering is manual and case-sensitive [S018].
- HAR-to-k6 output is one VU, one iteration: load profile must be added [S046].
- Hard cases (OAuth refresh, rotating tokens) need modelled flows, not regexes [S047].

### B4. Causes of correlation and parameterization failures (analysis from S012, S030, S047)
1. Dynamic values replayed as static (session IDs, CSRF, tokens) [S012].
2. Fragile extractors (regex on structured data) [S047].
3. Silent failure propagation without assertions/visible defaults [S047].
4. Shared state across virtual users (properties vs thread-local) [S047].
5. Ambiguous multi-match selection [S047].
6. Token rotation/expiry during test [S047].
7. Detection is guessy: candidates found by identifier-like strings or by double-recording diff [S012]; LoadRunner adds replay-scan comparing failing responses [S030].
Design implication: detection (deterministic diff of two recordings + record/replay compare) and *proof* (replay shows the value changed and the step now succeeds) are separable from the *decision* (which is a good LLM task); this matches the observe/decide/prove split proposed in [S011] (vendor-biased, but consistent with S012/S030).

### B5. Install, config and distributed-execution pain
- JMeter distributed: identical JMeter and Java versions required; RMI SSL keystore by default since 4.0; data files not sent; reverse connections on high ports through firewalls; hosts-file/hostname failures; each server runs the full plan so load multiplies; client can bottleneck; Times-vs-Threads graph reflects one server only [S013, S035]. Users independently call it too complex [S017].
- k6 OSS distributed requires Kubernetes + k6-operator; ConfigMap 1 MiB limit [S022].
- Locust: one worker process per core (GIL), `--processes` does not work on Windows, `-f -` works for single locustfile only, restart needed for changes [S026].
- LoadRunner: Windows for VuGen/Controller (50-100 GB disk, 8-16 GB RAM), Linux only for load generators [S048].
- Taurus: Python 3.7+, Java for JMeter-based types, pip + build tooling; extra tools downloaded per executor (installation page does not confirm auto-download) [S045].
- JMeter 6.0 will need Java 17+ [S010]; 5.6.x runs on Java 8+ with 17 recommended [S001].

### B6. Reporting gaps
- JMeter dashboard values can differ from Aggregate Report due to a different percentile estimator; sliding window 20,000 samples; many gotchas in granularity and filters [S035]. Live graphs consume resources [S017].
- k6 end-of-test summary: checks totals not usable in thresholds; no HTML report is documented in sources fetched (unverified) [S036].
- Locust had percentile skew bugs fixed in 2.46.2/2.46.6 (None response times; median rank) [S008]. This shows percentile correctness is a recurring bug class.
- LoadRunner reporting: valued by some, "room for improvement" by others [S005].

### B7. Recording, debug and replay likes
LoadRunner: record, replay-scan loop [S030]. k6 Studio: recorder to HAR, rules, single-VU validator showing requests, responses, logs and checks [S023, S049]. JMeter: Validate on thread group + View Results Tree [S019]. These three patterns converge: record -> annotated request/response viewer -> single-iteration validation -> scale.

### B8. Paid licences / bundles
- LoadRunner: free Community licence = 50 Vusers (excluding COM/DCOM, Templates and GUI bundles); a Vuser fails if any protocol in its script lacks a licence; VUFD licences expire after one year if unused [S031].
- Gatling: Community minimal/local only; Enterprise adds distributed, real-time dashboards, CI integrations; gRPC/MQTT/Postman plugins have 5-min limit without subscription [S006, S025, S027].
- k6: OSS core free; AI Script Authoring needs Grafana Assistant in Grafana Cloud (tier not stated) [S024]; Studio is AGPL-3.0 [S049].
- BlazeMeter AI features: "Some features require an Enterprise plan" (secondary) [S034].
- JMeter, Locust, Taurus: open source (JMeter/Locust licence text not re-verified in fetched pages; unverified).

### B9. Commonly requested but poorly implemented (inference; labelled)
- Automatic correlation outside commercial tools (OSS gap: S019, S046; present in LoadRunner S030, NeoLoad S041, k6 Studio rules S023).
- Percentile correctness and open-model load (coordinated omission): JMeter best-practice page warns about it [S018]; k6 requires arrival-rate executors [S038, S039]; JMeter 6.0 changes Open Model Thread Group timing [S010].
- Distributed run UX (S013, S017, S022, S026).
- Legacy JMX migration: k6 "cannot run existing JMX files" [S044]; Gatling and BlazeMeter advertise converters (Gatling's marked experimental, secondary) [S027, S034]. Demand is implied by vendors building converters; no direct user request quoted.

### B10. What AI can reliably do vs what must stay deterministic (analysis)
Evidence base is thin and partly vendor-authored; treat as hypotheses to test.
Reasonable for AI (with human review): explaining responses/errors and results (LoadRunner Aviator Analysis, Gatling AI Analysis, NeoLoad, BlazeMeter, all vendor-stated) [S004, S027, S034]; drafting a script from OpenAPI/prompt then refining (Grafana says OpenAPI output may need refinement) [S024]; classifying which values look like tokens/IDs/usernames (LoadRunner beta flags likely parameters) [S004]; single, well-understood token correlation edits [S032]; suggesting SLOs/thresholds [S027].
Must stay deterministic: recording/parsing HAR; computing percentiles and aggregates [S008, S035]; running the load engine and scheduling; verifying a correlation (replay and compare); enforcing safety allowlists (inference; JMeter BeanShell server shows risk of unauthenticated control ports [S018]); applying edits to test assets with undo/diff (JMeter undo path noted in S032; backup advice S032).
Known AI failure modes cited: large recordings exceeding context, plausible wrong regex/JSONPath, cross-session amnesia [S011]; MCP clients freezing/confirmation prompts [S033]; token cost and data-leak caution [S032, S034].
