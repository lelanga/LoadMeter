package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"runtime"
	"time"

	"github.com/lelanga/loadmeter/internal/engine"
	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/sysres"
)

// EngineRunner runs each step as a separate constant-arrival-rate engine run
// of the scenario. The engine applies the safety policy (guarded dialer,
// maxRPS limiter, maxVUs and maxDuration checks) to every step.
type EngineRunner struct {
	Scenario *model.Scenario // not modified
	BaseDir  string
	Policy   safety.Policy
	MaxVUs   int // VU pool per step
	Criteria []model.SLA
	Logger   *slog.Logger
}

func cloneScenario(sc *model.Scenario) (*model.Scenario, error) {
	b, err := json.Marshal(sc)
	if err != nil {
		return nil, err
	}
	var out model.Scenario
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StepWorkload is the open-model workload used for one step.
func StepWorkload(base model.Workload, rate float64, dur time.Duration, maxVUs int) model.Workload {
	return model.Workload{Executor: model.ExecArrivalRate, Rate: rate, Duration: model.Duration(dur),
		VUs: max(1, min(maxVUs, int(math.Ceil(rate/10)))), MaxVUs: maxVUs,
		GracefulStop: base.GracefulStop, ThinkTimeScale: base.ThinkTimeScale, OnError: base.OnError}
}

// RunStep runs warmup+hold at rate and measures only the hold period: the
// collector's delta is taken (and discarded) at the end of the warm-up, and
// the hold delta is merged into a fresh collector so that its percentiles come
// from this step's own histogram.
func (e *EngineRunner) RunStep(ctx context.Context, rate float64, warmup, hold time.Duration) (Measure, error) {
	sc, err := cloneScenario(e.Scenario)
	if err != nil {
		return Measure{}, err
	}
	sc.Workload = StepWorkload(e.Scenario.Workload, rate, warmup+hold, e.MaxVUs)
	r, err := engine.Prepare(sc, engine.Options{BaseDir: e.BaseDir, Policy: e.Policy, Seed: 42, Logger: e.Logger,
		OnCollector: func(c *metrics.Collector) { c.TrackDeltas() }})
	if err != nil {
		return Measure{}, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(runCtx)
	abort := func() (Measure, error) {
		r.Stop() // graceful; the engine hard-stops after gracefulStop
		<-r.Done()
		return Measure{}, ctx.Err()
	}
	if warmup > 0 {
		select {
		case <-time.After(warmup):
		case <-ctx.Done():
			return abort()
		case <-r.Done():
			return Measure{}, stepErr(r, "the step ended during warm-up")
		}
	}
	_ = r.Col.TakeDelta() // discard warm-up samples
	cut := time.Now()
	cpu := sysres.NewSampler()
	select {
	case <-time.After(hold):
	case <-ctx.Done():
		return abort()
	case <-r.Done():
	}
	cpuPct, _ := cpu.Sample()
	select {
	case <-r.Done():
	case <-ctx.Done():
		return abort()
	}
	if r.Err() != nil {
		return Measure{}, r.Err()
	}
	d := r.Col.TakeDelta()
	return measureDelta(d, rate, hold, cut, cpuPct, e.Criteria)
}

func stepErr(r *engine.Run, msg string) error {
	if r.Err() != nil {
		return r.Err()
	}
	return errors.New(msg)
}

// measureDelta turns a hold-period delta into a Measure.
func measureDelta(d *metrics.Delta, rate float64, hold time.Duration, cut time.Time, cpuPct float64, criteria []model.SLA) (Measure, error) {
	col := metrics.NewCollector(cut, nil)
	if err := col.MergeDelta("step", d); err != nil {
		return Measure{}, err
	}
	snap := col.Snapshot(cut.Add(hold), math.MaxInt32)
	rq := snap.Requests
	m := Measure{OfferedRate: rate, HoldSec: hold.Seconds(), Iterations: snap.Iterations, Requests: rq.Count, Errors: rq.Errors,
		ErrorPct: rq.ErrorRate, P50Ms: rq.P50Ms, P90Ms: rq.P90Ms, P95Ms: rq.P95Ms, P99Ms: rq.P99Ms, MaxMs: rq.MaxMs, AvgMs: rq.AvgMs,
		Dropped: snap.Dropped, CPUPercent: cpuPct, CPUs: runtime.NumCPU(), ErrorKinds: rq.ErrorKind}
	if d != nil {
		for _, w := range d.Windows {
			m.PeakVUs = max(m.PeakVUs, w.ActiveVUs)
		}
	}
	m.SLAs, _ = results.EvaluateSLAs(criteria, col, snap)
	return m, nil
}
