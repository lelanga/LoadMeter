package jmx

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// Import and export of JMeter logic controllers and timers that map to the
// Load Meter control-flow steps (model/control.go).

// controlControllers are the controllers imported by controlController.
var controlControllers = map[string]bool{
	"WhileController": true, "ForeachController": true, "SwitchController": true,
	"ThroughputController": true, "InterleaveControl": true, "RandomController": true,
	"RandomOrderController": true, "RunTime": true, "CriticalSectionController": true,
}

// floatProp reads a simple property or a <FloatProperty><name/><value/></FloatProperty>.
func (n *node) floatProp(name string) (string, bool) {
	if v, ok := n.prop(name); ok {
		return v, true
	}
	if n == nil {
		return "", false
	}
	for _, c := range n.Children {
		switch c.Name {
		case "FloatProperty", "DoubleProperty", "floatProp", "doubleProp":
		default:
			continue
		}
		if nm := c.child("name"); nm != nil && strings.TrimSpace(nm.Text()) == name {
			if v := c.child("value"); v != nil {
				return v.Text(), true
			}
		}
	}
	return "", false
}

// childUnits builds one step per executable child element, as JMeter's
// selection controllers see them: a child that expands to several steps
// (timers before a sampler, for example) is wrapped in a group of its name.
func (im *importer) childUnits(kids []elem, sc *scope, loc string) []model.Step {
	var execs []elem
	for _, k := range kids {
		l := loc + "/" + nameOf(k.n)
		if !enabled(k.n) {
			im.disabled(k, l)
			continue
		}
		if im.scopedElement(k, sc, l) {
			continue
		}
		execs = append(execs, k)
	}
	var out []model.Step
	for _, k := range execs {
		steps := im.execElement(k, sc, loc+"/"+nameOf(k.n))
		switch {
		case len(steps) == 0:
		case len(steps) == 1 && steps[0].Name == nameOf(k.n):
			out = append(out, steps[0])
		default:
			out = append(out, model.Step{Type: model.StepGroup, Name: nameOf(k.n), Steps: steps})
		}
	}
	return out
}

func (im *importer) controlController(k elem, sc *scope, loc string) []model.Step {
	n := k.n
	tc := testClass(n)
	name := nameOf(n)
	ctrl := &scope{parent: sc, level: "ctrl"}
	mk := func(typ string, c model.StepControl, steps []model.Step) []model.Step {
		return []model.Step{{Type: typ, Name: name, Comment: comment(n), StepControl: c, Steps: steps}}
	}
	switch tc {
	case "WhileController":
		raw := n.str("WhileController.condition")
		cond, note, ok := im.whileCondition(raw, loc)
		if !ok {
			why := fmt.Sprintf("While condition %q is not a form Load Meter can evaluate (empty, LAST, ${var}, or a simple ==/!= comparison); children kept in a disabled group", raw)
			im.add(tc, name, loc, model.Unsupported, why)
			im.manual("Rewrite While Controller %q at %s as a Load Meter while condition: %s", name, loc, raw)
			return []model.Step{im.disabledGroup(k, sc, loc, why)}
		}
		idx := im.add(tc, name, loc, model.Partial, fmt.Sprintf("while %q -> %q; capped at %d passes (JMeter has no cap), and an unresolved variable ends the loop (JMeter would loop forever)", raw, cond, model.DefaultMaxIterations))
		if note != "" {
			im.note(idx, note)
		}
		im.loopDepth++
		steps := im.buildLevel(k.kids, ctrl, loc)
		im.loopDepth--
		out := mk(model.StepWhile, model.StepControl{}, steps)
		out[0].Cond = cond
		return out
	case "ForeachController":
		in := strings.TrimSpace(im.conv(n.str("ForeachController.inputVal"), loc))
		out := strings.TrimSpace(n.str("ForeachController.returnVal"))
		if in == "" || out == "" {
			why := "ForEach Controller without input or output variable"
			im.add(tc, name, loc, model.Unsupported, why+"; children kept in a disabled group")
			return []model.Step{im.disabledGroup(k, sc, loc, why)}
		}
		c := model.StepControl{Input: in, Var: out, NoSeparator: !n.boolean("ForeachController.useSeparator", true)}
		idx := im.add(tc, name, loc, model.Supported, fmt.Sprintf("foreach %s_N -> %s", in, out))
		var notes []string
		if raw := strings.TrimSpace(n.str("ForeachController.startIndex")); raw != "" {
			if v, ok := im.num(raw, loc, &notes); ok && v >= 0 {
				c.Start = int(v)
			}
		}
		if raw := strings.TrimSpace(n.str("ForeachController.endIndex")); raw != "" {
			if v, ok := im.num(raw, loc, &notes); ok && v > float64(c.Start) {
				c.End = int(v)
			}
		}
		for _, nt := range notes {
			im.escalate(idx, model.Partial, nt)
		}
		im.note(idx, fmt.Sprintf("at most %d passes; __jm__%s__idx is not set", model.DefaultMaxIterations, name))
		im.loopDepth++
		steps := im.buildLevel(k.kids, ctrl, loc)
		im.loopDepth--
		return mk(model.StepForeach, c, steps)
	case "SwitchController":
		val := im.conv(strings.TrimSpace(n.str("SwitchController.value")), loc)
		im.add(tc, name, loc, model.Supported, fmt.Sprintf("switch on %q (JMeter rules: empty/out-of-range index = first child, unknown name = child named default or nothing)", val))
		return mk(model.StepSwitch, model.StepControl{Value: val}, im.childUnits(k.kids, ctrl, loc))
	case "ThroughputController":
		c := model.StepControl{Shared: !n.boolean("ThroughputController.perThread", false)}
		idx := im.add(tc, name, loc, model.Supported, "")
		var notes []string
		if strings.TrimSpace(n.str("ThroughputController.style")) == "1" {
			raw, _ := n.floatProp("ThroughputController.percentThroughput")
			v, ok := im.num(orDefault(strings.TrimSpace(raw), "100"), loc, &notes)
			if !ok || v < 0 || v > 100 {
				why := fmt.Sprintf("percent %q not usable", raw)
				im.escalate(idx, model.Unsupported, why)
				return []model.Step{im.disabledGroup(k, sc, loc, why)}
			}
			if v == 0 {
				im.note(idx, "0% never runs: children kept in a disabled group")
				return []model.Step{im.disabledGroup(k, sc, loc, "Throughput Controller at 0%")}
			}
			c.Percent = v
			im.note(idx, fmt.Sprintf("percent step %g%% (deterministic JMeter rule)", v))
		} else {
			raw := orDefault(strings.TrimSpace(n.str("ThroughputController.maxThroughput")), "1")
			v, ok := im.num(raw, loc, &notes)
			if !ok || v < 0 {
				why := fmt.Sprintf("total executions %q not usable", raw)
				im.escalate(idx, model.Unsupported, why)
				return []model.Step{im.disabledGroup(k, sc, loc, why)}
			}
			if v == 0 {
				im.note(idx, "0 executions never runs: children kept in a disabled group")
				return []model.Step{im.disabledGroup(k, sc, loc, "Throughput Controller with 0 executions")}
			}
			c.Executions = int(v)
			im.note(idx, fmt.Sprintf("percent step limited to the first %d executions", c.Executions))
		}
		for _, nt := range notes {
			im.escalate(idx, model.Partial, nt)
		}
		if c.Shared {
			im.note(idx, "counters shared by all VUs (per user unchecked)")
		}
		return mk(model.StepPercent, c, im.buildLevel(k.kids, ctrl, loc))
	case "InterleaveControl":
		idx := im.add(tc, name, loc, model.Supported, "group with select roundRobin (one child per pass)")
		shared := n.boolean("InterleaveControl.accrossThreads", false)
		if shared {
			im.note(idx, "rotation shared by all VUs (interleave across threads)")
		}
		units := im.childUnits(k.kids, ctrl, loc)
		for _, u := range units {
			if u.Type != model.StepRequest {
				im.escalate(idx, model.Partial, "a child is a controller: Load Meter runs the whole child on its turn; JMeter's sub-controller interleaving (style) may pick one sampler at a time")
				break
			}
		}
		return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Select: model.SelectRoundRobin,
			StepControl: model.StepControl{Shared: shared}, Steps: units}}
	case "RandomController":
		im.add(tc, name, loc, model.Supported, "group with select weighted, equal weights (one random child per pass)")
		return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Select: model.SelectWeighted, Steps: im.childUnits(k.kids, ctrl, loc)}}
	case "RandomOrderController":
		im.add(tc, name, loc, model.Supported, "group with select shuffle (every child once, random order)")
		return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Select: model.SelectShuffle, Steps: im.childUnits(k.kids, ctrl, loc)}}
	case "RunTime":
		raw := strings.TrimSpace(n.str("RunTime.seconds"))
		var notes []string
		v, ok := im.num(raw, loc, &notes)
		if !ok || v <= 0 {
			why := fmt.Sprintf("runtime %q seconds not usable (0 runs nothing)", raw)
			im.add(tc, name, loc, model.Unsupported, why+"; children kept in a disabled group")
			return []model.Step{im.disabledGroup(k, sc, loc, why)}
		}
		idx := im.add(tc, name, loc, model.Supported, fmt.Sprintf("runtime %gs (checked between children, like JMeter)", v))
		for _, nt := range notes {
			im.escalate(idx, model.Partial, nt)
		}
		im.loopDepth++
		steps := im.buildLevel(k.kids, ctrl, loc)
		im.loopDepth--
		return mk(model.StepRuntime, model.StepControl{Duration: secs(v)}, steps)
	case "CriticalSectionController":
		lock := strings.TrimSpace(n.str("CriticalSectionController.lockName"))
		if lock == "" {
			lock = "global_lock"
		}
		idx := im.add(tc, name, loc, model.Supported, fmt.Sprintf("critical section, lock %q (shared by every VU of the run)", lock))
		if strings.Contains(lock, "${") {
			im.escalate(idx, model.Partial, "lock name with variables is used literally")
		}
		return mk(model.StepCritical, model.StepControl{Key: lock}, im.buildLevel(k.kids, ctrl, loc))
	}
	return nil
}

var (
	jexlCmp = regexp.MustCompile(`^\s*"?\$\{([^${}"]+)\}"?\s*(==|!=)\s*"?([^"\s]*)"?\s*$`)
)

// whileCondition converts a JMeter While condition.
func (im *importer) whileCondition(raw, loc string) (cond, note string, ok bool) {
	expr := strings.TrimSpace(raw)
	switch {
	case expr == "":
		return "", "empty condition: stop after a pass whose last request failed", true
	case strings.EqualFold(expr, "LAST"):
		return "LAST", "LAST: also stops when the request before the loop failed", true
	}
	if m := wholeVarRE.FindStringSubmatch(expr); m != nil && !strings.HasPrefix(m[1], "__") {
		return "${" + m[1] + "} != false", "JMeter continues unless the value is \"false\" (any case); Load Meter compares with \"false\" case-sensitively", true
	}
	if c, nt, ok := im.ifCondition(expr, loc); ok {
		return c, nt, true
	}
	if strings.HasPrefix(expr, "${__jexl3(") || strings.HasPrefix(expr, "${__jexl2(") || strings.HasPrefix(expr, "${__groovy(") {
		if r, parsed := parseRef(expr, 0); parsed && r.end == len(expr) && len(r.args) > 0 {
			if m := jexlCmp.FindStringSubmatch(unescapeArg(r.args[0])); m != nil {
				return "${" + m[1] + "} " + m[2] + " " + im.conv(m[3], loc), "compared as text (JEXL compares numbers numerically: 10 vs 10.0 differ here)", true
			}
		}
	}
	return "", "", false
}

func (im *importer) cttTimer(k elem, sc *scope, loc string) {
	n := k.n
	name := nameOf(n)
	tp, _ := n.floatProp("throughput")
	raw := strings.TrimSpace(orDefault(tp, n.str("ConstantThroughputTimer.throughput")))
	idx := im.add("ConstantThroughputTimer", name, loc, model.Supported, "")
	var notes []string
	v, ok := im.num(raw, loc, &notes)
	if !ok || v <= 0 || math.IsInf(v, 0) {
		im.escalate(idx, model.Unsupported, fmt.Sprintf("throughput %q samples/min not usable; timer not converted", raw))
		return
	}
	for _, nt := range notes {
		im.escalate(idx, model.Partial, nt)
	}
	mode := strings.TrimSpace(n.str("calcMode"))
	if mode == "" {
		mode = strings.TrimSpace(n.str("ConstantThroughputTimer.calcMode"))
	}
	if strings.HasPrefix(mode, "calcMode.") { // enum name form: calcMode.1 .. calcMode.5
		if d, err := strconv.Atoi(strings.TrimPrefix(mode, "calcMode.")); err == nil {
			mode = strconv.Itoa(d - 1)
		}
	}
	scope := model.ThroughputVU
	switch mode {
	case "", "0", "ThisThreadOnly":
	case "1", "AllActiveThreads":
		scope = model.ThroughputActive
	case "2", "AllActiveThreadsInCurrentThreadGroup":
		scope = model.ThroughputActive
		im.note(idx, "\"all active threads in current thread group\": Load Meter counts all active VUs (one workload)")
	case "3", "AllActiveThreads_Shared":
		scope = model.ThroughputShared
	case "4", "AllActiveThreadsInCurrentThreadGroup_Shared":
		scope = model.ThroughputShared
		im.note(idx, "group-shared mode mapped to run-wide shared (one workload)")
	default:
		im.escalate(idx, model.Partial, fmt.Sprintf("unknown calcMode %q: this-thread-only assumed", mode))
	}
	im.note(idx, fmt.Sprintf("throughput step %g/min (scope %s) before each sampler in scope; a late VU does not catch up (deficit lost, as in JMeter)", v, scope))
	st := model.Step{Type: model.StepThroughput, Name: name, Comment: comment(n),
		StepControl: model.StepControl{PerMinute: v, Scope: scope, Key: loc}}
	sc.timers = append(sc.timers, stepRef{step: st, item: idx})
	im.scoped = append(im.scoped, idx)
}

func (im *importer) syncTimer(k elem, sc *scope, loc string) {
	n := k.n
	name := nameOf(n)
	idx := im.add("SyncTimer", name, loc, model.Supported, "")
	var notes []string
	c := model.StepControl{Key: loc}
	if raw := strings.TrimSpace(n.str("groupSize")); raw != "" {
		if v, ok := im.num(raw, loc, &notes); ok && v >= 0 {
			c.Group = int(v)
		}
	}
	tmo := time.Duration(0)
	if raw := strings.TrimSpace(n.str("timeoutInMs")); raw != "" {
		if v, ok := im.num(raw, loc, &notes); ok && v > 0 {
			tmo = time.Duration(v * float64(time.Millisecond))
		}
	}
	for _, nt := range notes {
		im.escalate(idx, model.Partial, nt)
	}
	switch {
	case tmo == 0:
		im.escalate(idx, model.Partial, fmt.Sprintf("timeout 0 (wait forever in JMeter) mapped to the Load Meter default %s", model.DefaultRendezvousTimeout))
	case tmo > model.MaxRendezvousTimeout:
		tmo = model.MaxRendezvousTimeout
		im.escalate(idx, model.Partial, fmt.Sprintf("timeout capped at %s", model.MaxRendezvousTimeout))
	}
	c.Timeout = model.Duration(tmo)
	if c.Group == 0 {
		im.note(idx, "group size 0: the number of VUs active at each arrival")
	}
	im.note(idx, "rendezvous step before each sampler in scope; the wait counts as think time")
	sc.timers = append(sc.timers, stepRef{step: model.Step{Type: model.StepRendezvous, Name: name, Comment: comment(n), StepControl: c}, item: idx})
	im.scoped = append(im.scoped, idx)
}

// ---------------------------------------------------------------------------
// Export

// controlStep writes the control-flow steps and the new group modes; it
// returns false for other steps.
func (ex *exporter) controlStep(st model.Step, loc string) bool {
	w := ex.w
	en := !st.Disabled
	dis := ""
	if st.Disabled {
		dis = "; disabled (enabled=\"false\")"
	}
	kids := func(l string) func() { return func() { ex.writeSteps(st.Steps, l) } }
	capNote := func() string {
		return fmt.Sprintf("; Load Meter caps it at %d passes, JMeter does not", st.EffectiveMaxIterations())
	}
	switch {
	case st.Type == model.StepWhile:
		name := stepName(st, "While Controller")
		l := loc + "/" + name
		cond, status, note := ex.whileCond(st.Cond, l)
		if st.Duration > 0 {
			status, note = model.Partial, joinNote(note, "while duration cap not exported")
		}
		ex.item("while", name, l, status, note+capNote()+dis)
		w.element("WhileController", "WhileControllerGui", name, en, func() {
			w.str("WhileController.condition", cond)
			ex.commentProp(st)
		}, kids(l))
	case st.Type == model.StepForeach:
		name := stepName(st, "ForEach Controller")
		l := loc + "/" + name
		ex.item("foreach", name, l, model.Supported, "ForEach Controller"+capNote()+dis)
		w.element("ForeachController", "ForeachControlPanel", name, en, func() {
			w.str("ForeachController.inputVal", ex.fn(st.Input, l))
			w.str("ForeachController.returnVal", st.Var)
			w.boolp("ForeachController.useSeparator", !st.NoSeparator)
			if st.Start > 0 {
				w.str("ForeachController.startIndex", strconv.Itoa(st.Start))
			}
			if st.End > 0 {
				w.str("ForeachController.endIndex", strconv.Itoa(st.End))
			}
			ex.commentProp(st)
		}, kids(l))
	case st.Type == model.StepSwitch:
		name := stepName(st, "Switch Controller")
		l := loc + "/" + name
		ex.item("switch", name, l, model.Supported, "Switch Controller (each inner step is one switch child)"+dis)
		w.element("SwitchController", "SwitchControllerGui", name, en, func() {
			w.str("SwitchController.value", ex.fn(st.Value, l))
			ex.commentProp(st)
		}, ex.unitKids(st.Steps, l))
	case st.Type == model.StepPercent:
		name := stepName(st, "Throughput Controller")
		l := loc + "/" + name
		ex.item("percent", name, l, model.Supported, "Throughput Controller"+dis)
		w.element("ThroughputController", "ThroughputControllerGui", name, en, func() {
			if st.Executions > 0 {
				w.intp("ThroughputController.style", 0)
			} else {
				w.intp("ThroughputController.style", 1)
			}
			w.boolp("ThroughputController.perThread", !st.Shared)
			w.intp("ThroughputController.maxThroughput", max(st.Executions, 1))
			w.open("FloatProperty")
			w.leaf("name", "ThroughputController.percentThroughput")
			pct := st.Percent
			if st.Executions > 0 {
				pct = 100
			}
			w.leaf("value", strconv.FormatFloat(pct, 'f', -1, 64))
			w.leaf("savedValue", "0.0")
			w.close("FloatProperty")
			ex.commentProp(st)
		}, kids(l))
	case st.Type == model.StepRuntime:
		name := stepName(st, "Runtime Controller")
		l := loc + "/" + name
		secsF := st.Duration.D().Seconds()
		status, note := model.Supported, "Runtime Controller"
		if secsF != math.Trunc(secsF) {
			status, note = model.Partial, "Runtime Controller (duration rounded up to whole seconds)"
		}
		ex.item("runtime", name, l, status, note+dis)
		w.element("RunTime", "RunTimeGui", name, en, func() {
			w.str("RunTime.seconds", strconv.FormatInt(int64(math.Ceil(secsF)), 10))
			ex.commentProp(st)
		}, kids(l))
	case st.Type == model.StepCritical:
		name := stepName(st, "Critical Section Controller")
		l := loc + "/" + name
		lock := st.Key
		if lock == "" {
			lock = name
		}
		ex.item("critical", name, l, model.Supported, fmt.Sprintf("Critical Section Controller (lock %q)", lock)+dis)
		w.element("CriticalSectionController", "CriticalSectionControllerGui", name, en, func() {
			w.str("CriticalSectionController.lockName", lock)
			ex.commentProp(st)
		}, kids(l))
	case st.Type == model.StepThroughput:
		name := stepName(st, "Constant Throughput Timer")
		l := loc + "/" + name
		mode := 0
		switch st.Scope {
		case model.ThroughputActive:
			mode = 1
		case model.ThroughputShared:
			mode = 3
		}
		ex.item("throughput", name, l, model.Supported, "Flow Control Action (pause 0) carrying a Constant Throughput Timer"+dis)
		ex.carrier(name, en, st, func() {
			w.element("ConstantThroughputTimer", "TestBeanGUI", name, true, func() {
				w.open("doubleProp")
				w.leaf("name", "throughput")
				w.leaf("value", strconv.FormatFloat(st.PerMinute, 'f', -1, 64))
				w.leaf("savedValue", "0.0")
				w.close("doubleProp")
				w.intp("calcMode", mode)
			}, nil)
		})
	case st.Type == model.StepRendezvous:
		name := stepName(st, "Synchronizing Timer")
		l := loc + "/" + name
		ex.item("rendezvous", name, l, model.Supported, "Flow Control Action (pause 0) carrying a Synchronizing Timer"+dis)
		ex.carrier(name, en, st, func() {
			w.element("SyncTimer", "TestBeanGUI", name, true, func() {
				w.intp("groupSize", st.Group)
				w.leaf("longProp", strconv.FormatInt(st.EffectiveTimeout().Milliseconds(), 10), "name", "timeoutInMs")
			}, nil)
		})
	case st.Type == model.StepGroup && st.Select == model.SelectShuffle:
		name := stepName(st, "Random Order Controller")
		l := loc + "/" + name
		ex.item("group", name, l, model.Supported, "Random Order Controller"+dis)
		w.element("RandomOrderController", "RandomOrderControllerGui", name, en, func() { ex.commentProp(st) }, ex.unitKids(st.Steps, l))
	case st.Type == model.StepGroup && st.Select == model.SelectRoundRobin:
		name := stepName(st, "Interleave Controller")
		l := loc + "/" + name
		ex.item("group", name, l, model.Supported, "Interleave Controller"+dis)
		w.element("InterleaveControl", "InterleaveControlGui", name, en, func() {
			w.intp("InterleaveControl.style", 1)
			w.boolp("InterleaveControl.accrossThreads", st.Shared)
			ex.commentProp(st)
		}, ex.unitKids(st.Steps, l))
	case st.Type == model.StepGroup && st.Select == model.SelectWeighted && equalWeights(st.Steps):
		name := stepName(st, "Random Controller")
		l := loc + "/" + name
		ex.item("group", name, l, model.Supported, "Random Controller (weighted group with equal weights)"+dis)
		w.element("RandomController", "RandomControlGui", name, en, func() {
			w.intp("InterleaveControl.style", 1)
			ex.commentProp(st)
		}, ex.unitKids(st.Steps, l))
	default:
		return false
	}
	return true
}

// equalWeights reports whether every enabled step has the same weight > 0
// (a JMeter Random Controller); zero-weight steps make it unequal.
func equalWeights(steps []model.Step) bool {
	w := -1.0
	for i := range steps {
		if steps[i].Disabled {
			continue
		}
		x := steps[i].StepWeight()
		if x <= 0 || (w >= 0 && x != w) {
			return false
		}
		w = x
	}
	return w > 0
}

// unitKids writes each inner step as exactly one JMeter child (wrapping
// think-before-request pairs is not wanted here: every step is one unit).
func (ex *exporter) unitKids(steps []model.Step, loc string) func() {
	return func() {
		for _, s := range steps {
			ex.step(s, loc)
		}
	}
}

// carrier writes a pause-0 Flow Control Action whose only child is a timer.
func (ex *exporter) carrier(name string, en bool, st model.Step, timer func()) {
	w := ex.w
	w.element("TestAction", "TestActionGui", name, en, func() {
		w.intp("ActionProcessor.action", 1)
		w.intp("ActionProcessor.target", 0)
		w.str("ActionProcessor.duration", "0")
		ex.commentProp(st)
	}, timer)
}

func (ex *exporter) whileCond(c, loc string) (cond, status, note string) {
	t := strings.TrimSpace(c)
	switch {
	case t == "":
		return "", model.Supported, "While Controller (empty condition: until the last sample fails)"
	case strings.EqualFold(t, "LAST"):
		return "LAST", model.Supported, "While Controller (LAST)"
	}
	if m := condCmp.FindStringSubmatch(t); m != nil && strings.Trim(strings.TrimSpace(m[3]), `"'`) == "false" && m[2] == "!=" {
		return "${" + m[1] + "}", model.Partial, "While Controller on ${var} (JMeter stops only on the literal false; an unset variable loops forever in JMeter)"
	}
	if jc, ok := exportCond(ex.fn(t, loc)); ok {
		return jc, model.Partial, fmt.Sprintf("While Controller %q (an unresolved variable stops Load Meter's loop but not JMeter's)", jc)
	}
	return ex.fn(t, loc), model.Unsupported, fmt.Sprintf("condition %q not convertible; written verbatim", c)
}
