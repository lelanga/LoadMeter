package model

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Distribution types for Think.Distribution. Parameters are in seconds.
const (
	DistLogNormal   = "lognormal"   // mu, sigma: mean and standard deviation of ln(seconds)
	DistExponential = "exponential" // mean: seconds
	DistGamma       = "gamma"       // shape, scale (seconds)
	DistUniform     = "uniform"     // no parameters: uniform in [think.min, think.max]
)

// Distribution describes a probability distribution for think times.
type Distribution struct {
	Type  string  `json:"type"`
	Mu    float64 `json:"mu,omitempty"`
	Sigma float64 `json:"sigma,omitempty"`
	Mean  float64 `json:"mean,omitempty"`
	Shape float64 `json:"shape,omitempty"`
	Scale float64 `json:"scale,omitempty"`
}

func finitePos(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

// Validate reports a problem with the type or parameters.
func (d *Distribution) Validate() error {
	switch d.Type {
	case DistLogNormal:
		if !finitePos(d.Sigma) || math.IsNaN(d.Mu) || math.IsInf(d.Mu, 0) || math.Abs(d.Mu) > 20 {
			return fmt.Errorf("lognormal distribution needs sigma > 0 and a finite mu")
		}
	case DistExponential:
		if !finitePos(d.Mean) {
			return fmt.Errorf("exponential distribution needs mean > 0")
		}
	case DistGamma:
		if !finitePos(d.Shape) || !finitePos(d.Scale) {
			return fmt.Errorf("gamma distribution needs shape > 0 and scale > 0")
		}
	case DistUniform:
	default:
		return fmt.Errorf("unknown distribution type %q (expected lognormal, exponential, gamma or uniform)", d.Type)
	}
	return nil
}

// SampleSeconds draws one value in seconds. Uniform returns 0 (the caller
// applies min/max). Invalid parameters return 0.
func (d *Distribution) SampleSeconds(r *rand.Rand) float64 {
	if d.Validate() != nil {
		return 0
	}
	switch d.Type {
	case DistLogNormal:
		return math.Exp(d.Mu + d.Sigma*r.NormFloat64())
	case DistExponential:
		return d.Mean * r.ExpFloat64()
	case DistGamma:
		return d.Scale * gammaStd(r, d.Shape)
	}
	return 0
}

// gammaStd samples Gamma(shape, 1) with the Marsaglia-Tsang method.
func gammaStd(r *rand.Rand, k float64) float64 {
	if k < 1 { // boost: Gamma(k) = Gamma(k+1) * U^(1/k)
		return gammaStd(r, k+1) * math.Pow(r.Float64(), 1/k)
	}
	dd := k - 1.0/3
	c := 1 / math.Sqrt(9*dd)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+dd*(1-v+math.Log(v)) {
			return dd * v
		}
	}
}

// Sample draws a think duration: from the distribution when set (clamped to
// [Min, Max], Max = 0 meaning unbounded), otherwise uniform in [Min, Max].
func (t *Think) Sample(r *rand.Rand) time.Duration {
	if t.Distribution != nil && t.Distribution.Type != DistUniform {
		d := time.Duration(t.Distribution.SampleSeconds(r) * float64(time.Second))
		if d < t.Min.D() {
			d = t.Min.D()
		}
		if t.Max > 0 && d > t.Max.D() {
			d = t.Max.D()
		}
		return d
	}
	d := t.Min.D()
	if t.Max > t.Min {
		d += time.Duration(r.Int64N(int64(t.Max - t.Min)))
	}
	return d
}
