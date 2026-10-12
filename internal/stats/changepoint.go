package stats

import "math"

// Segment is a run of consecutive points [Start, End) with a common level.
type Segment struct {
	Start  int     `json:"start"`
	End    int     `json:"end"` // exclusive
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
}

// meanCost is the Gaussian mean-change cost: within-segment sum of squared
// deviations, from prefix sums, O(1) per segment.
type meanCost struct{ s1, s2 []float64 }

func newMeanCost(x []float64) meanCost {
	c := meanCost{make([]float64, len(x)+1), make([]float64, len(x)+1)}
	for i, v := range x {
		c.s1[i+1] = c.s1[i] + v
		c.s2[i+1] = c.s2[i] + v*v
	}
	return c
}

func (c meanCost) cost(s, e int) float64 {
	n := float64(e - s)
	if n <= 0 {
		return 0
	}
	a := c.s1[e] - c.s1[s]
	return math.Max(0, (c.s2[e]-c.s2[s])-a*a/n)
}

// ChangeOptions configure the offline change-point detectors.
type ChangeOptions struct {
	// Sigma is the noise standard deviation; 0 = RobustSigma(x).
	Sigma float64
	// MinSigma floors the noise estimate so a perfectly flat series does not
	// make every tiny wiggle "significant".
	MinSigma float64
	// PenaltyK scales the BIC-like penalty beta = PenaltyK * sigma^2 * ln(n).
	// The classic BIC for one extra mean parameter is 2; default 3 is a bit
	// more conservative because per-second windows are autocorrelated.
	PenaltyK float64
	// MinSize is the minimum segment length (default 3).
	MinSize int
}

func (o ChangeOptions) prep(x []float64) (sigma, beta float64, minSize int) {
	sigma = o.Sigma
	if sigma <= 0 {
		sigma = RobustSigma(x)
	}
	sigma = math.Max(sigma, o.MinSigma)
	if sigma <= 0 {
		sigma = 1e-9
	}
	k := o.PenaltyK
	if k <= 0 {
		k = 3
	}
	minSize = o.MinSize
	if minSize <= 0 {
		minSize = 3
	}
	beta = k * sigma * sigma * math.Log(math.Max(float64(len(x)), 2))
	return
}

// PELT finds the optimal segmentation of x into mean-shift segments under a
// linear penalty (Killick, Fearnhead & Eckley 2012). It returns the change
// points: indices where a new segment starts (never 0), ascending.
func PELT(x []float64, o ChangeOptions) []int {
	n := len(x)
	_, beta, minSize := o.prep(x)
	if n < 2*minSize {
		return nil
	}
	c := newMeanCost(x)
	F := make([]float64, n+1)
	last := make([]int, n+1)
	for i := range F {
		F[i] = math.Inf(1)
	}
	F[0] = -beta
	R := []int{0}
	for t := 1; t <= n; t++ {
		if t >= minSize {
			best, arg := math.Inf(1), 0
			for _, s := range R {
				if t-s >= minSize {
					if v := F[s] + c.cost(s, t) + beta; v < best {
						best, arg = v, s
					}
				}
			}
			F[t], last[t] = best, arg
		}
		// prune candidates that can never be optimal again
		keep := R[:0]
		for _, s := range R {
			if t-s < minSize || F[s]+c.cost(s, t) <= F[t] {
				keep = append(keep, s)
			}
		}
		R = keep
		if !math.IsInf(F[t], 1) {
			R = append(R, t)
		}
	}
	var cps []int
	for t := last[n]; t > 0; t = last[t] {
		cps = append([]int{t}, cps...)
	}
	return cps
}

// BinarySegmentation splits recursively at the point of largest cost
// reduction while that reduction exceeds the same penalty as PELT. It is a
// fast greedy approximation; PELT is exact for the penalised cost.
func BinarySegmentation(x []float64, o ChangeOptions) []int {
	_, beta, minSize := o.prep(x)
	c := newMeanCost(x)
	var cps []int
	var rec func(s, e int)
	rec = func(s, e int) {
		if e-s < 2*minSize {
			return
		}
		whole := c.cost(s, e)
		best, arg := 0.0, -1
		for k := s + minSize; k <= e-minSize; k++ {
			if g := whole - c.cost(s, k) - c.cost(k, e); g > best {
				best, arg = g, k
			}
		}
		if arg < 0 || best <= beta {
			return
		}
		rec(s, arg)
		cps = append(cps, arg)
		rec(arg, e)
	}
	rec(0, len(x))
	return cps
}

// Segments turns change points into segments with their level statistics.
func Segments(x []float64, cps []int) []Segment {
	bounds := append(append([]int{0}, cps...), len(x))
	var out []Segment
	for i := 0; i+1 < len(bounds); i++ {
		s, e := bounds[i], bounds[i+1]
		if e <= s {
			continue
		}
		out = append(out, Segment{Start: s, End: e, Mean: Mean(x[s:e]), Median: Median(x[s:e])})
	}
	return out
}

// CUSUMResult is the outcome of a tabular two-sided CUSUM.
type CUSUMResult struct {
	Alarm     int     `json:"alarm"`     // first index where a statistic exceeded h (-1 = none)
	Start     int     `json:"start"`     // estimated start of the shift (last reset before the alarm)
	Direction int     `json:"direction"` // +1 upward shift, -1 downward
	Max       float64 `json:"max"`       // largest statistic reached, in sigma units
}

// CUSUM runs Page's tabular CUSUM on x against a reference level target with
// noise sigma, slack k and decision interval h (both in sigma units; k=0.5,
// h=5 detect a 1-sigma shift with an in-control ARL of roughly 465).
func CUSUM(x []float64, target, sigma, k, h float64) CUSUMResult {
	r := CUSUMResult{Alarm: -1, Start: -1}
	if sigma <= 0 {
		return r
	}
	var hi, lo float64
	hiStart, loStart := 0, 0
	for i, v := range x {
		z := (v - target) / sigma
		if hi == 0 {
			hiStart = i
		}
		if lo == 0 {
			loStart = i
		}
		hi = math.Max(0, hi+z-k)
		lo = math.Max(0, lo-z-k)
		r.Max = math.Max(r.Max, math.Max(hi, lo))
		if hi > h {
			r.Alarm, r.Start, r.Direction = i, hiStart, 1
			return r
		}
		if lo > h {
			r.Alarm, r.Start, r.Direction = i, loStart, -1
			return r
		}
	}
	return r
}

// CUSUMChangePoint locates the single most likely mean shift offline: the
// argmax of |sum_{i<k}(x_i - mean)|. It returns the index where the new level
// starts and the normalised statistic max|S_k| / (sigma sqrt n).
func CUSUMChangePoint(x []float64) (int, float64) {
	n := len(x)
	if n < 4 {
		return -1, 0
	}
	m := Mean(x)
	var s, best float64
	arg := -1
	for k := 1; k < n; k++ {
		s += x[k-1] - m
		if a := math.Abs(s); a > best {
			best, arg = a, k
		}
	}
	sigma := RobustSigma(x)
	if sigma == 0 {
		sigma = StdDev(x)
	}
	if sigma == 0 {
		return -1, 0
	}
	return arg, best / (sigma * math.Sqrt(float64(n)))
}
