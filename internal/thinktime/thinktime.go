// Package thinktime fits parametric distributions to observed think-time gaps
// (seconds between a user's consecutive requests) by maximum likelihood and
// picks the best family by AIC. It is deterministic statistics, no AI.
//
// Families: exponential (mean), log-normal (mu, sigma of ln x) and gamma
// (shape, scale). All parameters are in seconds, matching model.Distribution.
package thinktime

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// MinSamples is the smallest sample Fit accepts.
const MinSamples = 10

// Candidate is one fitted family.
type Candidate struct {
	Dist   string             `json:"dist"`
	Params map[string]float64 `json:"params"`
	LogLik float64            `json:"logLikelihood"`
	AIC    float64            `json:"aic"`
}

// Result is the best fit plus every candidate that converged.
type Result struct {
	Dist       string             `json:"dist"`
	Params     map[string]float64 `json:"params"`
	AIC        float64            `json:"aic"`
	N          int                `json:"n"`
	MedianSec  float64            `json:"medianSeconds"`
	P995Sec    float64            `json:"p995Seconds"`
	Candidates []Candidate        `json:"candidates"`
}

// ErrTooFewSamples is returned when fewer than MinSamples positive gaps exist.
var ErrTooFewSamples = errors.New("too few think-time samples to fit a distribution")

// Fit fits exponential, log-normal and gamma to the positive, finite gaps (in
// seconds) and returns the family with the lowest AIC. Non-positive values are
// ignored (callers should drop machine-speed gaps first).
func Fit(gaps []float64) (*Result, error) {
	xs := make([]float64, 0, len(gaps))
	for _, g := range gaps {
		if g > 0 && !math.IsInf(g, 0) && !math.IsNaN(g) {
			xs = append(xs, g)
		}
	}
	n := len(xs)
	if n < MinSamples {
		return nil, fmt.Errorf("%w: have %d, need at least %d", ErrTooFewSamples, n, MinSamples)
	}
	var sum, sumLn, sumLn2 float64
	for _, x := range xs {
		l := math.Log(x)
		sum += x
		sumLn += l
		sumLn2 += l * l
	}
	fn := float64(n)
	mean := sum / fn
	var cands []Candidate

	// Exponential: lambda = 1/mean.
	cands = append(cands, mk("exponential", map[string]float64{"mean": mean}, fn*(-math.Log(mean))-sum/mean, 1))

	// Log-normal: mu = mean(ln x), sigma^2 = var(ln x) (MLE divides by n).
	mu := sumLn / fn
	v := sumLn2/fn - mu*mu
	if v > 1e-12 {
		sg := math.Sqrt(v)
		ll := -sumLn - fn/2*math.Log(2*math.Pi*v) - fn/2
		cands = append(cands, mk("lognormal", map[string]float64{"mu": mu, "sigma": sg}, ll, 2))
	}

	// Gamma: solve ln k - psi(k) = ln(mean) - mean(ln x) by Newton's method.
	s := math.Log(mean) - sumLn/fn
	if s > 1e-12 {
		k := (3 - s + math.Sqrt((s-3)*(s-3)+24*s)) / (12 * s)
		for i := 0; i < 100; i++ {
			f := math.Log(k) - digamma(k) - s
			fp := 1/k - trigamma(k)
			nk := k - f/fp
			if nk <= 0 {
				nk = k / 2
			}
			if math.Abs(nk-k) < 1e-12*k {
				k = nk
				break
			}
			k = nk
		}
		if k > 0 && !math.IsNaN(k) && !math.IsInf(k, 0) {
			theta := mean / k
			lg, _ := math.Lgamma(k)
			ll := (k-1)*sumLn - sum/theta - fn*k*math.Log(theta) - fn*lg
			cands = append(cands, mk("gamma", map[string]float64{"shape": k, "scale": theta}, ll, 2))
		}
	}

	best := 0
	for i, c := range cands {
		if c.AIC < cands[best].AIC {
			best = i
		}
	}
	sort.Float64s(xs)
	return &Result{Dist: cands[best].Dist, Params: cands[best].Params, AIC: cands[best].AIC, N: n,
		MedianSec: quantile(xs, 0.5), P995Sec: quantile(xs, 0.995), Candidates: cands}, nil
}

func mk(name string, p map[string]float64, ll float64, k int) Candidate {
	return Candidate{Dist: name, Params: p, LogLik: ll, AIC: 2*float64(k) - 2*ll}
}

// quantile of a sorted slice (linear interpolation).
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	pos := q * float64(len(xs)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return xs[lo] + (xs[hi]-xs[lo])*(pos-float64(lo))
}

// digamma via recurrence to x >= 6 then the asymptotic expansion.
func digamma(x float64) float64 {
	r := 0.0
	for x < 6 {
		r -= 1 / x
		x++
	}
	f := 1 / (x * x)
	return r + math.Log(x) - 0.5/x - f*(1.0/12-f*(1.0/120-f*(1.0/252-f*(1.0/240-f*(1.0/132)))))
}

// trigamma via recurrence and the asymptotic expansion.
func trigamma(x float64) float64 {
	r := 0.0
	for x < 6 {
		r += 1 / (x * x)
		x++
	}
	f := 1 / (x * x)
	return r + 1/x + f/2 + f/x*(1.0/6-f*(1.0/30-f*(1.0/42-f*(1.0/30))))
}
