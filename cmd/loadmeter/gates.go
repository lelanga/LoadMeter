package main

// Statistical comparison flags, CI gates, SLA suggestion and abort-on-breach.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/results"
)

func init() {
	register("gate", "CI quality gate: loadmeter gate <run-id> [--baseline <run-id>] [--junit out.xml --markdown out.md --json] (exit 0 pass, 3 fail, 4 inconclusive)", cmdGate)
	register("suggest-slas", "Propose SLAs from baseline runs: loadmeter suggest-slas <run-id> [--runs id,id --margin 20 --tx --json --write scenario.json]", cmdSuggestSLAs)
}

// ---------- shared statistical flags ----------

type compareFlags struct {
	alpha, minEffectPct, minEffectMs float64
}

func addCompareFlags(fs *flag.FlagSet, f *compareFlags) {
	fs.Float64Var(&f.alpha, "alpha", 0.05, "significance level for regression verdicts (Holm-adjusted across metrics)")
	fs.Float64Var(&f.minEffectPct, "min-effect-pct", 5, "smallest latency change, in percent, that can count as a regression or improvement")
	fs.Float64Var(&f.minEffectMs, "min-effect-ms", 2, "smallest latency change, in ms, that can count as a regression or improvement")
}

func (f compareFlags) options(baseDir, curDir string) results.CompareOptions {
	o := results.CompareOptions{Alpha: f.alpha, MinEffectPct: f.minEffectPct, MinEffectMs: f.minEffectMs, BaseDir: baseDir, CurDir: curDir}
	if f.minEffectMs == 0 {
		o.MinEffectMs = -1 // explicit 0: no absolute floor
	}
	return o
}

// statFlags are the extra `loadmeter run` flags (embedded in runFlags).
type statFlags struct {
	cmp           compareFlags
	abortOnBreach bool
	abortAfter    int
	abortMinReqs  int
}

func addStatFlags(fs *flag.FlagSet, f *statFlags) {
	addCompareFlags(fs, &f.cmp)
	fs.BoolVar(&f.abortOnBreach, "abort-on-breach", false, "stop the run early when an SLA is breached in every one of --abort-after consecutive complete seconds")
	fs.IntVar(&f.abortAfter, "abort-after", 10, "consecutive breaching seconds before --abort-on-breach stops the run")
	fs.IntVar(&f.abortMinReqs, "abort-min-requests", 20, "seconds with fewer requests are not evaluated and reset the breach count")
}

// ---------- compare output ----------

func printComparison(cmp *results.Comparison) {
	fmt.Printf("%-24s %-10s %11s %11s %8s  %-13s %9s %s\n", "scope", "metric", "baseline", "current", "change", "verdict", "p(Holm)", "95% CI of shift")
	for _, d := range cmp.Deltas {
		v := d.Verdict
		if d.Informational {
			v += "*"
		}
		p, ci := "-", "-"
		if s := d.Stats; s != nil && s.Method != "" {
			p = fmt.Sprintf("%.2g", s.PAdjusted)
			ci = fmt.Sprintf("%+.2f..%+.2f", s.CILow, s.CIHigh)
		}
		fmt.Printf("%-24s %-10s %11.2f %11.2f %7.1f%%  %-13s %9s %s\n", trunc(d.Name, 24), d.Metric, d.Baseline, d.Current, d.Change, v, p, ci)
	}
	fmt.Println()
	for _, d := range cmp.Deltas {
		if d.Verdict == results.VerdictRegression && !d.Informational || d.Verdict == results.VerdictImprovement && !d.Informational {
			fmt.Printf("%s %s %s: %s\n", strings.ToUpper(d.Verdict), d.Name, d.Metric, d.Reason)
		}
	}
	for _, w := range cmp.Warnings {
		fmt.Println("warning:", w)
	}
	fmt.Printf("overall: %s (%d regression(s), %d improvement(s), %d inconclusive). * = informational, never a regression.\n", cmp.Verdict, cmp.Regressions, cmp.Improvements, cmp.Inconclusive)
	fmt.Println(cmp.Note)
}

// ---------- gate ----------

func cmdGate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	base := fs.String("baseline", "", "baseline run ID for the regression check (omit to gate on SLAs only)")
	junit := fs.String("junit", "", "write JUnit XML to this file")
	md := fs.String("markdown", "", "write the markdown summary (PR comment) to this file")
	jsonOut := fs.Bool("json", false, "print the gate result as JSON instead of markdown")
	var cf compareFlags
	addCompareFlags(fs, &cf)
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter gate <run-id> [--baseline <run-id>] [--junit out.xml] [--json]")
	}
	cur, curDir, err := loadRun(*ws, fs.Arg(0))
	if err != nil {
		return err
	}
	var b *results.Summary
	baseDir := ""
	if *base != "" {
		if b, baseDir, err = loadRun(*ws, *base); err != nil {
			return err
		}
	}
	g := results.Gate(cur, b, cf.options(baseDir, curDir))
	if *junit != "" {
		f, err := os.Create(*junit)
		if err != nil {
			return err
		}
		if err := g.WriteJUnit(f); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if *md != "" {
		if err := os.WriteFile(*md, []byte(g.Markdown()), 0o644); err != nil {
			return err
		}
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(g); err != nil {
			return err
		}
	} else {
		fmt.Print(g.Markdown())
	}
	if g.ExitCode != 0 {
		return exitCode(g.ExitCode)
	}
	return nil
}

// ---------- suggest-slas ----------

func cmdSuggestSLAs(args []string) error {
	fs := flag.NewFlagSet("suggest-slas", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	var more stringList
	fs.Var(&more, "runs", "additional baseline run IDs (comma-separated or repeated)")
	margin := fs.Float64("margin", 20, "headroom in percent added to the observed upper bound")
	tx := fs.Bool("tx", false, "also suggest per-transaction p95 SLAs")
	jsonOut := fs.Bool("json", false, "print the suggestion as JSON")
	write := fs.String("write", "", "add the suggestions to this scenario file (shows a diff and asks first)")
	replace := fs.Bool("replace", false, "with --write: also update existing SLAs with the same metric and scope")
	yes := fs.Bool("yes", false, "with --write: apply without the interactive confirmation (the diff is still printed)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter suggest-slas <baseline-run-id> [--runs id1,id2] [--margin 20] [--json] [--write scenario.json]")
	}
	var runs []*results.Summary
	for _, id := range append([]string{fs.Arg(0)}, more...) {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		s, _, err := loadRun(*ws, id)
		if err != nil {
			return err
		}
		runs = append(runs, s)
	}
	sg := results.SuggestSLAs(runs, results.SuggestOptions{MarginPct: *margin, IncludeTx: *tx})
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sg); err != nil {
			return err
		}
	} else {
		fmt.Print(results.SuggestionText(sg))
	}
	if *write == "" {
		return nil
	}
	sc, err := app.LoadScenario(*write)
	if err != nil {
		return err
	}
	merged, diff := results.MergeSLAs(sc.SLAs, sg.SLAs, *replace)
	out := os.Stdout
	if *jsonOut {
		out = os.Stderr // keep stdout valid JSON
	}
	fmt.Fprintf(out, "\nChanges to the SLAs of %s:\n", *write)
	changed := false
	for _, d := range diff {
		fmt.Fprintln(out, "  "+d)
		changed = changed || d[0] == '+' || d[0] == '~'
	}
	if !changed {
		fmt.Fprintln(out, "Nothing to change.")
		return nil
	}
	if !sg.Reliable {
		fmt.Fprintln(out, "Note: these suggestions are marked low confidence (see warnings above).")
	}
	fmt.Fprintln(out, "The file will be rewritten with canonical JSON formatting.")
	if !*yes {
		st, err := os.Stdin.Stat()
		if err != nil || st.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("not applied: confirm interactively or pass --yes after reviewing the diff")
		}
		fmt.Fprint(out, "Apply these changes? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Not applied.")
			return nil
		}
	}
	sc.SLAs = merged
	if err := app.SaveScenario(*write, sc); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s (%d SLA(s)).\n", *write, len(merged))
	return nil
}

// ---------- abort on breach ----------

// breachWatch stops a run when an unscoped SLA is breached in N consecutive
// complete one-second windows. It is deliberately conservative: it evaluates
// only finished seconds, skips the first (partial) second, needs a minimum
// number of requests per second, and any second that cannot be evaluated
// resets the count. Only window statistics are used (p50/p95/p99/avg/max from
// that second's histogram, error_rate from that second's counts); SLAs on p90,
// rps or a transaction/request scope are not watched.
type breachWatch struct {
	f       statFlags
	slas    []model.SLA
	skipped []string
	mu      sync.Mutex
	reason  string
}

func newBreachWatch(sc *model.Scenario, f statFlags) *breachWatch {
	if !f.abortOnBreach {
		return nil
	}
	b := &breachWatch{f: f}
	b.f.abortAfter = max(b.f.abortAfter, 1)
	for _, s := range sc.SLAs {
		switch {
		case s.Scope != "":
			b.skipped = append(b.skipped, fmt.Sprintf("%s %s (scoped)", s.Scope, s.Metric))
		case s.Metric == "p50" || s.Metric == "p95" || s.Metric == "p99" || s.Metric == "avg" || s.Metric == "max" || s.Metric == "error_rate":
			b.slas = append(b.slas, s)
		default:
			b.skipped = append(b.skipped, s.Metric)
		}
	}
	return b
}

func windowValue(w metrics.WindowOut, metric string) float64 {
	switch metric {
	case "p50":
		return w.P50Ms
	case "p95":
		return w.P95Ms
	case "p99":
		return w.P99Ms
	case "avg":
		return w.AvgMs
	case "max":
		return w.MaxMs
	}
	return w.ErrorRate
}

func satisfies(a float64, op string, b float64) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return true
}

func (b *breachWatch) start(a *app.Active, quiet bool) {
	if b == nil {
		return
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "abort-on-breach: watching %d SLA(s); stops after %d consecutive breaching seconds (>= %d requests each)", len(b.slas), b.f.abortAfter, b.f.abortMinReqs)
		if len(b.skipped) > 0 {
			fmt.Fprintf(os.Stderr, "; not watched: %s", strings.Join(b.skipped, ", "))
		}
		fmt.Fprintln(os.Stderr)
	}
	if len(b.slas) == 0 {
		return
	}
	go func() {
		streak := make([]int, len(b.slas))
		next := 1 // window 0 is a partial second
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-a.Run.Done():
				return
			case now := <-t.C:
				cur := int(now.Sub(a.Run.StartedAt) / time.Second)
				if cur <= next {
					continue
				}
				snap := a.Run.Col.Snapshot(now, next)
				for _, w := range snap.Timeline {
					if w.T >= cur {
						break
					}
					next = w.T + 1
					for i, s := range b.slas {
						v := windowValue(w, s.Metric)
						switch {
						case w.RPS < float64(b.f.abortMinReqs):
							streak[i] = 0
						case !satisfies(v, s.Op, s.Threshold):
							streak[i]++
						default:
							streak[i] = 0
						}
						if streak[i] >= b.f.abortAfter {
							b.mu.Lock()
							b.reason = fmt.Sprintf("SLA %s %s %g was breached in %d consecutive seconds (t=%d-%ds; last second %s=%g over %.0f requests)",
								s.Metric, s.Op, s.Threshold, streak[i], w.T-streak[i]+1, w.T, s.Metric, v, w.RPS)
							b.mu.Unlock()
							fmt.Fprintln(os.Stderr, "\nabort-on-breach: "+b.Reason()+"; stopping the run")
							a.Run.Stop()
							return
						}
					}
				}
			}
		}
	}()
}

// Reason is empty unless the watch stopped the run.
func (b *breachWatch) Reason() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason
}

// finalize marks the summary before it is saved (app.RunOptions.Finalize).
func (b *breachWatch) finalize(s *results.Summary) {
	r := b.Reason()
	if r == "" {
		return
	}
	s.Status = "aborted-sla"
	s.Notes = append(s.Notes, "Stopped early by --abort-on-breach: "+r+".")
	s.Findings = append(s.Findings, results.Finding{Kind: "observation", Severity: "critical", Title: "Run aborted: SLA breached",
		Evidence: r + ". Evaluated on per-second window statistics; the whole-run values above cover only the time before the stop.",
		Method:   fmt.Sprintf("--abort-on-breach, %d consecutive seconds, >= %d requests per second", b.f.abortAfter, b.f.abortMinReqs)})
}

func (b *breachWatch) finalizer() func(*results.Summary) {
	if b == nil {
		return nil
	}
	return b.finalize
}
