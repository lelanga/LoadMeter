package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/results"
)

// AnalyzeRun triages a finished run directory using its summary. When raw.csv
// is missing it falls back to a coarse result built from the summary's error
// counts (Result.Coarse is then true); clusters have no messages or timing.
func AnalyzeRun(dir string, sum *results.Summary) (*Result, error) {
	f, err := os.Open(filepath.Join(dir, "raw.csv"))
	if errors.Is(err, os.ErrNotExist) {
		return coarse(sum), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var key cacheKey
	if st, err := f.Stat(); err == nil {
		key = cacheKey{f.Name(), st.Size(), st.ModTime().UnixNano(), sum.RunID}
		if r := cacheGet(key); r != nil {
			return r, nil
		}
	}
	opt := OptionsFromRun(dir, sum)
	res, err := Analyze(f, opt)
	if err != nil {
		return nil, err
	}
	res.RunID = sum.RunID
	addSignals(res, sum)
	cachePut(key, res)
	return res, nil
}

// A finished run's raw.csv never changes, so recent results are kept (the
// result is read-only; use Top to get a trimmed copy).
type cacheKey struct {
	path  string
	size  int64
	mtime int64
	run   string
}

var (
	cacheMu sync.Mutex
	cache   = map[cacheKey]*Result{}
)

func cacheGet(k cacheKey) *Result {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return cache[k]
}

func cachePut(k cacheKey, r *Result) {
	if k.path == "" {
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if len(cache) >= 8 {
		cache = map[cacheKey]*Result{}
	}
	cache[k] = r
}

type stepJSON struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Request *struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	} `json:"request"`
	Steps []stepJSON `json:"steps"`
}

// OptionsFromRun derives Analyze options from the run's summary and its
// scenario.json (critical requests, transaction membership, change points).
func OptionsFromRun(dir string, sum *results.Summary) Options {
	opt := Options{}
	if sum == nil {
		return opt
	}
	opt.Start, opt.DurationSec = sum.StartedAt, sum.DurationS
	tl := sum.Snapshot.Timeline
	opt.ChangePoints = ChangePoints(tl)
	cpus := sum.Env.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	for _, w := range tl {
		for len(opt.GeneratorBusy) <= w.T {
			opt.GeneratorBusy = append(opt.GeneratorBusy, false)
		}
		opt.GeneratorBusy[w.T] = w.CPU > float64(cpus)*100*0.85
	}
	// scenario structure
	var sc struct {
		Steps []stepJSON `json:"steps"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "scenario.json")); err == nil && json.Unmarshal(b, &sc) == nil {
		txReq := map[string][]string{} // transaction -> request names
		opt.TxOf = map[string]string{}
		var walk func(steps []stepJSON, tx string)
		walk = func(steps []stepJSON, tx string) {
			for _, st := range steps {
				switch {
				case st.Type == "transaction":
					walk(st.Steps, st.Name)
				case st.Type == "request" && st.Request != nil:
					name := st.Name
					if name == "" {
						name = st.Request.Method + " " + st.Request.URL
					}
					if tx != "" {
						opt.TxOf[name] = tx
						txReq[tx] = append(txReq[tx], name)
					}
				default:
					walk(st.Steps, tx)
				}
			}
		}
		walk(sc.Steps, "")
		opt.Critical = map[string]bool{}
		scoped := false
		for _, s := range sum.SLAs {
			if s.Scope == "" {
				continue
			}
			scoped = true
			opt.Critical[s.Scope] = true
			for _, r := range txReq[s.Scope] {
				opt.Critical[r] = true
			}
		}
		if !scoped { // no scoped SLA: anything inside a named transaction counts as critical
			for r := range opt.TxOf {
				opt.Critical[r] = true
			}
		}
	} else {
		opt.Critical = map[string]bool{}
		for _, s := range sum.SLAs {
			if s.Scope != "" {
				opt.Critical[s.Scope] = true
			}
		}
	}
	return opt
}

// ChangePoints finds, deterministically, the second at which the error rate
// and the p95 latency shifted upward, using a single best-split mean-shift
// search (maximising |mean2-mean1| x sqrt(k(n-k)/n)). A shift is reported only
// when it is clearly material: error rate at least doubles and rises by 1+
// percentage points; p95 rises by 50% and at least 20 ms. Runs shorter than
// 8 seconds have none.
func ChangePoints(tl []metrics.WindowOut) []float64 {
	if len(tl) < 8 {
		return nil
	}
	errs := make([]float64, len(tl))
	lat := make([]float64, len(tl))
	for i, w := range tl {
		errs[i], lat[i] = w.ErrorRate, w.P95Ms
	}
	var out []float64
	if k := bestSplit(errs, 2, 1); k > 0 {
		out = append(out, float64(tl[k].T))
	}
	if k := bestSplit(lat, 1.5, 20); k > 0 {
		t := float64(tl[k].T)
		dup := false
		for _, o := range out {
			dup = dup || math.Abs(o-t) < 1
		}
		if !dup {
			out = append(out, t)
		}
	}
	sort.Float64s(out)
	return out
}

func bestSplit(x []float64, ratio, minRise float64) int {
	n := len(x)
	pre := make([]float64, n+1)
	for i, v := range x {
		pre[i+1] = pre[i] + v
	}
	best, bestScore := -1, 0.0
	for k := 3; k <= n-3; k++ {
		m1 := pre[k] / float64(k)
		m2 := (pre[n] - pre[k]) / float64(n-k)
		if m2-m1 < minRise || m2 < ratio*m1 {
			continue
		}
		if s := (m2 - m1) * math.Sqrt(float64(k)*float64(n-k)/float64(n)); s > bestScore {
			best, bestScore = k, s
		}
	}
	return best
}

func addSignals(res *Result, sum *results.Summary) {
	if sum.Snapshot.Dropped > 0 {
		res.Signals = append(res.Signals, Signal{SideGenerator, fmt.Sprintf("%d scheduled iterations were dropped because every virtual user was busy; this is a limit of the test setup, not a target error.", sum.Snapshot.Dropped)})
	}
	cpus := max(sum.Env.CPUs, 1)
	hi := 0
	for _, w := range sum.Snapshot.Timeline {
		if w.CPU > float64(cpus)*100*0.85 {
			hi++
		}
	}
	if hi > 0 {
		res.Signals = append(res.Signals, Signal{SideGenerator, fmt.Sprintf("Generator CPU was above 85%% of its cores in %d of %d seconds; latency and timeouts in those seconds may include generator queuing.", hi, len(sum.Snapshot.Timeline))})
	}
}

// coarse builds clusters from the summary's per-request error kinds when raw
// samples are not available.
func coarse(sum *results.Summary) *Result {
	res := &Result{RunID: sum.RunID, Coarse: true, SideCounts: map[string]uint64{}, DurationSec: sum.DurationS,
		TotalRequests: sum.Snapshot.Requests.Count, TotalFailures: sum.Snapshot.Requests.Errors,
		Note: "No raw.csv for this run, so failures are grouped only by request and error kind (no messages, timing or sub-grouping)."}
	opt := Options{Critical: map[string]bool{}, DurationSec: sum.DurationS}
	for _, s := range sum.SLAs {
		if s.Scope != "" {
			opt.Critical[s.Scope] = true
		}
	}
	var order []*Cluster
	for _, n := range sum.Snapshot.ByRequest {
		kinds := make([]string, 0, len(n.ErrorKind))
		for k := range n.ErrorKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			c := &Cluster{Template: "(" + k + ", message not recorded)", ErrKind: k, Request: n.Name, Count: n.ErrorKind[k]}
			c.ID = clusterID(&group{tokens: []string{c.Template}}, 0, k, n.Name)
			c.Example = Example{Message: "(not recorded)"}
			order = append(order, c)
		}
	}
	dur := sum.DurationS
	finish(res, order, opt, dur)
	for i := range res.Clusters {
		res.Clusters[i].Parts.Early = 0 // timing unknown: keep the score honest
		res.Clusters[i].Priority = int(math.Round(res.Clusters[i].Parts.Share + res.Clusters[i].Parts.Critical))
	}
	sort.SliceStable(res.Clusters, func(i, j int) bool {
		a, b := res.Clusters[i], res.Clusters[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.Count > b.Count
	})
	for i := range res.Clusters {
		res.Clusters[i].Rank = i + 1
	}
	addSignals(res, sum)
	return res
}
