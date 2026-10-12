package correlate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lelanga/phoenix/internal/engine"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/safety"
)

type StepResult struct {
	Step   string `json:"step"`
	Status int    `json:"status"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type Replay struct {
	OK       bool         `json:"ok"`
	Failures int          `json:"failures"`
	Steps    []StepResult `json:"steps"`
	Error    string       `json:"error,omitempty"`
}

type Detail struct {
	Variable  string   `json:"variable"`
	Extracted bool     `json:"extracted"`
	UsedIn    []string `json:"usedIn"`
	Missing   []string `json:"notUsedIn,omitempty"`
	OK        bool     `json:"ok"`
	Message   string   `json:"message"`
}

type Validation struct {
	OK        bool     `json:"ok"`
	Original  *Replay  `json:"originalReplay"`
	WithRules *Replay  `json:"withRulesReplay"`
	Details   []Detail `json:"details"`
	Summary   string   `json:"summary"`
}

// ReplayOnce runs one iteration with one VU and returns every exchange.
func ReplayOnce(ctx context.Context, sc *model.Scenario, baseDir string, pol safety.Policy) ([]engine.Exchange, error) {
	c := sc.Clone()
	c.Workload = model.Workload{Executor: model.ExecPerVUIterations, VUs: 1, Iterations: 1, Duration: model.Duration(2 * time.Minute), GracefulStop: model.Duration(5 * time.Second)}
	z := 0.0
	c.Workload.ThinkTimeScale = &z
	var mu sync.Mutex
	var out []engine.Exchange
	r, err := engine.Prepare(c, engine.Options{BaseDir: baseDir, Policy: pol, MaxIters: 1, Inspect: func(e engine.Exchange) {
		mu.Lock()
		out = append(out, e)
		mu.Unlock()
	}})
	if err != nil {
		return nil, err
	}
	r.Start(ctx)
	select {
	case <-r.Done():
	case <-ctx.Done():
		r.Stop()
		r.Stop()
		<-r.Done()
	}
	return out, r.Err()
}

func summarize(exs []engine.Exchange, err error) *Replay {
	rp := &Replay{OK: err == nil}
	if err != nil {
		rp.Error = err.Error()
	}
	for _, e := range exs {
		rp.Steps = append(rp.Steps, StepResult{Step: e.Step, Status: e.Status, OK: e.OK, Error: e.Error})
		if !e.OK {
			rp.Failures++
			rp.OK = false
		}
	}
	return rp
}

// Validate replays the original and the corrected scenario and checks that each
// applied rule extracted a value and that destinations actually used it.
func Validate(ctx context.Context, original, corrected *model.Scenario, applied []Candidate, baseDir string, pol safety.Policy) *Validation {
	v := &Validation{}
	oex, oerr := ReplayOnce(ctx, original, baseDir, pol)
	v.Original = summarize(oex, oerr)
	cex, cerr := ReplayOnce(ctx, corrected, baseDir, pol)
	v.WithRules = summarize(cex, cerr)
	byStep := map[string]engine.Exchange{}
	for _, e := range cex {
		if _, seen := byStep[e.Step]; !seen {
			byStep[e.Step] = e
		}
	}
	v.OK = v.WithRules.OK
	for _, c := range applied {
		d := Detail{Variable: c.Variable}
		if src, ok := byStep[c.Source.Step]; ok {
			_, d.Extracted = src.Extracted[c.Variable]
		}
		for _, dst := range c.Destinations {
			e, ok := byStep[dst.Step]
			used := false
			if ok {
				for _, u := range e.VarsUsed {
					if u == c.Variable {
						used = true
					}
				}
			}
			if used && e.OK {
				d.UsedIn = append(d.UsedIn, dst.Step)
			} else {
				d.Missing = append(d.Missing, dst.Step)
			}
		}
		d.OK = d.Extracted && len(d.Missing) == 0
		switch {
		case !d.Extracted:
			d.Message = "Extractor found no value during replay."
		case len(d.Missing) > 0:
			d.Message = fmt.Sprintf("Value extracted, but %d destination request(s) did not use it successfully.", len(d.Missing))
		default:
			d.Message = fmt.Sprintf("Extracted during replay and used successfully by %d request(s).", len(d.UsedIn))
		}
		v.OK = v.OK && d.OK
		v.Details = append(v.Details, d)
	}
	v.Summary = fmt.Sprintf("Original replay: %d failing request(s). With rules: %d failing request(s). %d/%d rules validated.",
		v.Original.Failures, v.WithRules.Failures, countOK(v.Details), len(v.Details))
	return v
}

func countOK(ds []Detail) int {
	n := 0
	for _, d := range ds {
		if d.OK {
			n++
		}
	}
	return n
}
