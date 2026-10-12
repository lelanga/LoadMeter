// Package results turns a finished run into a persisted Summary, evaluates SLAs,
// compares runs and derives evidence-based findings.
package results

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
)

type SLAResult struct {
	model.SLA
	Actual float64 `json:"actual"`
	Passed bool    `json:"passed"`
	Note   string  `json:"note,omitempty"`
}

type Finding struct {
	Kind     string `json:"kind"`     // observation | hypothesis
	Severity string `json:"severity"` // info | warning | critical
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
	// Optional detail for timeline findings (change points, drift).
	Method    string    `json:"method,omitempty"`
	Interval  *Interval `json:"interval,omitempty"`
	Magnitude string    `json:"magnitude,omitempty"`
}

// Interval is a span of the run timeline in seconds since start.
type Interval struct {
	FromSec int `json:"fromSec"`
	ToSec   int `json:"toSec"`
}

type Environment struct {
	LoadMeter string `json:"loadmeterVersion"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	Hostname  string `json:"hostname"`
}

type Summary struct {
	RunID      string           `json:"runId"`
	Scenario   string           `json:"scenario"`
	Status     string           `json:"status"`
	StartedAt  time.Time        `json:"startedAt"`
	EndedAt    time.Time        `json:"endedAt"`
	DurationS  float64          `json:"durationSec"`
	Workload   model.Workload   `json:"workload"`
	Snapshot   metrics.Snapshot `json:"metrics"`
	SLAs       []SLAResult      `json:"slas,omitempty"`
	SLAPassed  bool             `json:"slaPassed"`
	Findings   []Finding        `json:"findings,omitempty"`
	Env        Environment      `json:"environment"`
	DataConfig []string         `json:"dataConfig,omitempty"`
	Baseline   *Comparison      `json:"baseline,omitempty"`
	Notes      []string         `json:"notes,omitempty"`
	// Agents is the per-node breakdown of a distributed run (absent for single-node runs).
	Agents []AgentStat `json:"agents,omitempty"`
}

// AgentStat is one node's share of a distributed run. The run-level metrics
// in Snapshot are merged from every node's histograms, not averaged.
type AgentStat struct {
	ID          string  `json:"id"`
	Address     string  `json:"address,omitempty"` // empty for the local (controller) node
	Status      string  `json:"status"`            // finished | failed | disconnected | stopped
	Error       string  `json:"error,omitempty"`
	VUs         int     `json:"vus"`
	Rate        float64 `json:"rate,omitempty"`
	Requests    uint64  `json:"requests"`
	Errors      uint64  `json:"errors"`
	ErrorRate   float64 `json:"errorRate"`
	P95Ms       float64 `json:"p95Ms"`
	ClockSkewMs float64 `json:"clockSkewMs"`
}

var Version = "0.1.0-dev"

func Env() Environment {
	h, _ := os.Hostname()
	return Environment{LoadMeter: Version, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Hostname: h}
}

func NewRunID(name string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, name)
	if len(safe) > 40 {
		safe = safe[:40]
	}
	var rb [2]byte
	_, _ = rand.Read(rb[:])
	// the random part keeps IDs unique for runs started within the same second
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(rb[:]) + "-" + strings.Trim(safe, "-")
}

// Build creates the summary from a finished collector.
func Build(runID string, sc *model.Scenario, col *metrics.Collector, start, end time.Time, status string) *Summary {
	snap := col.Snapshot(end, 0)
	s := &Summary{RunID: runID, Scenario: sc.Name, Status: status, StartedAt: start, EndedAt: end,
		DurationS: math.Round(end.Sub(start).Seconds()*100) / 100, Workload: sc.Workload, Snapshot: snap, Env: Env()}
	for _, d := range sc.DataSources {
		mode := d.Mode
		if mode == "" {
			mode = "sequential"
		}
		s.DataConfig = append(s.DataConfig, fmt.Sprintf("%s: mode=%s onExhaust=%s scope=%s rows=%s", d.Name, mode, or(d.OnExhaust, "recycle"), or(d.Scope, "iteration"), rowsDesc(d)))
	}
	s.SLAs, s.SLAPassed = EvaluateSLAs(sc.SLAs, col, snap)
	s.Findings = Diagnose(snap, s.SLAs)
	return s
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

func rowsDesc(d model.DataSource) string {
	if d.File != "" {
		return "file " + d.File
	}
	return strconv.Itoa(len(d.Rows)) + " inline"
}

func EvaluateSLAs(slas []model.SLA, col *metrics.Collector, snap metrics.Snapshot) ([]SLAResult, bool) {
	all := true
	var out []SLAResult
	for _, sla := range slas {
		r := SLAResult{SLA: sla}
		set, kind, found := findSet(snap, sla.Scope)
		if !found {
			r.Note = "no samples for scope " + sla.Scope
			r.Passed = false
			all = false
			out = append(out, r)
			continue
		}
		switch sla.Metric {
		case "p50", "p90", "p95", "p99":
			q, _ := strconv.ParseFloat(sla.Metric[1:], 64)
			v, _ := col.Quantile(kind, sla.Scope, q/100)
			r.Actual = math.Round(v*100) / 100
		case "avg":
			r.Actual = set.AvgMs
		case "max":
			r.Actual = set.MaxMs
		case "error_rate":
			r.Actual = set.ErrorRate
		case "rps":
			r.Actual = set.RPS
		default:
			r.Note = "unknown metric"
		}
		r.Passed = r.Note == "" && compare(r.Actual, sla.Op, sla.Threshold)
		all = all && r.Passed
		out = append(out, r)
	}
	return out, all
}

func findSet(s metrics.Snapshot, scope string) (metrics.MetricSet, string, bool) {
	if scope == "" {
		return s.Requests, metrics.KindRequest, s.Requests.Count > 0
	}
	for _, n := range s.ByTx {
		if n.Name == scope {
			return n.MetricSet, metrics.KindTransaction, true
		}
	}
	for _, n := range s.ByRequest {
		if n.Name == scope {
			return n.MetricSet, metrics.KindRequest, true
		}
	}
	return metrics.MetricSet{}, "", false
}

func compare(a float64, op string, b float64) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

// Diagnose produces findings; observations are measured facts, hypotheses are labelled.
func Diagnose(s metrics.Snapshot, slas []SLAResult) []Finding {
	var f []Finding
	if s.Requests.Count == 0 {
		return []Finding{{Kind: "observation", Severity: "critical", Title: "No requests completed", Evidence: "0 request samples were recorded."}}
	}
	// Slowest request by p95 (needs a minimum sample size).
	var slow *metrics.Named
	for i := range s.ByRequest {
		n := &s.ByRequest[i]
		if n.Count >= 5 && (slow == nil || n.P95Ms > slow.P95Ms) {
			slow = n
		}
	}
	if slow != nil {
		f = append(f, Finding{Kind: "observation", Severity: "info", Title: "Highest-latency request: " + slow.Name,
			Evidence: fmt.Sprintf("p95 %.1f ms, p99 %.1f ms over %d samples (overall p95 %.1f ms).", slow.P95Ms, slow.P99Ms, slow.Count, s.Requests.P95Ms)})
	}
	if s.Requests.ErrorRate > 0 {
		kinds := make([]string, 0, len(s.Requests.ErrorKind))
		for k, v := range s.Requests.ErrorKind {
			kinds = append(kinds, fmt.Sprintf("%s=%d", k, v))
		}
		sort.Strings(kinds)
		sev := "warning"
		if s.Requests.ErrorRate >= 5 {
			sev = "critical"
		}
		f = append(f, Finding{Kind: "observation", Severity: sev, Title: fmt.Sprintf("Error rate %.2f%%", s.Requests.ErrorRate),
			Evidence: fmt.Sprintf("%d of %d requests failed (%s).", s.Requests.Errors, s.Requests.Count, strings.Join(kinds, ", "))})
	}
	// Auth/correlation hint: 401/403 responses.
	var authFails uint64
	for code, n := range s.Requests.Status {
		if code == "401" || code == "403" {
			authFails += n
		}
	}
	if authFails > 0 {
		f = append(f, Finding{Kind: "hypothesis", Severity: "warning", Title: "Possible authentication or correlation failure",
			Evidence: fmt.Sprintf("%d responses were 401/403. Check for expired tokens, missing CSRF/session correlation or wrong test credentials.", authFails)})
	}
	if s.Requests.ErrorKind["blocked_by_policy"] > 0 {
		f = append(f, Finding{Kind: "observation", Severity: "critical", Title: "Requests blocked by the safety allowlist",
			Evidence: fmt.Sprintf("%d requests targeted hosts that are not allowlisted.", s.Requests.ErrorKind["blocked_by_policy"])})
	}
	// Level shifts and drift in the per-second timeline (change-point detection).
	f = append(f, timelineFindings(s.Timeline)...)
	if tl := s.Timeline; len(tl) >= 6 {
		// Load generator saturation.
		var hiCPU int
		for _, w := range tl {
			if w.CPU > float64(runtime.NumCPU())*100*0.85 {
				hiCPU++
			}
		}
		if hiCPU > 0 {
			f = append(f, Finding{Kind: "hypothesis", Severity: "warning", Title: "Load generator may be a bottleneck",
				Evidence: fmt.Sprintf("Generator CPU exceeded 85%% of available cores in %d of %d seconds; measured latency may include client-side queuing.", hiCPU, len(tl))})
		}
	}
	if s.Dropped > 0 {
		f = append(f, Finding{Kind: "observation", Severity: "warning", Title: "Iterations dropped (VU pool exhausted)",
			Evidence: fmt.Sprintf("%d scheduled iterations could not start because all %s VUs were busy; raise maxVUs or investigate slow responses.", s.Dropped, "allocated")})
	}
	for _, r := range slas {
		if !r.Passed {
			scope := r.Scope
			if scope == "" {
				scope = "all requests"
			}
			f = append(f, Finding{Kind: "observation", Severity: "critical", Title: fmt.Sprintf("SLA breached: %s %s %s %g", scope, r.Metric, r.Op, r.Threshold),
				Evidence: fmt.Sprintf("Actual %g. %s", r.Actual, r.Note)})
		}
	}
	return f
}

// ---------- Comparison ----------

type Delta struct {
	Name     string  `json:"name"`
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Change   float64 `json:"changePercent"`
	Worse    bool    `json:"worse"` // true exactly when Verdict is "regression"
	// Statistical verdict (absent in summaries written before it existed).
	Verdict       string      `json:"verdict,omitempty"` // regression | improvement | no-change | inconclusive
	Reason        string      `json:"reason,omitempty"`
	Informational bool        `json:"informational,omitempty"` // never counted as a regression (rps)
	Stats         *DeltaStats `json:"stats,omitempty"`
}

type Comparison struct {
	BaselineRun string  `json:"baselineRun"`
	Deltas      []Delta `json:"deltas"`
	Regressions int     `json:"regressions"`
	Note        string  `json:"note"`
	// Statistical comparison (optional fields; see CompareWith).
	Verdict      string   `json:"verdict,omitempty"` // overall: regression | improvement | no-change | inconclusive
	Improvements int      `json:"improvements,omitempty"`
	Inconclusive int      `json:"inconclusive,omitempty"`
	Alpha        float64  `json:"alpha,omitempty"`
	MinEffectPct float64  `json:"minEffectPct,omitempty"`
	MinEffectMs  float64  `json:"minEffectMs,omitempty"`
	MinErrorPP   float64  `json:"minErrorPP,omitempty"`
	Method       string   `json:"method,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// Compare runs CompareWith with the default options (alpha 0.05, minimum
// effect 5% and 2 ms, errors +1 percentage point, at least 10 windows).
func Compare(base, cur *Summary) *Comparison {
	return CompareWith(base, cur, DefaultCompareOptions())
}

// ---------- Persistence ----------

func Save(dir string, s *Summary) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
		return err
	}
	return WriteCSV(filepath.Join(dir, "summary.csv"), s)
}

func Load(dir string) (*Summary, error) {
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return &s, nil
}

// WriteCSV exports per-request and per-transaction statistics.
func WriteCSV(path string, s *Summary) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"kind", "name", "count", "errors", "errorRatePct", "rps", "minMs", "avgMs", "p50Ms", "p90Ms", "p95Ms", "p99Ms", "maxMs", "bytesIn", "bytesOut"})
	row := func(kind string, n string, m metrics.MetricSet) {
		ff := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
		_ = w.Write([]string{kind, n, strconv.FormatUint(m.Count, 10), strconv.FormatUint(m.Errors, 10), ff(m.ErrorRate), ff(m.RPS),
			ff(m.MinMs), ff(m.AvgMs), ff(m.P50Ms), ff(m.P90Ms), ff(m.P95Ms), ff(m.P99Ms), ff(m.MaxMs),
			strconv.FormatInt(m.BytesIn, 10), strconv.FormatInt(m.BytesOut, 10)})
	}
	row("all", "ALL REQUESTS", s.Snapshot.Requests)
	for _, n := range s.Snapshot.ByTx {
		row("transaction", n.Name, n.MetricSet)
	}
	for _, n := range s.Snapshot.ByRequest {
		row("request", n.Name, n.MetricSet)
	}
	w.Flush()
	return w.Error()
}
