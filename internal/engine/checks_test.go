package engine

import (
	"bytes"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/model"
)

const itemsBody = `<a id="1">x</a><a id="2">y</a><a id="3">z</a>`

func checksServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/items":
			w.Header().Set("X-Trace", "abc123")
			fmt.Fprint(w, itemsBody)
		case "/few":
			fmt.Fprint(w, `<a id="9">q</a>`)
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[{"id":10},{"id":20}],"nothing":null,"name":"Bob"}`)
		case "/fail":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "missing thing")
		default:
			fmt.Fprint(w, "ok")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func query(t *testing.T, e Exchange) url.Values {
	t.Helper()
	u, err := url.Parse(e.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestExtractAllTemplateAndSources(t *testing.T) {
	t.Parallel()
	srv := checksServer(t)
	rec := &recorder{}
	items := withExtract(get("items", "/items?q=term"),
		model.Extractor{Name: "ids", Type: model.ExRegex, Expr: `id="(\d+)">(\w)`, Match: model.MatchAll},
		model.Extractor{Name: "t", Type: model.ExRegex, Expr: `id="(\d+)">(\w)`, Match: 2, ExtractorOptions: model.ExtractorOptions{Template: "$2$-$1$"}},
		model.Extractor{Name: "whole", Type: model.ExRegex, Expr: `id="(\d+)"`, ExtractorOptions: model.ExtractorOptions{Template: "[$0$]"}},
		model.Extractor{Name: "capped", Type: model.ExBoundary, Left: `">`, Right: "</", Match: model.MatchAll, ExtractorOptions: model.ExtractorOptions{MaxMatches: 2}},
		model.Extractor{Name: "trace", Type: model.ExRegex, Expr: `(?m)^X-Trace: (\w+)$`, ExtractorOptions: model.ExtractorOptions{Source: model.SrcHeaders}},
		model.Extractor{Name: "line", Type: model.ExRegex, Expr: `^HTTP/1\.1 (\d+)`, ExtractorOptions: model.ExtractorOptions{Source: model.SrcHeaders}},
		model.Extractor{Name: "req", Type: model.ExRegex, Expr: `X-Req: (\w+)`, ExtractorOptions: model.ExtractorOptions{Source: model.SrcRequestHeaders}},
		model.Extractor{Name: "q", Type: model.ExRegex, Expr: `q=(\w+)`, ExtractorOptions: model.ExtractorOptions{Source: model.SrcURL}},
		model.Extractor{Name: "code", Type: model.ExBoundary, Left: "2", Right: "0", ExtractorOptions: model.ExtractorOptions{Source: model.SrcStatus}},
		model.Extractor{Name: "msg", Type: model.ExRegex, Expr: `(\w+)`, ExtractorOptions: model.ExtractorOptions{Source: model.SrcMessage}},
	)
	items.Request.Headers = []model.Header{{Name: "X-Req", Value: "hello"}}
	echo := get("echo", "/echo?a=${ids_1}&b=${ids_3}&n=${ids_matchNr}&g=${ids_2_g2}&gc=${ids_2_g}&t=${t}&all=${ids_ALL}"+
		"&first=${ids}&whole=${whole}&wg=${whole_g1}&cap=${capped_matchNr}&trace=${trace}&line=${line}&req=${req}&q=${q}&code=${code}&msg=${msg}")
	few := withExtract(get("few", "/few"), model.Extractor{Name: "ids", Type: model.ExRegex, Expr: `id="(\d+)">(\w)`, Match: model.MatchAll})
	echo2 := get("echo2", "/echo?n=${ids_matchNr}&a=${ids_1}&stale=${ids_2}")
	single := withExtract(get("single", "/items"), model.Extractor{Name: "ids", Type: model.ExRegex, Expr: `id="(\d+)"`, Match: 1})
	echo3 := get("echo3", "/echo?v=${ids}&left=${ids_1}&nr=${ids_matchNr}")
	sc := newSc(srv.URL, perVU(1, 1), items, echo, few, echo2, single, echo3)
	if p := sc.Validate(); len(p) > 0 {
		t.Fatal(p)
	}
	runSc(t, sc, opts(rec))

	q := query(t, rec.byStep("echo")[0])
	want := map[string]string{"a": "1", "b": "3", "n": "3", "g": "y", "gc": "2", "t": "y-2", "all": "1,2,3", "first": "1",
		"whole": `[id="1"]`, "wg": "1", "cap": "2", "trace": "abc123", "line": "200", "req": "hello", "q": "term", "code": "", "msg": "OK"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q want %q", k, q.Get(k), v)
		}
	}
	e2 := rec.byStep("echo2")[0]
	if q2 := query(t, e2); q2.Get("n") != "1" || q2.Get("a") != "9" {
		t.Errorf("second all-extraction: %v", q2)
	}
	if !strings.Contains(strings.Join(e2.Unresolved, ","), "ids_2") {
		t.Errorf("stale ids_2 should be removed: unresolved=%v url=%s", e2.Unresolved, e2.URL)
	}
	e3 := rec.byStep("echo3")[0]
	if query(t, e3).Get("v") != "1" || !strings.Contains(strings.Join(e3.Unresolved, ","), "ids_1") ||
		!strings.Contains(strings.Join(e3.Unresolved, ","), "ids_matchNr") {
		t.Errorf("single match after all must clear name_N/matchNr: %s %v", e3.URL, e3.Unresolved)
	}
}

func TestExtractedValuesAreNotReexpanded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `token=${secret}`)
	}))
	t.Cleanup(srv.Close)
	rec := &recorder{}
	sc := newSc(srv.URL, perVU(1, 1),
		withExtract(get("a", "/"), model.Extractor{Name: "tok", Type: model.ExRegex, Expr: `token=(\S+)`}),
		model.Step{Type: model.StepSetVar, Set: []model.KV{{Name: "copy", Value: "${tok}"}}},
		get("b", "/b?t=${tok}"))
	sc.Variables = map[string]string{"secret": "s3cr3t", "chain": "${secret}-x"}
	runSc(t, sc, opts(rec))
	b := rec.byStep("b")[0]
	if strings.Contains(b.URL, "s3cr3t") {
		t.Errorf("extracted value was re-expanded: %s", b.URL)
	}
}

func TestChainedScenarioVariables(t *testing.T) {
	t.Parallel()
	srv := checksServer(t)
	rec := &recorder{}
	sc := newSc(srv.URL, perVU(1, 1), get("c", "${path}"))
	sc.Variables = map[string]string{"root": "/echo", "path": "${root}?x=${__vu()}"}
	runSc(t, sc, opts(rec))
	if e := rec.byStep("c")[0]; !strings.HasSuffix(e.URL, "/echo?x=1") {
		t.Errorf("chained variables: %s %v", e.URL, e.Unresolved)
	}
}

func TestAssertionParity(t *testing.T) {
	t.Parallel()
	srv := checksServer(t)
	rec := &recorder{}
	sum := md5.Sum([]byte(itemsBody))
	md5hex := hex.EncodeToString(sum[:])
	opt := func(o model.AssertionOptions) model.AssertionOptions { return o }
	pass := withAssert(get("pass", "/items"),
		model.Assertion{Type: model.AsEquals, Value: itemsBody},
		model.Assertion{Type: model.AsContains, Value: "zzz", AssertionOptions: opt(model.AssertionOptions{Not: true})},
		model.Assertion{Type: model.AsNotContains, Value: "x", AssertionOptions: opt(model.AssertionOptions{Not: true})},
		model.Assertion{Type: model.AsSize, Value: strconv.Itoa(len(itemsBody))},
		model.Assertion{Type: model.AsSize, Expr: "full", Value: strconv.Itoa(len(itemsBody)), AssertionOptions: opt(model.AssertionOptions{Op: ">"})},
		model.Assertion{Type: model.AsSize, Value: "10", AssertionOptions: opt(model.AssertionOptions{Op: "<=", Not: true})},
		model.Assertion{Type: model.AsHash, Value: strings.ToUpper(md5hex)},
		model.Assertion{Type: model.AsContains, Value: "abc123", AssertionOptions: opt(model.AssertionOptions{Source: model.SrcHeaders})},
		model.Assertion{Type: model.AsEquals, Value: "200", AssertionOptions: opt(model.AssertionOptions{Source: model.SrcStatus})},
		model.Assertion{Type: model.AsEquals, Value: "OK", AssertionOptions: opt(model.AssertionOptions{Source: model.SrcMessage})},
		model.Assertion{Type: model.AsRegex, Expr: `/items$`, AssertionOptions: opt(model.AssertionOptions{Source: model.SrcURL})},
		model.Assertion{Type: model.AsStatus, Value: "5xx", AssertionOptions: opt(model.AssertionOptions{Not: true})},
	)
	js := withAssert(get("json", "/json"),
		model.Assertion{Type: model.AsJSONPath, Expr: "$.items[*].id", Value: `2\d`, AssertionOptions: opt(model.AssertionOptions{Regex: true})},
		model.Assertion{Type: model.AsJSONPath, Expr: "$.nothing", AssertionOptions: opt(model.AssertionOptions{ExpectNull: true})},
		model.Assertion{Type: model.AsJSONPath, Expr: "$.missing", AssertionOptions: opt(model.AssertionOptions{Not: true})},
		model.Assertion{Type: model.AsJSONPath, Expr: "$.name", Value: "B", AssertionOptions: opt(model.AssertionOptions{Regex: true, Not: true})},
	)
	failNull := withAssert(get("failnull", "/json"),
		model.Assertion{Type: model.AsJSONPath, Expr: "$.name", AssertionOptions: opt(model.AssertionOptions{ExpectNull: true})})
	failMsg := withAssert(get("failmsg", "/items"),
		model.Assertion{Type: model.AsContains, Value: "x", AssertionOptions: opt(model.AssertionOptions{Not: true, Message: "item ${__vu()} leaked"})})
	failNot := withAssert(get("failnot", "/items"),
		model.Assertion{Type: model.AsContains, Value: "x", AssertionOptions: opt(model.AssertionOptions{Not: true})})
	failHash := withAssert(get("failhash", "/items"), model.Assertion{Type: model.AsHash, Expr: "sha256", Value: md5hex})
	ignored := get("ignored", "/fail")
	ignored.Request.IgnoreStatus = true
	ignoredChecked := withAssert(get("ignoredChecked", "/fail"), model.Assertion{Type: model.AsContains, Value: "missing"})
	ignoredChecked.Request.IgnoreStatus = true
	ignoredFail := withAssert(get("ignoredFail", "/fail"), model.Assertion{Type: model.AsContains, Value: "nope"})
	ignoredFail.Request.IgnoreStatus = true
	plainFail := get("plainFail", "/fail")
	sc := newSc(srv.URL, perVU(1, 1), pass, js, failNull, failMsg, failNot, failHash, ignored, ignoredChecked, ignoredFail, plainFail)
	if p := sc.Validate(); len(p) > 0 {
		t.Fatal(p)
	}
	runSc(t, sc, opts(rec))
	for _, name := range []string{"pass", "json", "ignored", "ignoredChecked"} {
		e := rec.byStep(name)[0]
		if !e.OK {
			t.Errorf("%s failed: %s %+v", name, e.Error, e.Assertions)
		}
	}
	for name, msg := range map[string]string{"failnull": "expected null", "failmsg": "item 1 leaked", "failnot": "NOT contains",
		"failhash": "sha256", "ignoredFail": "does not contain", "plainFail": "404"} {
		e := rec.byStep(name)[0]
		if e.OK || !strings.Contains(e.Error, msg) {
			t.Errorf("%s: ok=%v error %q, want it to contain %q", name, e.OK, e.Error, msg)
		}
	}
}

func TestTransactionRowsInRawJTL(t *testing.T) {
	t.Parallel()
	srv := checksServer(t)
	var raw bytes.Buffer
	o := opts(nil)
	o.Raw = &raw
	tx := model.Step{Type: model.StepTransaction, Name: "checkout", Steps: []model.Step{get("ok", "/items"), get("bad", "/fail")}}
	sc := newSc(srv.URL, perVU(1, 1), tx)
	runSc(t, sc, o)
	rows, err := csv.NewReader(&raw).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	found := false
	for _, r := range rows[1:] {
		if r[col["grpThreads"]] != "1" || r[col["allThreads"]] != "1" {
			t.Errorf("threads columns: %v", r)
		}
		if r[col["label"]] == "checkout" {
			found = true
			if m := r[col["responseMessage"]]; m != "Number of samples in transaction : 2, number of failing samples : 1" {
				t.Errorf("transaction message %q", m)
			}
		} else if strings.HasPrefix(r[col["responseMessage"]], "Number of samples") {
			t.Errorf("request row marked as transaction: %v", r)
		}
	}
	if !found {
		t.Error("no transaction row")
	}
}
