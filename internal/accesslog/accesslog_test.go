package accesslog

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "accesslog", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func analyze(t *testing.T, name string, o Options) *Profile {
	t.Helper()
	p, err := Analyze(bytes.NewReader(fixture(t, name)), name, o)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

func epMap(p *Profile) map[string]int {
	m := map[string]int{}
	for _, e := range p.Endpoints {
		m[e.Key()] = e.Count
	}
	return m
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestNginxCombined(t *testing.T) {
	p := analyze(t, "nginx_combined.log", Options{})
	eq(t, "format", p.Format, FormatNginx)
	eq(t, "lines", p.Lines, 14)
	eq(t, "parsed", p.Parsed, 12)
	eq(t, "malformed", p.Malformed, 2)
	eq(t, "first bad lines", p.FirstBad, []int{7, 13})
	eq(t, "replayable", p.Replayable, 6)
	eq(t, "endpoints", epMap(p), map[string]int{"GET /api/products": 2, "GET /api/products/{id}": 3, "GET /health": 1})
	eq(t, "static", p.Ignored["static assets"], 1)
	eq(t, "writes", p.Ignored["write requests (POST)"], 1)
	eq(t, "errors", p.Ignored["requests that returned 4xx/5xx"], 2)
	eq(t, "template", p.Ignored["paths containing template syntax ${...}"], 1)
	eq(t, "options", p.Ignored["OPTIONS (CORS preflight)"], 1)
	eq(t, "methods", p.Methods, map[string]int{"GET": 10, "POST": 1, "OPTIONS": 1})
	eq(t, "status 200", p.Statuses["200"], 7)
	eq(t, "query count", p.QueryCount, 1)
	if p.Rate.Minutes != 3 {
		t.Errorf("minutes = %d", p.Rate.Minutes)
	}
	// Sessions are keyed by (ip, user agent): Firefox, curl, Go-http-client, kube-probe, scanner.
	eq(t, "sessions", p.Sessions.Sessions, 5)
}

func TestApacheCommonAndTimezone(t *testing.T) {
	p := analyze(t, "apache_common.log", Options{Format: FormatApache})
	eq(t, "parsed", p.Parsed, 5)
	eq(t, "malformed", p.Malformed, 2)
	eq(t, "endpoints", epMap(p), map[string]int{"GET /index.html": 1, "HEAD /index.html": 1, "GET /catalog/items/{id}": 2})
	eq(t, "static", p.Ignored["static assets"], 1)
	// Minutes are the log's own wall clock (-0700), not UTC.
	if !strings.HasPrefix(p.Rate.PeakMinute, "2000-10-10 13:5") {
		t.Errorf("peak minute %q should be in log-local time", p.Rate.PeakMinute)
	}
	auto := analyze(t, "apache_common.log", Options{})
	eq(t, "auto-detected", auto.Format, FormatNginx)
}

func TestALB(t *testing.T) {
	p := analyze(t, "alb.log", Options{})
	eq(t, "format", p.Format, FormatALB)
	eq(t, "parsed", p.Parsed, 4)
	eq(t, "malformed", p.Malformed, 1)
	eq(t, "endpoints", epMap(p), map[string]int{"GET /v2/items": 1, "GET /v2/items/{id}": 2})
	eq(t, "writes", p.Ignored["write requests (POST)"], 1)
	eq(t, "host", p.Hosts["api.example.com"], 4)
	eq(t, "methods", p.Methods["POST"], 1)
}

func TestCloudFront(t *testing.T) {
	p := analyze(t, "cloudfront.log", Options{})
	eq(t, "format", p.Format, FormatCloudFront)
	eq(t, "parsed", p.Parsed, 4)
	eq(t, "malformed", p.Malformed, 1)
	eq(t, "comments", p.Comments, 2)
	eq(t, "endpoints", epMap(p), map[string]int{"GET /catalog": 1, "GET /catalog/{id}": 1})
	eq(t, "static", p.Ignored["static assets"], 1)
	eq(t, "writes", p.Ignored["write requests (POST)"], 1)
	eq(t, "query count", p.QueryCount, 1)
}

func TestCloudFrontCookieSessions(t *testing.T) {
	// /catalog/9981 and /catalog share cookie sid=s-77 although one line lacks the other cookie;
	// IP+UA also matches, so check the cookie path by changing the UA of nothing: just count sessions.
	p := analyze(t, "cloudfront.log", Options{SessionCookie: "sid"})
	if p.Sessions.Sessions < 1 || !strings.Contains(p.Sessions.Keyed, "cookie sid") {
		t.Errorf("sessions: %+v", p.Sessions)
	}
}

func TestGzipMatchesPlain(t *testing.T) {
	plain := analyze(t, "nginx_combined.log", Options{})
	gz := analyze(t, "nginx_combined.log.gz", Options{})
	if !gz.Gzip || plain.Gzip {
		t.Errorf("gzip flags: %v %v", gz.Gzip, plain.Gzip)
	}
	eq(t, "endpoints", epMap(gz), epMap(plain))
	eq(t, "malformed", gz.Malformed, plain.Malformed)
}

func TestExplicitFormatMismatchCountsMalformed(t *testing.T) {
	_, err := Analyze(bytes.NewReader(fixture(t, "alb.log")), "x", Options{Format: FormatNginx})
	if err == nil || !strings.Contains(err.Error(), "no valid") {
		t.Errorf("want 'no valid lines' error, got %v", err)
	}
	if _, err := Analyze(strings.NewReader("hello\nworld\n"), "x", Options{}); err == nil {
		t.Error("garbage input must fail")
	}
	if _, err := Analyze(strings.NewReader(""), "x", Options{Format: "iis"}); err == nil {
		t.Error("unknown format must fail")
	}
}

func TestInputCapAndLongLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "10.0.0.1 - - [10/Oct/2023:13:%02d:%02d +0000] \"GET /a HTTP/1.1\" 200 1 \"-\" \"ua\"\n", (i/60)%60, i%60)
	}
	b.WriteString(strings.Repeat("x", 200000) + "\n")
	b.WriteString("10.0.0.1 - - [10/Oct/2023:14:00:00 +0000] \"GET /b HTTP/1.1\" 200 1 \"-\" \"ua\"\n")
	p, err := Analyze(strings.NewReader(b.String()), "big", Options{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "parsed", p.Parsed, 2001)
	eq(t, "malformed (long line)", p.Malformed, 1)

	p, err = Analyze(strings.NewReader(b.String()), "big", Options{MaxBytes: 50000})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || p.Parsed >= 2000 {
		t.Errorf("cap not applied: truncated=%v parsed=%d", p.Truncated, p.Parsed)
	}

	// A gzip bomb is capped on the decompressed side.
	var gzb bytes.Buffer
	zw := gzip.NewWriter(&gzb)
	_, _ = zw.Write([]byte(b.String()))
	_ = zw.Close()
	p, err = Analyze(&gzb, "bomb", Options{MaxBytes: 50000})
	if err != nil || !p.Truncated {
		t.Errorf("gzip cap: err=%v truncated=%v", err, p != nil && p.Truncated)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want, q string }{
		{"/orders/123", "/orders/{id}", ""},
		{"/orders/123/items/9?x=1", "/orders/{id}/items/{id}", "x=1"},
		{"/u/550e8400-e29b-41d4-a716-446655440000/profile", "/u/{id}/profile", ""},
		{"/files/9f86d081884c7d659a2feaa0c55ad015", "/files/{id}", ""},
		{"/files/5f4dcc3b5aa765d61d8327deb882cf99/", "/files/{id}", ""},
		{"/obj/507f1f77bcf86cd799439011", "/obj/{id}", ""},
		{"/t/AbCdEf0123456789XyZw99", "/t/{id}", ""},
		{"/api/v2/products", "/api/v2/products", ""}, // v2 is not an id
		{"/api/deadbeef", "/api/deadbeef", ""},       // short, letters only
		{"/a/b/../c//d", "/a/c/d", ""},               // cleaned
		{"/", "/", ""},
		{"/search?q=a%20b", "/search", "q=a%20b"},
		{"/%7Euser/2024", "/~user/{id}", ""},
	}
	for _, c := range cases {
		got, q, _ := Normalize(c.in)
		if got != c.want || q != c.q {
			t.Errorf("Normalize(%q) = %q, %q; want %q, %q", c.in, got, q, c.want, c.q)
		}
	}
	_, _, ids := Normalize("/orders/10/items/700")
	if len(ids) != 2 || ids[0].Kind != IDNumeric || ids[1].Index != 3 || ids[1].Max != 700 {
		t.Errorf("slots: %+v", ids)
	}
}

// synthLog builds an nginx log whose minute m has counts[m] requests, split
// across endpoints in the given proportions (integer parts per 10 requests).
func synthLog(day string, startHour, startMin int, counts []int, paths []string, ratio []int) string {
	var b strings.Builder
	for m, n := range counts {
		minute := startHour*60 + startMin + m
		for i := 0; i < n; i++ {
			// choose path by position in a repeating pattern of sum(ratio)
			pos := i % sumInts(ratio)
			pi := 0
			for acc := ratio[0]; pos >= acc; pi++ {
				acc += ratio[pi+1]
			}
			sec := (i * 60 / max(n, 1)) % 60
			fmt.Fprintf(&b, "10.%d.%d.%d - - [%s:%02d:%02d:%02d +0000] \"GET %s HTTP/1.1\" 200 10 \"-\" \"ua\"\n",
				m%250, i%250, 1, day, (minute/60)%24, minute%60, sec, paths[pi])
		}
	}
	return b.String()
}

func sumInts(a []int) int {
	s := 0
	for _, v := range a {
		s += v
	}
	return s
}

func TestRateCurveWindows(t *testing.T) {
	counts := []int{30, 60, 90, 300, 600, 300, 60, 30}
	log := synthLog("10/Oct/2023", 12, 0, counts, []string{"/a"}, []int{1})
	p, err := Analyze(strings.NewReader(log), "synth", Options{Window: "peak", PeakMinutes: 3})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "rate minutes", p.Rate.Minutes, 8)
	eq(t, "peak rps", p.Rate.PeakRPS, 10.0)
	eq(t, "peak minute", p.Rate.PeakMinute, "2023-10-10 12:04")
	eq(t, "window mode", p.Window.Mode, "peak")
	eq(t, "window start", p.Window.Start, "2023-10-10 12:03")
	var got []float64
	for _, c := range p.Curve {
		got = append(got, c.Requests)
	}
	eq(t, "window curve", got, []float64{300, 600, 300})

	sc, rep, err := Generate(p, Options{BaseURL: "http://127.0.0.1:8099", Scale: 0.1, Window: "peak"})
	if err != nil {
		t.Fatal(err)
	}
	w := sc.Workload
	eq(t, "executor", w.Executor, "ramping-arrival-rate")
	eq(t, "stages", len(w.Stages), 3)
	// 600 req/min = 10/s, at 10% = 1/s; 300 req/min -> 0.5/s
	eq(t, "stage targets", []float64{w.Stages[0].Target, w.Stages[1].Target, w.Stages[2].Target}, []float64{0.5, 1, 0.5})
	if w.Stages[0].Duration.D() != time.Minute {
		t.Errorf("stage duration %v", w.Stages[0].Duration.D())
	}
	if rep.Validated != "passed" || len(sc.Validate()) != 0 {
		t.Errorf("validation: %s %v", rep.Validated, sc.Validate())
	}

	// avg: constant rate over the whole span
	pa, _ := Analyze(strings.NewReader(log), "synth", Options{Window: "avg"})
	sca, _, err := Generate(pa, Options{BaseURL: "http://127.0.0.1:8099", Scale: 1, Window: "avg"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "avg executor", sca.Workload.Executor, "constant-arrival-rate")
	eq(t, "avg rate", sca.Workload.Rate, 3.063) // 1470 req / 8 min / 60
	eq(t, "avg duration", sca.Workload.Duration.D(), 8*time.Minute)

	// explicit range
	pr, err := Analyze(strings.NewReader(log), "synth", Options{Window: "12:02-12:05"})
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, c := range pr.Curve {
		got = append(got, c.Requests)
	}
	eq(t, "range curve", got, []float64{90, 300, 600})
	eq(t, "range label", pr.Curve[0].Minute, "12:02")
	if _, err := Analyze(strings.NewReader(log), "synth", Options{Window: "03:00-04:00"}); err == nil {
		t.Error("window without data must fail")
	}
	if _, err := Analyze(strings.NewReader(log), "synth", Options{Window: "25:00-26:00"}); err == nil {
		t.Error("invalid window must fail")
	}
}

func TestRangeWindowAveragesDaysAndWrapsMidnight(t *testing.T) {
	a := []string{"/a"}
	r := []int{1}
	d1 := synthLog("10/Oct/2023", 23, 58, []int{10, 20}, a, r) + synthLog("11/Oct/2023", 0, 0, []int{30, 40}, a, r)
	d2 := synthLog("11/Oct/2023", 23, 58, []int{30, 40}, a, r) + synthLog("12/Oct/2023", 0, 0, []int{50, 60}, a, r)
	p, err := Analyze(strings.NewReader(d1+d2), "two-days", Options{Window: "23:58-00:02"})
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, c := range p.Curve {
		got = append(got, c.Requests)
	}
	eq(t, "2-day average across midnight", got, []float64{20, 30, 40, 50})
	eq(t, "days", p.Window.Days, 2)
}

func TestMixWeightsAndSteps(t *testing.T) {
	log := synthLog("10/Oct/2023", 9, 0, []int{100, 100}, []string{"/p", "/p/7", "/c"}, []int{6, 3, 1})
	sc, rep, p, err := Import(strings.NewReader(log), "mix.log", Options{BaseURL: "http://localhost:8080", Name: "mix"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Endpoints) != 3 || p.Endpoints[0].Path != "/p" || p.Endpoints[0].Share != 60 {
		t.Fatalf("endpoints: %+v", p.Endpoints[0])
	}
	g := sc.Steps[0]
	if g.Type != "group" || g.Select != "weighted" || len(g.Steps) != 3 {
		t.Fatalf("group: %+v", g)
	}
	var ws []float64
	for _, s := range g.Steps {
		ws = append(ws, *s.Weight)
	}
	eq(t, "weights", ws, []float64{60, 30, 10})
	eq(t, "id url", g.Steps[1].Request.URL, "/p/${__randomInt(7,7)}")
	eq(t, "name", g.Steps[0].Name, "GET /p")
	if len(rep.Warnings) == 0 || !strings.Contains(rep.Warnings[0], "authorized") {
		t.Errorf("missing production warning: %v", rep.Warnings)
	}
}

func TestGenerateTopIgnoresWritesAndQueries(t *testing.T) {
	data := fixture(t, "nginx_combined.log")
	// Default: GET only, query dropped.
	sc, rep, _, err := Import(bytes.NewReader(data), "nginx", Options{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sc.AllRequests() {
		if s.Request.Method != "GET" && s.Request.Method != "HEAD" || strings.Contains(s.Request.URL, "?") {
			t.Errorf("unexpected request %+v", s.Request)
		}
	}
	found := map[string]string{}
	for _, it := range rep.Items {
		found[it.Name] = it.Status
	}
	if found["write requests (POST)"] != "ignored" || found["static assets"] != "ignored" {
		t.Errorf("ignored items missing: %v", found)
	}

	// --include-writes, --keep-query, --include-errors
	sc, rep, _, err = Import(bytes.NewReader(data), "nginx", Options{BaseURL: "http://127.0.0.1:1", IncludeWrites: true, KeepQuery: true, IncludeErrors: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawPost, sawQuery bool
	for _, s := range sc.AllRequests() {
		sawPost = sawPost || s.Request.Method == "POST"
		if strings.Contains(s.Request.URL, "?") {
			sawQuery = true
			if strings.Contains(s.Request.URL, "sk_live") || !strings.Contains(s.Request.URL, "api_key=[REDACTED]") || !strings.Contains(s.Request.URL, "currency=EUR") {
				t.Errorf("query not redacted properly: %s", s.Request.URL)
			}
		}
	}
	if !sawPost || !sawQuery {
		t.Errorf("post=%v query=%v", sawPost, sawQuery)
	}
	if len(rep.Manual) == 0 {
		t.Error("write bodies need a manual-change note")
	}

	// top: keep one endpoint, long tail reported
	sc, rep, _, err = Import(bytes.NewReader(data), "nginx", Options{BaseURL: "http://127.0.0.1:1", Top: 1})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "kept", len(sc.Steps[0].Steps), 1)
	eq(t, "weight", *sc.Steps[0].Steps[0].Weight, 100.0)
	var tail bool
	for _, it := range rep.Items {
		tail = tail || it.Element == "long tail"
	}
	if !tail {
		t.Error("long tail not reported")
	}
}

func TestTargetSafety(t *testing.T) {
	data := fixture(t, "nginx_combined.log")
	if _, _, _, err := Import(bytes.NewReader(data), "x", Options{}); err == nil || !strings.Contains(err.Error(), "--base-url") {
		t.Errorf("missing base url: %v", err)
	}
	_, _, _, err := Import(bytes.NewReader(data), "x", Options{BaseURL: "https://shop.example.com"})
	if err == nil || !strings.Contains(err.Error(), "--allow-host shop.example.com") {
		t.Errorf("non-loopback must be refused: %v", err)
	}
	if _, _, _, err := Import(bytes.NewReader(data), "x", Options{BaseURL: "https://shop.example.com", AllowHosts: []string{"shop.example.com"}}); err != nil {
		t.Errorf("allowed host refused: %v", err)
	}
	for _, bad := range []string{"ftp://127.0.0.1", "http://user:pw@127.0.0.1", "127.0.0.1:80", "http://"} {
		if _, err := CheckTarget(Options{BaseURL: bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if _, _, _, err := Import(bytes.NewReader(data), "x", Options{BaseURL: "http://localhost", Scale: 50}); err == nil {
		t.Error("scale 50 must be rejected")
	}
	_, rep, _, err := Import(bytes.NewReader(data), "x", Options{BaseURL: "http://localhost", Scale: 2})
	if err != nil || !strings.Contains(strings.Join(rep.Warnings, "|"), "above 1") {
		t.Errorf("scale>1 warning: %v %v", err, rep)
	}
}

func TestNoReplayableRequests(t *testing.T) {
	line := "10.0.0.1 - - [10/Oct/2023:13:00:00 +0000] \"POST /a HTTP/1.1\" 200 1 \"-\" \"ua\"\n"
	_, _, _, err := Import(strings.NewReader(line), "x", Options{BaseURL: "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "--include-writes") {
		t.Errorf("want helpful error, got %v", err)
	}
}

func TestPathsWithTemplateSyntaxNeverReachScenario(t *testing.T) {
	log := "10.0.0.1 - - [10/Oct/2023:13:00:00 +0000] \"GET /ok HTTP/1.1\" 200 1 \"-\" \"ua\"\n" +
		"10.0.0.1 - - [10/Oct/2023:13:00:01 +0000] \"GET /x?a=${__env(LOADMETER_SECRET)} HTTP/1.1\" 200 1 \"-\" \"ua\"\n"
	sc, _, _, err := Import(strings.NewReader(log), "x", Options{BaseURL: "http://127.0.0.1:1", KeepQuery: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sc.AllRequests() {
		if strings.Contains(s.Request.URL, "LOADMETER_SECRET") {
			t.Fatalf("template injection reached the scenario: %s", s.Request.URL)
		}
	}
}
