package triage

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/results"
)

const hdr = "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind\n"

type csvB struct {
	w  *csv.Writer
	sb *strings.Builder
}

func newCSV() *csvB {
	sb := &strings.Builder{}
	sb.WriteString(hdr)
	return &csvB{csv.NewWriter(sb), sb}
}

// fail adds a failed request row at offset sec (run starts at epoch 1_000_000 ms).
func (c *csvB) fail(sec float64, label string, code int, kind, vu, msg string) {
	c.w.Write([]string{strconv.FormatInt(1_000_000+int64(sec*1000), 10), "10", label, strconv.Itoa(code), kind, vu, "false", msg, "0", "0", "5", "http://127.0.0.1:8099/x?token=abc123", "request"})
}
func (c *csvB) ok(sec float64, label string) {
	c.w.Write([]string{strconv.FormatInt(1_000_000+int64(sec*1000), 10), "10", label, "200", "", "vu-1", "true", "", "0", "0", "5", "http://127.0.0.1:8099/x", "request"})
}
func (c *csvB) String() string { c.w.Flush(); return c.sb.String() }

var t0 = time.UnixMilli(1_000_000)

func analyze(t *testing.T, c *csvB, opt Options) *Result {
	t.Helper()
	opt.Start = t0
	r, err := Analyze(strings.NewReader(c.String()), opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNormalizePlaceholders(t *testing.T) {
	cases := map[string]string{
		`Get "http://127.0.0.1:8099/api/x": dial tcp 127.0.0.1:8099: connect: connection refused`: `Get <STR> dial tcp <IP> connect connection refused`,
		`read tcp 127.0.0.1:8099->127.0.0.1:53422: read: connection reset by peer`:                `read tcp <IP>-><IP> read connection reset by peer`,
		`order 8f14e45f-ceea-467a-9b3d-0c2f6a1b2c3d not found at 2026-10-12T10:11:12Z`:            `order <UUID> not found at <TS>`,
		`session deadbeef12345678 expired after 30s (limit 5m0s)`:                                 `session <HEX> expired after <NUM> limit <NUM>`,
		`latency 1.234567s exceeds 500ms`:                                                         `latency <NUM> exceeds <NUM>`,
		`dial tcp: lookup api.example.com on 10.0.0.2:53: no such host`:                           `dial tcp lookup <HOST> on <IP> no such host`,
		`status 503 does not match 200`:                                                           `status <NUM> does not match <NUM>`,
		`user42 failed with id=9981 on /api/items/7`:                                              `user<NUM> failed with id=<NUM> on <PATH>`,
		`required extraction "csrf" found nothing`:                                                `required extraction <STR> found nothing`,
		`can't open 'file one' now`:                                                               `can't open <STR> now`,
	}
	for in, want := range cases {
		if got := strings.Join(Normalize(in), " "); got != want {
			t.Errorf("Normalize(%q)\n got  %q\n want %q", in, got, want)
		}
	}
	if got := Normalize(strings.Repeat("word ", 500)); len(got) > maxTokens {
		t.Errorf("token cap: %d", len(got))
	}
}

func TestDrainRecoversKnownTemplates(t *testing.T) {
	gens := []struct {
		tmpl string
		gen  func(i int) string
	}{
		{`Get <STR> dial tcp <IP> connect connection refused`, func(i int) string {
			return fmt.Sprintf(`Get "http://127.0.0.1:%d/api/%d": dial tcp 127.0.0.1:%d: connect: connection refused`, 8000+i, i, 8000+i)
		}},
		{`read tcp <IP>-><IP> read connection reset by peer`, func(i int) string {
			return fmt.Sprintf(`read tcp 127.0.0.1:%d->127.0.0.1:%d: read: connection reset by peer`, 8099, 50000+i)
		}},
		{`required extraction <STR> found nothing`, func(i int) string {
			return fmt.Sprintf(`required extraction "var%d" found nothing`, i)
		}},
		{`body does not contain <STR>`, func(i int) string { return fmt.Sprintf(`body does not contain "welcome back user %d"`, i) }},
		{`order <UUID> was not found for customer <*>`, func(i int) string {
			return fmt.Sprintf(`order 8f14e45f-ceea-467a-9b3d-%012d was not found for customer %s`, i, []string{"alice", "bob", "carol", "dave"}[i%4])
		}},
	}
	d := NewDrain(0, 0)
	for i := 0; i < 400; i++ {
		g := gens[i%len(gens)]
		d.Add(Normalize(g.gen(i)))
	}
	var got []string
	for _, g := range d.Groups() {
		got = append(got, g.Template())
	}
	var want []string
	for _, g := range gens {
		want = append(want, g.tmpl)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("templates\n got  %q\n want %q", got, want)
	}
	for _, g := range d.Groups() {
		if g.count != 80 {
			t.Errorf("group %q count %d", g.Template(), g.count)
		}
	}
}

func TestDrainCaps(t *testing.T) {
	d := NewDrain(10, 0.7)
	for i := 0; i < 500; i++ {
		// 100 distinct 6-word shapes (all-different words), five times each
		w := fmt.Sprintf("alpha%[1]c beta%[1]c gamma%[1]c delta%[1]c eps%[1]c zeta%[1]c", rune('a'+i%26)) + strings.Repeat(" x", i%7)
		d.Add(strings.Fields(w))
	}
	if n := len(d.Groups()); n != 10 {
		t.Fatalf("groups = %d, want capped at 10", n)
	}
	if d.Other().count == 0 {
		t.Error("overflow group should have counted the rest")
	}
	var sum uint64 = d.Other().count
	for _, g := range d.Groups() {
		sum += g.count
	}
	if sum != 500 {
		t.Errorf("counts must add up to 500, got %d", sum)
	}
}

func TestClusterCapCountsOverflow(t *testing.T) {
	c := newCSV()
	for i := 0; i < 50; i++ {
		c.fail(float64(i), fmt.Sprintf("GET /r%d", i), 500, "http_5xx", "vu-1", "500 Internal Server Error")
	}
	r := analyze(t, c, Options{MaxClusters: 10})
	if len(r.Clusters) != 11 || r.OverflowCount != 40 {
		t.Fatalf("clusters=%d overflow=%d", len(r.Clusters), r.OverflowCount)
	}
	var total uint64
	for _, cl := range r.Clusters {
		total += cl.Count
	}
	if total != 50 || r.TotalFailures != 50 {
		t.Errorf("total %d / %d", total, r.TotalFailures)
	}
}

func byReason(r *Result) map[string]Cluster {
	m := map[string]Cluster{}
	for _, c := range r.Clusters {
		m[c.Reason] = c
	}
	return m
}

func TestClassificationRules(t *testing.T) {
	c := newCSV()
	c.ok(0, "GET /home")
	n := 0
	add := func(code int, kind, msg string) {
		n++
		c.fail(float64(n), fmt.Sprintf("REQ%d", n), code, kind, fmt.Sprintf("vu-%d", n), msg)
	}
	add(0, "blocked_by_policy", "host evil.example.com is not in the allowlist")
	add(0, "extraction", `required extraction "csrf" found nothing`)
	add(401, "http_4xx", "401 Unauthorized")
	add(200, "assertion", `body does not contain "Welcome"`)
	add(503, "assertion", `status 503 does not match 200`)
	add(200, "assertion", `latency 2.1s exceeds 1s`)
	add(429, "http_4xx", "429 Too Many Requests")
	add(502, "http_5xx", "502 Bad Gateway")
	add(0, "timeout", `Get "x": context deadline exceeded`)
	add(0, "connect", `dial tcp 127.0.0.1:50000: socket: too many open files`)
	add(0, "connect", `dial tcp 127.0.0.1:8099: connect: connection refused`)
	add(0, "connect", `dial tcp: lookup nope.example.com: no such host`)
	add(0, "other", `read tcp 127.0.0.1:1->127.0.0.1:2: read: connection reset by peer`)
	add(0, "invalid_request", `parse "http://${host}/x": invalid`)
	add(0, "other", `something odd`)
	r := analyze(t, c, Options{})
	want := map[string]string{
		"policy_block": SidePolicy, "4xx_after_extractor_failure": SideScript, "extraction_missing": SideScript, "assertion_value": SideScript,
		"assertion_on_error_response": SideTarget, "assertion_latency": SideTarget,
		"rate_limited": SideTarget, "server_error": SideTarget, "timeout": SideTarget,
		"local_port_exhaustion": SideGenerator, "connection_refused": SideTarget, "dns_failure": SideScript,
		"connection_dropped": SideTarget, "unresolved_variable": SideScript, "unclassified": SideUnknown,
	}
	got := byReason(r)
	for reason, side := range want {
		cl, ok := got[reason]
		if !ok {
			t.Errorf("no cluster with reason %s; have %v", reason, keys(got))
			continue
		}
		if cl.Side != side {
			t.Errorf("%s: side %s want %s", reason, cl.Side, side)
		}
		if cl.Explanation == "" || cl.NextCheck == "" {
			t.Errorf("%s: missing explanation/next check", reason)
		}
	}
	// without any extraction failure in the run, 401 and 404 are plain script-side rejections
	c2 := newCSV()
	c2.fail(1, "GET /me", 401, "http_4xx", "vu-1", "401 Unauthorized")
	c2.fail(2, "GET /gone", 404, "http_4xx", "vu-1", "404 Not Found")
	g2 := byReason(analyze(t, c2, Options{}))
	if g2["auth_rejected"].Side != SideScript || g2["client_error"].Side != SideScript {
		t.Errorf("%v", keys(g2))
	}
}

func keys(m map[string]Cluster) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestFourXXRightAfterExtractionFailureSameUser(t *testing.T) {
	c := newCSV()
	for i := 0; i < 30; i++ {
		vu := fmt.Sprintf("vu-%d", i)
		c.fail(float64(i), "POST /login", 200, "extraction", vu, `required extraction "token" found nothing`)
		c.fail(float64(i)+0.5, "GET /cart", 401, "http_4xx", vu, "401 Unauthorized")
	}
	r := analyze(t, c, Options{})
	cl := byReason(r)["4xx_after_extraction"]
	if cl.Side != SideScript || cl.Confidence != "high" || cl.AfterExtractionPct != 100 {
		t.Fatalf("%+v", cl)
	}
}

func TestGeneratorBusyTimeoutsAndSignals(t *testing.T) {
	c := newCSV()
	busy := make([]bool, 20)
	for i := 10; i < 20; i++ {
		busy[i] = true
		c.fail(float64(i), "GET /a", 0, "timeout", "vu-1", `context deadline exceeded`)
	}
	r := analyze(t, c, Options{GeneratorBusy: busy})
	if cl := r.Clusters[0]; cl.Side != SideGenerator || cl.Reason != "timeout_generator_busy" {
		t.Errorf("%+v", cl)
	}
}

func TestPriorityScoreAndRanking(t *testing.T) {
	c := newCSV()
	// big late cluster (500s from 60s), small early cluster on a critical request, mid cluster at a change point
	for i := 0; i < 80; i++ {
		c.fail(60+float64(i)/4, "GET /big", 500, "http_5xx", "vu-1", "500 Internal Server Error")
	}
	for i := 0; i < 5; i++ {
		c.fail(float64(i), "GET /crit", 404, "http_4xx", "vu-2", "404 Not Found")
	}
	for i := 0; i < 15; i++ {
		c.fail(61+float64(i)/5, "GET /cp", 503, "http_5xx", "vu-3", "503 Service Unavailable")
	}
	r := analyze(t, c, Options{DurationSec: 100, Critical: map[string]bool{"GET /crit": true}, ChangePoints: []float64{60}})
	if r.TotalFailures != 100 {
		t.Fatal(r.TotalFailures)
	}
	top := r.Clusters[0]
	if top.Request != "GET /big" || top.Priority != 40+6+20 {
		// share 80% -> 40, early 15*(1-0.6)=6, change point 20
		t.Errorf("top: %+v parts=%+v", top.Request, top.Parts)
	}
	byReq := map[string]Cluster{}
	for _, cl := range r.Clusters {
		byReq[cl.Request] = cl
	}
	if p := byReq["GET /crit"].Parts; p.Critical != 15 || p.ChangePoint != 0 {
		t.Errorf("crit parts %+v", p)
	}
	if !byReq["GET /cp"].BeganAtChangePoint {
		t.Error("cp cluster should be flagged")
	}
	if byReq["GET /big"].Percent != 80 || byReq["GET /big"].FirstSeenSec != 60 || byReq["GET /big"].LastSeenSec != 79.8 {
		t.Errorf("big: %+v", byReq["GET /big"])
	}
	// sparkline: all of /big falls in the second half
	sp := byReq["GET /big"].Spark
	first, second := 0, 0
	for i, v := range sp {
		if i < 12 {
			first += v
		} else {
			second += v
		}
	}
	if first != 0 || second != 80 {
		t.Errorf("spark %v", sp)
	}
}

func TestDeterministic(t *testing.T) {
	c := newCSV()
	for i := 0; i < 500; i++ {
		c.fail(float64(i%60), fmt.Sprintf("GET /r%d", i%7), []int{500, 503, 404}[i%3], []string{"http_5xx", "http_5xx", "http_4xx"}[i%3], "vu-1", fmt.Sprintf("err %d from 10.0.0.%d", i, i%250))
	}
	a := analyze(t, c, Options{})
	b := analyze(t, c, Options{})
	if !reflect.DeepEqual(a, b) {
		t.Error("two runs over the same CSV differ")
	}
}

func TestRedactedExample(t *testing.T) {
	c := newCSV()
	c.fail(1, "GET /x", 500, "http_5xx", "vu-1", `upstream said password=hunter2 for bob@example.com`)
	r := analyze(t, c, Options{})
	ex := r.Clusters[0].Example
	if strings.Contains(ex.Message, "hunter2") || strings.Contains(ex.Message, "bob@example.com") || strings.Contains(ex.URL, "abc123") {
		t.Errorf("example not redacted: %+v", ex)
	}
}

func TestTransactionRowsNotCountedAsRequests(t *testing.T) {
	c := newCSV()
	c.w.Write([]string{"1000500", "10", "Checkout", "0", "transaction", "vu-1", "false", "", "0", "0", "0", "", "transaction"})
	c.fail(0.5, "GET /pay", 500, "http_5xx", "vu-1", "500 Internal Server Error")
	r := analyze(t, c, Options{})
	if r.TotalFailures != 1 || r.TxFailures["Checkout"] != 1 || len(r.Clusters) != 1 {
		t.Errorf("%+v", r)
	}
}

func TestBadCSV(t *testing.T) {
	if _, err := Analyze(strings.NewReader("a,b\n1,2\n"), Options{}); err == nil {
		t.Error("expected error for foreign CSV")
	}
	if _, err := Analyze(strings.NewReader(""), Options{}); err == nil {
		t.Error("expected error for empty CSV")
	}
	// a header-only file is a valid, empty result
	r, err := Analyze(strings.NewReader(hdr), Options{})
	if err != nil || len(r.Clusters) != 0 {
		t.Errorf("%v %+v", err, r)
	}
}

func TestChangePoints(t *testing.T) {
	var tl []metrics.WindowOut
	for i := 0; i < 60; i++ {
		w := metrics.WindowOut{T: i, P95Ms: 50, ErrorRate: 0}
		if i >= 40 {
			w.ErrorRate, w.P95Ms = 12, 400
		}
		tl = append(tl, w)
	}
	cp := ChangePoints(tl)
	if len(cp) != 1 || cp[0] < 39 || cp[0] > 41 {
		t.Errorf("cp = %v", cp)
	}
	flat := make([]metrics.WindowOut, 60)
	for i := range flat {
		flat[i] = metrics.WindowOut{T: i, P95Ms: 50, ErrorRate: 2}
	}
	if cp := ChangePoints(flat); len(cp) != 0 {
		t.Errorf("flat series should have no change point: %v", cp)
	}
}

func TestAnalyzeRunAndCoarseFallback(t *testing.T) {
	dir := t.TempDir()
	c := newCSV()
	c.ok(0, "GET /a")
	c.fail(3, "GET /flaky", 500, "http_5xx", "vu-1", "500 Internal Server Error")
	if err := os.WriteFile(filepath.Join(dir, "raw.csv"), []byte(c.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := `{"steps":[{"type":"transaction","name":"Shop","steps":[{"type":"request","name":"","request":{"method":"GET","url":"/flaky"}}]}]}`
	os.WriteFile(filepath.Join(dir, "scenario.json"), []byte(sc), 0o644)
	sum := &results.Summary{RunID: "r1", StartedAt: t0, DurationS: 10,
		SLAs: []results.SLAResult{{}}}
	sum.SLAs[0].Scope = "Shop"
	r, err := AnalyzeRun(dir, sum)
	if err != nil {
		t.Fatal(err)
	}
	cl := r.Clusters[0]
	if cl.Request != "GET /flaky" || cl.Transaction != "Shop" || !cl.Critical || cl.FirstSeenSec != 3 || r.RunID != "r1" {
		t.Errorf("%+v", cl)
	}
	// no raw.csv: coarse result from the summary
	dir2 := t.TempDir()
	sum.Snapshot.Requests.Errors, sum.Snapshot.Requests.Count = 7, 100
	sum.Snapshot.ByRequest = []metrics.Named{{Name: "GET /flaky", MetricSet: metrics.MetricSet{Count: 100, Errors: 7, ErrorKind: map[string]uint64{"http_5xx": 7}}}}
	r2, err := AnalyzeRun(dir2, sum)
	if err != nil || !r2.Coarse || len(r2.Clusters) != 1 || r2.Clusters[0].Side != SideTarget || r2.Clusters[0].Count != 7 {
		t.Fatalf("%v %+v", err, r2)
	}
}

func TestHTMLSectionEscapes(t *testing.T) {
	c := newCSV()
	c.fail(1, `GET /<script>alert(1)</script>`, 500, "http_5xx", "vu-1", `<img src=x onerror=alert(1)>`)
	h := string(analyze(t, c, Options{DurationSec: 10}).HTMLSection(5))
	if strings.Contains(h, "<script>") || strings.Contains(h, "<img src") {
		t.Errorf("unescaped HTML: %s", h)
	}
	if !strings.Contains(h, "Failure triage") {
		t.Error("missing heading")
	}
}

// 1M-row CSV (about 12% failures) must triage in a few seconds with bounded memory.
func TestPerformanceOneMillionRows(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		w := csv.NewWriter(pw)
		w.Write(strings.Split(strings.TrimSpace(hdr), ","))
		for i := 0; i < 1_000_000; i++ {
			ts := strconv.FormatInt(1_000_000+int64(i/100), 10)
			label := "GET /api/items/" + strconv.Itoa(i%20)
			switch {
			case i%8 == 0:
				w.Write([]string{ts, "12", label, "503", "http_5xx", "vu-" + strconv.Itoa(i%50), "false", "503 Service Unavailable", "10", "5", "3", "http://127.0.0.1:8099/api/items/1", "request"})
			case i%50 == 1:
				w.Write([]string{ts, "12", label, "0", "connect", "vu-1", "false", fmt.Sprintf(`Get "http://127.0.0.1:8099/x": dial tcp 127.0.0.1:%d: connect: connection refused`, 40000+i%20000), "0", "5", "0", "http://127.0.0.1:8099/x", "request"})
			case i%97 == 2:
				w.Write([]string{ts, "12", label, "200", "assertion", "vu-2", "false", fmt.Sprintf("body does not contain \"order %d\"", i), "10", "5", "3", "http://127.0.0.1:8099/x", "request"})
			default:
				w.Write([]string{ts, "12", label, "200", "", "vu-" + strconv.Itoa(i%50), "true", "", "512", "120", "3", "http://127.0.0.1:8099/api/items/1", "request"})
			}
		}
		w.Flush()
		pw.Close()
	}()
	start := time.Now()
	r, err := Analyze(pr, Options{Start: time.UnixMilli(1_000_000), DurationSec: 10000})
	el := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("1M rows: %d failures, %d clusters, %d templates in %s", r.TotalFailures, len(r.Clusters), r.Templates, el)
	if r.TotalRequests != 1_000_000 || r.TotalFailures < 100000 {
		t.Errorf("counts %d / %d", r.TotalRequests, r.TotalFailures)
	}
	if r.Templates > 10 {
		t.Errorf("templates = %d; the 3 message shapes should not explode", r.Templates)
	}
	if limit := 8 * time.Second; el > limit {
		t.Errorf("took %s, limit %s", el, limit)
	}
}
