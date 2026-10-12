package stats

import "math"

// MSER returns the warm-up truncation point of a series using MSER-m
// (White 1997, "marginal standard error rule"): the series is averaged in
// batches of m, and the truncation d (in batches) minimising
// sum_{i>=d}(b_i - mean_d)^2 / (k-d)^2 is chosen, searching only the first
// half so that a trend in the steady state cannot eat the whole run.
// The result is in original points (a multiple of m), 0 = keep everything.
func MSER(x []float64, m int) int {
	if m <= 0 {
		m = 5
	}
	k := len(x) / m
	if k < 4 {
		return 0
	}
	b := make([]float64, k)
	for i := 0; i < k; i++ {
		b[i] = Mean(x[i*m : (i+1)*m])
	}
	best, arg := math.Inf(1), 0
	for d := 0; d <= k/2; d++ {
		tail := b[d:]
		mu := Mean(tail)
		var ss float64
		for _, v := range tail {
			ss += (v - mu) * (v - mu)
		}
		nt := float64(len(tail))
		if v := ss / (nt * nt); d == 0 || v < best*(1-1e-12) {
			best, arg = v, d
		}
	}
	return arg * m
}

// RampEnd returns the first index from which the load level (e.g. active VUs
// or offered rate) has reached frac (e.g. 0.9) of its maximum, or 0 when that
// happens only in the second half (a run that ramps throughout has no
// separate warm-up to discard).
func RampEnd(load []float64, frac float64) int {
	var mx float64
	for _, v := range load {
		mx = math.Max(mx, v)
	}
	if mx <= 0 {
		return 0
	}
	for i, v := range load {
		if v >= frac*mx {
			if i > len(load)/2 {
				return 0
			}
			return i
		}
	}
	return 0
}

// WarmupEnd combines the load ramp and MSER-5 on the metric itself and
// returns the number of leading points to discard (at most half the series).
func WarmupEnd(metric, load []float64) int {
	d := MSER(metric, 5)
	if load != nil {
		d = max(d, RampEnd(load, 0.9))
	}
	return min(d, len(metric)/2)
}
