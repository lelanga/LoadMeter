// Package report renders a self-contained HTML report (no external assets,
// works offline) from a run summary.
package report

import (
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/triage"
)

type series struct {
	Name   string
	Color  string
	Values []float64
}

// svgChart draws a simple multi-series line chart with axes.
func svgChart(title, unit string, ss []series) template.HTML {
	const w, h, pl, pb, pt = 760.0, 200.0, 48.0, 22.0, 18.0
	n := 0
	maxV := 0.0
	for _, s := range ss {
		n = max(n, len(s.Values))
		for _, v := range s.Values {
			maxV = math.Max(maxV, v)
		}
	}
	if n < 2 {
		return template.HTML(`<p class="muted">Not enough data points for a chart.</p>`)
	}
	if maxV == 0 {
		maxV = 1
	}
	maxV = niceCeil(maxV)
	var b strings.Builder
	fmt.Fprintf(&b, `<figure><figcaption>%s</figcaption><svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, template.HTMLEscapeString(title), w, h, template.HTMLEscapeString(title))
	for i := 0; i <= 4; i++ {
		y := pt + (h-pt-pb)*float64(i)/4
		v := maxV * float64(4-i) / 4
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="grid"/><text x="%.0f" y="%.1f" class="axis" text-anchor="end">%s</text>`, pl, w, y, y, pl-4, y+4, short(v))
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="axis">0s</text><text x="%.0f" y="%.0f" class="axis" text-anchor="end">%ds</text>`, pl, h-4, w, h-4, n-1)
	for _, s := range ss {
		var pts []string
		for i, v := range s.Values {
			x := pl + (w-pl)*float64(i)/float64(n-1)
			y := pt + (h-pt-pb)*(1-v/maxV)
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="1.8" points="%s"/>`, s.Color, strings.Join(pts, " "))
	}
	b.WriteString(`</svg><div class="legend">`)
	for _, s := range ss {
		fmt.Fprintf(&b, `<span><i style="background:%s"></i>%s</span>`, s.Color, template.HTMLEscapeString(s.Name))
	}
	fmt.Fprintf(&b, `<span class="muted">(%s)</span></div></figure>`, template.HTMLEscapeString(unit))
	return template.HTML(b.String())
}

func niceCeil(v float64) float64 {
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*p {
			return m * p
		}
	}
	return 10 * p
}

func short(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

type view struct {
	*results.Summary
	Charts          []template.HTML
	Triage          template.HTML
	Observations    []results.Finding
	Hypotheses      []results.Finding
	Recommendations []string
	Verdict         string
	VerdictClass    string
	Objective       string
}

func recommendations(s *results.Summary) []string {
	var r []string
	for _, f := range s.Findings {
		switch {
		case strings.HasPrefix(f.Title, "Possible authentication"):
			r = append(r, "Validate correlation rules with `loadmeter correlate --validate` and confirm test credentials before re-running.")
		case strings.HasPrefix(f.Title, "Load generator"):
			r = append(r, "Re-run with fewer VUs per generator or on a larger machine to separate client-side from server-side latency.")
		case strings.HasPrefix(f.Title, "Iterations dropped"):
			r = append(r, "Increase maxVUs for the arrival-rate executor, or investigate why responses are slow enough to exhaust the pool.")
		case strings.HasPrefix(f.Title, "Errors increased"):
			r = append(r, "Run a step-load test around the VU level where errors started to find the capacity limit, and correlate with server-side monitoring.")
		case strings.HasPrefix(f.Title, "SLA breached"):
			r = append(r, "Investigate the breached transaction with server-side traces; compare against the last passing baseline.")
		case strings.HasPrefix(f.Title, "Requests blocked"):
			r = append(r, "Add the target host to the allowlist only if you are authorized to load test it.")
		}
	}
	if len(r) == 0 {
		r = append(r, "No issues detected by automated checks. Keep this run as a baseline for regression comparison.")
	}
	return dedupe(r)
}

func dedupe(a []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Render writes the HTML report.
func Render(w io.Writer, s *results.Summary, description string) error {
	v := view{Summary: s, Objective: description}
	// Failure triage: the report file sits next to raw.csv in the run folder.
	if f, ok := w.(*os.File); ok && s.Snapshot.Requests.Errors > 0 {
		if res, err := triage.AnalyzeRun(filepath.Dir(f.Name()), s); err == nil {
			v.Triage = res.HTMLSection(10)
		}
	}
	tl := s.Snapshot.Timeline
	col := func(f func(i int) float64) []float64 {
		out := make([]float64, len(tl))
		for i := range tl {
			out[i] = f(i)
		}
		return out
	}
	v.Charts = []template.HTML{
		svgChart("Response time percentiles per second", "ms", []series{
			{"p50", "#2f6fdf", col(func(i int) float64 { return tl[i].P50Ms })},
			{"p95", "#e08a1e", col(func(i int) float64 { return tl[i].P95Ms })},
			{"p99", "#c23b3b", col(func(i int) float64 { return tl[i].P99Ms })}}),
		svgChart("Throughput and active VUs", "requests/s, VUs", []series{
			{"requests/s", "#2f6fdf", col(func(i int) float64 { return tl[i].RPS })},
			{"VUs", "#7a52c7", col(func(i int) float64 { return float64(tl[i].VUs) })}}),
		svgChart("Errors per second", "count", []series{{"errors", "#c23b3b", col(func(i int) float64 { return float64(tl[i].Errors) })}}),
		svgChart("Load generator resources", "CPU %, MB", []series{
			{"CPU %", "#2a9d8f", col(func(i int) float64 { return tl[i].CPU })},
			{"RSS MB", "#8d6e63", col(func(i int) float64 { return tl[i].MemMB })}}),
	}
	for _, f := range s.Findings {
		if f.Kind == "hypothesis" {
			v.Hypotheses = append(v.Hypotheses, f)
		} else {
			v.Observations = append(v.Observations, f)
		}
	}
	v.Recommendations = recommendations(s)
	switch {
	case s.Snapshot.Requests.Count == 0:
		v.Verdict, v.VerdictClass = "No data", "bad"
	case len(s.SLAs) > 0 && !s.SLAPassed:
		v.Verdict, v.VerdictClass = "SLA failed", "bad"
	case len(s.SLAs) > 0:
		v.Verdict, v.VerdictClass = "SLA passed", "good"
	case s.Snapshot.Requests.ErrorRate >= 5:
		v.Verdict, v.VerdictClass = "High error rate", "bad"
	default:
		v.Verdict, v.VerdictClass = "Completed (no SLAs defined)", "neutral"
	}
	return tpl.Execute(w, v)
}

var tpl = template.Must(template.New("r").Funcs(template.FuncMap{
	"pct": func(v float64) string { return fmt.Sprintf("%.2f%%", v) },
	"f1":  func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"kb":  func(v int64) string { return fmt.Sprintf("%.1f", float64(v)/1024) },
}).Parse(htmlTpl))

const htmlTpl = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Load Meter report: {{.Scenario}}</title>
<style>
:root{--bg:#fff;--fg:#1d2330;--muted:#667085;--line:#e4e7ec;--card:#f7f8fa;--good:#1a7f37;--bad:#c23b3b;--accent:#2f6fdf}
@media (prefers-color-scheme:dark){:root{--bg:#12151c;--fg:#e6e8ee;--muted:#98a2b3;--line:#2a303c;--card:#1a1f29}}
body{background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;margin:0;padding:24px;max-width:1100px;margin:auto}
h1{font-size:22px;margin:0 0 4px}h2{font-size:17px;margin:28px 0 8px;border-bottom:1px solid var(--line);padding-bottom:4px}
.muted{color:var(--muted)}table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:right;padding:5px 8px;border-bottom:1px solid var(--line)}th:first-child,td:first-child{text-align:left}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:10px}.kpi{background:var(--card);border-radius:8px;padding:10px 12px}.kpi b{display:block;font-size:20px}
.badge{display:inline-block;padding:2px 10px;border-radius:12px;font-weight:600}.good{background:#dcfce7;color:var(--good)}.bad{background:#fde2e2;color:var(--bad)}.neutral{background:var(--card)}
svg{width:100%;height:auto}.grid{stroke:var(--line)}.axis{fill:var(--muted);font-size:10px}figure{margin:12px 0}figcaption{font-weight:600;margin-bottom:4px}
.legend span{margin-right:14px;font-size:12px}.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:4px}
.finding{border-left:3px solid var(--line);padding:4px 10px;margin:6px 0}.critical{border-color:var(--bad)}.warning{border-color:#e08a1e}
code{background:var(--card);padding:1px 4px;border-radius:4px}
</style></head><body>
<h1>Performance test report — {{.Scenario}}</h1>
<div class="muted">Run {{.RunID}} · {{.StartedAt.Format "2006-01-02 15:04:05 MST"}} · {{f1 .DurationS}} s · status {{.Status}}</div>

<h2>Executive summary</h2>
<p><span class="badge {{.VerdictClass}}">{{.Verdict}}</span></p>
<div class="kpis">
<div class="kpi"><span class="muted">Requests</span><b>{{.Snapshot.Requests.Count}}</b></div>
<div class="kpi"><span class="muted">Throughput</span><b>{{f1 .Snapshot.Requests.RPS}}/s</b></div>
<div class="kpi"><span class="muted">p95</span><b>{{f1 .Snapshot.Requests.P95Ms}} ms</b></div>
<div class="kpi"><span class="muted">p99</span><b>{{f1 .Snapshot.Requests.P99Ms}} ms</b></div>
<div class="kpi"><span class="muted">Error rate</span><b>{{pct .Snapshot.Requests.ErrorRate}}</b></div>
<div class="kpi"><span class="muted">Iterations</span><b>{{.Snapshot.Iterations}}</b></div>
</div>
{{if .Observations}}<p>Key observations: {{range $i, $f := .Observations}}{{if $i}}; {{end}}{{$f.Title}}{{end}}.</p>{{end}}

<h2>Test objectives</h2><p>{{if .Objective}}{{.Objective}}{{else}}<span class="muted">No description was provided in the scenario.</span>{{end}}</p>

<h2>Environment and configuration</h2>
<table><tr><th>Item</th><th>Value</th></tr>
<tr><td>Load Meter version</td><td>{{.Env.LoadMeter}} ({{.Env.GoVersion}})</td></tr>
<tr><td>Load generator</td><td>{{.Env.Hostname}} · {{.Env.OS}}/{{.Env.Arch}} · {{.Env.CPUs}} CPUs</td></tr></table>

<h2>Workload profile</h2>
<table><tr><th>Setting</th><th>Value</th></tr>
<tr><td>Executor</td><td>{{.Workload.Executor}}</td></tr>
{{if .Workload.VUs}}<tr><td>Virtual users</td><td>{{.Workload.VUs}}</td></tr>{{end}}
{{if .Workload.Rate}}<tr><td>Arrival rate</td><td>{{.Workload.Rate}} iterations/s (max {{.Workload.MaxVUs}} VUs)</td></tr>{{end}}
{{if .Workload.Duration}}<tr><td>Duration</td><td>{{.Workload.Duration.D}}</td></tr>{{end}}
{{range .Workload.Stages}}<tr><td>Stage</td><td>{{.Duration.D}} → {{.Target}}</td></tr>{{end}}
{{if .Workload.Pacing}}<tr><td>Pacing</td><td>{{.Workload.Pacing.D}}</td></tr>{{end}}
<tr><td>Dropped iterations</td><td>{{.Snapshot.Dropped}}</td></tr></table>

<h2>Test data</h2>{{if .DataConfig}}<ul>{{range .DataConfig}}<li>{{.}}</li>{{end}}</ul>{{else}}<p class="muted">No parameter data sources.</p>{{end}}

<h2>Results</h2>
{{range .Charts}}{{.}}{{end}}
<h3>Transactions</h3>
{{if .Snapshot.ByTx}}<table><tr><th>Transaction</th><th>Count</th><th>Errors</th><th>Avg</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th><th>Max</th></tr>
{{range .Snapshot.ByTx}}<tr><td>{{.Name}}</td><td>{{.Count}}</td><td>{{.Errors}}</td><td>{{f1 .AvgMs}}</td><td>{{f1 .P50Ms}}</td><td>{{f1 .P90Ms}}</td><td>{{f1 .P95Ms}}</td><td>{{f1 .P99Ms}}</td><td>{{f1 .MaxMs}}</td></tr>{{end}}</table>
{{else}}<p class="muted">No transactions defined.</p>{{end}}

<h2>SLA assessment</h2>
{{if .SLAs}}<table><tr><th>Scope</th><th>Metric</th><th>Threshold</th><th>Actual</th><th>Result</th></tr>
{{range .SLAs}}<tr><td>{{if .Scope}}{{.Scope}}{{else}}all requests{{end}}</td><td>{{.Metric}}</td><td>{{.Op}} {{.Threshold}}</td><td>{{.Actual}}</td><td>{{if .Passed}}<span class="badge good">pass</span>{{else}}<span class="badge bad">fail</span> {{.Note}}{{end}}</td></tr>{{end}}</table>
{{else}}<p class="muted">No SLAs were configured.</p>{{end}}

<h2>Error analysis</h2>
<table><tr><th>HTTP status</th><th>Count</th></tr>{{range $k, $v := .Snapshot.Requests.Status}}<tr><td>{{$k}}</td><td>{{$v}}</td></tr>{{end}}</table>
{{if .Snapshot.Requests.ErrorKind}}<table><tr><th>Error category</th><th>Count</th></tr>{{range $k, $v := .Snapshot.Requests.ErrorKind}}<tr><td>{{$k}}</td><td>{{$v}}</td></tr>{{end}}</table>{{end}}

{{if .Baseline}}<h2>Comparison with baseline {{.Baseline.BaselineRun}}</h2>
<p class="muted">{{.Baseline.Note}}</p>
{{if .Baseline.Verdict}}<h3>Statistical comparison: {{.Baseline.Verdict}}</h3><p class="muted">{{.Baseline.Method}}.{{range .Baseline.Warnings}} Warning: {{.}}{{end}}</p>{{end}}
<table><tr><th>Scope</th><th>Metric</th><th>Baseline</th><th>Current</th><th>Change</th><th>Verdict</th><th>Reason</th></tr>
{{range .Baseline.Deltas}}<tr><td>{{.Name}}</td><td>{{.Metric}}</td><td>{{.Baseline}}</td><td>{{.Current}}</td><td>{{.Change}}%</td><td>{{if .Worse}}<span class="badge bad">regression</span>{{else}}{{.Verdict}}{{end}}</td><td class="muted">{{.Reason}}</td></tr>{{end}}</table>{{end}}

{{if .Agents}}<h2>Load agents</h2>
<p class="muted">Distributed run: overall metrics above are merged from every node's full histograms.</p>
<table><tr><th>Node</th><th>Address</th><th>Status</th><th>VUs</th><th>Requests</th><th>Errors</th><th>p95 ms</th><th>Clock skew ms</th></tr>
{{range .Agents}}<tr><td>{{.ID}}</td><td>{{if .Address}}{{.Address}}{{else}}local{{end}}</td><td>{{.Status}}{{if .Error}} — {{.Error}}{{end}}</td><td>{{.VUs}}</td><td>{{.Requests}}</td><td>{{.Errors}}</td><td>{{f1 .P95Ms}}</td><td>{{f1 .ClockSkewMs}}</td></tr>{{end}}</table>{{end}}

{{.Triage}}
<h2>Findings</h2>
<h3>Observed facts</h3>{{range .Observations}}<div class="finding {{.Severity}}"><b>{{.Title}}</b><br>{{.Evidence}}</div>{{else}}<p class="muted">None.</p>{{end}}
<h3>Hypotheses (need confirmation)</h3>{{range .Hypotheses}}<div class="finding {{.Severity}}"><b>{{.Title}}</b><br>{{.Evidence}}</div>{{else}}<p class="muted">None.</p>{{end}}

<h2>Recommendations</h2><ul>{{range .Recommendations}}<li>{{.}}</li>{{end}}</ul>

<h2>Limitations and caveats</h2><ul>
<li>Latencies are measured at the load generator and include network time and any client-side queuing.</li>
<li>Percentiles come from log-linear histograms (relative error below 1%) of every sample, never from averaged per-second percentiles. Per-second chart percentiles describe that second only.</li>
<li>Closed-model executors (VU-based) can under-report latency during server slowdowns (coordinated omission). Use an arrival-rate executor for latency-sensitive SLAs.</li>
<li>Generator CPU is process CPU time over wall time and may exceed 100% on multi-core hosts.</li>
{{range .Notes}}<li>{{.}}</li>{{end}}</ul>

<h2>Technical appendix: requests</h2>
<table><tr><th>Request</th><th>Count</th><th>Err</th><th>RPS</th><th>Min</th><th>Avg</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th><th>Max</th><th>KB in</th></tr>
{{range .Snapshot.ByRequest}}<tr><td>{{.Name}}</td><td>{{.Count}}</td><td>{{.Errors}}</td><td>{{f1 .RPS}}</td><td>{{f1 .MinMs}}</td><td>{{f1 .AvgMs}}</td><td>{{f1 .P50Ms}}</td><td>{{f1 .P90Ms}}</td><td>{{f1 .P95Ms}}</td><td>{{f1 .P99Ms}}</td><td>{{f1 .MaxMs}}</td><td>{{kb .BytesIn}}</td></tr>{{end}}</table>
<p class="muted">Generated by Load Meter {{.Env.LoadMeter}}. Raw per-sample data: <code>raw.csv</code> (JTL-compatible columns) in the run folder.</p>
</body></html>`
