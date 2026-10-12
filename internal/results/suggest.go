package results

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/stats"
)

// SuggestOptions control SLA suggestion.
type SuggestOptions struct {
	MarginPct      float64 // headroom added to the observed upper bound, % (default 20)
	MinTxSamples   uint64  // transactions with fewer samples per run are skipped (default 100)
	IncludeTx      bool    // also suggest per-transaction p95 SLAs
	MinWindowsWarn int     // warn when a run has fewer usable windows (default 30)
}

// SuggestedSLA is a proposed threshold with its derivation.
type SuggestedSLA struct {
	model.SLA
	Observed    float64 `json:"observed"`    // the observed upper bound before the margin
	Variability string  `json:"variability"` // what spread was seen
	Reasoning   string  `json:"reasoning"`
}

// Suggestion is the outcome of SuggestSLAs.
type Suggestion struct {
	Runs      []string       `json:"runs"`
	MarginPct float64        `json:"marginPct"`
	SLAs      []SuggestedSLA `json:"slas"`
	Warnings  []string       `json:"warnings,omitempty"`
	Reliable  bool           `json:"reliable"`
}

// SuggestSLAs proposes thresholds from one or more baseline runs. For
// whole-run latency percentiles the observed upper bound is
// max(highest run-level percentile, pooled per-second median + 3 x MAD), so
// both run-to-run and second-to-second variability are covered; the margin is
// added on top and the value rounded up to two significant digits. The
// error-rate ceiling is max(0.1%, 2 x the 95% Wilson upper bound of the pooled
// error proportion). Proposals are never applied by this function.
func SuggestSLAs(runs []*Summary, o SuggestOptions) *Suggestion {
	if o.MarginPct <= 0 {
		o.MarginPct = 20
	}
	if o.MinTxSamples == 0 {
		o.MinTxSamples = 100
	}
	if o.MinWindowsWarn <= 0 {
		o.MinWindowsWarn = 30
	}
	sg := &Suggestion{MarginPct: o.MarginPct, Reliable: true}
	if len(runs) == 0 {
		sg.Reliable = false
		sg.Warnings = append(sg.Warnings, "No baseline runs given.")
		return sg
	}
	m := 1 + o.MarginPct/100
	var pooled = map[string][]float64{}
	var errs, reqs float64
	for _, r := range runs {
		sg.Runs = append(sg.Runs, r.RunID)
		ws := timelineSeries(r.Snapshot.Timeline)
		if ws.windows-ws.warmup < o.MinWindowsWarn {
			sg.Reliable = false
			sg.Warnings = append(sg.Warnings, fmt.Sprintf("Run %s has only %d usable per-second windows after warm-up (want >= %d); suggestions are less reliable.", r.RunID, ws.windows-ws.warmup, o.MinWindowsWarn))
		}
		for _, q := range []string{"p95", "p99"} {
			pooled[q] = append(pooled[q], ws.lat[q]...)
		}
		errs += float64(r.Snapshot.Requests.Errors)
		reqs += float64(r.Snapshot.Requests.Count)
		if r.Snapshot.Requests.ErrorRate > 5 {
			sg.Reliable = false
			sg.Warnings = append(sg.Warnings, fmt.Sprintf("Run %s had %.2f%% errors; a baseline with many errors makes a poor reference.", r.RunID, r.Snapshot.Requests.ErrorRate))
		}
		if r.Status != "" && r.Status != "finished" {
			sg.Reliable = false
			sg.Warnings = append(sg.Warnings, fmt.Sprintf("Run %s status is %s.", r.RunID, r.Status))
		}
		hi := 0
		for _, w := range r.Snapshot.Timeline {
			if w.CPU > float64(runtime.NumCPU())*100*0.85 {
				hi++
			}
		}
		if hi > 0 {
			sg.Reliable = false
			sg.Warnings = append(sg.Warnings, fmt.Sprintf("Run %s: generator CPU was above 85%% in %d seconds; its latencies may include client-side queuing.", r.RunID, hi))
		}
	}
	for i := 1; i < len(runs); i++ {
		if runs[i].Scenario != runs[0].Scenario || runs[i].Workload.MaxPlannedVUs() != runs[0].Workload.MaxPlannedVUs() || runs[i].Workload.Executor != runs[0].Workload.Executor {
			sg.Warnings = append(sg.Warnings, fmt.Sprintf("Run %s differs from %s in scenario or workload; thresholds mix different loads.", runs[i].RunID, runs[0].RunID))
		}
	}
	for _, q := range []struct {
		metric string
		get    func(*Summary) float64
	}{{"p95", func(s *Summary) float64 { return s.Snapshot.Requests.P95Ms }}, {"p99", func(s *Summary) float64 { return s.Snapshot.Requests.P99Ms }}} {
		var runVals []float64
		for _, r := range runs {
			runVals = append(runVals, q.get(r))
		}
		obs := maxOf(runVals)
		w := pooled[q.metric]
		upper, variab := obs, "no per-second data"
		why := fmt.Sprintf("highest whole-run %s %.2f ms", q.metric, obs)
		if len(w) > 0 {
			med, mad := stats.Median(w), stats.MAD(w)
			variab = fmt.Sprintf("per-second %s median %.2f ms, MAD %.2f ms (robust CV %.0f%%) over %d windows", q.metric, med, mad, stats.CV(w)*100, len(w))
			if b := med + 3*mad; b > upper {
				upper = b
				why = fmt.Sprintf("per-second median + 3 x MAD = %.2f ms (above the highest whole-run %s %.2f ms)", b, q.metric, obs)
			}
		}
		if len(runVals) > 1 {
			variab += fmt.Sprintf("; whole-run %s across %d runs %.2f-%.2f ms", q.metric, len(runVals), minOf(runVals), obs)
		}
		if upper <= 0 {
			continue
		}
		th := niceCeil(upper * m)
		sg.SLAs = append(sg.SLAs, SuggestedSLA{SLA: model.SLA{Metric: q.metric, Op: "<", Threshold: th}, Observed: r2(upper), Variability: variab,
			Reasoning: fmt.Sprintf("Upper bound = %s; x %.2f margin = %.2f ms, rounded up to %g ms.", why, m, upper*m, th)})
	}
	if reqs > 0 {
		ub := wilsonUpper(errs, reqs, 0.95) * 100
		th := niceCeil(math.Max(0.1, 2*ub))
		obs := errs / reqs * 100
		sg.SLAs = append(sg.SLAs, SuggestedSLA{SLA: model.SLA{Metric: "error_rate", Op: "<", Threshold: th}, Observed: r3(obs),
			Variability: fmt.Sprintf("%.0f errors in %.0f requests (%.3f%%); 95%% Wilson upper bound %.3f%%", errs, reqs, obs, ub),
			Reasoning:   fmt.Sprintf("Ceiling = max(0.1%%, 2 x upper bound %.3f%%) = %g%%.", ub, th)})
	}
	if o.IncludeTx {
		names := map[string][]float64{}
		for _, r := range runs {
			for _, t := range r.Snapshot.ByTx {
				if t.Count >= o.MinTxSamples {
					names[t.Name] = append(names[t.Name], t.P95Ms)
				}
			}
		}
		keys := make([]string, 0, len(names))
		for k := range names {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := names[k]
			if len(v) < len(runs) || maxOf(v) <= 0 {
				continue // not present with enough samples in every run
			}
			obs := maxOf(v)
			th := niceCeil(obs * m)
			variab := "single run; no per-transaction per-second data in the summary"
			if len(v) > 1 {
				variab = fmt.Sprintf("whole-run p95 across %d runs %.2f-%.2f ms", len(v), minOf(v), obs)
			}
			sg.SLAs = append(sg.SLAs, SuggestedSLA{SLA: model.SLA{Metric: "p95", Scope: k, Op: "<", Threshold: th}, Observed: obs, Variability: variab,
				Reasoning: fmt.Sprintf("Highest whole-run p95 %.2f ms x %.2f margin, rounded up to %g ms.", obs, m, th)})
		}
	}
	return sg
}

// MergeSLAs adds suggestions to existing SLAs. Existing SLAs with the same
// metric and scope are kept unless replace is true. It returns the merged
// list and a human-readable diff (lines starting with +, ~ or =).
func MergeSLAs(existing []model.SLA, sugg []SuggestedSLA, replace bool) ([]model.SLA, []string) {
	out := append([]model.SLA(nil), existing...)
	var diff []string
	key := func(s model.SLA) string { return s.Metric + "\x00" + s.Scope }
	idx := map[string]int{}
	for i, s := range out {
		idx[key(s)] = i
	}
	for _, s := range sugg {
		scope := s.Scope
		if scope == "" {
			scope = "all"
		}
		if i, ok := idx[key(s.SLA)]; ok {
			old := out[i]
			if old.Op == s.Op && old.Threshold == s.Threshold {
				diff = append(diff, fmt.Sprintf("= %s %s %s %g (unchanged)", scope, s.Metric, s.Op, s.Threshold))
				continue
			}
			if !replace {
				diff = append(diff, fmt.Sprintf("= %s %s %s %g kept (suggested %s %g; use --replace to update)", scope, old.Metric, old.Op, old.Threshold, s.Op, s.Threshold))
				continue
			}
			out[i] = s.SLA
			diff = append(diff, fmt.Sprintf("~ %s %s %s %g -> %s %g", scope, s.Metric, old.Op, old.Threshold, s.Op, s.Threshold))
			continue
		}
		out = append(out, s.SLA)
		idx[key(s.SLA)] = len(out) - 1
		diff = append(diff, fmt.Sprintf("+ %s %s %s %g", scope, s.Metric, s.Op, s.Threshold))
	}
	return out, diff
}

// niceCeil rounds v up to two significant digits (e.g. 31.2 -> 32, 247 -> 250).
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 0
	}
	e := math.Pow(10, math.Floor(math.Log10(v))-1)
	return math.Round(math.Ceil(v/e-1e-9)*e*1e6) / 1e6
}

// wilsonUpper is the upper bound of the Wilson score interval for x/n.
func wilsonUpper(x, n, level float64) float64 {
	if n <= 0 {
		return 0
	}
	z := stats.NormalQuantile(1 - (1-level)/2)
	p := x / n
	d := 1 + z*z/n
	c := p + z*z/(2*n)
	r := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n))
	return (c + r) / d
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	if math.IsInf(m, -1) {
		return 0
	}
	return m
}

func minOf(v []float64) float64 {
	m := math.Inf(1)
	for _, x := range v {
		m = math.Min(m, x)
	}
	if math.IsInf(m, 1) {
		return 0
	}
	return m
}

// SuggestionText renders a suggestion for terminals and tool output.
func SuggestionText(s *Suggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Suggested SLAs from %d run(s) (margin %g%%)%s:\n", len(s.Runs), s.MarginPct, map[bool]string{true: "", false: " [LOW CONFIDENCE]"}[s.Reliable])
	for _, x := range s.SLAs {
		scope := x.Scope
		if scope == "" {
			scope = "all"
		}
		fmt.Fprintf(&b, "  %-20s %-10s %s %g\n      variability: %s\n      reasoning:   %s\n", scope, x.Metric, x.Op, x.Threshold, x.Variability, x.Reasoning)
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	return b.String()
}
