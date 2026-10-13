package jmx

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/model"
)

func el(tc, name, props string) string {
	return `<` + tc + ` guiclass="X" testclass="` + tc + `" testname="` + name + `" enabled="true">` + props + `</` + tc + `><hashTree/>`
}

func sp(name, v string) string { return `<stringProp name="` + name + `">` + v + `</stringProp>` }
func bp(name, v string) string { return `<boolProp name="` + name + `">` + v + `</boolProp>` }

func regexXML(name, useHeaders, re, tpl, match string) string {
	return el("RegexExtractor", name, sp("RegexExtractor.useHeaders", useHeaders)+sp("RegexExtractor.refname", name)+
		sp("RegexExtractor.regex", re)+sp("RegexExtractor.template", tpl)+sp("RegexExtractor.match_number", match))
}

func importOne(t *testing.T, kids string) (*model.Request, *model.CompatReport) {
	t.Helper()
	sc, rep := importString(t, plan("P", tg(loops("1"), sampler("s", "", kids)), ""))
	return findStep(sc.Steps, "s").Request, rep
}

func TestImportExtractorParity(t *testing.T) {
	kids := regexXML("all", "false", `id=(\d+)`, "$1$", "-1") +
		regexXML("rnd", "false", `id=(\d+)`, "$1$", "0") +
		regexXML("third", "false", `id=(\d+)`, "$1$", "3") +
		regexXML("tpl", "false", `(\w+)=(\d+)`, "$2$-$1$", "1") +
		regexXML("whole", "false", `id=(\d+)`, "$0$", "1") +
		regexXML("hdrs", "true", `Location: (.*)`, "$1$", "1") +
		regexXML("reqh", "request_headers", `Cookie: (.*)`, "$1$", "1") +
		regexXML("url", "URL", `id=(\d+)`, "$1$", "1") +
		regexXML("msg", "message", `(.*)`, "$1$", "1") +
		regexXML("code", "code", `(2)\d\d`, "$1$", "1") +
		regexXML("doc", "as_document", `x`, "$1$", "1") +
		el("BoundaryExtractor", "b", sp("BoundaryExtractor.useHeaders", "true")+sp("BoundaryExtractor.refname", "b")+
			sp("BoundaryExtractor.lboundary", "X-A: ")+sp("BoundaryExtractor.rboundary", "\n")+sp("BoundaryExtractor.match_number", "-1")) +
		el("JSONPostProcessor", "j", sp("JSONPostProcessor.referenceNames", "j")+sp("JSONPostProcessor.jsonPathExprs", "$..id")+
			sp("JSONPostProcessor.match_numbers", "-1")+bp("JSONPostProcessor.compute_concat", "true"))
	req, rep := importOne(t, kids)
	by := map[string]model.Extractor{}
	for _, e := range req.Extract {
		by[e.Name] = e
	}
	want := map[string]model.Extractor{
		"all":   {Name: "all", Type: model.ExRegex, Expr: `id=(\d+)`, Group: 1, Match: model.MatchAll},
		"rnd":   {Name: "rnd", Type: model.ExRegex, Expr: `id=(\d+)`, Group: 1, Match: model.MatchRandom},
		"third": {Name: "third", Type: model.ExRegex, Expr: `id=(\d+)`, Group: 1, Match: 3},
		"tpl":   {Name: "tpl", Type: model.ExRegex, Expr: `(\w+)=(\d+)`, Match: 1, ExtractorOptions: model.ExtractorOptions{Template: "$2$-$1$"}},
		"whole": {Name: "whole", Type: model.ExRegex, Expr: `id=(\d+)`, Match: 1, ExtractorOptions: model.ExtractorOptions{Template: "$0$"}},
		"hdrs":  {Name: "hdrs", Type: model.ExRegex, Expr: `Location: (.*)`, Group: 1, Match: 1, ExtractorOptions: model.ExtractorOptions{Source: model.SrcHeaders}},
		"reqh":  {Name: "reqh", Type: model.ExRegex, Expr: `Cookie: (.*)`, Group: 1, Match: 1, ExtractorOptions: model.ExtractorOptions{Source: model.SrcRequestHeaders}},
		"url":   {Name: "url", Type: model.ExRegex, Expr: `id=(\d+)`, Group: 1, Match: 1, ExtractorOptions: model.ExtractorOptions{Source: model.SrcURL}},
		"msg":   {Name: "msg", Type: model.ExRegex, Expr: `(.*)`, Group: 1, Match: 1, ExtractorOptions: model.ExtractorOptions{Source: model.SrcMessage}},
		"code":  {Name: "code", Type: model.ExRegex, Expr: `(2)\d\d`, Group: 1, Match: 1, ExtractorOptions: model.ExtractorOptions{Source: model.SrcStatus}},
		"b":     {Name: "b", Type: model.ExBoundary, Left: "X-A: ", Right: "\n", Match: model.MatchAll, ExtractorOptions: model.ExtractorOptions{Source: model.SrcHeaders}},
		"j":     {Name: "j", Type: model.ExJSONPath, Expr: "$..id", Match: model.MatchAll},
	}
	for name, w := range want {
		if got, ok := by[name]; !ok || !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %+v, want %+v", name, got, w)
		}
	}
	if _, ok := by["doc"]; ok {
		t.Error("as_document regex should not be mapped")
	}
	for loc, st := range map[string]string{"TestPlan/TG/s/all": model.Supported, "TestPlan/TG/s/tpl": model.Supported,
		"TestPlan/TG/s/hdrs": model.Partial, "TestPlan/TG/s/doc": model.Unsupported, "TestPlan/TG/s/j": model.Supported,
		"TestPlan/TG/s/url": model.Supported} {
		if it := findItem(rep, loc); it == nil || it.Status != st {
			t.Errorf("%s: item %+v, want %s", loc, it, st)
		}
	}
	sc := &model.Scenario{Version: model.SchemaVersion, Steps: []model.Step{{Type: model.StepRequest, Request: req}},
		Workload: model.Workload{Executor: model.ExecPerVUIterations, VUs: 1, Iterations: 1}}
	if p := sc.Validate(); len(p) > 0 {
		t.Errorf("imported extractors invalid: %v", p)
	}
}

func TestImportAssertionParity(t *testing.T) {
	jsonA := func(name, props string) string { return el("JSONPathAssertion", name, props) }
	ra := `<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname="ign" enabled="true"><collectionProp name="Asserion.test_strings"/>` +
		sp("Assertion.test_field", "Assertion.response_data") + bp("Assertion.assume_success", "true") +
		`<intProp name="Assertion.test_type">16</intProp></ResponseAssertion><hashTree/>`
	msgRA := `<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname="msg" enabled="true"><collectionProp name="Asserion.test_strings"><stringProp name="1">welcome</stringProp></collectionProp>` +
		sp("Assertion.test_field", "Assertion.response_data") + sp("Assertion.custom_message", "no welcome for ${user}") +
		`<intProp name="Assertion.test_type">16</intProp></ResponseAssertion><hashTree/>`
	kids := ra + msgRA +
		jsonA("inv", sp("JSON_PATH", "$.error")+bp("INVERT", "true")) +
		jsonA("null", sp("JSON_PATH", "$.x")+bp("JSONVALIDATION", "true")+bp("EXPECT_NULL", "true")) +
		jsonA("re", sp("JSON_PATH", "$.id")+bp("JSONVALIDATION", "true")+sp("EXPECTED_VALUE", `\d+`)) +
		jsonA("lit", sp("JSON_PATH", "$.n")+bp("JSONVALIDATION", "true")+sp("EXPECTED_VALUE", "abc")+bp("ISREGEX", "false")) +
		jsonA("empty", sp("JSON_PATH", "$.e")+bp("JSONVALIDATION", "true")+sp("EXPECTED_VALUE", "")) +
		el("SizeAssertion", "size", sp("Assertion.test_field", "SizeAssertion.response_data")+sp("SizeAssertion.size", "100")+`<intProp name="SizeAssertion.operator">6</intProp>`) +
		el("SizeAssertion", "full", sp("SizeAssertion.size", "5")+`<intProp name="SizeAssertion.operator">3</intProp>`) +
		el("SizeAssertion", "code", sp("Assertion.test_field", "SizeAssertion.response_code")+sp("SizeAssertion.size", "3")) +
		el("MD5HexAssertion", "md5", sp("MD5HexAssertion.size", "ABCDEF"))
	req, rep := importOne(t, kids)
	if !req.IgnoreStatus {
		t.Error("assume_success not mapped to ignoreStatus")
	}
	want := []model.Assertion{
		{Type: model.AsContains, Value: "welcome", AssertionOptions: model.AssertionOptions{Message: "no welcome for ${user}"}},
		{Type: model.AsJSONPath, Expr: "$.error", AssertionOptions: model.AssertionOptions{Not: true}},
		{Type: model.AsJSONPath, Expr: "$.x", AssertionOptions: model.AssertionOptions{ExpectNull: true}},
		{Type: model.AsJSONPath, Expr: "$.id", Value: `\d+`, AssertionOptions: model.AssertionOptions{Regex: true}},
		{Type: model.AsJSONPath, Expr: "$.n", Value: "abc"},
		{Type: model.AsJSONPath, Expr: "$.e", AssertionOptions: model.AssertionOptions{Regex: true}},
		{Type: model.AsSize, Expr: "body", Value: "100", AssertionOptions: model.AssertionOptions{Op: "<="}},
		{Type: model.AsSize, Expr: "full", Value: "5", AssertionOptions: model.AssertionOptions{Op: ">"}},
		{Type: model.AsHash, Expr: "md5", Value: "abcdef"},
	}
	if !reflect.DeepEqual(req.Assert, want) {
		t.Errorf("assertions\n got %+v\nwant %+v", req.Assert, want)
	}
	for loc, st := range map[string]string{"TestPlan/TG/s/ign": model.Supported, "TestPlan/TG/s/inv": model.Supported,
		"TestPlan/TG/s/code": model.Unsupported, "TestPlan/TG/s/md5": model.Supported, "TestPlan/TG/s/size": model.Supported} {
		if it := findItem(rep, loc); it == nil || it.Status != st {
			t.Errorf("%s: item %+v, want %s", loc, it, st)
		}
	}
}

func TestExportExtractorAndAssertionParity(t *testing.T) {
	sc := model.NewScenario("x")
	sc.Defaults.BaseURL = "http://h.example"
	sc.Workload = model.Workload{Executor: model.ExecPerVUIterations, VUs: 1, Iterations: 1}
	req := &model.Request{Method: "GET", URL: "/a", RequestCheckOptions: model.RequestCheckOptions{IgnoreStatus: true},
		Extract: []model.Extractor{
			{Name: "all", Type: model.ExRegex, Expr: `id=(\d+)`, Match: model.MatchAll},
			{Name: "tpl", Type: model.ExRegex, Expr: `(\w)(\d)`, Match: model.MatchRandom, ExtractorOptions: model.ExtractorOptions{Template: "$2$$1$", Source: model.SrcURL}},
			{Name: "b", Type: model.ExBoundary, Left: "a", Right: "b", ExtractorOptions: model.ExtractorOptions{Source: model.SrcMessage}},
			{Name: "j", Type: model.ExJSONPath, Expr: "$..id", Match: model.MatchAll},
		},
		Assert: []model.Assertion{
			{Type: model.AsContains, Value: "ok", AssertionOptions: model.AssertionOptions{Not: true, Message: "m1", Source: model.SrcHeaders}},
			{Type: model.AsEquals, Value: "OK", AssertionOptions: model.AssertionOptions{Source: model.SrcMessage}},
			{Type: model.AsStatus, Value: "500,503", AssertionOptions: model.AssertionOptions{Not: true}},
			{Type: model.AsJSONPath, Expr: "$.a", Value: "x+", AssertionOptions: model.AssertionOptions{Regex: true, Not: true}},
			{Type: model.AsSize, Expr: "full", Value: "10", AssertionOptions: model.AssertionOptions{Op: "<", Not: true}},
			{Type: model.AsHash, Value: "ABC"},
			{Type: model.AsHash, Expr: "sha256", Value: "abc"},
		}}
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "a", Request: req}}
	sc.DataSources = []model.DataSource{{Name: "users", File: "users.csv", Delimiter: ";", Columns: []string{"user", "pass"}}}
	if p := sc.Validate(); len(p) > 0 {
		t.Fatal(p)
	}
	out, rep, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	x := string(out)
	for _, want := range []string{
		`<stringProp name="variableNames">user,pass</stringProp>`,
		`<stringProp name="delimiter">;</stringProp>`,
		`<stringProp name="RegexExtractor.match_number">-1</stringProp>`,
		`<stringProp name="RegexExtractor.template">$2$$1$</stringProp>`,
		`<stringProp name="RegexExtractor.useHeaders">URL</stringProp>`,
		`<stringProp name="RegexExtractor.match_number">0</stringProp>`,
		`<stringProp name="BoundaryExtractor.useHeaders">message</stringProp>`,
		`<boolProp name="JSONPostProcessor.compute_concat">true</boolProp>`,
		`<stringProp name="Assertion.custom_message">m1</stringProp>`,
		`<boolProp name="Assertion.assume_success">true</boolProp>`,
		`<intProp name="Assertion.test_type">20</intProp>`, // substring + NOT
		`<intProp name="Assertion.test_type">8</intProp>`,
		`<intProp name="Assertion.test_type">12</intProp>`, // status NOT: equals + NOT, AND
		`<stringProp name="Assertion.test_field">Assertion.response_message</stringProp>`,
		`<boolProp name="INVERT">true</boolProp>`, `<boolProp name="ISREGEX">true</boolProp>`,
		`<intProp name="SizeAssertion.operator">5</intProp>`, // NOT < -> >=
		`<stringProp name="SizeAssertion.size">10</stringProp>`,
		`<stringProp name="MD5HexAssertion.size">abc</stringProp>`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("export lacks %s", want)
		}
	}
	if strings.Count(x, `"Assertion.assume_success">true`) != 1 {
		t.Error("ignore status should be written once")
	}
	unsupported := 0
	for _, it := range rep.Items {
		if it.Status == model.Unsupported {
			unsupported++
		}
	}
	if unsupported != 1 { // sha256 hash
		t.Errorf("unsupported items = %d: %+v", unsupported, rep.Items)
	}
	// Round trip: re-import maps the elements back.
	back, _, err := Import(bytes.NewReader(out), "x.jmx")
	if err != nil {
		t.Fatal(err)
	}
	if len(back.DataSources) != 1 || !reflect.DeepEqual(back.DataSources[0].Columns, []string{"user", "pass"}) || back.DataSources[0].Delimiter != ";" {
		t.Errorf("CSV round trip: %+v", back.DataSources)
	}
	br := findStep(back.Steps, "a").Request
	if !br.IgnoreStatus {
		t.Error("ignoreStatus lost on round trip")
	}
	gotEx := map[string]model.Extractor{}
	for _, e := range br.Extract {
		gotEx[e.Name] = e
	}
	for _, e := range req.Extract {
		g := gotEx[e.Name]
		norm := func(m model.MatchIndex) model.MatchIndex { // 0 and 1 both mean "first"
			if m == 0 {
				return 1
			}
			return m
		}
		if norm(g.Match) != norm(e.Match) || g.Template != e.Template || g.Source != e.Source {
			t.Errorf("extractor %s round trip: %+v -> %+v", e.Name, e, g)
		}
	}
	// sha256 is not exported; "NOT status 500,503" comes back as two NOT equals.
	if len(br.Assert) != len(req.Assert) {
		t.Errorf("assertions round trip: %+v", br.Assert)
	}
}

func TestExportNoAssertionIgnoreStatus(t *testing.T) {
	sc := model.NewScenario("x")
	sc.Defaults.BaseURL = "http://h.example"
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "a", Request: &model.Request{Method: "GET", URL: "/a",
		RequestCheckOptions: model.RequestCheckOptions{IgnoreStatus: true}}}}
	out, _, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"Assertion.assume_success">true`) {
		t.Error("pass-through Ignore Status assertion missing")
	}
	back, _, err := Import(bytes.NewReader(out), "x.jmx")
	if err != nil {
		t.Fatal(err)
	}
	if r := findStep(back.Steps, "a").Request; !r.IgnoreStatus {
		t.Errorf("ignoreStatus lost: %+v", r)
	}
}

func TestFunctionMappingParity(t *testing.T) {
	cases := []struct{ in, want, status string }{
		{"${__V(id_${n})}", "${__v(id_${n})}", ""},
		{"${__split(${list},parts,|)}", "${__split(${list},parts,|)}", ""},
		{"${__digest(SHA-256,${pw},${salt},true,h)}", "${__digest(SHA-256,${pw},${salt},true,h)}", ""},
		{"${__timeShift(yyyy-MM-dd,,P1D,,d)}", "${__timeShift(yyyy-MM-dd,,P1D,,d)}", ""},
		{"${__timeShift(dd MMMM,,P1D,fr_FR)}", "${__timeShift(dd MMMM,,P1D,fr_FR)}", model.Partial},
		{"${__RandomDate(,,2050-01-01,,)}", "${__randomDate(,,2050-01-01,,)}", ""},
		{"${__RandomFromMultipleVars(a|b,r)}", "${__randomFromVars(a|b,r)}", ""},
		{"${__escapeOroRegexpChars(a.b,)}", "${__escapeRegex(a.b,)}", model.Partial},
		{"${__changeCase(${__UUID()},LOWER)}", "${__changeCase(${__uuid()},LOWER)}", ""},
		{"${__time(/1000,)}", "${__timestamp(/1000)}", ""},
		{"${__time(YMD,d)}", "${__timestamp(YMD,d)}", ""},
		{"${__counter(TRUE,)}", "${__counter(TRUE)}", ""},
		{`${__base64Encode(a\,b)}`, `${__base64Encode(a\,b)}`, ""},
	}
	for _, c := range cases {
		got, issues := importFuncs(c.in, map[string]string{})
		worst := ""
		for _, is := range issues {
			if rank[is.Status] > rank[worst] {
				worst = is.Status
			}
		}
		if got != c.want || worst != c.status {
			t.Errorf("importFuncs(%q) = %q (%q), want %q (%q) %v", c.in, got, worst, c.want, c.status, issues)
		}
	}
	for _, s := range []string{"${__v(x)}", "${__split(a,b)}", "${__intSum(1,2,s)}", "${__timeShift(,,PT1H)}",
		"${__randomDate(yyyy-MM-dd,,2050-01-01)}", "${__timestamp(/1000)}", "${__counter(TRUE,c)}", "${__randomInt(1,2,r)}",
		"${__randomFromVars(a|b)}", "${__dateTimeConvert(1,,yyyy)}"} {
		j, issues := exportFuncs(s)
		if len(issues) != 0 {
			t.Errorf("exportFuncs(%q) issues %v", s, issues)
		}
		back, iss := importFuncs(j, map[string]string{})
		if strings.ReplaceAll(back, ",)", ")") != s || len(iss) != 0 {
			t.Errorf("round trip %q -> %q -> %q (%v)", s, j, back, iss)
		}
	}
	if _, issues := exportFuncs("${__randomFromList(a,b)}"); len(issues) != 1 {
		t.Error("__randomFromList has no JMeter equivalent and should be reported")
	}
	if _, issues := exportFuncs("${__base64Encode(a)}"); len(issues) != 1 || issues[0].Status != model.Partial {
		t.Errorf("__base64Encode export should note jmeter-plugins: %v", issues)
	}
}

func TestImportLegacyCSVVariableNames(t *testing.T) {
	csv := func(names string) string {
		return el("CSVDataSet", "users", sp("filename", "u.csv")+sp("delimiter", ";")+sp("variableNames", names))
	}
	for names, want := range map[string]string{"a,b": "", "a;b": model.Partial} {
		sc, rep := importString(t, plan("P", tg(loops("1"), csv(names)+sampler("s", "", "")), ""))
		if len(sc.DataSources) != 1 || !reflect.DeepEqual(sc.DataSources[0].Columns, []string{"a", "b"}) {
			t.Errorf("%s: columns %+v", names, sc.DataSources)
		}
		it := findItem(rep, "TestPlan/TG/users")
		if it == nil || (want == model.Partial) != (it.Status == model.Partial) {
			t.Errorf("%s: item %+v", names, it)
		}
	}
}
