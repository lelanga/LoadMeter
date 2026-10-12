package stats

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func normals(seed uint64, n int, mu, sd float64) []float64 {
	r := NewRand(seed)
	x := make([]float64, n)
	for i := range x {
		x[i] = mu + sd*r.NormFloat64()
	}
	return x
}

func TestDescriptive(t *testing.T) {
	x := []float64{1, 2, 3, 4, 100}
	if Median(x) != 3 || MAD(x) != 1 || Mean(x) != 22 {
		t.Errorf("median %v mad %v mean %v", Median(x), MAD(x), Mean(x))
	}
	if q := Quantile([]float64{1, 2, 3, 4}, 0.5); q != 2.5 {
		t.Errorf("type-7 median %v", q)
	}
	if q := Quantile([]float64{10, 20}, 0.25); q != 12.5 {
		t.Errorf("q25 %v", q)
	}
	if !near(StdDev([]float64{2, 4, 4, 4, 5, 5, 7, 9}), 2.138, 1e-3) {
		t.Error("stddev")
	}
	if Median(nil) != 0 || MAD(nil) != 0 || StdDev([]float64{1}) != 0 {
		t.Error("empty handling")
	}
	// RobustSigma ignores a level shift
	y := normals(3, 400, 10, 1)
	for i := 200; i < 400; i++ {
		y[i] += 50
	}
	if s := RobustSigma(y); s < 0.8 || s > 1.25 {
		t.Errorf("robust sigma %v (true 1)", s)
	}
}

func TestNormal(t *testing.T) {
	for _, c := range []struct{ p, z float64 }{{0.5, 0}, {0.975, 1.959964}, {0.995, 2.575829}, {0.001, -3.090232}, {1e-8, -5.612001}} {
		if z := NormalQuantile(c.p); !near(z, c.z, 1e-5) {
			t.Errorf("q(%v)=%v want %v", c.p, z, c.z)
		}
		if p := NormalCDF(c.z); !near(p, c.p, 1e-6) {
			t.Errorf("cdf(%v)=%v want %v", c.z, p, c.p)
		}
	}
}

func TestRanksAndHolm(t *testing.T) {
	r, tie := Ranks([]float64{10, 20, 20, 5, 20})
	want := []float64{2, 4, 4, 1, 4}
	for i := range want {
		if r[i] != want[i] {
			t.Fatalf("ranks %v", r)
		}
	}
	if tie != 24 { // one group of 3: 27-3
		t.Errorf("tie term %v", tie)
	}
	h := Holm([]float64{0.01, 0.04, 0.03, 0.5})
	// sorted 0.01*4=.04, .03*3=.09, .04*2=.08->.09 (monotone), .5
	w := []float64{0.04, 0.09, 0.09, 0.5}
	for i := range w {
		if !near(h[i], w[i], 1e-12) {
			t.Errorf("holm %v want %v", h, w)
		}
	}
}

func TestMannWhitneyKnownValues(t *testing.T) {
	// Exact: x all above y, n=4,4 -> U=16, one-sided p = 1/C(8,4) = 1/70.
	x, y := []float64{5, 6, 7, 8}, []float64{1, 2, 3, 4}
	r := MannWhitney(x, y, Greater)
	if !r.Exact || r.U != 16 || !near(r.P, 1.0/70, 1e-12) {
		t.Errorf("%+v", r)
	}
	if r2 := MannWhitney(x, y, TwoSided); !near(r2.P, 2.0/70, 1e-12) {
		t.Errorf("two-sided %+v", r2)
	}
	if r3 := MannWhitney(x, y, Less); !near(r3.P, 1, 1e-12) {
		t.Errorf("less %+v", r3)
	}
	// Exact with interleaving: x={1,3,5}, y={2,4,6}: U = 0+1+2 = 3, P(U<=3) for n=3,3 = 7/20.
	r = MannWhitney([]float64{1, 3, 5}, []float64{2, 4, 6}, Less)
	if r.U != 3 || !near(r.P, 7.0/20, 1e-12) {
		t.Errorf("%+v", r)
	}
	// Normal approximation with ties: U checked by brute-force pair counting,
	// p checked against the textbook formula.
	x = []float64{1, 2, 2, 3, 3, 3, 4, 4, 5, 6, 7, 8, 8, 9, 10, 11, 12, 13}
	y = []float64{1, 1, 2, 2, 2, 3, 3, 4, 4, 4, 5, 5, 6, 6, 7, 8}
	var bruteU float64
	for _, a := range x {
		for _, b := range y {
			switch {
			case a > b:
				bruteU++
			case a == b:
				bruteU += 0.5
			}
		}
	}
	r = MannWhitney(x, y, TwoSided)
	if r.Exact || r.U != bruteU {
		t.Errorf("U=%v want %v exact=%v", r.U, bruteU, r.Exact)
	}
	n1, n2 := 18.0, 16.0
	_, tie := Ranks(append(append([]float64{}, x...), y...))
	v := n1 * n2 / 12 * ((n1 + n2 + 1) - tie/((n1+n2)*(n1+n2-1)))
	wantP := 2 * (1 - NormalCDF((bruteU-n1*n2/2-0.5)/math.Sqrt(v)))
	if !near(r.P, wantP, 1e-12) {
		t.Errorf("p=%v want %v", r.P, wantP)
	}
	if g := MannWhitney(x, y, Greater); !near(g.P, wantP/2, 1e-12) {
		t.Errorf("one-sided p=%v want %v", g.P, wantP/2)
	}
	// identical constant samples
	if r := MannWhitney([]float64{1, 1, 1}, []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, TwoSided); r.P != 1 {
		t.Errorf("constant %+v", r)
	}
	if r := MannWhitney(nil, y, TwoSided); r.P != 1 {
		t.Error("empty")
	}
}

func TestExactMatchesEnumeration(t *testing.T) {
	// brute force: all C(7,3) labelings of 1..7
	n1, n2 := 3, 4
	cdf := exactUDist(n1, n2)
	counts := make([]int, n1*n2+1)
	total := 0
	for mask := 0; mask < 1<<7; mask++ {
		if popcount(mask) != n1 {
			continue
		}
		u := 0
		for i := 0; i < 7; i++ {
			if mask&(1<<i) != 0 {
				for j := 0; j < i; j++ {
					if mask&(1<<j) == 0 {
						u++
					}
				}
			}
		}
		counts[u]++
		total++
	}
	acc := 0
	for u := range counts {
		acc += counts[u]
		if !near(cdf[u], float64(acc)/float64(total), 1e-12) {
			t.Fatalf("u=%d cdf %v want %v", u, cdf[u], float64(acc)/float64(total))
		}
	}
}

func popcount(x int) int {
	c := 0
	for ; x > 0; x &= x - 1 {
		c++
	}
	return c
}

func TestMannWhitneyDetectsShift(t *testing.T) {
	detected := 0
	for s := uint64(1); s <= 50; s++ {
		a := normals(s, 60, 100, 10)
		b := normals(s+1000, 60, 108, 10) // 0.8 sigma shift
		if r := MannWhitney(b, a, Greater); r.P < 0.05 {
			detected++
		}
	}
	if detected < 45 { // power ~0.99 for d=0.8, n=60
		t.Errorf("detected %d/50", detected)
	}
}

func TestMannWhitneyFalsePositiveRate(t *testing.T) {
	const trials = 1000
	alpha := 0.05
	fp := 0
	for s := uint64(1); s <= trials; s++ {
		a := normals(s, 40, 50, 5)
		b := normals(s+1e6, 40, 50, 5)
		if MannWhitney(a, b, TwoSided).P < alpha {
			fp++
		}
	}
	rate := float64(fp) / trials
	// binomial sd at 0.05 with 1000 trials is 0.0069; allow ~3 sd
	if rate > alpha+0.021 {
		t.Errorf("false positive rate %.3f exceeds alpha %.2f", rate, alpha)
	}
	t.Logf("MW false-positive rate %.3f at alpha %.2f", rate, alpha)
}

func TestCliffsDelta(t *testing.T) {
	if d := CliffsDelta([]float64{5, 6, 7}, []float64{1, 2, 3}); d != 1 {
		t.Errorf("%v", d)
	}
	if d := CliffsDelta([]float64{1, 2}, []float64{1, 2}); d != 0 {
		t.Errorf("%v", d)
	}
	// x={1,3}, y={2}: 3>2 (+1), 1<2 (-1) => 0; x={2,3} y={2}: tie, gt => 0.5
	if d := CliffsDelta([]float64{2, 3}, []float64{2}); d != 0.5 {
		t.Errorf("%v", d)
	}
	// relation to U: delta = 2U/(n1 n2) - 1
	a, b := normals(1, 30, 0, 1), normals(2, 25, 0.5, 1)
	u := MannWhitney(a, b, TwoSided).U
	if d := CliffsDelta(a, b); !near(d, 2*u/(30*25)-1, 1e-12) {
		t.Errorf("delta %v vs U %v", d, u)
	}
	if CliffsMagnitude(0.1) != "negligible" || CliffsMagnitude(-0.4) != "medium" || CliffsMagnitude(0.9) != "large" {
		t.Error("magnitude labels")
	}
}

func TestBootstrapDeterministicAndCovers(t *testing.T) {
	a, b := normals(1, 80, 100, 10), normals(2, 80, 120, 10)
	o := BootstrapOptions{Resamples: 1000, Seed: 7}
	c1 := BootstrapCI(a, b, MedianShift, o)
	c2 := BootstrapCI(a, b, MedianShift, o)
	if c1 != c2 {
		t.Fatal("bootstrap must be deterministic for a seed")
	}
	if !(c1.Lo < 20 && 20 < c1.Hi) || c1.Lo <= 0 {
		t.Errorf("CI %+v should cover 20 and exclude 0", c1)
	}
	if c3 := BootstrapCI(a, b, MedianShift, BootstrapOptions{Resamples: 1000, Seed: 8}); c3 == c1 {
		t.Error("different seed should give a different interval")
	}
	// coverage over trials: 95% CI should contain the true shift most of the time
	cover := 0
	for s := uint64(1); s <= 100; s++ {
		x, y := normals(s, 60, 0, 1), normals(s+500, 60, 1, 1)
		ci := BootstrapCI(x, y, func(b, c []float64) float64 { return Mean(c) - Mean(b) }, BootstrapOptions{Resamples: 400, Seed: s, Block: 1})
		if ci.Lo <= 1 && 1 <= ci.Hi {
			cover++
		}
	}
	if cover < 88 {
		t.Errorf("coverage %d/100", cover)
	}
	if AutoBlock(1000) != 10 || AutoBlock(1) != 1 {
		t.Error("auto block")
	}
	if ci := BootstrapOne([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9}, Median, BootstrapOptions{Seed: 1}); ci.Estimate != 5 || ci.Lo > 5 || ci.Hi < 5 {
		t.Errorf("one-sample %+v", ci)
	}
}

func TestTwoProportions(t *testing.T) {
	r := TwoProportions(10, 1000, 40, 1000, 0.95)
	if !r.Comparable || !near(r.Diff, 0.03, 1e-12) || r.P > 0.001 || r.Lo <= 0 {
		t.Errorf("%+v", r)
	}
	r = TwoProportions(10, 1000, 12, 1000, 0.95)
	if r.P < 0.3 || r.Lo > 0 {
		t.Errorf("%+v", r)
	}
	if r := TwoProportions(0, 0, 1, 10, 0.95); r.Comparable || r.P != 1 {
		t.Errorf("%+v", r)
	}
	if r := TwoProportions(0, 100, 0, 100, 0.95); r.P != 1 {
		t.Errorf("both zero %+v", r)
	}
}

func TestMannWhitneyDependent(t *testing.T) {
	ar := func(seed uint64, n int, phi float64) []float64 {
		r := NewRand(seed)
		x, e := make([]float64, n), 0.0
		for i := range x {
			e = phi*e + r.NormFloat64()
			x[i] = e
		}
		return x
	}
	if l := Lag1(ar(1, 2000, 0.7)); !near(l, 0.7, 0.05) {
		t.Errorf("lag1 %v", l)
	}
	if Lag1([]float64{1, 1, 1}) != 0 {
		t.Error("constant")
	}
	// iid data: (almost) unchanged p-value
	a, b := normals(1, 50, 0, 1), normals(2, 50, 0.5, 1)
	plain := MannWhitney(a, b, TwoSided)
	dep, r := MannWhitneyDependent(a, b, TwoSided)
	if r > 0.3 || dep.P < plain.P {
		t.Errorf("iid r=%v p %v vs %v", r, dep.P, plain.P)
	}
	// AR(1) phi=0.6: false-positive rate of the plain test is inflated; the
	// adjusted one stays near alpha.
	fpPlain, fpDep := 0, 0
	for s := uint64(1); s <= 300; s++ {
		x, y := ar(s, 60, 0.6), ar(s+9999, 60, 0.6)
		if MannWhitney(x, y, TwoSided).P < 0.05 {
			fpPlain++
		}
		if p, _ := MannWhitneyDependent(x, y, TwoSided); p.P < 0.05 {
			fpDep++
		}
	}
	if fpDep >= fpPlain || float64(fpDep)/300 > 0.08 {
		t.Errorf("AR(1) false positives: plain %d/300, adjusted %d/300", fpPlain, fpDep)
	}
	t.Logf("AR(1) false positives: plain %d/300, adjusted %d/300", fpPlain, fpDep)
}
