package extract

import (
	"reflect"
	"strings"
	"testing"
)

const jdoc = `{"a":{"b":"x","c":[10,20,30],"d":{"b":"deep"}},"items":[{"id":1,"n":"one"},{"id":2,"n":"two"},{"id":9007199254740993,"n":"big"}],"k.dot":"dv","t":true,"nil":null}`

func TestJSONPath(t *testing.T) {
	doc, err := ParseJSON([]byte(jdoc))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path string
		want []string
	}{
		{"$.a.b", []string{"x"}},
		{"$.a.c[0]", []string{"10"}},
		{"$.a.c[2]", []string{"30"}},
		{"$.a.c[-1]", []string{"30"}},
		{"$.a.c[-3]", []string{"10"}},
		{"$.a.c[3]", nil},
		{"$.a.c[-4]", nil},
		{"$.a.c[*]", []string{"10", "20", "30"}},
		{"$.items[*].id", []string{"1", "2", "9007199254740993"}},
		{"$.items[1].n", []string{"two"}},
		{"$..b", []string{"x", "deep"}},
		{"$..n", []string{"one", "two", "big"}},
		{"$['a']['b']", []string{"x"}},
		{`$["a"].b`, []string{"x"}},
		{"$['k.dot']", []string{"dv"}},
		{"$.t", []string{"true"}},
		{"$.nil", []string{""}},
		{"$.missing", nil},
		{"$.a.b.c", nil},
		{"$.a.*", []string{"x", "[10,20,30]", `{"b":"deep"}`}},
		{"  $.a.b  ", []string{"x"}},
		{"$.a[0]", nil},  // index on object
		{"$.a.c.b", nil}, // key on array
		{"$..*", nil},    // placeholder replaced below
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if tc.path == "$..*" {
				got, err := JSONPath(doc, tc.path)
				if err != nil || len(got) < 5 {
					t.Fatalf("recursive wildcard got %v err %v", got, err)
				}
				return
			}
			got, err := JSONPath(doc, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			var strs []string
			for _, g := range got {
				strs = append(strs, Stringify(g))
			}
			if !reflect.DeepEqual(strs, tc.want) {
				t.Errorf("got %q want %q", strs, tc.want)
			}
		})
	}
}

func TestJSONPathErrors(t *testing.T) {
	doc, _ := ParseJSON([]byte(jdoc))
	for _, p := range []string{
		"a.b", "", "$.items[?(@.id==1)]", "$.items[?(@.id>1)].n", "$.a.c[1:2]", "$.a[", "$.", "$..", "$a", "$.a.c[x]",
	} {
		t.Run(p, func(t *testing.T) {
			if _, err := JSONPath(doc, p); err == nil {
				t.Errorf("expected error for %q", p)
			}
		})
	}
	_, err := JSONPath(doc, "$.items[?(@.id==1)]")
	if err == nil || !strings.Contains(err.Error(), "filter") {
		t.Errorf("filter error should mention filters: %v", err)
	}
}

func TestParseJSONKeepsBigNumbers(t *testing.T) {
	v, err := ParseJSON([]byte(`{"id":12345678901234567890}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := JSONPath(v, "$.id")
	if Stringify(got[0]) != "12345678901234567890" {
		t.Errorf("got %v", got)
	}
	if _, err := ParseJSON([]byte(`{bad`)); err == nil {
		t.Error("expected error")
	}
}

func TestStringify(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""}, {"s", "s"}, {true, "true"}, {1.5, "1.5"}, {float64(3), "3"},
		{map[string]any{"a": 1}, `{"a":1}`}, {[]any{1, "x"}, `[1,"x"]`},
	}
	for _, tc := range tests {
		if got := Stringify(tc.in); got != tc.want {
			t.Errorf("Stringify(%v)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestLeaves(t *testing.T) {
	doc, _ := ParseJSON([]byte(`{"user":{"id":7,"tags":["a","b"]},"my key":"v","9x":"n","ok":true,"z":null,"e":[]}`))
	got := Leaves(doc)
	m := map[string]string{}
	for _, l := range got {
		m[l.Path] = l.Value
	}
	want := map[string]string{
		"$.user.id": "7", "$.user.tags[0]": "a", "$.user.tags[1]": "b",
		"$['my key']": "v", "$['9x']": "n", "$.ok": "true", "$.z": "",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("leaf %s=%q want %q (all=%v)", k, m[k], v, m)
		}
	}
	// every leaf path must round-trip through JSONPath
	for _, l := range got {
		res, err := JSONPath(doc, l.Path)
		if err != nil || len(res) != 1 || Stringify(res[0]) != l.Value {
			t.Errorf("leaf %s did not round-trip: %v %v", l.Path, res, err)
		}
	}
	// deterministic order
	if !reflect.DeepEqual(got, Leaves(doc)) {
		t.Error("Leaves not deterministic")
	}
	if l := Leaves("scalar"); len(l) != 1 || l[0].Path != "$" || l[0].Value != "scalar" {
		t.Errorf("scalar root: %+v", l)
	}
}

func TestRegex(t *testing.T) {
	body := `id=12 name="bob" id=34 id=56`
	tests := []struct {
		expr  string
		group int
		want  []string
		err   bool
	}{
		{`id=(\d+)`, 0, []string{"12", "34", "56"}, false}, // default group 1
		{`id=(\d+)`, 1, []string{"12", "34", "56"}, false},
		{`id=(\d+)`, 2, nil, true},
		{`id=\d+`, 0, []string{"id=12", "id=34", "id=56"}, false}, // no groups -> whole
		{`(id)=(\d+)`, 2, []string{"12", "34", "56"}, false},
		{`name="(\w+)"`, 0, []string{"bob"}, false},
		{`nomatch(\d)`, 0, nil, false},
		{`(`, 0, nil, true},
	}
	for _, tc := range tests {
		got, err := Regex(body, tc.expr, tc.group)
		if (err != nil) != tc.err || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q/%d got %v err %v want %v err=%v", tc.expr, tc.group, got, err, tc.want, tc.err)
		}
	}
	if ok, err := Match(body, `name="b`); !ok || err != nil {
		t.Error("Match failed")
	}
	if ok, _ := Match(body, `zzz`); ok {
		t.Error("Match false positive")
	}
	if _, err := Match(body, `[`); err == nil {
		t.Error("Match bad regex should error")
	}
}

func TestBoundary(t *testing.T) {
	tests := []struct {
		body, l, r string
		want       []string
	}{
		{`<a href="x1"><a href="x2">`, `href="`, `"`, []string{"x1", "x2"}},
		{`token=abc;`, "token=", ";", []string{"abc"}},
		{`token=abc`, "token=", ";", nil},
		{`token=abc`, "token=", "", []string{"abc"}},
		{`nothing`, "LB", "RB", nil},
		{`LBaRBLBbRB`, "LB", "RB", []string{"a", "b"}},
		{`LBRB`, "LB", "RB", []string{""}},
	}
	for _, tc := range tests {
		if got := Boundary(tc.body, tc.l, tc.r); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %q want %q", tc.body, got, tc.want)
		}
	}
}

const xdoc = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" xmlns:t="urn:t">
 <s:Body>
  <t:Resp id="r1">
    <t:Item k="a">first</t:Item>
    <t:Item k="b"> second </t:Item>
    <t:Group><t:Item k="c">third</t:Item></t:Group>
  </t:Resp>
 </s:Body>
</s:Envelope>`

func TestXPath(t *testing.T) {
	root, err := ParseXML([]byte(xdoc))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		expr string
		want []string
	}{
		{"/Envelope/Body/Resp/Item", []string{"first", "second"}},
		{"//Item", []string{"first", "second", "third"}},
		{"/Envelope/Body/Resp/Item[2]", []string{"second"}},
		{"/Envelope/Body/Resp/Item[3]", nil},
		// NOTE: full XPath would return "first" and "third" (first Item of each parent);
		// this subset applies the predicate to the whole descendant set.
		{"//Item[1]", []string{"first"}},
		{"//Item[3]", []string{"third"}},
		{"/Envelope/Body/Resp/@id", []string{"r1"}},
		{"/Envelope/Body/Resp/Item/@k", []string{"a", "b"}},
		{"//Item[2]/@k", []string{"b"}},
		{"/Envelope/Body/Resp/Item/text()", []string{"first", "second"}},
		{"//Item/text()", []string{"first", "second", "third"}},
		{"/s:Envelope/s:Body/t:Resp/t:Item[1]", []string{"first"}},
		{"//t:Item", []string{"first", "second", "third"}},
		{"/Envelope/*/Resp/@id", []string{"r1"}},
		{"//Group/Item", []string{"third"}},
		{"/Envelope/Nope", nil},
		{"/Envelope/Body/Resp/@missing", nil},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := XPath(root, tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "//Item[x]", "//Item[0]", "//Item[@k='a']"} {
		if _, err := XPath(root, bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestParseXMLRejectsDoctype(t *testing.T) {
	bad := []string{
		`<?xml version="1.0"?><!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><foo>&xxe;</foo>`,
		`<!DOCTYPE lolz [<!ENTITY lol "lol">]><lolz>&lol;</lolz>`,
		`<!DOCTYPE html><html/>`,
		`<a><b></a>`,
		`<a>`,
	}
	for i, b := range bad {
		if _, err := ParseXML([]byte(b)); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if _, err := ParseXML([]byte(`<a><!-- c --><b>1</b></a>`)); err != nil {
		t.Errorf("comments should be fine: %v", err)
	}
}

func TestPick(t *testing.T) {
	m := []string{"a", "b", "c"}
	tests := []struct {
		m    []string
		n    int
		want string
		ok   bool
	}{
		{m, 0, "a", true}, {m, 1, "a", true}, {m, 3, "c", true}, {m, 4, "", false},
		{m, -1, "c", true}, {nil, 0, "", false}, {nil, -1, "", false},
	}
	for _, tc := range tests {
		got, ok := Pick(tc.m, tc.n, func(n int) int { return n - 1 })
		if got != tc.want || ok != tc.ok {
			t.Errorf("Pick(%v,%d)=%q,%v want %q,%v", tc.m, tc.n, got, ok, tc.want, tc.ok)
		}
	}
}
