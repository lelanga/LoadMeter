package loadrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

const webDir = "../../testdata/loadrunner/webtours_sample"
const citrixDir = "../../testdata/loadrunner/citrix_sample"

// lineOf returns the 1-based line of the first line in file containing sub.
func lineOf(t *testing.T, dir, file, sub string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, sub) {
			return i + 1
		}
	}
	t.Fatalf("%q not found in %s", sub, file)
	return 0
}

func requests(steps []model.Step) []*model.Step {
	var out []*model.Step
	model.Walk(steps, func(s *model.Step) {
		if s.Type == model.StepRequest {
			out = append(out, s)
		}
	})
	return out
}

func findReq(t *testing.T, sc *model.Scenario, name string) *model.Request {
	t.Helper()
	for _, s := range sc.AllRequests() {
		if s.Name == name {
			return s.Request
		}
	}
	t.Fatalf("request %q not found", name)
	return nil
}

func findItem(rep *model.CompatReport, element, location string) *model.CompatItem {
	for i := range rep.Items {
		if rep.Items[i].Element == element && (location == "" || rep.Items[i].Location == location) {
			return &rep.Items[i]
		}
	}
	return nil
}

func header(r *model.Request, name string) string { return model.HeaderValue(r.Headers, name) }

func TestImportWebToursSample(t *testing.T) {
	sc, rep, err := ImportDir(webDir)
	if err != nil {
		t.Fatal(err)
	}
	if p := sc.Validate(); len(p) != 0 {
		t.Fatalf("scenario does not validate: %v", p)
	}
	// JSON round trip of the IR.
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	var back model.Scenario
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}

	// Structure: vuser_init has no requests, one action group, vuser_end -> teardown.
	if len(requests(sc.Setup)) != 0 {
		t.Fatalf("setup requests = %d", len(requests(sc.Setup)))
	}
	if len(sc.Steps) != 1 || sc.Steps[0].Type != model.StepGroup || sc.Steps[0].Name != "Action" {
		t.Fatalf("steps = %+v", sc.Steps)
	}
	var names []string
	for _, r := range requests(sc.Steps) {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, ","); got != "login-page,login,dashboard,items,create-order,order-status" {
		t.Fatalf("request order = %s", got)
	}
	var txs []string
	model.Walk(sc.Steps, func(s *model.Step) {
		if s.Type == model.StepTransaction {
			txs = append(txs, fmt.Sprintf("%s(%d)", s.Name, len(s.Steps)))
		}
	})
	if got := strings.Join(txs, ","); got != "01_open_login(1),02_login(1),03_search(1),04_order(1)" {
		t.Fatalf("transactions = %s", got)
	}

	// web_url + registration semantics: the hidden token extractor and the
	// "<form" check attach to the login page request.
	lp := findReq(t, sc, "login-page")
	if lp.Method != "GET" || lp.URL != "http://127.0.0.1:8080/login-page" {
		t.Fatalf("login-page = %+v", lp)
	}
	if len(lp.Extract) != 1 || lp.Extract[0] != (model.Extractor{Name: "FormToken", Type: model.ExBoundary,
		Left: `name="__RequestVerificationToken" value="`, Right: `"`, Match: 1, Required: true}) {
		t.Fatalf("login-page extract = %+v", lp.Extract)
	}
	if len(lp.Assert) != 1 || lp.Assert[0] != (model.Assertion{Type: model.AsContains, Value: "<form"}) {
		t.Fatalf("login-page assert = %+v", lp.Assert)
	}
	if header(lp, "Referer") != "" || header(lp, "X-Client") != "loadmeter-sample" {
		t.Fatalf("login-page headers = %+v", lp.Headers)
	}

	// web_submit_data -> form with parameter references.
	lg := findReq(t, sc, "login")
	if lg.Method != "POST" || lg.URL != "http://127.0.0.1:8080/login" {
		t.Fatalf("login = %+v", lg)
	}
	wantForm := []model.KV{{Name: "__RequestVerificationToken", Value: "${FormToken}"}, {Name: "username", Value: "${UserName}"}, {Name: "password", Value: "${Password}"}}
	if fmt.Sprint(lg.Form) != fmt.Sprint(wantForm) {
		t.Fatalf("login form = %+v", lg.Form)
	}
	if header(lg, "Referer") != "http://127.0.0.1:8080/login-page" {
		t.Fatalf("login referer = %+v", lg.Headers)
	}
	if len(lg.Extract) != 2 || lg.Extract[0].Type != model.ExJSONPath || lg.Extract[0].Expr != "$.csrf" || lg.Extract[0].Name != "CsrfToken" ||
		lg.Extract[1].Type != model.ExRegex || lg.Extract[1].Expr != `"token":"([0-9a-f]+)"` || lg.Extract[1].Match != 1 {
		t.Fatalf("login extract = %+v", lg.Extract)
	}
	if len(lg.Assert) != 1 || lg.Assert[0] != (model.Assertion{Type: model.AsNotContains, Value: "invalid credentials"}) {
		t.Fatalf("login assert = %+v", lg.Assert)
	}
	// web_reg_find before dashboard belongs to dashboard (not to login).
	db := findReq(t, sc, "dashboard")
	if len(db.Assert) != 1 || db.Assert[0].Value != "Welcome ${UserName}" {
		t.Fatalf("dashboard assert = %+v", db.Assert)
	}

	items := findReq(t, sc, "items")
	if items.URL != "http://127.0.0.1:8080/api/items?q=widget&run=${__timestamp(yyyyMMdd-HHmmss)}" {
		t.Fatalf("items url = %s", items.URL)
	}
	if len(items.Extract) != 1 || items.Extract[0].Required || items.Extract[0].Left != `"id":"` || items.Extract[0].Match != 1 {
		t.Fatalf("items extract (NotFound=warning) = %+v", items.Extract)
	}

	// web_custom_request: body concatenation, EncType -> Content-Type,
	// web_add_header applies to this request only.
	co := findReq(t, sc, "create-order")
	if co.Method != "POST" || co.Body != `{"item":"${FirstSku}","qty":${__randomInt(1,5)}}` {
		t.Fatalf("create-order = %+v", co)
	}
	if header(co, "Content-Type") != "application/json" || header(co, "X-CSRF-Token") != "${CsrfToken}" {
		t.Fatalf("create-order headers = %+v", co.Headers)
	}
	if len(co.Extract) != 1 || co.Extract[0].Expr != "$.orderId" || len(co.Assert) != 1 || co.Assert[0].Value != `"status":"created"` {
		t.Fatalf("create-order extract/assert = %+v %+v", co.Extract, co.Assert)
	}
	st := findReq(t, sc, "order-status")
	if header(st, "X-CSRF-Token") != "" {
		t.Fatal("web_add_header leaked into a later request")
	}
	if st.URL != "http://127.0.0.1:8080/api/orders/${LastOrder}" {
		t.Fatalf("order-status url = %s", st.URL)
	}
	// lr_save_string(lr_eval_string("{OrderId}"), "LastOrder") -> set step.
	var set *model.Step
	model.Walk(sc.Steps, func(s *model.Step) {
		if s.Type == model.StepSetVar {
			set = s
		}
	})
	if set == nil || len(set.Set) != 1 || set.Set[0] != (model.KV{Name: "LastOrder", Value: "${OrderId}"}) {
		t.Fatalf("set step = %+v", set)
	}

	// Think time: RANDOM 50..150 %.
	var thinks []string
	model.Walk(sc.Steps, func(s *model.Step) {
		if s.Type == model.StepThink {
			thinks = append(thinks, s.Think.Min.D().String()+"-"+s.Think.Max.D().String())
		}
	})
	if got := strings.Join(thinks, ","); got != "1s-3s,1.5s-4.5s,500ms-1.5s" {
		t.Fatalf("think steps = %s", got)
	}

	// Teardown: auto header still active, removed afterwards.
	td := requests(sc.Teardown)
	if len(td) != 1 || td[0].Request.URL != "http://127.0.0.1:8080/" || header(td[0].Request, "X-Client") != "loadmeter-sample" {
		t.Fatalf("teardown = %+v", sc.Teardown)
	}

	// Parameters: table data source with two parameters on the same line.
	if len(sc.DataSources) != 1 {
		t.Fatalf("data sources = %+v", sc.DataSources)
	}
	ds := sc.DataSources[0]
	if ds.Name != "users" || ds.Columns[0] != "UserName" || ds.Columns[1] != "Password" || len(ds.Rows) != 3 ||
		ds.Rows[1][0] != "bob" || ds.Rows[1][1] != "secret" || ds.Mode != "sequential" || ds.Scope != "iteration" ||
		ds.OnExhaust != "recycle" || !ds.Sensitive {
		t.Fatalf("data source = %+v", ds)
	}
	if sc.Variables["LoadGroup"] != "group1" {
		t.Fatalf("variables = %+v", sc.Variables)
	}

	// Run-time settings.
	w := sc.Workload
	if w.Executor != model.ExecPerVUIterations || w.Iterations != 3 || w.VUs != 1 || w.Pacing.D() != 10*time.Second ||
		w.ThinkTimeScale == nil || *w.ThinkTimeScale != 1 {
		t.Fatalf("workload = %+v", w)
	}
	if sc.Defaults.Timeout.D() != time.Minute {
		t.Fatalf("timeout = %v", sc.Defaults.Timeout.D())
	}

	// Compatibility report: unsupported items carry file:line.
	loc := regexp.MustCompile(`^[A-Za-z0-9_.-]+:\d+$`)
	unsupported := 0
	for _, it := range rep.Items {
		if it.Status == model.Unsupported {
			unsupported++
			if !loc.MatchString(it.Location) {
				t.Errorf("unsupported item without file:line: %+v", it)
			}
		}
	}
	if unsupported != 3 {
		t.Errorf("unsupported items = %d, want 3 (declaration, for, lrs_send)", unsupported)
	}
	forLine := lineOf(t, webDir, "Action.c", "for (i = 0;")
	it := findItem(rep, "for", fmt.Sprintf("Action.c:%d", forLine))
	if it == nil || it.Status != model.Unsupported || !strings.Contains(it.Note, "1 HTTP request call(s) inside were NOT imported") ||
		!strings.Contains(it.Note, "atoi(lr_eval_string") {
		t.Fatalf("for item = %+v", it)
	}
	if it := findItem(rep, "declaration", fmt.Sprintf("Action.c:%d", lineOf(t, webDir, "Action.c", "int i;"))); it == nil {
		t.Fatal("declaration not reported at its line")
	}
	lrsLine := lineOf(t, webDir, "Action.c", "lrs_send(")
	if it := findItem(rep, "lrs_send", fmt.Sprintf("Action.c:%d", lrsLine)); it == nil || !strings.Contains(it.Note, "Windows Sockets") {
		t.Fatalf("lrs_send item = %+v", it)
	}
	if it := findItem(rep, "EXTRARES", ""); it == nil || it.Status != model.Partial || it.Location != fmt.Sprintf("Action.c:%d", lineOf(t, webDir, "Action.c", `web_url("login-page"`)) {
		t.Fatalf("EXTRARES item = %+v", it)
	}
	// Ignored attributes are reported once per type.
	n := 0
	for _, it := range rep.Items {
		if it.Element == "web_url Snapshot=" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Snapshot= reported %d times, want 1", n)
	}
	if it := findItem(rep, "global declaration", "globals.h:"+fmt.Sprint(lineOf(t, webDir, "globals.h", "g_orders_placed"))); it == nil || it.Status != model.Partial {
		t.Fatalf("globals.h item = %+v", it)
	}
	if rep.Format != "loadrunner" || rep.Source != "loadmeter_shop.usr" || rep.Confidence == "" || rep.Validated != "not-run" {
		t.Fatalf("report header = %+v", rep)
	}
	if len(rep.Manual) < 2 {
		t.Fatalf("manual changes = %v", rep.Manual)
	}
	if sc.Metadata["portability"] != PortableWithChanges || sc.Metadata["importer"] != "loadrunner" {
		t.Fatalf("metadata = %v", sc.Metadata)
	}
	t.Logf("report counts: %v confidence=%s items=%d warnings=%d manual=%d", rep.Counts(), rep.Confidence, len(rep.Items), len(rep.Warnings), len(rep.Manual))
}

func TestAnalyzeWebToursSample(t *testing.T) {
	an, err := AnalyzeDir(webDir)
	if err != nil {
		t.Fatal(err)
	}
	if an.Assessment != PortableWithChanges || !an.WebProtocol {
		t.Fatalf("assessment = %s (%v)", an.Assessment, an.Reasons)
	}
	if an.RuntimeFunctions["web_url"] != 6 || an.RuntimeFunctions["lrs_send"] != 1 || an.RuntimeFunctions["lr_eval_string"] != 3 ||
		an.RuntimeFunctions["lr_param_sprintf"] != 1 || an.ProtocolFunctions["Windows Sockets"] != 1 {
		t.Fatalf("runtime functions = %v / %v", an.RuntimeFunctions, an.ProtocolFunctions)
	}
	if an.Requests != 7 {
		t.Fatalf("requests = %d", an.Requests)
	}
	var corr []string
	for _, c := range an.Correlations {
		corr = append(corr, c.Param+":"+c.Kind)
	}
	if got := strings.Join(corr, ","); got != "FormToken:boundary,CsrfToken:jsonpath,AuthToken:regex,FirstSku:boundary,OrderId:jsonpath" {
		t.Fatalf("correlations = %s", got)
	}
	if strings.Join(an.Transactions, ",") != "01_open_login,02_login,03_search,04_order" {
		t.Fatalf("transactions = %v", an.Transactions)
	}
	if len(an.Parameters) != 7 {
		t.Fatalf("parameters = %+v", an.Parameters)
	}
	if strings.Join(an.ExternalFiles, ",") != "users.dat" {
		t.Fatalf("external files = %v", an.ExternalFiles)
	}
	kinds := map[string]int{}
	for _, f := range an.CustomCode {
		kinds[f.Kind]++
	}
	if kinds["control-flow"] != 1 || kinds["declaration"] != 1 || kinds["global"] != 1 {
		t.Fatalf("custom code = %+v", an.CustomCode)
	}
	var action *FileFindings
	for _, f := range an.Files {
		if f.File == "Action.c" {
			action = f
		}
	}
	if action == nil || action.Role != "action" || action.RuntimeFunctions["web_reg_find"] != 4 || len(action.Transactions) != 4 ||
		len(action.Correlations) != 5 || len(action.Parameters) == 0 {
		t.Fatalf("Action.c findings = %+v", action)
	}
	roles := map[string]string{}
	for _, f := range an.Files {
		roles[f.File] = f.Role
	}
	for file, role := range map[string]string{"loadmeter_shop.usr": "usr", "vuser_init.c": "init", "vuser_end.c": "end", "globals.h": "globals",
		"loadmeter_shop.prm": "parameters", "users.dat": "data", "default.cfg": "runtime-settings", "default.usp": "runtime-settings"} {
		if roles[file] != role {
			t.Errorf("role of %s = %q, want %q", file, roles[file], role)
		}
	}
	t.Logf("assessment=%s reasons=%v", an.Assessment, an.Reasons)
}

func TestImportNonWebProtocol(t *testing.T) {
	sc, rep, err := ImportDir(citrixDir)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("err = %v", err)
	}
	if sc != nil || rep == nil {
		t.Fatalf("want nil scenario and a report, got %v %v", sc, rep)
	}
	if it := findItem(rep, "protocol", "citrix_sample.usr"); it == nil || it.Status != model.Unsupported || it.Name != "Citrix_ICA" {
		t.Fatalf("protocol item = %+v", it)
	}
	if it := findItem(rep, "ctrx_type", fmt.Sprintf("Action.c:%d", lineOf(t, citrixDir, "Action.c", "ctrx_type"))); it == nil {
		t.Fatalf("ctrx_type not reported with file:line: %+v", rep.Items)
	}
	if rep.Confidence != "low" {
		t.Fatalf("confidence = %s", rep.Confidence)
	}
	an, err := AnalyzeDir(citrixDir)
	if err != nil {
		t.Fatal(err)
	}
	if an.Assessment != NotPortable || an.WebProtocol || an.ProtocolFunctions["Citrix ICA"] != 7 || an.RuntimeFunctions["ctrx_set_connect_opt"] != 2 {
		t.Fatalf("analysis = %+v", an)
	}
}

// --- focused unit tests over in-memory files ----------------------------------

func importAction(t *testing.T, action string, extra map[string]string) (*model.Scenario, *model.CompatReport) {
	t.Helper()
	files := map[string][]byte{
		"t.usr":    []byte("[General]\nType=QTWeb\n[Actions]\nvuser_init=vuser_init.c\nAction=Action.c\nvuser_end=vuser_end.c\n"),
		"Action.c": []byte(action),
	}
	for k, v := range extra {
		files[k] = []byte(v)
	}
	sc, rep, err := ImportFiles(files, "t")
	if err != nil {
		t.Fatal(err)
	}
	return sc, rep
}

func TestCountedLoopBecomesLoopStep(t *testing.T) {
	sc, rep := importAction(t, `Action()
{
	int i;
	for (i = 1; i <= 3; i++) {
		web_url("poll", "URL=http://h/poll", LAST);
		lr_think_time(1);
	}
	return 0;
}`, nil)
	g := sc.Steps[0].Steps
	if len(g) != 1 || g[0].Type != model.StepLoop || g[0].Count != 3 || len(g[0].Steps) != 2 {
		t.Fatalf("loop = %+v", g)
	}
	if it := findItem(rep, "for", "Action.c:4"); it == nil || it.Status != model.Partial {
		t.Fatalf("for item = %+v", it)
	}
	// A loop whose body uses the variable is not converted.
	sc, rep = importAction(t, `Action(){ int i; for (i = 0; i < 2; i++) { lr_save_int(i, "n"); web_url("a", "URL=http://h/", LAST); } return 0; }`, nil)
	if it := findItem(rep, "for", "Action.c:1"); it == nil || it.Status != model.Unsupported {
		t.Fatalf("for item = %+v", it)
	}
	if sc.Steps != nil {
		t.Fatalf("steps = %+v", sc.Steps)
	}
}

func TestParamRefsAndArrays(t *testing.T) {
	sc, rep := importAction(t, `Action()
{
	web_reg_save_param("Ids", "LB=id=", "RB=;", "Ord=ALL", LAST);
	web_url("list", "URL=http://h/list", LAST);
	web_custom_request("x", "URL=http://h/x?first={Ids_1}&n={Ids_count}&u={Nope}", "Method=PUT",
		"Body={\"a\":{\"b\":1}} {Nope}", LAST);
	return 0;
}`, nil)
	l := findReq(t, sc, "list")
	if len(l.Extract) != 1 || l.Extract[0].Name != "Ids_1" || l.Extract[0].Match != 1 {
		t.Fatalf("Ord=ALL extract = %+v", l.Extract)
	}
	if it := findItem(rep, "web_reg_save_param", "Action.c:3"); it == nil || it.Status != model.Partial {
		t.Fatalf("Ord=ALL item = %+v", it)
	}
	x := findReq(t, sc, "x")
	if x.URL != "http://h/x?first=${Ids_1}&n=${Ids_count}&u={Nope}" || x.Method != "PUT" {
		t.Fatalf("url = %s", x.URL)
	}
	if x.Body != `{"a":{"b":1}} {Nope}` {
		t.Fatalf("body = %s", x.Body)
	}
	warned := 0
	for _, w := range rep.Warnings {
		if strings.Contains(w, "{Nope}") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("warnings = %v", rep.Warnings)
	}
}

func TestSubmitDataVariants(t *testing.T) {
	sc, rep := importAction(t, `Action()
{
	web_submit_data("upload", "Action=http://h/upload", "Method=POST", "EncType=multipart/form-data",
		ITEMDATA,
		"Name=title", "Value=hello", ENDITEM,
		"Name=file", "Value=report.pdf", "File=Yes", "ContentType=application/pdf", ENDITEM,
		LAST);
	web_submit_data("search", "Action=http://h/search?x=1", "Method=GET",
		ITEMDATA, "Name=q", "Value=a b&c", ENDITEM, LAST);
	return 0;
}`, nil)
	up := findReq(t, sc, "upload")
	if len(up.Multipart) != 2 || up.Multipart[0] != (model.Part{Name: "title", Value: "hello"}) ||
		up.Multipart[1] != (model.Part{Name: "file", File: "report.pdf", ContentType: "application/pdf"}) || len(up.Form) != 0 {
		t.Fatalf("multipart = %+v", up)
	}
	if it := findItem(rep, "web_submit_data", "Action.c:3"); it == nil || it.Status != model.Partial {
		t.Fatalf("upload item = %+v", it)
	}
	s := findReq(t, sc, "search")
	if s.Method != "GET" || s.URL != "http://h/search?x=1&q=a+b%26c" || len(s.Form) != 0 {
		t.Fatalf("GET form = %+v", s)
	}
}

func TestBodyBinaryAndSubmitForm(t *testing.T) {
	sc, rep := importAction(t, `Action()
{
	web_url("page", "URL=http://h/page", LAST);
	web_submit_form("submit", ITEMDATA, "Name=q", "Value=x", ENDITEM, LAST);
	web_custom_request("bin", "URL=http://h/bin", "Method=POST", "EncType=application/octet-stream",
		"BodyBinary=\\x41\\x42C", LAST);
	web_custom_request("file", "URL=http://h/f", "Method=POST", "BodyFilePath=payload.json", LAST);
	return 0;
}`, nil)
	bin := findReq(t, sc, "bin")
	if bin.Body != "ABC" || header(bin, "Content-Type") != "application/octet-stream" {
		t.Fatalf("bin = %+v", bin)
	}
	var form *model.Step
	for _, s := range sc.AllRequests() {
		if s.Name == "submit" {
			form = s
		}
	}
	if form == nil || !form.Disabled || form.Request.URL != "http://h/page" || form.Request.Method != "POST" || len(form.Request.Form) != 1 {
		t.Fatalf("submit_form = %+v", form)
	}
	if it := findItem(rep, "web_submit_form", "Action.c:4"); it == nil || it.Status != model.Partial {
		t.Fatalf("submit_form item = %+v", it)
	}
	if it := findItem(rep, "web_custom_request", "Action.c:7"); it == nil || it.Status != model.Partial || !strings.Contains(it.Note, "payload.json") {
		t.Fatalf("BodyFilePath item = %+v", it)
	}
	if p := sc.Validate(); len(p) != 0 {
		t.Fatalf("validate: %v", p)
	}
}

func TestTransactionsUnbalancedAndFindVariants(t *testing.T) {
	sc, rep := importAction(t, `Action()
{
	lr_start_transaction("outer");
	lr_start_transaction("inner");
	web_reg_find("Text=ok", "SaveCount=okCount", LAST);
	web_reg_find("TextPfx=<b>", "TextSfx=</b>", LAST);
	web_reg_find("Text/IC=welcome", LAST);
	web_url("a", "URL=http://h/a", LAST);
	lr_end_transaction("outer", LR_FAIL);
	lr_end_transaction("ghost", LR_AUTO);
	lr_start_transaction("open");
	web_url("b", "URL=http://h/b", LAST);
	web_reg_save_param_ex("ParamName=Loc", SEARCH_FILTERS, "Scope=Headers", "HeaderNames=Location", LAST);
	return 0;
}`, nil)
	g := sc.Steps[0].Steps
	if len(g) != 2 || g[0].Name != "outer" || g[0].Steps[0].Name != "inner" || g[1].Name != "open" {
		t.Fatalf("structure = %+v", g)
	}
	a := findReq(t, sc, "a")
	if len(a.Assert) != 2 || a.Assert[0].Type != model.AsRegex || a.Assert[0].Expr != `<b>(?s:.*?)</b>` ||
		a.Assert[1].Type != model.AsRegex || a.Assert[1].Expr != `(?i)welcome` {
		t.Fatalf("asserts = %+v", a.Assert)
	}
	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{`transaction "inner" closed implicitly`, `lr_end_transaction("ghost")`, `transaction "open" is never ended`, "Action.c:13: registration"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}
	if it := findItem(rep, "lr_end_transaction", "Action.c:9"); it == nil || !strings.Contains(it.Note, "LR_FAIL") {
		t.Fatalf("forced status item = %+v", it)
	}
}

func TestHeaderScopeExtractor(t *testing.T) {
	sc, _ := importAction(t, `Action(){
	web_reg_save_param_ex("ParamName=Loc", SEARCH_FILTERS, "Scope=Headers", "HeaderNames=Location", LAST);
	web_url("a", "URL=http://h/a", LAST);
	return 0; }`, nil)
	a := findReq(t, sc, "a")
	if len(a.Extract) != 1 || a.Extract[0].Type != model.ExHeader || a.Extract[0].Expr != "Location" {
		t.Fatalf("extract = %+v", a.Extract)
	}
}

func TestCustomFunctionsAndSetUser(t *testing.T) {
	sc, rep := importAction(t, `int helper(char *s)
{
	return strlen(s);
}

Action()
{
	web_set_user("bob", "pw", "h:80");
	helper("x");
	strcpy(buf, "y");
	web_url("a", "URL=http://h/a", LAST);
	return 1;
}`, nil)
	if it := findItem(rep, "custom function", "Action.c:1"); it == nil || it.Status != model.Unsupported {
		t.Fatalf("custom function item = %+v", it)
	}
	if it := findItem(rep, "helper", "Action.c:9"); it == nil || it.Status != model.Unsupported {
		t.Fatalf("helper call item = %+v", it)
	}
	if it := findItem(rep, "strcpy", "Action.c:10"); it == nil || it.Status != model.Unsupported {
		t.Fatalf("strcpy item = %+v", it)
	}
	if it := findItem(rep, "return", "Action.c:12"); it == nil || it.Status != model.Partial {
		t.Fatalf("return item = %+v", it)
	}
	if model.HeaderValue(sc.Defaults.Headers, "Authorization") != "Basic Ym9iOnB3" {
		t.Fatalf("auth header = %+v", sc.Defaults.Headers)
	}
}

func TestParameterPolicies(t *testing.T) {
	prm := `[parameter:Acct]
Type="Table"
Table="accts.dat"
ColumnName="Col 1"
Delimiter="tab"
SelectNextRow="Unique"
UpdateValueOn="Once"
OutOfRangePolicy="AbortVuser"
[parameter:Code]
Type="Table"
Table="codes.dat"
ColumnName="code"
SelectNextRow="Random"
UpdateValueOn="Each occurrence"
[parameter:Missing]
Type="Table"
Table="nowhere.dat"
[parameter:Seq]
Type="UniqueNumber"
[parameter:Odd]
Type="UserDefinedFunction"
`
	sc, rep := importAction(t, `Action(){ web_url("a", "URL=http://h/{Acct}/{Code}/{Seq}/{Odd}", LAST); return 0; }`,
		map[string]string{"t.prm": prm, "accts.dat": "acct\tname\nA1\tx\nA2\ty\n", "codes.dat": "code\nC1\n"})
	if len(sc.DataSources) != 2 {
		t.Fatalf("data sources = %+v", sc.DataSources)
	}
	a, c := sc.DataSources[0], sc.DataSources[1]
	if a.Name != "accts" || a.Mode != "per-vu" || a.Scope != "vu" || a.OnExhaust != "stop" || a.Delimiter != "\t" || a.Columns[0] != "Acct" || a.Rows[1][0] != "A2" {
		t.Fatalf("accts = %+v", a)
	}
	if c.Mode != "random" || c.Columns[0] != "Code" {
		t.Fatalf("codes = %+v", c)
	}
	if u := findReq(t, sc, "a").URL; u != "http://h/${Acct}/${Code}/${__counter()}/${Odd}" {
		t.Fatalf("url = %s", u)
	}
	for el, st := range map[string]string{"parameter:UniqueNumber": model.Partial, "parameter:UserDefinedFunction": model.Unsupported} {
		if it := findItem(rep, el, ""); it == nil || it.Status != st {
			t.Errorf("%s = %+v", el, it)
		}
	}
	if it := findItem(rep, "parameter:Table", "t.prm:15"); it == nil || it.Status != model.Unsupported || it.Name != "Missing" {
		t.Fatalf("missing table = %+v", it)
	}
}

// The official file list names init.c / run.c / end.c; scripts without a
// .usr and with path-prefixed, differently-cased upload keys must still import.
func TestOfficialFileNamesWithoutUsr(t *testing.T) {
	files := map[string][]byte{
		"upload/INIT.C": []byte(`init(){ web_url("i", "URL=http://h/i", LAST); return 0; }`),
		"upload/run.c":  []byte(`run(){ web_url("r", "URL=http://h/r", LAST); return 0; }`),
		`upload\end.c`:  []byte(`end(){ web_url("e", "URL=http://h/e", LAST); return 0; }`),
		"upload/vdf.h":  []byte("#ifndef VDF_H\n#define VDF_H\n#endif\n"),
	}
	sc, rep, err := ImportFiles(files, "upload")
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Setup) != 1 || sc.Steps[0].Name != "run" || len(sc.Teardown) != 1 || sc.Name != "upload" {
		t.Fatalf("scenario = %+v", sc)
	}
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "no .usr file") {
		t.Fatalf("warnings = %v", rep.Warnings)
	}
	if _, _, err := ImportFiles(map[string][]byte{"readme.txt": []byte("x")}, "x"); !errors.Is(err, ErrNoScript) {
		t.Fatalf("err = %v", err)
	}
}

func TestStrftimeToJava(t *testing.T) {
	for in, want := range map[string]string{"%Y-%m-%d %H:%M:%S": "yyyy-MM-dd HH:mm:ss", "%d/%m/%Y": "dd/MM/yyyy"} {
		if got, ok := strftimeToJava(in); !ok || got != want {
			t.Errorf("%q -> %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"%b %d", "%Y,%m", "T%H"} {
		if _, ok := strftimeToJava(bad); ok {
			t.Errorf("%q should not convert", bad)
		}
	}
}
