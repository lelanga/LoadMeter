package engine

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/metrics"
	"github.com/lelanga/phoenix/internal/mockserver"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/safety"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "phoenix-engine-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("PHOENIX_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// recorder collects Exchanges from the Inspect callback.
type recorder struct {
	mu sync.Mutex
	ex []Exchange
}

func (r *recorder) fn(e Exchange) {
	r.mu.Lock()
	r.ex = append(r.ex, e)
	r.mu.Unlock()
}

func (r *recorder) all() []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Exchange(nil), r.ex...)
}

func (r *recorder) byStep(name string) []Exchange {
	var out []Exchange
	for _, e := range r.all() {
		if e.Step == name {
			out = append(out, e)
		}
	}
	return out
}

func newMock(t *testing.T) (*httptest.Server, *mockserver.Server) {
	t.Helper()
	ms := mockserver.New("test-seed")
	srv := httptest.NewServer(ms.Handler())
	t.Cleanup(srv.Close)
	return srv, ms
}

func reqStep(name, method, url string) model.Step {
	return model.Step{Type: model.StepRequest, Name: name, Request: &model.Request{Method: method, URL: url}}
}

func get(name, path string) model.Step { return reqStep(name, "GET", path) }

func withAssert(st model.Step, a ...model.Assertion) model.Step {
	st.Request.Assert = append(st.Request.Assert, a...)
	return st
}

func withExtract(st model.Step, e ...model.Extractor) model.Step {
	st.Request.Extract = append(st.Request.Extract, e...)
	return st
}

func withTimeout(st model.Step, d time.Duration) model.Step {
	st.Request.Timeout = model.Duration(d)
	return st
}

func newSc(base string, w model.Workload, steps ...model.Step) *model.Scenario {
	sc := model.NewScenario("test")
	sc.Defaults.BaseURL = base
	sc.Workload = w
	sc.Steps = steps
	return sc
}

func dur(d time.Duration) model.Duration { return model.Duration(d) }

func perVU(vus, iters int) model.Workload {
	return model.Workload{Executor: model.ExecPerVUIterations, VUs: vus, Iterations: iters}
}

func constVUs(vus int, d time.Duration) model.Workload {
	return model.Workload{Executor: model.ExecConstantVUs, VUs: vus, Duration: dur(d)}
}

func opts(rec *recorder) Options {
	o := Options{Policy: safety.DefaultPolicy(), Seed: 1}
	if rec != nil {
		o.Inspect = rec.fn
	}
	return o
}

func waitDone(t *testing.T, r *Run, d time.Duration) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(d):
		r.Stop()
		r.Stop()
		t.Fatalf("run did not finish within %s", d)
	}
}

// runSc prepares, runs to completion and returns the run plus a final snapshot.
func runSc(t *testing.T, sc *model.Scenario, o Options) (*Run, metrics.Snapshot) {
	t.Helper()
	r, err := Prepare(sc, o)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	r.Start(context.Background())
	waitDone(t, r, 30*time.Second)
	if r.Err() != nil {
		t.Fatalf("run error: %v", r.Err())
	}
	return r, r.Col.Snapshot(r.EndedAt, 0)
}

func byReq(s metrics.Snapshot, name string) (metrics.Named, bool) {
	for _, n := range s.ByRequest {
		if n.Name == name {
			return n, true
		}
	}
	return metrics.Named{}, false
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
