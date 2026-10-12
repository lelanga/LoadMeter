// Package summary writes a short plain-English summary of a finished run that
// is grounded in a deterministic "facts document".
//
// Every number in the summary comes from Facts, which is built only from the
// run's recorded results (summary.json, triage clusters, comparison). A
// deterministic template renders the summary with AI switched off. Optionally
// an AI model may reword the facts; its text is then machine-checked by Verify
// and replaced by the template if it contains any number that is not in Facts.
package summary

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/triage"
)

// Facts is the grounding document. All numbers are aggregates; messages are
// the normalised templates (quoted values already stripped), never raw samples.
type Facts struct {
	RunID        string          `json:"runId"`
	Scenario     string          `json:"scenario"`
	Status       string          `json:"status"`
	Verdict      string          `json:"verdict"` // passed | problems | failed | no_data
	DurationSec  float64         `json:"durationSec"`
	DurationText string          `json:"durationText"`
	Load         LoadFacts       `json:"load"`
	Headline     Headline        `json:"headline"`
	SLAs         SLAFacts        `json:"slas"`
	Failures     FailureFacts    `json:"failures"`
	Findings     []FindingFact   `json:"findings,omitempty"`
	Comparison   *ComparisonFact `json:"comparison,omitempty"`
}

type LoadFacts struct {
	Executor string  `json:"executor"`
	PeakVUs  int     `json:"peakVirtualUsers"`
	Rate     float64 `json:"targetIterationsPerSecond,omitempty"`
}

type Headline struct {
	Requests     uint64  `json:"requests"`
	RPS          float64 `json:"requestsPerSecond"`
	Errors       uint64  `json:"failedRequests"`
	ErrorRatePct float64 `json:"failedPercent"`
	P50Ms        float64 `json:"medianMs"`
	P95Ms        float64 `json:"p95Ms"`
	P99Ms        float64 `json:"p99Ms"`
	MaxMs        float64 `json:"maxMs"`
	Iterations   uint64  `json:"iterations"`
	Dropped      uint64  `json:"droppedIterations,omitempty"`
	// Percentiles names the ranks behind medianMs, p95Ms and p99Ms.
	Percentiles []int `json:"percentileRanks"`
}

type SLAFact struct {
	Scope     string  `json:"scope"`
	Metric    string  `json:"metric"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
	Actual    float64 `json:"actual"`
	Passed    bool    `json:"passed"`
	Text      string  `json:"text"` // plain-English description
}

type SLAFacts struct {
	Defined int       `json:"defined"`
	Passed  int       `json:"passed"`
	Failed  int       `json:"failed"`
	Results []SLAFact `json:"results,omitempty"`
}

type ClusterFact struct {
	Rank         int     `json:"rank"`
	Side         string  `json:"side"` // script | target | generator | policy | unknown
	SideText     string  `json:"sideText"`
	Confidence   string  `json:"confidence"`
	Request      string  `json:"request"`
	Status       int     `json:"httpStatus,omitempty"`
	Count        uint64  `json:"failures"`
	Percent      float64 `json:"percentOfFailures"`
	FirstSeenSec float64 `json:"firstSeenSec"`
	What         string  `json:"what"`
	Explanation  string  `json:"explanation"`
	NextCheck    string  `json:"nextCheck"`
}

type FailureFacts struct {
	Total     uint64             `json:"total"`
	Groups    int                `json:"groups"`
	Clusters  []ClusterFact      `json:"topClusters,omitempty"`
	SidePct   map[string]float64 `json:"percentBySide,omitempty"`
	Coarse    bool               `json:"coarse,omitempty"`
	Available bool               `json:"available"`
}

type FindingFact struct {
	Kind     string `json:"kind"` // observation | hypothesis
	Severity string `json:"severity"`
	Title    string `json:"title"`
}

type ComparisonFact struct {
	BaselineRun string       `json:"baselineRun"`
	Regressions int          `json:"regressions"`
	Verdict     string       `json:"verdict"` // regressed | no_regression
	Worst       []DeltaFact  `json:"worst,omitempty"`
	Changes     []ChangeFact `json:"overall,omitempty"`
}

type DeltaFact struct {
	Name          string  `json:"name"`
	Metric        string  `json:"metric"`
	Baseline      float64 `json:"baseline"`
	Current       float64 `json:"current"`
	ChangePercent float64 `json:"changePercent"`
}

type ChangeFact struct {
	Metric        string  `json:"metric"`
	Baseline      float64 `json:"baseline"`
	Current       float64 `json:"current"`
	ChangePercent float64 `json:"changePercent"`
}

const maxClusters = 3

// BuildFacts assembles the facts document. tri and cmp may be nil.
func BuildFacts(s *results.Summary, tri *triage.Result, cmp *results.Comparison) Facts {
	r := s.Snapshot.Requests
	f := Facts{RunID: s.RunID, Scenario: s.Scenario, Status: s.Status, DurationSec: round(s.DurationS, 1), DurationText: DurationText(s.DurationS)}
	f.Load = LoadFacts{Executor: s.Workload.Executor, Rate: s.Workload.Rate}
	for _, w := range s.Snapshot.Timeline {
		f.Load.PeakVUs = max(f.Load.PeakVUs, w.VUs)
	}
	if f.Load.PeakVUs == 0 {
		f.Load.PeakVUs = s.Workload.VUs
	}
	f.Headline = Headline{Requests: r.Count, RPS: round(r.RPS, 1), Errors: r.Errors, ErrorRatePct: round(r.ErrorRate, 2),
		P50Ms: round(r.P50Ms, 1), P95Ms: round(r.P95Ms, 1), P99Ms: round(r.P99Ms, 1), MaxMs: round(r.MaxMs, 1),
		Iterations: s.Snapshot.Iterations, Dropped: s.Snapshot.Dropped, Percentiles: []int{50, 95, 99}}
	f.SLAs.Defined = len(s.SLAs)
	for _, x := range s.SLAs {
		if x.Passed {
			f.SLAs.Passed++
		} else {
			f.SLAs.Failed++
		}
		f.SLAs.Results = append(f.SLAs.Results, SLAFact{Scope: x.Scope, Metric: x.Metric, Op: x.Op, Threshold: x.Threshold,
			Actual: x.Actual, Passed: x.Passed, Text: slaText(x)})
	}
	if tri != nil {
		f.Failures = FailureFacts{Total: tri.TotalFailures, Groups: len(tri.Clusters), Coarse: tri.Coarse, Available: true}
		if tri.TotalFailures > 0 {
			f.Failures.SidePct = map[string]float64{}
			for side, n := range tri.SideCounts {
				f.Failures.SidePct[side] = round(float64(n)/float64(tri.TotalFailures)*100, 1)
			}
		}
		for i, c := range tri.Clusters {
			if i >= maxClusters {
				break
			}
			cf := ClusterFact{Rank: c.Rank, Side: c.Side, SideText: SideText(c.Side), Confidence: c.Confidence, Request: c.Request,
				Status: c.Status, Count: c.Count, Percent: c.Percent, FirstSeenSec: c.FirstSeenSec,
				What: What(c), Explanation: c.Explanation, NextCheck: c.NextCheck}
			if tri.Coarse {
				cf.FirstSeenSec = 0
			}
			f.Failures.Clusters = append(f.Failures.Clusters, cf)
		}
	} else {
		f.Failures = FailureFacts{Total: r.Errors}
	}
	for _, fi := range s.Findings {
		f.Findings = append(f.Findings, FindingFact{Kind: fi.Kind, Severity: fi.Severity, Title: fi.Title})
		if len(f.Findings) == 6 {
			break
		}
	}
	if cmp != nil {
		f.Comparison = comparisonFacts(cmp)
	}
	f.Verdict = verdict(s, f)
	return f
}

func verdict(s *results.Summary, f Facts) string {
	switch {
	case f.Headline.Requests == 0:
		return "no_data"
	case s.Status == "failed":
		return "failed"
	case f.SLAs.Failed > 0, f.Headline.ErrorRatePct >= 1, f.Comparison != nil && f.Comparison.Regressions > 0:
		return "problems"
	}
	return "passed"
}

func comparisonFacts(c *results.Comparison) *ComparisonFact {
	cf := &ComparisonFact{BaselineRun: c.BaselineRun, Regressions: c.Regressions, Verdict: "no_regression"}
	if c.Regressions > 0 {
		cf.Verdict = "regressed"
	}
	var worse []results.Delta
	for _, d := range c.Deltas {
		if d.Name == "ALL" && (d.Metric == "p95Ms" || d.Metric == "errorRate" || d.Metric == "rps") {
			cf.Changes = append(cf.Changes, ChangeFact{Metric: d.Metric, Baseline: round(d.Baseline, 2), Current: round(d.Current, 2), ChangePercent: round(d.Change, 1)})
		}
		if d.Worse {
			worse = append(worse, d)
		}
	}
	sort.SliceStable(worse, func(i, j int) bool { return worse[i].Change > worse[j].Change })
	for i, d := range worse {
		if i == 3 {
			break
		}
		cf.Worst = append(cf.Worst, DeltaFact{Name: d.Name, Metric: d.Metric, Baseline: round(d.Baseline, 2), Current: round(d.Current, 2), ChangePercent: round(d.Change, 1)})
	}
	return cf
}

func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}

// DurationText renders seconds as "2 min 5 s" / "45 s" / "1 h 3 min".
func DurationText(sec float64) string {
	n := int(math.Round(sec))
	switch {
	case n >= 3600:
		h, m := n/3600, n%3600/60
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	case n >= 60:
		if n%60 == 0 {
			return fmt.Sprintf("%d min", n/60)
		}
		return fmt.Sprintf("%d min %d s", n/60, n%60)
	}
	return fmt.Sprintf("%d s", n)
}

// SideText names a side in plain English.
func SideText(side string) string {
	switch side {
	case triage.SideTarget:
		return "the system under test"
	case triage.SideScript:
		return "the test script"
	case triage.SideGenerator:
		return "the machine generating the load"
	case triage.SidePolicy:
		return "Load Meter's safety settings"
	}
	return "an unknown cause"
}

// What describes a cluster in plain words.
func What(c triage.Cluster) string {
	var w string
	switch c.Reason {
	case "server_error":
		w = fmt.Sprintf("the server answered with errors (HTTP %d)", c.Status)
	case "timeout", "timeout_generator_busy":
		w = "requests timed out waiting for an answer"
	case "connection_refused", "connect_failed":
		w = "the connection to the target was refused or could not be made"
	case "local_port_exhaustion":
		w = "the test machine ran out of network connections"
	case "connect_generator_busy":
		w = "connections failed while the test machine was overloaded"
	case "dns_failure":
		w = "the target's host name could not be found"
	case "connection_dropped":
		w = "connections were dropped or reset"
	case "extraction_missing":
		w = "the script could not find a value it needed from an earlier response"
	case "assertion_value", "assertion_after_extraction":
		w = "responses did not contain what the test expected"
	case "assertion_on_error_response":
		w = fmt.Sprintf("the test's checks failed because the server answered HTTP %d", c.Status)
	case "assertion_latency":
		w = "responses were slower than the limit set in the test"
	case "auth_rejected":
		w = fmt.Sprintf("the server rejected the login or token (HTTP %d)", c.Status)
	case "rate_limited":
		w = "the server limited the request rate (HTTP 429)"
	case "4xx_after_extraction", "4xx_after_extractor_failure":
		w = fmt.Sprintf("the server rejected requests (HTTP %d), after the script failed to pick up a value", c.Status)
	case "client_error":
		w = fmt.Sprintf("the server rejected the request as invalid (HTTP %d)", c.Status)
	case "policy_block":
		w = "requests were blocked by Load Meter's safety allowlist"
	case "unresolved_variable":
		w = "the request still contained an unfilled variable"
	case "invalid_request":
		w = "the request could not be built"
	default:
		w = "failures of an unrecognised kind"
		if c.ErrKind != "" {
			w = "failures of kind " + strings.ReplaceAll(c.ErrKind, "_", " ")
		}
	}
	if c.Request != "" && c.Request != "(various)" {
		w += " on " + c.Request
	}
	return w
}

func slaText(x results.SLAResult) string {
	scope := "all requests"
	if x.Scope != "" {
		scope = x.Scope
	}
	var what, unit string
	switch x.Metric {
	case "p50":
		what, unit = "median response time", " ms"
	case "p90", "p95", "p99":
		what, unit = x.Metric[1:]+"th percentile response time", " ms"
	case "avg":
		what, unit = "average response time", " ms"
	case "max":
		what, unit = "slowest response time", " ms"
	case "error_rate":
		what, unit = "failure rate", "%"
	case "rps":
		what, unit = "throughput", " per second"
	default:
		what = x.Metric
	}
	op := map[string]string{"<": "below", "<=": "at most", ">": "above", ">=": "at least"}[x.Op]
	if op == "" {
		op = x.Op
	}
	return fmt.Sprintf("%s for %s should be %s %s%s (measured %s%s)", what, scope, op, num(x.Threshold), unit, num(x.Actual), unit)
}

// num formats a number compactly: 200, 3.4, 1,234.
func num(v float64) string {
	if v == math.Trunc(v) {
		return commas(int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func commas(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
