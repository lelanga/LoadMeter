// Command loadmeter is the Load Meter CLI: run tests, import/convert
// artifacts, serve the web UI and expose an MCP server for AI assistants.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/examples"
	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/mockserver"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/report"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/safety"
)

type command struct {
	summary string
	run     func(args []string) error
}

var commands = map[string]command{}

func register(name, summary string, fn func([]string) error) {
	commands[name] = command{summary, fn}
}

func init() {
	register("version", "Print version information", func([]string) error {
		fmt.Printf("loadmeter %s (%s %s/%s)\n", results.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	})
	register("run", "Run a scenario: loadmeter run scenario.json [--vus N --duration 1m --rate R --allow-host H]", cmdRun)
	register("validate", "Validate a scenario file without running it", cmdValidate)
	register("demo", "Start the bundled mock server and run the sample scenario (one-click sample)", cmdDemo)
	register("mock", "Run the deterministic mock target server: loadmeter mock [--addr 127.0.0.1:8099]", cmdMock)
	register("report", "Re-render the HTML report for a run: loadmeter report <run-id> [--baseline <run-id>]", cmdReport)
	register("compare", "Compare two runs: loadmeter compare <baseline-run-id> <run-id>", cmdCompare)
	register("runs", "List runs in the workspace", cmdRuns)
	register("doctor", "Check the installation and environment", cmdDoctor)
	register("init", "Create a workspace with the sample scenario", cmdInit)
}

func usage() {
	fmt.Fprintf(os.Stderr, "Load Meter %s — lightweight load testing with optional AI assistance\n\nUsage: loadmeter <command> [flags]\n\nCommands:\n", results.Version)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", n, commands[n].summary)
	}
	fmt.Fprintf(os.Stderr, "\nGlobal: --workspace DIR (default ./loadmeter-workspace or $LOADMETER_WORKSPACE)\nRun 'loadmeter <command> -h' for command flags.\n")
}

// guiMode is set to "1" at link time for the windowless Windows build
// (-ldflags "-H=windowsgui -X main.guiMode=1"): no console, errors in a message box.
var guiMode string

func main() {
	// Double-clicked on Windows (no arguments): open the app window and keep the
	// console open on errors, instead of printing usage and closing at once.
	doubleClicked := runtime.GOOS == "windows" && len(os.Args) < 2
	if doubleClicked {
		if os.Getenv("LOADMETER_WORKSPACE") == "" {
			if home, err := os.UserHomeDir(); err == nil {
				_ = os.Setenv("LOADMETER_WORKSPACE", filepath.Join(home, "LoadMeter-Workspace"))
			}
		}
		os.Args = append(os.Args, "serve", "--app")
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		if len(os.Args) < 2 {
			os.Exit(2)
		}
		return
	}
	c, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err := c.run(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec)) // outcome codes (SLA failed, regression) were already reported
		}
		if guiMode == "1" {
			showError("Load Meter", err.Error())
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		if doubleClicked {
			fmt.Fprintln(os.Stderr, "\nPress Enter to close this window.")
			_, _ = fmt.Scanln()
		}
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit code %d", int(e)) }

func defaultWorkspace() string {
	if d := os.Getenv("LOADMETER_WORKSPACE"); d != "" {
		return d
	}
	return "loadmeter-workspace"
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, strings.Split(v, ",")...); return nil }

// ---------- run ----------

type runFlags struct {
	workspace, baseline, duration, rampUp string
	vus, maxVUs                           int
	rate                                  float64
	allow                                 stringList
	jsonOut, quiet, yes                   bool
	set                                   stringList
	dist                                  distFlags // distributed.go
	stat                                  statFlags // gates.go
}

func addRunFlags(fs *flag.FlagSet, f *runFlags) {
	fs.StringVar(&f.workspace, "workspace", defaultWorkspace(), "workspace directory for runs")
	fs.StringVar(&f.baseline, "baseline", "", "run ID to compare against")
	fs.IntVar(&f.vus, "vus", 0, "override: virtual users (concurrency)")
	fs.StringVar(&f.duration, "duration", "", "override: test duration, e.g. 30s, 5m")
	fs.StringVar(&f.rampUp, "ramp-up", "", "override: ramp-up time (with --vus)")
	fs.Float64Var(&f.rate, "rate", 0, "override: open-model arrival rate (iterations/s) instead of VUs")
	fs.IntVar(&f.maxVUs, "max-vus", 0, "VU pool size for --rate")
	fs.Var(&f.allow, "allow-host", "allow a non-loopback target host (repeatable; only hosts you are authorized to test)")
	fs.BoolVar(&f.jsonOut, "json", false, "print the summary as JSON")
	fs.BoolVar(&f.quiet, "quiet", false, "no live progress output")
	fs.BoolVar(&f.yes, "yes", false, "confirm high-impact tests against non-loopback hosts")
	fs.Var(&f.set, "set", "set a scenario variable, e.g. --set baseUrl=http://127.0.0.1:8080 (repeatable)")
	addDistFlags(fs, &f.dist)
	addStatFlags(fs, &f.stat)
}

// applyOverrides applies --set variables and workload flags.
func applyOverrides(sc *model.Scenario, f *runFlags) error {
	for _, kv := range f.set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--set expects name=value, got %q", kv)
		}
		if sc.Variables == nil {
			sc.Variables = map[string]string{}
		}
		sc.Variables[k] = v
	}
	return app.ApplyOverrides(sc, app.Overrides{VUs: f.vus, Duration: f.duration, RampUp: f.rampUp, Rate: f.rate, MaxVUs: f.maxVUs})
}

func targetHosts(sc *model.Scenario) []string { return app.TargetHosts(sc) }

func checkTargets(sc *model.Scenario, pol safety.Policy, yes bool) error {
	remote, err := app.CheckTargets(sc, pol, yes)
	if err != nil {
		if _, ok := err.(*app.ErrNeedsConfirmation); ok {
			return fmt.Errorf("%v (pass --yes)", err)
		}
		return err
	}
	if remote {
		fmt.Fprintln(os.Stderr, "WARNING: this test targets a non-local system. Only load test systems you own or are authorized to test, and avoid production unless explicitly approved.")
	}
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var f runFlags
	addRunFlags(fs, &f)
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter run scenario.json [flags]")
	}
	path := fs.Arg(0)
	sc, err := app.LoadScenario(path)
	if err != nil {
		return err
	}
	return runScenario(sc, filepath.Dir(path), &f)
}

func runScenario(sc *model.Scenario, baseDir string, f *runFlags) error {
	if err := applyOverrides(sc, f); err != nil {
		return err
	}
	pol, err := safety.LoadPolicy()
	if err != nil {
		return err
	}
	pol.AllowedHosts = append(pol.AllowedHosts, f.allow...)
	if len(f.allow) > 0 {
		safety.Audit("policy.allow_host_cli", map[string]any{"hosts": []string(f.allow)})
	}
	if err := checkTargets(sc, pol, f.yes); err != nil {
		return err
	}
	if f.dist.enabled() {
		return runDistributed(sc, baseDir, f, pol)
	}
	ws := app.Workspace{Dir: f.workspace}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	bw := newBreachWatch(sc, f.stat)
	a, err := ws.StartRun(ctx, sc, app.RunOptions{BaseDir: baseDir, Policy: pol, Baseline: f.baseline, Logger: logger,
		Compare: f.stat.cmp.options("", ""), Finalize: bw.finalizer()})
	if err != nil {
		return err
	}
	bw.start(a, f.quiet || f.jsonOut)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		for range sig {
			fmt.Fprintln(os.Stderr, "\nstopping gracefully (Ctrl+C again to abort in-flight requests)...")
			a.Run.Stop()
		}
	}()
	if !f.quiet && !f.jsonOut {
		fmt.Fprintf(os.Stderr, "run %s: %s, planned %s, up to %d VUs\n", a.ID, sc.Workload.Executor, sc.Workload.PlannedDuration(), sc.Workload.MaxPlannedVUs())
		t := time.NewTicker(2 * time.Second)
	loop:
		for {
			select {
			case <-a.Run.Done():
				break loop
			case now := <-t.C:
				s := a.Run.Col.Snapshot(now, 1<<30)
				fmt.Fprintf(os.Stderr, "  t=%5.0fs vus=%-4d reqs=%-7d rps=%-8.1f p95=%7.1fms err=%.2f%%\n",
					s.ElapsedSec, s.ActiveVUs, s.Requests.Count, s.Requests.RPS, s.Requests.P95Ms, s.Requests.ErrorRate)
			}
		}
		t.Stop()
	}
	sum, err := a.Wait()
	if err != nil {
		return err
	}
	if f.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sum)
	}
	printSummary(sum, a.Dir)
	if len(sum.SLAs) > 0 && !sum.SLAPassed || bw.Reason() != "" {
		return exitCode(3)
	}
	return nil
}

func printSummary(s *results.Summary, dir string) {
	r := s.Snapshot.Requests
	fmt.Printf("\n%s — %s in %.1fs\n", s.Scenario, s.Status, s.DurationS)
	fmt.Printf("requests %d  errors %d (%.2f%%)  rps %.1f  iterations %d  dropped %d\n", r.Count, r.Errors, r.ErrorRate, r.RPS, s.Snapshot.Iterations, s.Snapshot.Dropped)
	fmt.Printf("latency ms: min %.1f avg %.1f p50 %.1f p90 %.1f p95 %.1f p99 %.1f max %.1f\n", r.MinMs, r.AvgMs, r.P50Ms, r.P90Ms, r.P95Ms, r.P99Ms, r.MaxMs)
	if len(s.Snapshot.ByTx) > 0 {
		fmt.Printf("\n%-28s %8s %7s %9s %9s %9s\n", "transaction", "count", "errors", "p50ms", "p95ms", "p99ms")
		for _, t := range s.Snapshot.ByTx {
			fmt.Printf("%-28s %8d %7d %9.1f %9.1f %9.1f\n", trunc(t.Name, 28), t.Count, t.Errors, t.P50Ms, t.P95Ms, t.P99Ms)
		}
	}
	for _, sla := range s.SLAs {
		mark := "PASS"
		if !sla.Passed {
			mark = "FAIL"
		}
		scope := sla.Scope
		if scope == "" {
			scope = "all"
		}
		fmt.Printf("SLA %-4s %s %s %s %g (actual %g)\n", mark, scope, sla.Metric, sla.Op, sla.Threshold, sla.Actual)
	}
	for _, fi := range s.Findings {
		fmt.Printf("[%s/%s] %s — %s\n", fi.Kind, fi.Severity, fi.Title, fi.Evidence)
	}
	if s.Baseline != nil {
		fmt.Printf("baseline %s: %s, %d regression(s) (details: loadmeter compare %s %s)\n", s.Baseline.BaselineRun, s.Baseline.Verdict, s.Baseline.Regressions, s.Baseline.BaselineRun, s.RunID)
	}
	fmt.Printf("\nreport: %s\n", filepath.Join(dir, "report.html"))
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// reorder moves flags before positional args so "run file.json --vus 5" works.
// reorder moves flags before positional arguments so "cmd file --flag v" works.
// Whether a flag takes a value is read from the flag set itself.
func reorder(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !isBoolFlag(fs, a) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func isBoolFlag(fs *flag.FlagSet, a string) bool {
	f := fs.Lookup(strings.TrimLeft(a, "-"))
	if f == nil {
		return true // unknown: let flag.Parse report it
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// ---------- other commands ----------

func cmdValidate(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stderr, "usage: loadmeter validate scenario.json")
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: loadmeter validate scenario.json")
	}
	sc, err := app.LoadScenario(args[0])
	if err != nil {
		return err
	}
	probs := sc.Validate()
	for _, ds := range sc.DataSources {
		if ds.File != "" {
			if _, err := os.Stat(filepath.Join(filepath.Dir(args[0]), ds.File)); err != nil {
				probs = append(probs, fmt.Sprintf("data source %q: %v", ds.Name, err))
			}
		}
	}
	if len(probs) > 0 {
		for _, p := range probs {
			fmt.Println("✗", p)
		}
		return exitCode(1)
	}
	fmt.Printf("✓ %s is valid: %d request(s), executor %s, up to %d VUs, %s; targets: %s\n", args[0], len(sc.AllRequests()),
		sc.Workload.Executor, sc.Workload.MaxPlannedVUs(), sc.Workload.PlannedDuration(), strings.Join(targetHosts(sc), ", "))
	return nil
}

func startMock(addr string) (string, func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: mockserver.New("loadmeter-demo").Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return "http://" + ln.Addr().String(), func() { srv.Close() }, nil
}

func cmdMock(args []string) error {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8099", "listen address (loopback recommended)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url, stop, err := startMock(*addr)
	if err != nil {
		return err
	}
	defer stop()
	fmt.Printf("mock server listening on %s (Ctrl+C to stop)\nendpoints: /login-page /login /dashboard /api/items /api/profile /api/orders /delay?ms= /status/{code} /redirect?n= /hang /malformed /api/limited /flaky /upload /soap /echo\n", url)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	return nil
}

// writeExamples copies embedded examples into dir (without overwriting).
func writeExamples(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(examples.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		dst := filepath.Join(dir, p)
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		b, err := examples.FS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	w := app.Workspace{Dir: *ws}
	if err := w.Init(); err != nil {
		return err
	}
	if err := writeExamples(w.ScenariosDir()); err != nil {
		return err
	}
	fmt.Printf("workspace ready: %s\nsample scenario: %s\nnext: loadmeter mock   (in another terminal), then: loadmeter run %s\n", *ws,
		filepath.Join(w.ScenariosDir(), "shop-login.json"), filepath.Join(w.ScenariosDir(), "shop-login.json"))
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var f runFlags
	addRunFlags(fs, &f)
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	w := app.Workspace{Dir: f.workspace}
	if err := w.Init(); err != nil {
		return err
	}
	if err := writeExamples(w.ScenariosDir()); err != nil {
		return err
	}
	url, stop, err := startMock("127.0.0.1:0")
	if err != nil {
		return err
	}
	defer stop()
	path := filepath.Join(w.ScenariosDir(), "shop-login.json")
	sc, err := app.LoadScenario(path)
	if err != nil {
		return err
	}
	sc.Variables["baseUrl"] = url
	fmt.Fprintf(os.Stderr, "demo: mock server at %s, scenario %s\n", url, path)
	return runScenario(sc, filepath.Dir(path), &f)
}

func cmdRuns(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	w := app.Workspace{Dir: *ws}
	ids, err := w.ListRuns()
	if err != nil {
		return err
	}
	for _, id := range ids {
		s, err := results.Load(filepath.Join(w.RunsDir(), id))
		if err != nil {
			continue
		}
		sla := "-"
		if len(s.SLAs) > 0 {
			sla = map[bool]string{true: "pass", false: "FAIL"}[s.SLAPassed]
		}
		fmt.Printf("%-48s %-30s reqs=%-8d p95=%-8.1f err=%.2f%% sla=%s\n", id, trunc(s.Scenario, 30), s.Snapshot.Requests.Count, s.Snapshot.Requests.P95Ms, s.Snapshot.Requests.ErrorRate, sla)
	}
	return nil
}

func loadRun(ws, id string) (*results.Summary, string, error) {
	if !app.ValidRunID(id) {
		return nil, "", fmt.Errorf("invalid run id %q", id)
	}
	dir := filepath.Join(app.Workspace{Dir: ws}.RunsDir(), id)
	s, err := results.Load(dir)
	return s, dir, err
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	base := fs.String("baseline", "", "baseline run ID to include")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter report <run-id>")
	}
	s, dir, err := loadRun(*ws, fs.Arg(0))
	if err != nil {
		return err
	}
	if *base != "" {
		b, bdir, err := loadRun(*ws, *base)
		if err != nil {
			return err
		}
		s.Baseline = results.CompareWith(b, s, results.CompareOptions{BaseDir: bdir, CurDir: dir})
	}
	desc := ""
	if sc, err := app.LoadScenario(filepath.Join(dir, "scenario.json")); err == nil {
		desc = sc.Description
	}
	out := filepath.Join(dir, "report.html")
	fh, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := report.Render(fh, s, desc); err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	jsonOut := fs.Bool("json", false, "print the comparison as JSON")
	var cf compareFlags
	addCompareFlags(fs, &cf) // gates.go
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: loadmeter compare <baseline-run-id> <run-id> [--alpha 0.05 --min-effect-pct 5 --json]")
	}
	b, bdir, err := loadRun(*ws, fs.Arg(0))
	if err != nil {
		return err
	}
	c, cdir, err := loadRun(*ws, fs.Arg(1))
	if err != nil {
		return err
	}
	cmp := results.CompareWith(b, c, cf.options(bdir, cdir))
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cmp); err != nil {
			return err
		}
	} else {
		printComparison(cmp)
	}
	if cmp.Regressions > 0 {
		return exitCode(3)
	}
	return nil
}

func cmdDoctor(args []string) error {
	ok := true
	check := func(name string, err error, detail string) {
		if err != nil {
			ok = false
			fmt.Printf("✗ %-22s %v\n", name, err)
			return
		}
		fmt.Printf("✓ %-22s %s\n", name, detail)
	}
	check("runtime", nil, fmt.Sprintf("%s %s/%s, %d CPUs", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU()))
	exe, _ := os.Executable()
	if st, err := os.Stat(exe); err == nil {
		check("binary", nil, fmt.Sprintf("%s (%.1f MB)", exe, float64(st.Size())/(1<<20)))
	}
	pol, err := safety.LoadPolicy()
	check("safety policy", err, fmt.Sprintf("%s/policy.json — maxVUs %d, allowlisted hosts %d (loopback always allowed)", safety.ConfigDir(), pol.MaxVUs, len(pol.AllowedHosts)))
	tmp, err := os.MkdirTemp("", "loadmeter-doctor")
	if err == nil {
		os.RemoveAll(tmp)
	}
	check("temp dir writable", err, os.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		ln.Close()
	}
	check("loopback networking", err, "can open local ports")
	url, stop, err := startMock("127.0.0.1:0")
	if err == nil {
		resp, herr := http.Get(url + "/health")
		if herr == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		stop()
		err = herr
	}
	check("mock server self-test", err, "HTTP round trip OK")
	if fd := fdLimit(); fd > 0 {
		var e error
		if fd < 4096 {
			e = fmt.Errorf("open-file limit %d is low for high concurrency; raise with 'ulimit -n 65535'", fd)
		}
		check("open-file limit", e, fmt.Sprintf("%d", fd))
	}
	aiKeys := []string{}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"} {
		if os.Getenv(k) != "" {
			aiKeys = append(aiKeys, k)
		}
	}
	detail := "none configured (optional; core features work without AI)"
	if len(aiKeys) > 0 {
		detail = strings.Join(aiKeys, ", ") + " present in environment"
	}
	check("AI providers", nil, detail)
	if !ok {
		return exitCode(1)
	}
	return nil
}
