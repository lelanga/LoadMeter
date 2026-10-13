package vars

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRecursiveExpansion(t *testing.T) {
	c := NewContext(1, 1)
	c.Vars["host"] = "example.com"
	c.Vars["base"] = "https://${host}/api"
	c.Vars["users"] = "${base}/users"
	got, res := c.Expand("${users}?id=1")
	if got != "https://example.com/api/users?id=1" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(strings.Join(res.Used, ","), "host") {
		t.Errorf("nested use not reported: %v", res.Used)
	}
	// Unresolved inner references are reported and kept.
	c.Vars["bad"] = "x-${nope}"
	got, res = c.Expand("${bad}")
	if got != "x-${nope}" || len(res.Unresolved) != 1 || res.Unresolved[0] != "nope" {
		t.Errorf("got %q %v", got, res.Unresolved)
	}
	// Cycles stop at MaxDepth instead of looping forever.
	c.Vars["a"] = "${b}"
	c.Vars["b"] = "${a}"
	if got, _ := c.Expand("${a}"); !strings.Contains(got, "${") {
		t.Errorf("cycle expanded to %q", got)
	}
	// Functions inside variable values run on every read.
	c.Vars["vu"] = "vu-${__vu()}"
	if got, _ := c.Expand("${vu}"); got != "vu-1" {
		t.Errorf("got %q", got)
	}
	// Set clears the raw flag again.
	c.SetRaw("r", "${host}")
	c.Set("r", "${host}")
	if got, _ := c.Expand("${r}"); got != "example.com" {
		t.Errorf("Set did not clear raw: %q", got)
	}
}

func TestArgEscaping(t *testing.T) {
	c := NewContext(1, 1)
	c.Vars["x"] = "X"
	tests := []struct{ in, want string }{
		{`${__changeCase(a\,b,LOWER)}`, "a,b"},
		{`${__changeCase(a\\b,UPPER)}`, `A\B`},
		{`${__changeCase(\${x},LOWER)}`, "${x}"},
		{`${__changeCase(${x},LOWER)}`, "x"},
		{`${__changeCase(f(a,b),UPPER)}`, "F(A,B)"},                // commas inside parentheses are not separators
		{`${__changeCase(f\(a,UPPER)}`, "F(A"},                     // escaped parenthesis
		{`${__escapeRegex(\d+)}`, `\\d\+`},                         // other backslashes are kept
		{`${__randomFromList(a\,b)}`, "a,b"},                       // one argument
		{`${__urlencode(a,b)}`, "a%2Cb"},                           // historical: joined back
		{`${__changeCase(${__changeCase(Ab,UPPER)},LOWER)}`, "ab"}, // nested call with comma
	}
	for _, tc := range tests {
		if got, res := c.Expand(tc.in); got != tc.want {
			t.Errorf("%s -> %q want %q (unresolved %v)", tc.in, got, tc.want, res.Unresolved)
		}
	}
}

func TestNewFunctions(t *testing.T) {
	c := NewContext(2, 1)
	c.Vars["n"] = "3"
	c.Vars["id_3"] = "abc"
	c.Vars["list"] = "a|b"
	tests := []struct{ in, want string }{
		{"${__v(id_${n})}", "abc"},
		{"${__V(id_${n})}", "abc"},
		{"${__v(missing,dflt)}", "dflt"},
		{"${__isVarDefined(n)}", "true"},
		{"${__isVarDefined(zz)}", "false"},
		{"${__intSum(1,2,3)}", "6"},
		{"${__intSum(2147483647,1)}", "-2147483648"},
		{"${__longSum(2147483647,1)}", "2147483648"},
		{"${__intSum(1,2,total)}", "3"},
		{"${__changeCase(hello)}", "HELLO"},
		{"${__changeCase(hello,CAPITALIZE)}", "Hello"},
		{"${__digest(MD5,abc)}", "900150983cd24fb0d6963f7d28e17f72"},
		{"${__digest(SHA-256,abc)}", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"${__digest(SHA-1,ab,c,true)}", "A9993E364706816ABA3E25717850C26C9CD0D89D"},
		{"${__base64Encode(user:pass)}", "dXNlcjpwYXNz"},
		{"${__base64Decode(dXNlcjpwYXNz)}", "user:pass"},
		{"${__escapeHtml(<a href=\"x\">&</a>)}", "&lt;a href=&#34;x&#34;&gt;&amp;&lt;/a&gt;"},
		{"${__unescapeHtml(&lt;b&gt; &amp; &quot;)}", `<b> & "`},
		{"${__escapeXml(<a b='c'>&)}", "&lt;a b=&apos;c&apos;&gt;&amp;"},
		{"${__urldecode(a%20b%2Bc+d)}", "a b+c d"},
		{`${__unescape(a\tb\u0041)}`, "a\tbA"},
		{"${__char(72,0x69)}", "Hi"},
		{"${__escapeRegex(a.b*)}", `a\.b\*`},
		{"${__dateTimeConvert(2024-02-29,yyyy-MM-dd,dd/MM/yy)}", "29/02/24"},
		{"${__dateTimeConvert(15/03/2024 14:05,dd/MM/yyyy HH:mm,EEE d MMM yyyy h:mm a)}", "Fri 15 Mar 2024 2:05 PM"},
		{"${__timeShift(yyyy-MM-dd,2024-02-28,P2D)}", "2024-03-01"},
		{"${__timeShift(yyyy-MM-dd HH:mm,2024-01-01 10:00,-PT90M)}", "2024-01-01 08:30"},
		{"${__timeShift(yyyy-MM-dd HH:mm,2024-01-01 10:00,PT-1H30M)}", "2024-01-01 09:30"},
		{"${__randomDate(yyyy-MM-dd,2024-01-01,2024-01-02)}", "2024-01-01"},
		{"${__randomFromVars(list)}", "a|b"},
	}
	for _, tc := range tests {
		if got, res := c.Expand(tc.in); got != tc.want {
			t.Errorf("%s -> %q want %q (unresolved %v)", tc.in, got, tc.want, res.Unresolved)
		}
	}
	if c.Vars["total"] != "3" {
		t.Errorf("intSum result variable = %q", c.Vars["total"])
	}
	for _, bad := range []string{"${__digest(NOPE,a)}", "${__base64Decode(!!!)}", "${__intSum(1,x,y)}",
		"${__randomDate(yyyy-MM-dd,2024-01-02,2024-01-01)}", "${__char(xyz)}", "${__changeCase(a,SIDEWAYS)}",
		"${__timeShift(,,P1Y)}", "${__split()}", "${__v()}", "${__randomFromVars(none)}"} {
		if _, res := c.Expand(bad); len(res.Unresolved) == 0 {
			t.Errorf("%s should be unresolved", bad)
		}
	}
}

func TestSplit(t *testing.T) {
	c := NewContext(1, 1)
	c.Vars["parts_5"] = "stale"
	got, _ := c.Expand("${__split(a,,b,,parts)}") // unescaped commas: text "a", var "", ... -> error
	if !strings.HasPrefix(got, "${") {
		t.Fatalf("split with unescaped commas should not resolve: %q", got)
	}
	got, _ = c.Expand(`${__split(a\,\,b\,,parts)}`)
	if got != "a,,b," {
		t.Fatalf("returned %q", got)
	}
	want := map[string]string{"parts": "a,,b,", "parts_n": "4", "parts_1": "a", "parts_2": "?", "parts_3": "b", "parts_4": "?"}
	for k, v := range want {
		if c.Vars[k] != v {
			t.Errorf("%s = %q want %q", k, c.Vars[k], v)
		}
	}
	if _, ok := c.Vars["parts_5"]; ok {
		t.Error("stale parts_5 kept")
	}
	_, _ = c.Expand("${__split(x-y|z,p,-|)}")
	if c.Vars["p_n"] != "3" || c.Vars["p_3"] != "z" {
		t.Errorf("char-set delimiters: %v", c.Vars)
	}
	// Split values are raw: never re-expanded.
	_, _ = c.Expand(`${__split(\${n},q)}`)
	c.Vars["n"] = "1"
	if got, _ := c.Expand("${q_1}"); got != "${n}" {
		t.Errorf("split value re-expanded: %q", got)
	}
}

func TestCounterPerVU(t *testing.T) {
	a, b := NewContext(1, 1), NewContext(2, 1)
	b.Counter = a.Counter
	a.Expand("${__counter()}") // initialises a shared counter lazily on a
	b.Counter = a.Counter
	for i := 1; i <= 3; i++ {
		got, _ := a.Expand("${__counter(TRUE)}")
		if got != strconv.Itoa(i) {
			t.Fatalf("per-VU counter %q at %d", got, i)
		}
	}
	if got, _ := b.Expand("${__counter(TRUE,c)}"); got != "1" || b.Vars["c"] != "1" {
		t.Errorf("second VU per-VU counter %q", got)
	}
	if got, _ := b.Expand("${__counter(FALSE)}"); got != "2" {
		t.Errorf("shared counter %q", got)
	}
}

func TestTimestampDivisorAndNames(t *testing.T) {
	c := NewContext(1, 1)
	s, _ := c.Expand("${__timestamp(/1000)}")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1_600_000_000 || n > 100_000_000_000 {
		t.Errorf("seconds %q", s)
	}
	s, _ = c.Expand("${__timestamp(YMD)}")
	if !regexp.MustCompile(`^\d{8}$`).MatchString(s) {
		t.Errorf("YMD %q", s)
	}
	s, _ = c.Expand("${__timestamp(yyyy-MM-dd'T'HH:mm:ss.SSSXXX,ts)}")
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}(Z|[+-]\d\d:\d\d)$`).MatchString(s) || c.Vars["ts"] != s {
		t.Errorf("ISO %q", s)
	}
	if _, res := c.Expand("${__timestamp(/0)}"); len(res.Unresolved) == 0 {
		t.Error("divisor 0 accepted")
	}
}

func TestJavaDateFormat(t *testing.T) {
	tm := time.Date(2024, 3, 5, 0, 7, 9, 45_000_000, time.FixedZone("CET", 3600))
	tests := []struct{ pat, want string }{
		{"yyyy-MM-dd HH:mm:ss.SSS", "2024-03-05 00:07:09.045"},
		{"yy/M/d H:m:s", "24/3/5 0:7:9"},
		{"MMM MMMM", "Mar March"},
		{"EEE EEEE u", "Tue Tuesday 2"},
		{"h a k K", "12 AM 24 0"},
		{"D DDD", "65 065"},
		{"Z X XX XXX", "+0100 +01 +0100 +01:00"},
		{"z", "CET"},
		{"'T' 'o''clock' ''", "T o'clock '"},
		{"G yyyy", "AD 2024"},
		{"yyyyMMdd-HHmmss", "20240305-000709"},
	}
	for _, tc := range tests {
		got, err := FormatJavaDate(tm, tc.pat)
		if err != nil || got != tc.want {
			t.Errorf("%q -> %q (%v) want %q", tc.pat, got, err, tc.want)
		}
	}
	for _, bad := range []string{"yyyy-MM-dd'T", "qq", "yyyy-bb"} {
		if _, err := FormatJavaDate(tm, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	utc := time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)
	if s, _ := FormatJavaDate(utc, "X XXX"); s != "Z Z" {
		t.Errorf("UTC X = %q", s)
	}
}

func TestJavaToGoLayout(t *testing.T) {
	tests := []struct{ in, want string }{
		{"yyyy-MM-dd", "2006-01-02"},
		{"dd/MM/yy HH:mm:ss.SSS", "02/01/06 15:04:05.000"},
		{"d MMM yyyy h:mm a", "2 Jan 2006 3:04 PM"},
		{"EEEE, MMMM d", "Monday, January 2"},
		{"yyyy-MM-dd'T'HH:mm:ssXXX", "2006-01-02T15:04:05Z07:00"},
		{"Z z", "-0700 MST"},
		{"DDD", "002"},
	}
	for _, tc := range tests {
		got, err := JavaToGoLayout(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%q -> %q (%v) want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"SSS", "ww", "yyyy'1'", "kk"} {
		if _, err := JavaToGoLayout(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseISODuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"PT1H", time.Hour}, {"P1D", 24 * time.Hour}, {"P1DT2H3M4S", 26*time.Hour + 3*time.Minute + 4*time.Second},
		{"-PT30M", -30 * time.Minute}, {"PT-6H3M", -6*time.Hour + 3*time.Minute}, {"PT0.5S", 500 * time.Millisecond},
		{"pt2m", 2 * time.Minute},
	}
	for _, tc := range tests {
		if got, err := ParseISODuration(tc.in); err != nil || got != tc.want {
			t.Errorf("%s -> %v %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"", "P", "PT", "1H", "P1H", "PT1D", "P1Y", "PTxS", "PT1H1H T"} {
		if _, err := ParseISODuration(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
