package engine

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

func TestConstantVUs(t *testing.T) {
	t.Parallel()
	srv, ms := newMock(t)
	rec := &recorder{}
	sc := newSc(srv.URL, constVUs(3, time.Second), get("health", "/health"))
	start := time.Now()
	r, snap := runSc(t, sc, opts(rec))
	if el := time.Since(start); el < 900*time.Millisecond || el > 4*time.Second {
		t.Errorf("run took %v, want ~1s", el)
	}
	if snap.Requests.Count < 20 {
		t.Errorf("only %d requests", snap.Requests.Count)
	}
	if snap.Requests.Errors != 0 || snap.Iterations != snap.Requests.Count {
		t.Errorf("errors=%d iterations=%d requests=%d", snap.Requests.Errors, snap.Iterations, snap.Requests.Count)
	}
	if uint64(ms.Hits.Load()) != snap.Requests.Count {
		t.Errorf("server saw %d hits, collector %d", ms.Hits.Load(), snap.Requests.Count)
	}
	vus := map[int]bool{}
	for _, e := range rec.all() {
		vus[e.VU] = true
	}
	if len(vus) != 3 {
		t.Errorf("expected 3 distinct VUs, got %v", vus)
	}
	if r.State() != StateDone {
		t.Errorf("state=%s", r.State())
	}
	if snap.Requests.Status["200"] != snap.Requests.Count {
		t.Errorf("status map %v", snap.Requests.Status)
	}
	if snap.Requests.P50Ms <= 0 || snap.Requests.MaxMs < snap.Requests.P50Ms {
		t.Errorf("latency stats %+v", snap.Requests)
	}
}

func TestRampingVUsIncreaseOverTime(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	var mu sync.Mutex
	first := map[int]time.Time{}
	o := opts(nil)
	o.Inspect = func(e Exchange) {
		mu.Lock()
		if _, ok := first[e.VU]; !ok {
			first[e.VU] = time.Now()
		}
		mu.Unlock()
		rec.fn(e)
	}
	w := model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{{Duration: dur(2 * time.Second), Target: 4}}}
	sc := newSc(srv.URL, w, get("h", "/health"), model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(20 * time.Millisecond)}})
	start := time.Now()
	runSc(t, sc, o)
	if len(first) != 4 {
		t.Fatalf("expected 4 VUs to have started, got %d", len(first))
	}
	if d := first[4].Sub(first[1]); d < 900*time.Millisecond {
		t.Errorf("VU4 started only %v after VU1; ramp not gradual", d)
	}
	if first[1].Sub(start) > 800*time.Millisecond {
		t.Errorf("VU1 started late: %v", first[1].Sub(start))
	}
	for i := 2; i <= 4; i++ {
		if first[i].Before(first[i-1]) {
			t.Errorf("VU%d started before VU%d", i, i-1)
		}
	}
}

func TestRampingVUsRampDown(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	var mu sync.Mutex
	last := map[int]time.Time{}
	o := opts(nil)
	o.Inspect = func(e Exchange) { mu.Lock(); last[e.VU] = time.Now(); mu.Unlock() }
	w := model.Workload{Executor: model.ExecRampingVUs, VUs: 3, Stages: []model.Stage{{Duration: dur(2 * time.Second), Target: 0}}}
	sc := newSc(srv.URL, w, get("h", "/health"), model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(20 * time.Millisecond)}})
	runSc(t, sc, o)
	if len(last) != 3 {
		t.Fatalf("VUs seen: %d", len(last))
	}
	// highest-numbered VU leaves first
	if !last[3].Before(last[1]) {
		t.Errorf("VU3 should stop before VU1 on ramp-down: %v vs %v", last[3], last[1])
	}
}

func TestStageTarget(t *testing.T) {
	stages := []model.Stage{{Duration: dur(10 * time.Second), Target: 100}, {Duration: dur(10 * time.Second), Target: 100}, {Duration: dur(10 * time.Second), Target: 0}}
	tests := []struct {
		start int
		el    time.Duration
		want  float64
	}{
		{0, 0, 0}, {0, 5 * time.Second, 50}, {0, 10 * time.Second, 100}, {0, 15 * time.Second, 100},
		{0, 25 * time.Second, 50}, {0, 30 * time.Second, 0}, {0, time.Hour, 0}, {20, 0, 20}, {20, 5 * time.Second, 60},
	}
	for _, tc := range tests {
		if got := stageTarget(tc.start, stages, tc.el); math.Abs(got-tc.want) > 0.001 {
			t.Errorf("start=%d el=%v got %v want %v", tc.start, tc.el, got, tc.want)
		}
	}
	if got := stageTarget(5, nil, time.Second); got != 5 {
		t.Errorf("no stages: %v", got)
	}
	// zero-duration stage is skipped without dividing by zero
	z := []model.Stage{{Duration: 0, Target: 50}, {Duration: dur(time.Second), Target: 100}}
	if got := stageTarget(0, z, 500*time.Millisecond); math.IsNaN(got) || math.Abs(got-75) > 0.001 {
		t.Errorf("zero-duration stage: %v", got)
	}
}

func TestPerVUIterationsExact(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	sc := newSc(srv.URL, perVU(4, 5), get("h", "/health"), get("i", "/api/items"))
	_, snap := runSc(t, sc, opts(rec))
	if snap.Iterations != 20 {
		t.Errorf("iterations=%d want 20", snap.Iterations)
	}
	if snap.Requests.Count != 40 {
		t.Errorf("requests=%d want 40", snap.Requests.Count)
	}
	per := map[int]map[int]bool{}
	for _, e := range rec.all() {
		if per[e.VU] == nil {
			per[e.VU] = map[int]bool{}
		}
		per[e.VU][e.Iteration] = true
	}
	for vu := 1; vu <= 4; vu++ {
		if len(per[vu]) != 5 {
			t.Errorf("vu %d ran iterations %v", vu, per[vu])
		}
	}
}

func TestMaxItersCap(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	o := opts(nil)
	o.MaxIters = 7
	sc := newSc(srv.URL, constVUs(3, 5*time.Second), get("h", "/health"))
	start := time.Now()
	_, snap := runSc(t, sc, o)
	if snap.Iterations != 7 {
		t.Errorf("iterations=%d want 7", snap.Iterations)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("MaxIters should end the run early, took %v", time.Since(start))
	}
}

func TestConstantArrivalRate(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	w := model.Workload{Executor: model.ExecArrivalRate, Rate: 20, Duration: dur(2 * time.Second), VUs: 2, MaxVUs: 10}
	sc := newSc(srv.URL, w, get("h", "/health"))
	_, snap := runSc(t, sc, opts(nil))
	// 20 it/s * 2s = 40 expected
	if snap.Iterations < 28 || snap.Iterations > 46 {
		t.Errorf("iterations=%d want ~40", snap.Iterations)
	}
	if snap.Dropped != 0 {
		t.Errorf("unexpected dropped iterations: %d", snap.Dropped)
	}
}

func TestArrivalRateDroppedWhenSlowAndFewVUs(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	w := model.Workload{Executor: model.ExecArrivalRate, Rate: 40, Duration: dur(1500 * time.Millisecond), VUs: 1, MaxVUs: 1}
	sc := newSc(srv.URL, w, get("slow", "/delay?ms=400"))
	_, snap := runSc(t, sc, opts(nil))
	if snap.Dropped < 10 {
		t.Errorf("dropped=%d, expected many dropped iterations", snap.Dropped)
	}
	if snap.Iterations > 6 {
		t.Errorf("single VU with 400ms requests completed %d iterations in 1.5s", snap.Iterations)
	}
	if snap.Iterations < 1 {
		t.Error("no iterations completed")
	}
}

func TestRampingArrivalRate(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	w := model.Workload{Executor: model.ExecRampingRate, Rate: 5, VUs: 2, MaxVUs: 20,
		Stages: []model.Stage{{Duration: dur(2 * time.Second), Target: 50}}}
	rec := &recorder{}
	var mu sync.Mutex
	var times []time.Time
	o := opts(nil)
	o.Inspect = func(e Exchange) { mu.Lock(); times = append(times, time.Now()); mu.Unlock(); rec.fn(e) }
	sc := newSc(srv.URL, w, get("h", "/health"))
	r, snap := runSc(t, sc, o)
	// area under the ramp: (5+50)/2*2s = 55
	if snap.Iterations < 30 || snap.Iterations > 80 {
		t.Errorf("iterations=%d want ~55", snap.Iterations)
	}
	var firstHalf, secondHalf int
	for _, ts := range times {
		if ts.Sub(r.StartedAt) < time.Second {
			firstHalf++
		} else {
			secondHalf++
		}
	}
	if secondHalf <= firstHalf {
		t.Errorf("rate should increase: first second=%d second second=%d", firstHalf, secondHalf)
	}
}

func TestGracefulThenHardStop(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	sc := newSc(srv.URL, constVUs(2, 30*time.Second), get("hang", "/hang"))
	sc.Defaults.Timeout = dur(30 * time.Second)
	r, err := Prepare(sc, opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	time.Sleep(400 * time.Millisecond)
	r.Stop()
	if r.State() != StateStopping {
		t.Errorf("state after Stop=%s", r.State())
	}
	select {
	case <-r.Done():
		t.Fatal("graceful stop should wait for in-flight requests (default grace 5s)")
	case <-time.After(600 * time.Millisecond):
	}
	t0 := time.Now()
	r.Stop() // second call = hard stop
	waitDone(t, r, 3*time.Second)
	if time.Since(t0) > 2*time.Second {
		t.Errorf("hard stop took %v", time.Since(t0))
	}
	snap := r.Col.Snapshot(r.EndedAt, 0)
	if snap.Requests.Errors != 0 || snap.Requests.Count != 0 {
		t.Errorf("cancelled requests must not be recorded as failures: %+v", snap.Requests)
	}
	if r.State() != StateDone {
		t.Errorf("final state=%s", r.State())
	}
	r.Stop() // Stop after finish must not panic
}

func TestGracefulStopTimeoutCancels(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	w := constVUs(1, 30*time.Second)
	w.GracefulStop = dur(300 * time.Millisecond)
	sc := newSc(srv.URL, w, get("hang", "/hang"))
	sc.Defaults.Timeout = dur(30 * time.Second)
	r, _ := Prepare(sc, opts(nil))
	r.Start(context.Background())
	time.Sleep(200 * time.Millisecond)
	t0 := time.Now()
	r.Stop()
	waitDone(t, r, 3*time.Second)
	if el := time.Since(t0); el < 200*time.Millisecond || el > 2*time.Second {
		t.Errorf("graceful window %v, want ~300ms", el)
	}
}

func TestGracefulStopLetsInflightFinish(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	sc := newSc(srv.URL, constVUs(1, 30*time.Second), get("d", "/delay?ms=500"))
	r, _ := Prepare(sc, opts(nil))
	r.Start(context.Background())
	time.Sleep(150 * time.Millisecond)
	r.Stop()
	waitDone(t, r, 4*time.Second)
	snap := r.Col.Snapshot(r.EndedAt, 0)
	if snap.Requests.Count != 1 || snap.Requests.Errors != 0 {
		t.Errorf("in-flight request should complete: %+v", snap.Requests)
	}
}

func TestContextCancelHardStops(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	sc := newSc(srv.URL, constVUs(1, 30*time.Second), get("hang", "/hang"))
	sc.Defaults.Timeout = dur(30 * time.Second)
	r, _ := Prepare(sc, opts(nil))
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	waitDone(t, r, 3*time.Second)
}

func TestPolicyRejections(t *testing.T) {
	t.Parallel()
	base := "http://127.0.0.1:1"
	tests := []struct {
		name   string
		w      model.Workload
		pol    safety.Policy
		wantIn string
	}{
		{"constant too many", constVUs(10, time.Second), safety.Policy{MaxVUs: 5}, "safety policy allows 5"},
		{"per-vu too many", perVU(6, 1), safety.Policy{MaxVUs: 5}, "safety policy"},
		{"ramping stage target", model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{{Duration: dur(time.Second), Target: 9}}}, safety.Policy{MaxVUs: 5}, "needs 9 VUs"},
		{"arrival maxVUs", model.Workload{Executor: model.ExecArrivalRate, Rate: 1, Duration: dur(time.Second), VUs: 1, MaxVUs: 50}, safety.Policy{MaxVUs: 5}, "needs 50 VUs"},
		{"duration", constVUs(1, 2*time.Hour), safety.Policy{MaxDuration: "1h"}, "exceeds the safety policy maximum"},
		{"ramping duration", model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{{Duration: dur(40 * time.Minute), Target: 1}, {Duration: dur(40 * time.Minute), Target: 1}}}, safety.Policy{MaxDuration: "1h"}, "exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := newSc(base, tc.w, get("h", "/health"))
			o := opts(nil)
			o.Policy = tc.pol
			_, err := Prepare(sc, o)
			if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err=%v want containing %q", err, tc.wantIn)
			}
		})
	}
	// within limits passes
	o := opts(nil)
	o.Policy = safety.Policy{MaxVUs: 5, MaxDuration: "1h"}
	if _, err := Prepare(newSc(base, constVUs(5, time.Hour), get("h", "/health")), o); err != nil {
		t.Errorf("at-limit workload rejected: %v", err)
	}
	// zero MaxVUs means no cap
	o.Policy = safety.Policy{}
	if _, err := Prepare(newSc(base, constVUs(5000, time.Minute), get("h", "/health")), o); err != nil {
		t.Errorf("uncapped policy rejected workload: %v", err)
	}
}

func TestPolicyOverridesAreChecked(t *testing.T) {
	t.Parallel()
	sc := newSc("http://127.0.0.1:1", constVUs(1, time.Second), get("h", "/health"))
	o := opts(nil)
	o.Policy = safety.Policy{MaxVUs: 5}
	ov := constVUs(50, time.Second)
	o.Overrides = &ov
	if _, err := Prepare(sc, o); err == nil || !strings.Contains(err.Error(), "safety policy") {
		t.Errorf("override should be policy-checked: %v", err)
	}
}

func TestPrepareRejectsInvalidScenario(t *testing.T) {
	t.Parallel()
	sc := model.NewScenario("empty")
	if _, err := Prepare(sc, opts(nil)); err == nil || !strings.Contains(err.Error(), "invalid scenario") {
		t.Errorf("err=%v", err)
	}
}

func TestRateLimitPolicy(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	o := opts(nil)
	o.Policy.MaxRPS = 20
	sc := newSc(srv.URL, constVUs(4, 1200*time.Millisecond), get("h", "/health"))
	_, snap := runSc(t, sc, o)
	if snap.Requests.Count < 10 || snap.Requests.Count > 32 {
		t.Errorf("requests=%d under MaxRPS=20 for 1.2s (expected ~25)", snap.Requests.Count)
	}
}

func TestPacing(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	w := perVU(1, 3)
	w.Pacing = dur(250 * time.Millisecond)
	sc := newSc(srv.URL, w, get("h", "/health"))
	start := time.Now()
	_, snap := runSc(t, sc, opts(nil))
	if snap.Iterations != 3 {
		t.Fatalf("iterations %d", snap.Iterations)
	}
	if el := time.Since(start); el < 450*time.Millisecond {
		t.Errorf("pacing 250ms between 3 iterations should take >=500ms, took %v", el)
	}
}

func TestRawCSVOutput(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	var sb strings.Builder
	o := opts(nil)
	o.Raw = &sb
	sc := newSc(srv.URL, perVU(1, 3), get("h", "/health"))
	runSc(t, sc, o)
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "timeStamp,elapsed,label") {
		t.Errorf("raw csv: %q", sb.String())
	}
}
