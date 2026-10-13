package results

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/stats"
)

// Verdicts of a statistical comparison.
const (
	VerdictRegression   = "regression"
	VerdictImprovement  = "improvement"
	VerdictNoChange     = "no-change"
	VerdictInconclusive = "inconclusive"
)

// CompareOptions control the statistical comparison. Zero values mean defaults.
type CompareOptions struct {
	Alpha        float64 `json:"alpha"`        // family-wise significance level (Holm), default 0.05
	MinEffectPct float64 `json:"minEffectPct"` // smallest latency change worth reporting, % (default 5)
	MinEffectMs  float64 `json:"minEffectMs"`  // and in absolute ms (default 2)
	MinErrorPP   float64 `json:"minErrorPP"`   // smallest error-rate rise worth reporting, percentage points (default 1)
	MinWindows   int     `json:"minWindows"`   // usable per-second windows needed per run (default 10)
	Resamples    int     `json:"resamples"`    // bootstrap resamples (default 2000)
	Seed         uint64  `json:"seed"`         // bootstrap seed (default 1)
	NoWarmup     bool    `json:"noWarmup,omitempty"`
	// BaseDir/CurDir are optional run folders; when both hold raw.csv the
	// per-transaction comparison uses per-second windows rebuilt from it.
	BaseDir string `json:"-"`
	CurDir  string `json:"-"`
}

// DefaultCompareOptions returns the defaults used by Compare.
func DefaultCompareOptions() CompareOptions { return CompareOptions{}.norm() }

func (o CompareOptions) norm() CompareOptions {
	if o.Alpha <= 0 || o.Alpha >= 1 {
		o.Alpha = 0.05
	}
	if o.MinEffectPct <= 0 {
		o.MinEffectPct = 5
	}
	if o.MinEffectMs < 0 {
		o.MinEffectMs = 0
	} else if o.MinEffectMs == 0 {
		o.MinEffectMs = 2
	}
	if o.MinErrorPP <= 0 {
		o.MinErrorPP = 1
	}
	if o.MinWindows <= 0 {
		o.MinWindows = 10
	}
	if o.Resamples <= 0 {
		o.Resamples = 2000
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	return o
}

// DeltaStats is the statistical evidence behind a Delta's verdict.
type DeltaStats struct {
	Method          string  `json:"method"`
	Source          string  `json:"source"` // timeline | raw.csv | totals
	NBase           int     `json:"nBase"`  // windows (or requests for error rate) used
	NCur            int     `json:"nCur"`
	WarmupBase      int     `json:"warmupBase,omitempty"` // leading windows discarded
	WarmupCur       int     `json:"warmupCur,omitempty"`
	BaseMedian      float64 `json:"baseMedian"` // median of the per-window values (or the proportion, %)
	CurMedian       float64 `json:"curMedian"`
	Shift           float64 `json:"shift"`    // CurMedian - BaseMedian
	ShiftPct        float64 `json:"shiftPct"` // relative to BaseMedian
	CILow           float64 `json:"ciLow"`    // CI of Shift
	CIHigh          float64 `json:"ciHigh"`
	CILevel         float64 `json:"ciLevel"`
	PValue          float64 `json:"pValue"`
	PAdjusted       float64 `json:"pAdjusted"` // Holm-adjusted across the gated tests
	EffectSize      float64 `json:"effectSize"`
	EffectName      string  `json:"effectName"` // cliffs_delta | cohens_h
	EffectMagnitude string  `json:"effectMagnitude,omitempty"`
}

type cmpTest struct {
	d     *Delta
	p     float64
	gated bool
	done  func(padj float64)
}

// CompareWith compares cur against base metric by metric. Each latency metric
// is tested on per-second window values (the run timeline, or windows rebuilt
// from raw.csv for transactions) after discarding warm-up windows: Mann-Whitney
// U for significance, a moving-block bootstrap CI of the median shift, and
// Cliff's delta as effect size. Error rates use a two-proportion test on the
// request totals. P-values are Holm-adjusted across all gated tests. A
// regression needs significance, a CI excluding zero and a change at least
// MinEffectPct and MinEffectMs (MinErrorPP for errors).
func CompareWith(base, cur *Summary, o CompareOptions) *Comparison {
	o = o.norm()
	c := &Comparison{BaselineRun: base.RunID, Alpha: o.Alpha, MinEffectPct: o.MinEffectPct, MinEffectMs: o.MinEffectMs, MinErrorPP: o.MinErrorPP,
		Method: "per-second windows after warm-up; Mann-Whitney U + block-bootstrap CI of median shift + Cliff's delta; two-proportion test for errors; Holm across tests"}
	c.Note = fmt.Sprintf("Verdicts are statistical: regression = significant at alpha %.3g (Holm-adjusted), CI of the shift excludes 0, and change >= %.3g%% and >= %.3g ms (errors: >= %.3g percentage points). Samples are per-second window percentiles, not individual requests; whole-run percentiles in baseline/current come from full histograms.",
		o.Alpha, o.MinEffectPct, o.MinEffectMs, o.MinErrorPP)
	c.Warnings = comparability(base, cur)
	var tests []cmpTest
	var deltas []*Delta // pointers stay valid while tests run; copied into c.Deltas at the end

	bw, cw := timelineSeries(base.Snapshot.Timeline), timelineSeries(cur.Snapshot.Timeline)
	var braw, craw map[string]*rawSeries
	if o.BaseDir != "" && o.CurDir != "" {
		braw, _ = readRawWindows(filepath.Join(o.BaseDir, "raw.csv"), base.StartedAt.UnixMilli())
		craw, _ = readRawWindows(filepath.Join(o.CurDir, "raw.csv"), cur.StartedAt.UnixMilli())
	}
	scope := func(name string, b, n metrics.MetricSet, bs, cs *windowSeries, src string) {
		for _, m := range []struct {
			k    string
			a, z float64
		}{{"p50Ms", b.P50Ms, n.P50Ms}, {"p95Ms", b.P95Ms, n.P95Ms}, {"p99Ms", b.P99Ms, n.P99Ms}, {"errorRate", b.ErrorRate, n.ErrorRate}, {"rps", b.RPS, n.RPS}} {
			d := &Delta{Name: name, Metric: m.k, Baseline: m.a, Current: m.z}
			if m.a > 0 {
				d.Change = math.Round((m.z-m.a)/m.a*10000) / 100
			}
			deltas = append(deltas, d)
		}
		ds := deltas[len(deltas)-5:]
		for i, q := range []string{"p50", "p95", "p99"} {
			var x, y []float64
			if bs != nil && cs != nil {
				x, y = bs.lat[q], cs.lat[q]
			}
			tests = append(tests, latencyTest(ds[i], x, y, bs, cs, src, o))
		}
		tests = append(tests, errorTest(ds[3], b, n, o))
		tests = append(tests, rpsTest(ds[4], bs, cs, src, o))
	}
	scope("ALL", base.Snapshot.Requests, cur.Snapshot.Requests, bw, cw, "timeline")
	bt := map[string]metrics.MetricSet{}
	for _, n := range base.Snapshot.ByTx {
		bt[n.Name] = n.MetricSet
	}
	for _, n := range cur.Snapshot.ByTx {
		if b, ok := bt[n.Name]; ok {
			var bs, cs *windowSeries
			if braw != nil && craw != nil {
				bs, cs = braw["transaction\x00"+n.Name].series(), craw["transaction\x00"+n.Name].series()
			}
			scope("tx:"+n.Name, b, n.MetricSet, bs, cs, "raw.csv")
		}
	}
	// Holm across every gated test that produced a p-value.
	var ps []float64
	var idx []int
	for i, t := range tests {
		if t.gated && t.done != nil {
			ps = append(ps, t.p)
			idx = append(idx, i)
		}
	}
	adj := stats.Holm(ps)
	for k, i := range idx {
		tests[i].done(adj[k])
	}
	for _, t := range tests {
		if !t.gated && t.done != nil {
			t.done(t.p) // informational (rps): unadjusted
		}
	}
	for _, d := range deltas {
		c.Deltas = append(c.Deltas, *d)
	}
	allInconclusive := false
	for i := range c.Deltas {
		d := &c.Deltas[i]
		if d.Informational {
			continue
		}
		switch d.Verdict {
		case VerdictRegression:
			d.Worse = true
			c.Regressions++
		case VerdictImprovement:
			c.Improvements++
		case VerdictInconclusive:
			c.Inconclusive++
			if d.Name == "ALL" && (d.Metric == "p95Ms" || d.Metric == "errorRate") {
				allInconclusive = true
			}
		}
	}
	switch {
	case c.Regressions > 0:
		c.Verdict = VerdictRegression
	case allInconclusive:
		c.Verdict = VerdictInconclusive
	case c.Improvements > 0:
		c.Verdict = VerdictImprovement
	default:
		c.Verdict = VerdictNoChange
	}
	return c
}

// windowSeries holds per-second values of one scope, warm-up already removed.
type windowSeries struct {
	lat     map[string][]float64 // p50/p95/p99 per usable window, ms
	rps     []float64
	warmup  int
	windows int // usable windows before trimming
}

// timelineSeries extracts per-window series from the run timeline. The first
// and last windows are partial seconds and are dropped, as are windows without
// requests; MSER-5 on the p95 series plus the VU ramp set the warm-up cut.
func timelineSeries(tl []metrics.WindowOut) *windowSeries {
	if len(tl) > 2 {
		tl = tl[1 : len(tl)-1]
	} else {
		tl = nil
	}
	var p50, p95, p99, rps, vus []float64
	for _, w := range tl {
		if w.RPS <= 0 {
			continue
		}
		p50, p95, p99 = append(p50, w.P50Ms), append(p95, w.P95Ms), append(p99, w.P99Ms)
		rps, vus = append(rps, w.RPS), append(vus, float64(w.VUs))
	}
	return trimmed(p50, p95, p99, rps, vus)
}

func trimmed(p50, p95, p99, rps, vus []float64) *windowSeries {
	s := &windowSeries{windows: len(p95)}
	s.warmup = stats.WarmupEnd(p95, vus)
	k := s.warmup
	s.lat = map[string][]float64{"p50": p50[k:], "p95": p95[k:], "p99": p99[k:]}
	s.rps = rps[k:]
	return s
}

func latencyTest(d *Delta, x, y []float64, bs, cs *windowSeries, src string, o CompareOptions) cmpTest {
	t := cmpTest{d: d, gated: true}
	st := &DeltaStats{Source: src, NBase: len(x), NCur: len(y), CILevel: 0.95, EffectName: "cliffs_delta"}
	if bs != nil && cs != nil {
		st.WarmupBase, st.WarmupCur = bs.warmup, cs.warmup
	}
	d.Stats = st
	if len(x) < o.MinWindows || len(y) < o.MinWindows {
		d.Verdict = VerdictInconclusive
		if bs == nil || cs == nil {
			d.Reason = "No per-second data for this scope (needs the run timeline, or raw.csv for transactions); cannot test the change."
			if src == "raw.csv" {
				d.Reason = "No per-second data for this transaction in both runs' raw.csv; cannot test the change."
			}
		} else {
			d.Reason = fmt.Sprintf("Only %d baseline and %d current usable windows after warm-up (need %d each).", len(x), len(y), o.MinWindows)
		}
		return t
	}
	mw, _ := stats.MannWhitneyDependent(y, x, stats.TwoSided)
	ci := stats.BootstrapCI(x, y, stats.MedianShift, stats.BootstrapOptions{Resamples: o.Resamples, Seed: o.Seed, Level: 0.95})
	st.Method = mw.Method + "; moving-block bootstrap (block " + strconv.Itoa(ci.Block) + ") 95% CI of median shift"
	st.BaseMedian, st.CurMedian = r3(stats.Median(x)), r3(stats.Median(y))
	st.Shift, st.CILow, st.CIHigh = r3(ci.Estimate), r3(ci.Lo), r3(ci.Hi)
	bm := stats.Median(x)
	if bm > 0 {
		st.ShiftPct = r2(ci.Estimate / bm * 100)
	}
	st.PValue = mw.P
	st.EffectSize = r3(stats.CliffsDelta(y, x))
	st.EffectMagnitude = stats.CliffsMagnitude(st.EffectSize)
	t.p = mw.P
	t.done = func(padj float64) {
		st.PAdjusted = padj
		sig := padj < o.Alpha && (ci.Lo > 0 || ci.Hi < 0)
		practical := math.Abs(st.ShiftPct) >= o.MinEffectPct && math.Abs(ci.Estimate) >= o.MinEffectMs
		desc := fmt.Sprintf("median per-second %s %.2f -> %.2f ms (%+.1f%%, 95%% CI %+.2f..%+.2f ms), p=%s (Holm %s), Cliff's delta %.2f (%s), %d vs %d windows",
			strings.TrimSuffix(d.Metric, "Ms"), st.BaseMedian, st.CurMedian, st.ShiftPct, st.CILow, st.CIHigh, fmtP(mw.P), fmtP(padj), st.EffectSize, st.EffectMagnitude, len(x), len(y))
		switch {
		case sig && practical && ci.Estimate > 0:
			d.Verdict, d.Reason = VerdictRegression, "Slower: "+desc+"."
		case sig && practical:
			d.Verdict, d.Reason = VerdictImprovement, "Faster: "+desc+"."
		case sig:
			d.Verdict, d.Reason = VerdictNoChange, "Statistically detectable but smaller than the minimum effect: "+desc+"."
		default:
			d.Verdict, d.Reason = VerdictNoChange, "No significant difference: "+desc+"."
			if bm > 0 && ci.Hi/bm*100 >= o.MinEffectPct && ci.Hi >= o.MinEffectMs {
				d.Reason += fmt.Sprintf(" A slowdown of up to %.1f%% is not ruled out by this data.", ci.Hi/bm*100)
			}
		}
	}
	return t
}

func errorTest(d *Delta, b, n metrics.MetricSet, o CompareOptions) cmpTest {
	t := cmpTest{d: d, gated: true}
	st := &DeltaStats{Source: "totals", NBase: int(b.Count), NCur: int(n.Count), CILevel: 0.95, EffectName: "cohens_h"}
	d.Stats = st
	if b.Count == 0 || n.Count == 0 {
		d.Verdict, d.Reason = VerdictInconclusive, "No request counts in one of the runs; cannot test the error rate."
		return t
	}
	pr := stats.TwoProportions(float64(b.Errors), float64(b.Count), float64(n.Errors), float64(n.Count), 0.95)
	st.Method = pr.Method
	st.BaseMedian, st.CurMedian = r3(pr.P1*100), r3(pr.P2*100)
	st.Shift, st.CILow, st.CIHigh = r3(pr.Diff*100), r3(pr.Lo*100), r3(pr.Hi*100)
	if pr.P1 > 0 {
		st.ShiftPct = r2(pr.Diff / pr.P1 * 100)
	}
	st.PValue, st.EffectSize = pr.P, r3(pr.CohensH)
	t.p = pr.P
	t.done = func(padj float64) {
		st.PAdjusted = padj
		desc := fmt.Sprintf("errors %.2f%% -> %.2f%% (%+.2f pp, 95%% CI %+.2f..%+.2f pp), p=%s (Holm %s), Cohen's h %.2f, %d vs %d requests",
			st.BaseMedian, st.CurMedian, st.Shift, st.CILow, st.CIHigh, fmtP(pr.P), fmtP(padj), st.EffectSize, b.Count, n.Count)
		sig := padj < o.Alpha
		switch {
		case sig && pr.Lo > 0 && pr.Diff*100 >= o.MinErrorPP:
			d.Verdict, d.Reason = VerdictRegression, "More errors: "+desc+"."
		case sig && pr.Hi < 0 && -pr.Diff*100 >= o.MinErrorPP:
			d.Verdict, d.Reason = VerdictImprovement, "Fewer errors: "+desc+"."
		case sig:
			d.Verdict, d.Reason = VerdictNoChange, "Statistically detectable but below the minimum effect: "+desc+"."
		default:
			d.Verdict, d.Reason = VerdictNoChange, "No significant difference: "+desc+". Requests are treated as independent, which overstates confidence when errors come in bursts."
		}
	}
	return t
}

func rpsTest(d *Delta, bs, cs *windowSeries, src string, o CompareOptions) cmpTest {
	d.Informational = true
	t := cmpTest{d: d}
	st := &DeltaStats{Source: src, CILevel: 0.95, EffectName: "cliffs_delta"}
	d.Stats = st
	if bs == nil || cs == nil || len(bs.rps) < o.MinWindows || len(cs.rps) < o.MinWindows {
		d.Verdict, d.Reason = VerdictInconclusive, "Not enough per-second windows to test throughput (informational only)."
		return t
	}
	x, y := bs.rps, cs.rps
	st.NBase, st.NCur, st.WarmupBase, st.WarmupCur = len(x), len(y), bs.warmup, cs.warmup
	mw, _ := stats.MannWhitneyDependent(y, x, stats.TwoSided)
	ci := stats.BootstrapCI(x, y, stats.MedianShift, stats.BootstrapOptions{Resamples: o.Resamples, Seed: o.Seed})
	st.Method = mw.Method + "; moving-block bootstrap 95% CI of median shift (unadjusted, informational)"
	st.BaseMedian, st.CurMedian, st.Shift, st.CILow, st.CIHigh = r3(stats.Median(x)), r3(stats.Median(y)), r3(ci.Estimate), r3(ci.Lo), r3(ci.Hi)
	if m := stats.Median(x); m > 0 {
		st.ShiftPct = r2(ci.Estimate / m * 100)
	}
	st.PValue, st.PAdjusted = mw.P, mw.P
	st.EffectSize = r3(stats.CliffsDelta(y, x))
	st.EffectMagnitude = stats.CliffsMagnitude(st.EffectSize)
	t.p = mw.P
	t.done = func(float64) {
		desc := fmt.Sprintf("median requests/s %.1f -> %.1f (%+.1f%%), p=%s", st.BaseMedian, st.CurMedian, st.ShiftPct, fmtP(mw.P))
		sig := mw.P < o.Alpha && (ci.Lo > 0 || ci.Hi < 0) && math.Abs(st.ShiftPct) >= o.MinEffectPct
		switch {
		case sig && ci.Estimate < 0:
			d.Verdict = VerdictRegression
		case sig:
			d.Verdict = VerdictImprovement
		default:
			d.Verdict = VerdictNoChange
		}
		d.Reason = "Throughput (informational, never gates; it depends on the workload model): " + desc + "."
	}
	return t
}

// comparability lists reasons the two runs may not be comparable.
func comparability(b, c *Summary) []string {
	var w []string
	if b.Workload.Executor != c.Workload.Executor || b.Workload.MaxPlannedVUs() != c.Workload.MaxPlannedVUs() || b.Workload.Rate != c.Workload.Rate {
		w = append(w, fmt.Sprintf("Workloads differ (baseline %s/%d VUs/rate %g, current %s/%d VUs/rate %g); differences may be caused by load, not code.",
			b.Workload.Executor, b.Workload.MaxPlannedVUs(), b.Workload.Rate, c.Workload.Executor, c.Workload.MaxPlannedVUs(), c.Workload.Rate))
	}
	if b.Scenario != "" && c.Scenario != "" && b.Scenario != c.Scenario {
		w = append(w, fmt.Sprintf("Scenarios differ (%q vs %q).", b.Scenario, c.Scenario))
	}
	if b.Status != "" && b.Status != "finished" {
		w = append(w, "Baseline run status is "+b.Status+".")
	}
	if c.Status != "" && c.Status != "finished" {
		w = append(w, "Current run status is "+c.Status+".")
	}
	return w
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }
func r3(v float64) float64 { return math.Round(v*1000) / 1000 }

func fmtP(p float64) string {
	if p < 0.0001 {
		return "<0.0001"
	}
	return strconv.FormatFloat(p, 'g', 2, 64)
}

// ---------- raw.csv windows ----------

type rawWin struct {
	n, errs uint64
	hist    metrics.Sparse
}

type rawSeries struct{ wins map[int]*rawWin }

// readRawWindows rebuilds per-second, per-label windows from a JTL-compatible
// raw.csv (columns timeStamp, elapsed, label, ..., success, ..., kind). JTL
// elapsed is whole milliseconds (truncated), so each value is taken as
// elapsed+0.5 ms: sub-millisecond differences are invisible in this source.
func readRawWindows(path string, startMs int64) (map[string]*rawSeries, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	need := []string{"timeStamp", "elapsed", "label", "success"}
	for _, k := range need {
		if _, ok := col[k]; !ok {
			return nil, fmt.Errorf("%s: missing column %s", path, k)
		}
	}
	kindCol, hasKind := col["kind"]
	out := map[string]*rawSeries{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		ts, err1 := strconv.ParseInt(rec[col["timeStamp"]], 10, 64)
		el, err2 := strconv.ParseInt(rec[col["elapsed"]], 10, 64)
		if err1 != nil || err2 != nil || el < 0 {
			continue
		}
		kind := metrics.KindRequest
		if hasKind && kindCol < len(rec) && rec[kindCol] != "" {
			kind = rec[kindCol]
		} else if mc, ok := col["responseMessage"]; ok && mc < len(rec) && strings.HasPrefix(rec[mc], metrics.TransactionMessagePrefix) {
			kind = metrics.KindTransaction // JMeter JTL: Transaction Controller row
		}
		key := kind + "\x00" + rec[col["label"]]
		s := out[key]
		if s == nil {
			s = &rawSeries{wins: map[int]*rawWin{}}
			out[key] = s
		}
		sec := int((ts + el - startMs) / 1000)
		if sec < 0 {
			sec = 0
		}
		w := s.wins[sec]
		if w == nil {
			w = &rawWin{hist: metrics.Sparse{}}
			s.wins[sec] = w
		}
		w.n++
		if rec[col["success"]] != "true" {
			w.errs++
		}
		w.hist.Record(uint64(el)*1000 + 500)
	}
	return out, nil
}

func (s *rawSeries) series() *windowSeries {
	if s == nil || len(s.wins) == 0 {
		return nil
	}
	secs := make([]int, 0, len(s.wins))
	last := 0
	for k := range s.wins {
		secs = append(secs, k)
		last = max(last, k)
	}
	sort.Ints(secs)
	var p50, p95, p99, rps []float64
	for _, k := range secs {
		if k == 0 || k == last { // partial first/last second
			continue
		}
		w := s.wins[k]
		p50 = append(p50, float64(w.hist.Quantile(0.5))/1000)
		p95 = append(p95, float64(w.hist.Quantile(0.95))/1000)
		p99 = append(p99, float64(w.hist.Quantile(0.99))/1000)
		rps = append(rps, float64(w.n))
	}
	return trimmed(p50, p95, p99, rps, nil)
}
