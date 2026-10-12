package capacity

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dir is where a capacity result is stored: <workspace>/capacity/<id>/.
func Dir(workspace, id string) string { return filepath.Join(workspace, "capacity", id) }

// Save writes capacity.json and report.html into dir.
func Save(dir string, r *Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "capacity.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "report.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return RenderHTML(f, r)
}

// Load reads capacity.json from dir.
func Load(dir string) (*Result, error) {
	b, err := os.ReadFile(filepath.Join(dir, "capacity.json"))
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return &r, nil
}

// List returns stored capacity result IDs, newest first.
func List(workspace string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(workspace, "capacity"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(workspace, "capacity", e.Name(), "capacity.json")); e.IsDir() && err == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// StepHeader and StepRow format the per-step table for terminals.
func StepHeader() string {
	return fmt.Sprintf("%-3s %-6s %9s %9s %9s %7s %8s %8s %8s %7s %6s  %s",
		"#", "phase", "offered", "achieved", "req/s", "err%", "p50ms", "p95ms", "p99ms", "drop", "cpu%", "verdict")
}

func StepRow(s Step) string {
	v := s.Verdict
	if s.Reason != "" {
		v += ": " + s.Reason
	}
	return fmt.Sprintf("%-3d %-6s %9.2f %9.2f %9.2f %7.2f %8.1f %8.1f %8.1f %7d %6.1f  %s",
		s.Index, s.Phase, s.OfferedRate, s.AchievedRate, s.RequestRate, s.ErrorPct, s.P50Ms, s.P95Ms, s.P99Ms, s.Dropped, s.CPUSharePct, v)
}

// WriteText prints a human-readable result.
func WriteText(w io.Writer, r *Result) {
	fmt.Fprintf(w, "\nCapacity of %q (rates are scenario iterations/s)\n", r.Scenario)
	fmt.Fprintln(w, StepHeader())
	for _, s := range r.Steps {
		fmt.Fprintln(w, StepRow(s))
	}
	fmt.Fprintln(w)
	crit := make([]string, 0, len(r.Criteria))
	for _, c := range r.Criteria {
		scope := c.Scope
		if scope == "" {
			scope = "all"
		}
		crit = append(crit, fmt.Sprintf("%s %s %s %g", scope, c.Metric, c.Op, c.Threshold))
	}
	fmt.Fprintf(w, "criteria:                %s\n", strings.Join(crit, "; "))
	if r.MaxSustainableRate > 0 {
		fmt.Fprintf(w, "max sustainable rate:    %.2f it/s (%.2f requests/s) [%s]\n", r.MaxSustainableRate, r.MaxSustainableRPS, r.Bound)
	} else {
		fmt.Fprintf(w, "max sustainable rate:    none (no step met the criteria)\n")
	}
	if r.FirstFailRate > 0 {
		fmt.Fprintf(w, "first failing rate:      %.2f it/s\n", r.FirstFailRate)
	}
	if r.FirstBreach != "" {
		fmt.Fprintf(w, "first breached:          %s\n", r.FirstBreach)
	}
	fmt.Fprintf(w, "stopped because:         %s", r.StopReason)
	if r.StopDetail != "" {
		fmt.Fprintf(w, " (%s)", r.StopDetail)
	}
	fmt.Fprintln(w)
	if r.Knee != nil {
		fmt.Fprintf(w, "latency knee:            %.2f it/s offered, %.2f req/s, p95 %.1f ms (score %.2f)\n", r.Knee.OfferedRate, r.Knee.Throughput, r.Knee.P95Ms, r.Knee.Score)
	}
	if r.Plateau != nil {
		fmt.Fprintf(w, "throughput plateau:      scaling stops after %.2f it/s offered (max achieved %.2f it/s)\n", r.Plateau.LastScalingRate, r.Plateau.MaxThroughput)
	}
	fmt.Fprintf(w, "knee vs plateau:         %s\n", r.KneeAgreement)
	if r.GeneratorLimited {
		fmt.Fprintln(w, "GENERATOR-LIMITED:       yes (the result is a lower bound of the target's capacity)")
	}
	fmt.Fprintf(w, "confidence:              %s\n", r.Confidence)
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	fmt.Fprintln(w, "limitations:")
	for _, l := range r.Limitations {
		fmt.Fprintf(w, "  - %s\n", l)
	}
	if e := r.Estimate; e != nil {
		fmt.Fprintf(w, "\n%s\n", e.Label)
		fmt.Fprintf(w, "  per-instance sustainable: %.2f it/s; usable with %g%% headroom: %.2f it/s\n", e.PerInstanceRate, e.HeadroomPct, e.UsablePerInst)
		if e.Instances > 0 {
			fmt.Fprintf(w, "  instances for %.2f it/s (%.2f req/s): ceil(%.2f / %.2f) = %d\n", e.TargetRate, e.TargetRPS, e.TargetRate, e.UsablePerInst, e.Instances)
		}
		if e.InstancesNote != "" {
			fmt.Fprintf(w, "  instances: %s\n", e.InstancesNote)
		}
		if e.GenReqPerCore > 0 {
			fmt.Fprintf(w, "  generator: about %.0f requests/s per fully used core on this machine (%d cores)\n", e.GenReqPerCore, e.GenCPUs)
			if e.GenCoresForTarget > 0 {
				fmt.Fprintf(w, "  generator for the target: %.2f cores incl. headroom = %d machine(s) like this one\n", e.GenCoresForTarget, e.GenMachinesForTarget)
			}
		} else if e.GenNote != "" {
			fmt.Fprintf(w, "  generator: %s\n", e.GenNote)
		}
		fmt.Fprintln(w, "  assumptions:")
		for _, a := range e.Assumptions {
			fmt.Fprintf(w, "    - %s\n", a)
		}
	}
}

var htmlTpl = template.Must(template.New("cap").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Capacity: {{.Scenario}}</title>
<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1d1d1f;--mut:#666;--line:#ddd;--bad:#b3261e;--ok:#1e7b34}
@media (prefers-color-scheme:dark){:root{--bg:#161618;--fg:#eee;--mut:#aaa;--line:#333;--bad:#ff8a80;--ok:#7bd88f}}
body{background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,sans-serif;margin:0 auto;max-width:1100px;padding:16px}
table{border-collapse:collapse;width:100%;font-variant-numeric:tabular-nums}th,td{border-bottom:1px solid var(--line);padding:4px 6px;text-align:right}
th:last-child,td:last-child,th:nth-child(2),td:nth-child(2){text-align:left}.wrap{overflow-x:auto}
.pass{color:var(--ok)}.breach,.error-storm,.generator-limited{color:var(--bad)}.mut{color:var(--mut)}
</style></head><body>
<h1>Capacity: {{.Scenario}}</h1>
<p class="mut">{{.ID}} · {{.StartedAt.Format "2006-01-02 15:04:05"}} · rates are scenario iterations per second</p>
<p><b>Max sustainable rate:</b> {{printf "%.2f" .MaxSustainableRate}} it/s ({{printf "%.2f" .MaxSustainableRPS}} requests/s) [{{.Bound}}] · <b>confidence:</b> {{.Confidence}}</p>
{{if .FirstBreach}}<p><b>First breached:</b> {{.FirstBreach}}</p>{{end}}
<p><b>Stopped:</b> {{.StopReason}} {{.StopDetail}}</p>
{{if .Knee}}<p><b>Latency knee:</b> {{printf "%.2f" .Knee.OfferedRate}} it/s (p95 {{printf "%.1f" .Knee.P95Ms}} ms)</p>{{end}}
{{if .Plateau}}<p><b>Throughput plateau:</b> scaling stops after {{printf "%.2f" .Plateau.LastScalingRate}} it/s</p>{{end}}
<p>{{.KneeAgreement}}</p>
<div class="wrap"><table><tr><th>#</th><th>phase</th><th>offered</th><th>achieved</th><th>req/s</th><th>err %</th><th>p50</th><th>p95</th><th>p99</th><th>dropped</th><th>CPU %</th><th>verdict</th></tr>
{{range .Steps}}<tr><td>{{.Index}}</td><td>{{.Phase}}</td><td>{{printf "%.2f" .OfferedRate}}</td><td>{{printf "%.2f" .AchievedRate}}</td><td>{{printf "%.2f" .RequestRate}}</td><td>{{printf "%.2f" .ErrorPct}}</td><td>{{printf "%.1f" .P50Ms}}</td><td>{{printf "%.1f" .P95Ms}}</td><td>{{printf "%.1f" .P99Ms}}</td><td>{{.Dropped}}</td><td>{{printf "%.1f" .CPUSharePct}}</td><td class="{{.Verdict}}">{{.Verdict}} {{.Reason}}</td></tr>
{{end}}</table></div>
{{with .Estimate}}<h2>{{.Label}}</h2>
<p>Per instance {{printf "%.2f" .PerInstanceRate}} it/s; usable with {{.HeadroomPct}}% headroom {{printf "%.2f" .UsablePerInst}} it/s.{{if .Instances}} Instances for {{.TargetRate}} it/s: <b>{{.Instances}}</b>.{{end}} {{.InstancesNote}}</p>
{{if .GenReqPerCore}}<p>Generator: about {{printf "%.0f" .GenReqPerCore}} requests/s per core on this machine.</p>{{end}}
<ul>{{range .Assumptions}}<li>{{.}}</li>{{end}}</ul>{{end}}
<h2>Limitations</h2><ul>{{range .Limitations}}<li>{{.}}</li>{{end}}</ul>
{{if .Notes}}<h2>Notes</h2><ul>{{range .Notes}}<li>{{.}}</li>{{end}}</ul>{{end}}
</body></html>
`))

// RenderHTML writes a self-contained HTML report.
func RenderHTML(w io.Writer, r *Result) error { return htmlTpl.Execute(w, r) }
