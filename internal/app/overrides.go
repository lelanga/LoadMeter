package app

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/safety"
)

// Overrides are simple workload controls exposed by the CLI, UI, API and MCP.
type Overrides struct {
	VUs      int     `json:"vus,omitempty"`
	Duration string  `json:"duration,omitempty"`
	RampUp   string  `json:"rampUp,omitempty"`
	Rate     float64 `json:"rate,omitempty"`
	MaxVUs   int     `json:"maxVUs,omitempty"`
}

func (o Overrides) Empty() bool {
	return o.VUs == 0 && o.Duration == "" && o.RampUp == "" && o.Rate == 0
}

// ApplyOverrides rewrites the workload. VUs (closed model) and Rate (open
// model) are different things: VUs fix concurrency, Rate fixes arrivals/s.
func ApplyOverrides(sc *model.Scenario, o Overrides) error {
	var dur time.Duration
	if o.Duration != "" {
		d, err := time.ParseDuration(o.Duration)
		if err != nil || d <= 0 {
			return fmt.Errorf("duration %q: use a value like 30s, 5m or 1h", o.Duration)
		}
		dur = d
	}
	if o.VUs < 0 || o.Rate < 0 || o.MaxVUs < 0 {
		return fmt.Errorf("VUs, rate and maxVUs must not be negative")
	}
	w := &sc.Workload
	keep := func(n model.Workload) model.Workload {
		n.Pacing, n.GracefulStop, n.ThinkTimeScale, n.OnError = w.Pacing, w.GracefulStop, w.ThinkTimeScale, w.OnError
		return n
	}
	switch {
	case o.Rate > 0:
		if dur == 0 {
			dur = w.PlannedDuration()
		}
		if dur == 0 {
			dur = time.Minute
		}
		*w = keep(model.Workload{Executor: model.ExecArrivalRate, Rate: o.Rate, Duration: model.Duration(dur), VUs: max(o.VUs, 1),
			MaxVUs: max(o.MaxVUs, o.VUs, int(o.Rate*2), 10)})
	case o.VUs > 0:
		if dur == 0 {
			dur = w.PlannedDuration()
		}
		if dur == 0 {
			dur = 30 * time.Second
		}
		if o.RampUp != "" && o.RampUp != "0" && o.RampUp != "0s" {
			ru, err := time.ParseDuration(o.RampUp)
			if err != nil {
				return fmt.Errorf("ramp-up %q: %w", o.RampUp, err)
			}
			if ru >= dur {
				return fmt.Errorf("ramp-up must be shorter than the duration")
			}
			*w = keep(model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{
				{Duration: model.Duration(ru), Target: float64(o.VUs)}, {Duration: model.Duration(dur - ru), Target: float64(o.VUs)}}})
		} else {
			*w = keep(model.Workload{Executor: model.ExecConstantVUs, VUs: o.VUs, Duration: model.Duration(dur)})
		}
	case dur > 0:
		switch w.Executor {
		case model.ExecConstantVUs, model.ExecArrivalRate, model.ExecPerVUIterations:
			w.Duration = model.Duration(dur)
		default:
			return fmt.Errorf("a duration alone only applies to fixed-VU or fixed-rate workloads; also set VUs or rate")
		}
	}
	return nil
}

// TargetHosts lists literal hosts referenced by requests after variable expansion.
func TargetHosts(sc *model.Scenario) []string {
	seen := map[string]bool{}
	expand := func(s string) string {
		for i := 0; i < 3 && strings.Contains(s, "${"); i++ {
			for k, v := range sc.Variables {
				s = strings.ReplaceAll(s, "${"+k+"}", v)
			}
		}
		return s
	}
	base := expand(sc.Defaults.BaseURL)
	for _, st := range sc.AllRequests() {
		u := expand(st.Request.URL)
		if !strings.Contains(u, "://") {
			u = base
		}
		if i := strings.Index(u, "://"); i >= 0 {
			h := u[i+3:]
			if j := strings.IndexAny(h, "/?#"); j >= 0 {
				h = h[:j]
			}
			if hh, _, err := net.SplitHostPort(h); err == nil {
				h = hh
			}
			if h != "" && !strings.Contains(h, "${") {
				seen[strings.Trim(h, "[]")] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// ErrNeedsConfirmation signals a high-impact test that needs explicit confirmation.
type ErrNeedsConfirmation struct{ Reason string }

func (e *ErrNeedsConfirmation) Error() string { return e.Reason }

// CheckTargets enforces the allowlist and high-impact confirmation before a run.
// It returns whether any target is non-local.
func CheckTargets(sc *model.Scenario, pol safety.Policy, confirmed bool) (bool, error) {
	remote := false
	for _, h := range TargetHosts(sc) {
		if !pol.HostAllowed(h) {
			return false, &safety.ErrHostNotAllowed{Host: h}
		}
		if !safety.IsLoopback(h) {
			remote = true
		}
	}
	if remote && !confirmed {
		if v := sc.Workload.MaxPlannedVUs(); v > pol.RequireConfirmAbove || sc.Workload.Rate > float64(pol.RequireConfirmAbove) {
			return true, &ErrNeedsConfirmation{Reason: fmt.Sprintf("This test targets a non-local system with up to %d VUs. Only load test systems you are authorized to test; confirm to proceed.", v)}
		}
	}
	return remote, nil
}
