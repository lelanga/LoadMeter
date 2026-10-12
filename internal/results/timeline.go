package results

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/stats"
)

// Thresholds for timeline findings. A detected change must be both
// statistically supported (penalised change-point cost / Mann-Kendall) and
// large enough to matter.
const (
	cpPenaltyK      = 3    // beta = 3 sigma^2 ln n
	cpMinSegment    = 3    // seconds
	cpMinLatencyRel = 0.20 // 20% of the earlier level
	cpMinLatencyMs  = 1.0
	cpMinErrorPP    = 1.0
	cpMaxPerMetric  = 3
	driftAlpha      = 0.01 // stricter than usual: windows are autocorrelated
	driftMinWindows = 30
	driftMinRel     = 0.20
	memDriftMinMB   = 20
)

type tlSeries struct {
	t, y, vus []float64
}

// usableTimeline drops windows without requests and a trailing partial second.
func usableTimeline(tl []metrics.WindowOut) []metrics.WindowOut {
	var out []metrics.WindowOut
	for _, w := range tl {
		if w.RPS > 0 {
			out = append(out, w)
		}
	}
	if len(out) >= 3 {
		r := make([]float64, len(out))
		for i, w := range out {
			r[i] = w.RPS
		}
		if out[len(out)-1].RPS < 0.5*stats.Median(r) {
			out = out[:len(out)-1]
		}
	}
	return out
}

func series(tl []metrics.WindowOut, f func(metrics.WindowOut) float64) tlSeries {
	var s tlSeries
	for _, w := range tl {
		s.t = append(s.t, float64(w.T))
		s.y = append(s.y, f(w))
		s.vus = append(s.vus, float64(w.VUs))
	}
	return s
}

// timelineFindings reports level shifts (PELT change points) and slow drift
// (Theil-Sen + Mann-Kendall) in the per-second p95 and error-rate series, and
// drift in generator memory. It replaces the old first-half/second-half rule.
func timelineFindings(timeline []metrics.WindowOut) []Finding {
	tl := usableTimeline(timeline)
	if len(tl) < 2*cpMinSegment {
		return nil
	}
	var out []Finding
	p95 := series(tl, func(w metrics.WindowOut) float64 { return w.P95Ms })
	drift, ok := driftFinding(p95, "p95 latency", "ms")
	if ok {
		out = append(out, drift...)
	} else {
		out = append(out, stepFindings(p95, "p95 latency", 0.05*math.Abs(stats.Median(p95.y))+0.1, func(a, b float64) bool {
			return math.Abs(b-a) >= cpMinLatencyMs && math.Abs(b-a) >= cpMinLatencyRel*math.Max(a, 1e-9)
		})...)
	}
	errs := series(tl, func(w metrics.WindowOut) float64 { return float64(w.Errors) / w.RPS * 100 })
	out = append(out, stepFindings(errs, "error rate", 0.5, func(a, b float64) bool { return math.Abs(b-a) >= cpMinErrorPP })...)
	mem := series(tl, func(w metrics.WindowOut) float64 { return w.MemMB })
	if f, ok := memDrift(mem); ok {
		out = append(out, f)
	}
	return out
}

func stepFindings(s tlSeries, label string, minSigma float64, practical func(a, b float64) bool) []Finding {
	o := stats.ChangeOptions{PenaltyK: cpPenaltyK, MinSize: cpMinSegment, MinSigma: minSigma}
	cps := stats.PELT(s.y, o)
	if len(cps) == 0 {
		return nil
	}
	sigma := math.Max(stats.RobustSigma(s.y), minSigma)
	segs := stats.Segments(s.y, cps)
	type change struct {
		i    int
		a, b stats.Segment
		mag  float64
	}
	var ch []change
	for i := 1; i < len(segs); i++ {
		a, b := segs[i-1], segs[i]
		if practical(a.Median, b.Median) {
			ch = append(ch, change{i, a, b, math.Abs(b.Median - a.Median)})
		}
	}
	sort.SliceStable(ch, func(i, j int) bool { return ch[i].mag > ch[j].mag })
	if len(ch) > cpMaxPerMetric {
		ch = ch[:cpMaxPerMetric]
	}
	sort.SliceStable(ch, func(i, j int) bool { return ch[i].i < ch[j].i })
	method := fmt.Sprintf("PELT mean-shift change points on per-second values, penalty %d*sigma^2*ln(n) with sigma=%.3g (robust, from first differences), minimum segment %d s", cpPenaltyK, sigma, cpMinSegment)
	var out []Finding
	for _, c := range ch {
		at := int(s.t[c.b.Start])
		prevT := int(s.t[c.b.Start-1])
		ta := [2]int{int(s.t[c.a.Start]), int(s.t[c.a.End-1])}
		tb := [2]int{int(s.t[c.b.Start]), int(s.t[c.b.End-1])}
		va, vb := stats.Median(s.vus[c.a.Start:c.a.End]), stats.Median(s.vus[c.b.Start:c.b.End])
		up := c.b.Median > c.a.Median
		dir := map[bool]string{true: "rose", false: "fell"}[up]
		unit, mag := "ms", fmt.Sprintf("%.2f -> %.2f ms", c.a.Median, c.b.Median)
		if label == "error rate" {
			unit, mag = "%", fmt.Sprintf("%.2f%% -> %.2f%%", c.a.Median, c.b.Median)
		}
		rel := ""
		if c.a.Median > 0 && unit == "ms" {
			rel = fmt.Sprintf(" (%+.0f%%)", (c.b.Median-c.a.Median)/c.a.Median*100)
		}
		loadCtx := fmt.Sprintf(" Active VUs (median) were %.0f before and %.0f after.", va, vb)
		loadRose := vb >= 1.1*va && vb > va
		title := fmt.Sprintf("%s %s at about t=%ds", capitalize(label), dir, at)
		sev := "info"
		if up {
			sev = "warning"
		}
		if label == "error rate" && up && loadRose {
			title = "Errors increased as load increased"
		}
		out = append(out, Finding{Kind: "observation", Severity: sev, Title: title,
			Evidence: fmt.Sprintf("Per-second %s changed from a median of %s over t=%d-%ds to %s over t=%d-%ds%s; the shift happened between t=%ds and t=%ds.%s",
				label, fmtLevel(c.a.Median, unit), ta[0], ta[1], fmtLevel(c.b.Median, unit), tb[0], tb[1], rel, prevT, at, loadCtx),
			Method: method, Interval: &Interval{FromSec: prevT, ToSec: at}, Magnitude: mag + rel})
		if up && loadRose && label != "error rate" {
			out = append(out, Finding{Kind: "hypothesis", Severity: "warning", Title: fmt.Sprintf("%s increase coincides with higher load", capitalize(label)),
				Evidence: fmt.Sprintf("The %s step at t=%ds came with active VUs rising from %.0f to %.0f. The target may be approaching saturation; confirm with server-side metrics before concluding.", label, at, va, vb),
				Method:   method, Interval: &Interval{FromSec: prevT, ToSec: at}, Magnitude: mag + rel})
		}
	}
	return out
}

func fmtLevel(v float64, unit string) string {
	if unit == "%" {
		return fmt.Sprintf("%.2f%%", v)
	}
	return fmt.Sprintf("%.2f ms", v)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// driftFinding looks for a monotonic trend after warm-up under roughly
// constant load. It also reports ok=true when a drift explains the series
// better than steps (no change points remain after removing the trend).
func driftFinding(s tlSeries, label, unit string) ([]Finding, bool) {
	k := stats.WarmupEnd(s.y, s.vus)
	t, y, vus := s.t[k:], s.y[k:], s.vus[k:]
	if len(y) < driftMinWindows {
		return nil, false
	}
	vmin, vmax := math.Inf(1), 0.0
	for _, v := range vus {
		vmin, vmax = math.Min(vmin, v), math.Max(vmax, v)
	}
	if vmax > 1.1*vmin {
		return nil, false // load varies: a trend would be load-driven, steps cover it
	}
	slope, _ := stats.TheilSen(t, y)
	mk := stats.MannKendall(y)
	span := t[len(t)-1] - t[0]
	total := slope * span
	med := stats.Median(y)
	if mk.P >= driftAlpha || math.Abs(total) < cpMinLatencyMs || med <= 0 || math.Abs(total) < driftMinRel*med {
		return nil, false
	}
	resid := make([]float64, len(y))
	for i := range y {
		resid[i] = y[i] - slope*t[i]
	}
	if len(stats.PELT(resid, stats.ChangeOptions{PenaltyK: cpPenaltyK, MinSize: cpMinSegment, MinSigma: 0.05*med + 0.1})) > 0 {
		return nil, false // steps remain after detrending: report steps instead
	}
	dir := map[bool]string{true: "crept up", false: "drifted down"}[slope > 0]
	sev := "info"
	if slope > 0 {
		sev = "warning"
	}
	from, to := int(t[0]), int(t[len(t)-1])
	return []Finding{{Kind: "observation", Severity: sev, Title: fmt.Sprintf("%s %s steadily during the run", capitalize(label), dir),
		Evidence: fmt.Sprintf("Under constant load (%.0f VUs), per-second %s changed by about %+.2f %s over t=%d-%ds (%+.0f%% of the median %.2f %s; slope %+.3f %s/min). Mann-Kendall tau %.2f, p=%s. A slow creep can indicate resource exhaustion (memory, connections, queues) on the target; a longer run would confirm it.",
			vmax, label, total, unit, from, to, total/med*100, med, unit, slope*60, unit, mk.Tau, fmtP(mk.P)),
		Method:   fmt.Sprintf("Theil-Sen slope + Mann-Kendall trend test (alpha %.2g) on per-second values after %d warm-up windows", driftAlpha, k),
		Interval: &Interval{FromSec: from, ToSec: to}, Magnitude: fmt.Sprintf("%+.3f %s/min", slope*60, unit)}}, true
}

func memDrift(s tlSeries) (Finding, bool) {
	k := stats.WarmupEnd(s.y, nil)
	t, y := s.t[k:], s.y[k:]
	if len(y) < driftMinWindows || stats.Median(y) <= 0 {
		return Finding{}, false
	}
	slope, _ := stats.TheilSen(t, y)
	mk := stats.MannKendall(y)
	total := slope * (t[len(t)-1] - t[0])
	med := stats.Median(y)
	if mk.P >= driftAlpha || total < memDriftMinMB || total < driftMinRel*med {
		return Finding{}, false
	}
	from, to := int(t[0]), int(t[len(t)-1])
	return Finding{Kind: "observation", Severity: "info", Title: "Load generator memory grew during the run",
		Evidence: fmt.Sprintf("Generator memory rose by about %.0f MB over t=%d-%ds (slope %+.1f MB/min, Mann-Kendall p=%s). This is the load generator process, not the target; large response bodies or unbounded data sources are common causes.",
			total, from, to, slope*60, fmtP(mk.P)),
		Method:   "Theil-Sen slope + Mann-Kendall trend test on per-second generator memory",
		Interval: &Interval{FromSec: from, ToSec: to}, Magnitude: fmt.Sprintf("%+.1f MB/min", slope*60)}, true
}
