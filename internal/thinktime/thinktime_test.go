package thinktime

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/lelanga/loadmeter/internal/model"
)

func sample(d model.Distribution, n int, seed uint64) []float64 {
	r := rand.New(rand.NewPCG(seed, 99))
	out := make([]float64, n)
	for i := range out {
		out[i] = d.SampleSeconds(r)
	}
	return out
}

func near(t *testing.T, name string, got, want, relTol float64) {
	t.Helper()
	if math.Abs(got-want) > relTol*math.Abs(want) {
		t.Errorf("%s = %.4f, want %.4f (±%.0f%%)", name, got, want, relTol*100)
	}
}

func TestFitRecoversLogNormal(t *testing.T) {
	r, err := Fit(sample(model.Distribution{Type: "lognormal", Mu: 1.5, Sigma: 0.8}, 5000, 1))
	if err != nil {
		t.Fatal(err)
	}
	if r.Dist != "lognormal" {
		t.Fatalf("AIC picked %s, want lognormal (%+v)", r.Dist, r.Candidates)
	}
	near(t, "mu", r.Params["mu"], 1.5, 0.03)
	near(t, "sigma", r.Params["sigma"], 0.8, 0.04)
	if r.N != 5000 {
		t.Errorf("n=%d", r.N)
	}
}

func TestFitRecoversExponential(t *testing.T) {
	r, err := Fit(sample(model.Distribution{Type: "exponential", Mean: 12}, 5000, 2))
	if err != nil {
		t.Fatal(err)
	}
	if r.Dist != "exponential" {
		t.Fatalf("AIC picked %s, want exponential (%+v)", r.Dist, r.Candidates)
	}
	near(t, "mean", r.Params["mean"], 12, 0.05)
}

func TestFitRecoversGamma(t *testing.T) {
	r, err := Fit(sample(model.Distribution{Type: "gamma", Shape: 4, Scale: 2.5}, 5000, 3))
	if err != nil {
		t.Fatal(err)
	}
	if r.Dist != "gamma" {
		t.Fatalf("AIC picked %s, want gamma (%+v)", r.Dist, r.Candidates)
	}
	near(t, "shape", r.Params["shape"], 4, 0.06)
	near(t, "scale", r.Params["scale"], 2.5, 0.06)
}

func TestFitGammaShapeBelowOne(t *testing.T) {
	r, err := Fit(sample(model.Distribution{Type: "gamma", Shape: 0.6, Scale: 10}, 8000, 4))
	if err != nil {
		t.Fatal(err)
	}
	var g *Candidate
	for i := range r.Candidates {
		if r.Candidates[i].Dist == "gamma" {
			g = &r.Candidates[i]
		}
	}
	if g == nil {
		t.Fatal("no gamma candidate")
	}
	near(t, "shape", g.Params["shape"], 0.6, 0.06)
}

func TestFitTooFewAndBadInput(t *testing.T) {
	if _, err := Fit([]float64{1, 2, 3}); !errors.Is(err, ErrTooFewSamples) {
		t.Errorf("want ErrTooFewSamples, got %v", err)
	}
	gaps := make([]float64, 5)
	gaps = append(gaps, -1, 0, math.NaN(), math.Inf(1))
	if _, err := Fit(gaps); !errors.Is(err, ErrTooFewSamples) {
		t.Errorf("non-positive values must be ignored, got %v", err)
	}
	same := make([]float64, 50)
	for i := range same {
		same[i] = 5
	}
	r, err := Fit(same) // degenerate: only exponential is defined
	if err != nil || r.Dist != "exponential" {
		t.Errorf("degenerate sample: %v %+v", err, r)
	}
}

func TestDigamma(t *testing.T) {
	near(t, "psi(1)", digamma(1), -0.5772156649, 1e-8)
	near(t, "psi'(1)", trigamma(1), math.Pi*math.Pi/6, 1e-8)
}
