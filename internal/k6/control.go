package k6

import (
	"fmt"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

// k6Control tracks whether the control-flow helpers are needed.
type k6Control struct{ used bool }

// helpers returns the control-flow JS helpers when a control step was written.
func (c k6Control) helpers() string {
	if !c.used {
		return ""
	}
	return controlJS + "\n"
}

const controlJS = `// ---- control-flow helpers (Load Meter while/foreach/switch/percent/...) ----
const _ctl = {}; // per-VU state, keyed by step location
function _ctlCond(c, vars) {
  if (_expand(c, vars).indexOf('${') >= 0) return false; // unresolved variable ends the loop
  return _cond(c, vars);
}
function _ctlSwitch(v, names) {
  v = String(v).trim();
  if (v === '') return 0;
  if (/^[0-9]+$/.test(v)) { const n = parseInt(v, 10); return n < names.length ? n : 0; }
  let d = -1;
  for (let i = 0; i < names.length; i++) {
    if (names[i] === v) return i;
    if (d < 0 && names[i].toLowerCase() === 'default') d = i;
  }
  return d;
}
function _ctlPct(key, pct, execs) {
  const c = _ctl[key] || (_ctl[key] = { passes: 0, execs: 0 });
  const run = execs > 0 ? c.execs < execs : (100 * c.execs + 50) / (c.passes + 1) < pct;
  c.passes++;
  if (run) c.execs++;
  return run;
}
function _ctlRR(key, n) {
  const c = _ctl[key] || (_ctl[key] = { passes: 0 });
  return c.passes++ % n;
}
function _ctlShuffle(n) {
  const o = [];
  for (let i = 0; i < n; i++) o.push(i);
  for (let i = n - 1; i > 0; i--) { const j = Math.floor(Math.random() * (i + 1)); const t = o[i]; o[i] = o[j]; o[j] = t; }
  return o;
}
function _ctlCTT(key, intervalMs) {
  const now = Date.now();
  const prev = _ctl[key];
  if (prev === undefined || now > prev + intervalMs) { _ctl[key] = now; return; }
  _ctl[key] = prev + intervalMs;
  sleep((prev + intervalMs - now) / 1000);
}
// ---- end of control-flow helpers ----
`

func enabledSteps(steps []model.Step) []model.Step {
	var out []model.Step
	for _, s := range steps {
		if !s.Disabled {
			out = append(out, s)
		}
	}
	return out
}

// branches writes `if (v === 0) {...} else if (v === 1) {...}` over steps.
func (e *exporter) branches(v string, steps []model.Step, loc string) {
	for i := range steps {
		kw := "} else if"
		if i == 0 {
			kw = "if"
		}
		e.line("%s (%s === %d) {", kw, v, i)
		e.ind++
		e.steps(steps[i:i+1], fmt.Sprintf("%s.steps[%d]", loc, i))
		e.ind--
	}
	if len(steps) > 0 {
		e.line("}")
	}
}

// controlStep writes the control-flow steps; false for other step types.
func (e *exporter) controlStep(st *model.Step, l string) bool {
	maxN := st.EffectiveMaxIterations()
	switch {
	case st.Type == model.StepWhile:
		e.ctl.used = true
		cond := strings.TrimSpace(st.Cond)
		if cond == "" || strings.EqualFold(cond, "LAST") {
			e.rep.Add("while", st.Name, l, model.Unsupported, "a while condition on the last request's outcome (empty or LAST) cannot be expressed in the k6 export; inner steps run once")
			e.steps(st.Steps, l+".steps")
			return true
		}
		e.loopN++
		v := fmt.Sprintf("_w%d", e.loopN)
		guard := ""
		if st.Duration > 0 {
			e.line("const %s_end = Date.now() + %d;", v, ms(st.Duration))
			guard = fmt.Sprintf(" && Date.now() < %s_end", v)
		}
		e.line("for (let %s = 0; %s < %d && _ctlCond(%s, V)%s; %s++) {", v, v, maxN, e.lit(st.Cond), guard, v)
		e.ind++
		e.steps(st.Steps, l+".steps")
		e.ind--
		e.line("}")
		e.flush("while", st.Name, l, "")
	case st.Type == model.StepForeach:
		e.loopN++
		v := fmt.Sprintf("_f%d", e.loopN)
		sep := "_"
		if st.NoSeparator {
			sep = ""
		}
		e.line("for (let %s = %d, %s_n = 0; (%d === 0 || %s <= %d) && %s_n < %d; %s++, %s_n++) {", v, st.Start+1, v, st.End, v, st.End, v, maxN, v, v)
		e.ind++
		e.line("const %s_v = V[_expand(%s, V) + %s + %s];", v, e.lit(st.Input), js(sep), v)
		e.line("if (%s_v === undefined) break;", v)
		e.line("V[%s] = %s_v;", js(st.Var), v)
		e.steps(st.Steps, l+".steps")
		e.ind--
		e.line("}")
		e.flush("foreach", st.Name, l, "")
	case st.Type == model.StepSwitch:
		e.ctl.used = true
		kids := enabledSteps(st.Steps)
		var names []string
		for _, k := range kids {
			names = append(names, k.Name)
		}
		e.loopN++
		v := fmt.Sprintf("_s%d", e.loopN)
		e.line("{")
		e.ind++
		e.line("const %s = _ctlSwitch(_expand(%s, V), %s);", v, e.lit(st.Value), js(names))
		e.branches(v, kids, l)
		e.ind--
		e.line("}")
		e.flush("switch", st.Name, l, "")
	case st.Type == model.StepPercent:
		e.ctl.used = true
		e.line("if (_ctlPct(%s, %s, %d)) {", js(l), num(st.Percent), st.Executions)
		e.ind++
		e.steps(st.Steps, l+".steps")
		e.ind--
		e.line("}")
		if st.Shared { // after the children: their flush consumes pending notes
			e.note("shared counters become per-VU counters in k6 (no cross-VU state)")
		}
		e.flush("percent", st.Name, l, "")
	case st.Type == model.StepRuntime:
		e.loopN++
		v := fmt.Sprintf("_r%d", e.loopN)
		kids := enabledSteps(st.Steps)
		e.line("const %s_end = Date.now() + %d;", v, ms(st.Duration))
		e.line("for (let %s = 0; %s < %d && Date.now() < %s_end; %s++) {", v, v, maxN, v, v)
		e.ind++
		for i := range kids {
			e.line("if (Date.now() >= %s_end) break;", v)
			e.steps(kids[i:i+1], fmt.Sprintf("%s.steps[%d]", l, i))
		}
		e.ind--
		e.line("}")
		e.flush("runtime", st.Name, l, "")
	case st.Type == model.StepThroughput:
		e.ctl.used = true
		switch st.Scope {
		case model.ThroughputActive, model.ThroughputShared:
			e.note(fmt.Sprintf("scope %q approximated per VU at %g/min (k6 has no shared pacing; use an arrival-rate executor)", st.Scope, st.PerMinute))
		}
		e.line("_ctlCTT(%s, %s);", js(l), num(60000/st.PerMinute))
		e.flush("throughput", st.Name, l, "")
	case st.Type == model.StepRendezvous:
		e.line("// rendezvous %s skipped: k6 VUs cannot synchronise", js(st.Name))
		e.rep.Add("rendezvous", st.Name, l, model.Unsupported, "k6 has no cross-VU barrier; the rendezvous is skipped")
	case st.Type == model.StepCritical:
		e.rep.Add("critical", st.Name, l, model.Partial, "k6 has no cross-VU lock; inner steps run without mutual exclusion")
		e.steps(st.Steps, l+".steps")
	case st.Type == model.StepGroup && st.Select == model.SelectShuffle:
		e.ctl.used = true
		kids := enabledSteps(st.Steps)
		e.loopN++
		v := fmt.Sprintf("_o%d", e.loopN)
		e.line("group(%s, function () {", e.lit(st.Name))
		e.ind++
		e.line("for (const %s of _ctlShuffle(%d)) {", v, len(kids))
		e.ind++
		e.branches(v, kids, l)
		e.ind--
		e.line("}")
		e.ind--
		e.line("});")
		e.flush("group", st.Name, l, "")
	case st.Type == model.StepGroup && st.Select == model.SelectRoundRobin:
		e.ctl.used = true
		kids := enabledSteps(st.Steps)
		e.loopN++
		v := fmt.Sprintf("_k%d", e.loopN)
		e.line("group(%s, function () {", e.lit(st.Name))
		e.ind++
		e.line("const %s = _ctlRR(%s, %d);", v, js(l), max(len(kids), 1))
		e.branches(v, kids, l)
		e.ind--
		e.line("});")
		if st.Shared {
			e.note("shared rotation becomes a per-VU rotation in k6")
		}
		e.flush("group", st.Name, l, "")
	default:
		return false
	}
	return true
}

// workloadControl reports the WorkloadControl options.
func (e *exporter) workloadControl() {
	w := e.s.Workload
	if w.Arrivals == model.ArrivalsPoisson {
		e.rep.Add("workload.arrivals", "poisson", "workload.arrivals", model.Partial, "k6 arrival-rate executors start iterations evenly; Poisson spacing is not reproduced")
	}
	if w.NewUserEachIteration {
		e.rep.Add("workload.newUserEachIteration", "", "workload.newUserEachIteration", model.Partial, "k6 already resets the VU cookie jar every iteration by default (noCookiesReset: false); connections are kept unless the k6 option noVUConnectionReuse is set")
	}
}
