package stats

import (
	"fmt"
	"math"
	"sort"
)

// Alternative selects the alternative hypothesis of a test.
type Alternative int

const (
	TwoSided Alternative = iota
	Greater              // x tends to be larger than y
	Less                 // x tends to be smaller than y
)

// MWResult is the outcome of a Mann-Whitney U test.
type MWResult struct {
	U      float64 `json:"u"`      // U statistic of x: #(x>y) + 0.5 #(x=y)
	Z      float64 `json:"z"`      // normal approximation z (0 when exact)
	P      float64 `json:"p"`      // p-value for the requested alternative
	N1     int     `json:"n1"`     // len(x)
	N2     int     `json:"n2"`     // len(y)
	Exact  bool    `json:"exact"`  // exact null distribution was used
	Method string  `json:"method"` // human-readable method
}

// ExactLimit: with no ties and n1+n2 <= ExactLimit the exact distribution of
// U is enumerated; otherwise the tie-corrected normal approximation with
// continuity correction is used.
const ExactLimit = 30

// MannWhitney runs the Mann-Whitney U (Wilcoxon rank-sum) test of x vs y.
func MannWhitney(x, y []float64, alt Alternative) MWResult {
	n1, n2 := len(x), len(y)
	r := MWResult{N1: n1, N2: n2, P: 1}
	if n1 == 0 || n2 == 0 {
		r.Method = "not computed (empty sample)"
		return r
	}
	all := make([]float64, 0, n1+n2)
	all = append(append(all, x...), y...)
	ranks, tie := Ranks(all)
	var r1 float64
	for i := 0; i < n1; i++ {
		r1 += ranks[i]
	}
	f1, f2 := float64(n1), float64(n2)
	u := r1 - f1*(f1+1)/2
	r.U = u
	mu := f1 * f2 / 2
	n := f1 + f2
	if tie == 0 && n1+n2 <= ExactLimit {
		r.Exact = true
		r.Method = "Mann-Whitney U, exact null distribution"
		cdf := exactUDist(n1, n2)
		// U takes integer values without ties.
		k := int(math.Round(u))
		le := cdf[k]
		ge := 1.0
		if k > 0 {
			ge = 1 - cdf[k-1]
		}
		switch alt {
		case Greater:
			r.P = ge
		case Less:
			r.P = le
		default:
			r.P = math.Min(1, 2*math.Min(le, ge))
		}
		return r
	}
	r.Method = "Mann-Whitney U, normal approximation with tie and continuity correction"
	v := f1 * f2 / 12 * ((n + 1) - tie/(n*(n-1)))
	if v <= 0 { // every value identical
		r.P = 1
		return r
	}
	sd := math.Sqrt(v)
	d := u - mu
	switch alt {
	case Greater:
		r.Z = (d - 0.5) / sd
		r.P = 1 - NormalCDF(r.Z)
	case Less:
		r.Z = (d + 0.5) / sd
		r.P = NormalCDF(r.Z)
	default:
		c := math.Max(math.Abs(d)-0.5, 0)
		r.Z = math.Copysign(c/sd, d)
		r.P = math.Min(1, 2*(1-NormalCDF(c/sd)))
	}
	return r
}

// exactUDist returns the CDF of U for sample sizes n1, n2 (no ties), via the
// standard recurrence f(n1,n2,u) = f(n1-1,n2,u-n2) + f(n1,n2-1,u).
func exactUDist(n1, n2 int) []float64 {
	maxU := n1 * n2
	// dp[i][j] = counts for sizes (i, j); rolling over i.
	prev := make([][]float64, n2+1)
	for j := 0; j <= n2; j++ {
		prev[j] = []float64{1} // i = 0: only U = 0
	}
	for i := 1; i <= n1; i++ {
		cur := make([][]float64, n2+1)
		cur[0] = []float64{1}
		for j := 1; j <= n2; j++ {
			c := make([]float64, i*j+1)
			// last element from x is the largest: contributes j to U
			for u, v := range prev[j] {
				c[u+j] += v
			}
			// last element from y is the largest: contributes 0
			for u, v := range cur[j-1] {
				c[u] += v
			}
			cur[j] = c
		}
		prev = cur
	}
	cnt := prev[n2]
	var total float64
	for _, v := range cnt {
		total += v
	}
	cdf := make([]float64, maxU+1)
	var acc float64
	for u := 0; u <= maxU; u++ {
		acc += cnt[u]
		cdf[u] = acc / total
	}
	return cdf
}

// CliffsDelta = P(x > y) - P(x < y), in [-1, 1]. Positive means x tends to be larger.
func CliffsDelta(x, y []float64) float64 {
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	ys := Sorted(y)
	var gt, lt int
	for _, v := range x {
		lt += len(ys) - sort.SearchFloat64s(ys, math.Nextafter(v, math.Inf(1))) // y > v
		gt += sort.SearchFloat64s(ys, v)                                        // y < v
	}
	return float64(gt-lt) / float64(len(x)*len(y))
}

// CliffsMagnitude labels |delta| with the Romano et al. (2006) thresholds.
func CliffsMagnitude(d float64) string {
	a := math.Abs(d)
	switch {
	case a < 0.147:
		return "negligible"
	case a < 0.33:
		return "small"
	case a < 0.474:
		return "medium"
	}
	return "large"
}

// PropResult is a two-proportion comparison.
type PropResult struct {
	P1, P2     float64 // proportions (0..1)
	Diff       float64 // P2 - P1
	Lo, Hi     float64 // CI of Diff
	Z, P       float64 // two-sided test
	CohensH    float64 // effect size 2asin(sqrt p2) - 2asin(sqrt p1)
	Method     string
	Comparable bool // false when a sample is empty
}

// TwoProportions compares x1/n1 (baseline) with x2/n2 (current): pooled z test
// with Yates continuity correction, Wald interval with continuity correction.
func TwoProportions(x1, n1, x2, n2 float64, level float64) PropResult {
	r := PropResult{Method: "two-proportion z-test (pooled, continuity-corrected)", P: 1}
	if n1 <= 0 || n2 <= 0 {
		return r
	}
	r.Comparable = true
	r.P1, r.P2 = x1/n1, x2/n2
	r.Diff = r.P2 - r.P1
	r.CohensH = 2*math.Asin(math.Sqrt(r.P2)) - 2*math.Asin(math.Sqrt(r.P1))
	cc := 0.5 * (1/n1 + 1/n2)
	p := (x1 + x2) / (n1 + n2)
	se0 := math.Sqrt(p * (1 - p) * (1/n1 + 1/n2))
	if se0 > 0 {
		c := math.Max(math.Abs(r.Diff)-cc, 0)
		r.Z = math.Copysign(c/se0, r.Diff)
		r.P = math.Min(1, 2*(1-NormalCDF(c/se0)))
	}
	z := NormalQuantile(1 - (1-level)/2)
	se := math.Sqrt(r.P1*(1-r.P1)/n1 + r.P2*(1-r.P2)/n2)
	r.Lo, r.Hi = r.Diff-z*se-cc, r.Diff+z*se+cc
	return r
}

// Lag1 is the lag-1 autocorrelation of x (0 for short or constant series).
func Lag1(x []float64) float64 {
	n := len(x)
	if n < 3 {
		return 0
	}
	m := Mean(x)
	var num, den float64
	for i := 0; i < n; i++ {
		d := x[i] - m
		den += d * d
		if i > 0 {
			num += d * (x[i-1] - m)
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// MannWhitneyDependent is MannWhitney for serially correlated samples such as
// per-second series. The normal-approximation variance is inflated by the
// AR(1) factor (1+r)/(1-r), where r is the mean lag-1 autocorrelation of the
// two samples, corrected for its small-sample bias (E[r] ~ phi - (1+3 phi)/n,
// Marriott & Pope 1954) and clipped to [0, 0.9]; equivalently the effective
// sample size is n(1-r)/(1+r). With r = 0 it equals MannWhitney.
func MannWhitneyDependent(x, y []float64, alt Alternative) (MWResult, float64) {
	debias := func(v []float64) float64 {
		r := Lag1(v)
		if n := float64(len(v)); n > 3 && r > 0 {
			r += (1 + 3*r) / n
		}
		return r
	}
	r := math.Max(0, math.Min(0.9, (debias(x)+debias(y))/2))
	res := MannWhitney(x, y, alt)
	if r == 0 || res.Exact || res.Z == 0 {
		return res, r
	}
	inf := math.Sqrt((1 + r) / (1 - r))
	res.Z /= inf
	switch alt {
	case Greater:
		res.P = 1 - NormalCDF(res.Z)
	case Less:
		res.P = NormalCDF(res.Z)
	default:
		res.P = math.Min(1, 2*(1-NormalCDF(math.Abs(res.Z))))
	}
	res.Method += fmt.Sprintf(", variance inflated for lag-1 autocorrelation %.2f", r)
	return res, r
}
