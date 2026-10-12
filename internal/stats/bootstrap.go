package stats

import (
	"math"
	"math/rand/v2"
	"slices"
)

// NewRand returns a deterministic PRNG for a seed (PCG from math/rand/v2).
func NewRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// CI is a bootstrap confidence interval.
type CI struct {
	Estimate  float64 `json:"estimate"`
	Lo        float64 `json:"lo"`
	Hi        float64 `json:"hi"`
	Level     float64 `json:"level"`
	Resamples int     `json:"resamples"`
	Block     int     `json:"block"` // moving-block length (1 = iid bootstrap)
	Seed      uint64  `json:"seed"`
}

// BootstrapOptions control BootstrapCI. Zero values pick defaults.
type BootstrapOptions struct {
	Resamples int     // default 2000
	Level     float64 // default 0.95
	Seed      uint64  // default 1
	Block     int     // moving-block length; 0 = auto (ceil(n^(1/3))), 1 = iid
}

func (o BootstrapOptions) norm() BootstrapOptions {
	if o.Resamples <= 0 {
		o.Resamples = 2000
	}
	if o.Level <= 0 || o.Level >= 1 {
		o.Level = 0.95
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	return o
}

// AutoBlock is the moving-block length used when Block is 0: ceil(n^(1/3)),
// the usual rate for variance/distribution estimation under weak dependence.
func AutoBlock(n int) int {
	if n < 2 {
		return 1
	}
	return max(1, int(math.Ceil(math.Cbrt(float64(n)))))
}

// blockResample fills dst (len n) with a moving-block bootstrap resample of x.
func blockResample(rng *rand.Rand, x, dst []float64, block int) {
	n := len(x)
	if block <= 1 || block >= n {
		for i := range dst {
			dst[i] = x[rng.IntN(n)]
		}
		return
	}
	for i := 0; i < len(dst); {
		s := rng.IntN(n - block + 1)
		for k := 0; k < block && i < len(dst); k++ {
			dst[i] = x[s+k]
			i++
		}
	}
}

// BootstrapCI estimates a percentile confidence interval of stat(base, cur)
// by resampling each sample independently (moving blocks keep short-range
// autocorrelation of per-second series). Deterministic for a given seed.
func BootstrapCI(base, cur []float64, stat func(b, c []float64) float64, o BootstrapOptions) CI {
	o = o.norm()
	ci := CI{Level: o.Level, Resamples: o.Resamples, Seed: o.Seed}
	if len(base) == 0 || len(cur) == 0 {
		return ci
	}
	ci.Estimate = stat(base, cur)
	bb, cb := o.Block, o.Block
	if o.Block == 0 {
		bb, cb = AutoBlock(len(base)), AutoBlock(len(cur))
	}
	ci.Block = max(bb, cb)
	rng := NewRand(o.Seed)
	rb, rc := make([]float64, len(base)), make([]float64, len(cur))
	vals := make([]float64, 0, o.Resamples)
	for i := 0; i < o.Resamples; i++ {
		blockResample(rng, base, rb, bb)
		blockResample(rng, cur, rc, cb)
		if v := stat(rb, rc); !math.IsNaN(v) && !math.IsInf(v, 0) {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return ci
	}
	slices.Sort(vals)
	a := (1 - o.Level) / 2
	ci.Lo, ci.Hi = quantileSorted(vals, a), quantileSorted(vals, 1-a)
	return ci
}

// BootstrapOne is BootstrapCI for a one-sample statistic.
func BootstrapOne(x []float64, stat func([]float64) float64, o BootstrapOptions) CI {
	return BootstrapCI(x, x, func(b, _ []float64) float64 { return stat(b) }, o)
}

// MedianShift is median(cur) - median(base).
func MedianShift(base, cur []float64) float64 { return Median(cur) - Median(base) }

// MedianRatio is median(cur)/median(base) - 1 (NaN when the base median is 0).
func MedianRatio(base, cur []float64) float64 {
	m := Median(base)
	if m == 0 {
		return math.NaN()
	}
	return Median(cur)/m - 1
}
