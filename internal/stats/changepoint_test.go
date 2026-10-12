package stats

import (
	"math"
	"testing"
)

func step(seed uint64, n, at int, a, b, sd float64) []float64 {
	x := normals(seed, n, 0, sd)
	for i := range x {
		if i < at {
			x[i] += a
		} else {
			x[i] += b
		}
	}
	return x
}

func TestPELTKnownStep(t *testing.T) {
	for s := uint64(1); s <= 30; s++ {
		x := step(s, 120, 70, 20, 30, 2) // 5-sigma step at index 70
		cps := PELT(x, ChangeOptions{})
		if len(cps) != 1 || math.Abs(float64(cps[0]-70)) > 2 {
			t.Fatalf("seed %d: PELT %v want [70±2]", s, cps)
		}
		bs := BinarySegmentation(x, ChangeOptions{})
		if len(bs) != 1 || math.Abs(float64(bs[0]-70)) > 2 {
			t.Fatalf("seed %d: binseg %v", s, bs)
		}
		seg := Segments(x, cps)
		if len(seg) != 2 || !near(seg[0].Median, 20, 1.5) || !near(seg[1].Median, 30, 1.5) {
			t.Fatalf("segments %+v", seg)
		}
	}
}

func TestPELTTwoSteps(t *testing.T) {
	x := step(5, 150, 50, 10, 25, 1.5)
	for i := 110; i < 150; i++ {
		x[i] -= 10 // back down to ~15
	}
	cps := PELT(x, ChangeOptions{})
	if len(cps) != 2 || math.Abs(float64(cps[0]-50)) > 2 || math.Abs(float64(cps[1]-110)) > 2 {
		t.Errorf("cps %v", cps)
	}
}

func TestPELTNoChangeOnNoise(t *testing.T) {
	falseAlarms := 0
	for s := uint64(1); s <= 100; s++ {
		x := normals(s, 200, 50, 3)
		if len(PELT(x, ChangeOptions{})) > 0 {
			falseAlarms++
		}
	}
	if falseAlarms > 5 {
		t.Errorf("PELT reported changes in %d/100 pure-noise series", falseAlarms)
	}
	// flat series with a sigma floor: no change
	if cps := PELT(make([]float64, 50), ChangeOptions{MinSigma: 1}); len(cps) != 0 {
		t.Errorf("flat %v", cps)
	}
	if PELT([]float64{1, 2}, ChangeOptions{}) != nil {
		t.Error("too short")
	}
}

func TestPELTMatchesExhaustiveSearch(t *testing.T) {
	// For small n the penalised optimum can be found by brute force over all
	// segmentations; PELT must reach the same cost.
	x := step(9, 14, 6, 0, 4, 1)
	o := ChangeOptions{Sigma: 1, MinSize: 2}
	_, beta, minSize := o.prep(x)
	c := newMeanCost(x)
	total := func(cps []int) float64 {
		b := append(append([]int{0}, cps...), len(x))
		v := 0.0
		for i := 0; i+1 < len(b); i++ {
			v += c.cost(b[i], b[i+1]) + beta
		}
		return v
	}
	best := math.Inf(1)
	n := len(x)
	for mask := 0; mask < 1<<(n-1); mask++ {
		var cps []int
		ok := true
		prev := 0
		for i := 1; i < n; i++ {
			if mask&(1<<(i-1)) != 0 {
				if i-prev < minSize {
					ok = false
					break
				}
				cps = append(cps, i)
				prev = i
			}
		}
		if !ok || n-prev < minSize {
			continue
		}
		best = math.Min(best, total(cps))
	}
	if got := total(PELT(x, o)); !near(got, best, 1e-9) {
		t.Errorf("PELT cost %v, exhaustive %v", got, best)
	}
}

func TestCUSUM(t *testing.T) {
	x := step(2, 100, 60, 10, 12, 1) // +2 sigma at 60
	r := CUSUM(x, 10, 1, 0.5, 5)
	if r.Alarm < 60 || r.Alarm > 70 || r.Direction != 1 || math.Abs(float64(r.Start-60)) > 4 {
		t.Errorf("%+v", r)
	}
	if r := CUSUM(normals(4, 100, 10, 1), 10, 1, 0.5, 5); r.Alarm != -1 {
		t.Errorf("in-control alarm %+v", r)
	}
	y := step(3, 100, 40, 10, 7, 1)
	if r := CUSUM(y, 10, 1, 0.5, 5); r.Direction != -1 || r.Alarm < 40 {
		t.Errorf("down %+v", r)
	}
	k, stat := CUSUMChangePoint(step(6, 100, 30, 0, 3, 1))
	if math.Abs(float64(k-30)) > 2 || stat < 1.36 { // 1.36 ~ 5% KS-type critical value
		t.Errorf("offline cusum %d %v", k, stat)
	}
}

func TestTheilSenAndMannKendall(t *testing.T) {
	// slope 0.5 ms/s plus noise and 10% gross outliers
	n := 300
	ts, y := make([]float64, n), normals(1, n, 0, 2)
	for i := range y {
		ts[i] = float64(i)
		y[i] += 20 + 0.5*float64(i)
		if i%10 == 0 {
			y[i] += 500
		}
	}
	sl, ic := TheilSen(ts, y)
	if !near(sl, 0.5, 0.03) || !near(ic, 20, 2) {
		t.Errorf("slope %v intercept %v", sl, ic)
	}
	mk := MannKendall(y)
	if mk.P > 1e-6 || mk.Tau <= 0.5 || mk.S <= 0 {
		t.Errorf("%+v", mk)
	}
	// negative drift
	for i := range y {
		y[i] = 100 - 0.2*float64(i) + NewRand(uint64(i)).NormFloat64()
	}
	if sl, _ := TheilSen(ts, y); !near(sl, -0.2, 0.02) {
		t.Errorf("neg slope %v", sl)
	}
	// no trend: false-positive rate near alpha
	fp := 0
	for s := uint64(1); s <= 200; s++ {
		if MannKendall(normals(s, 100, 0, 1)).P < 0.05 {
			fp++
		}
	}
	if fp > 20 {
		t.Errorf("MK false positives %d/200", fp)
	}
	// ties and constants
	if r := MannKendall([]float64{1, 1, 1, 1}); r.P != 1 {
		t.Errorf("constant %+v", r)
	}
	// Known value: y = 1..10 strictly increasing: S = 45, var = 125, z = 44/sqrt(125)
	r := MannKendall([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	if r.S != 45 || r.Var != 125 || !near(r.Z, 44/math.Sqrt(125), 1e-12) || r.Tau != 1 {
		t.Errorf("%+v", r)
	}
	// long series are binned
	long := make([]float64, 5000)
	lt := make([]float64, 5000)
	for i := range long {
		lt[i] = float64(i)
		long[i] = 0.01 * float64(i)
	}
	if sl, _ := TheilSen(lt, long); !near(sl, 0.01, 1e-4) {
		t.Errorf("binned slope %v", sl)
	}
	if MannKendall(long).N != MaxTrendPoints {
		t.Error("binning")
	}
}

func TestWarmup(t *testing.T) {
	// 20 s of decaying warm-up then steady state
	x := normals(1, 120, 50, 2)
	for i := 0; i < 20; i++ {
		x[i] += 80 * math.Exp(-float64(i)/6)
	}
	d := MSER(x, 5)
	if d < 5 || d > 30 {
		t.Errorf("MSER truncation %d", d)
	}
	// steady series: little or no truncation on average
	total := 0
	for s := uint64(1); s <= 50; s++ {
		total += MSER(normals(s, 120, 50, 2), 5)
	}
	if avg := float64(total) / 50; avg > 20 {
		t.Errorf("average truncation on steady data %v", avg)
	}
	load := []float64{1, 2, 4, 6, 8, 10, 10, 10, 10, 10, 10, 10, 10, 10}
	if RampEnd(load, 0.9) != 5 {
		t.Errorf("ramp end %d", RampEnd(load, 0.9))
	}
	ramping := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if RampEnd(ramping, 0.9) != 0 {
		t.Error("ramp through the whole run => no warm-up")
	}
	if w := WarmupEnd(x, nil); w != d {
		t.Errorf("warmup %d vs %d", w, d)
	}
	if MSER([]float64{1, 2}, 5) != 0 {
		t.Error("short")
	}
}
