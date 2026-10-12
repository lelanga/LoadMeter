package results

import (
	"encoding/csv"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
)

var t0 = time.Unix(1_700_000_000, 0)

func addReqs(c *metrics.Collector, name string, n int, base time.Duration, status int, ok bool, kind string) {
	for i := 1; i <= n; i++ {
		c.Add(metrics.Sample{Kind: metrics.KindRequest, Name: name, Start: t0, Latency: time.Duration(i) * base,
			Status: status, OK: ok, ErrKind: kind, BytesIn: 10})
	}
}

// fixture: "fast" 1..100 ms (all OK), "slow" 10..1000 ms (10 errors), tx "Checkout" 1..50 ms.
func fixture() (*metrics.Collector, metrics.Snapshot) {
	c := metrics.NewCollector(t0, nil)
	addReqs(c, "fast", 100, time.Millisecond, 200, true, "")
	addReqs(c, "slow", 100, 10*time.Millisecond, 200, true, "")
	for i := 0; i < 10; i++ {
		c.Add(metrics.Sample{Kind: metrics.KindRequest, Name: "slow", Start: t0, Latency: time.Second, Status: 500, ErrKind: "http_5xx"})
	}
	for i := 1; i <= 50; i++ {
		c.Add(metrics.Sample{Kind: metrics.KindTransaction, Name: "Checkout", Start: t0, Latency: time.Duration(i) * time.Millisecond, OK: true})
	}
	return c, c.Snapshot(t0.Add(10*time.Second), 0)
}

func TestEvaluateSLAs(t *testing.T) {
	c, snap := fixture()
	near := func(a, b float64) bool { return math.Abs(a-b) <= math.Max(0.02*math.Abs(b), 0.02) }
	tests := []struct {
		name   string
		sla    model.SLA
		pass   bool
		actual float64 // 0 = don't check
		note   string
	}{
		{"p95 all lt pass", model.SLA{Metric: "p95", Op: "<", Threshold: 5000}, true, 0, ""},
		{"p95 all lt fail", model.SLA{Metric: "p95", Op: "<", Threshold: 1}, false, 0, ""},
		{"p50 scope request", model.SLA{Metric: "p50", Scope: "fast", Op: "<=", Threshold: 51}, true, 50, ""},
		{"p90 scope request", model.SLA{Metric: "p90", Scope: "fast", Op: ">=", Threshold: 89}, true, 90, ""},
		{"p99 scope request", model.SLA{Metric: "p99", Scope: "fast", Op: "<", Threshold: 98}, false, 99, ""},
		{"p95 scope tx", model.SLA{Metric: "p95", Scope: "Checkout", Op: "<", Threshold: 60}, true, 48, ""},
		{"avg scope", model.SLA{Metric: "avg", Scope: "fast", Op: "<", Threshold: 51}, true, 50.5, ""},
		{"max scope", model.SLA{Metric: "max", Scope: "fast", Op: "<=", Threshold: 100}, true, 100, ""},
		{"max slow", model.SLA{Metric: "max", Scope: "slow", Op: "<", Threshold: 500}, false, 1000, ""},
		{"error_rate scope", model.SLA{Metric: "error_rate", Scope: "slow", Op: "<", Threshold: 5}, false, 9.09, ""},
		{"error_rate scope pass", model.SLA{Metric: "error_rate", Scope: "fast", Op: "<", Threshold: 0.1}, true, 0, ""},
		{"error_rate all", model.SLA{Metric: "error_rate", Op: "<", Threshold: 5}, true, 4.76, ""},
		{"rps all", model.SLA{Metric: "rps", Op: ">", Threshold: 10}, true, 21, ""},
		{"rps fail", model.SLA{Metric: "rps", Op: ">=", Threshold: 1000}, false, 21, ""},
		{"missing scope", model.SLA{Metric: "p95", Scope: "nope", Op: "<", Threshold: 1e9}, false, 0, "no samples for scope nope"},
		{"unknown metric", model.SLA{Metric: "p42", Op: "<", Threshold: 1e9}, false, 0, ""},
		{"bogus metric", model.SLA{Metric: "median", Op: "<", Threshold: 1e9}, false, 0, "unknown metric"},
		{"bad op", model.SLA{Metric: "p95", Op: "=", Threshold: 1e9}, false, 0, ""},
		{"strict lt equal", model.SLA{Metric: "max", Scope: "fast", Op: "<", Threshold: 100}, false, 100, ""},
		{"strict gt equal", model.SLA{Metric: "max", Scope: "fast", Op: ">", Threshold: 100}, false, 100, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, all := EvaluateSLAs([]model.SLA{tc.sla}, c, snap)
			if len(res) != 1 {
				t.Fatalf("results %d", len(res))
			}
			r := res[0]
			if r.Passed != tc.pass || all != tc.pass {
				t.Errorf("passed=%v all=%v want %v (actual %v, note %q)", r.Passed, all, tc.pass, r.Actual, r.Note)
			}
			if tc.actual != 0 && !near(r.Actual, tc.actual) {
				t.Errorf("actual=%v want ~%v", r.Actual, tc.actual)
			}
			if tc.note != "" && r.Note != tc.note {
				t.Errorf("note=%q want %q", r.Note, tc.note)
			}
			if r.Metric != tc.sla.Metric || r.Threshold != tc.sla.Threshold {
				t.Errorf("SLA fields not carried: %+v", r.SLA)
			}
		})
	}
}

func TestEvaluateSLAsAggregation(t *testing.T) {
	c, snap := fixture()
	slas := []model.SLA{
		{Metric: "p95", Scope: "fast", Op: "<", Threshold: 1000},
		{Metric: "max", Scope: "slow", Op: "<", Threshold: 1},
		{Metric: "avg", Scope: "fast", Op: "<", Threshold: 1000},
	}
	res, all := EvaluateSLAs(slas, c, snap)
	if all || len(res) != 3 || !res[0].Passed || res[1].Passed || !res[2].Passed {
		t.Errorf("all=%v res=%+v", all, res)
	}
	res, all = EvaluateSLAs(nil, c, snap)
	if !all || len(res) != 0 {
		t.Errorf("no SLAs => pass: %v %v", all, res)
	}
	// no samples at all: scope "" fails with a note
	empty := metrics.NewCollector(t0, nil)
	res, all = EvaluateSLAs([]model.SLA{{Metric: "p95", Op: "<", Threshold: 10}}, empty, empty.Snapshot(t0, 0))
	if all || res[0].Note == "" {
		t.Errorf("empty run must fail SLAs: %+v", res)
	}
}

func TestDiagnoseFindings(t *testing.T) {
	has := func(fs []Finding, sub string) *Finding {
		for i := range fs {
			if strings.Contains(fs[i].Title, sub) {
				return &fs[i]
			}
		}
		return nil
	}
	t.Run("no requests", func(t *testing.T) {
		f := Diagnose(metrics.Snapshot{}, nil)
		if len(f) != 1 || f[0].Severity != "critical" || !strings.Contains(f[0].Title, "No requests") {
			t.Errorf("%+v", f)
		}
	})
	t.Run("healthy run only has info", func(t *testing.T) {
		c := metrics.NewCollector(t0, nil)
		addReqs(c, "ok", 20, time.Millisecond, 200, true, "")
		f := Diagnose(c.Snapshot(t0.Add(time.Second), 0), nil)
		for _, x := range f {
			if x.Severity != "info" {
				t.Errorf("unexpected finding %+v", x)
			}
		}
		if has(f, "Highest-latency request: ok") == nil {
			t.Errorf("slowest request not reported: %+v", f)
		}
	})
	t.Run("errors warning vs critical", func(t *testing.T) {
		mk := func(rate float64) metrics.Snapshot {
			return metrics.Snapshot{Requests: metrics.MetricSet{Count: 100, Errors: uint64(rate), ErrorRate: rate,
				ErrorKind: map[string]uint64{"timeout": 2, "http_5xx": 1}}}
		}
		f := has(Diagnose(mk(2), nil), "Error rate 2.00%")
		if f == nil || f.Severity != "warning" || !strings.Contains(f.Evidence, "http_5xx=1, timeout=2") {
			t.Errorf("%+v", f)
		}
		f = has(Diagnose(mk(7), nil), "Error rate 7.00%")
		if f == nil || f.Severity != "critical" || f.Kind != "observation" {
			t.Errorf("%+v", f)
		}
	})
	t.Run("401 hypothesis", func(t *testing.T) {
		s := metrics.Snapshot{Requests: metrics.MetricSet{Count: 10, Errors: 3, ErrorRate: 30, Status: map[string]uint64{"200": 7, "401": 2, "403": 1}}}
		f := has(Diagnose(s, nil), "authentication or correlation")
		if f == nil || f.Kind != "hypothesis" || !strings.Contains(f.Evidence, "3 responses") {
			t.Errorf("%+v", f)
		}
		s.Requests.Status = map[string]uint64{"200": 10}
		if has(Diagnose(s, nil), "authentication") != nil {
			t.Error("no 401 => no hypothesis")
		}
	})
	t.Run("blocked by policy", func(t *testing.T) {
		s := metrics.Snapshot{Requests: metrics.MetricSet{Count: 4, Errors: 4, ErrorRate: 100, ErrorKind: map[string]uint64{"blocked_by_policy": 4}}}
		f := has(Diagnose(s, nil), "blocked by the safety allowlist")
		if f == nil || f.Severity != "critical" {
			t.Errorf("%+v", f)
		}
	})
	t.Run("dropped iterations", func(t *testing.T) {
		s := metrics.Snapshot{Requests: metrics.MetricSet{Count: 10}, Dropped: 77}
		f := has(Diagnose(s, nil), "Iterations dropped")
		if f == nil || !strings.Contains(f.Evidence, "77") {
			t.Errorf("%+v", f)
		}
	})
	t.Run("errors rise with load", func(t *testing.T) {
		var tl []metrics.WindowOut
		for i := 0; i < 8; i++ {
			w := metrics.WindowOut{T: i, RPS: 100, VUs: 5}
			if i >= 4 {
				w.VUs, w.Errors = 50, 30
			}
			tl = append(tl, w)
		}
		s := metrics.Snapshot{Requests: metrics.MetricSet{Count: 800, Errors: 120, ErrorRate: 15}, Timeline: tl}
		if has(Diagnose(s, nil), "Errors increased as load increased") == nil {
			t.Error("missing finding")
		}
		for i := range tl {
			tl[i].VUs = 5 // same load => no finding
		}
		if has(Diagnose(s, nil), "Errors increased") != nil {
			t.Error("load did not increase => no finding")
		}
	})
	t.Run("generator saturation", func(t *testing.T) {
		tl := make([]metrics.WindowOut, 6)
		for i := range tl {
			tl[i] = metrics.WindowOut{T: i, RPS: 10, VUs: 1, CPU: 1e6}
		}
		f := has(Diagnose(metrics.Snapshot{Requests: metrics.MetricSet{Count: 60}, Timeline: tl}, nil), "Load generator may be a bottleneck")
		if f == nil || f.Kind != "hypothesis" {
			t.Errorf("%+v", f)
		}
	})
	t.Run("SLA breach", func(t *testing.T) {
		slas := []SLAResult{{SLA: model.SLA{Metric: "p95", Op: "<", Threshold: 100}, Actual: 250, Passed: false},
			{SLA: model.SLA{Metric: "avg", Scope: "login", Op: "<", Threshold: 1}, Actual: 0.5, Passed: true},
			{SLA: model.SLA{Metric: "max", Scope: "pay", Op: "<", Threshold: 5}, Actual: 9, Passed: false}}
		fs := Diagnose(metrics.Snapshot{Requests: metrics.MetricSet{Count: 10}}, slas)
		a, b := has(fs, "SLA breached: all requests p95 < 100"), has(fs, "SLA breached: pay max < 5")
		if a == nil || b == nil || a.Severity != "critical" {
			t.Errorf("%+v", fs)
		}
		if has(fs, "login") != nil {
			t.Error("passed SLA reported as breach")
		}
	})
	t.Run("slowest needs minimum samples", func(t *testing.T) {
		s := metrics.Snapshot{Requests: metrics.MetricSet{Count: 10}, ByRequest: []metrics.Named{
			{Name: "rare", MetricSet: metrics.MetricSet{Count: 2, P95Ms: 9999}},
			{Name: "common", MetricSet: metrics.MetricSet{Count: 8, P95Ms: 50}}}}
		if has(Diagnose(s, nil), "Highest-latency request: common") == nil {
			t.Error("rare request with <5 samples must be ignored")
		}
	})
}

// TestCompareLegacyShape: the old fixed 10%/5 ms rule is gone (see
// compare_test.go for the statistical verdicts). Summaries without a
// timeline or request counts cannot be tested, so every metric is
// inconclusive and nothing is counted as a regression; the old JSON fields
// (deltas, changePercent, worse, regressions) keep their shape.
func TestCompareLegacyShape(t *testing.T) {
	sw := func(id string, p50, p95, p99, errRate, rps float64, tx map[string][3]float64) *Summary {
		s := &Summary{RunID: id}
		s.Snapshot.Requests = metrics.MetricSet{P50Ms: p50, P95Ms: p95, P99Ms: p99, ErrorRate: errRate, RPS: rps}
		for n, v := range tx {
			s.Snapshot.ByTx = append(s.Snapshot.ByTx, metrics.Named{Name: n, MetricSet: metrics.MetricSet{P50Ms: v[0], P95Ms: v[1], P99Ms: v[2]}})
		}
		return s
	}
	c := Compare(sw("b", 10, 100, 200, 0, 50, nil), sw("c", 10, 150, 200, 0, 50, nil))
	if c.BaselineRun != "b" || c.Regressions != 0 || c.Verdict != VerdictInconclusive {
		t.Errorf("%+v", c)
	}
	var p95 *Delta
	for i := range c.Deltas {
		if c.Deltas[i].Name == "ALL" && c.Deltas[i].Metric == "p95Ms" {
			p95 = &c.Deltas[i]
		}
		if c.Deltas[i].Worse {
			t.Errorf("no data => never worse: %+v", c.Deltas[i])
		}
	}
	if p95 == nil || p95.Change != 50 || p95.Baseline != 100 || p95.Current != 150 || p95.Verdict != VerdictInconclusive || !strings.Contains(p95.Reason, "usable windows") {
		t.Errorf("%+v", p95)
	}
	// tx only in current is not compared; ALL always present (5 metrics)
	c = Compare(sw("b", 1, 1, 1, 0, 1, nil), sw("c", 1, 1, 1, 0, 1, map[string][3]float64{"X": {1, 1, 1}}))
	if len(c.Deltas) != 5 {
		t.Errorf("deltas=%d", len(c.Deltas))
	}
	// zero baseline => no division by zero / NaN
	c = Compare(sw("b", 0, 0, 0, 0, 0, nil), sw("c", 10, 10, 10, 0, 1, nil))
	for _, d := range c.Deltas {
		if math.IsNaN(d.Change) || math.IsInf(d.Change, 0) {
			t.Errorf("bad change %+v", d)
		}
	}
	b, _ := json.Marshal(c)
	for _, k := range []string{`"baselineRun"`, `"deltas"`, `"changePercent"`, `"worse"`, `"regressions"`, `"note"`, `"verdict"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json lacks %s", k)
		}
	}
}

func TestNewRunID(t *testing.T) {
	re := regexp.MustCompile(`^\d{8}-\d{6}-[A-Za-z0-9-]*$`)
	for _, name := range []string{"Shop Login", "../../etc/passwd", "ünïcode test", "", strings.Repeat("a", 200), "a/b\\c"} {
		id := NewRunID(name)
		if !re.MatchString(id) {
			t.Errorf("NewRunID(%q)=%q", name, id)
		}
		if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
			t.Errorf("unsafe id %q", id)
		}
		if len(id) > 16+5+40 { // timestamp, random suffix, name
			t.Errorf("id too long: %d", len(id))
		}
	}
	if id := NewRunID("My Test!!"); !strings.HasSuffix(id, "-My-Test") {
		t.Errorf("trailing dashes: %q", id)
	}
}

func buildSummary() *Summary {
	c, _ := fixture()
	sc := model.NewScenario("Shop")
	sc.SLAs = []model.SLA{{Metric: "p95", Scope: "fast", Op: "<", Threshold: 500}, {Metric: "error_rate", Op: "<", Threshold: 1}}
	sc.DataSources = []model.DataSource{
		{Name: "users", File: "users.csv", Mode: "unique", OnExhaust: "stop", Scope: "vu"},
		{Name: "inline", Rows: [][]string{{"a"}, {"b"}}},
	}
	c.IterationDone(true)
	c.DroppedIteration()
	return Build("20240101-000000-shop", sc, c, t0, t0.Add(10*time.Second), "finished")
}

func TestBuild(t *testing.T) {
	s := buildSummary()
	if s.RunID != "20240101-000000-shop" || s.Scenario != "Shop" || s.Status != "finished" || s.DurationS != 10 {
		t.Errorf("%+v", s)
	}
	if len(s.SLAs) != 2 || !s.SLAs[0].Passed || s.SLAs[1].Passed || s.SLAPassed {
		t.Errorf("slas %+v pass=%v", s.SLAs, s.SLAPassed)
	}
	if len(s.DataConfig) != 2 ||
		s.DataConfig[0] != "users: mode=unique onExhaust=stop scope=vu rows=file users.csv" ||
		s.DataConfig[1] != "inline: mode=sequential onExhaust=recycle scope=iteration rows=2 inline" {
		t.Errorf("dataConfig %q", s.DataConfig)
	}
	if s.Snapshot.Requests.Count != 210 || s.Snapshot.Dropped != 1 || s.Snapshot.Iterations != 1 {
		t.Errorf("snapshot %+v", s.Snapshot.Requests)
	}
	if len(s.Findings) == 0 {
		t.Error("expected findings (SLA breach, dropped, errors)")
	}
	if s.Env.GoVersion == "" || s.Env.CPUs < 1 || s.Env.LoadMeter == "" {
		t.Errorf("env %+v", s.Env)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := buildSummary()
	s.Notes = []string{"n1"}
	s.Baseline = &Comparison{BaselineRun: "x", Regressions: 2, Deltas: []Delta{{Name: "ALL", Metric: "p95Ms", Baseline: 1, Current: 2, Change: 100, Worse: true}}}
	dir := filepath.Join(t.TempDir(), "runs", "r1") // nested: Save must create it
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"summary.json", "summary.csv"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(s)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Errorf("round trip differs:\n%s\n%s", a, b)
	}
	if got.Snapshot.Requests.P95Ms != s.Snapshot.Requests.P95Ms || got.Baseline == nil || got.Baseline.Regressions != 2 || !got.StartedAt.Equal(s.StartedAt) {
		t.Errorf("fields lost: %+v", got)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Error("missing summary must error")
	}
	os.WriteFile(filepath.Join(dir, "summary.json"), []byte("{not json"), 0o600)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("corrupt summary error should name the dir: %v", err)
	}
	if err := Save(filepath.Join(dir, "summary.json", "sub"), &Summary{}); err == nil {
		t.Error("Save into a path under a file must error")
	}
}

func TestWriteCSV(t *testing.T) {
	s := buildSummary()
	path := filepath.Join(t.TempDir(), "s.csv")
	if err := WriteCSV(path, s); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rows[0], ",") != "kind,name,count,errors,errorRatePct,rps,minMs,avgMs,p50Ms,p90Ms,p95Ms,p99Ms,maxMs,bytesIn,bytesOut" {
		t.Errorf("header %v", rows[0])
	}
	// all + 1 tx + 2 requests
	if len(rows) != 5 {
		t.Fatalf("rows=%d: %v", len(rows), rows)
	}
	if rows[1][0] != "all" || rows[1][1] != "ALL REQUESTS" || rows[1][2] != "210" || rows[1][3] != "10" {
		t.Errorf("all row %v", rows[1])
	}
	kinds := []string{rows[2][0] + ":" + rows[2][1], rows[3][0] + ":" + rows[3][1], rows[4][0] + ":" + rows[4][1]}
	if strings.Join(kinds, "|") != "transaction:Checkout|request:fast|request:slow" {
		t.Errorf("order %v", kinds)
	}
	for _, r := range rows[1:] {
		if len(r) != 15 {
			t.Errorf("row width %d", len(r))
		}
	}
	if err := WriteCSV(filepath.Join(t.TempDir(), "no", "such", "dir.csv"), s); err == nil {
		t.Error("expected error for bad path")
	}
}
