package capacity

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/safety"
)

// ---------- knee / plateau ----------

func TestKneedleHockeyStick(t *testing.T) {
	// Flat 10 ms up to x=60, then +100 ms per 10 units: the knee is x=60.
	var xs, ys []float64
	for x := 10.0; x <= 100; x += 10 {
		y := 10.0
		if x > 60 {
			y = 10 + 100*(x-60)/10
		}
		xs, ys = append(xs, x), append(ys, y)
	}
	i, score, ok := Kneedle(xs, ys)
	if !ok || xs[i] != 60 {
		t.Fatalf("knee = %v (ok=%v), want x=60", xs[i], ok)
	}
	// D at x=60 is (60-10)/90 - 0 = 0.556
	if math.Abs(score-50.0/90) > 1e-9 {
		t.Fatalf("score %v", score)
	}
}

func TestKneedleSmoothConvex(t *testing.T) {
	// y = 1/(C - x) queueing curve (M/M/1 style) with C=100: knee near x=80-90.
	var xs, ys []float64
	for x := 10.0; x <= 95; x += 5 {
		xs, ys = append(xs, x), append(ys, 1000/(100-x))
	}
	i, _, ok := Kneedle(xs, ys)
	if !ok || xs[i] < 75 || xs[i] > 90 {
		t.Fatalf("knee = %v ok=%v, want 75..90", xs[i], ok)
	}
}

func TestKneedleNoKnee(t *testing.T) {
	line := []float64{1, 2, 3, 4, 5, 6}
	if _, _, ok := Kneedle(line, line); ok {
		t.Fatal("a straight line has no knee")
	}
	flat := []float64{20, 20.5, 21, 20.8, 21.2, 21.5}
	if _, _, ok := Kneedle(line, flat); ok {
		t.Fatal("flat latency (rise < 1.2x) has no knee")
	}
	// Rising only at the last point: the knee would be the last point, which is not accepted... unless
	// a previous point dominates. Here every earlier point has D=xn (y flat), so the max is the
	// second-to-last point: the curve is a hockey stick whose rise is the final step.
	stick := []float64{10, 10, 10, 10, 10, 100}
	i, _, ok := Kneedle(line, stick)
	if !ok || i != 4 {
		t.Fatalf("got %d ok=%v, want 4", i, ok)
	}
	if _, _, ok := Kneedle(line[:2], stick[:2]); ok {
		t.Fatal("two points cannot have a knee")
	}
}

func TestPlateau(t *testing.T) {
	var off, ach []float64
	for o := 10.0; o <= 100; o += 10 {
		off, ach = append(off, o), append(ach, math.Min(o, 55))
	}
	p, ok := DetectPlateau(off, ach, DefaultPlateauGain)
	if !ok || p.LastScalingRate != 60 || p.StartIndex != 6 || p.MaxThroughput != 55 {
		t.Fatalf("plateau %+v ok=%v", p, ok)
	}
	// One noisy step is not a plateau.
	noisy := []float64{10, 20, 22, 40, 50, 60}
	if _, ok := DetectPlateau([]float64{10, 20, 30, 40, 50, 60}, noisy, DefaultPlateauGain); ok {
		t.Fatal("single low-gain step must not count as a plateau")
	}
	// Linear scaling: none.
	if _, ok := DetectPlateau(off, off, DefaultPlateauGain); ok {
		t.Fatal("linear scaling has no plateau")
	}
}

// ---------- fake runner ----------

type fakeClock struct{ t time.Time }

type fakeTarget struct {
	capacity   float64            // p95 jumps above 100 ms past this rate
	reqPerIter int                // default 1
	stormAbove float64            // error rate 80% above this rate (0 = never)
	cpuAbove   float64            // generator CPU saturates above this rate (0 = never)
	dropAbove  float64            // dropped iterations above this rate (latency OK)
	failAt     map[float64]bool   // one-off failures (noise)
	clock      *fakeClock         // advanced by the step time
	cancelAt   int                // cancel after this many calls
	cancel     context.CancelFunc //
	calls      []float64
}

func (f *fakeTarget) RunStep(ctx context.Context, rate float64, warmup, hold time.Duration) (Measure, error) {
	if err := ctx.Err(); err != nil {
		return Measure{}, err
	}
	f.calls = append(f.calls, rate)
	if f.clock != nil {
		f.clock.t = f.clock.t.Add(warmup + hold + stepOverhead)
	}
	if f.cancelAt > 0 && len(f.calls) == f.cancelAt {
		f.cancel()
	}
	rpi := max(f.reqPerIter, 1)
	iters := uint64(rate * hold.Seconds())
	m := Measure{HoldSec: hold.Seconds(), Iterations: iters, Requests: iters * uint64(rpi), CPUs: 4, CPUPercent: 40}
	m.P95Ms = 20 + 5*rate/f.capacity
	if rate > f.capacity {
		m.P95Ms = 120 + (rate-f.capacity)*10
	}
	if f.failAt[rate] {
		m.P95Ms = 150
	}
	m.P50Ms, m.P99Ms = m.P95Ms/2, m.P95Ms*1.5
	if f.stormAbove > 0 && rate > f.stormAbove {
		m.Errors = m.Requests * 8 / 10
		m.ErrorPct = 80
	}
	if f.cpuAbove > 0 && rate > f.cpuAbove {
		m.CPUPercent = 390 // 97.5% of 4 cores
	}
	if f.dropAbove > 0 && rate > f.dropAbove {
		m.Dropped = 7
		m.P95Ms = 30
	}
	m.SLAs = []results.SLAResult{
		{SLA: model.SLA{Metric: "p95", Op: "<=", Threshold: 100}, Actual: m.P95Ms, Passed: m.P95Ms <= 100},
		{SLA: model.SLA{Metric: "error_rate", Op: "<=", Threshold: 1}, Actual: m.ErrorPct, Passed: m.ErrorPct <= 1},
	}
	return m, nil
}

func basePlan() Plan {
	return Plan{StartRate: 10, StepPct: 50, Warmup: model.Duration(time.Second), Hold: model.Duration(5 * time.Second),
		MaxRate: 1000, BreachSteps: 1, RefineSteps: 4, RefineTolPct: 2, MaxVUs: 100}
}

func TestFinderStaircaseAndBisection(t *testing.T) {
	ft := &fakeTarget{capacity: 63}
	var seen []Step
	f := &Finder{Plan: basePlan(), Runner: ft, OnStep: func(s Step) { seen = append(seen, s) }}
	res, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// ramp: 10 15 22.5 33.75 50.63 75.95(fail); refine between 50.63 and 75.95
	if res.StopReason != StopBreach || res.Bound != "measured" {
		t.Fatalf("stop=%s bound=%s", res.StopReason, res.Bound)
	}
	if res.MaxSustainableRate > 63 || res.MaxSustainableRate < 60 {
		t.Fatalf("max sustainable %v, want within (60, 63]", res.MaxSustainableRate)
	}
	if res.FirstFailRate <= 63 || res.FirstFailRate > 64 {
		t.Fatalf("first fail %v, want just above 63", res.FirstFailRate)
	}
	if !strings.Contains(res.FirstBreach, "p95") {
		t.Fatalf("first breach %q", res.FirstBreach)
	}
	var ramp, refine int
	for _, s := range res.Steps {
		if s.Phase == "ramp" {
			ramp++
		} else {
			refine++
		}
	}
	if ramp != 6 || refine != 4 || len(seen) != 10 {
		t.Fatalf("ramp=%d refine=%d seen=%d calls=%v", ramp, refine, len(seen), ft.calls)
	}
	if res.Knee == nil {
		t.Fatal("expected a latency knee")
	}
	if res.Confidence != "high" {
		t.Fatalf("confidence %s (%v)", res.Confidence, res.Notes)
	}
}

func TestFinderConsecutiveBreachesAndNoise(t *testing.T) {
	ft := &fakeTarget{capacity: 63, failAt: map[float64]bool{22.5: true}}
	p := basePlan()
	p.BreachSteps = 2
	res, err := (&Finder{Plan: p, Runner: ft}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 22.5 fails once (noise), 33.75 passes, then 75.95 and 113.92 fail -> stop.
	if res.StopReason != StopBreach {
		t.Fatalf("stop %s", res.StopReason)
	}
	if got := ft.calls[:7]; got[5] != 75.95 || got[6] != 113.93 && got[6] != 113.92 {
		t.Fatalf("calls %v", ft.calls)
	}
	if res.FirstFailRate > 64 || res.MaxSustainableRate < 60 {
		t.Fatalf("result %v..%v", res.MaxSustainableRate, res.FirstFailRate)
	}
	if res.Confidence != "medium" {
		t.Fatalf("noisy run should lower confidence, got %s", res.Confidence)
	}
}

func TestFinderGeneratorLimited(t *testing.T) {
	ft := &fakeTarget{capacity: 1000, cpuAbove: 40}
	res, err := (&Finder{Plan: basePlan(), Runner: ft}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.GeneratorLimited || res.StopReason != StopGenerator || res.Bound != "lower-bound" || res.Confidence != "low" {
		t.Fatalf("%+v", res)
	}
	if res.MaxSustainableRate != 33.75 {
		t.Fatalf("max %v", res.MaxSustainableRate)
	}
	for _, s := range res.Steps {
		if s.Phase == "refine" {
			t.Fatal("no refinement after generator saturation")
		}
	}
	if !strings.Contains(strings.Join(res.Limitations, " "), "GENERATOR-LIMITED") {
		t.Fatal("limitation missing")
	}
}

func TestClassifyDropped(t *testing.T) {
	p := basePlan().WithDefaults()
	ok := []results.SLAResult{{SLA: model.SLA{Metric: "p95", Op: "<=", Threshold: 100}, Actual: 20, Passed: true}}
	bad := []results.SLAResult{{SLA: model.SLA{Metric: "p95", Op: "<=", Threshold: 100}, Actual: 900, Passed: false}}
	m := Measure{OfferedRate: 10, HoldSec: 1, Iterations: 10, Requests: 10, CPUs: 4, Dropped: 3, SLAs: ok}
	if v, _ := classify(p, m); v != VerdictGenerator {
		t.Fatalf("dropped with latency OK = %s", v)
	}
	m.SLAs = bad
	if v, r := classify(p, m); v != VerdictBreach || !strings.Contains(r, "p95") {
		t.Fatalf("dropped with SLA breach = %s %s", v, r)
	}
	m = Measure{OfferedRate: 100, HoldSec: 1, Iterations: 50, Requests: 50, CPUs: 4, SLAs: ok}
	if v, r := classify(p, m); v != VerdictBreach || !strings.Contains(r, "achieved") {
		t.Fatalf("achieved 50%% of offered = %s %s", v, r)
	}
	m = Measure{OfferedRate: 10, HoldSec: 1}
	if v, _ := classify(p, m); v != VerdictStorm {
		t.Fatalf("no requests = %s", v)
	}
}

func TestFinderErrorStorm(t *testing.T) {
	ft := &fakeTarget{capacity: 1000, stormAbove: 40}
	p := basePlan()
	p.BreachSteps = 3
	res, err := (&Finder{Plan: p, Runner: ft}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopStorm || res.FirstFailRate == 0 || res.Bound != "measured" {
		t.Fatalf("%+v", res)
	}
	// storm stops at once despite BreachSteps=3: 10 15 22.5 33.75 50.63 then refinement
	if ft.calls[5] >= 50.63 {
		t.Fatalf("calls %v", ft.calls)
	}
	if res.MaxSustainableRate < 38 || res.MaxSustainableRate > 40 {
		t.Fatalf("max %v", res.MaxSustainableRate)
	}
}

func TestFinderMaxRateCap(t *testing.T) {
	ft := &fakeTarget{capacity: 1000}
	p := basePlan()
	p.MaxRate = 40
	res, err := (&Finder{Plan: p, Runner: ft}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopMaxRate || res.Bound != "lower-bound" || res.MaxSustainableRate != 40 {
		t.Fatalf("%+v", res)
	}
	for _, r := range ft.calls {
		if r > 40 {
			t.Fatalf("rate %v above the cap", r)
		}
	}
}

func TestFinderPolicyMaxRPS(t *testing.T) {
	ft := &fakeTarget{capacity: 1000, reqPerIter: 2}
	p := basePlan()
	p.PolicyMaxRPS = 60 // 2 requests per iteration -> 30 it/s
	res, err := (&Finder{Plan: p, Runner: ft}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopPolicy {
		t.Fatalf("stop %s", res.StopReason)
	}
	for _, r := range ft.calls {
		if r*2 > 60 {
			t.Fatalf("offered %v req/s above maxRPS", r*2)
		}
	}
	if ft.calls[len(ft.calls)-1] != 30 {
		t.Fatalf("calls %v", ft.calls)
	}
}

func TestFinderBudget(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	ft := &fakeTarget{capacity: 1000, clock: clk}
	p := basePlan()
	p.Warmup, p.Hold = model.Duration(5*time.Second), model.Duration(20*time.Second) // 27s per step
	p.Budget = model.Duration(90 * time.Second)
	res, err := (&Finder{Plan: p, Runner: ft, Clock: func() time.Time { return clk.t }}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopBudget || len(res.Steps) != 3 {
		t.Fatalf("stop=%s steps=%d", res.StopReason, len(res.Steps))
	}
	if d := clk.t.Sub(res.StartedAt); d > 90*time.Second {
		t.Fatalf("used %s > budget", d)
	}
}

func TestFinderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ft := &fakeTarget{capacity: 1000, cancelAt: 2, cancel: cancel}
	res, err := (&Finder{Plan: basePlan(), Runner: ft}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopCanceled || len(res.Steps) != 2 || res.Confidence != "low" {
		t.Fatalf("stop=%s steps=%d conf=%s", res.StopReason, len(res.Steps), res.Confidence)
	}
}

func TestFinderStartRateFails(t *testing.T) {
	ft := &fakeTarget{capacity: 5}
	res, err := (&Finder{Plan: basePlan(), Runner: ft}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxSustainableRate != 0 || !strings.Contains(strings.Join(res.Notes, " "), "lower --start-rate") {
		t.Fatalf("%+v", res)
	}
}

// ---------- safety and criteria ----------

func scenario(url string, slas ...model.SLA) *model.Scenario {
	return &model.Scenario{Version: model.SchemaVersion, Name: "cap", SLAs: slas,
		Workload: model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(time.Second)},
		Steps:    []model.Step{{Type: model.StepRequest, Name: "a", Request: &model.Request{Method: "GET", URL: url}}, {Type: model.StepRequest, Name: "b", Request: &model.Request{Method: "GET", URL: url}}}}
}

func TestCriteria(t *testing.T) {
	sc := scenario("http://127.0.0.1:1/")
	if _, _, err := Criteria(sc, 0, -1); !errors.Is(err, ErrNoCriteria) {
		t.Fatalf("want ErrNoCriteria, got %v", err)
	}
	c, _, err := Criteria(sc, 250, 1)
	if err != nil || len(c) != 2 || c[0].Metric != "p95" || c[0].Threshold != 250 || c[1].Metric != "error_rate" {
		t.Fatalf("%v %v", c, err)
	}
	sc.SLAs = []model.SLA{{Metric: "p95", Op: "<", Threshold: 500}, {Metric: "rps", Op: ">=", Threshold: 100}, {Metric: "p99", Scope: "a", Op: "<", Threshold: 900}}
	c, notes, err := Criteria(sc, 200, -1)
	if err != nil || len(c) != 2 || c[0].Metric != "p99" || c[1].Threshold != 200 || len(notes) != 1 {
		t.Fatalf("%v %v %v", c, notes, err)
	}
}

func TestPreparePlanPolicy(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	pol := safety.DefaultPolicy()
	pol.MaxVUs, pol.MaxRPS = 50, 100
	sc := scenario("http://127.0.0.1:9/x")
	p, notes, err := PreparePlan(sc, Plan{MaxVUs: 500, Hold: model.Duration(5 * time.Second)}, pol, false)
	if err != nil {
		t.Fatal(err)
	}
	// 2 requests/iteration and maxRPS 100 -> 50 it/s; loopback default max rate 500 lowered.
	if p.MaxVUs != 50 || p.MaxRate != 50 || p.RateCap != "policy maxRPS" || len(notes) < 2 {
		t.Fatalf("plan %+v notes %v", p, notes)
	}
	pol.MaxDuration = "10s"
	if _, _, err := PreparePlan(sc, Plan{Hold: model.Duration(20 * time.Second)}, pol, false); err == nil || !strings.Contains(err.Error(), "maximum duration") {
		t.Fatalf("step longer than maxDuration: %v", err)
	}
	pol = safety.DefaultPolicy()
	remote := scenario("http://example.com/x")
	var hna *safety.ErrHostNotAllowed
	if _, _, err := PreparePlan(remote, Plan{MaxRate: 10}, pol, true); !errors.As(err, &hna) {
		t.Fatalf("non-allowlisted host: %v", err)
	}
	pol.AllowedHosts = []string{"example.com"}
	if _, _, err := PreparePlan(remote, Plan{}, pol, true); err == nil || !strings.Contains(err.Error(), "--max-rate") {
		t.Fatalf("remote needs max rate: %v", err)
	}
	var nc *app.ErrNeedsConfirmation
	if _, _, err := PreparePlan(remote, Plan{MaxRate: 100}, pol, false); !errors.As(err, &nc) {
		t.Fatalf("remote needs confirmation: %v", err)
	}
	if _, _, err := PreparePlan(remote, Plan{MaxRate: 100}, pol, true); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
}

func TestEstimate(t *testing.T) {
	res := &Result{MaxSustainableRate: 100, Bound: "measured", Env: results.Environment{CPUs: 8},
		Steps: []Step{
			{Verdict: VerdictPass, Requests: 2000, Iterations: 1000, RequestRate: 200, CPUPercent: 50},
			{Verdict: VerdictPass, Requests: 4000, Iterations: 2000, RequestRate: 400, CPUPercent: 100},
			{Verdict: VerdictGenerator, Requests: 1, Iterations: 1000, RequestRate: 1, CPUPercent: 790},
		}}
	e := BuildEstimate(res, 1000, 30, 2)
	// per instance 50, usable 35, ceil(1000/35) = 29
	if e.PerInstanceRate != 50 || e.UsablePerInst != 35 || e.Instances != 29 {
		t.Fatalf("%+v", e)
	}
	// 2 requests/iteration, 400 req/s per core -> 2000 req/s / 400 / 0.7 = 7.14 cores -> 1 machine of 8
	if e.RequestsPerIteration != 2 || e.GenReqPerCore != 400 || e.GenCoresForTarget != 7.14 || e.GenMachinesForTarget != 1 {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.Label, "ESTIMATE") || len(e.Assumptions) < 4 {
		t.Fatal("estimate must be labelled with assumptions")
	}
	if e2 := BuildEstimate(&Result{}, 1000, 0, 0); e2.Instances != 0 || e2.HeadroomPct != 30 || e2.InstancesNote == "" {
		t.Fatalf("%+v", e2)
	}
}
