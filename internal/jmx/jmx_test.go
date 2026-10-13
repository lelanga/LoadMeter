package jmx

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

const fixture = "../../testdata/jmx/login_flow.jmx"

func importFile(t *testing.T, path string) (*model.Scenario, *model.CompatReport) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc, rep, err := Import(f, path)
	if err != nil {
		t.Fatalf("Import(%s): %v", path, err)
	}
	return sc, rep
}

func importString(t *testing.T, s string) (*model.Scenario, *model.CompatReport) {
	t.Helper()
	sc, rep, err := Import(strings.NewReader(s), "inline.jmx")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return sc, rep
}

func findStep(steps []model.Step, name string) *model.Step {
	var found *model.Step
	model.Walk(steps, func(s *model.Step) {
		if found == nil && s.Name == name {
			found = s
		}
	})
	return found
}

func findItem(rep *model.CompatReport, loc string) *model.CompatItem {
	for i := range rep.Items {
		if rep.Items[i].Location == loc {
			return &rep.Items[i]
		}
	}
	return nil
}

func dur(s string) model.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return model.Duration(d)
}

func TestImportLoginFlow(t *testing.T) {
	sc, rep := importFile(t, fixture)

	if sc.Name != "Shop Login Flow" || sc.Description != "Login, browse, add to cart." {
		t.Errorf("name/description = %q / %q", sc.Name, sc.Description)
	}
	if sc.Metadata["importer"] != "jmx" {
		t.Errorf("metadata importer = %q", sc.Metadata["importer"])
	}
	if got := sc.Variables; got["apiVersion"] != "v2" || got["threads"] != "10" {
		t.Errorf("variables = %v", got)
	}
	d := sc.Defaults
	if d.BaseURL != "https://shop.example.com" || d.Timeout != dur("30s") || d.DisableCookies {
		t.Errorf("defaults = %+v", d)
	}
	if len(d.Headers) != 2 || d.Headers[0].Name != "User-Agent" || d.Headers[1].Value != "application/json, text/html" {
		t.Errorf("default headers = %+v", d.Headers)
	}
	wantWL := model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{
		{Duration: dur("30s"), Target: 10}, {Duration: dur("270s"), Target: 10}}}
	if !reflect.DeepEqual(sc.Workload, wantWL) {
		t.Errorf("workload = %+v, want %+v", sc.Workload, wantWL)
	}
	if len(sc.DataSources) != 1 {
		t.Fatalf("data sources = %+v", sc.DataSources)
	}
	ds := sc.DataSources[0]
	if ds.File != "users.csv" || !reflect.DeepEqual(ds.Columns, []string{"username", "password"}) ||
		ds.Mode != "sequential" || ds.OnExhaust != "recycle" || ds.Delimiter != "," {
		t.Errorf("data source = %+v", ds)
	}
	if len(sc.Setup) != 1 || sc.Setup[0].Request == nil || sc.Setup[0].Request.URL != "/health" {
		t.Errorf("setup = %+v", sc.Setup)
	}
	if p := sc.Validate(); len(p) != 0 {
		t.Errorf("Validate: %v", p)
	}

	// Transaction with think before each sampler (thread-group timer scope).
	tx := sc.Steps[0]
	if tx.Type != model.StepTransaction || tx.Name != "Login" || len(tx.Steps) != 4 {
		t.Fatalf("first step = %+v", tx)
	}
	if th := tx.Steps[0]; th.Type != model.StepThink || th.Think.Min != dur("1s") || th.Think.Max != dur("3s") {
		t.Errorf("think = %+v", th)
	}
	open := tx.Steps[1].Request
	if open.Method != "GET" || open.URL != "/login" || *open.FollowRedirects != true {
		t.Errorf("open login = %+v", open)
	}
	if !reflect.DeepEqual(open.Headers, []model.Header{{Name: "X-Client", Value: "loadmeter-${__vu()}"}}) {
		t.Errorf("open login headers = %+v", open.Headers)
	}
	wantRe := model.Extractor{Name: "csrf", Type: model.ExRegex, Expr: `name="csrf" value="([^"]+)"`, Group: 1, Match: 1, Default: "NOT_FOUND"}
	if len(open.Extract) != 1 || open.Extract[0] != wantRe {
		t.Errorf("csrf extractor = %+v", open.Extract)
	}
	if !reflect.DeepEqual(open.Assert, []model.Assertion{{Type: model.AsStatus, Value: "200"}}) {
		t.Errorf("open login asserts = %+v", open.Assert)
	}

	login := tx.Steps[3].Request
	if login.Method != "POST" || login.URL != "/api/${apiVersion}/login" || login.Timeout != dur("5s") || *login.FollowRedirects {
		t.Errorf("login = %+v", login)
	}
	wantForm := []model.KV{{Name: "username", Value: "${username}"}, {Name: "password", Value: "${password}"}, {Name: "csrf", Value: "${csrf}"}}
	if !reflect.DeepEqual(login.Form, wantForm) {
		t.Errorf("form = %+v", login.Form)
	}
	if model.HeaderValue(login.Headers, "content-type") != "application/x-www-form-urlencoded" || model.HeaderValue(login.Headers, "X-Client") == "" {
		t.Errorf("login headers = %+v", login.Headers)
	}
	wantJSON := []model.Extractor{
		{Name: "token", Type: model.ExJSONPath, Expr: "$.token", Match: 1, Default: "NONE"},
		{Name: "userId", Type: model.ExJSONPath, Expr: "$.user.id", Match: 1, Default: "0"},
	}
	if !reflect.DeepEqual(login.Extract, wantJSON) {
		t.Errorf("json extractors = %+v", login.Extract)
	}
	wantAs := []model.Assertion{{Type: model.AsJSONPath, Expr: "$.status", Value: "ok"}, {Type: model.AsMaxLatency, Value: "2s"}}
	if !reflect.DeepEqual(login.Assert, wantAs) {
		t.Errorf("login asserts = %+v", login.Assert)
	}

	// GET arguments become the query string; functions are mapped.
	lp := findStep(sc.Steps, "List products")
	if lp == nil {
		t.Fatal("List products not found")
	}
	if want := "/api/products?category=shoes&page=1&q=red+shoes+${__randomInt(1,100)}"; lp.Request.URL != want {
		t.Errorf("products URL = %q, want %q", lp.Request.URL, want)
	}
	if model.HeaderValue(lp.Request.Headers, "Authorization") != "Bearer ${token}" {
		t.Errorf("products headers = %+v", lp.Request.Headers)
	}
	wantPA := []model.Assertion{{Type: model.AsContains, Value: "products"}, {Type: model.AsNotContains, Value: "error"}}
	if !reflect.DeepEqual(lp.Request.Assert, wantPA) {
		t.Errorf("products asserts = %+v", lp.Request.Assert)
	}
	if e := lp.Request.Extract; len(e) != 1 || e[0].Type != model.ExBoundary || e[0].Left != `"id":"` || e[0].Right != `"` {
		t.Errorf("boundary = %+v", e)
	}
	// The two timers in scope (thread group uniform + sampler constant) precede the sampler.
	if i := indexOf(sc.Steps, "List products"); i < 2 || sc.Steps[i-1].Name != "Pause 500ms" || sc.Steps[i-2].Name != "Think 1-3s" {
		t.Errorf("timers before List products not as expected: %+v", sc.Steps[:i])
	}

	ifs := findStep(sc.Steps, "Has product")
	if ifs == nil || ifs.Type != model.StepIf || ifs.Cond != "${productId} != NOT_FOUND" {
		t.Fatalf("if step = %+v", ifs)
	}
	cart := findStep(ifs.Steps, "Add to cart").Request
	if cart.Body != `{"productId":"${productId}","qty":1,"ref":"${__uuid()}"}` {
		t.Errorf("cart body = %q", cart.Body)
	}
	if !reflect.DeepEqual(cart.Assert, []model.Assertion{{Type: model.AsStatus, Value: "200,201"}}) {
		t.Errorf("cart asserts = %+v", cart.Assert)
	}
	if lp := findStep(sc.Steps, "Browse 3 times"); lp == nil || lp.Type != model.StepLoop || lp.Count != 3 {
		t.Errorf("loop = %+v", lp)
	}
	// Unconvertible controllers keep their children in disabled groups.
	if w := findStep(sc.Steps, "Paginate"); w == nil || w.Type != model.StepWhile || w.Cond != "${more} == true" || len(w.Steps) == 0 {
		t.Errorf("Paginate: expected a while step, got %+v", w)
	}
	for _, name := range []string{"Long token", "Admins"} {
		g := findStep(sc.Steps, name)
		if g == nil || g.Type != model.StepGroup || !g.Disabled || len(g.Steps) == 0 || g.Comment == "" {
			t.Errorf("%s: expected disabled group with children, got %+v", name, g)
		}
	}
	if findStep(sc.Steps, "Debug endpoint") != nil {
		t.Error("disabled sampler was imported")
	}
	gp := findStep(sc.Steps, "Gaussian pause")
	if gp == nil || gp.Think.Min != dur("200ms") || gp.Think.Max != dur("400ms") {
		t.Errorf("gaussian think = %+v", gp)
	}
	if rep.Confidence == "" || rep.Validated != "not-run" {
		t.Errorf("confidence/validated = %q/%q", rep.Confidence, rep.Validated)
	}
}

func indexOf(steps []model.Step, name string) int {
	for i, s := range steps {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// countElements counts test elements the way the importer must report them:
// every element once; children of disabled elements are not visited.
func countElements(kids []elem) int {
	c := 0
	for _, k := range kids {
		c++
		if enabled(k.n) {
			c += countElements(k.kids)
		}
	}
	return c
}

func TestEveryElementReportedOnce(t *testing.T) {
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseTree(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := countElements(pairs(root.child("hashTree")))
	_, rep := importString(t, string(raw))
	got := 0
	locs := map[string]int{}
	for _, it := range rep.Items {
		if strings.HasPrefix(it.Element, "function ") {
			continue
		}
		got++
		locs[it.Location]++
		if it.Status != model.Supported && it.Note == "" {
			t.Errorf("%s (%s) is %s without a note", it.Location, it.Element, it.Status)
		}
	}
	if got != want {
		t.Errorf("element items = %d, want %d (one per element)", got, want)
	}
	for l, n := range locs {
		if n > 1 {
			t.Errorf("location %q reported %d times", l, n)
		}
	}
}

func TestUnsupportedElementsReported(t *testing.T) {
	_, rep := importFile(t, fixture)
	cases := []struct {
		loc, status string
		noteHas     []string
	}{
		{"TestPlan/Shoppers/Login/Submit login/Uppercase token", model.Unsupported, []string{"groovy", "toUpperCase", "never executed"}},
		{"TestPlan/Shoppers/Paginate", model.Partial, []string{"${more} == true", "capped at"}},
		{"TestPlan/Shoppers/Long token", model.Unsupported, []string{"__groovy", "disabled group"}},
		{"TestPlan/Shoppers/Throughput Shaping Timer", model.Unsupported, []string{"kg.apc.jmeter.timers.VariableThroughputTimer", "JMeter Plugins"}},
		{"TestPlan/View Results Tree", model.Ignored, []string{"UI/listener element; Load Meter reports replace it"}},
		{"TestPlan/Shoppers/Debug endpoint", model.Ignored, []string{"disabled"}},
		{"TestPlan/HTTP Cache Manager", model.Ignored, []string{"cache"}},
		{"TestPlan/Admins", model.Partial, []string{"one load thread group"}},
		{"TestPlan/setUp Thread Group", model.Partial, []string{"once per VU"}},
		{"TestPlan/Shoppers/Logout/Gaussian pause", model.Partial, []string{"uniform"}},
		{"TestPlan/Shoppers/Paginate/Next page", model.Supported, nil},
	}
	for _, c := range cases {
		it := findItem(rep, c.loc)
		if it == nil {
			t.Errorf("no item for %s", c.loc)
			continue
		}
		if it.Status != c.status {
			t.Errorf("%s: status %s, want %s (%s)", c.loc, it.Status, c.status, it.Note)
		}
		for _, s := range c.noteHas {
			if !strings.Contains(it.Note, s) {
				t.Errorf("%s: note %q lacks %q", c.loc, it.Note, s)
			}
		}
	}
	if len(rep.Manual) < 4 {
		t.Errorf("expected manual-change entries, got %v", rep.Manual)
	}
	if rep.Confidence == "high" {
		t.Errorf("confidence should not be high with unsupported elements")
	}
	var sawP bool
	for _, it := range rep.Items {
		if it.Element == "function __P" && it.Status == model.Partial {
			sawP = true
		}
	}
	if !sawP {
		t.Error("__P property mapping not reported")
	}
}

// stripDisabled removes disabled steps (unsupported content kept for reference)
// so that only the supported subset is compared.
func stripDisabled(steps []model.Step) []model.Step {
	var out []model.Step
	for _, s := range steps {
		if s.Disabled {
			continue
		}
		s.Steps = stripDisabled(s.Steps)
		s.Comment = ""
		out = append(out, s)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	sc1, _ := importFile(t, fixture)
	out, exRep, err := Export(sc1)
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(out, new(struct{ XMLName xml.Name })); err != nil {
		t.Fatalf("exported XML is not well-formed: %v", err)
	}
	if exRep.Format != "jmx" || len(exRep.Items) == 0 {
		t.Errorf("export report = %+v", exRep)
	}
	sc2, _, err := Import(bytes.NewReader(out), "roundtrip.jmx")
	if err != nil {
		t.Fatalf("re-import: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(stripDisabled(sc1.Steps), stripDisabled(sc2.Steps)) {
		t.Errorf("steps differ after round trip\nbefore: %+v\nafter:  %+v", stripDisabled(sc1.Steps), stripDisabled(sc2.Steps))
	}
	if !reflect.DeepEqual(stripDisabled(sc1.Setup), stripDisabled(sc2.Setup)) {
		t.Errorf("setup differs: %+v vs %+v", sc1.Setup, sc2.Setup)
	}
	if !reflect.DeepEqual(sc1.Workload, sc2.Workload) {
		t.Errorf("workload differs: %+v vs %+v", sc1.Workload, sc2.Workload)
	}
	if !reflect.DeepEqual(sc1.Defaults, sc2.Defaults) {
		t.Errorf("defaults differ: %+v vs %+v", sc1.Defaults, sc2.Defaults)
	}
	if !reflect.DeepEqual(sc1.DataSources, sc2.DataSources) {
		t.Errorf("data sources differ: %+v vs %+v", sc1.DataSources, sc2.DataSources)
	}
	if !reflect.DeepEqual(sc1.Variables, sc2.Variables) {
		t.Errorf("variables differ: %v vs %v", sc1.Variables, sc2.Variables)
	}
	if sc1.Name != sc2.Name || sc1.Description != sc2.Description {
		t.Errorf("name/description differ")
	}
}

// TestRoundTripFromModel exports a hand-built scenario covering constructs the
// fixture does not (set steps, multipart, header/status/cookie extractors,
// regex assertions, teardown, per-vu-iterations) and re-imports it.
func TestRoundTripFromModel(t *testing.T) {
	yes, no := true, false
	sc := model.NewScenario("Model <&> \"quoted\"")
	sc.Variables["base"] = "x&y<z>"
	sc.Defaults.BaseURL = "http://api.local:8080"
	sc.Workload = model.Workload{Executor: model.ExecPerVUIterations, VUs: 3, Iterations: 7}
	sc.Steps = []model.Step{
		{Type: model.StepSetVar, Name: "init", Set: []model.KV{{Name: "a", Value: "1"}, {Name: "id", Value: "${__uuid()}"}}},
		{Type: model.StepRequest, Name: "upload", Request: &model.Request{Method: "POST", URL: "/upload", FollowRedirects: &no,
			Multipart: []model.Part{{Name: "title", Value: "hello"}, {Name: "file", File: "data/a.png", ContentType: "image/png"}},
			Extract: []model.Extractor{
				{Name: "loc", Type: model.ExHeader, Expr: "Location", Match: 1},
				{Name: "code", Type: model.ExStatus, Match: 1},
				{Name: "sid", Type: model.ExCookie, Expr: "SESSION", Match: 1},
				{Name: "first", Type: model.ExRegex, Expr: `id=(\d+)`, Group: 1, Match: -1, Default: "none"},
			},
			Assert: []model.Assertion{{Type: model.AsStatus, Value: "2xx"}, {Type: model.AsRegex, Expr: `ok\s+done`}}}},
		{Type: model.StepThink, Name: "wait", Think: &model.Think{Min: dur("2s")}},
		{Type: model.StepIf, Name: "nonempty", Cond: "${a}", Steps: []model.Step{
			{Type: model.StepRequest, Name: "abs", Request: &model.Request{Method: "GET", URL: "https://other.example:8443/p?q=${a}", FollowRedirects: &yes}},
		}},
	}
	sc.Teardown = []model.Step{{Type: model.StepRequest, Name: "bye", Request: &model.Request{Method: "DELETE", URL: "/session", FollowRedirects: &yes}}}
	out, _, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	got, rep, err := Import(bytes.NewReader(out), "model.jmx")
	if err != nil {
		t.Fatalf("re-import: %v\n%s", err, out)
	}
	if got.Name != sc.Name || got.Variables["base"] != "x&y<z>" {
		t.Errorf("escaping lost: name %q vars %v", got.Name, got.Variables)
	}
	if !reflect.DeepEqual(got.Workload, sc.Workload) {
		t.Errorf("workload = %+v", got.Workload)
	}
	if got.Defaults.BaseURL != sc.Defaults.BaseURL {
		t.Errorf("baseUrl = %q", got.Defaults.BaseURL)
	}
	// The trailing think step is exported as a Flow Control Action + timer.
	if !reflect.DeepEqual(got.Steps, sc.Steps) {
		t.Errorf("steps differ\nwant %+v\ngot  %+v", sc.Steps, got.Steps)
	}
	if !reflect.DeepEqual(got.Teardown, sc.Teardown) {
		t.Errorf("teardown = %+v", got.Teardown)
	}
	_ = rep
}

func TestMalformedAndUnsafeInput(t *testing.T) {
	cases := map[string]string{
		"malformed":   `<jmeterTestPlan><hashTree><TestPlan testclass="TestPlan"></hashTree></jmeterTestPlan>`,
		"truncated":   `<jmeterTestPlan><hashTree>`,
		"doctype":     `<?xml version="1.0"?><!DOCTYPE jmeterTestPlan [<!ENTITY x "y">]><jmeterTestPlan><hashTree/></jmeterTestPlan>`,
		"xxe":         `<?xml version="1.0"?><!DOCTYPE r [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><jmeterTestPlan>&xxe;</jmeterTestPlan>`,
		"undefEntity": `<jmeterTestPlan><hashTree>&lol;</hashTree></jmeterTestPlan>`,
		"wrongRoot":   `<project><hashTree/></project>`,
		"noTestPlan":  `<jmeterTestPlan><hashTree/></jmeterTestPlan>`,
		"empty":       ``,
		"twoRoots":    `<jmeterTestPlan/><jmeterTestPlan/>`,
		"badCharset":  `<?xml version="1.0" encoding="EBCDIC"?><jmeterTestPlan/>`,
	}
	for name, in := range cases {
		if _, _, err := Import(strings.NewReader(in), name); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, _, err := Import(strings.NewReader(cases["doctype"]), "x"); err == nil || !strings.Contains(err.Error(), "DOCTYPE") {
		t.Errorf("doctype error = %v", err)
	}
	for _, f := range []string{"malformed.jmx", "doctype_xxe.jmx", "billion_laughs.jmx"} {
		fh, err := os.Open("../../testdata/jmx/" + f)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = Import(fh, f)
		fh.Close()
		if err == nil {
			t.Errorf("%s: expected an error", f)
		}
	}
	if _, _, err := Import(nil, "nil"); err == nil {
		t.Error("nil reader accepted")
	}
}

func TestInputSizeAndDepthLimits(t *testing.T) {
	old := inputLimit
	inputLimit = 1024
	defer func() { inputLimit = old }()
	big := `<jmeterTestPlan>` + strings.Repeat(" ", 2048) + `</jmeterTestPlan>`
	if _, _, err := Import(strings.NewReader(big), "big"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("size limit error = %v", err)
	}
	inputLimit = old
	deep := strings.Repeat("<a>", maxDepth+5) + strings.Repeat("</a>", maxDepth+5)
	if _, _, err := Import(strings.NewReader(deep), "deep"); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Errorf("depth limit error = %v", err)
	}
	// The reader is never read past the limit + 1.
	lr := &countingReader{r: strings.NewReader(strings.Repeat("x", 4096))}
	inputLimit = 100
	_, _, _ = Import(lr, "count")
	inputLimit = old
	if lr.n > 101 {
		t.Errorf("read %d bytes, want <= 101", lr.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestLatin1Input(t *testing.T) {
	in := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>" + plan("Caf\xe9", tg(`<stringProp name="ThreadGroup.num_threads">1</stringProp>`, ""), "")
	sc, _ := importString(t, in)
	if sc.Name != "Café" {
		t.Errorf("name = %q", sc.Name)
	}
}

func TestFunctionMapping(t *testing.T) {
	cases := []struct{ in, want, status string }{
		{"${__UUID()}", "${__uuid()}", ""},
		{"${__UUID}", "${__uuid()}", ""},
		{"${__Random(1,10)}", "${__randomInt(1,10)}", ""},
		{"${__Random(1,10,r)}", "${__randomInt(1,10,r)}", ""},
		{"${__time()}", "${__timestamp()}", ""},
		{"${__time(,)}", "${__timestamp()}", ""},
		{"${__time(yyyy-MM-dd,)}", "${__timestamp(yyyy-MM-dd)}", ""},
		{"${__counter(FALSE,)}", "${__counter()}", ""},
		{"${__counter(TRUE,c)}", "${__counter(TRUE,c)}", ""},
		{"${__threadNum}", "${__vu()}", ""},
		{"${__RandomString(12," + alnum + ",)}", "${__randomString(12)}", ""},
		{"${__RandomString(5,abc)}", "${__randomString(5)}", model.Partial},
		{"${__P(host,example.com)}", "${host}", model.Partial},
		{"${__property(port,,8080)}", "${port}", model.Partial},
		{"${__intSum(1,2)}", "${__intSum(1,2)}", ""},
		{"a ${user} b ${__threadNum}-${__UUID()}", "a ${user} b ${__vu()}-${__uuid()}", ""},
		{"${__jexl3(\"${x}\" == \"y\")}", "${__jexl3(\"${x}\" == \"y\")}", model.Unsupported},
	}
	for _, c := range cases {
		vars := map[string]string{}
		got, issues := importFuncs(c.in, vars)
		if got != c.want {
			t.Errorf("importFuncs(%q) = %q, want %q", c.in, got, c.want)
		}
		worst := ""
		for _, is := range issues {
			if rank[is.Status] > rank[worst] {
				worst = is.Status
			}
		}
		if worst != c.status {
			t.Errorf("importFuncs(%q) status %q, want %q (%v)", c.in, worst, c.status, issues)
		}
	}
	vars := map[string]string{}
	importFuncs("${__P(host,example.com)}", vars)
	if vars["host"] != "example.com" {
		t.Errorf("__P default not recorded: %v", vars)
	}
	// Export maps back and re-import is the identity for supported functions.
	for _, s := range []string{"${__uuid()}", "${__randomInt(1,10)}", "${__timestamp()}", "${__counter()}", "${__vu()}", "${__randomString(8)}"} {
		j, issues := exportFuncs(s)
		if len(issues) != 0 {
			t.Errorf("exportFuncs(%q) issues %v", s, issues)
		}
		back, iss := importFuncs(j, map[string]string{})
		if back != s || len(iss) != 0 {
			t.Errorf("round trip %q -> %q -> %q (%v)", s, j, back, iss)
		}
	}
	if _, issues := exportFuncs("${__iteration()}"); len(issues) != 1 {
		t.Errorf("unmapped Load Meter function not reported")
	}
}

// --- small plan builders for focused tests ---

func plan(name, body, planLevel string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<jmeterTestPlan version="1.2" properties="5.0" jmeter="5.6.3"><hashTree>
<TestPlan guiclass="TestPlanGui" testclass="TestPlan" testname="` + name + `" enabled="true"/>
<hashTree>` + planLevel + body + `</hashTree></hashTree></jmeterTestPlan>`
}

func tg(props, kids string) string {
	return `<ThreadGroup guiclass="ThreadGroupGui" testclass="ThreadGroup" testname="TG" enabled="true">` + props +
		`</ThreadGroup><hashTree>` + kids + `</hashTree>`
}

func loops(n string) string {
	return `<elementProp name="ThreadGroup.main_controller" elementType="LoopController"><stringProp name="LoopController.loops">` + n + `</stringProp></elementProp>`
}

func sampler(name, extraProps, kids string) string {
	return `<HTTPSamplerProxy guiclass="HttpTestSampleGui" testclass="HTTPSamplerProxy" testname="` + name + `" enabled="true">` +
		`<stringProp name="HTTPSampler.domain">h.example</stringProp><stringProp name="HTTPSampler.path">/` + name + `</stringProp>` +
		extraProps + `</HTTPSamplerProxy><hashTree>` + kids + `</hashTree>`
}

func TestWorkloadMapping(t *testing.T) {
	cases := []struct {
		name, props string
		want        model.Workload
		status      string
	}{
		{"iterations", `<stringProp name="ThreadGroup.num_threads">5</stringProp>` + loops("20"),
			model.Workload{Executor: model.ExecPerVUIterations, VUs: 5, Iterations: 20}, model.Supported},
		{"constant", `<stringProp name="ThreadGroup.num_threads">4</stringProp><stringProp name="ThreadGroup.ramp_time">0</stringProp>` + loops("-1") +
			`<boolProp name="ThreadGroup.scheduler">true</boolProp><stringProp name="ThreadGroup.duration">60</stringProp>`,
			model.Workload{Executor: model.ExecConstantVUs, VUs: 4, Duration: dur("60s")}, model.Supported},
		{"loops+duration", `<stringProp name="ThreadGroup.num_threads">2</stringProp><stringProp name="ThreadGroup.ramp_time">10</stringProp>` + loops("3") +
			`<boolProp name="ThreadGroup.scheduler">true</boolProp><stringProp name="ThreadGroup.duration">40</stringProp>`,
			model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{{Duration: dur("10s"), Target: 2}, {Duration: dur("30s"), Target: 2}}}, model.Partial},
		{"infinite", `<stringProp name="ThreadGroup.num_threads">3</stringProp>` + loops("-1"),
			model.Workload{Executor: model.ExecConstantVUs, VUs: 3, Duration: model.Duration(defaultUnboundedDuration)}, model.Partial},
	}
	for _, c := range cases {
		sc, rep := importString(t, plan("W", tg(c.props, sampler("a", "", "")), ""))
		if !reflect.DeepEqual(sc.Workload, c.want) {
			t.Errorf("%s: workload %+v, want %+v", c.name, sc.Workload, c.want)
		}
		if it := findItem(rep, "TestPlan/TG"); it == nil || it.Status != c.status {
			t.Errorf("%s: thread group item %+v, want status %s", c.name, it, c.status)
		}
	}
}

func TestPluginThreadGroups(t *testing.T) {
	utg := `<kg.apc.jmeter.threads.UltimateThreadGroup guiclass="kg.apc.jmeter.threads.UltimateThreadGroupGui" testclass="kg.apc.jmeter.threads.UltimateThreadGroup" testname="UTG" enabled="true">
<collectionProp name="ultimatethreadgroupdata"><collectionProp name="1">
<stringProp name="a">10</stringProp><stringProp name="b">0</stringProp><stringProp name="c">30</stringProp><stringProp name="d">60</stringProp><stringProp name="e">10</stringProp>
</collectionProp></collectionProp></kg.apc.jmeter.threads.UltimateThreadGroup><hashTree>` + sampler("a", "", "") + `</hashTree>`
	sc, rep := importString(t, plan("U", utg, ""))
	want := []model.Stage{{Duration: dur("30s"), Target: 10}, {Duration: dur("60s"), Target: 10}, {Duration: dur("10s"), Target: 0}}
	if sc.Workload.Executor != model.ExecRampingVUs || !reflect.DeepEqual(sc.Workload.Stages, want) {
		t.Errorf("UTG workload = %+v", sc.Workload)
	}
	if it := findItem(rep, "TestPlan/UTG"); it == nil || it.Status != model.Partial {
		t.Errorf("UTG item = %+v", it)
	}
	ctg := `<com.blazemeter.jmeter.threads.concurrency.ConcurrencyThreadGroup guiclass="com.blazemeter.jmeter.threads.concurrency.ConcurrencyThreadGroupGui" testclass="com.blazemeter.jmeter.threads.concurrency.ConcurrencyThreadGroup" testname="CTG" enabled="true">
<stringProp name="TargetLevel">20</stringProp><stringProp name="RampUp">2</stringProp><stringProp name="Steps">4</stringProp><stringProp name="Hold">5</stringProp><stringProp name="Unit">M</stringProp>
</com.blazemeter.jmeter.threads.concurrency.ConcurrencyThreadGroup><hashTree>` + sampler("a", "", "") + `</hashTree>`
	sc, rep = importString(t, plan("C", ctg, ""))
	want = []model.Stage{{Duration: dur("2m"), Target: 20}, {Duration: dur("5m"), Target: 20}}
	if !reflect.DeepEqual(sc.Workload.Stages, want) {
		t.Errorf("CTG stages = %+v", sc.Workload.Stages)
	}
	if it := findItem(rep, "TestPlan/CTG"); it == nil || it.Status != model.Partial || !strings.Contains(it.Note, "stepped") {
		t.Errorf("CTG item = %+v", it)
	}
}

func respAssert(field string, tt int, pats ...string) string {
	s := `<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname="RA" enabled="true"><collectionProp name="Asserion.test_strings">`
	for _, p := range pats {
		s += `<stringProp name="0">` + p + `</stringProp>`
	}
	return s + `</collectionProp><stringProp name="Assertion.test_field">` + field + `</stringProp><intProp name="Assertion.test_type">` +
		strconv.Itoa(tt) + `</intProp></ResponseAssertion><hashTree/>`
}

func TestResponseAssertionMapping(t *testing.T) {
	cases := []struct {
		name   string
		xml    string
		want   []model.Assertion
		status string
	}{
		{"equals body", respAssert("Assertion.response_data", 8, "OK."), []model.Assertion{{Type: model.AsEquals, Value: "OK."}}, model.Supported},
		{"matches", respAssert("Assertion.response_data", 1, "ok.*"), []model.Assertion{{Type: model.AsRegex, Expr: `^(?:ok.*)$`}}, model.Supported},
		{"contains regex", respAssert("Assertion.response_data", 2, `id=\d+`), []model.Assertion{{Type: model.AsRegex, Expr: `id=\d+`}}, model.Supported},
		{"substring OR", respAssert("Assertion.response_data", 48, "a.b", "c"), []model.Assertion{{Type: model.AsRegex, Expr: `(?:a\.b|c)`}}, model.Supported},
		{"not regex", respAssert("Assertion.response_data", 6, `x\d`), []model.Assertion{{Type: model.AsRegex, Expr: `x\d`, AssertionOptions: model.AssertionOptions{Not: true}}}, model.Supported},
		{"not literal contains", respAssert("Assertion.response_data", 6, "fail"), []model.Assertion{{Type: model.AsNotContains, Value: "fail"}}, model.Supported},
		{"not OR", respAssert("Assertion.response_data", 52, "a", "b"), nil, model.Unsupported},
		{"lookahead", respAssert("Assertion.response_data", 2, "a(?=b)"), nil, model.Unsupported},
		{"status class", respAssert("Assertion.response_code", 1, `2\d\d`), []model.Assertion{{Type: model.AsStatus, Value: "2xx"}}, model.Supported},
		{"status AND conflict", respAssert("Assertion.response_code", 8, "200", "201"), []model.Assertion{
			{Type: model.AsEquals, Value: "200", AssertionOptions: model.AssertionOptions{Source: model.SrcStatus}},
			{Type: model.AsEquals, Value: "201", AssertionOptions: model.AssertionOptions{Source: model.SrcStatus}}}, model.Supported},
		{"status NOT", respAssert("Assertion.response_code", 12, "500"), []model.Assertion{{Type: model.AsEquals, Value: "500", AssertionOptions: model.AssertionOptions{Source: model.SrcStatus, Not: true}}}, model.Supported},
		{"header", respAssert("Assertion.response_headers", 16, "Content-Type: application/json"), []model.Assertion{{Type: model.AsHeader, Expr: "Content-Type", Value: "application/json"}}, model.Partial},
		{"message", respAssert("Assertion.response_message", 16, "OK"), []model.Assertion{{Type: model.AsContains, Value: "OK", AssertionOptions: model.AssertionOptions{Source: model.SrcMessage}}}, model.Supported},
		{"url", respAssert("Assertion.sample_label", 2, "/home$"), []model.Assertion{{Type: model.AsRegex, Expr: "/home$", AssertionOptions: model.AssertionOptions{Source: model.SrcURL}}}, model.Supported},
		{"header regex", respAssert("Assertion.response_headers", 2, `X-Id: \d+`), []model.Assertion{{Type: model.AsRegex, Expr: `X-Id: \d+`, AssertionOptions: model.AssertionOptions{Source: model.SrcHeaders}}}, model.Partial},
		{"document", respAssert("Assertion.response_data_as_document", 16, "x"), nil, model.Unsupported},
	}
	for _, c := range cases {
		sc, rep := importString(t, plan("A", tg(loops("1"), sampler("s", "", c.xml)), ""))
		got := findStep(sc.Steps, "s").Request.Assert
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: asserts %+v, want %+v", c.name, got, c.want)
		}
		if it := findItem(rep, "TestPlan/TG/s/RA"); it == nil || it.Status != c.status {
			t.Errorf("%s: item %+v, want %s", c.name, it, c.status)
		}
	}
}

func TestSamplerAndScopeDetails(t *testing.T) {
	multipart := `<elementProp name="HTTPsampler.Arguments" elementType="Arguments"><collectionProp name="Arguments.arguments">
<elementProp name="title" elementType="HTTPArgument"><stringProp name="Argument.name">title</stringProp><stringProp name="Argument.value">hi</stringProp></elementProp>
</collectionProp></elementProp>
<elementProp name="HTTPsampler.Files" elementType="HTTPFileArgs"><collectionProp name="HTTPFileArgs.files">
<elementProp name="a.png" elementType="HTTPFileArg"><stringProp name="File.path">a.png</stringProp><stringProp name="File.paramname">file</stringProp><stringProp name="File.mimetype">image/png</stringProp></elementProp>
</collectionProp></elementProp><stringProp name="HTTPSampler.method">POST</stringProp>`
	nestedDefaults := `<GenericController guiclass="LogicControllerGui" testclass="GenericController" testname="Other host" enabled="true"/><hashTree>
<ConfigTestElement guiclass="HttpDefaultsGui" testclass="ConfigTestElement" testname="D2" enabled="true"><stringProp name="HTTPSampler.domain">other.example</stringProp><stringProp name="HTTPSampler.protocol">https</stringProp><stringProp name="HTTPSampler.port">8443</stringProp></ConfigTestElement><hashTree/>
<HTTPSamplerProxy guiclass="HttpTestSampleGui" testclass="HTTPSamplerProxy" testname="rel" enabled="true"><stringProp name="HTTPSampler.path">x</stringProp></HTTPSamplerProxy><hashTree/>
</hashTree>`
	once := `<OnceOnlyController guiclass="OnceOnlyControllerGui" testclass="OnceOnlyController" testname="Once" enabled="true"/><hashTree>` + sampler("login", "", "") + `</hashTree>`
	pause := `<TestAction guiclass="TestActionGui" testclass="TestAction" testname="Pause" enabled="true"><intProp name="ActionProcessor.action">1</intProp><stringProp name="ActionProcessor.duration">1500</stringProp></TestAction><hashTree/>`
	stop := `<TestAction guiclass="TestActionGui" testclass="TestAction" testname="Stop" enabled="true"><intProp name="ActionProcessor.action">0</intProp></TestAction><hashTree/>`
	jdbc := `<JDBCSampler guiclass="TestBeanGUI" testclass="JDBCSampler" testname="DB" enabled="true"/><hashTree/>`
	headerRegex := `<RegexExtractor guiclass="RegexExtractorGui" testclass="RegexExtractor" testname="hdr" enabled="true"><stringProp name="RegexExtractor.useHeaders">true</stringProp><stringProp name="RegexExtractor.refname">h</stringProp><stringProp name="RegexExtractor.regex">X-Id: (\d+)</stringProp><stringProp name="RegexExtractor.template">$1$</stringProp></RegexExtractor><hashTree/>`
	tgLevelAssert := `<DurationAssertion guiclass="DurationAssertionGui" testclass="DurationAssertion" testname="Fast" enabled="true"><stringProp name="DurationAssertion.duration">750</stringProp></DurationAssertion><hashTree/>`
	body := tg(loops("1"), tgLevelAssert+once+sampler("up", multipart, headerRegex)+nestedDefaults+pause+stop+jdbc)
	sc, rep := importString(t, plan("S", body, ""))

	if len(sc.Setup) != 1 || sc.Setup[0].Name != "login" {
		t.Errorf("once-only not moved to setup: %+v", sc.Setup)
	}
	if it := findItem(rep, "TestPlan/TG/Once"); it == nil || it.Status != model.Partial {
		t.Errorf("once-only item = %+v", it)
	}
	up := findStep(sc.Steps, "up").Request
	wantParts := []model.Part{{Name: "title", Value: "hi"}, {Name: "file", File: "a.png", ContentType: "image/png"}}
	if !reflect.DeepEqual(up.Multipart, wantParts) || up.URL != "http://h.example/up" {
		t.Errorf("multipart request = %+v", up)
	}
	if len(up.Extract) != 1 || up.Extract[0].Source != model.SrcHeaders || up.Extract[0].Group != 1 {
		t.Errorf("header regex should map to a regex on headers: %+v", up.Extract)
	}
	if it := findItem(rep, "TestPlan/TG/up/hdr"); it == nil || it.Status != model.Partial {
		t.Errorf("header regex item = %+v", it)
	}
	// thread-group-level assertion applies to every sampler in scope, including setup
	if a := up.Assert; len(a) != 1 || a[0] != (model.Assertion{Type: model.AsMaxLatency, Value: "750ms"}) {
		t.Errorf("scoped assertion = %+v", a)
	}
	if it := findItem(rep, "TestPlan/TG/Fast"); it == nil || !strings.Contains(it.Note, "applied to 3 samplers") {
		t.Errorf("scoped assertion item = %+v", it)
	}
	if rel := findStep(sc.Steps, "rel"); rel == nil || rel.Request.URL != "https://other.example:8443/x" {
		t.Errorf("nested defaults URL = %+v", rel)
	}
	if p := findStep(sc.Steps, "Pause"); p == nil || p.Type != model.StepThink || p.Think.Min != dur("1.5s") {
		t.Errorf("pause = %+v", p)
	}
	if it := findItem(rep, "TestPlan/TG/Stop"); it == nil || it.Status != model.Unsupported {
		t.Errorf("stop action item = %+v", it)
	}
	if it := findItem(rep, "TestPlan/TG/DB"); it == nil || it.Status != model.Unsupported {
		t.Errorf("JDBC item = %+v", it)
	}
	if !sc.Defaults.DisableCookies {
		t.Error("cookies should be disabled without a Cookie Manager")
	}
}

func TestIfConditionForms(t *testing.T) {
	im := &importer{sc: model.NewScenario("x"), rep: &model.CompatReport{}, seen: map[string]bool{}}
	cases := []struct{ in, want string }{
		{`"${x}" == "y"`, "${x} == y"},
		{`"${x}" != "y z"`, "${x} != y z"},
		{`'${x}' == 'y'`, "${x} == y"},
		{`${__jexl3("${x}" == "y")}`, "${x} == y"},
		{`${__jexl3("${x}" == "a\,b",)}`, "${x} == a,b"},
		{`${__groovy(vars.get("tok") != null && vars.get("tok") != "")}`, "${tok}"},
	}
	for _, c := range cases {
		got, _, ok := im.ifCondition(c.in, "x")
		if !ok || got != c.want {
			t.Errorf("ifCondition(%q) = %q,%v want %q", c.in, got, ok, c.want)
		}
		exp, ok := exportCond(got)
		if !ok {
			t.Errorf("exportCond(%q) failed", got)
			continue
		}
		back, _, ok := im.ifCondition(exp, "x")
		if !ok || back != got {
			t.Errorf("condition round trip %q -> %q -> %q", got, exp, back)
		}
	}
	for _, bad := range []string{`${x} > 3`, `${__groovy(vars.get("a").size() > 0)}`, `"${x}" == "y" && "${z}" == "w"`, `${flag}`, `${__javaScript("${x}" == "y")}`} {
		if got, _, ok := im.ifCondition(bad, "x"); ok {
			t.Errorf("ifCondition(%q) unexpectedly mapped to %q", bad, got)
		}
	}
}

func TestExportApproximationsReported(t *testing.T) {
	sc := model.NewScenario("approx")
	sc.Workload = model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{
		{Duration: dur("10s"), Target: 5}, {Duration: dur("20s"), Target: 20}, {Duration: dur("10s"), Target: 0}}}
	sc.DataSources = []model.DataSource{{Name: "inline users", Rows: [][]string{{"u", "p"}, {"a", "b"}}, Mode: "random"}}
	sc.SLAs = []model.SLA{{Metric: "p95", Op: "<", Threshold: 500}}
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "r", Request: &model.Request{Method: "GET", URL: "/x",
		Extract: []model.Extractor{{Name: "v", Type: model.ExJSONPath, Expr: "$.v", Required: true}}}}}
	out, rep, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`<stringProp name="filename">inline_users.csv</stringProp>`)) {
		t.Errorf("inline CSV file name not written:\n%s", out)
	}
	want := map[string]string{"workload": model.Partial, "dataSource": model.Partial, "sla": model.Unsupported, "extract.jsonpath": model.Partial}
	for el, st := range want {
		found := false
		for _, it := range rep.Items {
			if it.Element == el {
				found = true
				if it.Status != st {
					t.Errorf("%s status %s, want %s (%s)", el, it.Status, st, it.Note)
				}
			}
		}
		if !found {
			t.Errorf("no export item for %s", el)
		}
	}
	if len(rep.Manual) == 0 {
		t.Error("inline CSV should produce a manual-change entry")
	}
	arr := model.NewScenario("rate")
	arr.Workload = model.Workload{Executor: model.ExecArrivalRate, Rate: 50, Duration: dur("1m"), MaxVUs: 40}
	arr.Steps = sc.Steps
	_, rep, _ = Export(arr)
	ok := false
	for _, it := range rep.Items {
		if it.Element == "workload" && it.Status == model.Partial && strings.Contains(it.Note, "NOT enforced") {
			ok = true
		}
	}
	if !ok {
		t.Errorf("arrival-rate approximation not reported: %+v", rep.Items)
	}
}
