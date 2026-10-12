package jmx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lelanga/loadmeter/internal/model"
)

// xw is a tiny indenting XML writer; all text and attribute values are escaped.
type xw struct {
	b   bytes.Buffer
	ind int
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func attrs(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, ` %s="%s"`, kv[i], esc(kv[i+1]))
	}
	return b.String()
}

func (w *xw) line(s string) {
	w.b.WriteString(strings.Repeat("  ", w.ind))
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

func (w *xw) open(tag string, kv ...string) {
	w.line("<" + tag + attrs(kv...) + ">")
	w.ind++
}

func (w *xw) close(tag string) {
	w.ind--
	w.line("</" + tag + ">")
}

func (w *xw) leaf(tag, text string, kv ...string) {
	if text == "" {
		w.line("<" + tag + attrs(kv...) + "/>")
		return
	}
	w.line("<" + tag + attrs(kv...) + ">" + esc(text) + "</" + tag + ">")
}

func (w *xw) str(name, v string)        { w.leaf("stringProp", v, "name", name) }
func (w *xw) boolp(name string, v bool) { w.leaf("boolProp", strconv.FormatBool(v), "name", name) }
func (w *xw) intp(name string, v int)   { w.leaf("intProp", strconv.Itoa(v), "name", name) }

// element writes a test element followed by its <hashTree> of children.
func (w *xw) element(tag, gui, name string, enabled bool, props func(), kids func()) {
	w.open(tag, "guiclass", gui, "testclass", tag, "testname", name, "enabled", strconv.FormatBool(enabled))
	if props != nil {
		props()
	}
	w.close(tag)
	if kids == nil {
		w.line("<hashTree/>")
		return
	}
	w.open("hashTree")
	kids()
	w.close("hashTree")
}

// javaHash mirrors String.hashCode(), which JMeter uses to name collection entries.
func javaHash(s string) string {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = 31*h + int32(u)
	}
	return strconv.Itoa(int(h))
}

type exporter struct {
	s        *model.Scenario
	rep      *model.CompatReport
	w        *xw
	basePath string
	defFol   *bool
	seen     map[string]bool
}

func (ex *exporter) item(el, name, loc, status, note string) {
	ex.rep.Add(el, name, loc, status, note)
}

func (ex *exporter) fn(s, loc string) string {
	out, issues := exportFuncs(s)
	for _, is := range issues {
		key := loc + "|" + is.Note
		if !ex.seen[key] {
			ex.seen[key] = true
			ex.item("function "+is.Fn, "", loc, is.Status, is.Note)
		}
	}
	return out
}

// Export writes s as a JMeter 5.6-style .jmx test plan. The report lists how
// each Load Meter construct was represented.
func Export(s *model.Scenario) ([]byte, *model.CompatReport, error) {
	if s == nil {
		return nil, nil, errors.New("jmx: nil scenario")
	}
	ex := &exporter{s: s, rep: &model.CompatReport{Source: s.Name, Format: "jmx"}, w: &xw{}, seen: map[string]bool{}}
	w := ex.w
	name := s.Name
	if name == "" {
		name = "Load Meter scenario"
	}
	w.line(`<?xml version="1.0" encoding="UTF-8"?>`)
	w.open("jmeterTestPlan", "version", "1.2", "properties", "5.0", "jmeter", "5.6.3")
	w.open("hashTree")
	ex.item("scenario", name, "TestPlan", model.Supported, "Test Plan")
	keys := make([]string, 0, len(s.Variables))
	for k := range s.Variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		ex.item("variables", "", "TestPlan", model.Supported, fmt.Sprintf("%d variable(s) -> Test Plan User Defined Variables", len(keys)))
	}
	w.element("TestPlan", "TestPlanGui", name, true, func() {
		if s.Description != "" {
			w.str("TestPlan.comments", s.Description)
		}
		w.boolp("TestPlan.functional_mode", false)
		w.boolp("TestPlan.tearDown_on_shutdown", true)
		w.boolp("TestPlan.serialize_threadgroups", false)
		w.open("elementProp", "name", "TestPlan.user_defined_variables", "elementType", "Arguments",
			"guiclass", "ArgumentsPanel", "testclass", "Arguments", "testname", "User Defined Variables", "enabled", "true")
		if len(keys) == 0 {
			w.leaf("collectionProp", "", "name", "Arguments.arguments")
		} else {
			w.open("collectionProp", "name", "Arguments.arguments")
			for _, k := range keys {
				w.open("elementProp", "name", k, "elementType", "Argument")
				w.str("Argument.name", k)
				w.str("Argument.value", ex.fn(s.Variables[k], "TestPlan/variables"))
				w.str("Argument.metadata", "=")
				w.close("elementProp")
			}
			w.close("collectionProp")
		}
		w.close("elementProp")
		w.str("TestPlan.user_define_classpath", "")
	}, ex.planChildren)
	w.close("hashTree")
	w.close("jmeterTestPlan")
	ex.rep.ComputeConfidence()
	return w.b.Bytes(), ex.rep, nil
}

// splitURL splits an absolute URL into JMeter's protocol/domain/port/path
// fields without url.Parse, so ${...} references survive in any part.
func splitURL(u string) (proto, domain, port, p string) {
	i := strings.Index(u, "://")
	if i <= 0 || strings.ContainsAny(u[:i], "/?${") {
		return "", "", "", u
	}
	proto = strings.ToLower(u[:i])
	rest := u[i+3:]
	j := strings.IndexAny(rest, "/?")
	hostport := rest
	p = "/"
	if j >= 0 {
		hostport, p = rest[:j], rest[j:]
		if strings.HasPrefix(p, "?") {
			p = "/" + p
		}
	}
	domain = hostport
	if k := strings.LastIndex(hostport, ":"); k >= 0 && !strings.Contains(hostport[k:], "]") && !strings.Contains(hostport[k:], "}") {
		domain, port = hostport[:k], hostport[k+1:]
	}
	return
}

func durMS(d model.Duration) int64 { return d.D().Milliseconds() }

func (ex *exporter) planChildren() {
	s, w := ex.s, ex.w
	d := s.Defaults
	if d.BaseURL != "" || d.Timeout > 0 {
		proto, domain, port, p := splitURL(ex.fn(d.BaseURL, "defaults"))
		status, note := model.Supported, "HTTP Request Defaults"
		if d.BaseURL != "" && domain == "" {
			status, note = model.Partial, fmt.Sprintf("baseUrl %q is not an absolute URL; written as default path", d.BaseURL)
			p = d.BaseURL
		} else if p != "/" && p != "" {
			ex.basePath = strings.TrimRight(p, "/")
			note += fmt.Sprintf("; baseUrl path %q prefixed to every relative request path (JMeter default path is not a prefix)", p)
			p = ""
		} else {
			p = ""
		}
		if d.Timeout > 0 {
			note += "; timeout written as response (read) timeout"
		}
		ex.item("defaults", "", "TestPlan/HTTP Request Defaults", status, note)
		w.element("ConfigTestElement", "HttpDefaultsGui", "HTTP Request Defaults", true, func() {
			w.open("elementProp", "name", "HTTPsampler.Arguments", "elementType", "Arguments", "guiclass", "HTTPArgumentsPanel",
				"testclass", "Arguments", "testname", "User Defined Variables", "enabled", "true")
			w.leaf("collectionProp", "", "name", "Arguments.arguments")
			w.close("elementProp")
			w.str("HTTPSampler.domain", domain)
			w.str("HTTPSampler.port", port)
			w.str("HTTPSampler.protocol", proto)
			w.str("HTTPSampler.contentEncoding", "")
			w.str("HTTPSampler.path", p)
			w.str("HTTPSampler.connect_timeout", "")
			if d.Timeout > 0 {
				w.str("HTTPSampler.response_timeout", strconv.FormatInt(durMS(d.Timeout), 10))
			} else {
				w.str("HTTPSampler.response_timeout", "")
			}
		}, nil)
	}
	ex.defFol = d.FollowRedirects
	if d.InsecureSkipVerify {
		ex.item("defaults.insecureSkipVerify", "", "TestPlan", model.Supported, "JMeter does not verify server certificates by default")
	}
	if d.MaxBodyBytes > 0 {
		ex.item("defaults.maxBodyBytes", "", "TestPlan", model.Ignored, "response body cap has no JMeter equivalent")
	}
	if len(d.Headers) > 0 {
		ex.item("defaults.headers", "", "TestPlan/HTTP Header Manager", model.Supported, "Test Plan level HTTP Header Manager")
		ex.headerManager("HTTP Header Manager", d.Headers, "TestPlan")
	}
	if !d.DisableCookies {
		ex.item("defaults.cookies", "", "TestPlan/HTTP Cookie Manager", model.Supported, "HTTP Cookie Manager (standard policy)")
		w.element("CookieManager", "CookiePanel", "HTTP Cookie Manager", true, func() {
			w.leaf("collectionProp", "", "name", "CookieManager.cookies")
			w.boolp("CookieManager.clearEachIteration", false)
			w.boolp("CookieManager.controlledByThreadGroup", false)
		}, nil)
	} else {
		ex.item("defaults.disableCookies", "", "TestPlan", model.Supported, "cookies disabled: no HTTP Cookie Manager written (JMeter then ignores cookies)")
	}
	for _, ds := range s.DataSources {
		ex.csv(ds)
	}
	for _, sla := range s.SLAs {
		ex.item("sla", sla.Metric, "slas", model.Unsupported, fmt.Sprintf(
			"SLA %s %s %g (scope %q) not exported: JMeter has no run-level pass/fail criteria built in", sla.Metric, sla.Op, sla.Threshold, sla.Scope))
	}
	ex.threadGroup()
	if len(s.Teardown) > 0 {
		ex.item("teardown", "", "TestPlan/Load Meter teardown", model.Partial,
			"teardown steps written to a tearDown Thread Group (1 thread, 1 loop, after the test); Load Meter runs teardown per VU")
		w.element("PostThreadGroup", "PostThreadGroupGui", "Load Meter teardown", true, func() {
			ex.tgProps(1, 0, 1, false, 0)
		}, func() { ex.writeSteps(s.Teardown, "teardown") })
	}
}

func (ex *exporter) tgProps(threads, ramp, loops int, sched bool, dur int64) {
	w := ex.w
	w.str("ThreadGroup.on_sample_error", "continue")
	w.open("elementProp", "name", "ThreadGroup.main_controller", "elementType", "LoopController",
		"guiclass", "LoopControlPanel", "testclass", "LoopController", "testname", "Loop Controller", "enabled", "true")
	w.boolp("LoopController.continue_forever", false)
	w.str("LoopController.loops", strconv.Itoa(loops))
	w.close("elementProp")
	w.str("ThreadGroup.num_threads", strconv.Itoa(threads))
	w.str("ThreadGroup.ramp_time", strconv.Itoa(ramp))
	w.boolp("ThreadGroup.scheduler", sched)
	if sched {
		w.str("ThreadGroup.duration", strconv.FormatInt(dur, 10))
	} else {
		w.str("ThreadGroup.duration", "")
	}
	w.str("ThreadGroup.delay", "")
	w.boolp("ThreadGroup.same_user_on_next_iteration", true)
}

func ceilSecs(d model.Duration) (int64, bool) {
	s := d.D().Seconds()
	c := math.Ceil(s)
	return int64(c), c == s
}

func (ex *exporter) threadGroup() {
	s, w := ex.s, ex.w
	wl := s.Workload
	threads, ramp, loops, sched := max(wl.VUs, 1), 0, -1, true
	var dur int64
	status := model.Supported
	var notes []string
	exact := true
	switch wl.Executor {
	case model.ExecConstantVUs:
		var ok bool
		dur, ok = ceilSecs(wl.Duration)
		exact = exact && ok
		notes = append(notes, fmt.Sprintf("constant-vus: %d threads, %ds", threads, dur))
	case model.ExecRampingVUs:
		peak := 0.0
		var total time.Duration
		for _, st := range wl.Stages {
			peak = math.Max(peak, st.Target)
			total += st.Duration.D()
		}
		threads = max(int(math.Round(peak)), 1)
		var ok bool
		dur, ok = ceilSecs(model.Duration(total))
		exact = exact && ok
		simple := wl.VUs == 0 && len(wl.Stages) > 0 && wl.Stages[0].Target == peak &&
			(len(wl.Stages) == 1 || (len(wl.Stages) == 2 && wl.Stages[1].Target == peak))
		if len(wl.Stages) > 0 {
			r, ok := ceilSecs(wl.Stages[0].Duration)
			ramp = int(r)
			exact = exact && ok
		}
		if simple {
			notes = append(notes, fmt.Sprintf("ramping-vus: ramp to %d over %ds, total %ds", threads, ramp, dur))
		} else {
			status = model.Partial
			notes = append(notes, fmt.Sprintf("ramping-vus with %d stages approximated as a single ramp to the peak (%d threads over %ds, total %ds); use the Ultimate/Concurrency Thread Group plugin for the exact profile", len(wl.Stages), threads, ramp, dur))
		}
	case model.ExecPerVUIterations:
		loops, sched = max(wl.Iterations, 1), false
		notes = append(notes, fmt.Sprintf("per-vu-iterations: %d threads x %d loops", threads, loops))
		if wl.Duration > 0 {
			status = model.Partial
			notes = append(notes, "duration cap not exported (JMeter scheduler would also stop the loops)")
		}
	case model.ExecArrivalRate, model.ExecRampingRate:
		status = model.Partial
		threads = max(wl.MaxVUs, wl.VUs, 1)
		total := wl.Duration.D()
		if wl.Executor == model.ExecRampingRate {
			total = wl.PlannedDuration()
		}
		dur, _ = ceilSecs(model.Duration(total))
		notes = append(notes, fmt.Sprintf("%s (open model) approximated by a closed Thread Group of %d threads for %ds; the arrival rate is NOT enforced: add a Precise Throughput Timer or use the Arrivals/Concurrency Thread Group plugin", wl.Executor, threads, dur))
	default:
		status = model.Partial
		dur = 60
		notes = append(notes, fmt.Sprintf("unknown executor %q: wrote 1 thread for 60s", wl.Executor))
	}
	if !exact {
		status = model.Partial
		notes = append(notes, "sub-second durations rounded up to whole seconds")
	}
	if wl.Pacing > 0 {
		status = model.Partial
		notes = append(notes, fmt.Sprintf("pacing %s not exported (JMeter needs a Constant Throughput / Precise Throughput Timer)", wl.Pacing.D()))
	}
	if wl.ThinkTimeScale != nil && *wl.ThinkTimeScale != 1 {
		status = model.Partial
		notes = append(notes, fmt.Sprintf("thinkTimeScale %g not applied: timers are written unscaled", *wl.ThinkTimeScale))
	}
	if wl.GracefulStop > 0 {
		notes = append(notes, "gracefulStop has no JMeter equivalent (ignored)")
	}
	ex.item("workload", wl.Executor, "TestPlan/Thread Group", status, strings.Join(notes, "; "))
	w.element("ThreadGroup", "ThreadGroupGui", "Thread Group", true, func() {
		ex.tgProps(threads, ramp, loops, sched, dur)
	}, func() {
		if len(s.Setup) > 0 {
			ex.item("setup", "", "TestPlan/Thread Group/Load Meter setup", model.Partial,
				"setup steps written to a Once Only Controller (once per thread, inside the first iteration and its timing)")
			w.element("OnceOnlyController", "OnceOnlyControllerGui", "Load Meter setup", true, nil,
				func() { ex.writeSteps(s.Setup, "setup") })
		}
		ex.writeSteps(s.Steps, "steps")
	})
}

func (ex *exporter) csv(ds model.DataSource) {
	w := ex.w
	loc := "TestPlan/" + ds.Name
	status := model.Supported
	var notes []string
	file := ds.File
	if file == "" {
		file = safeFileName(ds.Name) + ".csv"
		status = model.Partial
		notes = append(notes, fmt.Sprintf("inline rows: JMeter needs a file; write the %d inline row(s) to %s next to the .jmx", len(ds.Rows), file))
		ex.rep.Manual = append(ex.rep.Manual, fmt.Sprintf("Create %s with the %d inline rows of data source %q", file, len(ds.Rows), ds.Name))
	}
	delim := ds.Delimiter
	if delim == "" {
		delim = ","
	}
	recycle, stop := true, false
	switch ds.OnExhaust {
	case "", "recycle":
	case "stop":
		recycle, stop = false, true
	case "error":
		recycle = false
		status = model.Partial
		notes = append(notes, "onExhaust=error: JMeter continues with <EOF> values instead of failing the iteration")
	}
	share := "shareMode.all"
	switch ds.Mode {
	case "", "sequential":
	case "per-vu":
		share = "shareMode.thread"
		status = model.Partial
		notes = append(notes, "per-vu: JMeter shareMode.thread makes every thread read the whole file from line 1 (not row = VU index)")
	case "random":
		status = model.Partial
		notes = append(notes, "random mode not supported by CSV Data Set: written as sequential")
	case "unique":
		recycle, stop = false, true
		status = model.Partial
		notes = append(notes, "unique mode approximated as sequential, no recycle, stop thread on EOF")
	}
	if ds.Scope == "vu" {
		status = model.Partial
		notes = append(notes, "scope=vu: JMeter reads a new line every iteration")
	}
	if ds.Sensitive {
		notes = append(notes, "sensitive flag has no JMeter equivalent (values appear in JMeter results)")
	}
	ex.item("dataSource", ds.Name, loc, status, strings.Join(notes, "; "))
	vn := ds.Columns
	w.element("CSVDataSet", "TestBeanGUI", ds.Name, true, func() {
		if delim == "\t" {
			w.str("delimiter", `\t`)
		} else {
			w.str("delimiter", delim)
		}
		w.str("fileEncoding", "UTF-8")
		w.str("filename", file)
		w.boolp("ignoreFirstLine", false)
		w.boolp("quotedData", true)
		w.boolp("recycle", recycle)
		w.str("shareMode", share)
		w.boolp("stopThread", stop)
		w.str("variableNames", strings.Join(vn, delim))
	}, nil)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFileName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if s == "" {
		return "data"
	}
	return s
}

func (ex *exporter) headerManager(name string, hs []model.Header, loc string) {
	w := ex.w
	w.element("HeaderManager", "HeaderPanel", name, true, func() {
		w.open("collectionProp", "name", "HeaderManager.headers")
		for _, h := range hs {
			w.open("elementProp", "name", "", "elementType", "Header")
			w.str("Header.name", h.Name)
			w.str("Header.value", ex.fn(h.Value, loc))
			w.close("elementProp")
		}
		w.close("collectionProp")
	}, nil)
}

func stepName(st model.Step, def string) string {
	if strings.TrimSpace(st.Name) != "" {
		return st.Name
	}
	return def
}

func (ex *exporter) writeSteps(steps []model.Step, loc string) {
	for i := 0; i < len(steps); i++ {
		st := steps[i]
		if st.Type == model.StepThink && !st.Disabled {
			j := i
			for j < len(steps) && steps[j].Type == model.StepThink && !steps[j].Disabled {
				j++
			}
			if j < len(steps) && steps[j].Type == model.StepRequest && !steps[j].Disabled && steps[j].Request != nil {
				ex.request(steps[j], steps[i:j], loc)
				i = j
				continue
			}
		}
		ex.step(st, loc)
	}
}

func (ex *exporter) commentProp(st model.Step) {
	if st.Comment != "" {
		ex.w.str("TestPlan.comments", st.Comment)
	}
}

func (ex *exporter) step(st model.Step, loc string) {
	w := ex.w
	en := !st.Disabled
	dis := ""
	if st.Disabled {
		dis = "; disabled (enabled=\"false\")"
	}
	switch st.Type {
	case model.StepRequest:
		ex.request(st, nil, loc)
	case model.StepThink:
		name := stepName(st, "Think Time")
		l := loc + "/" + name
		if st.Think == nil {
			ex.item("think", name, l, model.Unsupported, "think step without duration")
			return
		}
		if d := st.Think.Distribution; d != nil && d.Type != model.DistUniform {
			ex.item("think", name, l, model.Partial, "think-time distribution is not exported; min/max are used"+dis)
		} else {
			ex.item("think", name, l, model.Supported, "Flow Control Action (pause 0) carrying the timer"+dis)
		}
		w.element("TestAction", "TestActionGui", name, en, func() {
			w.intp("ActionProcessor.action", 1)
			w.intp("ActionProcessor.target", 0)
			w.str("ActionProcessor.duration", "0")
			ex.commentProp(st)
		}, func() { ex.timer(st, l) })
	case model.StepSetVar:
		name := stepName(st, "Set variables")
		l := loc + "/" + name
		ex.item("set", name, l, model.Supported, "Flow Control Action (pause 0) with a User Parameters pre-processor"+dis)
		w.element("TestAction", "TestActionGui", name, en, func() {
			w.intp("ActionProcessor.action", 1)
			w.intp("ActionProcessor.target", 0)
			w.str("ActionProcessor.duration", "0")
			ex.commentProp(st)
		}, func() {
			w.element("UserParameters", "UserParametersGui", name, true, func() {
				w.open("collectionProp", "name", "UserParameters.names")
				for _, kv := range st.Set {
					w.str(javaHash(kv.Name), kv.Name)
				}
				w.close("collectionProp")
				w.open("collectionProp", "name", "UserParameters.thread_values")
				w.open("collectionProp", "name", "0")
				for _, kv := range st.Set {
					v := ex.fn(kv.Value, l)
					w.str(javaHash(v), v)
				}
				w.close("collectionProp")
				w.close("collectionProp")
				w.boolp("UserParameters.per_iteration", false)
			}, nil)
		})
	case model.StepTransaction:
		name := stepName(st, "Transaction")
		l := loc + "/" + name
		ex.item("transaction", name, l, model.Supported, "Transaction Controller (timers excluded from its time, like Load Meter think time)"+dis)
		w.element("TransactionController", "TransactionControllerGui", name, en, func() {
			w.boolp("TransactionController.includeTimers", false)
			w.boolp("TransactionController.parent", false)
			ex.commentProp(st)
		}, func() { ex.writeSteps(st.Steps, l) })
	case model.StepLoop:
		name := stepName(st, "Loop Controller")
		l := loc + "/" + name
		ex.item("loop", name, l, model.Supported, fmt.Sprintf("Loop Controller x%d", st.Count)+dis)
		w.element("LoopController", "LoopControlPanel", name, en, func() {
			w.boolp("LoopController.continue_forever", true)
			w.str("LoopController.loops", strconv.Itoa(st.Count))
			ex.commentProp(st)
		}, func() { ex.writeSteps(st.Steps, l) })
	case model.StepIf:
		name := stepName(st, "If Controller")
		l := loc + "/" + name
		cond, ok := exportCond(ex.fn(st.Cond, l))
		status, note := model.Supported, fmt.Sprintf("If Controller %q", cond)
		if !ok {
			status, note = model.Unsupported, fmt.Sprintf("condition %q not convertible to JEXL; written verbatim (JMeter will evaluate it as JavaScript)", st.Cond)
			cond = ex.fn(st.Cond, l)
		}
		ex.item("if", name, l, status, note+dis)
		w.element("IfController", "IfControllerPanel", name, en, func() {
			w.str("IfController.condition", cond)
			w.boolp("IfController.evaluateAll", false)
			w.boolp("IfController.useExpression", ok)
			ex.commentProp(st)
		}, func() { ex.writeSteps(st.Steps, l) })
	case model.StepGroup:
		name := stepName(st, "Simple Controller")
		l := loc + "/" + name
		if st.Select != "" {
			ex.item("group", name, l, model.Partial, "weighted selection is not exported (Simple Controller runs every child; use a Random Controller by hand)"+dis)
		} else {
			ex.item("group", name, l, model.Supported, "Simple Controller"+dis)
		}
		w.element("GenericController", "LogicControllerGui", name, en, func() { ex.commentProp(st) },
			func() { ex.writeSteps(st.Steps, l) })
	default:
		ex.item(st.Type, st.Name, loc+"/"+st.Name, model.Unsupported, "unknown step type; not exported")
	}
}

var (
	condCmp = regexp.MustCompile(`^\$\{([^{}]+)\}\s*(==|!=)\s*(.*)$`)
	condVar = regexp.MustCompile(`^\$\{([^{}]+)\}$`)
)

// exportCond converts the Load Meter condition forms into JMeter If Controller expressions.
func exportCond(c string) (string, bool) {
	c = strings.TrimSpace(c)
	if m := condCmp.FindStringSubmatch(c); m != nil {
		v := strings.Trim(strings.TrimSpace(m[3]), `"'`)
		if strings.ContainsAny(v, `"\`) {
			return "", false
		}
		v = strings.NewReplacer(",", `\,`, "(", `\(`, ")", `\)`).Replace(v)
		return fmt.Sprintf(`${__jexl3("${%s}" %s "%s")}`, m[1], m[2], v), true
	}
	if m := condVar.FindStringSubmatch(c); m != nil && !strings.HasPrefix(m[1], "__") {
		return fmt.Sprintf(`${__groovy(vars.get("%s") != null && vars.get("%s") != "")}`, m[1], m[1]), true
	}
	return "", false
}

func (ex *exporter) timer(st model.Step, loc string) {
	w := ex.w
	th := st.Think
	name := stepName(st, "Think Time")
	minMS, maxMS := float64(th.Min.D())/1e6, float64(th.Max.D())/1e6
	status, note := model.Supported, ""
	if minMS != math.Trunc(minMS) || maxMS != math.Trunc(maxMS) {
		status, note = model.Partial, "sub-millisecond think time rounded"
	}
	if maxMS > minMS {
		ex.item("think", name, loc+"/"+name, status, joinNote("Uniform Random Timer", note))
		w.element("UniformRandomTimer", "UniformRandomTimerGui", name, true, func() {
			w.str("ConstantTimer.delay", strconv.FormatInt(int64(math.Round(minMS)), 10))
			w.str("RandomTimer.range", strconv.FormatFloat(math.Round(maxMS-minMS), 'f', 1, 64))
			ex.commentProp(st)
		}, nil)
		return
	}
	ex.item("think", name, loc+"/"+name, status, joinNote("Constant Timer", note))
	w.element("ConstantTimer", "ConstantTimerGui", name, true, func() {
		w.str("ConstantTimer.delay", strconv.FormatInt(int64(math.Round(minMS)), 10))
		ex.commentProp(st)
	}, nil)
}

type argOut struct {
	name, value, contentType string
	encode                   bool
}

func (ex *exporter) argsProp(args []argOut) {
	w := ex.w
	w.open("elementProp", "name", "HTTPsampler.Arguments", "elementType", "Arguments", "guiclass", "HTTPArgumentsPanel",
		"testclass", "Arguments", "testname", "User Defined Variables", "enabled", "true")
	if len(args) == 0 {
		w.leaf("collectionProp", "", "name", "Arguments.arguments")
	} else {
		w.open("collectionProp", "name", "Arguments.arguments")
		for _, a := range args {
			w.open("elementProp", "name", a.name, "elementType", "HTTPArgument")
			w.boolp("HTTPArgument.always_encode", a.encode)
			w.str("Argument.value", a.value)
			w.str("Argument.metadata", "=")
			w.boolp("HTTPArgument.use_equals", true)
			if a.name != "" {
				w.str("Argument.name", a.name)
			}
			if a.contentType != "" {
				w.str("HTTPArgument.content_type", a.contentType)
			}
			w.close("elementProp")
		}
		w.close("collectionProp")
	}
	w.close("elementProp")
}

func (ex *exporter) request(st model.Step, thinks []model.Step, loc string) {
	w := ex.w
	r := st.Request
	if r == nil {
		ex.item("request", st.Name, loc+"/"+st.Name, model.Unsupported, "request step without request")
		return
	}
	method := strings.ToUpper(r.Method)
	if method == "" {
		method = "GET"
	}
	name := stepName(st, method+" "+r.URL)
	l := loc + "/" + name
	status := model.Supported
	var notes []string
	u := ex.fn(r.URL, l)
	proto, domain, port, p := splitURL(u)
	if domain == "" && ex.basePath != "" {
		p = ex.basePath + "/" + strings.TrimLeft(p, "/")
	}
	follow := r.FollowRedirects
	if follow == nil {
		follow = ex.defFol
	}
	f := follow == nil || *follow
	raw := r.Body != ""
	mp := len(r.Multipart) > 0
	var args []argOut
	var files []model.Part
	switch {
	case raw:
		args = []argOut{{value: ex.fn(r.Body, l)}}
		if len(r.Form) > 0 || mp {
			status = model.Partial
			notes = append(notes, "a JMeter sampler has either a raw body or parameters: form/multipart fields dropped")
		}
	case mp:
		for _, part := range r.Multipart {
			if part.File != "" {
				files = append(files, part)
				continue
			}
			args = append(args, argOut{name: ex.fn(part.Name, l), value: ex.fn(part.Value, l), contentType: part.ContentType, encode: false})
		}
		if len(r.Form) > 0 {
			status = model.Partial
			notes = append(notes, "form fields dropped in favour of multipart parts")
		}
	default:
		for _, kv := range r.Form {
			args = append(args, argOut{name: ex.fn(kv.Name, l), value: ex.fn(kv.Value, l), encode: true})
		}
	}
	if r.Timeout > 0 {
		notes = append(notes, "timeout written as response (read) timeout")
	}
	if st.Disabled {
		notes = append(notes, `disabled (enabled="false")`)
	}
	ex.item("request", name, l, status, strings.Join(notes, "; "))
	w.element("HTTPSamplerProxy", "HttpTestSampleGui", name, !st.Disabled, func() {
		if raw {
			w.boolp("HTTPSampler.postBodyRaw", true)
		}
		ex.argsProp(args)
		if len(files) > 0 {
			w.open("elementProp", "name", "HTTPsampler.Files", "elementType", "HTTPFileArgs")
			w.open("collectionProp", "name", "HTTPFileArgs.files")
			for _, fp := range files {
				path := ex.fn(fp.File, l)
				w.open("elementProp", "name", path, "elementType", "HTTPFileArg")
				w.str("File.mimetype", fp.ContentType)
				w.str("File.path", path)
				w.str("File.paramname", ex.fn(fp.Name, l))
				w.close("elementProp")
			}
			w.close("collectionProp")
			w.close("elementProp")
		}
		w.str("HTTPSampler.domain", domain)
		w.str("HTTPSampler.port", port)
		w.str("HTTPSampler.protocol", proto)
		w.str("HTTPSampler.contentEncoding", "")
		w.str("HTTPSampler.path", p)
		w.str("HTTPSampler.method", method)
		w.boolp("HTTPSampler.follow_redirects", f)
		w.boolp("HTTPSampler.auto_redirects", false)
		w.boolp("HTTPSampler.use_keepalive", true)
		w.boolp("HTTPSampler.DO_MULTIPART_POST", mp && !raw)
		w.str("HTTPSampler.embedded_url_re", "")
		w.str("HTTPSampler.connect_timeout", "")
		if r.Timeout > 0 {
			w.str("HTTPSampler.response_timeout", strconv.FormatInt(durMS(r.Timeout), 10))
		} else {
			w.str("HTTPSampler.response_timeout", "")
		}
		ex.commentProp(st)
	}, func() {
		for _, t := range thinks {
			if t.Think != nil {
				ex.timer(t, l)
			} else {
				ex.item("think", t.Name, l, model.Unsupported, "think step without duration")
			}
		}
		if len(r.Headers) > 0 {
			ex.headerManager("HTTP Header Manager", r.Headers, l)
		}
		for _, e := range r.Extract {
			ex.extractor(e, l)
		}
		for _, a := range r.Assert {
			ex.assertion(a, l)
		}
	})
}

func jmMatch(m int) string {
	switch {
	case m == 0:
		return "1"
	case m < 0:
		return "0"
	}
	return strconv.Itoa(m)
}

func (ex *exporter) regexEl(name, useHeaders, re, tpl, def, match string) {
	w := ex.w
	w.element("RegexExtractor", "RegexExtractorGui", name, true, func() {
		w.str("RegexExtractor.useHeaders", useHeaders)
		w.str("RegexExtractor.refname", name)
		w.str("RegexExtractor.regex", re)
		w.str("RegexExtractor.template", tpl)
		w.str("RegexExtractor.default", def)
		w.boolp("RegexExtractor.default_empty_value", false)
		w.str("RegexExtractor.match_number", match)
	}, nil)
}

func (ex *exporter) extractor(e model.Extractor, loc string) {
	w := ex.w
	l := loc + "/" + e.Name
	status, note := model.Supported, ""
	if e.Required {
		status, note = model.Partial, "required=true not representable: JMeter does not fail the sample when nothing is extracted (add an assertion on the default value)"
	}
	def := ex.fn(e.Default, l)
	switch e.Type {
	case model.ExRegex:
		expr := ex.fn(e.Expr, l)
		g := e.Group
		if g == 0 {
			if c, err := regexp.Compile(varRefRE.ReplaceAllString(e.Expr, "x")); err == nil && c.NumSubexp() > 0 {
				g = 1
			}
		}
		ex.item("extract.regex", e.Name, l, status, joinNote("Regular Expression Extractor (Go RE2 pattern written as-is; JMeter uses Perl5 syntax)", note))
		ex.regexEl(e.Name, "false", expr, fmt.Sprintf("$%d$", g), def, jmMatch(e.Match))
	case model.ExJSONPath:
		ex.item("extract.jsonpath", e.Name, l, status, joinNote("JSON Extractor", note))
		w.element("JSONPostProcessor", "JSONPostProcessorGui", e.Name, true, func() {
			w.str("JSONPostProcessor.referenceNames", e.Name)
			w.str("JSONPostProcessor.jsonPathExprs", ex.fn(e.Expr, l))
			w.str("JSONPostProcessor.match_numbers", jmMatch(e.Match))
			w.str("JSONPostProcessor.defaultValues", def)
		}, nil)
	case model.ExBoundary:
		ex.item("extract.boundary", e.Name, l, status, joinNote("Boundary Extractor", note))
		w.element("BoundaryExtractor", "BoundaryExtractorGui", e.Name, true, func() {
			w.str("BoundaryExtractor.useHeaders", "false")
			w.str("BoundaryExtractor.refname", e.Name)
			w.str("BoundaryExtractor.lboundary", ex.fn(e.Left, l))
			w.str("BoundaryExtractor.rboundary", ex.fn(e.Right, l))
			w.str("BoundaryExtractor.default", def)
			w.boolp("BoundaryExtractor.default_empty_value", false)
			w.str("BoundaryExtractor.match_number", jmMatch(e.Match))
		}, nil)
	case model.ExXPath:
		ex.item("extract.xpath", e.Name, l, model.Partial, joinNote("XPath2 Extractor: XPath dialects may differ; verify", note))
		w.element("XPath2Extractor", "XPath2ExtractorGui", e.Name, true, func() {
			w.str("XPathExtractor2.default", def)
			w.str("XPathExtractor2.refname", e.Name)
			w.str("XPathExtractor2.matchNumber", jmMatch(e.Match))
			w.str("XPathExtractor2.xpathQuery", ex.fn(e.Expr, l))
			w.boolp("XPathExtractor2.fragment", false)
			w.str("XPathExtractor2.namespaces", "")
		}, nil)
	case model.ExHeader:
		ex.item("extract.header", e.Name, l, status, joinNote("Regular Expression Extractor on response headers (case-insensitive header name)", note))
		ex.regexEl(e.Name, "true", hdrPrefix+regexp.QuoteMeta(e.Expr)+hdrSuffix, "$1$", def, jmMatch(e.Match))
	case model.ExCookie:
		ex.item("extract.cookie", e.Name, l, model.Partial, joinNote("Regular Expression Extractor on Set-Cookie headers of this response only (Load Meter also falls back to the cookie jar)", note))
		ex.regexEl(e.Name, "true", cookiePrefix+regexp.QuoteMeta(e.Expr)+cookieSuffix, "$1$", def, jmMatch(e.Match))
	case model.ExStatus:
		ex.item("extract.status", e.Name, l, status, joinNote("Regular Expression Extractor on the response code", note))
		ex.regexEl(e.Name, "code", "(.*)", "$1$", def, "1")
	default:
		ex.item("extract."+e.Type, e.Name, l, model.Unsupported, "unknown extractor type; not exported")
	}
}

func (ex *exporter) responseAssertion(name, field string, testType int, pats []string) {
	w := ex.w
	w.element("ResponseAssertion", "AssertionGui", name, true, func() {
		w.open("collectionProp", "name", "Asserion.test_strings")
		for _, p := range pats {
			w.str(javaHash(p), p)
		}
		w.close("collectionProp")
		w.str("Assertion.custom_message", "")
		w.str("Assertion.test_field", field)
		w.boolp("Assertion.assume_success", false)
		w.intp("Assertion.test_type", testType)
	}, nil)
}

func (ex *exporter) assertion(a model.Assertion, loc string) {
	w := ex.w
	l := loc + "/assert " + a.Type
	val := ex.fn(a.Value, l)
	switch a.Type {
	case model.AsStatus:
		var pats []string
		classes := false
		for _, v := range strings.Split(val, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if len(v) == 3 && strings.HasSuffix(strings.ToLower(v), "xx") {
				pats = append(pats, v[:1]+`\d\d`)
				classes = true
				continue
			}
			pats = append(pats, v)
		}
		if len(pats) == 0 {
			ex.item("assert.status", "", l, model.Unsupported, "empty status list")
			return
		}
		tt := 8
		if classes {
			tt = 1
		}
		if len(pats) > 1 {
			tt |= 32
		}
		ex.item("assert.status", val, l, model.Supported, "Response Assertion on the response code")
		ex.responseAssertion("Status "+val, "Assertion.response_code", tt, pats)
	case model.AsContains:
		ex.item("assert.contains", val, l, model.Supported, "Response Assertion (Substring)")
		ex.responseAssertion("Contains", "Assertion.response_data", 16, []string{val})
	case model.AsNotContains:
		ex.item("assert.not_contains", val, l, model.Supported, "Response Assertion (Substring, NOT)")
		ex.responseAssertion("Does not contain", "Assertion.response_data", 20, []string{val})
	case model.AsRegex:
		ex.item("assert.regex", a.Expr, l, model.Supported, "Response Assertion (Contains = regex find; Go RE2 pattern written as-is)")
		ex.responseAssertion("Matches regex", "Assertion.response_data", 2, []string{ex.fn(a.Expr, l)})
	case model.AsJSONPath:
		ex.item("assert.jsonpath", a.Expr, l, model.Supported, "JSON Assertion")
		w.element("JSONPathAssertion", "JSONPathAssertionGui", "JSON "+a.Expr, true, func() {
			w.str("JSON_PATH", ex.fn(a.Expr, l))
			w.str("EXPECTED_VALUE", val)
			w.boolp("JSONVALIDATION", a.Value != "")
			w.boolp("EXPECT_NULL", false)
			w.boolp("INVERT", false)
			w.boolp("ISREGEX", false)
		}, nil)
	case model.AsMaxLatency:
		d, err := time.ParseDuration(strings.TrimSpace(a.Value))
		if err != nil || d <= 0 {
			ex.item("assert.max_latency", a.Value, l, model.Unsupported, "value is not a positive duration")
			return
		}
		ex.item("assert.max_latency", a.Value, l, model.Supported, "Duration Assertion")
		w.element("DurationAssertion", "DurationAssertionGui", "Max latency "+a.Value, true, func() {
			w.str("DurationAssertion.duration", strconv.FormatInt(d.Milliseconds(), 10))
		}, nil)
	case model.AsHeader:
		pat := a.Expr + ": " + val
		if val == "" {
			pat = a.Expr + ":"
		}
		ex.item("assert.header", a.Expr, l, model.Partial, "Response Assertion (Substring) on the raw response headers: case-sensitive, matches across the header block")
		ex.responseAssertion("Header "+a.Expr, "Assertion.response_headers", 16, []string{pat})
	default:
		ex.item("assert."+a.Type, "", l, model.Unsupported, "unknown assertion type; not exported")
	}
}
