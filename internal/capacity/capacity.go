// Package capacity finds the highest arrival rate a target sustains while it
// still meets the scenario's SLAs (the "capacity" or "breaking point").
//
// How it works:
//
//  1. Staircase. The scenario runs as an open-model (constant-arrival-rate)
//     load at a start rate. Each step holds one rate for Warmup+Hold; samples
//     completed during Warmup are discarded and the step is judged only on the
//     Hold samples. The next step multiplies the rate by (1+StepPct/100) or
//     adds StepAdd, never above MaxRate (and never above the policy caps).
//  2. Per-step measurement. Every step has its own merged latency histogram,
//     so p50/p95/p99 are read from that step's full distribution. Percentiles
//     are never averaged across steps or seconds. The step also records the
//     achieved throughput, error %, dropped iterations and generator CPU.
//  3. Stop rules. The staircase stops when the SLA criteria fail for
//     BreachSteps consecutive steps, when errors explode (ErrorStormPct), when
//     the generator saturates (CPU above CPUSatPct of all cores, or dropped
//     iterations while latency was still within SLA), when the rate cap is
//     reached, or when the time budget or context ends.
//  4. Refinement. Between the last passing rate and the first failing rate a
//     bounded bisection (RefineSteps) narrows the answer.
//  5. Analysis. The knee of the latency-vs-throughput curve (Kneedle) and the
//     throughput plateau are detected separately (see knee.go) and reported
//     with whether they agree.
//
// Everything here is deterministic given the measurements; the StepRunner
// interface lets tests drive the algorithm without generating load.
package capacity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/results"
)

// Plan controls the staircase. Rates are scenario iterations per second (the
// same unit as the arrival-rate executors), not HTTP requests per second.
type Plan struct {
	StartRate float64        `json:"startRate"`
	StepPct   float64        `json:"stepPct,omitempty"` // multiplicative increment, e.g. 25 = x1.25
	StepAdd   float64        `json:"stepAdd,omitempty"` // additive increment (wins over StepPct when > 0)
	Warmup    model.Duration `json:"warmup"`
	Hold      model.Duration `json:"hold"`
	MaxRate   float64        `json:"maxRate"`
	// RateCap names what set MaxRate: "max-rate" (the user) or "policy maxRPS".
	RateCap        string         `json:"rateCap,omitempty"`
	MaxVUs         int            `json:"maxVUs"`
	BreachSteps    int            `json:"breachSteps"`  // consecutive failing steps that end the staircase
	RefineSteps    int            `json:"refineSteps"`  // bisection steps (0 = none)
	RefineTolPct   float64        `json:"refineTolPct"` // stop bisecting when the gap is below this % of the failing rate
	ErrorStormPct  float64        `json:"errorStormPct"`
	CPUSatPct      float64        `json:"cpuSaturationPct"` // of all cores
	MinAchievedPct float64        `json:"minAchievedPct"`   // achieved/offered below this is a breach
	MaxSteps       int            `json:"maxSteps"`
	Budget         model.Duration `json:"budget"` // hard limit on the whole search
	// PolicyMaxRPS is the policy's request-rate cap (0 = none); the finder
	// lowers MaxRate once it has measured requests per iteration.
	PolicyMaxRPS float64 `json:"policyMaxRPS,omitempty"`
}

// Defaults used when a Plan field is zero.
const (
	DefaultStartRate     = 10
	DefaultStepPct       = 25
	DefaultWarmup        = 5 * time.Second
	DefaultHold          = 20 * time.Second
	DefaultBreachSteps   = 2
	DefaultRefineSteps   = 3
	DefaultRefineTolPct  = 5
	DefaultErrorStormPct = 50
	DefaultCPUSatPct     = 85
	DefaultMinAchieved   = 90
	DefaultMaxSteps      = 40
	DefaultBudget        = 30 * time.Minute
	// DefaultLoopbackMaxRate applies when no --max-rate is given and every target is loopback.
	DefaultLoopbackMaxRate = 500
	// stepOverhead is added to each step's planned time for graceful stop and setup.
	stepOverhead = 2 * time.Second
)

// WithDefaults fills zero fields.
func (p Plan) WithDefaults() Plan {
	if p.StartRate <= 0 {
		p.StartRate = DefaultStartRate
	}
	if p.StepPct <= 0 && p.StepAdd <= 0 {
		p.StepPct = DefaultStepPct
	}
	if p.Warmup == 0 { // negative = explicitly no warm-up (see WarmupD)
		p.Warmup = model.Duration(DefaultWarmup)
	}
	if p.Hold <= 0 {
		p.Hold = model.Duration(DefaultHold)
	}
	if p.BreachSteps <= 0 {
		p.BreachSteps = DefaultBreachSteps
	}
	if p.RefineSteps < 0 {
		p.RefineSteps = 0
	}
	if p.RefineTolPct <= 0 {
		p.RefineTolPct = DefaultRefineTolPct
	}
	if p.ErrorStormPct <= 0 {
		p.ErrorStormPct = DefaultErrorStormPct
	}
	if p.CPUSatPct <= 0 {
		p.CPUSatPct = DefaultCPUSatPct
	}
	if p.MinAchievedPct <= 0 {
		p.MinAchievedPct = DefaultMinAchieved
	}
	if p.MaxSteps <= 0 {
		p.MaxSteps = DefaultMaxSteps
	}
	if p.Budget <= 0 {
		p.Budget = model.Duration(DefaultBudget)
	}
	if p.RateCap == "" {
		p.RateCap = "max-rate"
	}
	return p
}

// Validate checks a plan after WithDefaults.
func (p Plan) Validate() error {
	switch {
	case p.MaxRate <= 0:
		return errors.New("max rate must be positive")
	case p.StartRate > p.MaxRate:
		return fmt.Errorf("start rate %g is above the max rate %g", p.StartRate, p.MaxRate)
	case p.StepAdd <= 0 && (p.StepPct <= 0 || p.StepPct > 400):
		return errors.New("step-pct must be between 0 and 400 (or use step-add)")
	case p.Hold.D() < time.Second:
		return errors.New("hold must be at least 1s")
	case p.WarmupD() >= p.Budget.D():
		return errors.New("warm-up must be shorter than the budget")
	}
	return nil
}

// WarmupD is the warm-up duration; a negative Warmup means none.
func (p Plan) WarmupD() time.Duration { return max(0, p.Warmup.D()) }

// StepTime is the planned wall time of one step.
func (p Plan) StepTime() time.Duration { return p.WarmupD() + p.Hold.D() + stepOverhead }

// Measure is what a StepRunner observed during one step's hold period.
type Measure struct {
	OfferedRate float64 // iterations/s requested
	HoldSec     float64
	Iterations  uint64
	Requests    uint64
	Errors      uint64
	ErrorPct    float64
	P50Ms       float64
	P90Ms       float64
	P95Ms       float64
	P99Ms       float64
	MaxMs       float64
	AvgMs       float64
	Dropped     uint64
	CPUPercent  float64 // generator process CPU, % of one core (0 = not measured)
	CPUs        int
	PeakVUs     int
	ErrorKinds  map[string]uint64
	// SLAs are the criteria evaluated on this step's own merged histogram.
	SLAs []results.SLAResult
}

// StepRunner runs one step at a fixed arrival rate. Implementations must
// discard samples from the warm-up and return ctx.Err() when ctx ends.
type StepRunner interface {
	RunStep(ctx context.Context, rate float64, warmup, hold time.Duration) (Measure, error)
}

// Verdicts of one step.
const (
	VerdictPass      = "pass"
	VerdictBreach    = "breach"
	VerdictStorm     = "error-storm"
	VerdictGenerator = "generator-limited"
)

// Step is one row of the per-step table.
type Step struct {
	Index        int                 `json:"index"`
	Phase        string              `json:"phase"` // ramp | refine
	OfferedRate  float64             `json:"offeredRate"`
	AchievedRate float64             `json:"achievedRate"` // iterations/s
	RequestRate  float64             `json:"requestRate"`  // requests/s
	Iterations   uint64              `json:"iterations"`
	Requests     uint64              `json:"requests"`
	Errors       uint64              `json:"errors"`
	ErrorPct     float64             `json:"errorPct"`
	P50Ms        float64             `json:"p50Ms"`
	P90Ms        float64             `json:"p90Ms"`
	P95Ms        float64             `json:"p95Ms"`
	P99Ms        float64             `json:"p99Ms"`
	MaxMs        float64             `json:"maxMs"`
	AvgMs        float64             `json:"avgMs"`
	Dropped      uint64              `json:"dropped"`
	CPUPercent   float64             `json:"cpuPercent"`  // % of one core
	CPUSharePct  float64             `json:"cpuSharePct"` // % of all cores
	PeakVUs      int                 `json:"peakVUs"`
	HoldSec      float64             `json:"holdSec"`
	ErrorKinds   map[string]uint64   `json:"errorKinds,omitempty"`
	SLAs         []results.SLAResult `json:"slas,omitempty"`
	Verdict      string              `json:"verdict"`
	Reason       string              `json:"reason,omitempty"` // first breached criterion or limit
}

// Stop reasons.
const (
	StopBreach    = "sla-breach"
	StopStorm     = "error-storm"
	StopGenerator = "generator-saturated"
	StopMaxRate   = "max-rate"
	StopPolicy    = "policy-cap"
	StopBudget    = "budget"
	StopCanceled  = "canceled"
	StopMaxSteps  = "max-steps"
	StopError     = "error"
)

// Result of a capacity search.
type Result struct {
	ID        string              `json:"id"`
	Scenario  string              `json:"scenario"`
	StartedAt time.Time           `json:"startedAt"`
	EndedAt   time.Time           `json:"endedAt"`
	Plan      Plan                `json:"plan"`
	Criteria  []model.SLA         `json:"criteria"`
	Steps     []Step              `json:"steps"`
	Env       results.Environment `json:"environment"`

	// MaxSustainableRate is the highest tested rate (iterations/s) that met
	// every criterion; 0 when no step passed.
	MaxSustainableRate float64 `json:"maxSustainableRate"`
	MaxSustainableRPS  float64 `json:"maxSustainableRPS"` // requests/s achieved at that step
	FirstFailRate      float64 `json:"firstFailRate,omitempty"`
	FirstBreach        string  `json:"firstBreach,omitempty"`
	// Bound is "measured" when a failing rate bracketed the answer, otherwise
	// "lower-bound" (the target did not fail within the tested range, or the
	// generator limited the test).
	Bound            string    `json:"bound"`
	StopReason       string    `json:"stopReason"`
	StopDetail       string    `json:"stopDetail,omitempty"`
	GeneratorLimited bool      `json:"generatorLimited"`
	Knee             *Knee     `json:"knee,omitempty"`
	Plateau          *Plateau  `json:"plateau,omitempty"`
	KneeAgreement    string    `json:"kneeAgreement,omitempty"`
	Confidence       string    `json:"confidence"` // high | medium | low
	Limitations      []string  `json:"limitations"`
	Notes            []string  `json:"notes,omitempty"`
	Estimate         *Estimate `json:"estimate,omitempty"`
}

// Finder runs the staircase and refinement with a StepRunner.
type Finder struct {
	Plan   Plan
	Runner StepRunner
	// OnStep is called after every step (progress output). Optional.
	OnStep func(Step)
	// Clock returns the current time (tests may fake it). Default time.Now.
	Clock func() time.Time
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func (f *Finder) now() time.Time {
	if f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

// classify judges a step. Order matters: errors exploding first (target
// broken), then generator saturation (the step is not a valid measurement of
// the target), then SLA criteria, then achieved-vs-offered throughput.
func classify(p Plan, m Measure) (string, string) {
	if m.Requests == 0 {
		return VerdictStorm, "no requests completed during the hold period"
	}
	if m.ErrorPct >= p.ErrorStormPct {
		return VerdictStorm, fmt.Sprintf("error rate %.2f%% >= error-storm limit %g%%", m.ErrorPct, p.ErrorStormPct)
	}
	cpus := m.CPUs
	if cpus <= 0 {
		cpus = runtime.NumCPU()
	}
	if share := m.CPUPercent / float64(cpus); share > p.CPUSatPct {
		return VerdictGenerator, fmt.Sprintf("generator CPU %.0f%% of %d cores > %g%%", share, cpus, p.CPUSatPct)
	}
	var firstFail *results.SLAResult
	for i := range m.SLAs {
		if !m.SLAs[i].Passed {
			firstFail = &m.SLAs[i]
			break
		}
	}
	if m.Dropped > 0 && firstFail == nil {
		// The VU pool ran out while responses were still within SLA: the test
		// could not offer the rate, so the target was not measured at it.
		return VerdictGenerator, fmt.Sprintf("%d iterations dropped (VU pool exhausted) while latency met the SLAs; raise --max-vus", m.Dropped)
	}
	if firstFail != nil {
		return VerdictBreach, describeSLA(*firstFail)
	}
	if m.OfferedRate > 0 && m.HoldSec > 0 {
		achieved := float64(m.Iterations) / m.HoldSec
		if achieved < m.OfferedRate*p.MinAchievedPct/100 {
			return VerdictBreach, fmt.Sprintf("achieved %.2f it/s < %g%% of offered %.2f it/s", achieved, p.MinAchievedPct, m.OfferedRate)
		}
	}
	return VerdictPass, ""
}

func describeSLA(r results.SLAResult) string {
	scope := r.Scope
	if scope == "" {
		scope = "all requests"
	}
	unit := " ms"
	if r.Metric == "error_rate" {
		unit = "%"
	} else if r.Metric == "rps" {
		unit = "/s"
	}
	s := fmt.Sprintf("%s %s %s %g%s (actual %g%s)", scope, r.Metric, r.Op, r.Threshold, unit, r.Actual, unit)
	if r.Note != "" {
		s += ": " + r.Note
	}
	return s
}

func toStep(idx int, phase string, m Measure) Step {
	s := Step{Index: idx, Phase: phase, OfferedRate: r2(m.OfferedRate), Iterations: m.Iterations, Requests: m.Requests,
		Errors: m.Errors, ErrorPct: m.ErrorPct, P50Ms: m.P50Ms, P90Ms: m.P90Ms, P95Ms: m.P95Ms, P99Ms: m.P99Ms, MaxMs: m.MaxMs,
		AvgMs: m.AvgMs, Dropped: m.Dropped, CPUPercent: r2(m.CPUPercent), PeakVUs: m.PeakVUs, HoldSec: r2(m.HoldSec),
		ErrorKinds: m.ErrorKinds, SLAs: m.SLAs}
	if m.HoldSec > 0 {
		s.AchievedRate = r2(float64(m.Iterations) / m.HoldSec)
		s.RequestRate = r2(float64(m.Requests) / m.HoldSec)
	}
	cpus := m.CPUs
	if cpus <= 0 {
		cpus = runtime.NumCPU()
	}
	s.CPUSharePct = r2(m.CPUPercent / float64(cpus))
	return s
}

// Run executes the search. It returns a partial Result (StopReason
// "canceled") rather than an error when ctx ends, so callers can save it.
func (f *Finder) Run(ctx context.Context) (*Result, error) {
	p := f.Plan.WithDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if f.Runner == nil {
		return nil, errors.New("capacity: no step runner")
	}
	start := f.now()
	res := &Result{Plan: p, StartedAt: start, Env: results.Env(), Bound: "lower-bound"}
	deadline := start.Add(p.Budget.D())
	maxRate := p.MaxRate
	capReason := StopMaxRate
	if p.RateCap == "policy maxRPS" {
		capReason = StopPolicy
	}

	var lastGood, firstBad float64 // rates
	var lastGoodRPS float64
	consecutive := 0
	breachRunStart := 0.0
	noisyBelow := false

	run := func(rate float64, phase string) (Step, bool) {
		if ctx.Err() != nil {
			res.StopReason = StopCanceled
			return Step{}, false
		}
		if f.now().Add(p.StepTime()).After(deadline) {
			res.StopReason, res.StopDetail = StopBudget, fmt.Sprintf("the next step (%s) would exceed the time budget %s", p.StepTime(), p.Budget.D())
			return Step{}, false
		}
		m, err := f.Runner.RunStep(ctx, rate, p.WarmupD(), p.Hold.D())
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				res.StopReason = StopCanceled
			} else {
				res.StopReason, res.StopDetail = StopError, err.Error()
			}
			return Step{}, false
		}
		m.OfferedRate = rate
		st := toStep(len(res.Steps)+1, phase, m)
		st.Verdict, st.Reason = classify(p, m)
		res.Steps = append(res.Steps, st)
		if f.OnStep != nil {
			f.OnStep(st)
		}
		// Once requests per iteration are known, keep offered requests/s under the policy cap.
		if p.PolicyMaxRPS > 0 && m.Iterations > 0 && m.Requests > 0 {
			perIter := float64(m.Requests) / float64(m.Iterations)
			if c := math.Floor(p.PolicyMaxRPS/perIter*100) / 100; c < maxRate {
				maxRate, capReason = c, StopPolicy
				res.Notes = append(res.Notes, fmt.Sprintf("rate capped at %.2f it/s so that %.2f requests/iteration stay under the policy maxRPS %g", c, perIter, p.PolicyMaxRPS))
			}
		}
		return st, true
	}

	rate := p.StartRate
	for {
		if len(res.Steps) >= p.MaxSteps {
			res.StopReason = StopMaxSteps
			break
		}
		st, ok := run(rate, "ramp")
		if !ok {
			break
		}
		stop := false
		switch st.Verdict {
		case VerdictPass:
			if consecutive > 0 {
				noisyBelow = true
			}
			consecutive = 0
			if rate > lastGood {
				lastGood, lastGoodRPS = rate, st.RequestRate
			}
		case VerdictBreach:
			if consecutive == 0 {
				breachRunStart = rate
				res.FirstBreach = st.Reason
			}
			consecutive++
			if consecutive >= p.BreachSteps {
				res.StopReason, res.StopDetail = StopBreach, st.Reason
				stop = true
			}
		case VerdictStorm:
			if consecutive == 0 {
				breachRunStart = rate
				res.FirstBreach = st.Reason
			}
			res.StopReason, res.StopDetail = StopStorm, st.Reason
			stop = true
		case VerdictGenerator:
			res.GeneratorLimited = true
			res.StopReason, res.StopDetail = StopGenerator, st.Reason
			stop = true
		}
		if stop {
			break
		}
		if rate >= maxRate {
			if consecutive > 0 {
				// Ended inside a failing run that is shorter than BreachSteps: still a breach.
				res.StopReason, res.StopDetail = StopBreach, res.FirstBreach+" (rate cap reached before confirming)"
			} else {
				res.StopReason = capReason
				res.StopDetail = fmt.Sprintf("reached the rate cap %.2f it/s without breaching the criteria", maxRate)
			}
			break
		}
		next := rate * (1 + p.StepPct/100)
		if p.StepAdd > 0 {
			next = rate + p.StepAdd
		}
		rate = math.Min(r2(next), maxRate)
	}
	if res.StopReason == StopBreach || res.StopReason == StopStorm {
		firstBad = breachRunStart
	}

	// Refinement by bisection between the last passing and first failing rate.
	if firstBad > 0 && lastGood > 0 && firstBad > lastGood && !res.GeneratorLimited {
		good, bad := lastGood, firstBad
		for i := 0; i < p.RefineSteps; i++ {
			if (bad-good)/bad*100 <= p.RefineTolPct {
				break
			}
			mid := r2((good + bad) / 2)
			if mid <= good || mid >= bad {
				break
			}
			saved := res.StopReason
			st, ok := run(mid, "refine")
			if !ok {
				res.Notes = append(res.Notes, "refinement stopped early: "+res.StopReason+" "+res.StopDetail)
				if res.StopReason == StopCanceled {
					break
				}
				res.StopReason = saved
				break
			}
			res.StopReason = saved
			switch st.Verdict {
			case VerdictPass:
				good, lastGoodRPS = mid, st.RequestRate
			case VerdictBreach, VerdictStorm:
				bad = mid
			default:
				res.GeneratorLimited = true
				res.Notes = append(res.Notes, "refinement stopped: generator limited at "+fmt.Sprint(mid)+" it/s ("+st.Reason+")")
				i = p.RefineSteps
			}
		}
		lastGood, firstBad = good, bad
	}
	if lastGood > 0 && firstBad == 0 {
		// no failing rate: report the passing step with the highest rate
		for _, s := range res.Steps {
			if s.Verdict == VerdictPass && s.OfferedRate == lastGood {
				lastGoodRPS = s.RequestRate
			}
		}
	}
	res.MaxSustainableRate, res.MaxSustainableRPS, res.FirstFailRate = r2(lastGood), r2(lastGoodRPS), r2(firstBad)
	if firstBad > 0 && !res.GeneratorLimited && res.StopReason != StopCanceled {
		res.Bound = "measured"
	}
	if noisyBelow {
		res.Notes = append(res.Notes, "a lower rate failed once and a higher rate then passed: results are noisy; consider a longer --hold")
	}
	if lastGood == 0 && len(res.Steps) > 0 && (res.StopReason == StopBreach || res.StopReason == StopStorm) {
		res.Notes = append(res.Notes, fmt.Sprintf("the start rate %.2f it/s already failed the criteria; lower --start-rate", p.StartRate))
	}
	analyze(res, noisyBelow)
	res.EndedAt = f.now()
	return res, nil
}

// analyze adds knee/plateau detection, confidence and limitations.
func analyze(res *Result, noisy bool) {
	var valid []Step
	for _, s := range res.Steps {
		if s.Verdict != VerdictGenerator && s.Requests > 0 {
			valid = append(valid, s)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].OfferedRate < valid[j].OfferedRate })
	xs, ys := make([]float64, len(valid)), make([]float64, len(valid))
	for i, s := range valid {
		xs[i], ys[i] = s.RequestRate, s.P95Ms
	}
	if i, score, ok := Kneedle(xs, ys); ok {
		res.Knee = &Knee{OfferedRate: valid[i].OfferedRate, Throughput: valid[i].RequestRate, P95Ms: valid[i].P95Ms, Score: r2(score),
			Method: "Kneedle on p95 latency vs achieved requests/s (all valid steps, ordered by offered rate)"}
	}
	var ramp []Step
	for _, s := range valid {
		if s.Phase == "ramp" {
			ramp = append(ramp, s)
		}
	}
	off, ach := make([]float64, len(ramp)), make([]float64, len(ramp))
	for i, s := range ramp {
		off[i], ach[i] = s.OfferedRate, s.AchievedRate
	}
	if pl, ok := DetectPlateau(off, ach, DefaultPlateauGain); ok {
		res.Plateau = &pl
	}
	switch {
	case res.Knee != nil && res.Plateau != nil:
		a, b := res.Knee.OfferedRate, res.Plateau.LastScalingRate
		if math.Abs(a-b) <= 0.25*math.Max(a, b) {
			res.KneeAgreement = "agree (within 25%)"
		} else {
			res.KneeAgreement = fmt.Sprintf("disagree: latency knee at %.2f it/s, throughput stops scaling after %.2f it/s", a, b)
		}
	case res.Knee != nil:
		res.KneeAgreement = "latency knee found; throughput kept scaling in the tested range"
	case res.Plateau != nil:
		res.KneeAgreement = "throughput plateau found; no clear latency knee"
	default:
		res.KneeAgreement = "no knee or plateau in the tested range"
	}

	res.Confidence = "high"
	lower := func(c string) {
		order := map[string]int{"high": 2, "medium": 1, "low": 0}
		if order[c] < order[res.Confidence] {
			res.Confidence = c
		}
	}
	if res.Bound != "measured" {
		lower("low")
	}
	if len(valid) < 3 {
		lower("low")
	}
	if res.FirstFailRate > 0 && res.MaxSustainableRate > 0 && (res.FirstFailRate-res.MaxSustainableRate)/res.FirstFailRate*100 > 2*res.Plan.RefineTolPct {
		lower("medium")
	}
	if res.Knee != nil && res.Plateau != nil && res.KneeAgreement != "agree (within 25%)" {
		lower("medium")
	}
	if noisy {
		lower("medium")
	}
	res.Limitations = append(res.Limitations,
		"Measured from one load generator on this machine; the answer is the target's capacity as seen from here, for this scenario's traffic mix.",
		fmt.Sprintf("Each step holds a rate for %s after a %s warm-up; slow effects (memory growth, cache expiry, connection-pool exhaustion over minutes) are not captured.", res.Plan.Hold.D(), res.Plan.WarmupD()),
		"Autoscaling or rate-limited targets can hide or move the limit.",
	)
	if res.GeneratorLimited {
		res.Limitations = append(res.Limitations, "GENERATOR-LIMITED: the load generator (CPU or VU pool) saturated before the target failed. The reported rate is a lower bound of the target's capacity, not its limit.")
	}
	if res.Bound == "lower-bound" && !res.GeneratorLimited {
		res.Limitations = append(res.Limitations, "No failing rate was found: the reported rate is a lower bound, not the breaking point.")
	}
	if res.Plan.RefineSteps > 0 {
		res.Limitations = append(res.Limitations, "Refinement steps are single measurements (no consecutive-step confirmation).")
	}
	if res.Plan.RateCap == "policy maxRPS" || res.StopReason == StopPolicy {
		res.Limitations = append(res.Limitations, "The safety policy capped the rate; the target may sustain more.")
	}
}
