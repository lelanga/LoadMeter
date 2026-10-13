package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMatchIndexJSON(t *testing.T) {
	for in, want := range map[string]MatchIndex{`1`: 1, `0`: 0, `-1`: MatchRandom, `"all"`: MatchAll, `"ALL"`: MatchAll,
		`"random"`: MatchRandom, `"first"`: MatchFirst, `"3"`: 3} {
		var m MatchIndex
		if err := json.Unmarshal([]byte(in), &m); err != nil || m != want {
			t.Errorf("%s -> %d %v, want %d", in, m, err, want)
		}
	}
	for _, bad := range []string{`-2`, `"some"`, `1.5`, `true`} {
		var m MatchIndex
		if err := json.Unmarshal([]byte(bad), &m); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// Round trip inside an extractor; numbers stay numbers (backward compatible output).
	in := `{"name":"a","type":"regex","expr":"x","match":"all","template":"$1$","source":"headers","maxMatches":5}`
	var e Extractor
	if err := json.Unmarshal([]byte(in), &e); err != nil {
		t.Fatal(err)
	}
	if e.Match != MatchAll || e.Template != "$1$" || e.Source != SrcHeaders || e.MaxMatches != 5 {
		t.Fatalf("decoded %+v", e)
	}
	out, _ := json.Marshal(e)
	if !strings.Contains(string(out), `"match":"all"`) || !strings.Contains(string(out), `"template":"$1$"`) {
		t.Errorf("encoded %s", out)
	}
	out, _ = json.Marshal(Extractor{Name: "b", Type: ExRegex, Match: 2})
	if string(out) != `{"name":"b","type":"regex","match":2}` {
		t.Errorf("legacy encoding changed: %s", out)
	}
	out, _ = json.Marshal(Extractor{Name: "b", Type: ExRegex})
	if strings.Contains(string(out), "match") {
		t.Errorf("zero match should be omitted: %s", out)
	}
}

func TestAssertionOptionsJSON(t *testing.T) {
	in := `{"type":"jsonpath","expr":"$.a","value":"x.*","not":true,"regex":true,"message":"bad"}`
	var a Assertion
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	if !a.Not || !a.Regex || a.Message != "bad" {
		t.Fatalf("%+v", a)
	}
	var r Request
	if err := json.Unmarshal([]byte(`{"method":"GET","url":"/","ignoreStatus":true}`), &r); err != nil || !r.IgnoreStatus {
		t.Fatalf("ignoreStatus: %+v %v", r, err)
	}
}

func TestValidateChecks(t *testing.T) {
	mk := func(ex []Extractor, as []Assertion) *Scenario {
		s := NewScenario("v")
		s.Steps = []Step{{Type: StepRequest, Request: &Request{Method: "GET", URL: "http://x/", Extract: ex, Assert: as}}}
		return s
	}
	ok := mk([]Extractor{
		{Name: "a", Type: ExRegex, Expr: "(x)", Match: MatchAll, ExtractorOptions: ExtractorOptions{Template: "$1$", Source: SrcURL}},
		{Name: "b", Type: ExBoundary, ExtractorOptions: ExtractorOptions{Source: SrcHeaders}},
	}, []Assertion{
		{Type: AsEquals, Value: "x", AssertionOptions: AssertionOptions{Source: SrcStatus, Not: true}},
		{Type: AsSize, Value: "10", AssertionOptions: AssertionOptions{Op: "<="}},
		{Type: AsSize, Expr: "full", Value: "${n}"},
		{Type: AsHash, Expr: "SHA256", Value: "ab"},
		{Type: AsJSONPath, Expr: "$.a", Value: "", AssertionOptions: AssertionOptions{Regex: true}},
		{Type: AsJSONPath, Expr: "$.a", AssertionOptions: AssertionOptions{ExpectNull: true}},
		{Type: AsMaxLatency, Value: "1s", AssertionOptions: AssertionOptions{Not: true, Message: "slow"}},
	})
	if p := ok.Validate(); len(p) > 0 {
		t.Fatalf("valid scenario rejected: %v", p)
	}
	bad := mk([]Extractor{
		{Name: "a", Type: ExJSONPath, Expr: "$", ExtractorOptions: ExtractorOptions{Source: SrcHeaders}},
		{Name: "b", Type: ExRegex, Expr: "x", ExtractorOptions: ExtractorOptions{Source: "cookies"}},
		{Name: "c", Type: ExBoundary, ExtractorOptions: ExtractorOptions{Template: "$1$"}},
		{Name: "d", Type: ExRegex, Expr: "x", Match: -3},
		{Name: "e", Type: ExRegex, Expr: "x", ExtractorOptions: ExtractorOptions{MaxMatches: -1}},
	}, []Assertion{
		{Type: "nope"},
		{Type: AsSize, Value: "big"},
		{Type: AsSize, Value: "1", AssertionOptions: AssertionOptions{Op: "~"}},
		{Type: AsSize, Expr: "headers", Value: "1"},
		{Type: AsHash, Expr: "crc32", Value: "1"},
		{Type: AsHash},
		{Type: AsStatus, AssertionOptions: AssertionOptions{Source: SrcBody}},
		{Type: AsContains, AssertionOptions: AssertionOptions{Source: "cookies"}},
		{Type: AsContains, AssertionOptions: AssertionOptions{Op: "=="}},
		{Type: AsRegex, AssertionOptions: AssertionOptions{Regex: true}},
		{Type: AsJSONPath, AssertionOptions: AssertionOptions{Regex: true, ExpectNull: true}},
	})
	p := strings.Join(bad.Validate(), "\n")
	for _, want := range []string{"source is only valid for regex and boundary", `unknown source "cookies"`, "template is only valid for regex",
		"match -3 is invalid", "maxMatches must be >= 0", `unknown assertion type "nope"`, `size value "big"`, `size op "~"`,
		`size expr "headers"`, `hash expr "crc32"`, "needs the expected hex digest", "source is not valid on a status assertion",
		"op is only valid on a size assertion", "only valid on a jsonpath assertion", "cannot set both regex and expectNull"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing problem %q in:\n%s", want, p)
		}
	}
}
