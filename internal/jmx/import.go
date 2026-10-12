package jmx

import (
	"fmt"
	"io"
	"math"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// elem is a JMeter test element paired with the children held in the
// <hashTree> sibling that follows it.
type elem struct {
	n    *node
	kids []elem
}

func pairs(ht *node) []elem {
	if ht == nil {
		return nil
	}
	var out []elem
	cs := ht.Children
	for i := 0; i < len(cs); i++ {
		c := cs[i]
		if c.Name == "hashTree" {
			continue // orphan tree: nothing to attach it to
		}
		e := elem{n: c}
		if i+1 < len(cs) && cs[i+1].Name == "hashTree" {
			e.kids = pairs(cs[i+1])
			i++
		}
		out = append(out, e)
	}
	return out
}

func testClass(n *node) string {
	if tc := strings.TrimSpace(n.Attr("testclass")); tc != "" {
		return tc
	}
	return n.Name
}

func nameOf(n *node) string {
	if s := strings.TrimSpace(n.Attr("testname")); s != "" {
		return s
	}
	return testClass(n)
}

func enabled(n *node) bool { return strings.TrimSpace(n.Attr("enabled")) != "false" }

func countDesc(kids []elem) int {
	c := 0
	for _, k := range kids {
		c += 1 + countDesc(k.kids)
	}
	return c
}

func comment(n *node) string { return strings.TrimSpace(n.str("TestPlan.comments")) }

type httpDefaults struct {
	protocol, domain, port, path string
	connectMS, responseMS        int64 // -1 = unset
	scenario                     bool  // mapped to Scenario.Defaults
}

type stepRef struct {
	step model.Step
	item int
}

type exRef struct {
	ex   []model.Extractor
	item int
}

type asRef struct {
	as   []model.Assertion
	item int
}

// scope models JMeter's scoping rules: config elements, timers, pre/post
// processors and assertions apply to every sampler at or below their level.
type scope struct {
	parent      *scope
	level       string // plan | tg | ctrl | sampler
	headers     []model.Header
	headerItems []int
	defaults    *httpDefaults
	pre         []stepRef
	timers      []stepRef
	extracts    []exRef
	asserts     []asRef
}

func (s *scope) chain() []*scope {
	var c []*scope
	for x := s; x != nil; x = x.parent {
		c = append(c, x)
	}
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
	return c
}

type importer struct {
	sc        *model.Scenario
	rep       *model.CompatReport
	seen      map[string]bool
	applied   map[int]int
	scoped    []int
	perIter   map[int]bool
	container string // name of the enclosing non-executed container, if any
	inMainTG  bool
	inSetup   bool
	loopDepth int
	cookies   bool
	scen      httpDefaults
	dsNames   map[string]bool
}

var rank = map[string]int{model.Supported: 0, model.Ignored: 1, model.Partial: 2, model.Unsupported: 3}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func (im *importer) add(element, name, loc, status, note string) int {
	if im.container != "" && status != model.Ignored {
		note = joinNote(note, fmt.Sprintf("inside non-executed container %q", im.container))
	}
	im.rep.Add(element, name, loc, status, note)
	return len(im.rep.Items) - 1
}

// escalate raises an item's status (never lowers it) and appends a note.
func (im *importer) escalate(idx int, status, note string) {
	it := &im.rep.Items[idx]
	if rank[status] > rank[it.Status] {
		it.Status = status
	}
	it.Note = joinNote(it.Note, note)
}

func (im *importer) note(idx int, note string) { im.escalate(idx, model.Supported, note) }

func (im *importer) manual(format string, a ...any) {
	im.rep.Manual = append(im.rep.Manual, fmt.Sprintf(format, a...))
}

// conv maps JMeter functions in s and reports unmappable ones at loc.
func (im *importer) conv(s, loc string) string {
	if !strings.Contains(s, "${__") {
		return s
	}
	out, issues := importFuncs(s, im.sc.Variables)
	for _, is := range issues {
		key := loc + "|" + is.Fn + "|" + is.Note
		if im.seen[key] {
			continue
		}
		im.seen[key] = true
		im.add("function "+is.Fn, "", loc, is.Status, is.Note)
	}
	return out
}

// Import parses a JMeter 5.x .jmx test plan into a Load Meter scenario. The
// returned report lists how every element was handled.
func Import(r io.Reader, sourceName string) (*model.Scenario, *model.CompatReport, error) {
	root, err := parseTree(r)
	if err != nil {
		return nil, nil, err
	}
	if root.Name != "jmeterTestPlan" {
		return nil, nil, fmt.Errorf("jmx: root element is <%s>, expected <jmeterTestPlan>", root.Name)
	}
	top := pairs(root.child("hashTree"))
	var plan *elem
	for i := range top {
		if testClass(top[i].n) == "TestPlan" {
			plan = &top[i]
			break
		}
	}
	if plan == nil {
		return nil, nil, fmt.Errorf("jmx: no <TestPlan> element found")
	}
	sc := model.NewScenario(nameOf(plan.n))
	sc.Metadata = map[string]string{"importer": "jmx"}
	if sourceName != "" {
		sc.Metadata["source"] = sourceName
	}
	if v := root.Attr("jmeter"); v != "" {
		sc.Metadata["jmeterVersion"] = v
	}
	rep := &model.CompatReport{Source: sourceName, Format: "jmx"}
	im := &importer{sc: sc, rep: rep, seen: map[string]bool{}, applied: map[int]int{},
		perIter: map[int]bool{}, dsNames: map[string]bool{},
		scen: httpDefaults{connectMS: -1, responseMS: -1}}
	for i := range top {
		if &top[i] != plan {
			im.add(testClass(top[i].n), nameOf(top[i].n), nameOf(top[i].n), model.Ignored,
				"element outside the Test Plan; JMeter does not execute it")
		}
	}
	im.importPlan(*plan)
	im.finish()
	rep.ComputeConfidence()
	return sc, rep, nil
}

func isThreadGroup(tc string) bool {
	switch tc {
	case "ThreadGroup", "SetupThreadGroup", "PostThreadGroup":
		return true
	}
	return strings.HasSuffix(tc, "ThreadGroup")
}

const (
	tcUTG = "kg.apc.jmeter.threads.UltimateThreadGroup"
	tcCTG = "com.blazemeter.jmeter.threads.concurrency.ConcurrencyThreadGroup"
)

func isLoadTG(tc string) bool { return tc == "ThreadGroup" || tc == tcUTG || tc == tcCTG }

func (im *importer) importPlan(plan elem) {
	const loc = "TestPlan"
	n := plan.n
	idx := im.add("TestPlan", nameOf(n), loc, model.Supported, "")
	im.sc.Description = comment(n)
	if udv := n.elemProp("TestPlan.user_defined_variables"); udv != nil {
		im.loadArguments(udv, loc)
	}
	if n.boolean("TestPlan.serialize_threadgroups", false) {
		im.note(idx, "\"Run Thread Groups consecutively\" is set; Load Meter imports a single load thread group")
	}
	if n.boolean("TestPlan.functional_mode", false) {
		im.note(idx, "functional test mode (save response data) ignored; Load Meter reports replace it")
	}
	planScope := &scope{level: "plan"}
	var tgs, others []elem
	for _, k := range plan.kids {
		l := loc + "/" + nameOf(k.n)
		if !enabled(k.n) {
			im.disabled(k, l)
			continue
		}
		if isThreadGroup(testClass(k.n)) {
			tgs = append(tgs, k)
			continue
		}
		if im.scopedElement(k, planScope, l) {
			continue
		}
		others = append(others, k)
	}
	main := -1
	for i, t := range tgs {
		if isLoadTG(testClass(t.n)) {
			main = i
			break
		}
	}
	// Scenario-level HTTP defaults may also live directly under the main thread group.
	if main >= 0 {
		for _, k := range tgs[main].kids {
			if enabled(k.n) && isHTTPDefaults(k.n) {
				im.mergeScenarioDefaults(im.readDefaults(k.n))
			}
		}
	}
	for i, t := range tgs {
		tc := testClass(t.n)
		name := nameOf(t.n)
		l := loc + "/" + name
		switch {
		case tc == "SetupThreadGroup":
			im.add(tc, name, l, model.Partial, fmt.Sprintf(
				"setUp Thread Group (%s thread(s), runs once before the load in JMeter) mapped to scenario setup steps; Load Meter runs setup once per VU, not once with its own thread pool",
				orDefault(strings.TrimSpace(t.n.str("ThreadGroup.num_threads")), "1")))
			im.inSetup = true
			steps := im.buildLevel(t.kids, &scope{parent: planScope, level: "tg"}, l)
			im.inSetup = false
			im.sc.Setup = append(im.sc.Setup, steps...)
		case tc == "PostThreadGroup":
			im.add(tc, name, l, model.Partial, fmt.Sprintf(
				"tearDown Thread Group (%s thread(s), runs once after the load in JMeter) mapped to scenario teardown steps; Load Meter runs teardown per VU",
				orDefault(strings.TrimSpace(t.n.str("ThreadGroup.num_threads")), "1")))
			im.inSetup = true
			steps := im.buildLevel(t.kids, &scope{parent: planScope, level: "tg"}, l)
			im.inSetup = false
			im.sc.Teardown = append(im.sc.Teardown, steps...)
		case i == main:
			var wl model.Workload
			var notes []string
			status := model.Supported
			switch tc {
			case tcUTG:
				wl, notes = im.utgWorkload(t.n, l)
				status = model.Partial
			case tcCTG:
				wl, notes = im.ctgWorkload(t.n, l)
				status = model.Partial
			default:
				var partial bool
				wl, notes, partial = im.tgWorkload(t.n, l)
				if partial {
					status = model.Partial
				}
			}
			im.add(tc, name, l, status, strings.Join(notes, "; "))
			im.sc.Workload = wl
			im.sc.Metadata["threadGroup"] = name
			im.inMainTG = true
			steps := im.buildLevel(t.kids, &scope{parent: planScope, level: "tg"}, l)
			im.inMainTG = false
			im.sc.Steps = append(im.sc.Steps, steps...)
		default:
			status, why := model.Partial, "only one load thread group per Load Meter scenario"
			if !isLoadTG(tc) {
				status, why = model.Unsupported, "thread group type "+tc+" has no Load Meter workload mapping"
			}
			note := why + fmt.Sprintf("; %q imported as a disabled group so nothing is lost (split it into its own scenario to run it)", name)
			im.add(tc, name, l, status, note)
			im.manual("Thread group %q was not imported as load: %s", name, why)
			im.sc.Steps = append(im.sc.Steps, im.disabledGroup(t, planScope, l, note))
		}
	}
	if main < 0 {
		im.rep.Warnings = append(im.rep.Warnings, "no enabled Thread Group found; workload left at Load Meter defaults")
	}
	for _, k := range others {
		l := loc + "/" + nameOf(k.n)
		im.add(testClass(k.n), nameOf(k.n), l, model.Ignored, "directly under the Test Plan (outside any Thread Group); JMeter does not execute it")
		if len(k.kids) > 0 {
			im.drain(k.kids, planScope, l, nameOf(k.n))
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (im *importer) finish() {
	sort.Ints(im.scoped)
	last := -1
	for _, idx := range im.scoped {
		if idx == last {
			continue
		}
		last = idx
		switch c := im.applied[idx]; {
		case c == 0:
			im.note(idx, "no sampler in scope: had no effect")
		case c > 1:
			im.note(idx, fmt.Sprintf("applied to %d samplers in scope", c))
			if im.perIter[idx] {
				im.escalate(idx, model.Partial, "JMeter updates these once per iteration; Load Meter sets them before each sampler in scope")
			}
		}
	}
	d := &im.sc.Defaults
	if im.scen.domain != "" {
		d.BaseURL = composeBase(im.scen.protocol, im.scen.domain, im.scen.port)
	}
	if t := timeoutMS(im.scen.connectMS, im.scen.responseMS); t > 0 {
		d.Timeout = msDur(float64(t))
	}
	if !im.cookies {
		d.DisableCookies = true
		im.rep.Warnings = append(im.rep.Warnings, "no HTTP Cookie Manager in scope: JMeter ignores cookies, so Load Meter cookies were disabled to match")
	}
}

func timeoutMS(conn, resp int64) int64 {
	var t int64
	if conn > 0 {
		t += conn
	}
	if resp > 0 {
		t += resp
	}
	return t
}

func msDur(ms float64) model.Duration {
	return model.Duration(time.Duration(ms * float64(time.Millisecond)))
}

func composeBase(protocol, domain, port string) string {
	p := strings.ToLower(protocol)
	if p == "" {
		if port == "443" {
			p = "https"
		} else {
			p = "http"
		}
	}
	host := domain
	if port != "" && !(p == "http" && port == "80") && !(p == "https" && port == "443") {
		host += ":" + port
	}
	return p + "://" + host
}

// buildLevel imports one hashTree level: scoped elements first (they apply to
// every sampler at this level regardless of position), then samplers and
// controllers in document order.
func (im *importer) buildLevel(kids []elem, sc *scope, loc string) []model.Step {
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
	var steps []model.Step
	for _, k := range execs {
		steps = append(steps, im.execElement(k, sc, loc+"/"+nameOf(k.n))...)
	}
	return steps
}

func (im *importer) disabled(k elem, loc string) {
	note := `disabled in source (enabled="false"); not imported`
	if c := countDesc(k.kids); c > 0 {
		note += fmt.Sprintf(" together with %d nested element(s)", c)
	}
	im.add(testClass(k.n), nameOf(k.n), loc, model.Ignored, note)
}

// drain imports kids only so that each gets its CompatItem; steps are discarded.
func (im *importer) drain(kids []elem, sc *scope, loc, container string) {
	prev := im.container
	im.container = container
	im.buildLevel(kids, &scope{parent: sc, level: "sampler"}, loc)
	im.container = prev
}

// disabledGroup keeps the children of an unconvertible element as a disabled group.
func (im *importer) disabledGroup(k elem, sc *scope, loc, why string) model.Step {
	prev := im.container
	im.container = nameOf(k.n)
	steps := im.buildLevel(k.kids, &scope{parent: sc, level: "ctrl"}, loc)
	im.container = prev
	c := "not executed: " + why
	if cm := comment(k.n); cm != "" {
		c = cm + " | " + c
	}
	return model.Step{Type: model.StepGroup, Name: nameOf(k.n), Disabled: true, Comment: c, Steps: steps}
}

// ---------------------------------------------------------------------------
// Scoped (non-executable) elements

var listenerClasses = map[string]bool{"ResultCollector": true, "BackendListener": true, "ResultSaver": true,
	"Summariser": true, "MailerResultCollector": true, "ProxyControl": true, "WorkBench": true,
	"MirrorControl": true, "PropertyControlGui": true}

var scriptClasses = map[string]bool{"JSR223PreProcessor": true, "JSR223PostProcessor": true, "JSR223Assertion": true,
	"JSR223Timer": true, "JSR223Listener": true, "BeanShellPreProcessor": true, "BeanShellPostProcessor": true,
	"BeanShellAssertion": true, "BeanShellTimer": true, "BeanShellListener": true, "BSFPreProcessor": true,
	"BSFPostProcessor": true, "BSFAssertion": true, "BSFTimer": true, "BSFListener": true}

var unsupportedScoped = map[string]string{
	"AuthManager":               "HTTP Authorization Manager not supported (Load Meter model has no auth config); add an Authorization header instead",
	"DNSCacheManager":           "DNS Cache Manager not supported; Load Meter uses the system resolver",
	"KeystoreConfig":            "Keystore (client certificate) configuration not supported",
	"CounterConfig":             "Counter config element not supported; consider ${__counter()}",
	"RandomVariableConfig":      "Random Variable config element not supported; consider ${__randomInt(min,max)}",
	"JDBCDataSource":            "JDBC connection configuration not supported (non-HTTP)",
	"HtmlExtractor":             "CSS Selector Extractor not supported (Load Meter has no CSS extractor); rewrite as regex or boundary",
	"JMESPathExtractor":         "JMESPath extractor not supported; rewrite as jsonpath",
	"JMESPathAssertion":         "JMESPath assertion not supported; rewrite as jsonpath",
	"SizeAssertion":             "Size Assertion not supported",
	"XPathAssertion":            "XPath Assertion not supported",
	"XPath2Assertion":           "XPath2 Assertion not supported",
	"HTMLAssertion":             "HTML (tidy) Assertion not supported",
	"MD5HexAssertion":           "MD5Hex Assertion not supported",
	"XMLAssertion":              "XML well-formedness Assertion not supported",
	"XMLSchemaAssertion":        "XML Schema Assertion not supported",
	"CompareAssertion":          "Compare Assertion not supported",
	"SMIMEAssertionTestElement": "SMIME Assertion not supported",
	"RegExUserParameters":       "RegEx User Parameters not supported",
	"AnchorModifier":            "HTML Link Parser not supported",
	"URLRewritingModifier":      "HTTP URL Re-writing Modifier not supported; extract the session id with a regex extractor",
	"SampleTimeout":             "Sample Timeout pre-processor not supported; use the request timeout",
	"JDBCPreProcessor":          "JDBC PreProcessor not supported (non-HTTP)",
	"JDBCPostProcessor":         "JDBC PostProcessor not supported (non-HTTP)",
	"ResultAction":              "Result Status Action Handler not supported (Load Meter continues on failure)",
	"PreciseThroughputTimer":    "Precise Throughput Timer not supported; use a constant-arrival-rate workload",
	"SyncTimer":                 "Synchronizing Timer not supported (no rendezvous in Load Meter)",
	"LoginConfig":               "Login Config element not supported",
}

func isHTTPDefaults(n *node) bool {
	if testClass(n) != "ConfigTestElement" {
		return false
	}
	if n.Attr("guiclass") == "HttpDefaultsGui" {
		return true
	}
	_, ok := n.prop("HTTPSampler.domain")
	return ok
}

func (im *importer) scopedElement(k elem, sc *scope, loc string) bool {
	n := k.n
	tc := testClass(n)
	name := nameOf(n)
	switch {
	case tc == "HeaderManager":
		im.headerManager(k, sc, loc)
	case tc == "ConfigTestElement":
		if isHTTPDefaults(n) {
			im.httpDefaultsEl(k, sc, loc)
		} else {
			im.add(tc, name, loc, model.Unsupported, fmt.Sprintf("config element (guiclass %s) has no Load Meter equivalent", n.Attr("guiclass")))
		}
	case tc == "CookieManager":
		im.cookieManager(k, sc, loc)
	case tc == "CacheManager":
		im.add(tc, name, loc, model.Ignored, "HTTP Cache Manager ignored: Load Meter does not emulate a browser cache (every request is sent)")
	case tc == "CSVDataSet":
		im.csv(k, sc, loc)
	case tc == "Arguments":
		im.add(tc, name, loc, model.Supported, "User Defined Variables mapped to scenario variables (JMeter also evaluates them once, globally, at test start)")
		im.loadArguments(n, loc)
	case tc == "ConstantTimer", tc == "UniformRandomTimer", tc == "GaussianRandomTimer", tc == "PoissonRandomTimer":
		im.timer(k, sc, loc)
	case tc == "ConstantThroughputTimer":
		im.add(tc, name, loc, model.Partial, fmt.Sprintf(
			"Constant Throughput Timer (%s samples/min) not converted: use a constant-arrival-rate workload (iterations/s) instead",
			strings.TrimSpace(orDefault(n.str("throughput"), n.str("ConstantThroughputTimer.throughput")))))
		im.manual("Replace Constant Throughput Timer %q with a Load Meter arrival-rate executor", name)
	case tc == "ResponseAssertion", tc == "JSONPathAssertion", tc == "DurationAssertion":
		im.assertion(k, sc, loc)
	case tc == "RegexExtractor", tc == "JSONPostProcessor", tc == "BoundaryExtractor", tc == "XPathExtractor", tc == "XPath2Extractor":
		im.extractor(k, sc, loc)
	case tc == "UserParameters":
		im.userParams(k, sc, loc)
	case scriptClasses[tc]:
		note := scriptNote(n)
		im.add(tc, name, loc, model.Unsupported, note)
		im.manual("Re-implement %s %q at %s without scripting: %s", tc, name, loc, note)
	case tc == "DebugPostProcessor":
		im.add(tc, name, loc, model.Ignored, "debug element; Load Meter debug output replaces it")
	case unsupportedScoped[tc] != "":
		im.add(tc, name, loc, model.Unsupported, unsupportedScoped[tc])
		im.manual("%s %q at %s: %s", tc, name, loc, unsupportedScoped[tc])
	case listenerClasses[tc] || strings.HasSuffix(tc, "ResultCollector") || strings.HasSuffix(tc, "Visualizer"):
		im.add(tc, name, loc, model.Ignored, "UI/listener element; Load Meter reports replace it")
	default:
		return false
	}
	if len(k.kids) > 0 {
		im.drain(k.kids, sc, loc, name)
	}
	return true
}

func scriptNote(n *node) string {
	tc := testClass(n)
	lang := strings.TrimSpace(n.str("scriptLanguage"))
	if lang == "" {
		switch {
		case strings.HasPrefix(tc, "BeanShell"):
			lang = "beanshell"
		default:
			lang = "unknown language"
		}
	}
	note := fmt.Sprintf("%s script (%s) is never executed by Load Meter", tc, lang)
	if f := strings.TrimSpace(n.str("filename")); f != "" {
		note += fmt.Sprintf("; script file %q", f)
	}
	if ex := excerpt(n.str("script"), 80); ex != "" {
		note += fmt.Sprintf("; excerpt: %q", ex)
	}
	return note
}

func excerpt(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

func (im *importer) loadArguments(n *node, loc string) {
	args, _ := n.coll("Arguments.arguments")
	for _, a := range args {
		name := strings.TrimSpace(a.str("Argument.name"))
		if name == "" {
			continue
		}
		im.sc.Variables[name] = im.conv(a.str("Argument.value"), loc)
	}
}

func mergeHeaders(base, add []model.Header) []model.Header {
	out := append([]model.Header(nil), base...)
	for _, h := range add {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].Name, h.Name) {
				out[i] = h
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, h)
		}
	}
	return out
}

func (im *importer) headerManager(k elem, sc *scope, loc string) {
	n := k.n
	idx := im.add("HeaderManager", nameOf(n), loc, model.Supported, "")
	var hs []model.Header
	list, _ := n.coll("HeaderManager.headers")
	for _, h := range list {
		nm := strings.TrimSpace(h.str("Header.name"))
		if nm == "" {
			continue
		}
		hs = append(hs, model.Header{Name: nm, Value: im.conv(h.str("Header.value"), loc)})
	}
	if sc.level == "plan" {
		im.sc.Defaults.Headers = mergeHeaders(im.sc.Defaults.Headers, hs)
		im.note(idx, "Test Plan level: mapped to scenario default headers")
		return
	}
	sc.headers = mergeHeaders(sc.headers, hs)
	sc.headerItems = append(sc.headerItems, idx)
	im.scoped = append(im.scoped, idx)
}

func msOpt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return -1
	}
	return int64(f)
}

func (im *importer) readDefaults(n *node) *httpDefaults {
	g := func(k string) string { return strings.TrimSpace(n.str("HTTPSampler." + k)) }
	return &httpDefaults{protocol: g("protocol"), domain: g("domain"), port: g("port"), path: g("path"),
		connectMS: msOpt(g("connect_timeout")), responseMS: msOpt(g("response_timeout"))}
}

func (im *importer) mergeScenarioDefaults(d *httpDefaults) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&im.scen.protocol, d.protocol)
	set(&im.scen.domain, d.domain)
	set(&im.scen.port, d.port)
	set(&im.scen.path, d.path)
	if d.connectMS >= 0 {
		im.scen.connectMS = d.connectMS
	}
	if d.responseMS >= 0 {
		im.scen.responseMS = d.responseMS
	}
}

func (im *importer) httpDefaultsEl(k elem, sc *scope, loc string) {
	n := k.n
	idx := im.add("ConfigTestElement", nameOf(n), loc, model.Supported, "")
	d := im.readDefaults(n)
	d.scenario = im.container == "" && (sc.level == "plan" || (sc.level == "tg" && im.inMainTG))
	for _, f := range []*string{&d.protocol, &d.domain, &d.port, &d.path} {
		*f = im.conv(*f, loc)
	}
	if sc.defaults == nil {
		sc.defaults = d
	} else { // several defaults at one level: fill blanks
		for _, p := range [][2]*string{{&sc.defaults.protocol, &d.protocol}, {&sc.defaults.domain, &d.domain},
			{&sc.defaults.port, &d.port}, {&sc.defaults.path, &d.path}} {
			if *p[0] == "" {
				*p[0] = *p[1]
			}
		}
	}
	if d.scenario {
		if sc.level == "plan" {
			im.mergeScenarioDefaults(d) // thread-group level ones were merged up front
		}
		im.note(idx, "mapped to scenario defaults (baseUrl, timeout)")
		if d.responseMS > 0 || d.connectMS > 0 {
			im.timeoutNote(idx, d.connectMS, d.responseMS)
		}
	} else {
		im.note(idx, "nested HTTP Request Defaults: values inlined into the requests in scope")
	}
	if d.path != "" {
		im.note(idx, fmt.Sprintf("default path %q used only for samplers with an empty path (as in JMeter)", d.path))
	}
	if args, _ := n.elemProp("HTTPsampler.Arguments").coll("Arguments.arguments"); len(args) > 0 {
		im.escalate(idx, model.Partial, fmt.Sprintf("%d default parameter(s) not applied (Load Meter defaults have no parameters)", len(args)))
	}
	if n.boolean("HTTPSampler.image_parser", false) {
		im.escalate(idx, model.Partial, "\"Retrieve all embedded resources\" not supported: Load Meter does not download page resources")
	}
	if strings.TrimSpace(n.str("HTTPSampler.proxyHost")) != "" {
		im.escalate(idx, model.Partial, "proxy settings not imported")
	}
}

func (im *importer) timeoutNote(idx int, conn, resp int64) {
	if conn > 0 {
		im.escalate(idx, model.Partial, fmt.Sprintf("connect timeout (%dms) and response timeout (%dms) combined into one overall Load Meter request timeout", conn, max(resp, 0)))
	} else {
		im.note(idx, "response timeout (JMeter: socket read timeout) mapped to the overall Load Meter request timeout")
	}
}

func (im *importer) cookieManager(k elem, sc *scope, loc string) {
	n := k.n
	idx := im.add("CookieManager", nameOf(n), loc, model.Supported,
		"cookies enabled: Load Meter keeps an RFC 6265 cookie jar per VU (Go net/http/cookiejar)")
	if p := strings.TrimSpace(n.str("CookieManager.policy")); p != "" && p != "standard" && p != "default" {
		im.escalate(idx, model.Partial, fmt.Sprintf("JMeter cookie policy %q not reproduced", p))
	}
	if n.boolean("CookieManager.clearEachIteration", false) {
		im.escalate(idx, model.Partial, "\"Clear cookies each iteration\" not represented in the model; verify Load Meter jar lifetime")
	}
	if cs, _ := n.coll("CookieManager.cookies"); len(cs) > 0 {
		im.escalate(idx, model.Partial, fmt.Sprintf("%d predefined cookie(s) not imported (model has no initial cookies); send them as a Cookie header", len(cs)))
	}
	if im.container != "" {
		im.note(idx, "not applied: inside a non-executed container")
		return
	}
	if sc.level != "plan" && sc.level != "tg" {
		im.note(idx, "Load Meter cookie handling is scenario-wide (JMeter scoped it to this controller)")
	}
	im.cookies = true
}

func (im *importer) csv(k elem, sc *scope, loc string) {
	n := k.n
	name := nameOf(n)
	idx := im.add("CSVDataSet", name, loc, model.Supported, "")
	file := im.conv(strings.TrimSpace(n.str("filename")), loc)
	if file == "" {
		im.escalate(idx, model.Unsupported, "no filename")
		return
	}
	delim := n.str("delimiter")
	switch delim {
	case "":
		delim = ","
	case `\t`:
		delim = "\t"
	}
	ds := model.DataSource{Name: name, File: file, Delimiter: delim}
	if names := strings.TrimSpace(n.str("variableNames")); names != "" {
		sep := delim
		if !strings.Contains(names, sep) && strings.Contains(names, ",") {
			sep = ","
		}
		for _, c := range strings.Split(names, sep) {
			if c = strings.TrimSpace(c); c != "" {
				ds.Columns = append(ds.Columns, c)
			}
		}
		if n.boolean("ignoreFirstLine", false) {
			im.escalate(idx, model.Partial, fmt.Sprintf(
				"ignoreFirstLine with explicit variable names (%s): the model cannot skip a header while naming columns, so the file's header row is used as column names; make sure it matches",
				strings.Join(ds.Columns, ",")))
			ds.Columns = nil
		}
	}
	recycle, stop := n.boolean("recycle", true), n.boolean("stopThread", false)
	switch {
	case recycle:
		ds.OnExhaust = "recycle"
	case stop:
		ds.OnExhaust = "stop"
	default:
		ds.OnExhaust = "stop"
		im.escalate(idx, model.Partial, "recycle=false, stopThread=false: JMeter continues with <EOF> values; Load Meter stops the VU instead")
	}
	switch share := strings.TrimSpace(n.str("shareMode")); share {
	case "", "shareMode.all":
		ds.Mode = "sequential"
	case "shareMode.group":
		ds.Mode = "sequential"
		im.note(idx, "shared per thread group: equivalent to all-threads because one thread group is imported")
	case "shareMode.thread":
		ds.Mode = "per-vu"
		im.escalate(idx, model.Partial, "shareMode.thread: each JMeter thread reads the whole file from line 1; Load Meter per-vu mode assigns row = VU index")
	default:
		ds.Mode = "sequential"
		im.escalate(idx, model.Partial, fmt.Sprintf("custom share identifier %q mapped to one shared sequential cursor", share))
	}
	if enc := strings.TrimSpace(n.str("fileEncoding")); enc != "" && !strings.EqualFold(strings.ReplaceAll(enc, "-", ""), "utf8") {
		im.escalate(idx, model.Partial, fmt.Sprintf("file encoding %s: Load Meter reads CSV as UTF-8", enc))
	}
	if !path.IsAbs(file) && !strings.Contains(file, "${") {
		im.note(idx, "relative path: JMeter resolves it against the .jmx directory, Load Meter against the scenario file")
	}
	if sc.level != "plan" && sc.level != "tg" {
		im.note(idx, "Load Meter data sources are scenario-wide")
	}
	if im.loopDepth > 0 {
		im.escalate(idx, model.Partial, "inside a Loop Controller: JMeter reads a new line on every loop pass; Load Meter advances once per iteration")
	}
	if im.container != "" {
		im.note(idx, "not added: inside a non-executed container")
		return
	}
	for base, i := ds.Name, 2; im.dsNames[ds.Name]; i++ {
		ds.Name = fmt.Sprintf("%s_%d", base, i)
	}
	im.dsNames[ds.Name] = true
	im.sc.DataSources = append(im.sc.DataSources, ds)
}

func (im *importer) timer(k elem, sc *scope, loc string) {
	n := k.n
	tc := testClass(n)
	idx := im.add(tc, nameOf(n), loc, model.Supported, "")
	delayRaw, rangeRaw := strings.TrimSpace(n.str("ConstantTimer.delay")), strings.TrimSpace(n.str("RandomTimer.range"))
	delay, err1 := strconv.ParseFloat(orDefault(delayRaw, "0"), 64)
	rng, err2 := strconv.ParseFloat(orDefault(rangeRaw, "0"), 64)
	if err1 != nil || (tc != "ConstantTimer" && err2 != nil) || delay < 0 || rng < 0 {
		im.escalate(idx, model.Unsupported, fmt.Sprintf("timer values (delay %q, range %q) are not plain milliseconds; variables/functions in timers are not supported", delayRaw, rangeRaw))
		return
	}
	var th model.Think
	switch tc {
	case "ConstantTimer":
		th = model.Think{Min: msDur(delay)}
	case "UniformRandomTimer":
		th = model.Think{Min: msDur(delay), Max: msDur(delay + rng)}
	case "GaussianRandomTimer":
		th = model.Think{Min: msDur(math.Max(0, delay-rng)), Max: msDur(delay + rng)}
		im.escalate(idx, model.Partial, fmt.Sprintf("Gaussian delay (offset %gms, deviation %gms) approximated as uniform [offset-deviation, offset+deviation]", delay, rng))
	case "PoissonRandomTimer":
		th = model.Think{Min: msDur(delay), Max: msDur(delay + 2*rng)}
		im.escalate(idx, model.Partial, fmt.Sprintf("Poisson delay (offset %gms, lambda %gms) approximated as uniform with the same mean", delay, rng))
	}
	im.note(idx, "emitted as a think step before each sampler in scope")
	sc.timers = append(sc.timers, stepRef{step: model.Step{Type: model.StepThink, Name: nameOf(n), Think: &th, Comment: comment(n)}, item: idx})
	im.scoped = append(im.scoped, idx)
}

func (im *importer) userParams(k elem, sc *scope, loc string) {
	n := k.n
	idx := im.add("UserParameters", nameOf(n), loc, model.Supported, "")
	names, _ := n.coll("UserParameters.names")
	cols, _ := n.coll("UserParameters.thread_values")
	if len(names) == 0 || len(cols) == 0 {
		im.escalate(idx, model.Unsupported, "no parameters defined")
		return
	}
	if len(cols) > 1 {
		im.escalate(idx, model.Partial, fmt.Sprintf("%d per-thread value columns: only the first (thread 1) is imported", len(cols)))
	}
	vals := cols[0].Children
	var kvs []model.KV
	for i, nm := range names {
		v := ""
		if i < len(vals) {
			v = vals[i].Text()
		}
		kvs = append(kvs, model.KV{Name: strings.TrimSpace(nm.Text()), Value: im.conv(v, loc)})
	}
	if n.boolean("UserParameters.per_iteration", false) {
		im.perIter[idx] = true
	}
	im.note(idx, "emitted as a set step before each sampler in scope")
	sc.pre = append(sc.pre, stepRef{step: model.Step{Type: model.StepSetVar, Name: nameOf(n), Set: kvs, Comment: comment(n)}, item: idx})
	im.scoped = append(im.scoped, idx)
}

// sampleScope reports how an assertion/extractor's "Apply to" setting maps.
func sampleScope(n *node) (string, string) {
	switch s := strings.TrimSpace(n.str("Sample.scope")); s {
	case "", "parent":
		return model.Supported, ""
	case "all", "children":
		return model.Partial, fmt.Sprintf("applies to %q samples in JMeter (incl. redirect/embedded sub-samples); Load Meter applies it to the final response only", s)
	case "variable":
		return model.Unsupported, fmt.Sprintf("applies to the content of variable %q, which Load Meter does not support", n.str("Scope.variable"))
	default:
		return model.Partial, fmt.Sprintf("unknown scope %q treated as main sample", s)
	}
}

func (im *importer) assertion(k elem, sc *scope, loc string) {
	n := k.n
	tc := testClass(n)
	idx := im.add(tc, nameOf(n), loc, model.Supported, "")
	var as []model.Assertion
	var st, note string
	switch tc {
	case "ResponseAssertion":
		as, st, note = im.responseAssertion(n, loc)
	case "JSONPathAssertion":
		as, st, note = im.jsonPathAssertion(n, loc)
	case "DurationAssertion":
		ms := msOpt(n.str("DurationAssertion.duration"))
		if ms <= 0 {
			st, note = model.Unsupported, fmt.Sprintf("duration %q is not a positive number of milliseconds", n.str("DurationAssertion.duration"))
		} else {
			as = []model.Assertion{{Type: model.AsMaxLatency, Value: time.Duration(ms * int64(time.Millisecond)).String()}}
			st = model.Supported
		}
	}
	im.escalate(idx, st, note)
	sst, snote := sampleScope(n)
	im.escalate(idx, sst, snote)
	if len(as) == 0 || sst == model.Unsupported {
		if rank[im.rep.Items[idx].Status] < rank[model.Unsupported] {
			im.escalate(idx, model.Unsupported, "nothing mapped")
		}
		return
	}
	sc.asserts = append(sc.asserts, asRef{as: as, item: idx})
	im.scoped = append(im.scoped, idx)
}

var (
	codeRE  = regexp.MustCompile(`^[1-5]\d\d$`)
	classRE = regexp.MustCompile(`^([1-5])(?:\\d\\d|\.\.|\[0-9\]\[0-9\]|\\d\{2\}|\[0-9\]\{2\})$`)
)

func (im *importer) responseAssertion(n *node, loc string) ([]model.Assertion, string, string) {
	field := strings.TrimSpace(n.str("Assertion.test_field"))
	if field == "" {
		field = "Assertion.response_data"
	}
	tt, err := strconv.Atoi(strings.TrimSpace(n.str("Assertion.test_type")))
	if err != nil {
		return nil, model.Unsupported, fmt.Sprintf("invalid test_type %q", n.str("Assertion.test_type"))
	}
	list, ok := n.coll("Asserion.test_strings") // sic: JMeter's property name
	if !ok {
		list, _ = n.coll("Assertion.test_strings")
	}
	var pats []string
	for _, c := range list {
		pats = append(pats, im.conv(c.Text(), loc))
	}
	if len(pats) == 0 {
		return nil, model.Unsupported, "no patterns to test"
	}
	not, or := tt&4 != 0, tt&32 != 0
	mode := map[int]string{1: "matches", 2: "contains", 8: "equals", 16: "substring"}[tt&^36]
	if mode == "" {
		return nil, model.Unsupported, fmt.Sprintf("unsupported test_type %d", tt)
	}
	status, note := model.Supported, ""
	if n.boolean("Assertion.assume_success", false) {
		status, note = model.Partial, "\"Ignore Status\" is set: JMeter ignores the HTTP status for pass/fail; Load Meter fails >= 400 responses unless a status assertion is present"
	}
	desc := mode
	if not {
		desc = "NOT " + desc
	}
	if or {
		desc += " (OR)"
	}
	unsup := func(why string) ([]model.Assertion, string, string) {
		return nil, model.Unsupported, joinNote(note, fmt.Sprintf("%s on %s: %s", desc, strings.TrimPrefix(field, "Assertion."), why))
	}
	toRegex := func(p string) string {
		switch mode {
		case "substring":
			return quoteKeepVars(p)
		case "equals":
			return "^" + quoteKeepVars(p) + "$"
		case "matches":
			return "^(?:" + p + ")$"
		}
		return p
	}
	switch field {
	case "Assertion.response_data":
		var as []model.Assertion
		switch {
		case !not && or && len(pats) > 1:
			alts := make([]string, len(pats))
			for i, p := range pats {
				alts[i] = toRegex(p)
			}
			re := "(?:" + strings.Join(alts, "|") + ")"
			if err := re2Error(re); err != nil {
				return unsup("pattern is not RE2-compatible: " + err.Error())
			}
			as = append(as, model.Assertion{Type: model.AsRegex, Expr: re})
		case !not:
			for _, p := range pats {
				if mode == "substring" {
					as = append(as, model.Assertion{Type: model.AsContains, Value: p})
					continue
				}
				re := toRegex(p)
				if err := re2Error(re); err != nil {
					return unsup("pattern is not RE2-compatible (Go regexp has no backreferences/lookaround): " + err.Error())
				}
				as = append(as, model.Assertion{Type: model.AsRegex, Expr: re})
			}
		case not && (!or || len(pats) == 1):
			for _, p := range pats {
				if mode == "substring" || (mode == "contains" && isLiteralPattern(p)) {
					as = append(as, model.Assertion{Type: model.AsNotContains, Value: p})
					continue
				}
				return unsup("negated regex/equality has no Load Meter assertion type")
			}
		default:
			return unsup("NOT combined with OR has no Load Meter equivalent")
		}
		if mode == "contains" || mode == "matches" {
			note = joinNote(note, "JMeter uses Perl5 (ORO) regex, Load Meter uses Go RE2; syntax checked to compile")
		}
		return as, status, note
	case "Assertion.response_code":
		if not {
			return unsup("negated status checks are not supported")
		}
		var vals []string
		classes := 0
		for _, p := range pats {
			p = strings.TrimSpace(p)
			switch {
			case codeRE.MatchString(p):
				vals = append(vals, p)
			case (mode == "matches" || mode == "contains") && classRE.MatchString(p):
				vals = append(vals, classRE.FindStringSubmatch(p)[1]+"xx")
				classes++
			default:
				return unsup(fmt.Sprintf("status pattern %q is not a status code or class", p))
			}
		}
		if len(vals) > 1 && !or {
			for _, v := range vals[1:] {
				if v != vals[0] {
					return unsup("AND of different status codes can never pass")
				}
			}
			vals = vals[:1]
		}
		if classes > 0 && len(vals) > 1 {
			status = model.Partial
			note = joinNote(note, "list mixes status classes and codes; verify Load Meter accepts it")
		}
		return []model.Assertion{{Type: model.AsStatus, Value: strings.Join(vals, ",")}}, status, note
	case "Assertion.response_headers":
		if not || (or && len(pats) > 1) || !(mode == "substring" || (mode == "contains" && allLiteral(pats))) {
			return unsup("only plain (non-negated) substring header checks are mapped")
		}
		var as []model.Assertion
		for _, p := range pats {
			i := strings.Index(p, ":")
			if i <= 0 {
				return unsup(fmt.Sprintf("pattern %q is not of the form 'Name: value'", p))
			}
			as = append(as, model.Assertion{Type: model.AsHeader, Expr: strings.TrimSpace(p[:i]), Value: strings.TrimSpace(p[i+1:])})
		}
		return as, model.Partial, joinNote(note, "JMeter matches the raw header block (case-sensitive); mapped to a Load Meter header name/contains-value check")
	}
	return unsup("this test field is not supported")
}

func allLiteral(ps []string) bool {
	for _, p := range ps {
		if !isLiteralPattern(p) {
			return false
		}
	}
	return true
}

func (im *importer) jsonPathAssertion(n *node, loc string) ([]model.Assertion, string, string) {
	p := strings.TrimSpace(n.str("JSON_PATH"))
	if p == "" {
		return nil, model.Unsupported, "empty JSON path"
	}
	if n.boolean("INVERT", false) {
		return nil, model.Unsupported, "inverted JSON assertion has no Load Meter equivalent"
	}
	if n.boolean("EXPECT_NULL", false) {
		return nil, model.Unsupported, "\"expect null\" has no Load Meter equivalent"
	}
	a := model.Assertion{Type: model.AsJSONPath, Expr: im.conv(p, loc)}
	status, note := model.Supported, ""
	if n.boolean("JSONVALIDATION", false) {
		exp := n.str("EXPECTED_VALUE")
		if n.boolean("ISREGEX", true) && !isLiteralPattern(exp) {
			return nil, model.Unsupported, fmt.Sprintf("expected value %q is a regular expression; Load Meter compares for equality", exp)
		}
		if exp == "" {
			status, note = model.Partial, "JMeter expects an empty value; Load Meter only checks that the path exists"
		}
		a.Value = im.conv(exp, loc)
	}
	return []model.Assertion{a}, status, note
}

func (im *importer) extractor(k elem, sc *scope, loc string) {
	n := k.n
	tc := testClass(n)
	idx := im.add(tc, nameOf(n), loc, model.Supported, "")
	var exs []model.Extractor
	var st, note string
	switch tc {
	case "RegexExtractor":
		exs, st, note = im.regexExtractor(n, loc)
	case "JSONPostProcessor":
		exs, st, note = im.jsonExtractor(n, loc)
	case "BoundaryExtractor":
		exs, st, note = im.boundaryExtractor(n, loc)
	case "XPathExtractor", "XPath2Extractor":
		exs, st, note = im.xpathExtractor(n, loc)
	}
	im.escalate(idx, st, note)
	sst, snote := sampleScope(n)
	im.escalate(idx, sst, snote)
	if len(exs) == 0 || sst == model.Unsupported {
		if rank[im.rep.Items[idx].Status] < rank[model.Unsupported] {
			im.escalate(idx, model.Unsupported, "nothing mapped")
		}
		return
	}
	sc.extracts = append(sc.extracts, exRef{ex: exs, item: idx})
	im.scoped = append(im.scoped, idx)
}

// matchNo maps a JMeter match number (0 random, -1 all, N nth) to the model.
func matchNo(s string) (int, string, string) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return -1, model.Supported, ""
	}
	v, err := strconv.Atoi(s)
	switch {
	case err != nil:
		return 0, model.Partial, fmt.Sprintf("match number %q is not a number; first match used", s)
	case v < 0:
		return 0, model.Partial, "match number -1 (all matches -> name_1..name_N, name_matchNr) not supported; first match stored"
	}
	return v, model.Supported, ""
}

const (
	hdrPrefix    = "(?im)^"
	hdrSuffix    = `:\s*([^\r\n]*)`
	cookiePrefix = `(?im)^Set-Cookie:\s*`
	cookieSuffix = `=([^;\r\n]*)`
)

var tplGroupRE = regexp.MustCompile(`\$(\d+)\$`)

func unquoteName(q string) (string, bool) {
	name := strings.ReplaceAll(q, `\`, "")
	return name, name != "" && regexp.QuoteMeta(name) == q
}

func (im *importer) regexExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	refname := strings.TrimSpace(n.str("RegexExtractor.refname"))
	if refname == "" {
		return nil, model.Unsupported, "no reference name"
	}
	re := im.conv(n.str("RegexExtractor.regex"), loc)
	ex := model.Extractor{Name: refname, Type: model.ExRegex, Expr: re, Default: im.conv(n.str("RegexExtractor.default"), loc)}
	status, note := model.Supported, ""
	esc := func(s, nt string) {
		if rank[s] > rank[status] {
			status = s
		}
		note = joinNote(note, nt)
	}
	m, s, nt := matchNo(n.str("RegexExtractor.match_number"))
	ex.Match = m
	esc(s, nt)
	tpl := strings.TrimSpace(n.str("RegexExtractor.template"))
	groups := tplGroupRE.FindAllStringSubmatch(tpl, -1)
	switch {
	case len(groups) == 1 && groups[0][0] == tpl:
		ex.Group, _ = strconv.Atoi(groups[0][1])
	case len(groups) >= 1:
		ex.Group, _ = strconv.Atoi(groups[0][1])
		esc(model.Partial, fmt.Sprintf("template %q combines text/groups; only group %d is extracted", tpl, ex.Group))
	default:
		esc(model.Partial, fmt.Sprintf("template %q has no $N$ group; whole match used", tpl))
	}
	if ex.Group == 0 {
		if c, err := regexp.Compile(varRefRE.ReplaceAllString(re, "x")); err == nil && c.NumSubexp() > 0 {
			esc(model.Partial, "template $0$ (whole match) with capture groups: Load Meter group 0 means \"group 1 if the regex has groups\"")
		}
	}
	switch field := strings.TrimSpace(n.str("RegexExtractor.useHeaders")); field {
	case "", "false":
	case "unescaped":
		esc(model.Partial, "JMeter HTML-unescapes the body before matching; Load Meter matches the raw body")
	case "true":
		switch {
		case strings.HasPrefix(re, cookiePrefix) && strings.HasSuffix(re, cookieSuffix):
			if name, ok := unquoteName(strings.TrimSuffix(strings.TrimPrefix(re, cookiePrefix), cookieSuffix)); ok {
				ex.Type, ex.Expr, ex.Group = model.ExCookie, name, 0
				return []model.Extractor{ex}, status, joinNote(note, "Set-Cookie header regex mapped to a cookie extractor")
			}
		case strings.HasPrefix(re, hdrPrefix) && strings.HasSuffix(re, hdrSuffix):
			if name, ok := unquoteName(strings.TrimSuffix(strings.TrimPrefix(re, hdrPrefix), hdrSuffix)); ok {
				ex.Type, ex.Expr, ex.Group = model.ExHeader, name, 0
				return []model.Extractor{ex}, status, note
			}
		}
		return nil, model.Unsupported, joinNote(note, fmt.Sprintf("regex %q on response headers: Load Meter regex extractors read the body only; use a header extractor", re))
	case "code":
		switch re {
		case "(.*)", "(.+)", `(\d+)`, `(\d{3})`, `(\d\d\d)`:
			ex.Type, ex.Expr, ex.Group = model.ExStatus, "", 0
			return []model.Extractor{ex}, status, note
		}
		return nil, model.Unsupported, joinNote(note, fmt.Sprintf("regex %q on the response code is not supported", re))
	default:
		return nil, model.Unsupported, joinNote(note, fmt.Sprintf("regex on %q is not supported (Load Meter extracts from the body)", field))
	}
	if err := re2Error(re); err != nil {
		return nil, model.Unsupported, joinNote(note, "regex is not RE2-compatible (Go regexp has no backreferences/lookaround): "+err.Error())
	}
	return []model.Extractor{ex}, status, note
}

func splitSemi(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func (im *importer) jsonExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	names := splitSemi(n.str("JSONPostProcessor.referenceNames"))
	exprs := splitSemi(n.str("JSONPostProcessor.jsonPathExprs"))
	mns := splitSemi(n.str("JSONPostProcessor.match_numbers"))
	defs := strings.Split(n.str("JSONPostProcessor.defaultValues"), ";")
	if len(names) == 0 {
		return nil, model.Unsupported, "no reference names"
	}
	if len(exprs) != len(names) {
		return nil, model.Unsupported, fmt.Sprintf("%d names but %d JSON paths (JMeter would fail too)", len(names), len(exprs))
	}
	status, note := model.Supported, ""
	var out []model.Extractor
	for i, nm := range names {
		mn := ""
		if i < len(mns) {
			mn = mns[i]
		} else if len(mns) == 1 {
			mn = mns[0]
		}
		m, s, nt := matchNo(mn)
		if rank[s] > rank[status] {
			status = s
		}
		note = joinNote(note, nt)
		def := ""
		if i < len(defs) {
			def = defs[i]
		}
		e := exprs[i]
		if strings.Contains(e, "?(") || strings.Contains(e, "()") {
			status = model.Partial
			note = joinNote(note, fmt.Sprintf("%q uses Jayway JSONPath filters/functions; verify Load Meter JSONPath supports them", e))
		}
		out = append(out, model.Extractor{Name: nm, Type: model.ExJSONPath, Expr: im.conv(e, loc), Match: m, Default: im.conv(def, loc)})
	}
	if n.boolean("JSONPostProcessor.compute_concat", false) {
		status = model.Partial
		note = joinNote(note, "\"compute concatenation\" (name_ALL) not supported")
	}
	if len(names) > 1 {
		note = joinNote(note, fmt.Sprintf("split into %d extractors", len(names)))
	}
	return out, status, note
}

func (im *importer) boundaryExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	refname := strings.TrimSpace(n.str("BoundaryExtractor.refname"))
	if refname == "" {
		return nil, model.Unsupported, "no reference name"
	}
	if f := strings.TrimSpace(n.str("BoundaryExtractor.useHeaders")); f != "" && f != "false" {
		return nil, model.Unsupported, fmt.Sprintf("boundary extraction from %q is not supported (Load Meter extracts from the body)", f)
	}
	m, status, note := matchNo(n.str("BoundaryExtractor.match_number"))
	ex := model.Extractor{Name: refname, Type: model.ExBoundary, Left: im.conv(n.str("BoundaryExtractor.lboundary"), loc),
		Right: im.conv(n.str("BoundaryExtractor.rboundary"), loc), Match: m, Default: im.conv(n.str("BoundaryExtractor.default"), loc)}
	if ex.Left == "" && ex.Right == "" {
		status = model.Partial
		note = joinNote(note, "both boundaries empty (JMeter returns the whole response)")
	}
	return []model.Extractor{ex}, status, note
}

func (im *importer) xpathExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	pre := "XPathExtractor."
	if testClass(n) == "XPath2Extractor" {
		pre = "XPathExtractor2."
	}
	refname := strings.TrimSpace(n.str(pre + "refname"))
	q := strings.TrimSpace(n.str(pre + "xpathQuery"))
	if refname == "" || q == "" {
		return nil, model.Unsupported, "missing reference name or XPath query"
	}
	m, _, mnote := matchNo(n.str(pre + "matchNumber"))
	ex := model.Extractor{Name: refname, Type: model.ExXPath, Expr: im.conv(q, loc), Match: m, Default: im.conv(n.str(pre+"default"), loc)}
	note := joinNote("XPath support differs (JMeter: Xalan XPath 1 / Saxon XPath 2-3 with tolerant HTML parsing); verify the expression in Load Meter", mnote)
	if strings.TrimSpace(n.str(pre+"namespaces")) != "" || n.boolean(pre+"namespace", false) {
		note = joinNote(note, "namespace configuration not imported")
	}
	if n.boolean(pre+"fragment", false) {
		note = joinNote(note, "\"return entire XPath fragment\" not supported")
	}
	return []model.Extractor{ex}, model.Partial, note
}

// ---------------------------------------------------------------------------
// Executable elements: samplers and controllers

var nonHTTPSamplers = map[string]bool{"JSR223Sampler": true, "BeanShellSampler": true, "BSFSampler": true,
	"JavaSampler": true, "JDBCSampler": true, "JMSSampler": true, "PublisherSampler": true, "SubscriberSampler": true,
	"FTPSampler": true, "LDAPSampler": true, "LDAPExtSampler": true, "TCPSampler": true, "SmtpSampler": true,
	"SystemSampler": true, "AjpSampler": true, "MailReaderSampler": true, "AccessLogSampler": true, "BoltSampler": true}

var unsupportedControllers = map[string]string{
	"WhileController":        "While Controller",
	"ForeachController":      "ForEach Controller",
	"ThroughputController":   "Throughput Controller",
	"InterleaveControl":      "Interleave Controller",
	"RandomController":       "Random Controller",
	"RandomOrderController":  "Random Order Controller",
	"RunTime":                "Runtime Controller",
	"SwitchController":       "Switch Controller",
	"ModuleController":       "Module Controller",
	"IncludeController":      "Include Controller",
	"TestFragmentController": "Test Fragment",
}

func (im *importer) execElement(k elem, sc *scope, loc string) []model.Step {
	n := k.n
	tc := testClass(n)
	name := nameOf(n)
	ctrl := &scope{parent: sc, level: "ctrl"}
	switch {
	case tc == "HTTPSamplerProxy" || tc == "HTTPSampler" || tc == "HTTPSampler2":
		return im.httpSampler(k, sc, loc)
	case tc == "TestAction":
		return im.testAction(k, sc, loc)
	case tc == "TransactionController":
		idx := im.add(tc, name, loc, model.Supported, "")
		if n.boolean("TransactionController.includeTimers", false) {
			im.escalate(idx, model.Partial, "\"include timers\" is set: JMeter counts timer delays in the transaction time, Load Meter excludes think time")
		}
		if n.boolean("TransactionController.parent", false) {
			im.note(idx, "\"generate parent sample\": Load Meter reports the transaction and its requests separately")
		}
		return []model.Step{{Type: model.StepTransaction, Name: name, Comment: comment(n), Steps: im.buildLevel(k.kids, ctrl, loc)}}
	case tc == "GenericController" || tc == "RecordingController":
		im.add(tc, name, loc, model.Supported, "mapped to a group step")
		return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Steps: im.buildLevel(k.kids, ctrl, loc)}}
	case tc == "CriticalSectionController":
		im.add(tc, name, loc, model.Partial, fmt.Sprintf("Critical Section (lock %q) mapped to a plain group: Load Meter has no cross-VU locking", n.str("CriticalSectionController.lockName")))
		return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Steps: im.buildLevel(k.kids, ctrl, loc)}}
	case tc == "LoopController":
		return im.loopController(k, sc, loc)
	case tc == "IfController":
		return im.ifController(k, sc, loc)
	case tc == "OnceOnlyController":
		if im.inSetup || im.container != "" {
			im.add(tc, name, loc, model.Partial, "Once Only Controller inside setup/teardown or a non-executed container: imported as a plain group")
			return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Steps: im.buildLevel(k.kids, ctrl, loc)}}
		}
		im.add(tc, name, loc, model.Partial,
			"children moved to scenario setup: Load Meter runs setup once per VU before the first iteration; JMeter runs them once per thread at this position of the first iteration")
		im.sc.Setup = append(im.sc.Setup, im.buildLevel(k.kids, ctrl, loc)...)
		return nil
	case unsupportedControllers[tc] != "":
		note := unsupportedControllers[tc] + " has no Load Meter equivalent" + controllerDetail(n) + "; children kept in a disabled group so nothing is lost"
		im.add(tc, name, loc, model.Unsupported, note)
		im.manual("Rework %s %q at %s: %s", unsupportedControllers[tc], name, loc, note)
		return []model.Step{im.disabledGroup(k, sc, loc, note)}
	case tc == "DebugSampler":
		im.add(tc, name, loc, model.Ignored, "debug element; Load Meter debug output replaces it")
		im.drain(k.kids, sc, loc, name)
		return nil
	case nonHTTPSamplers[tc]:
		note := "non-HTTP sampler: Load Meter only executes HTTP requests; never executed"
		if strings.HasPrefix(tc, "JSR223") || strings.HasPrefix(tc, "BeanShell") || strings.HasPrefix(tc, "BSF") {
			note = scriptNote(n)
		}
		im.add(tc, name, loc, model.Unsupported, note)
		im.manual("Replace %s %q at %s: %s", tc, name, loc, note)
		im.drain(k.kids, sc, loc, name)
		return nil
	case isThreadGroup(tc):
		note := "thread group nested below the Test Plan level is invalid; children kept in a disabled group"
		im.add(tc, name, loc, model.Unsupported, note)
		return []model.Step{im.disabledGroup(k, sc, loc, note)}
	}
	note := fmt.Sprintf("unknown element testclass %s", tc)
	if strings.HasPrefix(tc, "kg.apc.") || strings.HasPrefix(tc, "com.blazemeter.") {
		note += " (JMeter Plugins / jmeter-plugins.org element)"
	}
	note += "; not executed"
	im.add(tc, name, loc, model.Unsupported, note)
	im.manual("Unknown element %s %q at %s: no Load Meter mapping", tc, name, loc)
	if len(k.kids) > 0 {
		return []model.Step{im.disabledGroup(k, sc, loc, note)}
	}
	return nil
}

func controllerDetail(n *node) string {
	switch testClass(n) {
	case "WhileController":
		return fmt.Sprintf(" (condition %q)", n.str("WhileController.condition"))
	case "ForeachController":
		return fmt.Sprintf(" (input %q -> %q)", n.str("ForeachController.inputVal"), n.str("ForeachController.returnVal"))
	case "ThroughputController":
		return " (percentage/total execution control)"
	case "RunTime":
		return fmt.Sprintf(" (runtime %ss)", n.str("RunTime.seconds"))
	case "SwitchController":
		return fmt.Sprintf(" (switch value %q)", n.str("SwitchController.value"))
	case "ModuleController", "IncludeController":
		return " (references other plan parts; inline them manually)"
	}
	return ""
}

func (im *importer) loopController(k elem, sc *scope, loc string) []model.Step {
	n := k.n
	name := nameOf(n)
	idx := im.add("LoopController", name, loc, model.Supported, "")
	raw := strings.TrimSpace(n.str("LoopController.loops"))
	var notes []string
	cnt, ok := im.num(raw, loc, &notes)
	for _, nt := range notes {
		im.escalate(idx, model.Partial, nt)
	}
	im.loopDepth++
	steps := im.buildLevel(k.kids, &scope{parent: sc, level: "ctrl"}, loc)
	im.loopDepth--
	switch {
	case ok && cnt < 0:
		im.escalate(idx, model.Partial, "infinite loop (-1): Load Meter has no unbounded loop step; children run once per iteration in a group")
		return []model.Step{{Type: model.StepGroup, Name: name, Comment: comment(n), Steps: steps}}
	case !ok || cnt < 1:
		im.escalate(idx, model.Partial, fmt.Sprintf("loop count %q not usable; imported with count 1", raw))
		cnt = 1
	}
	return []model.Step{{Type: model.StepLoop, Name: name, Count: int(cnt), Comment: comment(n), Steps: steps}}
}

var (
	condDQ     = regexp.MustCompile(`^\s*"\$\{([^${}"]+)\}"\s*(==|!=)\s*"([^"]*)"\s*$`)
	condSQ     = regexp.MustCompile(`^\s*'\$\{([^${}']+)\}'\s*(==|!=)\s*'([^']*)'\s*$`)
	condGroovy = regexp.MustCompile(`^\s*vars\.get\("([^"]+)"\)\s*!=\s*null\s*&&\s*vars\.get\("([^"]+)"\)\s*!=\s*""\s*$`)
)

// ifCondition converts the simple JMeter If Controller forms; ok=false otherwise.
func (im *importer) ifCondition(raw, loc string) (cond, note string, ok bool) {
	expr := strings.TrimSpace(raw)
	if strings.HasPrefix(expr, "${__") {
		r, parsed := parseRef(expr, 0)
		if !parsed || r.end != len(expr) || len(r.args) == 0 {
			return "", "", false
		}
		switch r.name {
		case "__jexl3", "__jexl2", "__groovy":
		default:
			return "", "", false
		}
		if len(r.args) > 1 && strings.TrimSpace(r.args[1]) != "" {
			note = fmt.Sprintf("result variable %q of %s not stored", strings.TrimSpace(r.args[1]), r.name)
		}
		expr = unescapeArg(r.args[0])
		if m := condGroovy.FindStringSubmatch(expr); m != nil && m[1] == m[2] {
			return "${" + m[1] + "}", note, true
		}
	}
	m := condDQ.FindStringSubmatch(expr)
	if m == nil {
		m = condSQ.FindStringSubmatch(expr)
	}
	if m == nil {
		return "", "", false
	}
	return "${" + m[1] + "} " + m[2] + " " + im.conv(m[3], loc), note, true
}

func (im *importer) ifController(k elem, sc *scope, loc string) []model.Step {
	n := k.n
	name := nameOf(n)
	raw := n.str("IfController.condition")
	cond, note, ok := im.ifCondition(raw, loc)
	if !ok {
		why := fmt.Sprintf("condition %q is not a simple \"${var}\" ==/!= \"value\" comparison; children kept in a disabled group", raw)
		im.add("IfController", name, loc, model.Unsupported, why)
		im.manual("Rewrite If Controller %q at %s as a Load Meter condition: %s", name, loc, raw)
		return []model.Step{im.disabledGroup(k, sc, loc, why)}
	}
	idx := im.add("IfController", name, loc, model.Supported, fmt.Sprintf("condition %q -> %q", raw, cond))
	if note != "" {
		im.escalate(idx, model.Partial, note)
	}
	if n.boolean("IfController.evaluateAll", false) {
		im.escalate(idx, model.Partial, "\"evaluate for all children\" not supported: Load Meter evaluates once")
	}
	return []model.Step{{Type: model.StepIf, Name: name, Cond: cond, Comment: comment(n),
		Steps: im.buildLevel(k.kids, &scope{parent: sc, level: "ctrl"}, loc)}}
}

func cloneStep(s model.Step) model.Step {
	if s.Think != nil {
		t := *s.Think
		s.Think = &t
	}
	s.Set = append([]model.KV(nil), s.Set...)
	return s
}

// preSteps returns the set and think steps JMeter would run before a sampler:
// pre-processors first, then timers, outer scope first.
func (im *importer) preSteps(sc *scope) []model.Step {
	var out []model.Step
	ch := sc.chain()
	for _, s := range ch {
		for _, p := range s.pre {
			out = append(out, cloneStep(p.step))
			im.applied[p.item]++
		}
	}
	for _, s := range ch {
		for _, t := range s.timers {
			out = append(out, cloneStep(t.step))
			im.applied[t.item]++
		}
	}
	return out
}

func (im *importer) testAction(k elem, sc *scope, loc string) []model.Step {
	n := k.n
	name := nameOf(n)
	action := strings.TrimSpace(n.str("ActionProcessor.action"))
	if action != "1" {
		note := fmt.Sprintf("Flow Control Action %q (action=%s: stop/restart/break) not supported", name, action)
		im.add("TestAction", name, loc, model.Unsupported, note)
		im.manual("%s at %s", note, loc)
		im.drain(k.kids, sc, loc, name)
		return nil
	}
	idx := im.add("TestAction", name, loc, model.Supported, "Flow Control Action (pause) mapped to think step(s)")
	own := &scope{parent: sc, level: "sampler"}
	im.buildLevel(k.kids, own, loc)
	steps := im.preSteps(own)
	raw := strings.TrimSpace(n.str("ActionProcessor.duration"))
	ms, err := strconv.ParseFloat(orDefault(raw, "0"), 64)
	switch {
	case err != nil:
		im.escalate(idx, model.Partial, fmt.Sprintf("pause duration %q is not a number of milliseconds; dropped", raw))
	case ms > 0:
		steps = append(steps, model.Step{Type: model.StepThink, Name: name, Think: &model.Think{Min: msDur(ms)}, Comment: comment(n)})
	}
	return steps
}

func (im *importer) httpSampler(k elem, parent *scope, loc string) []model.Step {
	n := k.n
	name := nameOf(n)
	idx := im.add(testClass(n), name, loc, model.Supported, "")
	sc := &scope{parent: parent, level: "sampler"}
	if nested := im.buildLevel(k.kids, sc, loc); len(nested) > 0 {
		im.escalate(idx, model.Partial, "samplers/controllers nested under a sampler are invalid in JMeter and were dropped")
	}
	req := im.buildRequest(n, sc, loc, idx)
	steps := im.preSteps(sc)
	return append(steps, model.Step{Type: model.StepRequest, Name: name, Request: req, Comment: comment(n)})
}

type httpArg struct {
	name, value, contentType string
	encode                   bool
}

func httpArgs(n *node) []httpArg {
	ap := n.elemProp("HTTPsampler.Arguments")
	if ap == nil {
		ap = n.elemProp("HTTPSampler.Arguments")
	}
	list, _ := ap.coll("Arguments.arguments")
	var out []httpArg
	for _, e := range list {
		if e.Name != "elementProp" {
			continue
		}
		out = append(out, httpArg{name: e.str("Argument.name"), value: e.str("Argument.value"),
			contentType: strings.TrimSpace(e.str("HTTPArgument.content_type")), encode: e.boolean("HTTPArgument.always_encode", false)})
	}
	return out
}

type httpFile struct{ path, param, mime string }

func httpFiles(n *node) []httpFile {
	fp := n.elemProp("HTTPsampler.Files")
	if fp == nil {
		fp = n.elemProp("HTTPSampler.Files")
	}
	list, _ := fp.coll("HTTPFileArgs.files")
	var out []httpFile
	for _, e := range list {
		if e.Name != "elementProp" {
			continue
		}
		out = append(out, httpFile{path: e.str("File.path"), param: e.str("File.paramname"), mime: strings.TrimSpace(e.str("File.mimetype"))})
	}
	return out
}

func queryMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "OPTIONS", "TRACE", "DELETE":
		return true
	}
	return false
}

func formUnescape(s string) string {
	if strings.ContainsAny(s, "%+") {
		if d, err := url.QueryUnescape(s); err == nil {
			return d
		}
	}
	return s
}

func (im *importer) buildRequest(n *node, sc *scope, loc string, idx int) *model.Request {
	g := func(k string) string { return strings.TrimSpace(n.str("HTTPSampler." + k)) }
	method := strings.ToUpper(g("method"))
	if method == "" {
		method = "GET"
	}
	protocol, domain, port, p := g("protocol"), g("domain"), g("port"), g("path")
	abs := protocol != "" || domain != "" || port != ""
	connMS, respMS := msOpt(g("connect_timeout")), msOpt(g("response_timeout"))
	ch := sc.chain()
	for i := len(ch) - 1; i >= 0; i-- {
		d := ch[i].defaults
		if d == nil {
			continue
		}
		fill := func(dst *string, v string) {
			if *dst == "" && v != "" {
				*dst = v
				if !d.scenario {
					abs = true
				}
			}
		}
		fill(&protocol, d.protocol)
		fill(&domain, d.domain)
		fill(&port, d.port)
		if p == "" {
			p = d.path
		}
		if !d.scenario {
			if connMS < 0 {
				connMS = d.connectMS
			}
			if respMS < 0 {
				respMS = d.responseMS
			}
		}
	}
	var u string
	if lp := strings.ToLower(p); strings.HasPrefix(lp, "http://") || strings.HasPrefix(lp, "https://") {
		u = p
	} else {
		switch {
		case p == "":
			p = "/"
		case !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "${"):
			p = "/" + p
		}
		if domain == "" && im.scen.domain == "" {
			im.escalate(idx, model.Partial, "no server name in the sampler or HTTP Request Defaults; URL left relative to defaults.baseUrl")
		}
		if abs && domain != "" {
			u = composeBase(protocol, domain, port) + p
		} else {
			u = p
		}
	}
	u = im.conv(u, loc)
	req := &model.Request{Method: method}

	args := httpArgs(n)
	files := httpFiles(n)
	raw := n.boolean("HTTPSampler.postBodyRaw", false)
	multipart := n.boolean("HTTPSampler.DO_MULTIPART_POST", false) || len(files) > 0
	switch {
	case raw:
		var b strings.Builder
		for _, a := range args {
			b.WriteString(a.value)
		}
		req.Body = im.conv(b.String(), loc)
		if len(files) > 0 {
			im.escalate(idx, model.Partial, "file uploads ignored because the body is raw")
		}
	case multipart:
		for _, a := range args {
			req.Multipart = append(req.Multipart, model.Part{Name: im.conv(a.name, loc), Value: im.conv(a.value, loc), ContentType: a.contentType})
		}
		for _, f := range files {
			if f.param == "" {
				im.escalate(idx, model.Partial, fmt.Sprintf("file %q has no parameter name (JMeter would send it as the raw body)", f.path))
			}
			req.Multipart = append(req.Multipart, model.Part{Name: im.conv(f.param, loc), File: im.conv(f.path, loc), ContentType: f.mime})
		}
		if len(files) > 0 {
			im.note(idx, "file paths: JMeter resolves relative paths against its working/base directory, Load Meter against the scenario file")
		}
	case len(args) > 0 && queryMethod(method):
		var qs []string
		for _, a := range args {
			nm, v := im.conv(a.name, loc), im.conv(a.value, loc)
			if a.encode {
				nm, v = encodeKeepVars(nm), encodeKeepVars(v)
			}
			if nm == "" {
				if v != "" {
					qs = append(qs, v)
				}
				continue
			}
			qs = append(qs, nm+"="+v)
		}
		if len(qs) > 0 {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + strings.Join(qs, "&")
		}
	case len(args) > 0:
		named := false
		for _, a := range args {
			if a.name != "" {
				named = true
			}
		}
		if !named {
			var b strings.Builder
			for _, a := range args {
				b.WriteString(a.value)
			}
			req.Body = im.conv(b.String(), loc)
			break
		}
		for _, a := range args {
			nm, v := im.conv(a.name, loc), im.conv(a.value, loc)
			if !a.encode {
				nm, v = formUnescape(nm), formUnescape(v)
			}
			req.Form = append(req.Form, model.KV{Name: nm, Value: v})
		}
	}
	req.URL = u

	var hs []model.Header
	for _, s := range ch {
		if s.level == "plan" {
			continue
		}
		hs = mergeHeaders(hs, s.headers)
		for _, hi := range s.headerItems {
			im.applied[hi]++
		}
	}
	req.Headers = hs

	if connMS >= 0 || respMS >= 0 {
		if t := timeoutMS(connMS, respMS); t > 0 {
			req.Timeout = msDur(float64(t))
			im.timeoutNote(idx, connMS, respMS)
		}
	}
	follow := n.boolean("HTTPSampler.follow_redirects", false) || n.boolean("HTTPSampler.auto_redirects", false)
	req.FollowRedirects = &follow

	for _, s := range ch {
		for _, e := range s.extracts {
			req.Extract = append(req.Extract, e.ex...)
			im.applied[e.item]++
		}
	}
	for _, s := range ch {
		for _, a := range s.asserts {
			req.Assert = append(req.Assert, a.as...)
			im.applied[a.item]++
		}
	}

	if n.boolean("HTTPSampler.image_parser", false) {
		im.escalate(idx, model.Partial, "\"Retrieve all embedded resources\" not supported: Load Meter does not download page resources")
	}
	if g("ipSource") != "" {
		im.escalate(idx, model.Partial, "source IP spoofing (ipSource) not supported")
	}
	if g("proxyHost") != "" {
		im.escalate(idx, model.Partial, "per-sampler proxy not supported")
	}
	if v, ok := n.prop("HTTPSampler.use_keepalive"); ok && strings.TrimSpace(v) == "false" {
		im.escalate(idx, model.Partial, "keep-alive disabled in JMeter (new connection per request); Load Meter reuses connections")
	}
	if enc := g("contentEncoding"); enc != "" && !strings.EqualFold(strings.ReplaceAll(enc, "-", ""), "utf8") {
		im.escalate(idx, model.Partial, fmt.Sprintf("content encoding %s not applied: Load Meter sends bodies as given (UTF-8)", enc))
	}
	return req
}

// ---------------------------------------------------------------------------
// Workload

var wholeVarRE = regexp.MustCompile(`^\$\{([^{}]+)\}$`)

// num resolves a numeric JMeter field, following ${var} and ${__P(name,default)}.
func (im *importer) num(raw, loc string, notes *[]string) (float64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	if strings.Contains(s, "${") {
		c := im.conv(s, loc)
		if m := wholeVarRE.FindStringSubmatch(c); m != nil {
			if v, ok := im.sc.Variables[m[1]]; ok {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					*notes = append(*notes, fmt.Sprintf("%s resolved to its default %s at import time; later overrides are not applied", raw, v))
					return f, true
				}
			}
		}
		*notes = append(*notes, fmt.Sprintf("value %q could not be resolved to a number", raw))
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("value %q is not a number", raw))
		return 0, false
	}
	return f, true
}

func secs(f float64) model.Duration { return model.Duration(time.Duration(f * float64(time.Second))) }

const defaultUnboundedDuration = 10 * time.Minute

func (im *importer) tgWorkload(n *node, loc string) (model.Workload, []string, bool) {
	var notes, info []string
	threads, ok := im.num(n.str("ThreadGroup.num_threads"), loc, &notes)
	if !ok || threads < 1 {
		threads = 1
		notes = append(notes, "thread count missing; using 1")
	}
	ramp, _ := im.num(n.str("ThreadGroup.ramp_time"), loc, &notes)
	mc := n.elemProp("ThreadGroup.main_controller")
	loopsRaw := strings.TrimSpace(mc.str("LoopController.loops"))
	loops := -1.0
	if loopsRaw != "" && loopsRaw != "-1" {
		if l, ok := im.num(loopsRaw, loc, &notes); ok {
			loops = l
		}
	}
	var dur, delay float64
	if n.boolean("ThreadGroup.scheduler", false) {
		dur, _ = im.num(n.str("ThreadGroup.duration"), loc, &notes)
		delay, _ = im.num(n.str("ThreadGroup.delay"), loc, &notes)
	}
	if delay > 0 {
		notes = append(notes, fmt.Sprintf("startup delay of %gs not represented; Load Meter starts immediately", delay))
	}
	switch e := strings.TrimSpace(n.str("ThreadGroup.on_sample_error")); e {
	case "", "continue":
	default:
		notes = append(notes, fmt.Sprintf("on-sampler-error action %q not supported (Load Meter continues)", e))
	}
	if !n.boolean("ThreadGroup.same_user_on_next_iteration", true) {
		info = append(info, "\"same user on each iteration\" is off: JMeter resets cookies/cache per iteration; verify Load Meter jar lifetime")
	}
	N := int(math.Round(threads))
	w := model.Workload{}
	rampStages := func(total float64) {
		switch {
		case ramp <= 0:
			w.Executor, w.VUs, w.Duration = model.ExecConstantVUs, N, secs(total)
		case ramp >= total:
			w.Executor = model.ExecRampingVUs
			w.Stages = []model.Stage{{Duration: secs(total), Target: float64(N)}}
			notes = append(notes, fmt.Sprintf("ramp-up (%gs) not shorter than duration (%gs): approximated as a single ramp to %d", ramp, total, N))
		default:
			w.Executor = model.ExecRampingVUs
			w.Stages = []model.Stage{{Duration: secs(ramp), Target: float64(N)}, {Duration: secs(total - ramp), Target: float64(N)}}
		}
	}
	switch {
	case dur > 0:
		if loops > 0 {
			notes = append(notes, fmt.Sprintf("JMeter stops after %g loop(s) or %gs, whichever comes first; Load Meter uses the duration only", loops, dur))
		}
		rampStages(dur)
	case loops > 0:
		w.Executor, w.VUs, w.Iterations = model.ExecPerVUIterations, N, int(loops)
		if ramp > 0 {
			notes = append(notes, fmt.Sprintf("ramp-up of %gs not representable with per-vu-iterations: all VUs start together", ramp))
		}
	default:
		notes = append(notes, fmt.Sprintf("infinite loop without scheduler: JMeter runs until stopped; Load Meter needs a bound, defaulted to %s", defaultUnboundedDuration))
		rampStages(defaultUnboundedDuration.Seconds())
	}
	return w, append(notes, info...), len(notes) > 0
}

func (im *importer) utgWorkload(n *node, loc string) (model.Workload, []string) {
	notes := []string{"Ultimate Thread Group schedule converted to a piecewise-linear ramping-vus profile (JMeter starts threads at discrete times; zero-length ramps become 1s)"}
	type seg struct{ n, d, s, h, sd float64 }
	var segs []seg
	rows, _ := n.coll("ultimatethreadgroupdata")
	for _, r := range rows {
		var v [5]float64
		for i := 0; i < 5 && i < len(r.Children); i++ {
			v[i], _ = im.num(r.Children[i].Text(), loc, &notes)
		}
		sg := seg{n: v[0], d: v[1], s: math.Max(v[2], 1), h: v[3], sd: math.Max(v[4], 1)}
		if sg.n > 0 {
			segs = append(segs, sg)
		}
	}
	if len(segs) == 0 {
		notes = append(notes, "no schedule rows; using 1 VU for 10m")
		return model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(defaultUnboundedDuration)}, notes
	}
	contrib := func(sg seg, t float64) float64 {
		switch {
		case t <= sg.d:
			return 0
		case t < sg.d+sg.s:
			return sg.n * (t - sg.d) / sg.s
		case t <= sg.d+sg.s+sg.h:
			return sg.n
		case t < sg.d+sg.s+sg.h+sg.sd:
			return sg.n * (1 - (t-sg.d-sg.s-sg.h)/sg.sd)
		}
		return 0
	}
	pts := map[float64]bool{0: true}
	for _, sg := range segs {
		for _, p := range []float64{sg.d, sg.d + sg.s, sg.d + sg.s + sg.h, sg.d + sg.s + sg.h + sg.sd} {
			pts[p] = true
		}
	}
	var ts []float64
	for p := range pts {
		ts = append(ts, p)
	}
	sort.Float64s(ts)
	w := model.Workload{Executor: model.ExecRampingVUs}
	for i := 1; i < len(ts); i++ {
		total := 0.0
		for _, sg := range segs {
			total += contrib(sg, ts[i])
		}
		w.Stages = append(w.Stages, model.Stage{Duration: secs(ts[i] - ts[i-1]), Target: math.Round(total)})
	}
	return w, notes
}

func (im *importer) ctgWorkload(n *node, loc string) (model.Workload, []string) {
	notes := []string{"Concurrency Thread Group converted to ramping-vus stages"}
	target, ok := im.num(n.str("TargetLevel"), loc, &notes)
	if !ok || target < 1 {
		target = 1
	}
	ramp, _ := im.num(n.str("RampUp"), loc, &notes)
	steps, _ := im.num(n.str("Steps"), loc, &notes)
	hold, _ := im.num(n.str("Hold"), loc, &notes)
	unit := time.Minute
	if strings.EqualFold(strings.TrimSpace(n.str("Unit")), "S") {
		unit = time.Second
	}
	if steps > 0 {
		notes = append(notes, fmt.Sprintf("stepped ramp-up (%g steps) approximated as a linear ramp", steps))
	}
	if it := strings.TrimSpace(n.str("Iterations")); it != "" {
		notes = append(notes, fmt.Sprintf("thread iteration limit %s not applied", it))
	}
	w := model.Workload{Executor: model.ExecRampingVUs}
	if ramp > 0 {
		w.Stages = append(w.Stages, model.Stage{Duration: model.Duration(time.Duration(ramp * float64(unit))), Target: target})
	}
	if hold > 0 {
		w.Stages = append(w.Stages, model.Stage{Duration: model.Duration(time.Duration(hold * float64(unit))), Target: target})
	} else {
		notes = append(notes, "no hold time: JMeter runs until stopped; added a 10m hold")
		w.Stages = append(w.Stages, model.Stage{Duration: model.Duration(defaultUnboundedDuration), Target: target})
	}
	return w, notes
}
