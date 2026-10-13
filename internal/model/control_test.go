package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ctlScenario(steps ...Step) *Scenario {
	s := NewScenario("c")
	s.Defaults.BaseURL = "http://127.0.0.1:1"
	s.Steps = steps
	return s
}

func req() Step {
	return Step{Type: StepRequest, Name: "r", Request: &Request{Method: "GET", URL: "/"}}
}

func TestControlStepsJSONFlatAndBackwardCompatible(t *testing.T) {
	old := `{"version":"loadmeter/v1","name":"x","steps":[{"type":"loop","count":2,"steps":[{"type":"think","think":{"min":"1s"}}]}],"workload":{"executor":"constant-vus","vus":1,"duration":"1s"}}`
	var s Scenario
	if err := json.Unmarshal([]byte(old), &s); err != nil {
		t.Fatal(err)
	}
	if p := s.Validate(); len(p) > 0 {
		t.Fatalf("legacy scenario invalid: %v", p)
	}
	b, _ := json.Marshal(&s)
	for _, k := range []string{"maxIterations", "perMinute", "arrivals", "newUserEachIteration", "key", "timeout"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("zero-valued %s leaked into legacy JSON: %s", k, b)
		}
	}
	src := `{"type":"while","name":"w","condition":"${a} != b","maxIterations":5,"duration":"2s","steps":[{"type":"think","think":{"min":"1ms"}}]}`
	var st Step
	if err := json.Unmarshal([]byte(src), &st); err != nil {
		t.Fatal(err)
	}
	if st.MaxIterations != 5 || st.Duration.D() != 2*time.Second || st.Cond != "${a} != b" {
		t.Fatalf("flat fields not decoded: %+v", st)
	}
	w := `{"executor":"constant-arrival-rate","rate":5,"duration":"1s","arrivals":"poisson","newUserEachIteration":true}`
	var wl Workload
	if err := json.Unmarshal([]byte(w), &wl); err != nil || wl.Arrivals != ArrivalsPoisson || !wl.NewUserEachIteration {
		t.Fatalf("workload control fields: %+v %v", wl, err)
	}
}

func TestControlValidate(t *testing.T) {
	ok := []Step{
		{Type: StepWhile, Cond: "${x}", Steps: []Step{req()}},
		{Type: StepForeach, StepControl: StepControl{Input: "id", Var: "v"}, Steps: []Step{req()}},
		{Type: StepForeach, StepControl: StepControl{Input: "id", Var: "v", Start: 1, End: 3}, Steps: []Step{req()}},
		{Type: StepSwitch, Steps: []Step{req()}},
		{Type: StepPercent, StepControl: StepControl{Percent: 50}, Steps: []Step{req()}},
		{Type: StepPercent, StepControl: StepControl{Executions: 2, Shared: true}, Steps: []Step{req()}},
		{Type: StepRuntime, StepControl: StepControl{Duration: Duration(time.Second)}, Steps: []Step{req()}},
		{Type: StepThroughput, StepControl: StepControl{PerMinute: 60, Scope: ThroughputShared}},
		{Type: StepRendezvous, StepControl: StepControl{Group: 3, Timeout: Duration(time.Second)}},
		{Type: StepCritical, StepControl: StepControl{Key: "k"}, Steps: []Step{req()}},
		{Type: StepGroup, Select: SelectShuffle, Steps: []Step{req()}},
		{Type: StepGroup, Select: SelectRoundRobin, StepControl: StepControl{Shared: true}, Steps: []Step{req()}},
	}
	for i, st := range ok {
		if p := ctlScenario(st).Validate(); len(p) > 0 {
			t.Errorf("case %d (%s) should be valid: %v", i, st.Type, p)
		}
	}
	disabled := req()
	disabled.Disabled = true
	bad := map[string]Step{
		"enabled inner step":    {Type: StepWhile, Steps: []Step{disabled}},
		"maxIterations must be": {Type: StepWhile, StepControl: StepControl{MaxIterations: HardMaxIterations + 1}, Steps: []Step{req()}},
		"foreach needs input":   {Type: StepForeach, StepControl: StepControl{Var: "v"}, Steps: []Step{req()}},
		"foreach needs var":     {Type: StepForeach, StepControl: StepControl{Input: "i"}, Steps: []Step{req()}},
		"foreach end":           {Type: StepForeach, StepControl: StepControl{Input: "i", Var: "v", Start: 3, End: 2}, Steps: []Step{req()}},
		"percent must be":       {Type: StepPercent, StepControl: StepControl{Percent: 150}, Steps: []Step{req()}},
		"not both":              {Type: StepPercent, StepControl: StepControl{Percent: 10, Executions: 1}, Steps: []Step{req()}},
		"runtime needs":         {Type: StepRuntime, Steps: []Step{req()}},
		"perMinute":             {Type: StepThroughput},
		"takes no inner steps":  {Type: StepThroughput, StepControl: StepControl{PerMinute: 1}, Steps: []Step{req()}},
		"unknown throughput":    {Type: StepThroughput, StepControl: StepControl{PerMinute: 1, Scope: "group"}},
		"rendezvous group":      {Type: StepRendezvous, StepControl: StepControl{Group: -1}},
		"rendezvous timeout":    {Type: StepRendezvous, StepControl: StepControl{Timeout: Duration(2 * time.Hour)}},
		"unknown select":        {Type: StepGroup, Select: "random", Steps: []Step{req()}},
		"select is only valid":  {Type: StepLoop, Count: 1, Select: SelectShuffle, Steps: []Step{req()}},
	}
	for want, st := range bad {
		p := ctlScenario(st).Validate()
		if !strings.Contains(strings.Join(p, "|"), want) {
			t.Errorf("%s: problems %v do not mention %q", st.Type, p, want)
		}
	}
	// Children of control steps are validated too.
	nested := Step{Type: StepWhile, Steps: []Step{{Type: StepRequest}}}
	if p := ctlScenario(nested).Validate(); !strings.Contains(strings.Join(p, "|"), "steps[0].steps[0]") {
		t.Errorf("nested problems not reported: %v", p)
	}
	s := ctlScenario(req())
	s.Workload.Arrivals = ArrivalsPoisson
	if p := s.Validate(); !strings.Contains(strings.Join(p, "|"), "arrival-rate executor") {
		t.Errorf("poisson on constant-vus accepted: %v", p)
	}
	s.Workload = Workload{Executor: ExecArrivalRate, Rate: 1, Duration: Duration(time.Second), WorkloadControl: WorkloadControl{Arrivals: "bursty"}}
	if p := s.Validate(); !strings.Contains(strings.Join(p, "|"), "unknown workload.arrivals") {
		t.Errorf("unknown arrivals accepted: %v", p)
	}
}

func TestEffectiveLimits(t *testing.T) {
	cases := []struct {
		st   Step
		want int
	}{
		{Step{Type: StepWhile}, DefaultMaxIterations},
		{Step{Type: StepRuntime}, HardMaxIterations},
		{Step{Type: StepForeach, StepControl: StepControl{MaxIterations: 3}}, 3},
		{Step{Type: StepWhile, StepControl: StepControl{MaxIterations: 5 * HardMaxIterations}}, HardMaxIterations},
	}
	for _, c := range cases {
		if got := c.st.EffectiveMaxIterations(); got != c.want {
			t.Errorf("%s max %d, want %d", c.st.Type, got, c.want)
		}
	}
	if (&Step{}).EffectiveTimeout() != DefaultRendezvousTimeout {
		t.Error("default rendezvous timeout")
	}
	if !IsControlStep(StepCritical) || IsControlStep(StepLoop) {
		t.Error("IsControlStep")
	}
}
