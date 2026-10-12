package engine

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/model"
)

func ds(name, mode string, rows ...string) model.DataSource {
	d := model.DataSource{Name: name, Mode: mode, Columns: []string{"user"}}
	for _, r := range rows {
		d.Rows = append(d.Rows, []string{r})
	}
	return d
}

func qOf(e Exchange) string {
	i := strings.Index(e.URL, "q=")
	if i < 0 {
		return ""
	}
	return e.URL[i+2:]
}

func TestDataFeedModes(t *testing.T) {
	t.Parallel()
	step := get("item", "/api/items?q=${user}")

	t.Run("sequential recycles", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(1, 7), step)
		sc.DataSources = []model.DataSource{ds("d", "sequential", "a", "b", "c")}
		runSc(t, sc, opts(rec))
		var got []string
		for _, e := range rec.all() {
			got = append(got, qOf(e))
		}
		if strings.Join(got, ",") != "a,b,c,a,b,c,a" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("default mode is sequential", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(1, 3), step)
		sc.DataSources = []model.DataSource{ds("d", "", "x", "y")}
		runSc(t, sc, opts(rec))
		var got []string
		for _, e := range rec.all() {
			got = append(got, qOf(e))
		}
		if strings.Join(got, ",") != "x,y,x" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("sequential shared across VUs", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(4, 3), step)
		sc.DataSources = []model.DataSource{ds("d", "sequential", "r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10", "r11", "r12")}
		runSc(t, sc, opts(rec))
		seen := map[string]int{}
		for _, e := range rec.all() {
			seen[qOf(e)]++
		}
		if len(seen) != 12 {
			t.Errorf("12 iterations should consume 12 distinct rows, got %v", seen)
		}
	})

	t.Run("random", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(1, 40), step)
		sc.DataSources = []model.DataSource{ds("d", "random", "a", "b", "c")}
		runSc(t, sc, opts(rec))
		seen := map[string]bool{}
		for _, e := range rec.all() {
			q := qOf(e)
			if q != "a" && q != "b" && q != "c" {
				t.Fatalf("unexpected value %q", q)
			}
			seen[q] = true
		}
		if len(seen) < 2 {
			t.Errorf("random mode returned only %v over 40 iterations", seen)
		}
	})

	t.Run("unique stops on exhaust", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, constVUs(2, 5*time.Second), step)
		sc.DataSources = []model.DataSource{ds("d", "unique", "u1", "u2", "u3")}
		start := time.Now()
		_, snap := runSc(t, sc, opts(rec))
		if time.Since(start) > 3*time.Second {
			t.Errorf("run should end when data is exhausted, took %v", time.Since(start))
		}
		seen := map[string]int{}
		for _, e := range rec.all() {
			seen[qOf(e)]++
		}
		if len(seen) != 3 || seen["u1"] != 1 || seen["u2"] != 1 || seen["u3"] != 1 {
			t.Errorf("each row exactly once: %v", seen)
		}
		if snap.Iterations != 3 {
			t.Errorf("iterations=%d", snap.Iterations)
		}
	})

	t.Run("per-vu", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(3, 3), step)
		sc.DataSources = []model.DataSource{ds("d", "per-vu", "alice", "bob", "carol")}
		runSc(t, sc, opts(rec))
		want := map[int]string{1: "alice", 2: "bob", 3: "carol"}
		for _, e := range rec.all() {
			if qOf(e) != want[e.VU] {
				t.Errorf("vu %d got %q want %q", e.VU, qOf(e), want[e.VU])
			}
		}
		if len(rec.all()) != 9 {
			t.Errorf("exchanges=%d", len(rec.all()))
		}
	})

	t.Run("per-vu recycles when VUs exceed rows", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(3, 1), step)
		sc.DataSources = []model.DataSource{ds("d", "per-vu", "alice", "bob")}
		runSc(t, sc, opts(rec))
		for _, e := range rec.all() {
			want := []string{"alice", "bob"}[(e.VU-1)%2]
			if qOf(e) != want {
				t.Errorf("vu %d got %q want %q", e.VU, qOf(e), want)
			}
		}
	})

	t.Run("per-vu stop on exhaust", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(3, 1), step)
		d := ds("d", "per-vu", "alice", "bob")
		d.OnExhaust = "stop"
		sc.DataSources = []model.DataSource{d}
		runSc(t, sc, opts(rec))
		if len(rec.all()) != 2 {
			t.Errorf("VU3 has no row and must stop: %d exchanges", len(rec.all()))
		}
	})

	t.Run("onExhaust error fails iterations but continues", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		sc := newSc(srv.URL, perVU(1, 4), step)
		d := ds("d", "sequential", "a", "b")
		d.OnExhaust = "error"
		sc.DataSources = []model.DataSource{d}
		_, snap := runSc(t, sc, opts(nil))
		if snap.Iterations != 4 || snap.IterFailures != 2 {
			t.Errorf("iterations=%d failures=%d", snap.Iterations, snap.IterFailures)
		}
		if snap.Requests.Count != 2 {
			t.Errorf("requests=%d", snap.Requests.Count)
		}
	})

	t.Run("scope vu keeps one row per VU", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(2, 4), step)
		d := ds("d", "sequential", "a", "b", "c", "d")
		d.Scope = "vu"
		sc.DataSources = []model.DataSource{d}
		runSc(t, sc, opts(rec))
		rows := map[int]map[string]bool{}
		for _, e := range rec.all() {
			if rows[e.VU] == nil {
				rows[e.VU] = map[string]bool{}
			}
			rows[e.VU][qOf(e)] = true
		}
		for vu, m := range rows {
			if len(m) != 1 {
				t.Errorf("vu %d used rows %v", vu, m)
			}
		}
		if rows[1] != nil && rows[2] != nil {
			for k := range rows[1] {
				if rows[2][k] {
					t.Errorf("VUs share a row: %v %v", rows[1], rows[2])
				}
			}
		}
	})

	t.Run("csv file with header and qualified names", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "users.csv"), []byte("user,pass\nann,p1\nbo,p2\n"), 0o600)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(1, 2), get("item", "/api/items?q=${people.user}-${pass}"))
		sc.DataSources = []model.DataSource{{Name: "people", File: "users.csv"}}
		o := opts(rec)
		o.BaseDir = dir
		runSc(t, sc, o)
		var got []string
		for _, e := range rec.all() {
			got = append(got, qOf(e))
		}
		if strings.Join(got, ",") != "ann-p1,bo-p2" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("csv escape rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		sc := newSc("http://127.0.0.1:1", perVU(1, 1), step)
		sc.DataSources = []model.DataSource{{Name: "x", File: "../outside.csv"}}
		o := opts(nil)
		o.BaseDir = dir
		if _, err := Prepare(sc, o); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Errorf("err=%v", err)
		}
		sc.DataSources = []model.DataSource{{Name: "x", File: "/etc/passwd"}}
		if _, err := Prepare(sc, o); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("custom delimiter", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "d.csv"), []byte("user;n\nz1;1\n"), 0o600)
		rec := &recorder{}
		sc := newSc(srv.URL, perVU(1, 1), step)
		sc.DataSources = []model.DataSource{{Name: "d", File: "d.csv", Delimiter: ";"}}
		o := opts(rec)
		o.BaseDir = dir
		runSc(t, sc, o)
		if e := rec.all(); len(e) != 1 || qOf(e[0]) != "z1" {
			t.Errorf("%+v", e)
		}
	})
}

func TestSafePath(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		rel string
		ok  bool
	}{
		{"a.txt", true}, {"sub/a.txt", true}, {"sub/../a.txt", true}, {"./a.txt", true},
		{"../a.txt", false}, {"sub/../../a.txt", false}, {"..", false}, {"/etc/passwd", false}, {"", false},
		{"..hidden", true},
	}
	for _, tc := range tests {
		p, err := SafePath(base, tc.rel)
		if (err == nil) != tc.ok {
			t.Errorf("SafePath(%q) err=%v want ok=%v", tc.rel, err, tc.ok)
		}
		if err == nil && !strings.HasPrefix(p, base) {
			t.Errorf("SafePath(%q)=%q outside base", tc.rel, p)
		}
	}
}

func TestExtractors(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	login := model.Step{Type: model.StepRequest, Name: "login", Request: &model.Request{
		Method: "POST", URL: "/login", Headers: []model.Header{{Name: "Content-Type", Value: "application/json"}},
		Body: `{"username":"bob","password":"secret"}`,
		Extract: []model.Extractor{
			{Name: "tok", Type: model.ExJSONPath, Expr: "$.token", Required: true},
			{Name: "csrfH", Type: model.ExHeader, Expr: "X-CSRF-Token", Required: true},
			{Name: "sid", Type: model.ExCookie, Expr: "SESSIONID", Required: true},
			{Name: "csrfR", Type: model.ExRegex, Expr: `"csrf":"(\w+)"`, Required: true},
			{Name: "tokB", Type: model.ExBoundary, Left: `"token":"`, Right: `"`, Required: true},
			{Name: "name", Type: model.ExJSONPath, Expr: "$.user.name"},
			{Name: "code", Type: model.ExStatus},
			{Name: "nope", Type: model.ExJSONPath, Expr: "$.nope", Default: "dflt"},
		}}}
	profile := withAssert(reqStep("profile", "GET", "/api/profile"), model.Assertion{Type: model.AsStatus, Value: "200"}, model.Assertion{Type: model.AsJSONPath, Expr: "$.name", Value: "${name}"})
	profile.Request.Headers = []model.Header{{Name: "Authorization", Value: "Bearer ${tokB}"}}
	soap := model.Step{Type: model.StepRequest, Name: "soap", Request: &model.Request{
		Method: "POST", URL: "/soap", Body: "<Envelope><x/></Envelope>",
		Extract: []model.Extractor{
			{Name: "rid", Type: model.ExXPath, Expr: "//RequestId", Required: true},
			{Name: "len", Type: model.ExXPath, Expr: "/Envelope/Body/EchoResponse/Length/text()", Required: true},
			{Name: "rid2", Type: model.ExRegex, Expr: `<RequestId>(.*?)</RequestId>`, Required: true},
		}}}
	dash := withAssert(get("dash", "/dashboard"), model.Assertion{Type: model.AsContains, Value: "Welcome ${name}"})
	sc := newSc(srv.URL, perVU(1, 1), login, profile, soap, dash)
	_, snap := runSc(t, sc, opts(rec))
	if snap.Requests.Errors != 0 {
		for _, e := range rec.all() {
			t.Logf("%s ok=%v err=%s missing=%v", e.Step, e.OK, e.Error, e.Missing)
		}
		t.Fatalf("errors: %+v", snap.Requests.ErrorKind)
	}
	l := rec.byStep("login")[0]
	for _, k := range []string{"tok", "csrfH", "sid", "csrfR", "tokB", "name", "code"} {
		if l.Extracted[k] == "" {
			t.Errorf("extractor %s empty: %v", k, l.Extracted)
		}
	}
	if l.Extracted["tok"] != l.Extracted["tokB"] || l.Extracted["csrfH"] != l.Extracted["csrfR"] {
		t.Errorf("extractors disagree: %v", l.Extracted)
	}
	if l.Extracted["name"] != "bob" || l.Extracted["code"] != "200" {
		t.Errorf("name/code %v", l.Extracted)
	}
	if !has(l.Missing, "nope") {
		t.Errorf("missing=%v", l.Missing)
	}
	if p := rec.byStep("profile")[0]; !has(p.VarsUsed, "tokB") || p.ReqHeaders["Authorization"] != "Bearer "+l.Extracted["tok"] {
		t.Errorf("bearer not propagated: %+v", p)
	}
	s := rec.byStep("soap")[0]
	if s.Extracted["rid"] == "" || s.Extracted["rid"] != s.Extracted["rid2"] {
		t.Errorf("soap extraction %v", s.Extracted)
	}
	if s.Extracted["len"] != strconv.Itoa(len("<Envelope><x/></Envelope>")) {
		t.Errorf("soap length %v", s.Extracted)
	}
}

func TestExtractDefaultVariableUsed(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	s1 := withExtract(get("a", "/health"), model.Extractor{Name: "v", Type: model.ExJSONPath, Expr: "$.zzz", Default: "fallback"})
	s2 := get("b", "/api/items?q=${v}")
	sc := newSc(srv.URL, perVU(1, 1), s1, s2)
	_, snap := runSc(t, sc, opts(rec))
	if snap.Requests.Errors != 0 {
		t.Fatalf("non-required missing extraction must not fail: %+v", snap.Requests.ErrorKind)
	}
	if qOf(rec.byStep("b")[0]) != "fallback" {
		t.Errorf("default not applied: %s", rec.byStep("b")[0].URL)
	}
}

func TestExtractionMatchSelection(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	s1 := withExtract(get("a", "/api/items"),
		model.Extractor{Name: "first", Type: model.ExJSONPath, Expr: "$.items[*].id"},
		model.Extractor{Name: "third", Type: model.ExJSONPath, Expr: "$.items[*].id", Match: 3},
		model.Extractor{Name: "last", Type: model.ExJSONPath, Expr: "$.items[-1].id"},
		model.Extractor{Name: "rnd", Type: model.ExJSONPath, Expr: "$.items[*].id", Match: -1},
		model.Extractor{Name: "oob", Type: model.ExJSONPath, Expr: "$.items[*].id", Match: 99})
	sc := newSc(srv.URL, perVU(1, 1), s1)
	runSc(t, sc, opts(rec))
	x := rec.byStep("a")[0].Extracted
	if x["first"] != "SKU-001" || x["third"] != "SKU-003" || x["last"] != "SKU-005" || !strings.HasPrefix(x["rnd"], "SKU-") {
		t.Errorf("%v", x)
	}
	if _, ok := x["oob"]; ok {
		t.Errorf("out-of-range match should be missing: %v", x)
	}
}

func TestRequiredExtractionFailureKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		step func() model.Step
	}{
		{"jsonpath", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExJSONPath, Expr: "$.nope", Required: true})
		}},
		{"jsonpath on html", func() model.Step {
			return withExtract(get("x", "/"), model.Extractor{Name: "v", Type: model.ExJSONPath, Expr: "$.a", Required: true})
		}},
		{"regex", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExRegex, Expr: `nomatch(\d+)`, Required: true})
		}},
		{"bad regex", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExRegex, Expr: `(`, Required: true})
		}},
		{"header", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExHeader, Expr: "X-Nope", Required: true})
		}},
		{"cookie", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExCookie, Expr: "NOPE", Required: true})
		}},
		{"boundary", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExBoundary, Left: "<<", Right: ">>", Required: true})
		}},
		{"xpath on json", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExXPath, Expr: "//a", Required: true})
		}},
		{"jsonpath filter unsupported", func() model.Step {
			return withExtract(get("x", "/api/items"), model.Extractor{Name: "v", Type: model.ExJSONPath, Expr: "$.items[?(@.id=='SKU-001')]", Required: true})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newMock(t)
			rec := &recorder{}
			sc := newSc(srv.URL, perVU(1, 1), tc.step())
			_, snap := runSc(t, sc, opts(rec))
			if snap.Requests.ErrorKind["extraction"] != 1 || snap.Requests.Errors != 1 {
				t.Errorf("errorKinds=%v errors=%d", snap.Requests.ErrorKind, snap.Requests.Errors)
			}
			if snap.IterFailures != 1 {
				t.Errorf("iteration should fail: %d", snap.IterFailures)
			}
			if e := rec.all()[0]; e.OK || !has(e.Missing, "v") || !strings.Contains(e.Error, "required extraction") {
				t.Errorf("exchange %+v", e)
			}
		})
	}
}

func TestAssertions(t *testing.T) {
	t.Parallel()
	A := func(typ, expr, val string) model.Assertion { return model.Assertion{Type: typ, Expr: expr, Value: val} }
	tests := []struct {
		name string
		path string
		a    model.Assertion
		pass bool
		kind string // expected error kind when failing
	}{
		{"status 2xx", "/api/items", A("status", "", "2xx"), true, ""},
		{"status exact", "/api/items", A("status", "", "200"), true, ""},
		{"status list", "/api/items", A("status", "", "201, 200"), true, ""},
		{"status 3xx fails", "/api/items", A("status", "", "3xx"), false, "assertion"},
		{"status 404 expected", "/status/404", A("status", "", "404"), true, ""},
		{"status 5xx expected", "/status/503", A("status", "", "5xx"), true, ""},
		{"status default 2xx assert on 500", "/status/500", A("status", "", "2xx"), false, "assertion"},
		{"status empty spec means <400", "/api/items", A("status", "", ""), true, ""},
		{"contains ok", "/api/items", A("contains", "", "SKU-001"), true, ""},
		{"contains missing", "/api/items", A("contains", "", "ZZZ"), false, "assertion"},
		{"not_contains ok", "/api/items", A("not_contains", "", "ZZZ"), true, ""},
		{"not_contains fails", "/api/items", A("not_contains", "", "SKU"), false, "assertion"},
		{"regex ok", "/api/items", A("regex", `SKU-00\d`, ""), true, ""},
		{"regex fail", "/api/items", A("regex", `^nothing$`, ""), false, "assertion"},
		{"regex invalid", "/api/items", A("regex", `(`, ""), false, "assertion"},
		{"jsonpath exists", "/api/items", A("jsonpath", "$.items[0].id", ""), true, ""},
		{"jsonpath equals", "/api/items", A("jsonpath", "$.items[0].id", "SKU-001"), true, ""},
		{"jsonpath equals any", "/api/items", A("jsonpath", "$.items[*].id", "SKU-003"), true, ""},
		{"jsonpath number", "/api/items", A("jsonpath", "$.items[1].price", "20"), true, ""},
		{"jsonpath wrong value", "/api/items", A("jsonpath", "$.items[0].id", "SKU-999"), false, "assertion"},
		{"jsonpath missing", "/api/items", A("jsonpath", "$.nope", ""), false, "assertion"},
		{"jsonpath on html", "/", A("jsonpath", "$.a", ""), false, "assertion"},
		{"jsonpath filter unsupported", "/api/items", A("jsonpath", "$.items[?(@.id==1)]", ""), false, "assertion"},
		{"max_latency ok", "/health", A("max_latency", "", "5s"), true, ""},
		{"max_latency exceeded", "/delay?ms=60", A("max_latency", "", "5ms"), false, "assertion"},
		{"max_latency invalid", "/health", A("max_latency", "", "fast"), false, "assertion"},
		{"header contains", "/api/items", A("header", "Content-Type", "json"), true, ""},
		{"header exists", "/api/items", A("header", "Content-Type", ""), true, ""},
		{"header mismatch", "/api/items", A("header", "Content-Type", "xml"), false, "assertion"},
		{"header missing", "/api/items", A("header", "X-Nope", ""), false, "assertion"},
		{"4xx without status assert", "/status/404", A("contains", "", ""), false, "http_4xx"},
		{"5xx without status assert", "/status/500", A("contains", "", ""), false, "http_5xx"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newMock(t)
			rec := &recorder{}
			sc := newSc(srv.URL, perVU(1, 1), withAssert(get("r", tc.path), tc.a))
			_, snap := runSc(t, sc, opts(rec))
			ex := rec.all()
			if len(ex) != 1 {
				t.Fatalf("exchanges=%d", len(ex))
			}
			if ex[0].OK != tc.pass {
				t.Errorf("OK=%v want %v (assertions=%+v err=%q)", ex[0].OK, tc.pass, ex[0].Assertions, ex[0].Error)
			}
			if tc.pass {
				if snap.Requests.Errors != 0 {
					t.Errorf("unexpected errors %v", snap.Requests.ErrorKind)
				}
				return
			}
			if snap.Requests.ErrorKind[tc.kind] != 1 {
				t.Errorf("errorKinds=%v want %s", snap.Requests.ErrorKind, tc.kind)
			}
		})
	}
}

func TestStatusAssertOverridesHTTPError(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	sc := newSc(srv.URL, perVU(1, 1), withAssert(get("r", "/status/404"), model.Assertion{Type: model.AsStatus, Value: "404"}))
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.Errors != 0 {
		t.Errorf("expected 404 must not count as an error: %+v", snap.Requests.ErrorKind)
	}
}

func TestStatusMatches(t *testing.T) {
	tests := []struct {
		code int
		spec string
		want bool
	}{
		{200, "200", true}, {200, "2xx", true}, {200, "2XX", true}, {204, "2xx", true}, {301, "2xx", false},
		{404, "200,201,4xx", true}, {500, "200, 201", false}, {399, "", true}, {400, "", false}, {200, "xx", false},
		{200, "20x", false}, {200, "  200 ", true},
	}
	for _, tc := range tests {
		if got := statusMatches(tc.code, tc.spec); got != tc.want {
			t.Errorf("statusMatches(%d,%q)=%v want %v", tc.code, tc.spec, got, tc.want)
		}
	}
}

func TestOnErrorModes(t *testing.T) {
	t.Parallel()
	steps := func() []model.Step {
		return []model.Step{get("bad", "/status/500"), get("after", "/health")}
	}
	tests := []struct {
		onError           string
		wantReq           uint64
		wantIters         uint64
		wantAfterRequests uint64
	}{
		{"", 6, 3, 3},
		{"continue", 6, 3, 3},
		{"abort-iteration", 3, 3, 0},
		{"stop-vu", 1, 1, 0},
	}
	for _, tc := range tests {
		t.Run("onError="+tc.onError, func(t *testing.T) {
			t.Parallel()
			srv, _ := newMock(t)
			w := perVU(1, 3)
			w.OnError = tc.onError
			sc := newSc(srv.URL, w, steps()...)
			_, snap := runSc(t, sc, opts(nil))
			if snap.Requests.Count != tc.wantReq {
				t.Errorf("requests=%d want %d", snap.Requests.Count, tc.wantReq)
			}
			if snap.Iterations != tc.wantIters {
				t.Errorf("iterations=%d want %d", snap.Iterations, tc.wantIters)
			}
			n, _ := byReq(snap, "after")
			if n.Count != tc.wantAfterRequests {
				t.Errorf("after=%d want %d", n.Count, tc.wantAfterRequests)
			}
			if snap.IterFailures != snap.Iterations {
				t.Errorf("every iteration should be marked failed: %d/%d", snap.IterFailures, snap.Iterations)
			}
		})
	}
}

func TestAbortInsideTransactionAndLoop(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	w := perVU(1, 1)
	w.OnError = "abort-iteration"
	tx := model.Step{Type: model.StepTransaction, Name: "T", Steps: []model.Step{
		{Type: model.StepLoop, Count: 5, Steps: []model.Step{get("bad", "/status/500")}},
	}}
	sc := newSc(srv.URL, w, tx, get("after", "/health"))
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.Count != 1 {
		t.Errorf("abort must unwind loop+transaction: %d requests", snap.Requests.Count)
	}
	if len(snap.ByTx) != 1 || snap.Transactions.Errors != 1 {
		t.Errorf("transaction should be recorded as failed: %+v", snap.ByTx)
	}
}

func TestTimeoutClassification(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	sc := newSc(srv.URL, perVU(1, 2), withTimeout(get("slow", "/delay?ms=3000"), 150*time.Millisecond))
	start := time.Now()
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["timeout"] != 2 {
		t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("timeouts not enforced: %v", time.Since(start))
	}
	// defaults.timeout applies when the request has none
	sc = newSc(srv.URL, perVU(1, 1), get("slow", "/delay?ms=3000"))
	sc.Defaults.Timeout = dur(100 * time.Millisecond)
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["timeout"] != 1 {
		t.Errorf("defaults.timeout ignored: %v", snap.Requests.ErrorKind)
	}
}

func TestConnectionRefusedClassification(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	rec := &recorder{}
	sc := newSc("http://"+addr, perVU(1, 2), get("x", "/health"))
	_, snap := runSc(t, sc, opts(rec))
	if snap.Requests.ErrorKind["connect"] != 2 || snap.Requests.Errors != 2 {
		t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
	}
	if e := rec.all()[0]; e.Status != 0 || e.OK || e.Error == "" {
		t.Errorf("%+v", e)
	}
	if snap.IterFailures != 2 {
		t.Errorf("iterFailures=%d", snap.IterFailures)
	}
}

func TestBlockedByPolicy(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"10.255.255.1", "example.invalid", "8.8.8.8:8080", "[2001:db8::1]"} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			sc := newSc("http://"+host, perVU(1, 1), withTimeout(get("x", "/"), 2*time.Second))
			start := time.Now()
			_, snap := runSc(t, sc, opts(nil))
			if snap.Requests.ErrorKind["blocked_by_policy"] != 1 {
				t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
			}
			if time.Since(start) > time.Second {
				t.Errorf("blocked host must fail immediately, took %v", time.Since(start))
			}
		})
	}
	t.Run("allowlisted host passes the guard", func(t *testing.T) {
		t.Parallel()
		o := opts(nil)
		o.Policy.AllowedHosts = []string{"example.invalid"}
		sc := newSc("http://example.invalid", perVU(1, 1), withTimeout(get("x", "/"), 2*time.Second))
		_, snap := runSc(t, sc, o)
		if snap.Requests.ErrorKind["blocked_by_policy"] != 0 {
			t.Errorf("allowlisted host was blocked: %v", snap.Requests.ErrorKind)
		}
		if snap.Requests.Errors != 1 { // DNS fails, but not by policy
			t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
		}
	})
	t.Run("redirect to non-allowlisted host is blocked", func(t *testing.T) {
		t.Parallel()
		redir := httptestRedirect(t, "http://10.255.255.1/secret")
		sc := newSc(redir, perVU(1, 1), get("x", "/"))
		_, snap := runSc(t, sc, opts(nil))
		if snap.Requests.ErrorKind["blocked_by_policy"] != 1 {
			t.Errorf("redirect escaped the allowlist: %v", snap.Requests.ErrorKind)
		}
	})
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("dial tcp: connection refused"), "connect"},
		{fmt.Errorf("lookup x: no such host"), "connect"},
		{fmt.Errorf("something odd"), "other"},
	}
	for _, tc := range tests {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("%v -> %s want %s", tc.err, got, tc.want)
		}
	}
}

func TestRedirects(t *testing.T) {
	t.Parallel()
	f, tr := false, true
	tests := []struct {
		name       string
		defaults   *bool
		perRequest *bool
		path       string
		wantStatus int
		wantKind   string
	}{
		{"follow by default", nil, nil, "/redirect?n=3", 200, ""},
		{"disabled in defaults", &f, nil, "/redirect?n=3", 302, ""},
		{"disabled per request", nil, &f, "/redirect?n=1", 302, ""},
		{"request overrides defaults", &f, &tr, "/redirect?n=2", 200, ""},
		{"too many redirects", nil, nil, "/redirect?n=15", 0, "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newMock(t)
			rec := &recorder{}
			st := get("r", tc.path)
			st.Request.FollowRedirects = tc.perRequest
			sc := newSc(srv.URL, perVU(1, 1), st)
			sc.Defaults.FollowRedirects = tc.defaults
			_, snap := runSc(t, sc, opts(rec))
			e := rec.all()[0]
			if e.Status != tc.wantStatus {
				t.Errorf("status=%d want %d (err=%s)", e.Status, tc.wantStatus, e.Error)
			}
			if tc.wantKind != "" && snap.Requests.ErrorKind[tc.wantKind] != 1 {
				t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
			}
			if tc.wantStatus == 302 && !strings.HasPrefix(e.RespHeaders["Location"], "/redirect") {
				t.Errorf("Location=%q", e.RespHeaders["Location"])
			}
		})
	}
}

func TestMultipartUploadAndSafePath(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "scenario")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "up.txt"), []byte("hello"), 0o600)
	os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("TOP SECRET"), 0o600)

	up := func(file string) model.Step {
		return model.Step{Type: model.StepRequest, Name: "up", Request: &model.Request{Method: "POST", URL: "/upload",
			Multipart: []model.Part{{Name: "note", Value: "hi ${who}"}, {Name: "doc", File: file}}}}
	}
	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		rec := &recorder{}
		st := withAssert(up("up.txt"),
			model.Assertion{Type: model.AsJSONPath, Expr: "$.files.doc", Value: "5"},
			model.Assertion{Type: model.AsJSONPath, Expr: "$.fields.note[0]", Value: "hi bob"})
		sc := newSc(srv.URL, perVU(1, 1), st)
		sc.Variables = map[string]string{"who": "bob"}
		o := opts(rec)
		o.BaseDir = dir
		_, snap := runSc(t, sc, o)
		if snap.Requests.Errors != 0 {
			t.Fatalf("%v %+v", snap.Requests.ErrorKind, rec.all())
		}
		if snap.Requests.BytesOut < 5 {
			t.Errorf("bytesOut=%d", snap.Requests.BytesOut)
		}
		if ct := rec.all()[0].ReqHeaders["Content-Type"]; !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
			t.Errorf("content-type %q", ct)
		}
	})
	t.Run("inner dot-dot staying inside is ok", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		sc := newSc(srv.URL, perVU(1, 1), up("sub/../up.txt"))
		o := opts(nil)
		o.BaseDir = dir
		_, snap := runSc(t, sc, o)
		if snap.Requests.Errors != 0 {
			t.Errorf("%v", snap.Requests.ErrorKind)
		}
	})
	for _, bad := range []string{"../secret.txt", "sub/../../secret.txt", "/etc/passwd", filepath.Join(parent, "secret.txt")} {
		t.Run("reject "+bad, func(t *testing.T) {
			t.Parallel()
			srv, ms := newMock(t)
			rec := &recorder{}
			sc := newSc(srv.URL, perVU(1, 1), up(bad))
			o := opts(rec)
			o.BaseDir = dir
			_, snap := runSc(t, sc, o)
			if snap.Requests.ErrorKind["invalid_request"] != 1 {
				t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
			}
			if ms.Hits.Load() != 0 {
				t.Error("request must not be sent")
			}
			if e := rec.all()[0]; strings.Contains(e.Error, "TOP SECRET") || e.Error == "" {
				t.Errorf("error=%q", e.Error)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		sc := newSc(srv.URL, perVU(1, 1), up("nope.txt"))
		o := opts(nil)
		o.BaseDir = dir
		_, snap := runSc(t, sc, o)
		if snap.Requests.ErrorKind["invalid_request"] != 1 {
			t.Errorf("errorKinds=%v", snap.Requests.ErrorKind)
		}
	})
}

func TestThinkTimeExcludedFromTransaction(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	tx := model.Step{Type: model.StepTransaction, Name: "checkout", Steps: []model.Step{
		get("a", "/health"),
		{Type: model.StepThink, Think: &model.Think{Min: dur(400 * time.Millisecond)}},
		get("b", "/health"),
	}}
	sc := newSc(srv.URL, perVU(1, 2), tx)
	start := time.Now()
	_, snap := runSc(t, sc, opts(nil))
	if time.Since(start) < 750*time.Millisecond {
		t.Fatalf("think time not honoured: %v", time.Since(start))
	}
	if len(snap.ByTx) != 1 || snap.ByTx[0].Count != 2 {
		t.Fatalf("%+v", snap.ByTx)
	}
	if snap.ByTx[0].MaxMs > 200 {
		t.Errorf("transaction max=%vms includes think time", snap.ByTx[0].MaxMs)
	}
	if snap.Transactions.Errors != 0 {
		t.Errorf("tx errors %d", snap.Transactions.Errors)
	}
}

func TestThinkTimeScaleAndRandomRange(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	zero := 0.0
	w := perVU(1, 1)
	w.ThinkTimeScale = &zero
	sc := newSc(srv.URL, w, model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(3 * time.Second)}}, get("a", "/health"))
	start := time.Now()
	runSc(t, sc, opts(nil))
	if time.Since(start) > time.Second {
		t.Errorf("scale 0 should ignore think time: %v", time.Since(start))
	}
	half := 0.5
	w.ThinkTimeScale = &half
	sc = newSc(srv.URL, w, model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(200 * time.Millisecond), Max: dur(400 * time.Millisecond)}}, get("a", "/health"))
	start = time.Now()
	runSc(t, sc, opts(nil))
	if el := time.Since(start); el < 90*time.Millisecond || el > 600*time.Millisecond {
		t.Errorf("scaled random think out of range: %v", el)
	}
}

func TestInspectVarsUsedAndUnresolved(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	st := get("r", "/api/items?q=${user}&z=${missing_var}")
	st.Request.Headers = []model.Header{{Name: "X-Id", Value: "${user}-${__vu()}"}, {Name: "X-Bad", Value: "${other_missing}"}}
	sc := newSc(srv.URL, perVU(1, 1), st)
	sc.Variables = map[string]string{"user": "u1"}
	sc.Defaults.Headers = []model.Header{{Name: "X-Def", Value: "${def_missing}"}}
	runSc(t, sc, opts(rec))
	e := rec.all()[0]
	if !has(e.VarsUsed, "user") || len(e.VarsUsed) != 1 {
		t.Errorf("VarsUsed=%v (must be deduped)", e.VarsUsed)
	}
	for _, m := range []string{"missing_var", "other_missing", "def_missing"} {
		if !has(e.Unresolved, m) {
			t.Errorf("Unresolved=%v lacks %s", e.Unresolved, m)
		}
	}
	if e.ReqHeaders["X-Id"] != "u1-1" {
		t.Errorf("X-Id=%q", e.ReqHeaders["X-Id"])
	}
	if !strings.Contains(e.URL, "q=u1") {
		t.Errorf("url=%s", e.URL)
	}
	if e.Method != "GET" || e.Status != 200 || e.RespBody == "" || e.LatencyMs <= 0 || !e.OK {
		t.Errorf("exchange detail %+v", e)
	}
}

func TestControlFlowSteps(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	dis := get("disabled", "/health")
	dis.Disabled = true
	steps := []model.Step{
		dis,
		{Type: model.StepSetVar, Set: []model.KV{{Name: "y", Value: "${x}2"}}},
		get("setvar", "/api/items?q=${y}"),
		{Type: model.StepLoop, Count: 3, Steps: []model.Step{get("looped", "/health")}},
		{Type: model.StepIf, Cond: "${x} == 1", Steps: []model.Step{get("if-true", "/health")}},
		{Type: model.StepIf, Cond: "${x} == 2", Steps: []model.Step{get("if-false", "/health")}},
		{Type: model.StepGroup, Steps: []model.Step{get("grouped", "/health")}},
	}
	sc := newSc(srv.URL, perVU(1, 1), steps...)
	sc.Variables = map[string]string{"x": "1"}
	_, snap := runSc(t, sc, opts(rec))
	counts := map[string]uint64{}
	for _, n := range snap.ByRequest {
		counts[n.Name] = n.Count
	}
	want := map[string]uint64{"setvar": 1, "looped": 3, "if-true": 1, "grouped": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("%s=%d want %d (all=%v)", k, counts[k], v, counts)
		}
	}
	if counts["disabled"] != 0 || counts["if-false"] != 0 {
		t.Errorf("skipped steps ran: %v", counts)
	}
	if qOf(rec.byStep("setvar")[0]) != "12" {
		t.Errorf("set var: %s", rec.byStep("setvar")[0].URL)
	}
}

func TestSetupAndTeardownPerVU(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	sc := newSc(srv.URL, perVU(2, 3), get("main", "/health"))
	sc.Setup = []model.Step{get("setup", "/health")}
	sc.Teardown = []model.Step{get("teardown", "/health")}
	_, snap := runSc(t, sc, opts(nil))
	for name, want := range map[string]uint64{"setup": 2, "main": 6, "teardown": 2} {
		n, _ := byReq(snap, name)
		if n.Count != want {
			t.Errorf("%s=%d want %d", name, n.Count, want)
		}
	}
	if snap.Iterations != 6 {
		t.Errorf("setup/teardown must not count as iterations: %d", snap.Iterations)
	}
}

func TestCookiesJarAndDisable(t *testing.T) {
	t.Parallel()
	login := model.Step{Type: model.StepRequest, Name: "login", Request: &model.Request{Method: "POST", URL: "/login",
		Headers: []model.Header{{Name: "Content-Type", Value: "application/json"}}, Body: `{"username":"carl","password":"secret"}`}}
	dash := withAssert(get("dash", "/dashboard"), model.Assertion{Type: model.AsContains, Value: "Welcome carl"})
	t.Run("jar on", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		_, snap := runSc(t, newSc(srv.URL, perVU(1, 1), login, dash), opts(nil))
		if snap.Requests.Errors != 0 {
			t.Errorf("%v", snap.Requests.ErrorKind)
		}
	})
	t.Run("jar off", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		sc := newSc(srv.URL, perVU(1, 1), login, dash)
		sc.Defaults.DisableCookies = true
		_, snap := runSc(t, sc, opts(nil))
		if snap.Requests.ErrorKind["assertion"] != 1 {
			t.Errorf("session cookie leaked despite disableCookies: %v", snap.Requests.ErrorKind)
		}
	})
	t.Run("cookies isolated per VU", func(t *testing.T) {
		t.Parallel()
		srv, _ := newMock(t)
		// VU 1 logs in; VU 2 never logs in and must see the login redirect, not VU1's session.
		onlyVU1 := model.Step{Type: model.StepIf, Cond: "${__vu()} == 1", Steps: []model.Step{login}}
		wait := model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(200 * time.Millisecond)}}
		dash2 := withAssert(get("dash", "/dashboard"), model.Assertion{Type: model.AsContains, Value: "Welcome"})
		_, snap := runSc(t, newSc(srv.URL, perVU(2, 1), onlyVU1, wait, dash2), opts(nil))
		d, _ := byReq(snap, "dash")
		if d.Errors != 1 {
			t.Errorf("exactly one VU (the one without a login) should fail: %+v", d)
		}
	})
}

func TestRelativeURLWithoutBase(t *testing.T) {
	t.Parallel()
	sc := model.NewScenario("x")
	sc.Workload = perVU(1, 1)
	sc.Steps = []model.Step{get("r", "/health")}
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["invalid_request"] != 1 {
		t.Errorf("%v", snap.Requests.ErrorKind)
	}
	sc.Steps = []model.Step{get("r", "ftp://127.0.0.1/x")}
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["invalid_request"] != 1 {
		t.Errorf("non-http scheme: %v", snap.Requests.ErrorKind)
	}
}

func TestBodiesFormAndHostHeader(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	form := model.Step{Type: model.StepRequest, Name: "form", Request: &model.Request{Method: "post", URL: "/echo",
		Form:    []model.KV{{Name: "a", Value: "1 2"}, {Name: "b", Value: "x&y"}},
		Headers: []model.Header{{Name: "Host", Value: "virtual.example"}},
		Assert:  []model.Assertion{{Type: model.AsJSONPath, Expr: "$.headers.Content-Type", Value: "application/x-www-form-urlencoded"}, {Type: model.AsJSONPath, Expr: "$.body", Value: "a=1+2&b=x%26y"}}}}
	_, snap := runSc(t, newSc(srv.URL, perVU(1, 1), form), opts(rec))
	if snap.Requests.Errors != 0 {
		t.Fatalf("%v %+v", snap.Requests.ErrorKind, rec.all()[0].Assertions)
	}
	if rec.all()[0].ReqHeaders["Host"] != "virtual.example" || rec.all()[0].Method != "POST" {
		t.Errorf("%+v", rec.all()[0])
	}
}

func TestMaxBodyBytesTruncatesExtraction(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)
	rec := &recorder{}
	sc := newSc(srv.URL, perVU(1, 1), withAssert(get("r", "/api/items"), model.Assertion{Type: model.AsContains, Value: "SKU-005"}))
	sc.Defaults.MaxBodyBytes = 30
	_, snap := runSc(t, sc, opts(rec))
	if snap.Requests.ErrorKind["assertion"] != 1 {
		t.Errorf("assertion should see only the first 30 bytes: %v", snap.Requests.ErrorKind)
	}
	if snap.Requests.BytesIn < 100 {
		t.Errorf("bytesIn should still count the full body: %d", snap.Requests.BytesIn)
	}
}
