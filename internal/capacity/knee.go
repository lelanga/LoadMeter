package capacity

import "math"

// Knee is the point where latency starts rising much faster than throughput.
type Knee struct {
	OfferedRate float64 `json:"offeredRate"` // iterations/s at the knee step
	Throughput  float64 `json:"throughput"`  // achieved requests/s at the knee step
	P95Ms       float64 `json:"p95Ms"`
	Score       float64 `json:"score"` // Kneedle difference value (0..1)
	Method      string  `json:"method"`
}

// Plateau is where achieved throughput stops following the offered rate.
type Plateau struct {
	StartIndex      int     `json:"startIndex"`      // index (in the ramp series) of the first non-scaling step
	LastScalingRate float64 `json:"lastScalingRate"` // offered it/s of the last step that still scaled
	MaxThroughput   float64 `json:"maxThroughput"`   // highest achieved it/s seen in the ramp
	Gain            float64 `json:"gain"`            // marginal gain at StartIndex
	Method          string  `json:"method"`
}

// MinKneeScore is the smallest Kneedle difference treated as a real knee.
// A straight line scores 0; a sharp hockey stick approaches 1.
const MinKneeScore = 0.1

// MinKneeRise: latency at the top of the curve must be at least this many
// times the lowest latency, or the curve is called flat.
const MinKneeRise = 1.2

// DefaultPlateauGain: a step "scales" while it turns at least this fraction
// of the extra offered load into extra achieved throughput.
const DefaultPlateauGain = 0.5

// Kneedle finds the knee of an increasing, convex curve y(x) such as p95
// latency against throughput.
//
// Method (Satopaa et al., "Finding a 'Kneedle' in a Haystack", 2011), as
// implemented here:
//  1. Normalise x and y to [0,1] with min-max scaling.
//  2. For a convex increasing curve (flat, then rising steeply) the difference
//     curve is D_i = xn_i - yn_i. On a straight line D is 0 everywhere; on a
//     hockey stick D is largest at the last point before the steep rise.
//  3. The knee is argmax D_i, accepted only if D_max >= MinKneeScore, the
//     knee is not the last point (no rise observed after it yet), and the
//     latency range is meaningful (y_max >= MinKneeRise * y_min).
//
// Points are taken in the order given (callers order them by offered rate).
// x does not need to be monotonic: when throughput falls under overload, xn
// drops while yn rises, so those points get a low D and are never the knee.
// The paper's online sensitivity threshold is not used: with the few points
// of a staircase a single global maximum is more stable.
func Kneedle(xs, ys []float64) (idx int, score float64, ok bool) {
	n := len(xs)
	if n < 3 || len(ys) != n {
		return 0, 0, false
	}
	xmin, xmax, ymin, ymax := xs[0], xs[0], ys[0], ys[0]
	for i := 1; i < n; i++ {
		xmin, xmax = math.Min(xmin, xs[i]), math.Max(xmax, xs[i])
		ymin, ymax = math.Min(ymin, ys[i]), math.Max(ymax, ys[i])
	}
	if xmax == xmin || ymax == ymin {
		return 0, 0, false
	}
	if ymin > 0 && ymax < MinKneeRise*ymin {
		return 0, 0, false
	}
	best, bi := math.Inf(-1), -1
	for i := 0; i < n; i++ {
		d := (xs[i]-xmin)/(xmax-xmin) - (ys[i]-ymin)/(ymax-ymin)
		if d > best {
			best, bi = d, i
		}
	}
	if bi < 0 || bi == n-1 || best < MinKneeScore {
		return 0, 0, false
	}
	return bi, best, true
}

// DetectPlateau finds where achieved throughput stops scaling with offered
// load.
//
// Method: for consecutive steps the marginal gain is
// g_i = (A_i - A_{i-1}) / (O_i - O_{i-1}), the share of the extra offered
// rate that became extra achieved rate (1 = perfect scaling). The plateau
// starts at the first i where g_i < minGain AND g_{i+1} < minGain (two
// consecutive steps, so one noisy step is not a plateau). The last step that
// still scaled is i-1. Steps whose offered rate did not increase are skipped.
func DetectPlateau(offered, achieved []float64, minGain float64) (Plateau, bool) {
	n := len(offered)
	if n < 3 || len(achieved) != n {
		return Plateau{}, false
	}
	gain := func(i int) (float64, bool) {
		do := offered[i] - offered[i-1]
		if do <= 0 {
			return 0, false
		}
		return (achieved[i] - achieved[i-1]) / do, true
	}
	maxA := 0.0
	for _, a := range achieved {
		maxA = math.Max(maxA, a)
	}
	for i := 1; i < n-1; i++ {
		g1, ok1 := gain(i)
		g2, ok2 := gain(i + 1)
		if ok1 && ok2 && g1 < minGain && g2 < minGain {
			return Plateau{StartIndex: i, LastScalingRate: offered[i-1], MaxThroughput: r2(maxA), Gain: r2(g1),
				Method: "marginal gain of achieved vs offered rate below 0.5 for two consecutive ramp steps"}, true
		}
	}
	return Plateau{}, false
}
