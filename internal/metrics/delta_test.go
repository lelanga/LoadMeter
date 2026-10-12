package metrics

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// Merged quantiles must equal the quantiles of one histogram holding all
// samples (bucket-wise merge is exact), and stay within the histogram's
// relative error of the exact sample quantiles.
func TestHistogramMergeExactness(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	a, b, all := NewHistogram(), NewHistogram(), NewHistogram()
	var samples []uint64
	for i := 0; i < 50000; i++ {
		var v uint64
		if i%2 == 0 {
			v = uint64(rnd.ExpFloat64()*5000) + 100 // fast node
			a.Record(v)
		} else {
			v = uint64(rnd.NormFloat64()*20000+80000) + 1 // slow node
			b.Record(v)
		}
		all.Record(v)
		samples = append(samples, v)
	}
	// Round-trip both halves through JSON as the wire would.
	a, b = roundTrip(t, a), roundTrip(t, b)
	m := NewHistogram()
	m.Merge(a)
	m.Merge(b)
	slices.Sort(samples)
	for _, q := range []float64{0.01, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999} {
		if got, want := m.Quantile(q), all.Quantile(q); got != want {
			t.Errorf("q%.3f: merged %d != combined %d", q, got, want)
		}
		exact := samples[int(math.Ceil(q*float64(len(samples))))-1]
		if rel := math.Abs(float64(m.Quantile(q))-float64(exact)) / float64(exact); rel > 0.01 {
			t.Errorf("q%.3f: merged %d vs exact %d (rel err %.4f)", q, m.Quantile(q), exact, rel)
		}
	}
	if m.Total != all.Total || m.Min != all.Min || m.Max != all.Max || math.Abs(m.Sum-all.Sum) > 1e-6*all.Sum {
		t.Errorf("merged totals differ: %+v vs %+v", []any{m.Total, m.Min, m.Max, m.Sum}, []any{all.Total, all.Min, all.Max, all.Sum})
	}
	// Averaging per-node p95s would be wrong; make sure the test data would expose it.
	avg := (a.Quantile(0.95) + b.Quantile(0.95)) / 2
	if avg == m.Quantile(0.95) {
		t.Fatalf("test data does not distinguish merging from averaging")
	}
}

func roundTrip(t *testing.T, h *Histogram) *Histogram {
	t.Helper()
	bs, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var out Histogram
	if err := json.Unmarshal(bs, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestHistogramJSONRejectsBadInput(t *testing.T) {
	for _, in := range []string{`{"n":1,"b":[999999,1]}`, `{"n":2,"b":[5,1]}`, `{"n":1,"b":[5]}`} {
		var h Histogram
		if err := json.Unmarshal([]byte(in), &h); err == nil {
			t.Errorf("%s: expected error", in)
		}
	}
	e := roundTrip(t, NewHistogram())
	if e.Total != 0 || e.Quantile(0.5) != 0 || e.Min != math.MaxUint64 {
		t.Errorf("empty round trip: %+v", e)
	}
}

// Two nodes streaming deltas (over JSON) into a merged collector must give the
// same snapshot as one collector that saw every sample.
func TestDeltaMergeMatchesSingleCollector(t *testing.T) {
	start := time.Now()
	single := NewCollector(start, nil)
	merged := NewCollector(start, nil)
	nodes := []*Collector{NewCollector(start, nil), NewCollector(start, nil)}
	for _, n := range nodes {
		n.TrackDeltas()
	}
	rnd := rand.New(rand.NewPCG(7, 9))
	flush := func() {
		for i, n := range nodes {
			d := n.TakeDelta()
			bs, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var wire Delta
			if err := json.Unmarshal(bs, &wire); err != nil {
				t.Fatal(err)
			}
			if err := merged.MergeDelta([]string{"a", "b"}[i], &wire); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 20000; i++ {
		node := i % 2
		lat := time.Duration(rnd.ExpFloat64()*float64(20*time.Millisecond)) + time.Duration(node)*30*time.Millisecond
		s := Sample{Kind: KindRequest, Name: []string{"home", "search", "buy"}[i%3], Start: start.Add(time.Duration(i) * 400 * time.Microsecond),
			Latency: lat, Status: 200, OK: i%50 != 0, BytesIn: 100}
		if !s.OK {
			s.ErrKind, s.Status = "http_5xx", 503
		}
		single.Add(s)
		nodes[node].Add(s)
		if i%1000 == 999 {
			single.IterationDone(true)
			nodes[node].IterationDone(true)
		}
		if i%3000 == 0 {
			for _, n := range nodes {
				n.SetGauge(s.Start, 5, 10, 20)
			}
			flush()
		}
	}
	flush()
	end := start.Add(10 * time.Second)
	a, b := single.Snapshot(end, 0), merged.Snapshot(end, 0)
	if a.Requests.Count != b.Requests.Count || a.Requests.Errors != b.Requests.Errors || a.Iterations != b.Iterations {
		t.Fatalf("counts differ: %+v vs %+v", a.Requests, b.Requests)
	}
	for _, f := range []func(MetricSet) float64{
		func(m MetricSet) float64 { return m.P50Ms }, func(m MetricSet) float64 { return m.P95Ms },
		func(m MetricSet) float64 { return m.P99Ms }, func(m MetricSet) float64 { return m.MaxMs }, func(m MetricSet) float64 { return m.AvgMs }} {
		if f(a.Requests) != f(b.Requests) {
			t.Errorf("percentile differs: %v vs %v", a.Requests, b.Requests)
		}
	}
	if len(a.ByRequest) != len(b.ByRequest) {
		t.Fatalf("byRequest %d vs %d", len(a.ByRequest), len(b.ByRequest))
	}
	for i := range a.ByRequest {
		if a.ByRequest[i].Count != b.ByRequest[i].Count || a.ByRequest[i].P95Ms != b.ByRequest[i].P95Ms {
			t.Errorf("%s differs", a.ByRequest[i].Name)
		}
	}
	if len(a.Timeline) != len(b.Timeline) {
		t.Fatalf("timeline %d vs %d", len(a.Timeline), len(b.Timeline))
	}
	for i := range a.Timeline {
		if a.Timeline[i].RPS != b.Timeline[i].RPS || a.Timeline[i].P95Ms != b.Timeline[i].P95Ms || a.Timeline[i].Errors != b.Timeline[i].Errors {
			t.Errorf("window %d differs: %+v vs %+v", i, a.Timeline[i], b.Timeline[i])
		}
	}
	// Active VUs are summed across nodes in merged windows.
	if b.Timeline[0].VUs != 10 {
		t.Errorf("merged window VUs = %d, want 10 (5 per node)", b.Timeline[0].VUs)
	}
	if d := nodes[0].TakeDelta(); !d.Empty() {
		t.Errorf("expected empty delta after final flush")
	}
}

func TestMergeDeltaRejectsBadWindows(t *testing.T) {
	c := NewCollector(time.Now(), nil)
	if err := c.MergeDelta("x", &Delta{Windows: []*Window{{T: -1}}}); err == nil {
		t.Error("negative window accepted")
	}
	if err := c.MergeDelta("x", &Delta{Windows: []*Window{{T: 1, Hist: Sparse{60000: 1}}}}); err == nil {
		t.Error("bad bucket accepted")
	}
}
