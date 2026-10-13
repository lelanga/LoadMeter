package metrics

import (
	"bytes"
	"encoding/csv"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

func exactQ(sorted []uint64, q float64) float64 {
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return float64(sorted[rank-1])
}

func genData(kind string, n int, r *rand.Rand) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		switch kind {
		case "uniform":
			out[i] = uint64(r.Intn(5_000_000)) + 1000
		case "lognormal":
			out[i] = uint64(math.Exp(r.NormFloat64()*1.2+9)) + 1000
		case "pareto":
			out[i] = uint64(1000 / math.Pow(1-r.Float64(), 1/1.3))
		case "bimodal":
			if r.Intn(10) == 0 {
				out[i] = uint64(2_000_000 + r.Intn(500_000))
			} else {
				out[i] = uint64(5000 + r.Intn(3000))
			}
		}
	}
	return out
}

func TestHistogramPercentilesVsExact(t *testing.T) {
	for _, kind := range []string{"uniform", "lognormal", "pareto", "bimodal"} {
		t.Run(kind, func(t *testing.T) {
			r := rand.New(rand.NewSource(42))
			data := genData(kind, 50000, r)
			h := NewHistogram()
			sp := Sparse{}
			for _, v := range data {
				h.Record(v)
				sp.Record(v)
			}
			sorted := append([]uint64(nil), data...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			for _, q := range []float64{0.5, 0.9, 0.95, 0.99, 0.999} {
				want := exactQ(sorted, q)
				for name, got := range map[string]float64{"hist": float64(h.Quantile(q)), "sparse": float64(sp.Quantile(q))} {
					if rel := math.Abs(got-want) / want; rel > 0.01 {
						t.Errorf("%s q=%v got %v want %v rel=%.4f", name, q, got, want, rel)
					}
				}
			}
			if h.Min != sorted[0] || h.Max != sorted[len(sorted)-1] {
				t.Errorf("min/max %d/%d want %d/%d", h.Min, h.Max, sorted[0], sorted[len(sorted)-1])
			}
		})
	}
}

func TestHistogramSmallAndEmpty(t *testing.T) {
	h := NewHistogram()
	if h.Quantile(0.5) != 0 || h.Mean() != 0 {
		t.Fatal("empty histogram should return zeros")
	}
	if (Sparse{}).Quantile(0.5) != 0 {
		t.Fatal("empty sparse should be 0")
	}
	h.Record(5)
	if got := h.Quantile(0.99); got != 5 {
		t.Fatalf("single value quantile=%d", got)
	}
	h.Record(15)
	if h.Mean() != 10 {
		t.Fatalf("mean=%v", h.Mean())
	}
	if h.Quantile(0) != 5 || h.Quantile(1) != 15 {
		t.Fatalf("q0=%d q1=%d", h.Quantile(0), h.Quantile(1))
	}
	// values beyond the exact range are clamped to [Min,Max]
	h2 := NewHistogram()
	h2.Record(1_000_003)
	if got := h2.Quantile(0.5); got != 1_000_003 {
		t.Fatalf("clamped quantile=%d", got)
	}
}

func TestHistogramMerge(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	a, b, all := NewHistogram(), NewHistogram(), NewHistogram()
	for i := 0; i < 20000; i++ {
		v := uint64(math.Exp(r.NormFloat64()+8)) + 10
		if i%3 == 0 {
			a.Record(v)
		} else {
			b.Record(v)
		}
		all.Record(v)
	}
	a.Merge(b)
	if a.Total != all.Total || a.Min != all.Min || a.Max != all.Max || math.Abs(a.Sum-all.Sum) > 1e-6 {
		t.Fatalf("merge totals differ: %+v vs %+v", a.Total, all.Total)
	}
	for _, q := range []float64{0.5, 0.95, 0.99} {
		if a.Quantile(q) != all.Quantile(q) {
			t.Errorf("q=%v merged %d vs all %d", q, a.Quantile(q), all.Quantile(q))
		}
	}
	// merging an empty histogram must not change Min
	before := a.Min
	a.Merge(NewHistogram())
	if a.Min != before {
		t.Fatalf("merge of empty changed Min %d -> %d", before, a.Min)
	}
}

func req(name string, start time.Time, lat time.Duration, status int, ok bool, ek string) Sample {
	return Sample{Kind: KindRequest, Name: name, URL: "http://x/" + name, Start: start, Latency: lat, TTFB: lat / 2,
		Status: status, OK: ok, ErrKind: ek, ErrMsg: ek, BytesIn: 100, BytesOut: 10, VU: 1, Iteration: 1}
}

func TestCollectorWindowsAndNames(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	c := NewCollector(t0, nil)
	c.Add(req("a", t0, 10*time.Millisecond, 200, true, ""))
	c.Add(req("a", t0.Add(100*time.Millisecond), 20*time.Millisecond, 500, false, "http_5xx"))
	c.Add(req("b", t0.Add(1500*time.Millisecond), 30*time.Millisecond, 0, false, "timeout"))
	c.Add(Sample{Kind: KindTransaction, Name: "tx", Start: t0.Add(2 * time.Second), Latency: 100 * time.Millisecond, OK: true})
	c.Add(Sample{Kind: KindTransaction, Name: "tx", Start: t0.Add(2 * time.Second), Latency: 100 * time.Millisecond, OK: false, ErrKind: "assertion"})
	c.SetGauge(t0.Add(1*time.Second), 5, 12.34, 56.78)
	c.IterationDone(true)
	c.IterationDone(false)
	c.DroppedIteration()
	snap := c.Snapshot(t0.Add(4*time.Second), 0)

	if snap.Requests.Count != 3 || snap.Requests.Errors != 2 {
		t.Fatalf("requests %+v", snap.Requests)
	}
	if snap.Requests.ErrorRate != 66.67 {
		t.Errorf("errorRate=%v", snap.Requests.ErrorRate)
	}
	if snap.Requests.RPS != 0.75 {
		t.Errorf("rps=%v", snap.Requests.RPS)
	}
	if snap.Requests.Status["200"] != 1 || snap.Requests.Status["500"] != 1 || snap.Requests.Status["0"] != 1 {
		t.Errorf("status %+v", snap.Requests.Status)
	}
	if snap.Requests.ErrorKind["http_5xx"] != 1 || snap.Requests.ErrorKind["timeout"] != 1 {
		t.Errorf("errorKinds %+v", snap.Requests.ErrorKind)
	}
	if _, ok := snap.Requests.ErrorKind[""]; ok {
		t.Error("empty error kind must not be reported")
	}
	if snap.Requests.MinMs != 10 || snap.Requests.MaxMs != 30 || math.Abs(snap.Requests.AvgMs-20) > 0.05 {
		t.Errorf("min/avg/max %v %v %v", snap.Requests.MinMs, snap.Requests.AvgMs, snap.Requests.MaxMs)
	}
	if snap.Requests.BytesIn != 300 || snap.Requests.BytesOut != 30 {
		t.Errorf("bytes %d %d", snap.Requests.BytesIn, snap.Requests.BytesOut)
	}
	if snap.Transactions.Count != 2 || snap.Transactions.Errors != 1 || snap.Transactions.ErrorKind["assertion"] != 1 {
		t.Errorf("tx %+v", snap.Transactions)
	}
	if len(snap.ByRequest) != 2 || snap.ByRequest[0].Name != "a" || snap.ByRequest[1].Name != "b" {
		t.Fatalf("byRequest %+v", snap.ByRequest)
	}
	if snap.ByRequest[0].Count != 2 || snap.ByRequest[0].Errors != 1 {
		t.Errorf("a: %+v", snap.ByRequest[0])
	}
	if len(snap.ByTx) != 1 || snap.ByTx[0].Name != "tx" || snap.ByTx[0].Count != 2 {
		t.Errorf("byTx %+v", snap.ByTx)
	}
	if snap.Iterations != 2 || snap.IterFailures != 1 || snap.Dropped != 1 || snap.ActiveVUs != 5 {
		t.Errorf("iters %d fails %d dropped %d vus %d", snap.Iterations, snap.IterFailures, snap.Dropped, snap.ActiveVUs)
	}
	if snap.ElapsedSec != 4 {
		t.Errorf("elapsed=%v", snap.ElapsedSec)
	}
	// windows: 0 (2 reqs), 1 (1 req, gauge), 2 (tx only)
	if len(snap.Timeline) != 3 {
		t.Fatalf("timeline %d: %+v", len(snap.Timeline), snap.Timeline)
	}
	w0, w1, w2 := snap.Timeline[0], snap.Timeline[1], snap.Timeline[2]
	if w0.RPS != 2 || w0.Errors != 1 || w0.ErrorRate != 50 || w0.MaxMs != 20 {
		t.Errorf("w0 %+v", w0)
	}
	if w1.RPS != 1 || w1.VUs != 5 || w1.CPU != 12.3 || w1.MemMB != 56.8 {
		t.Errorf("w1 %+v", w1)
	}
	if w2.TPS != 2 || w2.RPS != 0 {
		t.Errorf("w2 %+v", w2)
	}
	// fromWindow trims the timeline
	if s2 := c.Snapshot(t0.Add(4*time.Second), 2); len(s2.Timeline) != 1 || s2.Timeline[0].T != 2 {
		t.Errorf("fromWindow=2 -> %+v", s2.Timeline)
	}
	if s3 := c.Snapshot(t0.Add(4*time.Second), 99); len(s3.Timeline) != 0 {
		t.Errorf("fromWindow=99 -> %+v", s3.Timeline)
	}
	if s4 := c.Snapshot(t0.Add(4*time.Second), -5); len(s4.Timeline) != 3 {
		t.Errorf("fromWindow=-5 -> %d", len(s4.Timeline))
	}
}

func TestCollectorSnapshotEmpty(t *testing.T) {
	c := NewCollector(time.Now(), nil)
	s := c.Snapshot(time.Now(), 0)
	if s.Requests.Count != 0 || s.Requests.P99Ms != 0 || len(s.Timeline) != 0 {
		t.Fatalf("%+v", s)
	}
	if _, ok := c.Quantile(KindRequest, "", 0.5); ok {
		t.Error("expected no data")
	}
}

func TestCollectorQuantile(t *testing.T) {
	t0 := time.Now()
	c := NewCollector(t0, nil)
	for i := 1; i <= 100; i++ {
		c.Add(req("r", t0, time.Duration(i)*time.Millisecond, 200, true, ""))
	}
	c.Add(Sample{Kind: KindTransaction, Name: "t", Start: t0, Latency: 500 * time.Millisecond, OK: true})
	tests := []struct {
		kind, name string
		q          float64
		want       float64
		ok         bool
	}{
		{KindRequest, "", 0.5, 50, true},
		{KindRequest, "r", 0.95, 95, true},
		{KindRequest, "nope", 0.95, 0, false},
		{KindTransaction, "", 0.5, 500, true},
		{KindTransaction, "t", 0.99, 500, true},
	}
	for _, tc := range tests {
		got, ok := c.Quantile(tc.kind, tc.name, tc.q)
		if ok != tc.ok || (ok && math.Abs(got-tc.want)/tc.want > 0.01) {
			t.Errorf("%v: got %v,%v want %v,%v", tc, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCollectorRawCSV(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	var buf bytes.Buffer
	c := NewCollector(t0, &buf)
	c.Add(req("login", t0, 12*time.Millisecond, 200, true, ""))
	c.Add(Sample{Kind: KindRequest, Name: "bad,name", URL: "http://x/?a=1,2", Start: t0, Latency: 5 * time.Millisecond, Status: 500,
		ErrKind: "http_5xx", ErrMsg: "boom \"quoted\"", VU: 3})
	if buf.Len() != 0 {
		// buffered; nothing guaranteed before Flush, but must not corrupt
		t.Log("raw output already flushed early")
	}
	c.Flush()
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d", len(rows))
	}
	// JTL-compatible leading columns keep their order; HTTP timing columns are appended.
	wantHdr := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind" +
		",Connect,dnsMs,tlsMs,connReused,retries,bodyBytes,grpThreads,allThreads"
	if strings.Join(rows[0], ",") != wantHdr {
		t.Fatalf("header %v", rows[0])
	}
	r1 := rows[1]
	if r1[0] != "1700000000000" || r1[1] != "12" || r1[2] != "login" || r1[3] != "200" || r1[5] != "vu-1" || r1[6] != "true" || r1[8] != "100" || r1[12] != "request" {
		t.Errorf("row1 %v", r1)
	}
	r2 := rows[2]
	if r2[2] != "bad,name" || r2[4] != "http_5xx" || r2[6] != "false" || r2[7] != `boom "quoted"` || r2[11] != "http://x/?a=1,2" || r2[5] != "vu-3" {
		t.Errorf("row2 %v", r2)
	}
	// Flush is safe when no raw writer
	NewCollector(t0, nil).Flush()
}

func TestCollectorConcurrent(t *testing.T) {
	t0 := time.Now()
	c := NewCollector(t0, nil)
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 500; i++ {
				c.Add(req("x", t0, time.Millisecond, 200, true, ""))
				c.IterationDone(true)
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	s := c.Snapshot(t0.Add(time.Second), 0)
	if s.Requests.Count != 4000 || s.Iterations != 4000 {
		t.Fatalf("%d %d", s.Requests.Count, s.Iterations)
	}
}
