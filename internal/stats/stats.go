// Package stats holds the deterministic statistics behind run comparison,
// change-point and drift detection, warm-up trimming and SLA suggestion.
//
// Everything here is pure: no I/O, no global state, and every randomised
// procedure takes an explicit seed so results are reproducible.
//
// Load Meter stores latency as histograms, not raw samples. The comparison
// code in internal/results therefore uses per-second window statistics (for
// example each second's p95) as the sample set. Those values are themselves
// estimates: a window percentile from the log-linear histogram has a relative
// bucket error below 0.8%, and a window with few requests has a noisy upper
// percentile (a p99 over fewer than 100 requests is close to that second's
// maximum). Tests on windows answer "did the typical per-second p95 move?",
// which is close to, but not the same as, "did the whole-run p95 move?".
package stats

import (
	"math"
	"slices"
)

// Sorted returns a sorted copy of x.
func Sorted(x []float64) []float64 {
	s := slices.Clone(x)
	slices.Sort(s)
	return s
}

// Mean of x (0 for empty input).
func Mean(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

// StdDev is the sample standard deviation (n-1 denominator).
func StdDev(x []float64) float64 {
	if len(x) < 2 {
		return 0
	}
	m := Mean(x)
	var s float64
	for _, v := range x {
		s += (v - m) * (v - m)
	}
	return math.Sqrt(s / float64(len(x)-1))
}

// Quantile returns the q-quantile (0..1) with linear interpolation between
// order statistics (Hyndman-Fan type 7, the R/NumPy default).
func Quantile(x []float64, q float64) float64 {
	if len(x) == 0 {
		return 0
	}
	return quantileSorted(Sorted(x), q)
}

func quantileSorted(s []float64, q float64) float64 {
	if len(s) == 0 {
		return 0
	}
	q = math.Max(0, math.Min(1, q))
	h := q * float64(len(s)-1)
	lo := int(math.Floor(h))
	hi := min(lo+1, len(s)-1)
	return s[lo] + (h-float64(lo))*(s[hi]-s[lo])
}

// Median of x.
func Median(x []float64) float64 { return Quantile(x, 0.5) }

// MAD is the raw median absolute deviation from the median.
func MAD(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	m := Median(x)
	d := make([]float64, len(x))
	for i, v := range x {
		d[i] = math.Abs(v - m)
	}
	return Median(d)
}

// MADScale converts a MAD into a standard-deviation estimate for normal data.
const MADScale = 1.4826

// RobustSigma estimates the noise level of a series from its first
// differences (MAD of x[i]-x[i-1], divided by sqrt 2). It is insensitive to
// level shifts and slow trends, which is what change-point penalties need.
func RobustSigma(x []float64) float64 {
	if len(x) < 3 {
		return 0
	}
	d := make([]float64, len(x)-1)
	for i := 1; i < len(x); i++ {
		d[i-1] = x[i] - x[i-1]
	}
	return MADScale * MAD(d) / math.Sqrt2
}

// CV is the robust coefficient of variation: scaled MAD / |median|.
func CV(x []float64) float64 {
	m := Median(x)
	if m == 0 {
		return 0
	}
	return MADScale * MAD(x) / math.Abs(m)
}

// NormalCDF is the standard normal cumulative distribution function.
func NormalCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }

// NormalQuantile is the inverse standard normal CDF (Acklam's rational
// approximation refined with one Newton step; absolute error < 1e-12).
func NormalQuantile(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const plow = 0.02425
	var x float64
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		x = (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= 1-plow:
		q := p - 0.5
		r := q * q
		x = (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * math.Log(1-p))
		x = -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	e := NormalCDF(x) - p
	u := e * math.Sqrt(2*math.Pi) * math.Exp(x*x/2)
	return x - u/(1+x*u/2)
}

// Ranks returns 1-based ranks with ties given their average rank, and the
// tie-correction term sum(t^3 - t) over tie groups.
func Ranks(x []float64) (ranks []float64, tieTerm float64) {
	n := len(x)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case x[a] < x[b]:
			return -1
		case x[a] > x[b]:
			return 1
		}
		return 0
	})
	ranks = make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && x[idx[j+1]] == x[idx[i]] {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			ranks[idx[k]] = r
		}
		if t := float64(j - i + 1); t > 1 {
			tieTerm += t*t*t - t
		}
		i = j + 1
	}
	return ranks, tieTerm
}

// Holm applies the Holm-Bonferroni step-down adjustment to p-values and
// returns adjusted p-values in the original order (monotone, capped at 1).
func Holm(p []float64) []float64 {
	m := len(p)
	idx := make([]int, m)
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case p[a] < p[b]:
			return -1
		case p[a] > p[b]:
			return 1
		}
		return 0
	})
	out := make([]float64, m)
	prev := 0.0
	for k, i := range idx {
		v := math.Min(1, float64(m-k)*p[i])
		v = math.Max(v, prev)
		out[i] = v
		prev = v
	}
	return out
}
