# ARCHITECTURE: RESEARCH-DRIVEN INPUTS

Inputs only; the engineering lead owns final ADRs. Each input states the evidence, the inference drawn, and open questions. Source IDs: SOURCE_REGISTER.md.

## RI-1. Go single binary rather than a JVM or interpreter-based runtime
- Evidence: JMeter needs a JVM, Java version coupling between client/servers, and JMeter 6.0 raises minimum to Java 17 [S013, S010, S001]; GUI memory complaints [S015, S017]. Locust needs Python/gevent and one process per core [S026, S050]. Taurus needs Python 3.7+, Java, build tools [S045]. LoadRunner authoring is Windows with 50-100 GB disk [S048]. k6 is a Go tool whose distributed story needs Kubernetes [S022] (Go-implementation detail for k6 is general knowledge, not in the fetched pages: unverified).
- Inference: install and runtime friction is a recurring, documented pain; a single static binary directly removes version-matching and dependency problems.
- Open question: performance of a Go engine vs JVM at high VU counts is not evidenced here; benchmark in-house.

## RI-2. Local web UI plus CLI plus MCP, all over one core API
- Evidence: JMeter docs say GUI mode is for debugging only; real runs in CLI [S013, S018]. Locust offers a web UI and headless mode [S050]. Gatling and Grafana expose MCP/AI front-ends [S027, S024]; LoadRunner MCP in private beta [S004].
- Inference: users want a GUI to author/debug and a headless path for load and CI; AI clients need a stable programmatic surface. One core API avoids GUI-only capabilities.

## RI-3. HAR first for capture, with open formats for assets
- Evidence: k6 Studio exports HAR; k6 provides a HAR converter [S023, S046]; Gatling Enterprise lists HAR import [S027]; JMeter has its own proxy recorder but no auto-correlation [S019]. HAR is browser-native and tool-neutral. HAR converters emit unfiltered noise and no correlation, so value is in post-processing [S046].
- Inference: HAR gives a recorder-free entry path (DevTools export) and a standard input for correlation detection. HAR files contain secrets: scrubbing is an architecture requirement.
- Also: scenario files should be human-diffable text (YAML/JSON) rather than an opaque binary or XML dialect, since Taurus users already choose YAML for readability and CI [S028] and JMX is hard to diff (inference).

## RI-4. JMX import is a migration feature with an explicit compatibility report, not a promise of full parity
- Evidence: k6 "cannot run existing JMX files" [S044]; Taurus converts JMX to YAML [S028]; Gatling markets AI converters for LoadRunner/JMeter scripts, marked experimental by a secondary source [S027, S034]. JMeter embeds Groovy/BeanShell and has an unauthenticated BeanShell server risk [S018].
- Inference: full JMX fidelity (JSR223, plugins) is infeasible and unsafe; importing a defined subset and reporting the rest builds trust. Never execute embedded scripts from imported files.

## RI-5. Correlation: deterministic detect + replay-prove, AI as optional ranker/explainer
- Evidence: LoadRunner's three-stage (rules, recording diff, replay scan) is what users praise [S030, S042, S043]; double-recording and single-recording-plus-replay techniques are documented [S012]; JMeter correlation failures are largely scoping, regex fragility, silent failure, shared state, token rotation [S047]; AI limits: context size, plausible-wrong regex, no persistence (vendor-biased) [S011].
- Inference: build candidate generation and verification as engine functions with explicit evidence; give the model small, structured inputs (candidate + surrounding snippets), not whole recordings; require proof by replay before acceptance. Failures must be loud (default value never silently used).

## RI-6. Metrics: one mergeable percentile structure, same code for live and final
- Evidence: JMeter dashboard vs Aggregate Report estimator differences and a 20,000-sample sliding window [S035]; Locust shipped percentile and median fixes in 2.46.2/2.46.6 [S008]; k6 summary metrics defined as Trend metrics [S036]; JMeter distributed Times-vs-Threads shows one server only [S035].
- Inference: choose a mergeable quantile sketch (HDR histogram or similar; choice is engineering's), define the error bound, and test against exact quantiles. Distributed merging must be a design-time concern.

## RI-7. Load model: open-model support and default warnings
- Evidence: coordinated omission described in k6 docs and JMeter best practices [S038, S018]; k6 users must pick arrival-rate executors [S039]; JMeter 6.0 changes Open Model Thread Group timing [S010].
- Inference: scheduler should expose open and closed models with clear semantics and correct timestamp attribution (intended start time).

## RI-8. Distributed mode: agent connects outbound/authenticated, plan pushed with data, no hand-managed keystores or ports
- Evidence: JMeter RMI: same versions, keystore distribution, reverse connections on high ports, data files placed manually, hostname/loopback failures [S013]; users call it complex [S017]; k6 distributed depends on Kubernetes [S022]; Locust needs worker per core, restart for changes [S026]; Gatling gates distributed to paid plans [S006].
- Inference: since distributed is gated or painful everywhere, a simple secure built-in coordinator is a differentiator. Security design needs care (signed plans, mutual auth, allowlist enforced on agents).

## RI-9. Safety allowlist enforced in the engine
- Evidence: JMeter BeanShell server has no security [S018]; JMeter remote testing warns about `AllPermission` shortcut [S013]; AI/agent tooling can run tests and edit plans [S033, S040] (Feather Wand agent mode is off by default [S040]; AI-plugin author advises backups and not pasting keys [S032]).
- Inference: with AI-driven control, the blast radius of a mistaken prompt is real traffic to a real host. The allowlist must be below the AI/MCP layer and not overridable from it.

## RI-10. AI architecture: local-first, pluggable models, deterministic verification
- Evidence: vendor AI is tied to their cloud/platform (Grafana Assistant in Grafana Cloud [S024]; Gatling Enterprise AI [S027]; BlazeMeter Enterprise-tier [S034]; Aviator [S004]); Feather Wand supports Ollama but agent mode is not available with Ollama [S040]; users flag token cost and sensitive data [S032, S034]; one LoadRunner reviewer says AI "should be improved and more accessible" [S005].
- Inference: define a model-provider interface (OpenAI-compatible endpoint covers local servers; verify), redact secrets before prompts, cap context via structured summaries, and record every model-proposed change as a reviewable diff with undo.
- Evidence gap: no measurement of local-model quality on correlation tasks; run an in-house evaluation.

## RI-11. Licensing and distribution
- Evidence: k6 Studio is AGPL-3.0 [S049]; Gatling splits community vs enterprise and restricts duration for some plugins [S025]; LoadRunner protocol bundles [S031]; users prize "no licensing cost" for JMeter [S015, S017].
- Inference: avoid copying AGPL code; avoid feature gating on core run/report/distributed paths if the goal is JMeter accessibility. Licence choice for Load Meter is a business decision outside this research.

## RI-12. Things this research could NOT establish (do not decide on it)
- Real practitioner sentiment from Reddit/Stack Overflow (blocked).
- Go engine performance ceilings, local-model correlation accuracy, GitHub issue frequency by theme.
- Gatling OSS recorder and Locust/Taurus correlation features (unverified).
