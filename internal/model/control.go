package model

import (
	"fmt"
	"math"
	"time"
)

// Control-flow step types (JMeter logic-controller parity, see
// docs/SCENARIO_FORMAT.md "Control flow"). Their parameters live in
// StepControl, which is embedded in Step so the JSON fields sit directly on
// the step object.
const (
	StepWhile      = "while"      // repeat steps while condition holds (capped)
	StepForeach    = "foreach"    // iterate input_1..input_N into var
	StepSwitch     = "switch"     // run the child selected by value (index or name)
	StepPercent    = "percent"    // run children on a deterministic share of passes
	StepRuntime    = "runtime"    // repeat children for a wall-clock duration
	StepThroughput = "throughput" // constant-throughput pause (JMeter CTT)
	StepRendezvous = "rendezvous" // barrier: wait until N VUs arrive (JMeter Synchronizing Timer)
	StepCritical   = "critical"   // run children under a named cross-VU lock
)

// Additional group selection modes.
const (
	SelectShuffle    = "shuffle"    // every enabled child once, in a random order (Random Order Controller)
	SelectRoundRobin = "roundRobin" // one child per execution, in rotation (Interleave Controller)
)

// Throughput step scopes.
const (
	ThroughputVU     = "vu"     // each VU paces itself at perMinute (JMeter "this thread only")
	ThroughputActive = "active" // each VU paces at perMinute / active VUs ("all active threads")
	ThroughputShared = "shared" // one schedule shared by all VUs ("all active threads (shared)")
)

// Arrival processes for the arrival-rate executors.
const (
	ArrivalsEven    = "even"    // fixed spacing 1/rate (default)
	ArrivalsPoisson = "poisson" // exponential gaps with mean 1/rate (seeded)
)

// Loop guards. Every repeating control step stops after MaxIterations passes
// even when its condition still holds, so a script can never spin forever.
const (
	DefaultMaxIterations     = 10_000
	HardMaxIterations        = 1_000_000
	DefaultRendezvousTimeout = 30 * time.Second
	MaxRendezvousTimeout     = time.Hour
)

// StepControl holds the parameters of the control-flow steps. Every field is
// optional and meaningful only on the step types named in its comment; the
// zero value changes nothing for the historical step types.
type StepControl struct {
	// MaxIterations caps the passes of while/foreach/runtime (default 10000
	// for while and foreach, 1000000 for runtime; never above 1000000).
	MaxIterations int `json:"maxIterations,omitempty"`
	// Input is the foreach variable prefix: input_1, input_2, ... are read
	// until one is missing (input1, input2, ... with noSeparator).
	Input string `json:"input,omitempty"`
	// Var receives the current foreach value.
	Var string `json:"var,omitempty"`
	// Start: foreach reads indexes start+1 .. end (JMeter semantics).
	Start int `json:"start,omitempty"`
	// End: last foreach index read (inclusive); 0 = until the first missing variable.
	End int `json:"end,omitempty"`
	// NoSeparator drops the "_" between input and index (JMeter useSeparator=false).
	NoSeparator bool `json:"noSeparator,omitempty"`
	// Value selects the switch child: empty = first child; digits = 0-based
	// index (out of range = first child); otherwise the child whose name
	// equals the value, else the child named "default" (case-insensitive),
	// else nothing runs.
	Value string `json:"value,omitempty"`
	// Percent: the percent step runs its children on this share (0-100] of
	// the passes, deterministically (JMeter Throughput Controller rule).
	Percent float64 `json:"percent,omitempty"`
	// Executions: the percent step runs its children only on the first N
	// passes instead (JMeter "total executions").
	Executions int `json:"executions,omitempty"`
	// Shared makes percent and roundRobin counters common to all VUs.
	Shared bool `json:"shared,omitempty"`
	// Duration: runtime repeats its children for this long; on while it is
	// an optional wall-clock cap.
	Duration Duration `json:"duration,omitempty"`
	// PerMinute is the throughput step's target rate (passes per minute).
	PerMinute float64 `json:"perMinute,omitempty"`
	// Scope is the throughput step's scope: vu (default), active or shared.
	Scope string `json:"scope,omitempty"`
	// Group is the rendezvous size; 0 = the number of VUs currently active.
	Group int `json:"group,omitempty"`
	// Timeout releases a rendezvous that is not full in time (default 30s).
	Timeout Duration `json:"timeout,omitempty"`
	// Key names shared state: throughput, rendezvous, critical and shared
	// percent/roundRobin steps with the same type and key share one schedule,
	// barrier, lock or counter. Default: the step itself (critical: its name).
	Key string `json:"key,omitempty"`
}

// WorkloadControl holds workload options added for JMeter parity. It is
// embedded in Workload.
type WorkloadControl struct {
	// Arrivals: "even" (default) or "poisson" for the arrival-rate executors.
	Arrivals string `json:"arrivals,omitempty"`
	// NewUserEachIteration gives a VU a fresh cookie jar, auth cache and (in
	// per-vu connection mode) fresh connections at the start of every
	// iteration after the first (JMeter "same user on each iteration" off).
	// Variables are kept.
	NewUserEachIteration bool `json:"newUserEachIteration,omitempty"`
}

// IsControlStep reports whether t is one of the control-flow step types above.
func IsControlStep(t string) bool {
	switch t {
	case StepWhile, StepForeach, StepSwitch, StepPercent, StepRuntime, StepThroughput, StepRendezvous, StepCritical:
		return true
	}
	return false
}

// EffectiveMaxIterations returns the pass cap for a repeating control step.
func (s *Step) EffectiveMaxIterations() int {
	switch {
	case s.MaxIterations > 0:
		return min(s.MaxIterations, HardMaxIterations)
	case s.Type == StepRuntime:
		return HardMaxIterations
	default:
		return DefaultMaxIterations
	}
}

// EffectiveTimeout returns the rendezvous timeout.
func (s *Step) EffectiveTimeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout.D()
	}
	return DefaultRendezvousTimeout
}

func hasEnabled(steps []Step) bool {
	for _, c := range steps {
		if !c.Disabled {
			return true
		}
	}
	return false
}

// validateControl checks one control-flow step (not its children).
func validateControl(where string, st *Step) []string {
	var p []string
	bad := func(f string, a ...any) { p = append(p, where+": "+fmt.Sprintf(f, a...)) }
	if st.MaxIterations < 0 || st.MaxIterations > HardMaxIterations {
		bad("maxIterations must be between 0 and %d", HardMaxIterations)
	}
	if st.Duration < 0 {
		bad("duration must be >= 0")
	}
	needChildren := func() {
		if !hasEnabled(st.Steps) {
			bad("%s step needs at least one enabled inner step", st.Type)
		}
	}
	noChildren := func() {
		if len(st.Steps) > 0 {
			bad("%s step takes no inner steps", st.Type)
		}
	}
	switch st.Type {
	case StepWhile:
		needChildren()
	case StepForeach:
		needChildren()
		if st.Input == "" {
			bad("foreach needs input (variable prefix)")
		}
		if st.Var == "" {
			bad("foreach needs var (output variable)")
		}
		if st.Start < 0 {
			bad("foreach start must be >= 0")
		}
		if st.End != 0 && st.End <= st.Start {
			bad("foreach end must be 0 (until missing) or > start")
		}
	case StepSwitch:
		needChildren()
	case StepPercent:
		needChildren()
		switch {
		case st.Executions < 0:
			bad("executions must be >= 0")
		case st.Executions > 0 && st.Percent != 0:
			bad("set either percent or executions, not both")
		case st.Executions == 0 && (math.IsNaN(st.Percent) || st.Percent <= 0 || st.Percent > 100):
			bad("percent must be in (0, 100] (or set executions); disable the step to never run it")
		}
	case StepRuntime:
		needChildren()
		if st.Duration <= 0 {
			bad("runtime needs duration > 0")
		}
	case StepThroughput:
		noChildren()
		if math.IsNaN(st.PerMinute) || math.IsInf(st.PerMinute, 0) || st.PerMinute <= 0 {
			bad("throughput needs perMinute > 0")
		}
		switch st.Scope {
		case "", ThroughputVU, ThroughputActive, ThroughputShared:
		default:
			bad("unknown throughput scope %q (expected vu, active or shared)", st.Scope)
		}
	case StepRendezvous:
		noChildren()
		if st.Group < 0 {
			bad("rendezvous group must be >= 0 (0 = active VUs)")
		}
		if st.Timeout < 0 || st.Timeout.D() > MaxRendezvousTimeout {
			bad("rendezvous timeout must be between 0 (default 30s) and %s", MaxRendezvousTimeout)
		}
	case StepCritical:
		needChildren()
	}
	return p
}

// validateWorkloadControl checks the WorkloadControl fields.
func (w Workload) validateControl() []string {
	switch w.Arrivals {
	case "", ArrivalsEven:
	case ArrivalsPoisson:
		if w.Executor != ExecArrivalRate && w.Executor != ExecRampingRate {
			return []string{"workload.arrivals \"poisson\" needs an arrival-rate executor"}
		}
	default:
		return []string{fmt.Sprintf("unknown workload.arrivals %q (expected even or poisson)", w.Arrivals)}
	}
	return nil
}
