package jmx

// Extractor and assertion mapping between JMX and the model (G02, G07):
// match numbers, regex templates, extraction sources, NOT / custom message /
// ignore status / equals / size / MD5 / JSON regex and null assertions.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// ---------------------------------------------------------------------------
// Match numbers. JMeter: 0 = random, -1 = all, N = nth.
// Load Meter: 0 = first, -1 = random, "all" (MatchAll), N = nth.

// matchNo maps a JMeter match number to the model.
func matchNo(s string) (model.MatchIndex, string, string) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return model.MatchRandom, model.Supported, ""
	}
	v, err := strconv.Atoi(s)
	switch {
	case err != nil:
		return model.MatchFirst, model.Partial, fmt.Sprintf("match number %q is not a number; first match used", s)
	case v < 0:
		return model.MatchAll, model.Supported, "all matches -> name_1..name_N, name_matchNr (and name_ALL)"
	}
	return model.MatchIndex(v), model.Supported, ""
}

// jmMatch maps a model match index to a JMeter match number.
func jmMatch(m model.MatchIndex) string {
	switch {
	case m == model.MatchFirst:
		return "1"
	case m == model.MatchAll:
		return "-1"
	case m < 0:
		return "0"
	}
	return strconv.Itoa(int(m))
}

// ---------------------------------------------------------------------------
// Sources. JMeter "useHeaders" (regex/boundary extractors) values.

var jmxExtractSource = map[string]string{
	"": "", "false": "", "true": model.SrcHeaders, "request_headers": model.SrcRequestHeaders,
	"URL": model.SrcURL, "code": model.SrcStatus, "message": model.SrcMessage,
}

func exportExtractSource(src string) string {
	switch src {
	case model.SrcHeaders:
		return "true"
	case model.SrcRequestHeaders:
		return "request_headers"
	case model.SrcURL:
		return "URL"
	case model.SrcStatus:
		return "code"
	case model.SrcMessage:
		return "message"
	}
	return "false"
}

// Response Assertion test fields.
var jmxAssertField = map[string]string{
	"Assertion.response_data": "", "Assertion.response_headers": model.SrcHeaders,
	"Assertion.request_headers": model.SrcRequestHeaders, "Assertion.sample_label": model.SrcURL,
	"Assertion.response_code": model.SrcStatus, "Assertion.response_message": model.SrcMessage,
}

func exportAssertField(src string) string {
	for f, s := range jmxAssertField {
		if s == src && src != "" {
			return f
		}
	}
	return "Assertion.response_data"
}

const headerRenderNote = "Load Meter renders headers as sorted \"Name: value\" lines with canonical name casing (response: status line first); JMeter matches the raw header block"

// ---------------------------------------------------------------------------
// Import: extractors

func (im *importer) regexExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	refname := strings.TrimSpace(n.str("RegexExtractor.refname"))
	if refname == "" {
		return nil, model.Unsupported, "no reference name"
	}
	re := im.conv(n.str("RegexExtractor.regex"), loc)
	ex := model.Extractor{Name: refname, Type: model.ExRegex, Expr: re, Default: im.conv(n.str("RegexExtractor.default"), loc)}
	status, note := model.Supported, ""
	esc := func(s, nt string) {
		if rank[s] > rank[status] {
			status = s
		}
		note = joinNote(note, nt)
	}
	m, s, nt := matchNo(n.str("RegexExtractor.match_number"))
	ex.Match = m
	esc(s, nt)
	tpl := strings.TrimSpace(n.str("RegexExtractor.template"))
	groups := tplGroupRE.FindAllStringSubmatch(tpl, -1)
	switch {
	case len(groups) == 1 && groups[0][0] == tpl && groups[0][1] != "0":
		ex.Group, _ = strconv.Atoi(groups[0][1])
	case tpl == "":
		esc(model.Partial, "empty template; whole match used")
	default:
		ex.Template = im.conv(tpl, loc) // $0$, literal text or several groups
	}
	switch field := strings.TrimSpace(n.str("RegexExtractor.useHeaders")); field {
	case "", "false":
	case "unescaped":
		esc(model.Partial, "JMeter HTML-unescapes the body before matching; Load Meter matches the raw body")
	case "true":
		if ex.Template == "" {
			switch {
			case strings.HasPrefix(re, cookiePrefix) && strings.HasSuffix(re, cookieSuffix):
				if name, ok := unquoteName(strings.TrimSuffix(strings.TrimPrefix(re, cookiePrefix), cookieSuffix)); ok {
					ex.Type, ex.Expr, ex.Group = model.ExCookie, name, 0
					return []model.Extractor{ex}, status, joinNote(note, "Set-Cookie header regex mapped to a cookie extractor")
				}
			case strings.HasPrefix(re, hdrPrefix) && strings.HasSuffix(re, hdrSuffix):
				if name, ok := unquoteName(strings.TrimSuffix(strings.TrimPrefix(re, hdrPrefix), hdrSuffix)); ok {
					ex.Type, ex.Expr, ex.Group = model.ExHeader, name, 0
					return []model.Extractor{ex}, status, note
				}
			}
		}
		ex.Source = model.SrcHeaders
		esc(model.Partial, headerRenderNote)
	case "request_headers":
		ex.Source = model.SrcRequestHeaders
		esc(model.Partial, headerRenderNote)
	case "code":
		if ex.Template == "" && ex.Match != model.MatchAll {
			switch re {
			case "(.*)", "(.+)", `(\d+)`, `(\d{3})`, `(\d\d\d)`:
				ex.Type, ex.Expr, ex.Group = model.ExStatus, "", 0
				return []model.Extractor{ex}, status, note
			}
		}
		ex.Source = model.SrcStatus
	case "URL", "message":
		ex.Source = jmxExtractSource[field]
	default:
		return nil, model.Unsupported, joinNote(note, fmt.Sprintf("regex on %q is not supported", field))
	}
	if err := re2Error(re); err != nil {
		return nil, model.Unsupported, joinNote(note, "regex is not RE2-compatible (Go regexp has no backreferences/lookaround): "+err.Error())
	}
	return []model.Extractor{ex}, status, note
}

func (im *importer) jsonExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	names := splitSemi(n.str("JSONPostProcessor.referenceNames"))
	exprs := splitSemi(n.str("JSONPostProcessor.jsonPathExprs"))
	mns := splitSemi(n.str("JSONPostProcessor.match_numbers"))
	defs := strings.Split(n.str("JSONPostProcessor.defaultValues"), ";")
	if len(names) == 0 {
		return nil, model.Unsupported, "no reference names"
	}
	if len(exprs) != len(names) {
		return nil, model.Unsupported, fmt.Sprintf("%d names but %d JSON paths (JMeter would fail too)", len(names), len(exprs))
	}
	status, note := model.Supported, ""
	var out []model.Extractor
	for i, nm := range names {
		mn := ""
		if i < len(mns) {
			mn = mns[i]
		} else if len(mns) == 1 {
			mn = mns[0]
		}
		m, s, nt := matchNo(mn)
		if rank[s] > rank[status] {
			status = s
		}
		note = joinNote(note, nt)
		def := ""
		if i < len(defs) {
			def = defs[i]
		}
		e := exprs[i]
		if strings.Contains(e, "?(") || strings.Contains(e, "()") {
			status = model.Partial
			note = joinNote(note, fmt.Sprintf("%q uses Jayway JSONPath filters/functions; verify Load Meter JSONPath supports them", e))
		}
		out = append(out, model.Extractor{Name: nm, Type: model.ExJSONPath, Expr: im.conv(e, loc), Match: m, Default: im.conv(def, loc)})
	}
	if len(names) > 1 {
		note = joinNote(note, fmt.Sprintf("split into %d extractors", len(names)))
	}
	return out, status, note
}

func (im *importer) boundaryExtractor(n *node, loc string) ([]model.Extractor, string, string) {
	refname := strings.TrimSpace(n.str("BoundaryExtractor.refname"))
	if refname == "" {
		return nil, model.Unsupported, "no reference name"
	}
	m, status, note := matchNo(n.str("BoundaryExtractor.match_number"))
	ex := model.Extractor{Name: refname, Type: model.ExBoundary, Left: im.conv(n.str("BoundaryExtractor.lboundary"), loc),
		Right: im.conv(n.str("BoundaryExtractor.rboundary"), loc), Match: m, Default: im.conv(n.str("BoundaryExtractor.default"), loc)}
	field := strings.TrimSpace(n.str("BoundaryExtractor.useHeaders"))
	src, ok := jmxExtractSource[field]
	switch {
	case field == "unescaped":
		status, note = model.Partial, joinNote(note, "JMeter HTML-unescapes the body first; Load Meter reads the raw body")
	case !ok:
		return nil, model.Unsupported, fmt.Sprintf("boundary extraction from %q is not supported", field)
	case src == model.SrcHeaders || src == model.SrcRequestHeaders:
		status, note = model.Partial, joinNote(note, headerRenderNote)
	}
	ex.Source = src
	if ex.Left == "" && ex.Right == "" {
		status = model.Partial
		note = joinNote(note, "both boundaries empty (JMeter returns the whole response)")
	}
	return []model.Extractor{ex}, status, note
}

// ---------------------------------------------------------------------------
// Import: assertions

// checkAssertion maps one JMeter assertion element. ignore reports JMeter's
// "Ignore Status" (applied to every sampler in the assertion's scope).
func (im *importer) checkAssertion(tc string, n *node, loc string) (as []model.Assertion, ignore bool, st, note string) {
	switch tc {
	case "ResponseAssertion":
		return im.responseAssertion(n, loc)
	case "JSONPathAssertion":
		as, st, note = im.jsonPathAssertion(n, loc)
	case "DurationAssertion":
		ms := msOpt(n.str("DurationAssertion.duration"))
		if ms <= 0 {
			return nil, false, model.Unsupported, fmt.Sprintf("duration %q is not a positive number of milliseconds", n.str("DurationAssertion.duration"))
		}
		as, st = []model.Assertion{{Type: model.AsMaxLatency, Value: time.Duration(ms * int64(time.Millisecond)).String()}}, model.Supported
	case "SizeAssertion":
		as, st, note = im.sizeAssertion(n, loc)
	case "MD5HexAssertion":
		hex := strings.ToLower(strings.TrimSpace(n.str("MD5HexAssertion.size")))
		if hex == "" {
			return nil, false, model.Unsupported, "no MD5 value"
		}
		as, st = []model.Assertion{{Type: model.AsHash, Expr: "md5", Value: im.conv(hex, loc)}}, model.Supported
		note = "MD5 of the decoded body (as kept, up to defaults.maxBodyBytes)"
	}
	return as, false, st, note
}

var sizeOps = map[string]string{"1": "==", "2": "!=", "3": ">", "4": "<", "5": ">=", "6": "<="}

func (im *importer) sizeAssertion(n *node, loc string) ([]model.Assertion, string, string) {
	size := strings.TrimSpace(n.str("SizeAssertion.size"))
	op, ok := sizeOps[strings.TrimSpace(orDefault(n.str("SizeAssertion.operator"), "1"))]
	if !ok {
		return nil, model.Unsupported, fmt.Sprintf("unknown size operator %q", n.str("SizeAssertion.operator"))
	}
	a := model.Assertion{Type: model.AsSize, Value: im.conv(size, loc), AssertionOptions: model.AssertionOptions{Op: op}}
	note := ""
	switch f := strings.TrimSpace(n.str("Assertion.test_field")); f {
	case "", "SizeAssertion.response_network_size":
		a.Expr = "full"
		note = "full response = bytes on the wire (headers + body as received)"
	case "SizeAssertion.response_data":
		a.Expr = "body"
		note = "body = decoded body size"
	default:
		return nil, model.Unsupported, fmt.Sprintf("size of %q is not supported (body or full response only)", strings.TrimPrefix(f, "SizeAssertion."))
	}
	if _, err := strconv.ParseInt(size, 10, 64); err != nil && !strings.Contains(a.Value, "${") {
		return nil, model.Unsupported, fmt.Sprintf("size %q is not a number", size)
	}
	return []model.Assertion{a}, model.Supported, note
}

func (im *importer) responseAssertion(n *node, loc string) ([]model.Assertion, bool, string, string) {
	field := strings.TrimSpace(n.str("Assertion.test_field"))
	if field == "" {
		field = "Assertion.response_data"
	}
	ignore := n.boolean("Assertion.assume_success", false)
	tt, err := strconv.Atoi(strings.TrimSpace(n.str("Assertion.test_type")))
	if err != nil {
		return nil, ignore, model.Unsupported, fmt.Sprintf("invalid test_type %q", n.str("Assertion.test_type"))
	}
	list, ok := n.coll("Asserion.test_strings") // sic: JMeter's property name
	if !ok {
		list, _ = n.coll("Assertion.test_strings")
	}
	var pats []string
	for _, c := range list {
		pats = append(pats, im.conv(c.Text(), loc))
	}
	if len(pats) == 0 {
		if ignore {
			// A pattern-less assertion used only for "Ignore Status".
			return nil, true, model.Supported, "\"Ignore Status\" mapped to request ignoreStatus"
		}
		return nil, false, model.Unsupported, "no patterns to test"
	}
	not, or := tt&4 != 0, tt&32 != 0
	mode := map[int]string{1: "matches", 2: "contains", 8: "equals", 16: "substring"}[tt&^36]
	if mode == "" {
		return nil, ignore, model.Unsupported, fmt.Sprintf("unsupported test_type %d", tt)
	}
	msg := im.conv(strings.TrimSpace(n.str("Assertion.custom_message")), loc)
	status, note := model.Supported, ""
	if ignore {
		note = "\"Ignore Status\" mapped to request ignoreStatus"
	}
	desc := mode
	if not {
		desc = "NOT " + desc
	}
	if or {
		desc += " (OR)"
	}
	unsup := func(why string) ([]model.Assertion, bool, string, string) {
		return nil, ignore, model.Unsupported, joinNote(note, fmt.Sprintf("%s on %s: %s", desc, strings.TrimPrefix(field, "Assertion."), why))
	}
	src, ok := jmxAssertField[field]
	if !ok {
		return unsup("this test field is not supported")
	}
	withMsg := func(as []model.Assertion) []model.Assertion {
		for i := range as {
			as[i].Message = msg
		}
		return as
	}
	// Status codes and classes keep the dedicated status assertion.
	if field == "Assertion.response_code" && !not {
		if as, st, nt, ok := statusAssertion(pats, mode, or); ok {
			return withMsg(as), ignore, worse(status, st), joinNote(note, nt)
		}
	}
	// Plain "Name: value" substring checks keep the header assertion.
	if field == "Assertion.response_headers" && !not && !(or && len(pats) > 1) && (mode == "substring" || (mode == "contains" && allLiteral(pats))) {
		var as []model.Assertion
		for _, p := range pats {
			i := strings.Index(p, ":")
			if i <= 0 {
				as = nil
				break
			}
			as = append(as, model.Assertion{Type: model.AsHeader, Expr: strings.TrimSpace(p[:i]), Value: strings.TrimSpace(p[i+1:])})
		}
		if as != nil {
			return withMsg(as), ignore, model.Partial, joinNote(note, "JMeter matches the raw header block (case-sensitive); mapped to a Load Meter header name/contains-value check")
		}
	}
	toRegex := func(p string) string {
		switch mode {
		case "substring":
			return quoteKeepVars(p)
		case "equals":
			return "^" + quoteKeepVars(p) + "$"
		case "matches":
			return "^(?:" + p + ")$"
		}
		return p
	}
	one := func(p string) (model.Assertion, error) {
		a := model.Assertion{AssertionOptions: model.AssertionOptions{Source: src, Not: not}}
		switch mode {
		case "substring":
			a.Type, a.Value = model.AsContains, p
			if not {
				a.Type, a.Not = model.AsNotContains, false
			}
		case "equals":
			a.Type, a.Value = model.AsEquals, p
		default:
			a.Type, a.Expr = model.AsRegex, toRegex(p)
			if mode == "contains" && not && isLiteralPattern(p) {
				a.Type, a.Expr, a.Value, a.Not = model.AsNotContains, "", p, false
			} else if err := re2Error(a.Expr); err != nil {
				return a, err
			}
		}
		return a, nil
	}
	var as []model.Assertion
	switch {
	case or && len(pats) > 1 && not:
		return unsup("NOT combined with OR (passes when any pattern is absent) has no Load Meter equivalent")
	case or && len(pats) > 1:
		alts := make([]string, len(pats))
		for i, p := range pats {
			alts[i] = toRegex(p)
		}
		re := "(?:" + strings.Join(alts, "|") + ")"
		if err := re2Error(re); err != nil {
			return unsup("pattern is not RE2-compatible: " + err.Error())
		}
		as = append(as, model.Assertion{Type: model.AsRegex, Expr: re, AssertionOptions: model.AssertionOptions{Source: src}})
	default:
		for _, p := range pats {
			a, err := one(p)
			if err != nil {
				return unsup("pattern is not RE2-compatible (Go regexp has no backreferences/lookaround): " + err.Error())
			}
			as = append(as, a)
		}
	}
	if mode == "contains" || mode == "matches" {
		note = joinNote(note, "JMeter uses Perl5 (ORO) regex, Load Meter uses Go RE2; syntax checked to compile")
	}
	if src == model.SrcHeaders || src == model.SrcRequestHeaders {
		status = model.Partial
		note = joinNote(note, headerRenderNote)
	}
	return withMsg(as), ignore, status, note
}

func worse(a, b string) string {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// statusAssertion maps response-code patterns that are plain codes or classes.
func statusAssertion(pats []string, mode string, or bool) ([]model.Assertion, string, string, bool) {
	var vals []string
	classes := 0
	for _, p := range pats {
		p = strings.TrimSpace(p)
		switch {
		case codeRE.MatchString(p):
			vals = append(vals, p)
		case (mode == "matches" || mode == "contains") && classRE.MatchString(p):
			vals = append(vals, classRE.FindStringSubmatch(p)[1]+"xx")
			classes++
		default:
			return nil, "", "", false
		}
	}
	if len(vals) > 1 && !or {
		for _, v := range vals[1:] {
			if v != vals[0] {
				return nil, "", "", false // AND of different codes: handled generically
			}
		}
		vals = vals[:1]
	}
	status, note := model.Supported, ""
	if classes > 0 && len(vals) > 1 {
		status, note = model.Partial, "list mixes status classes and codes; verify Load Meter accepts it"
	}
	return []model.Assertion{{Type: model.AsStatus, Value: strings.Join(vals, ",")}}, status, note, true
}

func (im *importer) jsonPathAssertion(n *node, loc string) ([]model.Assertion, string, string) {
	p := strings.TrimSpace(n.str("JSON_PATH"))
	if p == "" {
		return nil, model.Unsupported, "empty JSON path"
	}
	a := model.Assertion{Type: model.AsJSONPath, Expr: im.conv(p, loc)}
	a.Not = n.boolean("INVERT", false)
	status, note := model.Supported, ""
	if n.boolean("JSONVALIDATION", false) {
		exp := n.str("EXPECTED_VALUE")
		switch {
		case n.boolean("EXPECT_NULL", false):
			a.ExpectNull = true
		case n.boolean("ISREGEX", true) && (!isLiteralPattern(exp) || exp == ""):
			if err := re2Error(exp); err != nil {
				return nil, model.Unsupported, fmt.Sprintf("expected value %q is not an RE2 regular expression: %v", exp, err)
			}
			a.Regex, a.Value = true, im.conv(exp, loc)
			note = "JMeter uses Java regex, Load Meter uses Go RE2 (full match)"
		default:
			a.Value = im.conv(exp, loc)
		}
	}
	return []model.Assertion{a}, status, note
}

// ---------------------------------------------------------------------------
// Export: extractors

func (ex *exporter) extractor(e model.Extractor, loc string) {
	w := ex.w
	l := loc + "/" + e.Name
	status, note := model.Supported, ""
	if e.Required {
		status, note = model.Partial, "required=true not representable: JMeter does not fail the sample when nothing is extracted (add an assertion on the default value)"
	}
	if e.Match == model.MatchAll && e.MaxMatches > 0 {
		note = joinNote(note, "maxMatches has no JMeter equivalent (JMeter stores every match)")
	}
	def := ex.fn(e.Default, l)
	switch e.Type {
	case model.ExRegex:
		expr := ex.fn(e.Expr, l)
		tpl := ex.fn(e.Template, l)
		if tpl == "" {
			g := e.Group
			if g == 0 {
				if c, err := regexp.Compile(varRefRE.ReplaceAllString(e.Expr, "x")); err == nil && c.NumSubexp() > 0 {
					g = 1
				}
			}
			tpl = fmt.Sprintf("$%d$", g)
		}
		if e.Source == model.SrcHeaders || e.Source == model.SrcRequestHeaders {
			status = worse(status, model.Partial)
			note = joinNote(note, headerRenderNote)
		}
		ex.item("extract.regex", e.Name, l, status, joinNote("Regular Expression Extractor (Go RE2 pattern written as-is; JMeter uses Perl5 syntax)", note))
		ex.regexEl(e.Name, exportExtractSource(e.Source), expr, tpl, def, jmMatch(e.Match))
	case model.ExJSONPath:
		ex.item("extract.jsonpath", e.Name, l, status, joinNote("JSON Extractor", note))
		w.element("JSONPostProcessor", "JSONPostProcessorGui", e.Name, true, func() {
			w.str("JSONPostProcessor.referenceNames", e.Name)
			w.str("JSONPostProcessor.jsonPathExprs", ex.fn(e.Expr, l))
			w.str("JSONPostProcessor.match_numbers", jmMatch(e.Match))
			w.str("JSONPostProcessor.defaultValues", def)
			if e.Match == model.MatchAll {
				w.boolp("JSONPostProcessor.compute_concat", true)
			}
		}, nil)
	case model.ExBoundary:
		ex.item("extract.boundary", e.Name, l, status, joinNote("Boundary Extractor", note))
		w.element("BoundaryExtractor", "BoundaryExtractorGui", e.Name, true, func() {
			w.str("BoundaryExtractor.useHeaders", exportExtractSource(e.Source))
			w.str("BoundaryExtractor.refname", e.Name)
			w.str("BoundaryExtractor.lboundary", ex.fn(e.Left, l))
			w.str("BoundaryExtractor.rboundary", ex.fn(e.Right, l))
			w.str("BoundaryExtractor.default", def)
			w.boolp("BoundaryExtractor.default_empty_value", false)
			w.str("BoundaryExtractor.match_number", jmMatch(e.Match))
		}, nil)
	case model.ExXPath:
		ex.item("extract.xpath", e.Name, l, model.Partial, joinNote("XPath2 Extractor: XPath dialects may differ; verify", note))
		w.element("XPath2Extractor", "XPath2ExtractorGui", e.Name, true, func() {
			w.str("XPathExtractor2.default", def)
			w.str("XPathExtractor2.refname", e.Name)
			w.str("XPathExtractor2.matchNumber", jmMatch(e.Match))
			w.str("XPathExtractor2.xpathQuery", ex.fn(e.Expr, l))
			w.boolp("XPathExtractor2.fragment", false)
			w.str("XPathExtractor2.namespaces", "")
		}, nil)
	case model.ExHeader:
		ex.item("extract.header", e.Name, l, status, joinNote("Regular Expression Extractor on response headers (case-insensitive header name)", note))
		ex.regexEl(e.Name, "true", hdrPrefix+regexp.QuoteMeta(e.Expr)+hdrSuffix, "$1$", def, jmMatch(e.Match))
	case model.ExCookie:
		ex.item("extract.cookie", e.Name, l, model.Partial, joinNote("Regular Expression Extractor on Set-Cookie headers of this response only (Load Meter also falls back to the cookie jar)", note))
		ex.regexEl(e.Name, "true", cookiePrefix+regexp.QuoteMeta(e.Expr)+cookieSuffix, "$1$", def, jmMatch(e.Match))
	case model.ExStatus:
		ex.item("extract.status", e.Name, l, status, joinNote("Regular Expression Extractor on the response code", note))
		ex.regexEl(e.Name, "code", "(.*)", "$1$", def, "1")
	default:
		ex.item("extract."+e.Type, e.Name, l, model.Unsupported, "unknown extractor type; not exported")
	}
}

// ---------------------------------------------------------------------------
// Export: assertions

// responseAssertion writes a Response Assertion. The request's ignoreStatus
// is written on the first one (JMeter "Ignore Status").
func (ex *exporter) responseAssertion(name, field string, testType int, pats []string, msg string) {
	w := ex.w
	ignore := ex.ignoreStatus
	ex.ignoreStatus = false
	w.element("ResponseAssertion", "AssertionGui", name, true, func() {
		w.open("collectionProp", "name", "Asserion.test_strings")
		for _, p := range pats {
			w.str(javaHash(p), p)
		}
		w.close("collectionProp")
		w.str("Assertion.custom_message", msg)
		w.str("Assertion.test_field", field)
		w.boolp("Assertion.assume_success", ignore)
		w.intp("Assertion.test_type", testType)
	}, nil)
}

// flushIgnoreStatus writes a pass-through Response Assertion carrying "Ignore
// Status" when the request had ignoreStatus but no Response Assertion.
func (ex *exporter) flushIgnoreStatus(loc string) {
	if !ex.ignoreStatus {
		return
	}
	ex.item("request.ignoreStatus", "", loc, model.Supported, "Response Assertion with \"Ignore Status\" (matches any response code)")
	ex.responseAssertion("Ignore status", "Assertion.response_code", 2, []string{".*"}, "")
}

func notBit(not bool) int {
	if not {
		return 4
	}
	return 0
}

var invertOp = map[string]string{"==": "!=", "!=": "==", "<": ">=", ">=": "<", ">": "<=", "<=": ">"}
var jmxSizeOp = map[string]int{"==": 1, "!=": 2, ">": 3, "<": 4, ">=": 5, "<=": 6}

func (ex *exporter) assertion(a model.Assertion, loc string) {
	w := ex.w
	l := loc + "/assert " + a.Type
	val := ex.fn(a.Value, l)
	msg := ex.fn(a.Message, l)
	field := exportAssertField(a.Source)
	srcNote := ""
	srcStatus := model.Supported
	if a.Source == model.SrcHeaders || a.Source == model.SrcRequestHeaders {
		srcStatus, srcNote = model.Partial, headerRenderNote
	}
	msgLost := func(st, note string) (string, string) {
		if a.Message != "" {
			return worse(st, model.Partial), joinNote(note, "custom message not exported for this assertion type")
		}
		return st, note
	}
	switch a.Type {
	case model.AsStatus:
		var pats []string
		classes := false
		for _, v := range strings.Split(val, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if len(v) == 3 && strings.HasSuffix(strings.ToLower(v), "xx") {
				pats = append(pats, v[:1]+`\d\d`)
				classes = true
				continue
			}
			pats = append(pats, v)
		}
		if len(pats) == 0 {
			ex.item("assert.status", "", l, model.Unsupported, "empty status list")
			return
		}
		tt := 8
		if classes {
			tt = 1
		}
		if len(pats) > 1 && !a.Not {
			tt |= 32 // OR; NOT keeps AND so that no listed code may match
		}
		ex.item("assert.status", val, l, model.Supported, "Response Assertion on the response code")
		ex.responseAssertion("Status "+val, "Assertion.response_code", tt|notBit(a.Not), pats, msg)
	case model.AsContains:
		ex.item("assert.contains", val, l, srcStatus, joinNote("Response Assertion (Substring)", srcNote))
		ex.responseAssertion("Contains", field, 16|notBit(a.Not), []string{val}, msg)
	case model.AsNotContains:
		ex.item("assert.not_contains", val, l, srcStatus, joinNote("Response Assertion (Substring, NOT)", srcNote))
		ex.responseAssertion("Does not contain", field, 16|notBit(!a.Not), []string{val}, msg)
	case model.AsEquals:
		ex.item("assert.equals", val, l, srcStatus, joinNote("Response Assertion (Equals)", srcNote))
		ex.responseAssertion("Equals", field, 8|notBit(a.Not), []string{val}, msg)
	case model.AsRegex:
		ex.item("assert.regex", a.Expr, l, srcStatus, joinNote("Response Assertion (Contains = regex find; Go RE2 pattern written as-is)", srcNote))
		ex.responseAssertion("Matches regex", field, 2|notBit(a.Not), []string{ex.fn(a.Expr, l)}, msg)
	case model.AsJSONPath:
		st, note := msgLost(model.Supported, "JSON Assertion")
		ex.item("assert.jsonpath", a.Expr, l, st, note)
		w.element("JSONPathAssertion", "JSONPathAssertionGui", "JSON "+a.Expr, true, func() {
			w.str("JSON_PATH", ex.fn(a.Expr, l))
			w.str("EXPECTED_VALUE", val)
			w.boolp("JSONVALIDATION", a.Value != "" || a.Regex || a.ExpectNull)
			w.boolp("EXPECT_NULL", a.ExpectNull)
			w.boolp("INVERT", a.Not)
			w.boolp("ISREGEX", a.Regex)
		}, nil)
	case model.AsSize:
		op := a.Op
		if op == "" {
			op = "=="
		}
		if a.Not {
			op = invertOp[op]
		}
		f := "SizeAssertion.response_data"
		if a.Expr == "full" {
			f = "SizeAssertion.response_network_size"
		}
		st, note := msgLost(model.Supported, "Size Assertion")
		ex.item("assert.size", val, l, st, note)
		w.element("SizeAssertion", "SizeAssertionGui", "Size "+op+" "+val, true, func() {
			w.str("Assertion.test_field", f)
			w.str("SizeAssertion.size", val)
			w.intp("SizeAssertion.operator", jmxSizeOp[op])
		}, nil)
	case model.AsHash:
		alg := strings.ToLower(a.Expr)
		if alg != "" && alg != "md5" {
			ex.item("assert.hash", a.Expr, l, model.Unsupported, "JMeter only has an MD5 hash assertion; not exported")
			return
		}
		if a.Not {
			ex.item("assert.hash", a.Expr, l, model.Unsupported, "negated MD5 assertion has no JMeter equivalent; not exported")
			return
		}
		st, note := msgLost(model.Supported, "MD5Hex Assertion")
		ex.item("assert.hash", "md5", l, st, note)
		w.element("MD5HexAssertion", "MD5HexAssertionGUI", "MD5 "+val, true, func() {
			w.str("MD5HexAssertion.size", strings.ToLower(val))
		}, nil)
	case model.AsMaxLatency:
		d, err := time.ParseDuration(strings.TrimSpace(a.Value))
		if err != nil || d <= 0 {
			ex.item("assert.max_latency", a.Value, l, model.Unsupported, "value is not a positive duration")
			return
		}
		if a.Not {
			ex.item("assert.max_latency", a.Value, l, model.Unsupported, "negated duration assertion has no JMeter equivalent; not exported")
			return
		}
		st, note := msgLost(model.Supported, "Duration Assertion")
		ex.item("assert.max_latency", a.Value, l, st, note)
		w.element("DurationAssertion", "DurationAssertionGui", "Max latency "+a.Value, true, func() {
			w.str("DurationAssertion.duration", strconv.FormatInt(d.Milliseconds(), 10))
		}, nil)
	case model.AsHeader:
		pat := a.Expr + ": " + val
		if val == "" {
			pat = a.Expr + ":"
		}
		ex.item("assert.header", a.Expr, l, model.Partial, "Response Assertion (Substring) on the raw response headers: case-sensitive, matches across the header block")
		ex.responseAssertion("Header "+a.Expr, "Assertion.response_headers", 16|notBit(a.Not), []string{pat}, msg)
	default:
		ex.item("assert."+a.Type, "", l, model.Unsupported, "unknown assertion type; not exported")
	}
}
