package results

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Gate outcomes and their stable process exit codes.
const (
	GatePass         = "pass"
	GateFail         = "fail"
	GateInconclusive = "inconclusive"

	ExitGatePass         = 0
	ExitGateFail         = 3 // same code as an SLA failure in `loadmeter run`
	ExitGateInconclusive = 4
)

// GateCheck is one line of the gate (one JUnit test case).
type GateCheck struct {
	Class   string `json:"class"`  // run | sla | regression
	Name    string `json:"name"`   // e.g. "ALL p95Ms"
	Status  string `json:"status"` // pass | fail | inconclusive | skipped
	Message string `json:"message"`
}

// GateResult combines SLA results of the current run and the statistical
// comparison with a baseline into one CI verdict.
type GateResult struct {
	RunID      string      `json:"runId"`
	Baseline   string      `json:"baselineRun,omitempty"`
	Status     string      `json:"status"`
	ExitCode   int         `json:"exitCode"`
	Checks     []GateCheck `json:"checks"`
	Comparison *Comparison `json:"comparison,omitempty"`
	Summary    string      `json:"summary"`
}

// Gate evaluates cur: any failed SLA or statistical regression fails the
// gate; otherwise an inconclusive whole-run p95 or error-rate comparison (or
// a run without requests) makes it inconclusive. base may be nil (SLAs only).
func Gate(cur, base *Summary, o CompareOptions) *GateResult {
	g := &GateResult{RunID: cur.RunID}
	add := func(c GateCheck) { g.Checks = append(g.Checks, c) }
	switch {
	case cur.Snapshot.Requests.Count == 0:
		add(GateCheck{"run", "requests completed", GateInconclusive, "The run recorded no requests."})
	case cur.Status == "failed" || strings.HasPrefix(cur.Status, "aborted"):
		add(GateCheck{"run", "run completed", GateFail, "The run status is " + cur.Status + "."})
	default:
		add(GateCheck{"run", "run completed", GatePass, fmt.Sprintf("%d requests, status %s.", cur.Snapshot.Requests.Count, cur.Status)})
	}
	for _, s := range cur.SLAs {
		scope := s.Scope
		if scope == "" {
			scope = "all"
		}
		c := GateCheck{Class: "sla", Name: fmt.Sprintf("%s %s %s %g", scope, s.Metric, s.Op, s.Threshold), Status: GatePass, Message: fmt.Sprintf("actual %g", s.Actual)}
		if !s.Passed {
			c.Status = GateFail
			c.Message = strings.TrimSpace(fmt.Sprintf("actual %g violates %s %g. %s", s.Actual, s.Op, s.Threshold, s.Note))
		}
		add(c)
	}
	if base != nil {
		g.Baseline = base.RunID
		cmp := CompareWith(base, cur, o)
		g.Comparison = cmp
		for _, d := range cmp.Deltas {
			if d.Informational {
				continue
			}
			c := GateCheck{Class: "regression", Name: d.Name + " " + d.Metric, Message: d.Reason}
			switch d.Verdict {
			case VerdictRegression:
				c.Status = GateFail
			case VerdictInconclusive:
				c.Status = "skipped"
				if d.Name == "ALL" && (d.Metric == "p95Ms" || d.Metric == "errorRate") {
					c.Status = GateInconclusive
				}
			default:
				c.Status = GatePass
			}
			add(c)
		}
	}
	g.Status, g.ExitCode = GatePass, ExitGatePass
	for _, c := range g.Checks {
		if c.Status == GateFail {
			g.Status, g.ExitCode = GateFail, ExitGateFail
			break
		}
		if c.Status == GateInconclusive {
			g.Status, g.ExitCode = GateInconclusive, ExitGateInconclusive
		}
	}
	g.Summary = g.count()
	return g
}

func (g *GateResult) count() string {
	n := map[string]int{}
	for _, c := range g.Checks {
		n[c.Status]++
	}
	return fmt.Sprintf("%s: %d checks, %d failed, %d inconclusive, %d skipped", strings.ToUpper(g.Status), len(g.Checks), n[GateFail], n[GateInconclusive], n["skipped"])
}

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Class   string    `xml:"classname,attr"`
	Name    string    `xml:"name,attr"`
	Failure *junitMsg `xml:"failure,omitempty"`
	Skipped *junitMsg `xml:"skipped,omitempty"`
	Out     string    `xml:"system-out,omitempty"`
}

type junitMsg struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes the gate as JUnit XML: one suite per check class; failed
// checks are <failure>, inconclusive and skipped ones are <skipped>.
func (g *GateResult) WriteJUnit(w io.Writer) error {
	bySuite := map[string]*junitSuite{}
	var order []string
	for _, c := range g.Checks {
		s := bySuite[c.Class]
		if s == nil {
			s = &junitSuite{Name: "loadmeter." + c.Class}
			bySuite[c.Class] = s
			order = append(order, c.Class)
		}
		jc := junitCase{Class: "loadmeter." + c.Class, Name: c.Name}
		switch c.Status {
		case GateFail:
			jc.Failure = &junitMsg{Message: c.Message, Type: c.Class, Text: c.Message}
			s.Failures++
		case GateInconclusive, "skipped":
			jc.Skipped = &junitMsg{Message: c.Status + ": " + c.Message}
			s.Skipped++
		default:
			jc.Out = c.Message
		}
		s.Tests++
		s.Cases = append(s.Cases, jc)
	}
	all := junitSuites{Name: "loadmeter gate " + g.RunID}
	for _, k := range order {
		s := bySuite[k]
		all.Tests += s.Tests
		all.Failures += s.Failures
		all.Skipped += s.Skipped
		all.Suites = append(all.Suites, *s)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(all); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// Markdown renders a short summary suitable for a pull-request comment.
func (g *GateResult) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Load Meter gate: **%s**\n\n", strings.ToUpper(g.Status))
	fmt.Fprintf(&b, "Run `%s`", g.RunID)
	if g.Baseline != "" {
		fmt.Fprintf(&b, " vs baseline `%s`", g.Baseline)
	}
	fmt.Fprintf(&b, ". %s.\n\n", g.Summary)
	if c := g.Comparison; c != nil {
		b.WriteString("| Scope | Metric | Baseline | Current | Change | Verdict |\n|---|---|---:|---:|---:|---|\n")
		for _, d := range c.Deltas {
			if d.Name != "ALL" && (d.Verdict != VerdictRegression || d.Informational) {
				continue // keep the comment short: whole run + transaction regressions
			}
			v := d.Verdict
			if d.Informational {
				v += " (informational)"
			}
			fmt.Fprintf(&b, "| %s | %s | %.2f | %.2f | %+.1f%% | %s |\n", d.Name, d.Metric, d.Baseline, d.Current, d.Change, v)
		}
		b.WriteString("\n")
	}
	var bad []GateCheck
	for _, c := range g.Checks {
		if c.Status == GateFail || c.Status == GateInconclusive {
			bad = append(bad, c)
		}
	}
	sort.SliceStable(bad, func(i, j int) bool { return bad[i].Status == GateFail && bad[j].Status != GateFail })
	for _, c := range bad {
		fmt.Fprintf(&b, "- **%s** %s `%s`: %s\n", c.Status, c.Class, c.Name, c.Message)
	}
	if c := g.Comparison; c != nil {
		for _, w := range c.Warnings {
			fmt.Fprintf(&b, "- warning: %s\n", w)
		}
		fmt.Fprintf(&b, "\n<sub>alpha %.3g (Holm), minimum effect %.3g%% and %.3g ms, errors %.3g pp. Per-second window percentiles after warm-up; exit codes 0 pass, 3 fail, 4 inconclusive.</sub>\n",
			c.Alpha, c.MinEffectPct, c.MinEffectMs, c.MinErrorPP)
	}
	return b.String()
}
