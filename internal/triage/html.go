package triage

import (
	"bytes"
	"html/template"
	"strings"
)

var sectionTmpl = template.Must(template.New("triage").Funcs(template.FuncMap{
	"spark": sparkline,
	"cls": func(side string) string {
		switch side {
		case SideScript, SidePolicy:
			return "warn"
		case SideTarget, SideGenerator:
			return "bad"
		}
		return ""
	},
}).Parse(`<h2>Failure triage</h2>
<p class="muted">Failed requests grouped by similar message, error kind, status and request, with the side most likely at fault. Rule-based and reproducible; a hint to check first, not a proof.</p>
{{if not .Clusters}}<p class="muted">No failed requests.</p>{{else}}
{{if .Note}}<p class="muted">{{.Note}}</p>{{end}}
<table><tr><th>#</th><th>Priority</th><th>Side</th><th>Failures</th><th>First seen</th><th>Request / what happened</th></tr>
{{range .Clusters}}<tr><td>{{.Rank}}</td><td>{{.Priority}}</td><td><span class="badge {{cls .Side}}">{{.Side}}</span><br><span class="muted">{{.Confidence}}</span></td>
<td>{{.Count}} ({{.Percent}}%)</td><td>{{if $.Coarse}}n/a{{else}}{{.FirstSeenSec}} s{{if .BeganAtChangePoint}}<br><span class="muted">at a change point</span>{{end}}{{end}}</td>
<td><b>{{.Request}}</b>{{if .Transaction}} <span class="muted">in {{.Transaction}}</span>{{end}}{{if .Status}} <span class="muted">HTTP {{.Status}}</span>{{end}} <span class="muted">{{.ErrKind}}</span><br>
<code>{{.Template}}</code><br>{{.Explanation}}<br><i>Next check:</i> {{.NextCheck}}{{if .Spark}}<br><span class="muted" title="failures over the run, left to right">{{spark .Spark}}</span>{{end}}</td></tr>{{end}}</table>
{{if .ClustersHidden}}<p class="muted">{{.ClustersHidden}} lower-priority clusters not shown.</p>{{end}}
{{range .Signals}}<div class="finding"><b>{{.Side}}</b>: {{.Text}}</div>{{end}}{{end}}`))

// HTMLSection renders the top n clusters as an HTML fragment for the report.
func (r *Result) HTMLSection(n int) template.HTML {
	var b bytes.Buffer
	if err := sectionTmpl.Execute(&b, r.Top(n)); err != nil {
		return ""
	}
	return template.HTML(b.String())
}

const sparkChars = "▁▂▃▄▅▆▇█"

func sparkline(v []int) string {
	mx := 0
	for _, x := range v {
		mx = max(mx, x)
	}
	var b strings.Builder
	runes := []rune(sparkChars)
	for _, x := range v {
		if mx == 0 || x == 0 {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(runes[min(len(runes)-1, x*len(runes)/(mx+1))])
	}
	return b.String()
}

// Sparkline renders a cluster's timeline as a short text bar.
func Sparkline(v []int) string { return sparkline(v) }
