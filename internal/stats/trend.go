package stats

import (
	"math"
	"slices"
)

// MaxTrendPoints caps the series length for the O(n^2) trend methods. Longer
// series are reduced to this many bins (median of each bin, bin-centre time),
// which keeps a one-hour run at about 0.2 s of CPU and barely changes the
// slope of a slow drift.
const MaxTrendPoints = 600

// Bin reduces (t, y) to at most maxPoints bins by medians.
func Bin(t, y []float64, maxPoints int) ([]float64, []float64) {
	n := len(y)
	if n <= maxPoints || maxPoints <= 0 {
		return t, y
	}
	bt, by := make([]float64, 0, maxPoints), make([]float64, 0, maxPoints)
	for b := 0; b < maxPoints; b++ {
		s, e := b*n/maxPoints, (b+1)*n/maxPoints
		if e <= s {
			continue
		}
		bt = append(bt, Median(t[s:e]))
		by = append(by, Median(y[s:e]))
	}
	return bt, by
}

// TheilSen returns the Theil-Sen slope (median of pairwise slopes) and the
// intercept median(y - slope*t). Robust to up to ~29% outliers.
func TheilSen(t, y []float64) (slope, intercept float64) {
	t, y = Bin(t, y, MaxTrendPoints)
	n := len(y)
	if n < 2 {
		return 0, Median(y)
	}
	sl := make([]float64, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if dt := t[j] - t[i]; dt != 0 {
				sl = append(sl, (y[j]-y[i])/dt)
			}
		}
	}
	if len(sl) == 0 {
		return 0, Median(y)
	}
	slices.Sort(sl)
	slope = quantileSorted(sl, 0.5)
	res := make([]float64, n)
	for i := range y {
		res[i] = y[i] - slope*t[i]
	}
	return slope, Median(res)
}

// MKResult is a Mann-Kendall trend test result.
type MKResult struct {
	S   float64 `json:"s"`
	Var float64 `json:"var"`
	Z   float64 `json:"z"`
	P   float64 `json:"p"`   // two-sided
	Tau float64 `json:"tau"` // Kendall's tau-a
	N   int     `json:"n"`
}

// MannKendall tests for a monotonic trend in y (in time order), with the
// tie-corrected variance and continuity correction. It assumes independent
// observations; per-second load-test series are autocorrelated, so callers
// should use a stricter alpha and require a practical magnitude as well.
func MannKendall(y []float64) MKResult {
	if len(y) > MaxTrendPoints {
		t := make([]float64, len(y))
		for i := range t {
			t[i] = float64(i)
		}
		_, y = Bin(t, y, MaxTrendPoints)
	}
	n := len(y)
	r := MKResult{N: n, P: 1}
	if n < 3 {
		return r
	}
	var s float64
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			switch {
			case y[j] > y[i]:
				s++
			case y[j] < y[i]:
				s--
			}
		}
	}
	_, tie := Ranks(y)
	// sum t(t-1)(2t+5) over tie groups: recompute from group sizes
	var tieTerm float64
	if tie > 0 {
		ys := Sorted(y)
		for i := 0; i < n; {
			j := i
			for j+1 < n && ys[j+1] == ys[i] {
				j++
			}
			if g := float64(j - i + 1); g > 1 {
				tieTerm += g * (g - 1) * (2*g + 5)
			}
			i = j + 1
		}
	}
	fn := float64(n)
	r.S = s
	r.Var = (fn*(fn-1)*(2*fn+5) - tieTerm) / 18
	r.Tau = s / (fn * (fn - 1) / 2)
	if r.Var <= 0 {
		return r
	}
	switch {
	case s > 0:
		r.Z = (s - 1) / math.Sqrt(r.Var)
	case s < 0:
		r.Z = (s + 1) / math.Sqrt(r.Var)
	}
	r.P = math.Min(1, 2*(1-NormalCDF(math.Abs(r.Z))))
	return r
}
