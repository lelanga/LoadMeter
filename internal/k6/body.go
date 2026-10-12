package k6

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

var jpSubset = regexp.MustCompile(`^\$((\.[A-Za-z_$][\w$-]*)|(\[\d+\])|(\['[^']*'\])|(\["[^"]*"\]))*$`)

// body writes the default/teardown functions and returns them.
func (e *exporter) body() string {
	s := e.s
	e.b.Reset()
	e.line("let V = null; // per-VU variables (persist across iterations, like Load Meter)")
	e.line("")
	e.line("export default function () {")
	e.ind++
	e.line("const _t0 = Date.now();")
	e.line("let _first = false;")
	e.line("if (V === null) { V = Object.assign({}, VARS); _first = true; }")
	for _, ds := range s.DataSources {
		scope := ds.Scope
		mode := ds.Mode
		if mode == "" {
			mode = "sequential"
		}
		e.line("if (_feed(%s, DS_%s, %s, %s, %s, V) === 'skip') return;", js(ds.Name), ident(ds.Name), js(mode), js(scope), js(ds.OnExhaust))
	}
	if len(s.Setup) > 0 {
		e.line("if (_first) {")
		e.ind++
		e.line("// setup (runs once per VU, as in Load Meter)")
		e.steps(s.Setup, "setup")
		e.ind--
		e.line("}")
		e.rep.Add("setup", "", "setup", model.Supported, "executed lazily by each VU on its first iteration (k6 setup() runs once globally, which differs)")
	}
	e.steps(s.Steps, "steps")
	if p := s.Workload.Pacing; p > 0 {
		e.line("const _rest = %d - (Date.now() - _t0);", ms(p))
		e.line("if (_rest > 0) sleep(_rest / 1000);")
		e.rep.Add("pacing", p.D().String(), "workload.pacing", model.Supported, "implemented as a sleep at the end of each iteration")
	}
	e.ind--
	e.line("}")
	if len(s.Teardown) > 0 {
		e.line("")
		e.line("export function teardown() {")
		e.ind++
		e.line("V = Object.assign({}, VARS);")
		e.steps(s.Teardown, "teardown")
		e.ind--
		e.line("}")
		e.rep.Add("teardown", "", "teardown", model.Partial, "k6 teardown() runs once per test, not per VU, and does not see variables extracted during the run")
	}
	switch s.Workload.OnError {
	case "stop-vu":
		e.rep.Add("workload.onError", "stop-vu", "workload.onError", model.Partial, "mapped to abort-iteration; k6 cannot stop a single VU")
	case "abort-iteration":
		e.rep.Add("workload.onError", "abort-iteration", "workload.onError", model.Supported, "implemented with fail()")
	}
	if s.Workload.ThinkTimeScale != nil {
		e.rep.Add("workload.thinkTimeScale", num(*s.Workload.ThinkTimeScale), "workload.thinkTimeScale", model.Supported, "applied to every think step")
	}
	return e.b.String()
}

func (e *exporter) steps(steps []model.Step, loc string) {
	for i := range steps {
		st := &steps[i]
		l := fmt.Sprintf("%s[%d]", loc, i)
		if st.Disabled {
			e.rep.Add(st.Type, st.Name, l, model.Ignored, "disabled step skipped")
			continue
		}
		switch st.Type {
		case model.StepRequest:
			e.request(st, l)
		case model.StepTransaction, model.StepGroup:
			if st.Select != "" {
				e.rep.Add("group", st.Name, l, model.Partial, "weighted selection is not exported: every inner step runs in order; pick one weighted request manually")
			}
			e.line("group(%s, function () {", e.lit(st.Name))
			e.ind++
			e.steps(st.Steps, l+".steps")
			e.ind--
			e.line("});")
			e.flush(st.Type, st.Name, l, "")
		case model.StepThink:
			t := st.Think
			if t.Distribution != nil && t.Distribution.Type != model.DistUniform {
				e.rep.Add("think", st.Name, l, model.Partial, "think-time distribution is not exported; min/max are used")
			}
			e.line("_think(%d, %d, %s);", ms(t.Min), maxI(ms(t.Max), ms(t.Min)), num(e.scale))
			e.rep.Add("think", st.Name, l, model.Supported, "")
		case model.StepLoop:
			e.loopN++
			v := fmt.Sprintf("_i%d", e.loopN)
			e.line("for (let %s = 0; %s < %d; %s++) {", v, v, st.Count, v)
			e.ind++
			e.steps(st.Steps, l+".steps")
			e.ind--
			e.line("}")
			e.rep.Add("loop", st.Name, l, model.Supported, "")
		case model.StepIf:
			e.line("if (_cond(%s, V)) {", e.lit(st.Cond))
			e.ind++
			e.steps(st.Steps, l+".steps")
			e.ind--
			e.line("}")
			e.flush("if", st.Name, l, "")
		case model.StepSetVar:
			for _, kv := range st.Set {
				e.line("V[%s] = _expand(%s, V);", js(kv.Name), e.lit(kv.Value))
			}
			e.flush("set", st.Name, l, "")
		}
	}
}

func maxI(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (e *exporter) pairs(kv []model.KV) string {
	var parts []string
	for _, p := range kv {
		parts = append(parts, "["+e.lit(p.Name)+", "+e.lit(p.Value)+"]")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (e *exporter) request(st *model.Step, loc string) {
	rq := st.Request
	name := e.reqName(st)
	var sp []string
	sp = append(sp, "name: "+js(name), "method: "+js(strings.ToUpper(rq.Method)), "url: "+e.lit(rq.URL))
	if name != st.Name && st.Name != "" {
		e.note("k6 name tag sanitized: " + js(name))
	}
	if len(rq.Headers) > 0 {
		var hs []string
		for _, h := range rq.Headers {
			hs = append(hs, "["+e.lit(h.Name)+", "+e.lit(h.Value)+"]")
		}
		sp = append(sp, "headers: ["+strings.Join(hs, ", ")+"]")
	}
	switch {
	case len(rq.Multipart) > 0:
		sp = append(sp, e.multipart(rq, loc))
	case len(rq.Form) > 0:
		sp = append(sp, "form: "+e.pairs(rq.Form))
	case rq.Body != "":
		sp = append(sp, "body: "+e.lit(rq.Body))
	}
	if rq.Timeout > 0 {
		sp = append(sp, fmt.Sprintf("timeout: %d", ms(rq.Timeout)))
	}
	if rq.FollowRedirects != nil {
		if *rq.FollowRedirects {
			sp = append(sp, "redirects: 10")
		} else {
			sp = append(sp, "redirects: 0")
		}
	}
	e.line("{")
	e.ind++
	e.line("const res = _req(V, {%s});", strings.Join(sp, ", "))

	var checks []string
	seen := map[string]int{}
	addCheck := func(label, expr string) {
		seen[label]++
		if seen[label] > 1 {
			label = fmt.Sprintf("%s #%d", label, seen[label])
		}
		checks = append(checks, fmt.Sprintf("%s: function (r) { return %s; }", js(label), expr))
	}
	for _, x := range rq.Extract {
		def := "undefined"
		if x.Default != "" {
			def = e.lit(x.Default)
		}
		var val string
		switch x.Type {
		case model.ExJSONPath:
			if !jpSubset.MatchString(x.Expr) {
				e.note("jsonpath " + x.Expr + " uses features beyond $, .name, ['name'], [n]; extraction may return nothing")
			}
			val = "_jp(res.body, " + e.lit(x.Expr) + ")"
			if x.Default != "" {
				val = "(function (v) { return v === undefined ? " + def + " : v; })(" + val + ")"
			}
		case model.ExRegex:
			pat, flags := convertRegex(x.Expr, e)
			val = fmt.Sprintf("_rx(res.body, %s, %s, %d, %d, %s)", js(pat), js(flags), x.Group, x.Match, def)
		case model.ExHeader:
			val = "_hdr(res, " + e.lit(x.Expr) + ")"
			if x.Default != "" {
				val = "(function (v) { return v === undefined ? " + def + " : v; })(" + val + ")"
			}
		case model.ExCookie:
			val = "_cookie(res, " + e.lit(x.Expr) + ")"
			if x.Default != "" {
				val = "(function (v) { return v === undefined ? " + def + " : v; })(" + val + ")"
			}
		case model.ExBoundary:
			val = fmt.Sprintf("_bnd(res.body, %s, %s, %d, %s)", e.lit(x.Left), e.lit(x.Right), x.Match, def)
		case model.ExStatus:
			val = "String(res.status)"
		case model.ExXPath:
			e.note("xpath extractor " + x.Name + " is not supported by k6; variable keeps its default/previous value (TODO)")
			e.line("// TODO: xpath extractor %s (%s) not supported by k6", x.Name, strings.ReplaceAll(x.Expr, "\n", " "))
			if x.Default != "" {
				e.line("_set(V, %s, %s);", js(x.Name), def)
			}
			if x.Required {
				addCheck("extract "+x.Name, "false")
			}
			continue
		}
		e.line("_set(V, %s, %s);", js(x.Name), val)
		if x.Required {
			addCheck("extract "+x.Name, "V["+js(x.Name)+"] !== undefined && V["+js(x.Name)+"] !== ''")
		}
	}
	for _, a := range rq.Assert {
		switch a.Type {
		case model.AsStatus:
			addCheck("status "+a.Value, "_stat(r.status, "+e.lit(a.Value)+")")
		case model.AsContains:
			addCheck("contains "+short(a.Value), "typeof r.body === 'string' && r.body.indexOf(_expand("+e.lit(a.Value)+", V)) >= 0")
		case model.AsNotContains:
			addCheck("not contains "+short(a.Value), "typeof r.body !== 'string' || r.body.indexOf(_expand("+e.lit(a.Value)+", V)) < 0")
		case model.AsRegex:
			pat, flags := convertRegex(a.Expr, e)
			addCheck("regex "+short(a.Expr), fmt.Sprintf("new RegExp(%s, %s).test(r.body)", js(pat), js(flags)))
		case model.AsJSONPath:
			if !jpSubset.MatchString(a.Expr) {
				e.note("jsonpath " + a.Expr + " uses features beyond the supported subset; assertion may fail")
			}
			if a.Value != "" {
				addCheck("jsonpath "+a.Expr+" == "+short(a.Value), "_str(_jp(r.body, "+e.lit(a.Expr)+")) === _expand("+e.lit(a.Value)+", V)")
			} else {
				addCheck("jsonpath "+a.Expr+" exists", "_jp(r.body, "+e.lit(a.Expr)+") !== undefined")
			}
		case model.AsMaxLatency:
			d, err := parseDur(a.Value)
			if err != nil {
				e.note("max_latency value " + a.Value + " is not a duration; assertion dropped")
				continue
			}
			addCheck("latency < "+a.Value, fmt.Sprintf("r.timings.duration < %d", d))
		case model.AsHeader:
			addCheck("header "+a.Expr, "String(_hdr(r, "+e.lit(a.Expr)+")).indexOf(_expand("+e.lit(a.Value)+", V)) >= 0")
		}
	}
	tag := "{ name: " + js(name) + " }"
	if len(checks) > 0 {
		e.line("const ok = check(res, {")
		e.ind++
		for _, c := range checks {
			e.line("%s,", c)
		}
		e.ind--
		e.line("}, %s);", tag)
		if e.abort {
			e.line("if (!ok) fail(%s);", js("request failed: "+name))
		}
	}
	e.ind--
	e.line("}")
	e.flush("request", st.Name, loc, "")
}

func short(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// multipart returns the spec fragment for multipart bodies.
func (e *exporter) multipart(rq *model.Request, loc string) string {
	hasFile := false
	for _, p := range rq.Multipart {
		hasFile = hasFile || p.File != ""
	}
	if !hasFile {
		return "multipart: " + e.pairs(partsKV(rq.Multipart))
	}
	var parts []string
	for _, p := range rq.Multipart {
		if p.File == "" {
			parts = append(parts, "["+e.lit(p.Name)+", "+e.lit(p.Value)+"]")
			continue
		}
		if strings.Contains(p.File, "${") {
			e.note("multipart file " + p.File + " contains variables; k6 open() needs a static path; value left empty")
			parts = append(parts, "["+e.lit(p.Name)+", \"\"]")
			continue
		}
		fp := p.File
		if !path.IsAbs(fp) && e.o.DataPrefix != "" {
			fp = path.Join(e.o.DataPrefix, fp)
		}
		id := fmt.Sprintf("FILE_%d", len(e.files)+1)
		e.files = append(e.files, "const "+id+" = open("+js(fp)+", 'b');\n")
		ct := p.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		parts = append(parts, fmt.Sprintf("[%s, \"\", {data: %s, name: %s, type: %s}]", e.lit(p.Name), id, js(path.Base(p.File)), js(ct)))
		e.note("file " + fp + " is read with open() at init; keep it at that path relative to the script")
	}
	return "fileBody: [" + strings.Join(parts, ", ") + "]"
}

func partsKV(ps []model.Part) []model.KV {
	var out []model.KV
	for _, p := range ps {
		out = append(out, model.KV{Name: p.Name, Value: p.Value})
	}
	return out
}

// convertRegex adapts RE2 syntax to JavaScript: leading inline flags become
// RegExp flags and (?P<n> becomes (?<n>.
func convertRegex(p string, e *exporter) (string, string) {
	flags := ""
	for {
		m := regexp.MustCompile(`^\(\?([imsU]+)\)`).FindStringSubmatch(p)
		if m == nil {
			break
		}
		for _, f := range m[1] {
			switch f {
			case 'i', 'm', 's':
				if !strings.ContainsRune(flags, f) {
					flags += string(f)
				}
			case 'U':
				e.note("regex flag (?U) (ungreedy) is not supported by JavaScript")
			}
		}
		p = p[len(m[0]):]
	}
	p = strings.ReplaceAll(p, "(?P<", "(?<")
	if regexp.MustCompile(`\(\?[imsU-]+[:)]`).MatchString(p) {
		e.note("inline regex flags in the middle of a pattern are not supported by JavaScript")
	}
	return p, flags
}

func parseDur(v string) (int64, error) {
	var d model.Duration
	if err := d.UnmarshalJSON([]byte(js(v))); err != nil {
		return 0, err
	}
	return ms(d), nil
}
