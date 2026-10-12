package summary

import (
	"fmt"
	"math"
	"strings"
)

// MaxLines is the longest template summary.
const MaxLines = 12

// Render produces the deterministic plain-English summary (at most MaxLines
// lines). It needs no AI and uses only numbers present in f.
func Render(f Facts) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	h := f.Headline

	if h.Requests == 0 {
		add("The test of \"%s\" finished without completing any requests, so there is nothing to measure.", f.Scenario)
		add("Check that the target is reachable and the scenario is valid, then run it again.")
		return out
	}

	// 1. what was run
	load := ""
	switch {
	case f.Load.Rate > 0:
		load = fmt.Sprintf(" at a target of %s iterations per second", num(f.Load.Rate))
	case f.Load.PeakVUs > 0:
		load = fmt.Sprintf(" with up to %s virtual users", commas(int64(f.Load.PeakVUs)))
	}
	stopped := ""
	if f.Status == "stopped" || f.Status == "stopping" {
		stopped = " It was stopped early."
	}
	add("The test \"%s\" ran for %s%s.%s", f.Scenario, f.DurationText, load, stopped)

	// 2. volume and failures
	if h.Errors == 0 {
		add("It made %s requests (about %s per second) and none of them failed.", commas(int64(h.Requests)), short(h.RPS))
	} else {
		add("It made %s requests (about %s per second) and %s%% of them failed (%s requests).", commas(int64(h.Requests)), short(h.RPS), num(h.ErrorRatePct), commas(int64(h.Errors)))
	}

	// 3. speed
	add("Half of the responses took %s ms or less; %d%% took %s ms or less, and the slowest took %s ms.", short(h.P50Ms), h.Percentiles[1], short(h.P95Ms), short(h.MaxMs))

	// 4. targets (SLAs)
	switch {
	case f.SLAs.Defined == 0:
	case f.SLAs.Failed == 0:
		add("All %d pass/fail targets (SLAs) were met.", f.SLAs.Defined)
	default:
		var missed []string
		for _, r := range f.SLAs.Results {
			if !r.Passed {
				missed = append(missed, r.Text)
			}
		}
		more := ""
		if len(missed) > 2 {
			more = " (and others)"
			missed = missed[:2]
		}
		add("%d of %d pass/fail targets (SLAs) were missed: %s%s.", f.SLAs.Failed, f.SLAs.Defined, strings.Join(missed, "; "), more)
	}

	// 5-8. failures
	if len(f.Failures.Clusters) > 0 {
		c := f.Failures.Clusters[0]
		timing := ""
		if !f.Failures.Coarse {
			timing = fmt.Sprintf(", first seen %s s into the run", num(c.FirstSeenSec))
			if c.FirstSeenSec == 0 {
				timing = ", from the very start of the run"
			}
		}
		add("The failures fall into %d groups. The biggest (%s%% of failures%s) is: %s.", f.Failures.Groups, num(c.Percent), timing, c.What)
		add("That looks like a problem with %s (%s confidence). %s", c.SideText, c.Confidence, c.Explanation)
		if len(f.Failures.Clusters) > 1 {
			d := f.Failures.Clusters[1]
			add("The second biggest (%s%% of failures) is: %s; it points to %s.", num(d.Percent), d.What, d.SideText)
		}
		if parts := sideSplit(f); parts != "" {
			add("Split by side: %s.", parts)
		}
	}

	// 9. other findings
	var notes []string
	for _, fi := range f.Findings {
		if (fi.Severity == "critical" || fi.Severity == "warning") && !strings.HasPrefix(fi.Title, "Error rate") && !strings.HasPrefix(fi.Title, "SLA breached") {
			notes = append(notes, fi.Title)
		}
		if len(notes) == 2 {
			break
		}
	}
	if len(notes) > 0 {
		add("Also noted: %s.", strings.Join(notes, "; "))
	}

	// 10. comparison
	if c := f.Comparison; c != nil {
		switch {
		case c.Regressions == 0:
			add("Compared with the earlier run, nothing got meaningfully worse.")
		default:
			w := ""
			if len(c.Worst) > 0 {
				d := c.Worst[0]
				w = fmt.Sprintf(" The largest change: %s %s went from %s to %s (%s%%).", d.Name, metricWords(d.Metric), num(d.Baseline), num(d.Current), num(d.ChangePercent))
			}
			noun := "measurements got"
			if c.Regressions == 1 {
				noun = "measurement got"
			}
			add("Compared with the earlier run, %d %s worse.%s", c.Regressions, noun, w)
		}
	}

	// 11. verdict and next step
	switch f.Verdict {
	case "passed":
		add("Overall: the run looks healthy.")
	case "failed":
		add("Overall: the run itself failed, so treat these numbers with care.")
	default:
		if len(f.Failures.Clusters) > 0 {
			add("Overall: there are problems to look at. Next step: %s", f.Failures.Clusters[0].NextCheck)
		} else {
			add("Overall: there are problems to look at; see the findings in the full report.")
		}
	}
	if len(out) > MaxLines {
		// keep the head and the closing verdict
		out = append(out[:MaxLines-1], out[len(out)-1])
	}
	return out
}

// short rounds for reading: whole numbers from 10 up, one decimal below.
func short(v float64) string {
	if v >= 10 {
		return commas(int64(math.Round(v)))
	}
	return num(round(v, 1))
}

func sideSplit(f Facts) string {
	var parts []string
	for _, side := range []string{"target", "script", "generator", "policy", "unknown"} {
		if p, ok := f.Failures.SidePct[side]; ok && p > 0 {
			parts = append(parts, fmt.Sprintf("%s%% of failures point to %s", num(p), SideText(side)))
		}
	}
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func metricWords(m string) string {
	switch m {
	case "p50Ms":
		return "median response time (ms)"
	case "p95Ms":
		return "p95 response time (ms)"
	case "p99Ms":
		return "p99 response time (ms)"
	case "errorRate":
		return "failure rate (%)"
	case "rps":
		return "requests per second"
	}
	return m
}

// Text joins the lines.
func Text(lines []string) string { return strings.Join(lines, "\n") }
