package model

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestDistributionValidate(t *testing.T) {
	bad := []Distribution{{Type: "weibull"}, {Type: DistLogNormal}, {Type: DistExponential}, {Type: DistGamma, Shape: 2}}
	for _, d := range bad {
		if d.Validate() == nil {
			t.Errorf("%+v should be invalid", d)
		}
	}
	good := []Distribution{{Type: DistLogNormal, Mu: 1, Sigma: 0.5}, {Type: DistExponential, Mean: 3}, {Type: DistGamma, Shape: 2, Scale: 1}, {Type: DistUniform}}
	for _, d := range good {
		if err := d.Validate(); err != nil {
			t.Errorf("%+v: %v", d, err)
		}
	}
}

func TestThinkSampleSeededMoments(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	th := Think{Distribution: &Distribution{Type: DistLogNormal, Mu: 0.5, Sigma: 0.4}}
	var sum float64
	const n = 20000
	for i := 0; i < n; i++ {
		sum += th.Sample(r).Seconds()
	}
	want := math.Exp(0.5 + 0.4*0.4/2)
	if got := sum / n; math.Abs(got-want) > 0.03*want {
		t.Errorf("lognormal mean = %.3f, want %.3f", got, want)
	}
	// Same seed, same sequence.
	a := th.Sample(rand.New(rand.NewPCG(1, 2)))
	b := th.Sample(rand.New(rand.NewPCG(1, 2)))
	if a != b {
		t.Errorf("not deterministic: %v %v", a, b)
	}
}

func TestThinkSampleClampsAndLegacy(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	th := Think{Min: Duration(2 * time.Second), Max: Duration(3 * time.Second), Distribution: &Distribution{Type: DistExponential, Mean: 100}}
	for i := 0; i < 500; i++ {
		if d := th.Sample(r); d < 2*time.Second || d > 3*time.Second {
			t.Fatalf("sample %v outside clamp", d)
		}
	}
	legacy := Think{Min: Duration(time.Second), Max: Duration(2 * time.Second)}
	for i := 0; i < 100; i++ {
		if d := legacy.Sample(r); d < time.Second || d >= 2*time.Second {
			t.Fatalf("legacy sample %v", d)
		}
	}
	fixed := Think{Min: Duration(time.Second)}
	if d := fixed.Sample(r); d != time.Second {
		t.Errorf("fixed think = %v", d)
	}
}

func TestGammaMean(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	for _, k := range []float64{0.5, 3} {
		d := Distribution{Type: DistGamma, Shape: k, Scale: 2}
		var s float64
		for i := 0; i < 40000; i++ {
			s += d.SampleSeconds(r)
		}
		if got := s / 40000; math.Abs(got-k*2) > 0.04*k*2 {
			t.Errorf("gamma(%v) mean %.3f want %.3f", k, got, k*2)
		}
	}
}

func TestWeightedGroupAndThinkValidate(t *testing.T) {
	w := func(f float64) *float64 { return &f }
	req := func(n string, wt *float64) Step {
		return Step{Type: StepRequest, Name: n, Weight: wt, Request: &Request{Method: "GET", URL: "/" + n}}
	}
	sc := NewScenario("x")
	sc.Steps = []Step{{Type: StepGroup, Select: SelectWeighted, Steps: []Step{req("a", w(3)), req("b", nil)}}}
	if p := sc.Validate(); len(p) != 0 {
		t.Fatalf("valid weighted group rejected: %v", p)
	}
	check := func(steps []Step, want string) {
		t.Helper()
		s := NewScenario("x")
		s.Steps = steps
		p := strings.Join(s.Validate(), ";")
		if !strings.Contains(p, want) {
			t.Errorf("want problem containing %q, got %q", want, p)
		}
	}
	check([]Step{{Type: StepGroup, Select: "random", Steps: []Step{req("a", nil)}}}, "unknown select mode")
	check([]Step{{Type: StepLoop, Count: 1, Select: SelectWeighted, Steps: []Step{req("a", nil)}}}, "only valid on a group")
	check([]Step{{Type: StepGroup, Select: SelectWeighted, Steps: []Step{req("a", w(0))}}}, "weight > 0")
	check([]Step{{Type: StepGroup, Select: SelectWeighted, Steps: []Step{req("a", w(-1)), req("b", nil)}}}, "weights must be")
	check([]Step{req("a", nil), {Type: StepThink, Think: &Think{Distribution: &Distribution{Type: "lognormal"}}}}, "sigma")

	// JSON round trip keeps the new fields; absent fields stay absent.
	b, _ := json.Marshal(Think{Min: Duration(time.Second)})
	if strings.Contains(string(b), "distribution") {
		t.Errorf("legacy think JSON changed: %s", b)
	}
	var th Think
	if err := json.Unmarshal([]byte(`{"min":"1s","distribution":{"type":"gamma","shape":2,"scale":3}}`), &th); err != nil || th.Distribution.Shape != 2 {
		t.Errorf("unmarshal: %v %+v", err, th)
	}
}
