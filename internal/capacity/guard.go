package capacity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/safety"
)

// ErrNoCriteria is returned when neither the scenario nor the flags define SLAs.
var ErrNoCriteria = errors.New("the capacity finder needs pass/fail criteria: add \"slas\" to the scenario or pass --p95 <ms> and/or --max-error-pct <pct>")

// Criteria merges the scenario SLAs with the p95 / error-rate shortcuts. A
// shortcut replaces a scenario SLA with the same metric and no scope. "rps"
// SLAs are left out: a minimum throughput would fail every low step.
func Criteria(sc *model.Scenario, p95Ms, maxErrPct float64) ([]model.SLA, []string, error) {
	var out []model.SLA
	var notes []string
	for _, s := range sc.SLAs {
		if s.Metric == "rps" {
			notes = append(notes, fmt.Sprintf("SLA %s %s %g ignored: throughput is what the finder measures", s.Metric, s.Op, s.Threshold))
			continue
		}
		if s.Scope == "" && ((p95Ms > 0 && s.Metric == "p95") || (maxErrPct >= 0 && s.Metric == "error_rate")) {
			continue
		}
		out = append(out, s)
	}
	if p95Ms > 0 {
		out = append(out, model.SLA{Metric: "p95", Op: "<=", Threshold: p95Ms})
	}
	if maxErrPct >= 0 {
		out = append(out, model.SLA{Metric: "error_rate", Op: "<=", Threshold: maxErrPct})
	}
	if len(out) == 0 {
		return nil, notes, ErrNoCriteria
	}
	return out, notes, nil
}

// requestsPerIteration counts request steps in the main flow (loops counted
// once); used before the first step has measured the real ratio.
func requestsPerIteration(sc *model.Scenario) float64 {
	n := 0
	model.Walk(sc.Steps, func(st *model.Step) {
		if st.Type == model.StepRequest && st.Request != nil {
			n++
		}
	})
	return float64(max(n, 1))
}

// PreparePlan applies defaults and the safety policy to a plan:
//   - the VU pool is capped at the policy maxVUs;
//   - a step (warm-up + hold) and the whole budget must fit maxDuration;
//   - the max rate is lowered so that offered requests/s stay under maxRPS;
//   - non-loopback targets must be allowlisted and need an explicit max rate;
//   - app.CheckTargets is called with the highest load the plan can reach, so
//     the same confirmation rule as "run" applies (ErrNeedsConfirmation).
func PreparePlan(sc *model.Scenario, p Plan, pol safety.Policy, confirmed bool) (Plan, []string, error) {
	var notes []string
	userMax := p.MaxRate
	p = p.WithDefaults()
	if p.MaxVUs <= 0 {
		p.MaxVUs = pol.MaxVUs
		if p.MaxVUs <= 0 {
			p.MaxVUs = 1000
		}
	}
	if pol.MaxVUs > 0 && p.MaxVUs > pol.MaxVUs {
		notes = append(notes, fmt.Sprintf("VU pool lowered from %d to the policy maxVUs %d", p.MaxVUs, pol.MaxVUs))
		p.MaxVUs = pol.MaxVUs
	}
	if st := p.WarmupD() + p.Hold.D(); st > pol.MaxDur() {
		return p, notes, fmt.Errorf("a step (%s) exceeds the safety policy maximum duration %s", st, pol.MaxDur())
	}
	if p.Budget.D() > pol.MaxDur() {
		notes = append(notes, fmt.Sprintf("time budget lowered from %s to the policy maximum duration %s", p.Budget.D(), pol.MaxDur()))
		p.Budget = model.Duration(pol.MaxDur())
	}
	remote := false
	for _, h := range app.TargetHosts(sc) {
		if !pol.HostAllowed(h) {
			return p, notes, &safety.ErrHostNotAllowed{Host: h}
		}
		if !safety.IsLoopback(h) {
			remote = true
		}
	}
	if userMax <= 0 {
		if remote {
			return p, notes, errors.New("--max-rate is required when a target is not on this machine")
		}
		p.MaxRate = DefaultLoopbackMaxRate
		notes = append(notes, fmt.Sprintf("no max rate given: using %d it/s (loopback targets only)", DefaultLoopbackMaxRate))
	}
	p.PolicyMaxRPS = pol.MaxRPS
	if pol.MaxRPS > 0 {
		if c := pol.MaxRPS / requestsPerIteration(sc); c < p.MaxRate {
			p.MaxRate, p.RateCap = c, "policy maxRPS"
			notes = append(notes, fmt.Sprintf("max rate lowered to %.2f it/s by the policy maxRPS %g", c, pol.MaxRPS))
		}
	}
	if p.StartRate > p.MaxRate {
		return p, notes, fmt.Errorf("start rate %g is above the max rate %.2f", p.StartRate, p.MaxRate)
	}
	if err := p.Validate(); err != nil {
		return p, notes, err
	}
	probe := *sc
	probe.Workload = StepWorkload(sc.Workload, p.MaxRate, p.WarmupD()+p.Hold.D(), p.MaxVUs)
	if probs := probe.Validate(); len(probs) > 0 {
		return p, notes, fmt.Errorf("invalid scenario: %v", probs)
	}
	if _, err := app.CheckTargets(&probe, pol, confirmed); err != nil {
		var nc *app.ErrNeedsConfirmation
		if errors.As(err, &nc) {
			return p, notes, &app.ErrNeedsConfirmation{Reason: fmt.Sprintf("The capacity finder ramps load against a non-local system up to %.2f iterations/s with up to %d VUs. Only test systems you are authorized to test; confirm to proceed.", p.MaxRate, p.MaxVUs)}
		}
		return p, notes, err
	}
	return p, notes, nil
}

// Options for Find.
type Options struct {
	Scenario    *model.Scenario
	BaseDir     string
	Policy      safety.Policy
	Confirmed   bool
	Plan        Plan
	P95Ms       float64 // 0 = not set
	MaxErrorPct float64 // < 0 = not set
	// Estimate inputs (see Estimate).
	TargetRate      float64
	HeadroomPct     float64
	InstancesTested int
	Logger          *slog.Logger
	OnStep          func(Step)
	// OnStart is called once the plan is final, before the first step.
	OnStart func(Plan, []model.SLA)
}

// Find prepares the plan under the policy, runs the search with an
// EngineRunner and adds the estimate. Callers save the result.
func Find(ctx context.Context, o Options) (*Result, error) {
	crit, notes, err := Criteria(o.Scenario, o.P95Ms, o.MaxErrorPct)
	if err != nil {
		return nil, err
	}
	plan, pnotes, err := PreparePlan(o.Scenario, o.Plan, o.Policy, o.Confirmed)
	if err != nil {
		return nil, err
	}
	notes = append(notes, pnotes...)
	if o.OnStart != nil {
		o.OnStart(plan, crit)
	}
	safety.Audit("capacity.start", map[string]any{"scenario": o.Scenario.Name, "startRate": plan.StartRate, "maxRate": plan.MaxRate,
		"maxVUs": plan.MaxVUs, "budget": plan.Budget.D().String()})
	f := &Finder{Plan: plan, OnStep: o.OnStep, Runner: &EngineRunner{Scenario: o.Scenario, BaseDir: o.BaseDir, Policy: o.Policy,
		MaxVUs: plan.MaxVUs, Criteria: crit, Logger: o.Logger}}
	res, err := f.Run(ctx)
	if err != nil {
		return nil, err
	}
	res.ID = results.NewRunID("capacity-" + o.Scenario.Name)
	res.Scenario = o.Scenario.Name
	res.Criteria = crit
	res.Notes = append(notes, res.Notes...)
	res.Estimate = BuildEstimate(res, o.TargetRate, o.HeadroomPct, o.InstancesTested)
	safety.Audit("capacity.end", map[string]any{"scenario": o.Scenario.Name, "stopReason": res.StopReason,
		"maxSustainableRate": res.MaxSustainableRate, "seconds": time.Since(res.StartedAt).Seconds()})
	return res, nil
}
