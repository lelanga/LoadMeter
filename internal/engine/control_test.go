package engine

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// pathServer counts requests per path (query ignored) and answers with the
// handler's status and body.
type pathServer struct {
	mu     sync.Mutex
	hits   map[string]int
	order  []string
	handle func(w http.ResponseWriter, r *http.Request, n int) // n = 1-based hit count of the path
}

func newPathServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) (*pathServer, string) {
	t.Helper()
	ps := &pathServer{hits: map[string]int{}, handle: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		ps.mu.Lock()
		ps.hits[key]++
		n := ps.hits[key]
		ps.order = append(ps.order, r.URL.RequestURI())
		ps.mu.Unlock()
		if ps.handle != nil {
			ps.handle(w, r, n)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return ps, srv.URL
}

func (ps *pathServer) count(p string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.hits[p]
}

func (ps *pathServer) requests() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]string(nil), ps.order...)
}

func ctl(typ, name string, c model.StepControl, children ...model.Step) model.Step {
	return model.Step{Type: typ, Name: name, StepControl: c, Steps: children}
}

func setStep(kv ...string) model.Step {
	st := model.Step{Type: model.StepSetVar}
	for i := 0; i+1 < len(kv); i += 2 {
		st.Set = append(st.Set, model.KV{Name: kv[i], Value: kv[i+1]})
	}
	return st
}

func TestWhileStopsWhenConditionTurnsFalse(t *testing.T) {
	ps, base := newPathServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n >= 3 {
			_, _ = w.Write([]byte("state=stop"))
			return
		}
		_, _ = w.Write([]byte("state=go"))
	})
	poll := withExtract(get("poll", "/poll"), model.Extractor{Name: "state", Type: model.ExRegex, Expr: `state=(\w+)`})
	w := ctl(model.StepWhile, "wait", model.StepControl{}, poll)
	w.Cond = "${state} != stop"
	sc := newSc(base, perVU(1, 1), setStep("state", "go"), w)
	runSc(t, sc, opts(nil))
	if got := ps.count("/poll"); got != 3 {
		t.Fatalf("while ran %d passes, want 3", got)
	}
}

func TestWhileUnresolvedConditionIsFalse(t *testing.T) {
	ps, base := newPathServer(t, nil)
	w := ctl(model.StepWhile, "typo", model.StepControl{}, get("x", "/x"))
	w.Cond = "${nosuchvar} != done" // JMeter would loop forever here
	runSc(t, newSc(base, perVU(1, 2), w), opts(nil))
	if got := ps.count("/x"); got != 0 {
		t.Fatalf("unresolved condition must be false; ran %d passes", got)
	}
}

func TestWhileMaxIterationsCap(t *testing.T) {
	ps, base := newPathServer(t, nil)
	w := ctl(model.StepWhile, "forever", model.StepControl{MaxIterations: 7}, get("x", "/x"))
	w.Cond = "${flag}"
	sc := newSc(base, perVU(2, 3), setStep("flag", "yes"), w)
	runSc(t, sc, opts(nil))
	if got := ps.count("/x"); got != 2*3*7 {
		t.Fatalf("capped while ran %d requests, want %d", got, 2*3*7)
	}
}

func TestWhileEmptyConditionRunsUntilLastSampleFails(t *testing.T) {
	ps, base := newPathServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 4 {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	w := ctl(model.StepWhile, "until-fail", model.StepControl{}, get("x", "/x"))
	runSc(t, newSc(base, perVU(1, 1), w), opts(nil))
	if got := ps.count("/x"); got != 4 {
		t.Fatalf("empty-condition while ran %d passes, want 4 (stop after the failing 4th)", got)
	}
	// LAST also checks the sample before the loop: a failed request before
	// the loop means zero passes.
	ps2, base2 := newPathServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
		}
	})
	last := ctl(model.StepWhile, "last", model.StepControl{}, get("y", "/y"))
	last.Cond = "LAST"
	runSc(t, newSc(base2, perVU(1, 1), get("f", "/fail"), last), opts(nil))
	if got := ps2.count("/y"); got != 0 {
		t.Fatalf("LAST after a failed sample ran %d passes, want 0", got)
	}
}

func TestWhileHonoursDurationAndCancellation(t *testing.T) {
	_, base := newPathServer(t, nil)
	// A wall-clock cap on the loop itself.
	w := ctl(model.StepWhile, "spin", model.StepControl{MaxIterations: model.HardMaxIterations, Duration: dur(150 * time.Millisecond)},
		model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(10 * time.Millisecond)}})
	w.Cond = "${flag}"
	start := time.Now()
	runSc(t, newSc(base, perVU(1, 1), setStep("flag", "1"), w), opts(nil))
	if el := time.Since(start); el > 2*time.Second || el < 140*time.Millisecond {
		t.Fatalf("while duration cap not honoured: %s", el)
	}
	// The run's duration ends an endless-looking loop (graceful stop).
	w2 := ctl(model.StepWhile, "spin", model.StepControl{MaxIterations: model.HardMaxIterations}, get("x", "/x"))
	w2.Cond = "${flag}"
	sc := newSc(base, constVUs(2, 200*time.Millisecond), setStep("flag", "1"), w2)
	start = time.Now()
	runSc(t, sc, opts(nil))
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("graceful stop did not end the loop: %s", el)
	}
	// Hard cancellation via the context.
	r, err := Prepare(newSc(base, perVU(1, 1), setStep("flag", "1"), w2), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	waitDone(t, r, 3*time.Second)
}

func TestForeachReadsNumberedVariables(t *testing.T) {
	ps, base := newPathServer(t, nil)
	fe := ctl(model.StepForeach, "each", model.StepControl{Input: "id", Var: "cur"}, get("item", "/item/${cur}"))
	sc := newSc(base, perVU(1, 1), setStep("id_1", "a", "id_2", "b", "id_3", "c", "id_5", "skipped"), fe)
	runSc(t, sc, opts(nil))
	want := []string{"/item/a", "/item/b", "/item/c"}
	if got := ps.requests(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("foreach requests %v, want %v (stops at the first missing index)", got, want)
	}
	// start/end window and no separator.
	ps2, base2 := newPathServer(t, nil)
	fe2 := ctl(model.StepForeach, "win", model.StepControl{Input: "v", Var: "cur", Start: 1, End: 3, NoSeparator: true}, get("item", "/i/${cur}"))
	sc2 := newSc(base2, perVU(1, 1), setStep("v1", "1", "v2", "2", "v3", "3", "v4", "4"), fe2)
	runSc(t, sc2, opts(nil))
	if got := ps2.requests(); strings.Join(got, ",") != "/i/2,/i/3" {
		t.Fatalf("foreach window got %v", got)
	}
	// maxIterations caps a long list.
	ps3, base3 := newPathServer(t, nil)
	kv := []string{}
	for i := 1; i <= 20; i++ {
		kv = append(kv, "n_"+strconv.Itoa(i), "x")
	}
	fe3 := ctl(model.StepForeach, "cap", model.StepControl{Input: "n", Var: "cur", MaxIterations: 5}, get("item", "/c"))
	runSc(t, newSc(base3, perVU(1, 1), setStep(kv...), fe3), opts(nil))
	if got := ps3.count("/c"); got != 5 {
		t.Fatalf("foreach cap ran %d, want 5", got)
	}
}

func TestSwitchSelection(t *testing.T) {
	cases := []struct {
		value string
		want  string // path hit, "" = nothing
	}{
		{"", "/a"},
		{"1", "/b"},  // index counts enabled children only (the disabled one is skipped)
		{"99", "/a"}, // out of range: first child, like JMeter
		{"c", "/c"},
		{"nomatch", "/d"}, // falls back to the child named default
		{"${pick}", "/b"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			ps, base := newPathServer(t, nil)
			off := get("off", "/off")
			off.Disabled = true
			sw := ctl(model.StepSwitch, "sw", model.StepControl{Value: tc.value},
				get("a", "/a"), off, get("b", "/b"), get("c", "/c"), get("Default", "/d"))
			runSc(t, newSc(base, perVU(1, 1), setStep("pick", "b"), sw), opts(nil))
			got := ps.requests()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("value %q ran %v, want %s", tc.value, got, tc.want)
			}
		})
	}
	ps, base := newPathServer(t, nil)
	sw := ctl(model.StepSwitch, "sw", model.StepControl{Value: "zzz"}, get("a", "/a"), get("b", "/b"))
	runSc(t, newSc(base, perVU(1, 1), sw), opts(nil))
	if got := ps.requests(); len(got) != 0 {
		t.Fatalf("unmatched name without default must run nothing, ran %v", got)
	}
}

func TestPercentIsDeterministic(t *testing.T) {
	// JMeter rule: pass k (0-based) runs when (100*execs+50)/(k+1) < percent.
	var got []int
	st := &model.Step{Type: model.StepPercent, StepControl: model.StepControl{Percent: 25}}
	c := &ctr{}
	for k := 0; k < 12; k++ {
		if percentDecide(st, c) {
			got = append(got, k)
		}
	}
	if fmt.Sprint(got) != "[2 6 10]" {
		t.Fatalf("25%% passes %v, want [2 6 10]", got)
	}
	ps, base := newPathServer(t, nil)
	p := ctl(model.StepPercent, "quarter", model.StepControl{Percent: 25}, get("x", "/x"))
	n := ctl(model.StepPercent, "first3", model.StepControl{Executions: 3}, get("y", "/y"))
	runSc(t, newSc(base, perVU(2, 100), p, n), opts(nil))
	if a, b := ps.count("/x"), ps.count("/y"); a != 50 || b != 6 {
		t.Fatalf("per-VU percent ran %d (want 50) and executions ran %d (want 6)", a, b)
	}
	ps2, base2 := newPathServer(t, nil)
	sh := ctl(model.StepPercent, "shared", model.StepControl{Executions: 3, Shared: true}, get("z", "/z"))
	runSc(t, newSc(base2, perVU(4, 10), sh), opts(nil))
	if got := ps2.count("/z"); got != 3 {
		t.Fatalf("shared executions ran %d, want 3 across all VUs", got)
	}
}

func TestRuntimeRepeatsForDuration(t *testing.T) {
	ps, base := newPathServer(t, nil)
	rt := ctl(model.StepRuntime, "rt", model.StepControl{Duration: dur(220 * time.Millisecond)},
		get("x", "/x"), model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(50 * time.Millisecond)}})
	start := time.Now()
	runSc(t, newSc(base, perVU(1, 1), rt), opts(nil))
	el := time.Since(start)
	n := ps.count("/x")
	if n < 3 || n > 6 || el < 200*time.Millisecond || el > 2*time.Second {
		t.Fatalf("runtime ran %d passes in %s", n, el)
	}
	ps2, base2 := newPathServer(t, nil)
	capped := ctl(model.StepRuntime, "rt", model.StepControl{Duration: dur(10 * time.Second), MaxIterations: 4}, get("x", "/x"))
	runSc(t, newSc(base2, perVU(1, 1), capped), opts(nil))
	if got := ps2.count("/x"); got != 4 {
		t.Fatalf("runtime cap ran %d, want 4", got)
	}
}

func TestShuffleRunsEveryChildOnceInRandomOrder(t *testing.T) {
	run := func() []string {
		ps, base := newPathServer(t, nil)
		g := model.Step{Type: model.StepGroup, Name: "order", Select: model.SelectShuffle,
			Steps: []model.Step{get("a", "/a"), get("b", "/b"), get("c", "/c"), get("d", "/d")}}
		runSc(t, newSc(base, perVU(1, 30), g), opts(nil))
		return ps.requests()
	}
	got := run()
	if len(got) != 120 {
		t.Fatalf("got %d requests", len(got))
	}
	orders := map[string]bool{}
	for i := 0; i < len(got); i += 4 {
		blk := append([]string(nil), got[i:i+4]...)
		seen := map[string]bool{}
		for _, p := range blk {
			seen[p] = true
		}
		if len(seen) != 4 {
			t.Fatalf("pass %d did not run every child once: %v", i/4, blk)
		}
		orders[strings.Join(blk, "")] = true
	}
	if len(orders) < 5 {
		t.Errorf("shuffle produced only %d distinct orders in 30 passes", len(orders))
	}
	if again := run(); strings.Join(again, ",") != strings.Join(got, ",") {
		t.Errorf("seeded shuffle is not reproducible")
	}
}

func TestRoundRobin(t *testing.T) {
	ps, base := newPathServer(t, nil)
	off := get("off", "/off")
	off.Disabled = true
	g := model.Step{Type: model.StepGroup, Name: "rr", Select: model.SelectRoundRobin,
		Steps: []model.Step{get("a", "/a"), off, get("b", "/b"), get("c", "/c")}}
	runSc(t, newSc(base, perVU(1, 7), g), opts(nil))
	if got := strings.Join(ps.requests(), ""); got != "/a/b/c/a/b/c/a" {
		t.Fatalf("round robin order %s", got)
	}
	ps2, base2 := newPathServer(t, nil)
	g2 := g
	g2.StepControl.Shared = true
	runSc(t, newSc(base2, perVU(3, 4), g2), opts(nil))
	if a, b, c := ps2.count("/a"), ps2.count("/b"), ps2.count("/c"); a != 4 || b != 4 || c != 4 {
		t.Fatalf("shared rotation a=%d b=%d c=%d, want 4 each", a, b, c)
	}
}

func testVU(sc *model.Scenario) *vuState {
	r := &Run{sc: sc, stopIters: make(chan struct{})}
	return &vuState{r: r}
}

func TestThroughputDelayRules(t *testing.T) {
	sc := model.NewScenario("t")
	vu := testVU(sc)
	st := &model.Step{Type: model.StepThroughput, StepControl: model.StepControl{PerMinute: 600}} // 100ms
	t0 := time.Unix(1000, 0)
	steps := []struct {
		at   time.Duration
		want time.Duration
	}{
		{0, 0}, // first pass is immediate
		{10 * time.Millisecond, 90 * time.Millisecond}, // waits to prev+interval
		{100 * time.Millisecond, 100 * time.Millisecond},
		{500 * time.Millisecond, 0}, // late: no catch-up burst, deficit lost
		{510 * time.Millisecond, 90 * time.Millisecond},
	}
	for i, s := range steps {
		if got := vu.throughputDelay(st, t0.Add(s.at)); got != s.want {
			t.Fatalf("step %d: delay %s, want %s", i, got, s.want)
		}
	}
	// active scope: interval multiplied by active VUs.
	act := &model.Step{Type: model.StepThroughput, StepControl: model.StepControl{PerMinute: 600, Scope: model.ThroughputActive}}
	vu.r.active.Store(3)
	vu.throughputDelay(act, t0)
	if got := vu.throughputDelay(act, t0); got != 300*time.Millisecond {
		t.Fatalf("active scope delay %s, want 300ms", got)
	}
	// shared scope: slots handed out max(now, last+interval) across VUs.
	sh := &model.Step{Type: model.StepThroughput, StepControl: model.StepControl{PerMinute: 600, Scope: model.ThroughputShared, Key: "k"}}
	vu2 := &vuState{r: vu.r}
	want := []time.Duration{0, 100 * time.Millisecond, 200 * time.Millisecond}
	for i, v := range []*vuState{vu, vu2, vu} {
		if got := v.throughputDelay(sh, t0); got != want[i] {
			t.Fatalf("shared slot %d: %s, want %s", i, got, want[i])
		}
	}
	if got := vu2.throughputDelay(sh, t0.Add(time.Second)); got != 0 {
		t.Fatalf("shared late arrival should not wait, got %s", got)
	}
}

func TestThroughputStepCapsRate(t *testing.T) {
	ps, base := newPathServer(t, nil)
	thr := ctl(model.StepThroughput, "cap", model.StepControl{PerMinute: 1200, Scope: model.ThroughputShared}) // 50ms
	start := time.Now()
	runSc(t, newSc(base, perVU(2, 5), thr, get("x", "/x")), opts(nil))
	el := time.Since(start)
	if ps.count("/x") != 10 || el < 430*time.Millisecond {
		t.Fatalf("10 shared slots at 50ms need >= 450ms, took %s (%d requests)", el, ps.count("/x"))
	}
}

func TestBarrierGenerationsAndTimeout(t *testing.T) {
	b := &barrier{}
	var released atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ { // two full generations of 3
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.wait(context.Background(), nil, func() int { return 3 }, 5*time.Second) {
				released.Add(1)
			}
		}()
	}
	wg.Wait()
	if released.Load() != 6 {
		t.Fatalf("released %d, want 6", released.Load())
	}
	start := time.Now()
	if b.wait(context.Background(), nil, func() int { return 2 }, 60*time.Millisecond) {
		t.Fatal("lone arrival should time out")
	}
	if el := time.Since(start); el < 50*time.Millisecond {
		t.Fatalf("timed out too early: %s", el)
	}
	// After a timeout the next generation starts fresh.
	done := make(chan bool)
	go func() { done <- b.wait(context.Background(), nil, func() int { return 2 }, 5*time.Second) }()
	if !b.wait(context.Background(), nil, func() int { return 2 }, 5*time.Second) || !<-done {
		t.Fatal("fresh generation of 2 did not release")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if b.wait(ctx, nil, func() int { return 5 }, time.Minute) {
		t.Fatal("cancelled wait reported a full barrier")
	}
}

func TestRendezvousAlignsVUs(t *testing.T) {
	var mu sync.Mutex
	var hits []time.Time
	_, base := newPathServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/spike" {
			mu.Lock()
			hits = append(hits, time.Now())
			mu.Unlock()
		}
	})
	// Each VU pauses a different time (per-VU think via data), then meets.
	sc := newSc(base, perVU(3, 1),
		model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(0), Max: dur(200 * time.Millisecond)}},
		ctl(model.StepRendezvous, "meet", model.StepControl{Group: 3, Timeout: dur(5 * time.Second)}),
		get("spike", "/spike"))
	runSc(t, sc, opts(nil))
	if len(hits) != 3 {
		t.Fatalf("hits %d", len(hits))
	}
	lo, hi := hits[0], hits[0]
	for _, h := range hits {
		if h.Before(lo) {
			lo = h
		}
		if h.After(hi) {
			hi = h
		}
	}
	if spread := hi.Sub(lo); spread > 60*time.Millisecond {
		t.Fatalf("requests after the rendezvous spread %s", spread)
	}
	// A group larger than the VU count is released by the timeout.
	start := time.Now()
	sc2 := newSc(base, perVU(2, 1), ctl(model.StepRendezvous, "big", model.StepControl{Group: 10, Timeout: dur(100 * time.Millisecond)}), get("x", "/x"))
	runSc(t, sc2, opts(nil))
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("timeout did not release: %s", el)
	}
}

func TestCriticalSectionSerialises(t *testing.T) {
	var cur, peak atomic.Int32
	_, base := newPathServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
	})
	crit := ctl(model.StepCritical, "lock", model.StepControl{Key: "db"}, get("x", "/x"))
	runSc(t, newSc(base, perVU(6, 5), crit), opts(nil))
	if peak.Load() != 1 {
		t.Fatalf("critical section allowed %d concurrent requests", peak.Load())
	}
}

func TestPoissonArrivalGapsSeeded(t *testing.T) {
	sc := model.NewScenario("p")
	sc.Workload = model.Workload{Executor: model.ExecArrivalRate, Rate: 50, Duration: dur(time.Second)}
	r := &Run{sc: sc, opts: Options{Seed: 7}}
	if g := r.arrivalGap(50); g != 20*time.Millisecond {
		t.Fatalf("even gap %s", g)
	}
	sc.Workload.Arrivals = model.ArrivalsPoisson
	gaps := func() []time.Duration {
		r := &Run{sc: sc, opts: Options{Seed: 7}}
		out := make([]time.Duration, 20000)
		for i := range out {
			out[i] = r.arrivalGap(50)
		}
		return out
	}
	a, b := gaps(), gaps()
	var sum, sq float64
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("poisson gaps not reproducible with the same seed")
		}
		x := a[i].Seconds()
		sum += x
		sq += x * x
	}
	mean := sum / float64(len(a))
	sd := math.Sqrt(sq/float64(len(a)) - mean*mean)
	if math.Abs(mean-0.02) > 0.001 || math.Abs(sd-0.02) > 0.0015 {
		t.Fatalf("exponential gaps mean %.4f sd %.4f, want 0.02 / 0.02", mean, sd)
	}
}

func TestPoissonArrivalRun(t *testing.T) {
	ps, base := newPathServer(t, nil)
	w := model.Workload{Executor: model.ExecArrivalRate, Rate: 100, Duration: dur(500 * time.Millisecond), VUs: 5, MaxVUs: 20}
	w.Arrivals = model.ArrivalsPoisson
	runSc(t, newSc(base, w, get("x", "/x")), opts(nil))
	if n := ps.count("/x"); n < 20 || n > 90 {
		t.Fatalf("poisson arrivals at 100/s for 0.5s started %d iterations", n)
	}
}

func TestNewUserEachIteration(t *testing.T) {
	run := func(fresh bool) (withCookie int) {
		var n atomic.Int32
		_, base := newPathServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			if _, err := r.Cookie("sid"); err == nil {
				n.Add(1)
			}
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s", Path: "/"})
		})
		w := perVU(1, 4)
		w.NewUserEachIteration = fresh
		runSc(t, newSc(base, w, get("x", "/x")), opts(nil))
		return int(n.Load())
	}
	if got := run(false); got != 3 {
		t.Fatalf("same user: %d requests carried the cookie, want 3", got)
	}
	if got := run(true); got != 0 {
		t.Fatalf("new user each iteration: %d requests carried the cookie, want 0", got)
	}
}

func TestTeardownLoopsRunAfterStop(t *testing.T) {
	ps, base := newPathServer(t, nil)
	sc := newSc(base, constVUs(1, 100*time.Millisecond), get("main", "/main"))
	fe := ctl(model.StepForeach, "cleanup", model.StepControl{Input: "c", Var: "cur"}, get("del", "/del/${cur}"))
	sc.Teardown = []model.Step{setStep("c_1", "a", "c_2", "b"), fe}
	runSc(t, sc, opts(nil))
	if got := ps.count("/del/a") + ps.count("/del/b"); got != 2 {
		t.Fatalf("teardown foreach ran %d requests, want 2", got)
	}
}

// Nested control steps keep their own state inside shuffled/rotated groups.
func TestNestedControlStateIsStable(t *testing.T) {
	ps, base := newPathServer(t, nil)
	inner := ctl(model.StepPercent, "half", model.StepControl{Percent: 50}, get("p", "/p"))
	g := model.Step{Type: model.StepGroup, Name: "mix", Select: model.SelectShuffle, Steps: []model.Step{inner, get("q", "/q")}}
	runSc(t, newSc(base, perVU(1, 20), g), opts(nil))
	if p, q := ps.count("/p"), ps.count("/q"); p != 10 || q != 20 {
		t.Fatalf("p=%d (want 10) q=%d (want 20)", p, q)
	}
}

func TestPolicyMaxDurationBoundsLongControlSteps(t *testing.T) {
	_, base := newPathServer(t, nil)
	rt := ctl(model.StepRuntime, "long", model.StepControl{Duration: dur(time.Hour)},
		model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(20 * time.Millisecond)}})
	o := opts(nil)
	o.Policy.MaxDuration = "300ms"
	start := time.Now()
	runSc(t, newSc(base, perVU(2, 1), rt), o)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("policy maximum duration not enforced: %s", el)
	}
}
