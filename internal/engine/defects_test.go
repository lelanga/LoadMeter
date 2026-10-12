package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// Regression: ramping-arrival-rate truncated a fractional starting rate
// (int(w.Rate): 0.5/s became 0).
func TestStageTargetFractionalStart(t *testing.T) {
	stages := []model.Stage{{Duration: dur(10 * time.Second), Target: 0.5}}
	if got := stageTarget(0.5, stages, 5*time.Second); got != 0.5 {
		t.Errorf("flat 0.5/s stage: %v", got)
	}
	up := []model.Stage{{Duration: dur(10 * time.Second), Target: 3.5}}
	if got := stageTarget(1.5, up, 5*time.Second); got != 2.5 {
		t.Errorf("1.5 -> 3.5 midway: %v want 2.5", got)
	}
}

// Regression: in the arrival executor a VU whose setup failed was never
// returned to the pool but still counted as allocated, so the pool shrank for
// good and later iterations were dropped.
func TestArrivalSetupFailureFreesPoolSlot(t *testing.T) {
	t.Parallel()
	var setups atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/setup" {
			setups.Add(1)
			w.WriteHeader(500)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	sc := newSc(srv.URL, model.Workload{Executor: model.ExecArrivalRate, Rate: 20, Duration: dur(time.Second), VUs: 1, MaxVUs: 2, OnError: "stop-vu"},
		get("main", "/main"))
	sc.Setup = []model.Step{get("setup", "/setup")}
	r, _ := runSc(t, sc, opts(nil))
	// Before the fix: exactly 2 setups (the pool size), then everything dropped.
	if n := setups.Load(); n < 8 {
		t.Errorf("failed VUs must be replaced: only %d setup attempts in 1s at 20/s", n)
	}
	if r.Col.Dropped > 15 {
		t.Errorf("pool leaked: %d dropped iterations", r.Col.Dropped)
	}
}

// Regression: ${__counter()} was a process-global, so a second run in the same
// process (serve mode) continued from the first run's value.
func TestCounterRestartsPerRun(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	for run := 0; run < 2; run++ {
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(2, 2), get("c", "/health?n=${__counter()}"))
		runSc(t, sc, opts(rec))
		seen := map[string]bool{}
		for _, ex := range rec.all() {
			seen[ex.URL[strings.Index(ex.URL, "n=")+2:]] = true
		}
		for _, want := range []string{"1", "2", "3", "4"} {
			if !seen[want] || len(seen) != 4 {
				t.Errorf("run %d: counter values %v, want 1..4 shared by the run's VUs", run, seen)
				break
			}
		}
	}
}
