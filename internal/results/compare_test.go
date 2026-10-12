package results

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/stats"
)

type synthSpec struct {
	windows   int
	p95, sd   float64
	rps       float64
	errPerWin uint64
	vus       int
}

// synth builds a run summary with a per-second timeline whose window p95
// values are normal(p95, sd); p50 and p99 scale with it.
func synth(id string, seed uint64, sp synthSpec) *Summary {
	r := stats.NewRand(seed)
	s := &Summary{RunID: id, Scenario: "synthetic", Status: "finished", StartedAt: t0}
	s.Workload = model.Workload{Executor: model.ExecConstantVUs, VUs: max(sp.vus, 10)}
	var all []float64
	for i := 0; i < sp.windows; i++ {
		p := math.Max(0.1, sp.p95+sp.sd*r.NormFloat64())
		w := metrics.WindowOut{T: i, RPS: sp.rps, P50Ms: p * 0.5, P95Ms: p, P99Ms: p * 1.4, Errors: sp.errPerWin, VUs: max(sp.vus, 10)}
		w.ErrorRate = float64(w.Errors) / w.RPS * 100
		s.Snapshot.Timeline = append(s.Snapshot.Timeline, w)
		all = append(all, p)
	}
	n := uint64(float64(sp.windows) * sp.rps)
	e := uint64(sp.windows) * sp.errPerWin
	s.Snapshot.Requests = metrics.MetricSet{Count: n, Errors: e, RPS: sp.rps, P50Ms: stats.Median(all) * 0.5, P95Ms: stats.Quantile(all, 0.95), P99Ms: stats.Quantile(all, 0.99) * 1.4}
	if n > 0 {
		s.Snapshot.Requests.ErrorRate = math.Round(float64(e)/float64(n)*10000) / 100
	}
	return s
}

func delta(t *testing.T, c *Comparison, name, metric string) Delta {
	t.Helper()
	for _, d := range c.Deltas {
		if d.Name == name && d.Metric == metric {
			return d
		}
	}
	t.Fatalf("delta %s/%s not found", name, metric)
	return Delta{}
}

func TestCompareDetectsShift(t *testing.T) {
	base := synth("b", 1, synthSpec{windows: 60, p95: 100, sd: 10, rps: 200})
	cur := synth("c", 2, synthSpec{windows: 60, p95: 130, sd: 10, rps: 200})
	c := Compare(base, cur)
	d := delta(t, c, "ALL", "p95Ms")
	if d.Verdict != VerdictRegression || !d.Worse || c.Regressions < 1 || c.Verdict != VerdictRegression {
		t.Fatalf("%+v %+v", d, d.Stats)
	}
	st := d.Stats
	if st.PAdjusted >= 0.05 || st.CILow <= 0 || st.ShiftPct < 20 || st.ShiftPct > 40 || st.EffectMagnitude != "large" || st.Source != "timeline" {
		t.Errorf("stats %+v", st)
	}
	if !strings.HasPrefix(d.Reason, "Slower:") || !strings.Contains(d.Reason, "Cliff's delta") {
		t.Errorf("reason %q", d.Reason)
	}
	// the same pair reversed is an improvement, never a regression
	c = Compare(cur, base)
	if d := delta(t, c, "ALL", "p95Ms"); d.Verdict != VerdictImprovement || d.Worse || c.Regressions != 0 || c.Verdict != VerdictImprovement {
		t.Errorf("%+v", d)
	}
	// deterministic: same inputs => same JSON-relevant numbers
	a, b := Compare(base, cur), Compare(base, cur)
	if *delta(t, a, "ALL", "p95Ms").Stats != *delta(t, b, "ALL", "p95Ms").Stats {
		t.Error("comparison not deterministic")
	}
}

func TestCompareFalsePositiveRate(t *testing.T) {
	const trials = 150
	o := CompareOptions{Resamples: 300}
	regressions, anyVerdict := 0, 0
	for s := uint64(1); s <= trials; s++ {
		b := synth("b", s, synthSpec{windows: 40, p95: 50, sd: 8, rps: 100})
		c := synth("c", s+10_000, synthSpec{windows: 40, p95: 50, sd: 8, rps: 100})
		cmp := CompareWith(b, c, o)
		if cmp.Regressions > 0 {
			regressions++
		}
		if cmp.Regressions > 0 || cmp.Improvements > 0 {
			anyVerdict++
		}
	}
	rate := float64(regressions) / trials
	// family-wise alpha is 0.05 two-sided; one direction ~0.025. Allow 3 binomial sd.
	if rate > 0.05+3*math.Sqrt(0.05*0.95/trials) {
		t.Errorf("false regression rate %.3f with identical distributions", rate)
	}
	t.Logf("identical distributions: %d/%d runs flagged a regression, %d/%d any change", regressions, trials, anyVerdict, trials)
}

func TestCompareMinimumEffectAndInconclusive(t *testing.T) {
	// +3% with tiny noise: significant but below 5% => no-change
	b := synth("b", 3, synthSpec{windows: 60, p95: 100, sd: 1, rps: 100})
	c := synth("c", 4, synthSpec{windows: 60, p95: 103, sd: 1, rps: 100})
	d := delta(t, Compare(b, c), "ALL", "p95Ms")
	if d.Verdict != VerdictNoChange || d.Worse || !strings.Contains(d.Reason, "smaller than the minimum effect") {
		t.Errorf("%+v", d)
	}
	// with a 2% minimum effect it becomes a regression
	d = delta(t, CompareWith(b, c, CompareOptions{MinEffectPct: 2}), "ALL", "p95Ms")
	if d.Verdict != VerdictRegression {
		t.Errorf("min-effect 2%%: %+v", d)
	}
	// +50% but only 1 ms absolute: below the 2 ms floor
	b = synth("b", 5, synthSpec{windows: 60, p95: 2, sd: 0.05, rps: 100})
	c = synth("c", 6, synthSpec{windows: 60, p95: 3, sd: 0.05, rps: 100})
	if d := delta(t, Compare(b, c), "ALL", "p95Ms"); d.Verdict != VerdictNoChange {
		t.Errorf("1 ms change: %+v", d)
	}
	// too few windows => inconclusive overall, nothing counted
	b = synth("b", 7, synthSpec{windows: 6, p95: 100, sd: 1, rps: 100})
	c = synth("c", 8, synthSpec{windows: 6, p95: 300, sd: 1, rps: 100})
	cmp := Compare(b, c)
	if d := delta(t, cmp, "ALL", "p95Ms"); d.Verdict != VerdictInconclusive || cmp.Verdict != VerdictInconclusive || cmp.Regressions != 0 || d.Change < 100 {
		t.Errorf("%+v / %+v", cmp, d)
	}
}

func TestCompareErrorsAndThroughput(t *testing.T) {
	b := synth("b", 1, synthSpec{windows: 60, p95: 50, sd: 3, rps: 500, errPerWin: 5})    // 1%
	c := synth("c", 2, synthSpec{windows: 60, p95: 50, sd: 3, rps: 500, errPerWin: 12})   // 2.4%
	c2 := synth("c", 3, synthSpec{windows: 60, p95: 50, sd: 3, rps: 500, errPerWin: 7})   // 1.4%
	c3 := synth("c", 4, synthSpec{windows: 60, p95: 50, sd: 3, rps: 250, errPerWin: 2.0}) // half the throughput
	if d := delta(t, Compare(b, c), "ALL", "errorRate"); d.Verdict != VerdictRegression || d.Stats.EffectName != "cohens_h" || d.Stats.CILow <= 0 {
		t.Errorf("+1.4pp: %+v %+v", d, d.Stats)
	}
	if d := delta(t, Compare(b, c2), "ALL", "errorRate"); d.Verdict == VerdictRegression {
		t.Errorf("+0.4pp must not be a regression: %+v", d)
	}
	cmp := Compare(b, c3)
	d := delta(t, cmp, "ALL", "rps")
	if !d.Informational || d.Worse || d.Verdict != VerdictRegression || cmp.Regressions != 0 {
		t.Errorf("rps is informational: %+v regressions=%d", d, cmp.Regressions)
	}
	// differing workloads produce a warning
	c3.Workload.VUs = 99
	if cmp := Compare(b, c3); len(cmp.Warnings) == 0 || !strings.Contains(cmp.Warnings[0], "Workloads differ") {
		t.Errorf("%v", cmp.Warnings)
	}
}

func TestCompareDiscardsWarmup(t *testing.T) {
	b := synth("b", 11, synthSpec{windows: 90, p95: 40, sd: 2, rps: 100})
	c := synth("c", 12, synthSpec{windows: 90, p95: 40, sd: 2, rps: 100})
	for i := 0; i < 15; i++ { // cold start: 15 slow seconds
		c.Snapshot.Timeline[i].P95Ms += 200 * math.Exp(-float64(i)/5)
		c.Snapshot.Timeline[i].P50Ms += 100 * math.Exp(-float64(i)/5)
		c.Snapshot.Timeline[i].P99Ms += 300 * math.Exp(-float64(i)/5)
	}
	d := delta(t, Compare(b, c), "ALL", "p95Ms")
	if d.Verdict == VerdictRegression || d.Stats.WarmupCur < 5 {
		t.Errorf("warm-up should be trimmed: %+v %+v", d, d.Stats)
	}
}

// writeRaw writes a JTL-compatible raw.csv with one transaction per request.
func writeRaw(t *testing.T, dir string, seed uint64, secs int, perSec int, txMs float64) {
	t.Helper()
	r := stats.NewRand(seed)
	var b strings.Builder
	b.WriteString("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind\n")
	for s := 0; s < secs; s++ {
		for k := 0; k < perSec; k++ {
			el := int64(math.Max(1, txMs+txMs*0.1*r.NormFloat64()))
			ts := t0.UnixMilli() + int64(s)*1000 + int64(k*1000/perSec) - el
			fmt.Fprintf(&b, "%d,%d,Pay,200,,vu-1,true,,10,10,1,http://127.0.0.1/pay,transaction\n", ts, el)
			fmt.Fprintf(&b, "%d,%d,GET /x,200,,vu-1,true,,10,10,1,http://127.0.0.1/x,request\n", ts, el)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raw.csv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompareTransactionsFromRaw(t *testing.T) {
	root := t.TempDir()
	b := synth("b", 1, synthSpec{windows: 40, p95: 50, sd: 3, rps: 20})
	c := synth("c", 2, synthSpec{windows: 40, p95: 50, sd: 3, rps: 20})
	for _, s := range []*Summary{b, c} {
		s.Snapshot.ByTx = []metrics.Named{{Name: "Pay", MetricSet: metrics.MetricSet{Count: 800, P95Ms: 50}}}
	}
	c.Snapshot.ByTx[0].P95Ms = 80
	bd, cd := filepath.Join(root, "b"), filepath.Join(root, "c")
	writeRaw(t, bd, 1, 40, 20, 50)
	writeRaw(t, cd, 2, 40, 20, 80)
	// without the run folders the transaction cannot be tested
	if d := delta(t, Compare(b, c), "tx:Pay", "p95Ms"); d.Verdict != VerdictInconclusive {
		t.Errorf("%+v", d)
	}
	cmp := CompareWith(b, c, CompareOptions{BaseDir: bd, CurDir: cd})
	d := delta(t, cmp, "tx:Pay", "p95Ms")
	if d.Verdict != VerdictRegression || d.Stats.Source != "raw.csv" || d.Stats.NBase < 20 {
		t.Errorf("%+v %+v", d, d.Stats)
	}
	if v := delta(t, cmp, "ALL", "p95Ms").Verdict; v == VerdictRegression || v == "" {
		t.Errorf("ALL timeline unchanged, verdict %q", v)
	}
	for _, d := range cmp.Deltas {
		if d.Verdict == "" || d.Reason == "" || d.Stats == nil {
			t.Errorf("every delta needs a verdict, reason and stats: %+v", d)
		}
	}
	if cmp.Verdict != VerdictRegression || cmp.Regressions < 1 {
		t.Errorf("%+v", cmp)
	}
}

func TestGateExitCodesAndOutputs(t *testing.T) {
	base := synth("base", 1, synthSpec{windows: 60, p95: 100, sd: 8, rps: 200})
	same := synth("same", 2, synthSpec{windows: 60, p95: 100, sd: 8, rps: 200})
	slow := synth("slow", 3, synthSpec{windows: 60, p95: 140, sd: 8, rps: 200})
	short := synth("short", 4, synthSpec{windows: 5, p95: 100, sd: 8, rps: 200})
	cases := []struct {
		name   string
		cur    *Summary
		base   *Summary
		status string
		code   int
	}{
		{"no change", same, base, GatePass, 0},
		{"regression", slow, base, GateFail, 3},
		{"needs data", short, base, GateInconclusive, 4},
		{"no baseline, SLAs only", same, nil, GatePass, 0},
	}
	for _, tc := range cases {
		g := Gate(tc.cur, tc.base, CompareOptions{})
		if g.Status != tc.status || g.ExitCode != tc.code {
			t.Errorf("%s: %s/%d want %s/%d: %+v", tc.name, g.Status, g.ExitCode, tc.status, tc.code, g.Checks)
		}
	}
	// a failed SLA fails the gate even without a regression
	failing := *same
	failing.SLAs = []SLAResult{{SLA: model.SLA{Metric: "p95", Op: "<", Threshold: 50}, Actual: 101, Passed: false}}
	if g := Gate(&failing, base, CompareOptions{}); g.ExitCode != 3 {
		t.Errorf("SLA fail: %+v", g)
	}
	// failure outranks inconclusive
	failingShort := *short
	failingShort.SLAs = failing.SLAs
	if g := Gate(&failingShort, base, CompareOptions{}); g.ExitCode != 3 {
		t.Errorf("fail+inconclusive: %d", g.ExitCode)
	}
	if g := Gate(&Summary{RunID: "empty"}, nil, CompareOptions{}); g.ExitCode != 4 {
		t.Errorf("empty run: %d", g.ExitCode)
	}

	g := Gate(slow, base, CompareOptions{})
	var buf bytes.Buffer
	if err := g.WriteJUnit(&buf); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Suites   []struct {
			Name  string `xml:"name,attr"`
			Cases []struct {
				Name    string    `xml:"name,attr"`
				Failure *struct{} `xml:"failure"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("junit not parseable: %v\n%s", err, buf.String())
	}
	if parsed.Failures < 1 || parsed.Tests != len(g.Checks) || len(parsed.Suites) != 2 {
		t.Errorf("junit %+v\n%s", parsed, buf.String())
	}
	md := g.Markdown()
	for _, want := range []string{"Load Meter gate: **FAIL**", "| ALL | p95Ms |", "regression", "exit codes 0 pass, 3 fail, 4 inconclusive"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestSuggestSLAs(t *testing.T) {
	a := synth("r1", 1, synthSpec{windows: 60, p95: 100, sd: 10, rps: 100, errPerWin: 0})
	b := synth("r2", 2, synthSpec{windows: 60, p95: 110, sd: 10, rps: 100, errPerWin: 1})
	for _, s := range []*Summary{a, b} {
		s.Snapshot.ByTx = []metrics.Named{{Name: "Pay", MetricSet: metrics.MetricSet{Count: 500, P95Ms: 70}}, {Name: "Rare", MetricSet: metrics.MetricSet{Count: 5, P95Ms: 900}}}
	}
	sg := SuggestSLAs([]*Summary{a, b}, SuggestOptions{MarginPct: 20, IncludeTx: true})
	if !sg.Reliable || len(sg.Runs) != 2 {
		t.Errorf("%+v", sg)
	}
	got := map[string]SuggestedSLA{}
	for _, s := range sg.SLAs {
		got[s.Scope+"/"+s.Metric] = s
	}
	p95 := got["/p95"]
	maxRun := math.Max(a.Snapshot.Requests.P95Ms, b.Snapshot.Requests.P95Ms)
	if p95.Threshold < maxRun*1.2 || p95.Threshold > maxRun*1.6 || p95.Op != "<" || p95.Reasoning == "" || !strings.Contains(p95.Variability, "MAD") {
		t.Errorf("p95 %+v (max run p95 %.1f)", p95, maxRun)
	}
	if e := got["/error_rate"]; e.Threshold < 1 || e.Threshold > 2 { // 0.5% observed: 2 x Wilson upper bound
		t.Errorf("error ceiling %+v", e)
	}
	if _, ok := got["Pay/p95"]; !ok {
		t.Error("transaction suggestion missing")
	}
	if _, ok := got["Rare/p95"]; ok {
		t.Error("rare transaction must be skipped")
	}
	if niceCeil(31.2) != 32 || niceCeil(247) != 250 || niceCeil(0.834) != 0.84 || niceCeil(100) != 100 {
		t.Errorf("niceCeil %v %v %v %v", niceCeil(31.2), niceCeil(247), niceCeil(0.834), niceCeil(100))
	}
	// short run => low confidence
	if sg := SuggestSLAs([]*Summary{synth("x", 1, synthSpec{windows: 10, p95: 10, sd: 1, rps: 10})}, SuggestOptions{}); sg.Reliable {
		t.Error("10 windows should be flagged")
	}
	// merge: add new, keep existing unless replace
	existing := []model.SLA{{Metric: "p95", Op: "<", Threshold: 500}}
	merged, diff := MergeSLAs(existing, sg.SLAs, false)
	if merged[0].Threshold != 500 || len(merged) != len(sg.SLAs) || !strings.HasPrefix(diff[0], "= all p95") {
		t.Errorf("%v %v", merged, diff)
	}
	merged, diff = MergeSLAs(existing, sg.SLAs, true)
	if merged[0].Threshold != p95.Threshold || !strings.HasPrefix(diff[0], "~ all p95 < 500 -> <") {
		t.Errorf("%v %v", merged, diff)
	}
}

func TestDiagnoseChangePoint(t *testing.T) {
	for seed := uint64(1); seed <= 10; seed++ {
		r := stats.NewRand(seed)
		var tl []metrics.WindowOut
		for i := 0; i < 90; i++ {
			p := 40 + 2*r.NormFloat64()
			if i >= 55 {
				p += 30
			}
			tl = append(tl, metrics.WindowOut{T: i, RPS: 100, P95Ms: p, VUs: 20})
		}
		fs := Diagnose(metrics.Snapshot{Requests: metrics.MetricSet{Count: 9000}, Timeline: tl}, nil)
		var f *Finding
		for i := range fs {
			if strings.HasPrefix(fs[i].Title, "P95 latency rose") {
				f = &fs[i]
			}
		}
		if f == nil || f.Interval == nil || math.Abs(float64(f.Interval.ToSec-55)) > 2 || f.Kind != "observation" || !strings.Contains(f.Method, "PELT") {
			t.Fatalf("seed %d: %+v", seed, fs)
		}
		for _, x := range fs {
			if strings.Contains(x.Title, "coincides with higher load") {
				t.Error("VUs constant: no load hypothesis expected")
			}
		}
	}
	// stationary noise: no timeline findings
	r := stats.NewRand(99)
	var tl []metrics.WindowOut
	for i := 0; i < 120; i++ {
		tl = append(tl, metrics.WindowOut{T: i, RPS: 100, P95Ms: 40 + 2*r.NormFloat64(), VUs: 20})
	}
	for _, f := range Diagnose(metrics.Snapshot{Requests: metrics.MetricSet{Count: 12000}, Timeline: tl}, nil) {
		if f.Interval != nil {
			t.Errorf("false timeline finding on noise: %+v", f)
		}
	}
	// step coinciding with a VU increase => hypothesis as well
	for i := range tl {
		if i >= 60 {
			tl[i].P95Ms += 40
			tl[i].VUs = 60
		}
	}
	fs := Diagnose(metrics.Snapshot{Requests: metrics.MetricSet{Count: 12000}, Timeline: tl}, nil)
	found := false
	for _, f := range fs {
		if f.Kind == "hypothesis" && strings.Contains(f.Title, "coincides with higher load") && strings.Contains(f.Evidence, "20 to 60") {
			found = true
		}
	}
	if !found {
		t.Errorf("%+v", fs)
	}
}

func TestDiagnoseDrift(t *testing.T) {
	r := stats.NewRand(5)
	var tl []metrics.WindowOut
	for i := 0; i < 180; i++ { // +0.2 ms/s from 30 ms: +36 ms over the run
		tl = append(tl, metrics.WindowOut{T: i, RPS: 100, P95Ms: 30 + 0.2*float64(i) + 2*r.NormFloat64(), VUs: 20, MemMB: 50 + 0.5*float64(i)})
	}
	fs := Diagnose(metrics.Snapshot{Requests: metrics.MetricSet{Count: 18000}, Timeline: tl}, nil)
	var drift, mem *Finding
	for i := range fs {
		switch {
		case strings.HasPrefix(fs[i].Title, "P95 latency crept up"):
			drift = &fs[i]
		case strings.HasPrefix(fs[i].Title, "Load generator memory grew"):
			mem = &fs[i]
		case strings.Contains(fs[i].Title, "rose at about"):
			t.Errorf("drift reported as a step: %+v", fs[i])
		}
	}
	if drift == nil || !strings.Contains(drift.Method, "Theil-Sen") {
		t.Fatalf("%+v", fs)
	}
	// slope magnitude: +0.2 ms/s = +12 ms/min
	var slope float64
	fmt.Sscanf(drift.Magnitude, "%f", &slope)
	if math.Abs(slope-12) > 1.5 {
		t.Errorf("slope %v ms/min (%q)", slope, drift.Magnitude)
	}
	if mem == nil || !strings.Contains(mem.Magnitude, "+30") {
		t.Errorf("memory drift %+v", mem)
	}
}

var _ = time.Second

// Per-second windows are autocorrelated in real runs. With AR(1) noise
// (phi 0.6) Mann-Whitney alone is too liberal; the block-bootstrap CI and the
// minimum-effect rule must keep false regressions rare.
func TestCompareFalsePositiveRateAutocorrelated(t *testing.T) {
	const trials = 150
	ar := func(id string, seed uint64) *Summary {
		s := synth(id, seed, synthSpec{windows: 60, p95: 50, sd: 0, rps: 100})
		r := stats.NewRand(seed)
		e := 0.0
		for i := range s.Snapshot.Timeline {
			e = 0.6*e + 4*r.NormFloat64()
			w := &s.Snapshot.Timeline[i]
			w.P95Ms, w.P50Ms, w.P99Ms = 50+e, (50+e)*0.5, (50+e)*1.4
		}
		return s
	}
	fp := 0
	for s := uint64(1); s <= trials; s++ {
		if CompareWith(ar("b", s), ar("c", s+77_777), CompareOptions{Resamples: 300}).Regressions > 0 {
			fp++
		}
	}
	if rate := float64(fp) / trials; rate > 0.08 {
		t.Errorf("false regression rate %.3f with autocorrelated identical series", rate)
	}
	t.Logf("AR(1) identical series: %d/%d flagged a regression", fp, trials)
}
