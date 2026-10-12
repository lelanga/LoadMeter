package capacity

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/mockserver"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

// startMock serves the deterministic mock target on 127.0.0.1 only.
func startMock(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ms := mockserver.New("capacity-test") // /api/limited allows 50 requests per second
	srv := &http.Server{Handler: ms.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func limitedScenario(base string) *model.Scenario {
	return &model.Scenario{Version: model.SchemaVersion, Name: "limited", Defaults: model.Defaults{BaseURL: base},
		Workload: model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(time.Second), GracefulStop: model.Duration(time.Second)},
		Steps: []model.Step{
			{Type: model.StepRequest, Name: "delay", Request: &model.Request{Method: "GET", URL: "/delay?ms=5"}},
			{Type: model.StepRequest, Name: "limited", Request: &model.Request{Method: "GET", URL: "/api/limited"}},
		},
		SLAs: []model.SLA{{Metric: "p95", Op: "<=", Threshold: 250}}}
}

// TestIntegrationRateLimitedTarget runs the real engine against the mock
// server, whose /api/limited endpoint answers 429 above 50 requests/s. The
// finder must stop on the error-rate criterion and bracket the limit.
func TestIntegrationRateLimitedTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("generates load for ~12s")
	}
	t.Setenv("LOADMETER_HOME", t.TempDir())
	sc := limitedScenario(startMock(t))
	start := time.Now()
	var steps []Step
	res, err := Find(context.Background(), Options{Scenario: sc, Policy: safety.DefaultPolicy(), MaxErrorPct: 1,
		Plan: Plan{StartRate: 20, StepPct: 100, Warmup: model.Duration(500 * time.Millisecond), Hold: model.Duration(1500 * time.Millisecond),
			MaxRate: 160, BreachSteps: 1, RefineSteps: 2, Budget: model.Duration(40 * time.Second)},
		TargetRate: 200, HeadroomPct: 30, OnStep: func(s Step) { steps = append(steps, s); t.Log(StepRow(s)) }})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	WriteText(&sb, res)
	t.Log(sb.String())
	if res.StopReason != StopBreach || res.Bound != "measured" {
		t.Fatalf("stop=%s bound=%s", res.StopReason, res.Bound)
	}
	if !strings.Contains(res.FirstBreach, "error_rate") {
		t.Fatalf("first breach %q, want the error-rate criterion", res.FirstBreach)
	}
	if res.MaxSustainableRate < 40 || res.MaxSustainableRate >= 60 {
		t.Fatalf("max sustainable %v it/s, want 40..60 (limit is 50/s)", res.MaxSustainableRate)
	}
	if res.FirstFailRate <= 40 || res.FirstFailRate > 80 {
		t.Fatalf("first fail %v", res.FirstFailRate)
	}
	if len(steps) != len(res.Steps) || len(res.Steps) < 4 {
		t.Fatalf("steps %d", len(res.Steps))
	}
	for _, s := range res.Steps {
		if s.OfferedRate > 160 {
			t.Fatalf("rate above max: %v", s.OfferedRate)
		}
		// requests are measured only in the hold period: about 2 per iteration x rate x 1.5s
		if s.Verdict == VerdictPass && (s.Requests < uint64(s.OfferedRate*1.5*2*0.8) || s.Requests > uint64(s.OfferedRate*1.5*2*1.3)) {
			t.Fatalf("step %d: %d requests for %v it/s over 1.5s (warm-up must be excluded)", s.Index, s.Requests, s.OfferedRate)
		}
	}
	if res.Estimate == nil || res.Estimate.Instances == 0 || res.Estimate.RequestsPerIteration < 1.9 {
		t.Fatalf("estimate %+v", res.Estimate)
	}
	dir := filepath.Join(t.TempDir(), res.ID)
	if err := Save(dir, res); err != nil {
		t.Fatal(err)
	}
	back, err := Load(dir)
	if err != nil || back.MaxSustainableRate != res.MaxSustainableRate || len(back.Steps) != len(res.Steps) {
		t.Fatalf("round trip: %v", err)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("took %s", el)
	}
}

// TestIntegrationCancel checks that cancelling the context mid-step stops
// the load promptly and returns a partial result.
func TestIntegrationCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("generates load")
	}
	t.Setenv("LOADMETER_HOME", t.TempDir())
	sc := limitedScenario(startMock(t))
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := Find(ctx, Options{Scenario: sc, Policy: safety.DefaultPolicy(), MaxErrorPct: -1,
		Plan: Plan{StartRate: 5, Warmup: model.Duration(time.Second), Hold: model.Duration(5 * time.Second), MaxRate: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopCanceled || len(res.Steps) != 0 {
		t.Fatalf("stop=%s steps=%d", res.StopReason, len(res.Steps))
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("cancel took %s", el)
	}
}
