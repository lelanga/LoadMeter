package loadrunner

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lelanga/loadmeter/internal/model"
)

// importer holds conversion state for one script. Request-scoped state
// (registered extractors/assertions, one-shot headers) follows LoadRunner's
// registration semantics: it attaches to the NEXT action function.
type importer struct {
	name string
	fs   *fileSet
	rep  *model.CompatReport
	sc   *model.Scenario
	an   *Analysis

	lb, rb    string // parameter braces, "{" and "}" by default
	params    map[string]*paramDef
	known     map[string]bool   // parameter names that become Load Meter variables
	arrays    map[string]bool   // names registered with Ord=ALL
	funcs     map[string]string // parameter name -> Load Meter function expression
	userFuncs map[string]string // custom C function name -> defining file
	once      map[string]bool
	warnedRef map[string]bool
	rts       runtimeSettings

	auto    []model.Header
	next    []model.Header
	pendEx  []model.Extractor
	pendAs  []model.Assertion
	pendLoc []string
	lastURL string
	role    string // init | action | end
}

// frame is one level of the step-building stack: the root list, an open
// transaction, or a loop body.
type frame struct {
	kind  string // root | tx | loop
	name  string
	loc   string
	count int
	steps []model.Step
}

type builder struct {
	im    *importer
	file  string
	stack []*frame
}

func (im *importer) newBuilder(file string) *builder {
	return &builder{im: im, file: file, stack: []*frame{{kind: "root"}}}
}

func (b *builder) top() *frame { return b.stack[len(b.stack)-1] }

func (b *builder) emit(st model.Step) { b.top().steps = append(b.top().steps, st) }

func (b *builder) push(kind, name, loc string, count int) {
	b.stack = append(b.stack, &frame{kind: kind, name: name, loc: loc, count: count})
}

func (b *builder) pop() {
	f := b.top()
	b.stack = b.stack[:len(b.stack)-1]
	switch f.kind {
	case "tx":
		b.emit(model.Step{Type: model.StepTransaction, Name: f.name, Steps: f.steps, Comment: "lr_start_transaction at " + f.loc})
	case "loop":
		b.emit(model.Step{Type: model.StepLoop, Name: f.name, Count: f.count, Steps: f.steps, Comment: "C for-loop at " + f.loc})
	}
}

// endTx closes the named transaction; transactions opened after it (inside
// the same loop level) are closed too, with a warning.
func (b *builder) endTx(name, loc string) bool {
	for i := len(b.stack) - 1; i > 0; i-- {
		f := b.stack[i]
		if f.kind == "loop" {
			break
		}
		if f.kind == "tx" && f.name == name {
			for len(b.stack)-1 > i {
				inner := b.top()
				b.im.rep.Warnings = append(b.im.rep.Warnings, fmt.Sprintf("%s: transaction %q closed implicitly by the end of %q", inner.loc, inner.name, name))
				b.pop()
			}
			b.pop()
			return true
		}
	}
	return false
}

// closeTo pops frames above depth, warning about unclosed transactions.
func (b *builder) closeTo(depth int, why string) {
	for len(b.stack) > depth {
		f := b.top()
		if f.kind == "tx" {
			b.im.rep.Warnings = append(b.im.rep.Warnings, fmt.Sprintf("%s: transaction %q is never ended (%s); closed automatically", f.loc, f.name, why))
			b.im.rep.Add("lr_start_transaction", f.name, f.loc, model.Partial, "unbalanced transaction: no matching lr_end_transaction ("+why+")")
		}
		b.pop()
	}
}

func (b *builder) finish() []model.Step {
	b.closeTo(1, "end of "+b.file)
	return b.stack[0].steps
}

func (im *importer) ignoredOnce(key, element, name, loc, note string) {
	if im.once[key] {
		return
	}
	im.once[key] = true
	im.rep.Add(element, name, loc, model.Ignored, note+" (reported once)")
}

func (im *importer) external(f string) {
	for _, e := range im.an.ExternalFiles {
		if e == f {
			return
		}
	}
	im.an.ExternalFiles = append(im.an.ExternalFiles, f)
}

// --- parameter references -------------------------------------------------

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sub rewrites LoadRunner {Param} references into Load Meter ${Param}. Only
// names that are declared (.prm) or registered by the script are rewritten;
// other identifier-like braces are left verbatim and reported once.
func (im *importer) sub(s, loc string) string {
	lb, rb := im.lb, im.rb
	if !strings.Contains(s, lb) {
		return s
	}
	var out strings.Builder
	for {
		i := strings.Index(s, lb)
		if i < 0 {
			out.WriteString(s)
			break
		}
		j := strings.Index(s[i+len(lb):], rb)
		if j < 0 {
			out.WriteString(s)
			break
		}
		name := s[i+len(lb) : i+len(lb)+j]
		if lb == "{" && i > 0 && s[i-1] == '$' {
			// already Load Meter syntax (or a literal "${"): keep as is
			out.WriteString(s[:i+len(lb)])
			s = s[i+len(lb):]
			continue
		}
		if !identRe.MatchString(name) {
			out.WriteString(s[:i+len(lb)])
			s = s[i+len(lb):]
			continue
		}
		out.WriteString(s[:i])
		switch {
		case im.funcs[name] != "":
			out.WriteString(im.funcs[name])
		case im.known[name] || im.arrayRef(name):
			out.WriteString("${" + name + "}")
		default:
			out.WriteString(lb + name + rb)
			if !im.warnedRef[name] {
				im.warnedRef[name] = true
				im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s: %s%s%s is not a declared or registered parameter; left as literal text", loc, lb, name, rb))
			}
		}
		s = s[i+len(lb)+j+len(rb):]
	}
	return out.String()
}

func (im *importer) arrayRef(name string) bool {
	k := strings.LastIndexByte(name, '_')
	if k <= 0 {
		return false
	}
	base, suf := name[:k], name[k+1:]
	if !im.arrays[base] {
		return false
	}
	if suf == "count" {
		return true
	}
	_, err := strconv.Atoi(suf)
	return err == nil
}

// --- argument helpers -----------------------------------------------------

type lrAttr struct {
	key   string // base key, e.g. "LB"
	flags string // qualifiers after '/', e.g. "IC"
	val   string
	hasEq bool
	line  int
}

type lrArgs struct {
	first    string
	hasFirst bool
	attrs    []lrAttr
	items    [][]lrAttr
	extrares [][]lrAttr
	filters  []lrAttr
	idents   []string
	exprs    []cArg // arguments that are C expressions (variables, arithmetic, other calls)
}

func (a *lrArgs) get(key string) (lrAttr, bool) {
	for _, at := range a.attrs {
		if strings.EqualFold(at.key, key) {
			return at, true
		}
	}
	return lrAttr{}, false
}

func (a *lrArgs) val(key string) string {
	v, _ := a.get(key)
	return v.val
}

func filterVal(fs []lrAttr, key string) (string, bool) {
	for _, at := range fs {
		if strings.EqualFold(at.key, key) {
			return at.val, true
		}
	}
	return "", false
}

func itemVal(it []lrAttr, key string) (lrAttr, bool) {
	for _, at := range it {
		if strings.EqualFold(at.key, key) {
			return at, true
		}
	}
	return lrAttr{}, false
}

// strArg returns the string value of an argument: a literal, or
// lr_eval_string("literal") (evaluated == true).
func strArg(a cArg) (s string, evaluated, ok bool) {
	switch a.kind {
	case aString:
		return a.str, false, true
	case aCall:
		if a.call.name == "lr_eval_string" && len(a.call.args) == 1 && a.call.args[0].kind == aString {
			return a.call.args[0].str, true, true
		}
	}
	return "", false, false
}

func splitAttr(s string, line int) lrAttr {
	k, v, ok := strings.Cut(s, "=")
	at := lrAttr{key: strings.TrimSpace(k), val: v, hasEq: ok, line: line}
	if base, flags, has := strings.Cut(at.key, "/"); has {
		at.key, at.flags = base, flags
	}
	return at
}

func parseLRArgs(args []cArg, firstIsName bool) lrArgs {
	var r lrArgs
	mode := "attr"
	var cur []lrAttr
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if mode == "item" {
			r.items = append(r.items, cur)
		} else if mode == "extrares" {
			r.extrares = append(r.extrares, cur)
		}
		cur = nil
	}
	for i, a := range args {
		if a.kind == aIdent {
			switch a.text {
			case "LAST":
				flush()
				return r
			case "ITEMDATA":
				flush()
				mode = "item"
				continue
			case "EXTRARES":
				flush()
				mode = "extrares"
				continue
			case "ENDITEM":
				flush()
				continue
			case "SEARCH_FILTERS":
				flush()
				mode = "filters"
				continue
			}
			r.idents = append(r.idents, a.text)
			continue
		}
		s, _, ok := strArg(a)
		if !ok {
			r.exprs = append(r.exprs, a)
			continue
		}
		if i == 0 && firstIsName {
			r.first, r.hasFirst = s, true
			continue
		}
		at := splitAttr(s, a.line)
		switch mode {
		case "item", "extrares":
			cur = append(cur, at)
		case "filters":
			r.filters = append(r.filters, at)
		default:
			r.attrs = append(r.attrs, at)
		}
	}
	flush()
	return r
}

// --- statements -----------------------------------------------------------

func (im *importer) loc(f *cFile, line int) string { return fmt.Sprintf("%s:%d", f.name, line) }

func (im *importer) stmts(f *cFile, list []stmt, b *builder) {
	for i := range list {
		im.stmt(f, &list[i], b)
	}
}

func (im *importer) stmt(f *cFile, s *stmt, b *builder) {
	loc := im.loc(f, s.line)
	switch s.kind {
	case sEmpty:
	case sCall:
		im.call(f, s, b)
	case sBlock:
		im.stmts(f, s.body, b)
	case sReturn:
		switch s.keyword {
		case "0", "", "LR_PASS", "(0)":
		default:
			im.rep.Add("return", s.keyword, loc, model.Partial, "non-zero return value ignored (in LoadRunner it can end the iteration or the Vuser); excerpt: "+s.excerpt)
			im.customCode(f, s.line, "return", s.excerpt)
		}
	case sControl:
		if s.keyword == "for" {
			if n, ok := im.countedLoop(s); ok {
				im.rep.Add("for", fmt.Sprintf("%d iterations", n), loc, model.Partial,
					"simple counted C loop converted to a loop step; the loop variable itself is not available. Excerpt: "+s.excerpt)
				im.customCode(f, s.line, "control-flow", s.excerpt)
				depth := len(b.stack)
				b.push("loop", "", loc, n)
				im.stmts(f, s.body, b)
				b.closeTo(depth+1, "end of loop body")
				b.pop()
				return
			}
		}
		lost := 0
		walkCalls(s.body, func(c *cCall) {
			if isActionFunc(c.name) {
				lost++
			}
		})
		note := fmt.Sprintf("C %s statement is not converted (Load Meter has no general control flow import)", s.keyword)
		if lost > 0 {
			note += fmt.Sprintf("; %d HTTP request call(s) inside were NOT imported", lost)
		}
		note += ". Excerpt: " + s.excerpt
		im.rep.Add(s.keyword, "", loc, model.Unsupported, note)
		im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: re-create the logic of `%s` (loop/if steps or separate scenarios)", loc, s.excerpt))
		im.customCode(f, s.line, "control-flow", s.excerpt)
	case sDecl:
		im.rep.Add("declaration", "", loc, model.Unsupported, "C variable declaration not imported. Excerpt: "+s.excerpt)
		im.customCode(f, s.line, "declaration", s.excerpt)
	case sLabel:
		im.rep.Add("label", "", loc, model.Unsupported, "C label not imported. Excerpt: "+s.excerpt)
		im.customCode(f, s.line, "label", s.excerpt)
	default:
		im.rep.Add("expression", "", loc, model.Unsupported, "C expression statement not imported. Excerpt: "+s.excerpt)
		im.customCode(f, s.line, "expression", s.excerpt)
	}
}

func (im *importer) customCode(f *cFile, line int, kind, excerpt string) {
	ff := im.an.file(f.name)
	ff.CustomCode = append(ff.CustomCode, Finding{Location: im.loc(f, line), Kind: kind, Excerpt: excerpt})
}

// countedLoop recognises `for (i = A; i < B; i++)` (also <=, ++i, i += 1,
// and an `int` in the initializer) with integer literals, whose body does
// not use the loop variable.
func (im *importer) countedLoop(s *stmt) (int, bool) {
	var parts [][]token
	cur := []token{}
	for _, t := range s.header {
		if t.is(tPunct, ";") {
			parts = append(parts, cur)
			cur = []token{}
			continue
		}
		cur = append(cur, t)
	}
	parts = append(parts, cur)
	if len(parts) != 3 {
		return 0, false
	}
	init, cond, inc := parts[0], parts[1], parts[2]
	if len(init) == 4 && init[0].kind == tIdent && declKeywords[init[0].text] {
		init = init[1:]
	}
	if len(init) != 3 || init[0].kind != tIdent || !init[1].is(tPunct, "=") || init[2].kind != tNumber {
		return 0, false
	}
	v := init[0].text
	a, err := strconv.Atoi(init[2].text)
	if err != nil {
		return 0, false
	}
	if len(cond) != 3 || !cond[0].is(tIdent, v) || cond[2].kind != tNumber {
		return 0, false
	}
	bnd, err := strconv.Atoi(cond[2].text)
	if err != nil {
		return 0, false
	}
	var n int
	switch cond[1].text {
	case "<", "!=":
		n = bnd - a
	case "<=":
		n = bnd - a + 1
	default:
		return 0, false
	}
	incText := ""
	for _, t := range inc {
		incText += t.text
	}
	if incText != v+"++" && incText != "++"+v && incText != v+"+=1" && incText != v+"="+v+"+1" {
		return 0, false
	}
	if n < 1 || n > 1_000_000 {
		return 0, false
	}
	// The body must not reference the loop variable.
	used := false
	vre := regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\b`)
	var check func([]stmt)
	check = func(list []stmt) {
		for _, st := range list {
			if st.kind != sCall && vre.MatchString(st.keyword) {
				used = true
			}
			if st.call != nil {
				for _, a := range st.call.args {
					if a.kind != aString && vre.MatchString(a.text) {
						used = true
					}
				}
			}
			check(st.body)
		}
	}
	check(s.body)
	return n, !used
}

// --- calls ----------------------------------------------------------------

// isActionFunc reports whether name sends an HTTP request.
func isActionFunc(name string) bool {
	switch name {
	case "web_url", "web_submit_data", "web_custom_request", "web_submit_form", "web_link", "web_image",
		"web_browser", "web_text_link", "web_image_link", "web_button", "web_edit_field":
		return true
	}
	return false
}

var ignoredMessages = map[string]bool{
	"lr_output_message": true, "lr_log_message": true, "lr_message": true, "lr_error_message": true,
	"lr_vuser_status_message": true, "lr_debug_message": true, "lr_set_debug_message": true,
}

func (im *importer) call(f *cFile, s *stmt, b *builder) {
	c := s.call
	loc := im.loc(f, c.line)
	if strings.HasPrefix(s.keyword, "assign:") {
		im.rep.Add(c.name, strings.TrimPrefix(s.keyword, "assign:"), loc, model.Partial,
			"return value assigned to C variable "+strings.TrimPrefix(s.keyword, "assign:")+" is not available in Load Meter")
		im.customCode(f, c.line, "assignment", s.excerpt)
	}
	switch c.name {
	case "web_url":
		im.webURL(f, c, b)
	case "web_submit_data":
		im.webSubmitData(f, c, b)
	case "web_custom_request":
		im.webCustomRequest(f, c, b)
	case "web_submit_form":
		im.webSubmitForm(f, c, b)
	case "web_add_header", "web_add_auto_header":
		if len(c.args) < 2 {
			im.rep.Add(c.name, "", loc, model.Unsupported, "expected (name, value)")
			return
		}
		n, _, ok1 := strArg(c.args[0])
		v, _, ok2 := strArg(c.args[1])
		if !ok1 || !ok2 {
			im.unsupportedArgs(c, loc)
			return
		}
		h := model.Header{Name: n, Value: im.sub(v, loc)}
		if c.name == "web_add_header" {
			im.next = append(im.next, h)
			im.rep.Add(c.name, n, loc, model.Supported, "header added to the next request")
		} else {
			im.auto = removeHeader(im.auto, n)
			im.auto = append(im.auto, h)
			im.rep.Add(c.name, n, loc, model.Supported, "header added to every following request")
		}
	case "web_remove_auto_header", "web_revert_auto_header":
		if len(c.args) < 1 {
			im.rep.Add(c.name, "", loc, model.Unsupported, "expected header name")
			return
		}
		n, _, ok := strArg(c.args[0])
		if !ok {
			im.unsupportedArgs(c, loc)
			return
		}
		im.auto = removeHeader(im.auto, n)
		im.rep.Add(c.name, n, loc, model.Supported, "header no longer added to following requests")
	case "web_cleanup_auto_headers":
		im.auto = nil
		im.rep.Add(c.name, "", loc, model.Supported, "auto headers cleared for following requests")
	case "web_reg_save_param":
		im.regSaveParam(f, c)
	case "web_reg_save_param_ex":
		im.regSaveParamEx(f, c)
	case "web_reg_save_param_json":
		im.regSaveParamJSON(f, c)
	case "web_reg_save_param_regexp":
		im.regSaveParamRegexp(f, c)
	case "web_reg_save_param_xpath":
		im.regSaveParamXPath(f, c)
	case "web_reg_find":
		im.regFind(f, c)
	case "lr_start_transaction", "lr_start_sub_transaction":
		n, _, ok := firstStr(c)
		if !ok {
			im.unsupportedArgs(c, loc)
			return
		}
		b.push("tx", n, loc, 0)
		im.an.addTx(f.name, n)
		st, note := model.Supported, "transaction step"
		if c.name == "lr_start_sub_transaction" {
			st, note = model.Partial, "sub-transaction imported as a nested transaction step"
		}
		im.rep.Add(c.name, n, loc, st, note)
	case "lr_end_transaction", "lr_end_sub_transaction":
		n, _, ok := firstStr(c)
		if !ok {
			im.unsupportedArgs(c, loc)
			return
		}
		if !b.endTx(n, loc) {
			im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s: lr_end_transaction(%q) without a matching lr_start_transaction in the same block", loc, n))
			im.rep.Add(c.name, n, loc, model.Partial, "unbalanced transaction end ignored")
			return
		}
		if len(c.args) > 1 && c.args[len(c.args)-1].kind == aIdent {
			switch st := c.args[len(c.args)-1].text; st {
			case "LR_AUTO", "LR_PASS":
			default:
				im.rep.Add(c.name, n, loc, model.Partial, "forced transaction status "+st+" is not reproduced; Load Meter derives status from request results")
			}
		}
	case "lr_think_time":
		if len(c.args) != 1 || c.args[0].kind != aNumber {
			im.unsupportedArgs(c, loc)
			return
		}
		v, err := strconv.ParseFloat(c.args[0].text, 64)
		if err != nil || v < 0 {
			im.unsupportedArgs(c, loc)
			return
		}
		b.emit(model.Step{Type: model.StepThink, Think: im.think(v), Comment: loc})
		im.rep.Add(c.name, c.args[0].text, loc, model.Supported, "think step")
	case "lr_save_string":
		if len(c.args) < 2 {
			im.unsupportedArgs(c, loc)
			return
		}
		v, eval, ok1 := strArg(c.args[0])
		n, _, ok2 := strArg(c.args[1])
		if !ok1 || !ok2 {
			im.unsupportedArgs(c, loc)
			return
		}
		if eval {
			v = im.sub(v, loc)
		}
		im.known[n] = true
		b.emit(model.Step{Type: model.StepSetVar, Name: n, Set: []model.KV{{Name: n, Value: v}}, Comment: loc})
		im.rep.Add(c.name, n, loc, model.Supported, "set step")
	case "lr_save_int":
		if len(c.args) < 2 || c.args[0].kind != aNumber {
			im.unsupportedArgs(c, loc)
			return
		}
		n, _, ok := strArg(c.args[1])
		if !ok {
			im.unsupportedArgs(c, loc)
			return
		}
		im.known[n] = true
		b.emit(model.Step{Type: model.StepSetVar, Name: n, Set: []model.KV{{Name: n, Value: c.args[0].text}}, Comment: loc})
		im.rep.Add(c.name, n, loc, model.Supported, "set step")
	case "web_save_timestamp_param":
		n, _, ok := firstStr(c)
		if !ok {
			im.unsupportedArgs(c, loc)
			return
		}
		im.known[n] = true
		b.emit(model.Step{Type: model.StepSetVar, Name: n, Set: []model.KV{{Name: n, Value: "${__timestamp()}"}}, Comment: loc})
		im.rep.Add(c.name, n, loc, model.Supported, "set step with ${__timestamp()} (epoch milliseconds)")
	case "lr_eval_string":
		im.ignoredOnce(c.name, c.name, "", loc, "lr_eval_string used as a statement has no effect")
	case "web_set_timeout":
		im.setTimeout(c, loc)
	case "web_set_user":
		im.setUser(c, loc)
	case "web_set_cookie":
		v, _, _ := firstStr(c)
		im.rep.Add(c.name, v, loc, model.Partial, "explicit cookie not imported; Load Meter manages cookies from Set-Cookie responses")
		im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: cookie %q was set explicitly; add a Cookie header or a setup request that sets it", loc, v))
	case "web_cleanup_cookies":
		if im.role == "init" {
			im.rep.Add(c.name, "", loc, model.Ignored, "every Load Meter VU starts with an empty cookie jar")
		} else {
			im.rep.Add(c.name, "", loc, model.Partial, "clearing cookies mid-iteration is not supported; cookies persist for the VU")
		}
	case "web_set_max_html_param_len":
		im.ignoredOnce(c.name, c.name, "", loc, "Load Meter has no per-parameter length limit")
	case "web_cache_cleanup":
		im.ignoredOnce(c.name, c.name, "", loc, "Load Meter does not emulate a browser cache")
	case "web_set_sockets_option":
		opt, _, _ := firstStr(c)
		switch strings.ToUpper(opt) {
		case "IGNORE_PREMATURE_SHUTDOWN", "CLOSE_KEEPALIVE_CONNECTIONS", "OVERRIDE_CONNECTION_REUSE":
			im.rep.Add(c.name, opt, loc, model.Ignored, "socket tuning option not applicable to Load Meter's HTTP client")
		default:
			im.rep.Add(c.name, opt, loc, model.Partial, "socket/TLS option not mapped; Load Meter negotiates TLS with Go defaults")
		}
	default:
		switch {
		case ignoredMessages[c.name]:
			im.ignoredOnce(c.name, c.name, "", loc, "log/message output is not imported")
		case im.userFuncs[c.name] != "":
			im.rep.Add(c.name, "", loc, model.Unsupported, fmt.Sprintf("call to custom C function defined in %s is not imported. Excerpt: %s", im.userFuncs[c.name], s.excerpt))
			im.customCode(f, c.line, "custom-function-call", s.excerpt)
			im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: re-create custom function %s() with Load Meter steps", loc, c.name))
		default:
			if proto := protocolOf(c.name); proto != "" {
				im.rep.Add(c.name, "", loc, model.Unsupported, fmt.Sprintf("%s protocol function; requires the licensed LoadRunner runtime. Excerpt: %s", proto, s.excerpt))
				im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: %s() (%s) has no HTTP equivalent; remove it or replace it with the HTTP traffic it produces", loc, c.name, proto))
				return
			}
			if strings.HasPrefix(c.name, "lr_") || strings.HasPrefix(c.name, "web_") {
				im.rep.Add(c.name, "", loc, model.Unsupported, "LoadRunner runtime function not supported by the importer. Excerpt: "+s.excerpt)
				return
			}
			im.rep.Add(c.name, "", loc, model.Unsupported, "C library or custom function call is not imported. Excerpt: "+s.excerpt)
			im.customCode(f, c.line, "c-call", s.excerpt)
		}
	}
}

func firstStr(c *cCall) (string, bool, bool) {
	if len(c.args) == 0 {
		return "", false, false
	}
	return strArg(c.args[0])
}

func (im *importer) unsupportedArgs(c *cCall, loc string) {
	im.rep.Add(c.name, "", loc, model.Unsupported, "arguments are C expressions or have an unexpected shape; call not imported")
}

func removeHeader(h []model.Header, name string) []model.Header {
	out := h[:0:0]
	for _, x := range h {
		if !strings.EqualFold(x.Name, name) {
			out = append(out, x)
		}
	}
	return out
}

// attrsCheck reports C-expression arguments (call cannot be converted) and
// returns false in that case.
func (im *importer) attrsCheck(c *cCall, a *lrArgs, loc string) bool {
	if len(a.exprs) == 0 {
		return true
	}
	var parts []string
	for _, e := range a.exprs {
		parts = append(parts, collapseSpace(e.text))
	}
	im.rep.Add(c.name, a.first, loc, model.Unsupported, "argument(s) built by C code cannot be converted: "+strings.Join(parts, ", "))
	im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: %s uses C expressions as arguments; rewrite with ${...} parameters", loc, c.name))
	return false
}

var requestIgnoredAttrs = map[string]string{
	"resource":        "Resource flag only affects LoadRunner's resource statistics",
	"reccontenttype":  "RecContentType is recording metadata",
	"snapshot":        "Snapshot refers to VuGen recording snapshots",
	"mode":            "Mode=HTML makes LoadRunner parse pages and fetch embedded resources automatically; Load Meter replays only the explicit requests",
	"targetframe":     "TargetFrame is browser-frame metadata",
	"encodeatsign":    "EncodeAtSign is not applicable",
	"contentencoding": "ContentEncoding is recording metadata",
}

func (im *importer) noteAttrs(c *cCall, a *lrArgs, loc string, handled ...string) {
	for _, at := range a.attrs {
		k := strings.ToLower(at.key)
		isHandled := false
		for _, h := range handled {
			if k == strings.ToLower(h) {
				isHandled = true
			}
		}
		if isHandled {
			continue
		}
		if note, ok := requestIgnoredAttrs[k]; ok && note != "" {
			im.ignoredOnce("attr:"+k, c.name+" "+at.key+"=", "", loc, note)
			continue
		}
		im.rep.Add(c.name, a.first, loc, model.Partial, fmt.Sprintf("attribute %s=%q not mapped", at.key, at.val))
	}
}

func (im *importer) emitRequest(b *builder, f *cFile, c *cCall, name string, req *model.Request) {
	loc := im.loc(f, c.line)
	var hs []model.Header
	hs = append(hs, im.auto...)
	for _, h := range im.next {
		hs = removeHeader(hs, h.Name)
		hs = append(hs, h)
	}
	for _, h := range req.Headers {
		hs = removeHeader(hs, h.Name)
		hs = append(hs, h)
	}
	req.Headers = hs
	im.next = nil
	req.Extract = append(req.Extract, im.pendEx...)
	req.Assert = append(req.Assert, im.pendAs...)
	im.pendEx, im.pendAs, im.pendLoc = nil, nil, nil
	im.lastURL = req.URL
	im.an.Requests++
	b.emit(model.Step{Type: model.StepRequest, Name: name, Request: req, Comment: loc})
}

func (im *importer) extrares(c *cCall, a *lrArgs, loc string) {
	if len(a.extrares) == 0 {
		return
	}
	im.rep.Add("EXTRARES", a.first, loc, model.Partial,
		fmt.Sprintf("%d embedded resource(s) listed under EXTRARES are not replayed; add explicit requests if their load matters", len(a.extrares)))
}

func (im *importer) webURL(f *cFile, c *cCall, b *builder) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, true)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	u := a.val("URL")
	if u == "" {
		im.rep.Add(c.name, a.first, loc, model.Unsupported, "web_url without URL=")
		return
	}
	req := &model.Request{Method: "GET", URL: im.sub(u, loc)}
	if r := a.val("Referer"); r != "" {
		req.Headers = append(req.Headers, model.Header{Name: "Referer", Value: im.sub(r, loc)})
	}
	im.noteAttrs(c, &a, loc, "URL", "Referer")
	im.extrares(c, &a, loc)
	im.emitRequest(b, f, c, a.first, req)
	im.rep.Add(c.name, a.first, loc, model.Supported, "GET request")
}

func (im *importer) webSubmitData(f *cFile, c *cCall, b *builder) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, true)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	u := a.val("Action")
	if u == "" {
		im.rep.Add(c.name, a.first, loc, model.Unsupported, "web_submit_data without Action=")
		return
	}
	method := strings.ToUpper(a.val("Method"))
	if method == "" {
		method = "POST"
	}
	req := &model.Request{Method: method, URL: im.sub(u, loc)}
	if r := a.val("Referer"); r != "" {
		req.Headers = append(req.Headers, model.Header{Name: "Referer", Value: im.sub(r, loc)})
	}
	enc := strings.ToLower(a.val("EncType"))
	multipart := strings.HasPrefix(enc, "multipart/")
	status, notes := model.Supported, []string{}
	var query []string
	for _, it := range a.items {
		n, ok := itemVal(it, "Name")
		if !ok {
			status = model.Partial
			notes = append(notes, "ITEMDATA entry without Name= skipped")
			continue
		}
		v, _ := itemVal(it, "Value")
		name, val := im.sub(n.val, loc), im.sub(v.val, loc)
		file, _ := itemVal(it, "File")
		ct, _ := itemVal(it, "ContentType")
		isFile := strings.EqualFold(file.val, "yes")
		switch {
		case multipart:
			p := model.Part{Name: name, ContentType: ct.val}
			if isFile {
				p.File = val
				im.external(v.val)
				status = model.Partial
				notes = append(notes, fmt.Sprintf("upload file %q must be placed next to the scenario", v.val))
			} else {
				p.Value = val
			}
			req.Multipart = append(req.Multipart, p)
		case method == "GET":
			query = append(query, escapeKeepVars(name)+"="+escapeKeepVars(val))
		default:
			if isFile {
				status = model.Partial
				notes = append(notes, fmt.Sprintf("file field %q sent as its file name; EncType is not multipart", name))
			}
			req.Form = append(req.Form, model.KV{Name: name, Value: val})
		}
	}
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(req.URL, "?") {
			sep = "&"
		}
		req.URL += sep + strings.Join(query, "&")
	}
	if enc != "" && !multipart && enc != "application/x-www-form-urlencoded" {
		status = model.Partial
		notes = append(notes, fmt.Sprintf("EncType %q not reproduced; sent as form", enc))
	}
	im.noteAttrs(c, &a, loc, "Action", "Method", "Referer", "EncType")
	im.extrares(c, &a, loc)
	im.emitRequest(b, f, c, a.first, req)
	kind := "form"
	if multipart {
		kind = "multipart"
	} else if method == "GET" {
		kind = "query string"
	}
	note := fmt.Sprintf("%s request with %d %s field(s)", method, len(a.items), kind)
	if len(notes) > 0 {
		note += "; " + strings.Join(notes, "; ")
	}
	im.rep.Add(c.name, a.first, loc, status, note)
}

// escapeKeepVars URL-encodes s but keeps ${...} references intact.
func escapeKeepVars(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(url.QueryEscape(s))
			return b.String()
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(url.QueryEscape(s))
			return b.String()
		}
		b.WriteString(url.QueryEscape(s[:i]))
		b.WriteString(s[i : i+j+1])
		s = s[i+j+1:]
	}
}

// quoteKeepVars regexp-quotes s but keeps ${...} references intact.
func quoteKeepVars(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(regexp.QuoteMeta(s))
			return b.String()
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(regexp.QuoteMeta(s))
			return b.String()
		}
		b.WriteString(regexp.QuoteMeta(s[:i]))
		b.WriteString(s[i : i+j+1])
		s = s[i+j+1:]
	}
}

func (im *importer) webCustomRequest(f *cFile, c *cCall, b *builder) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, true)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	u := a.val("URL")
	if u == "" {
		im.rep.Add(c.name, a.first, loc, model.Unsupported, "web_custom_request without URL=")
		return
	}
	method := strings.ToUpper(a.val("Method"))
	if method == "" {
		method = "GET"
	}
	req := &model.Request{Method: method, URL: im.sub(u, loc)}
	if r := a.val("Referer"); r != "" {
		req.Headers = append(req.Headers, model.Header{Name: "Referer", Value: im.sub(r, loc)})
	}
	if enc := a.val("EncType"); enc != "" {
		req.Headers = append(req.Headers, model.Header{Name: "Content-Type", Value: im.sub(enc, loc)})
	}
	status, notes := model.Supported, []string{}
	if at, ok := a.get("Body"); ok {
		req.Body = im.sub(at.val, loc)
	}
	if at, ok := a.get("BodyBinary"); ok {
		raw := decodeBinaryEscapes(at.val)
		req.Body = im.sub(raw, loc)
		if !utf8.ValidString(raw) {
			status = model.Partial
			notes = append(notes, "BodyBinary contains non-UTF-8 bytes that may not survive JSON serialization of the scenario")
		}
	}
	if at, ok := a.get("BodyUnicode"); ok {
		req.Body = im.sub(at.val, loc)
		status = model.Partial
		notes = append(notes, "BodyUnicode sent as UTF-8 (LoadRunner converts it to UTF-16)")
	}
	if at, ok := a.get("BodyFilePath"); ok {
		status = model.Partial
		notes = append(notes, fmt.Sprintf("body is read from file %q at run time; paste its content into the request body", at.val))
		im.external(at.val)
		im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: copy the content of %s into the body of request %q", loc, at.val, a.first))
	}
	im.noteAttrs(c, &a, loc, "URL", "Method", "Referer", "EncType", "Body", "BodyBinary", "BodyUnicode", "BodyFilePath")
	im.extrares(c, &a, loc)
	im.emitRequest(b, f, c, a.first, req)
	note := method + " request"
	if len(notes) > 0 {
		note += "; " + strings.Join(notes, "; ")
	}
	im.rep.Add(c.name, a.first, loc, status, note)
}

func (im *importer) webSubmitForm(f *cFile, c *cCall, b *builder) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, true)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	target := im.lastURL
	if target == "" {
		target = "/"
	}
	req := &model.Request{Method: "POST", URL: target}
	for _, it := range a.items {
		n, ok := itemVal(it, "Name")
		if !ok {
			continue
		}
		v, _ := itemVal(it, "Value")
		req.Form = append(req.Form, model.KV{Name: im.sub(n.val, loc), Value: im.sub(v.val, loc)})
	}
	im.emitRequest(b, f, c, a.first, req)
	// emitRequest appended the step; mark it disabled so the scenario stays runnable.
	fr := b.top()
	fr.steps[len(fr.steps)-1].Disabled = true
	im.rep.Add(c.name, a.first, loc, model.Partial,
		"web_submit_form fills a form found in the previous page (action URL, hidden fields, method); imported as a DISABLED POST to the previous URL with the listed fields only")
	im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: set the real form action URL and hidden fields for %q (web_submit_form), then enable the step", loc, a.first))
}

// --- registrations ----------------------------------------------------------

func (im *importer) register(f *cFile, c *cCall, ex model.Extractor, status, note, expr string) {
	loc := im.loc(f, c.line)
	im.pendEx = append(im.pendEx, ex)
	im.pendLoc = append(im.pendLoc, loc)
	im.known[ex.Name] = true
	ff := im.an.file(f.name)
	cr := Correlation{Param: ex.Name, Function: c.name, Kind: ex.Type, Location: loc, Expr: expr}
	ff.Correlations = append(ff.Correlations, cr)
	if note == "" {
		note = ex.Type + " extractor on the next request"
	} else {
		note = ex.Type + " extractor on the next request; " + note
	}
	im.rep.Add(c.name, ex.Name, loc, status, note)
}

func notFoundRequired(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "warning", "warn", "ignore":
		return false
	}
	return true
}

// ordinal maps Ord/Ordinal: n -> n, RANDOM -> -1, ALL -> (1, all=true).
func ordinal(v string) (match int, all, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "":
		return 0, false, true
	case "ALL":
		return 1, true, true
	case "RANDOM":
		return -1, false, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 0, false, false
	}
	return n, false, true
}

func (im *importer) applyOrdinal(ex *model.Extractor, v string, notes *[]string, status *string) {
	m, all, ok := ordinal(v)
	switch {
	case !ok:
		*status = model.Partial
		*notes = append(*notes, fmt.Sprintf("ordinal %q not understood; first match used", v))
	case all:
		im.arrays[ex.Name] = true
		ex.Name += "_1"
		ex.Match = 1
		im.known[ex.Name] = true
		*status = model.Partial
		*notes = append(*notes, "Ord=ALL: only the first match is saved (as "+ex.Name+"); the _N array and _count are not produced")
	default:
		ex.Match = m
	}
}

func flagNotes(at lrAttr, notes *[]string, status *string) {
	if at.flags == "" {
		return
	}
	*status = model.Partial
	*notes = append(*notes, fmt.Sprintf("%s qualifiers /%s (case-insensitive or binary boundaries) are not reproduced", at.key, at.flags))
}

func (im *importer) regSaveParam(f *cFile, c *cCall) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, true)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	if !a.hasFirst {
		im.rep.Add(c.name, "", loc, model.Unsupported, "parameter name missing")
		return
	}
	ex := model.Extractor{Name: a.first, Type: model.ExBoundary}
	status, notes := model.Supported, []string{}
	lb, _ := a.get("LB")
	rb, _ := a.get("RB")
	ex.Left, ex.Right = im.sub(lb.val, loc), im.sub(rb.val, loc)
	flagNotes(lb, &notes, &status)
	flagNotes(rb, &notes, &status)
	ex.Required = notFoundRequired(a.val("NotFound"))
	im.applyOrdinal(&ex, a.val("Ord"), &notes, &status)
	for _, at := range a.attrs {
		switch strings.ToLower(at.key) {
		case "lb", "rb", "ord", "notfound":
		case "search":
			if strings.EqualFold(at.val, "headers") {
				status = model.Partial
				notes = append(notes, "Search=Headers: the boundary extractor searches the response body")
			}
		case "relframeid", "ignoreredirections":
			im.ignoredOnce("attr:"+strings.ToLower(at.key), c.name+" "+at.key+"=", "", loc, "frame/redirect filters are not applicable")
		default:
			status = model.Partial
			notes = append(notes, fmt.Sprintf("%s=%s not reproduced", at.key, at.val))
		}
	}
	if ex.Left == "" || ex.Right == "" {
		status = model.Partial
		notes = append(notes, "an empty boundary means 'from start'/'to end' in LoadRunner; verify the extractor")
	}
	im.register(f, c, ex, status, strings.Join(notes, "; "), "LB="+lb.val+" RB="+rb.val)
}

func (im *importer) paramName(a *lrArgs) string {
	return strings.TrimSpace(a.val("ParamName"))
}

func (im *importer) searchFilters(c *cCall, a *lrArgs, loc string, ex *model.Extractor, notes *[]string, status *string) {
	scope, _ := filterVal(a.filters, "Scope")
	switch strings.ToLower(scope) {
	case "", "body", "all":
	case "headers":
		hn, _ := filterVal(a.filters, "HeaderNames")
		if ex.Type == model.ExBoundary && hn != "" && !strings.Contains(hn, ",") && ex.Left == "" && ex.Right == "" {
			ex.Type, ex.Expr = model.ExHeader, hn
			return
		}
		*status = model.Partial
		*notes = append(*notes, "Scope=Headers: Load Meter applies this extractor to the response body; consider a header extractor")
	case "cookies":
		*status = model.Partial
		*notes = append(*notes, "Scope=Cookies: consider a cookie extractor")
	default:
		*status = model.Partial
		*notes = append(*notes, fmt.Sprintf("Scope=%s not reproduced", scope))
	}
	for _, ft := range a.filters {
		switch strings.ToLower(ft.key) {
		case "scope", "headernames":
		case "requesturl", "contenttype":
			*status = model.Partial
			*notes = append(*notes, fmt.Sprintf("filter %s=%s dropped: the extractor applies to the next request only", ft.key, ft.val))
		default:
			im.ignoredOnce("filter:"+strings.ToLower(ft.key), c.name+" SEARCH_FILTERS "+ft.key+"=", "", loc, "search filter not applicable")
		}
	}
}

func (im *importer) regSaveParamEx(f *cFile, c *cCall) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, false)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	name := im.paramName(&a)
	if name == "" {
		im.rep.Add(c.name, "", loc, model.Unsupported, "ParamName= missing")
		return
	}
	ex := model.Extractor{Name: name, Type: model.ExBoundary}
	status, notes := model.Supported, []string{}
	lb, _ := a.get("LB")
	rb, _ := a.get("RB")
	ex.Left, ex.Right = im.sub(lb.val, loc), im.sub(rb.val, loc)
	flagNotes(lb, &notes, &status)
	flagNotes(rb, &notes, &status)
	ex.Required = notFoundRequired(a.val("NotFound"))
	im.applyOrdinal(&ex, a.val("Ordinal"), &notes, &status)
	for _, at := range a.attrs {
		switch strings.ToLower(at.key) {
		case "paramname", "lb", "rb", "ordinal", "notfound":
		case "relframeid", "ignoreredirections":
			im.ignoredOnce("attr:"+strings.ToLower(at.key), c.name+" "+at.key+"=", "", loc, "frame/redirect filters are not applicable")
		default:
			status = model.Partial
			notes = append(notes, fmt.Sprintf("%s=%s not reproduced", at.key, at.val))
		}
	}
	im.searchFilters(c, &a, loc, &ex, &notes, &status)
	if ex.Type == model.ExBoundary && (ex.Left == "" || ex.Right == "") {
		status = model.Partial
		notes = append(notes, "an empty boundary means 'from start'/'to end' in LoadRunner; verify the extractor")
	}
	im.register(f, c, ex, status, strings.Join(notes, "; "), "LB="+lb.val+" RB="+rb.val)
}

func (im *importer) regSaveParamJSON(f *cFile, c *cCall) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, false)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	name := im.paramName(&a)
	q := a.val("QueryString")
	if name == "" || q == "" {
		im.rep.Add(c.name, name, loc, model.Unsupported, "ParamName= and QueryString= are required")
		return
	}
	ex := model.Extractor{Name: name, Type: model.ExJSONPath, Expr: q, Required: notFoundRequired(a.val("NotFound"))}
	status, notes := model.Supported, []string{}
	for _, at := range a.attrs {
		switch strings.ToLower(at.key) {
		case "paramname", "querystring", "notfound":
		case "selectall":
			if strings.EqualFold(at.val, "yes") {
				im.arrays[name] = true
				status = model.Partial
				notes = append(notes, "SelectAll=Yes: only the first match is saved")
			}
		default:
			status = model.Partial
			notes = append(notes, fmt.Sprintf("%s=%s not reproduced", at.key, at.val))
		}
	}
	im.searchFilters(c, &a, loc, &ex, &notes, &status)
	im.register(f, c, ex, status, strings.Join(notes, "; "), q)
}

func (im *importer) regSaveParamRegexp(f *cFile, c *cCall) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, false)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	name := im.paramName(&a)
	re := a.val("RegExp")
	if name == "" || re == "" {
		im.rep.Add(c.name, name, loc, model.Unsupported, "ParamName= and RegExp= are required")
		return
	}
	ex := model.Extractor{Name: name, Type: model.ExRegex, Expr: re, Required: notFoundRequired(a.val("NotFound"))}
	status, notes := model.Supported, []string{}
	if _, err := regexp.Compile(re); err != nil {
		status = model.Partial
		notes = append(notes, "expression is not valid RE2 syntax (Go regexp): "+err.Error())
	}
	if g := a.val("Group"); g != "" {
		n, err := strconv.Atoi(g)
		if err != nil || n < 0 {
			status = model.Partial
			notes = append(notes, "Group="+g+" not understood")
		} else {
			ex.Group = n
			if n == 0 {
				status = model.Partial
				notes = append(notes, "Group=0 (whole match) cannot be distinguished from the default in Load Meter")
			}
		}
	}
	im.applyOrdinal(&ex, a.val("Ordinal"), &notes, &status)
	for _, at := range a.attrs {
		switch strings.ToLower(at.key) {
		case "paramname", "regexp", "notfound", "group", "ordinal":
		default:
			status = model.Partial
			notes = append(notes, fmt.Sprintf("%s=%s not reproduced", at.key, at.val))
		}
	}
	im.searchFilters(c, &a, loc, &ex, &notes, &status)
	im.register(f, c, ex, status, strings.Join(notes, "; "), re)
}

func (im *importer) regSaveParamXPath(f *cFile, c *cCall) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, false)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	name := im.paramName(&a)
	q := a.val("QueryString")
	if name == "" || q == "" {
		im.rep.Add(c.name, name, loc, model.Unsupported, "ParamName= and QueryString= are required")
		return
	}
	ex := model.Extractor{Name: name, Type: model.ExXPath, Expr: q, Required: notFoundRequired(a.val("NotFound"))}
	notes := []string{"LoadRunner evaluates XPath on HTML/XML with its own engine; Load Meter supports a subset of XPath on well-formed XML — verify the expression"}
	status := model.Partial
	im.applyOrdinal(&ex, a.val("Ordinal"), &notes, &status)
	im.searchFilters(c, &a, loc, &ex, &notes, &status)
	im.register(f, c, ex, status, strings.Join(notes, "; "), q)
}

func (im *importer) regFind(f *cFile, c *cCall) {
	loc := im.loc(f, c.line)
	a := parseLRArgs(c.args, false)
	if !im.attrsCheck(c, &a, loc) {
		return
	}
	status, notes := model.Supported, []string{}
	text, hasText := a.get("Text")
	pfx, hasPfx := a.get("TextPfx")
	sfx, hasSfx := a.get("TextSfx")
	fail := strings.ToLower(a.val("Fail"))
	_, hasFail := a.get("Fail")
	found := fail == "found"
	var as *model.Assertion
	switch {
	case hasText && text.flags == "" && text.val != "":
		t := im.sub(text.val, loc)
		as = &model.Assertion{Type: model.AsContains, Value: t}
		if found {
			as.Type = model.AsNotContains
		}
	case hasText && text.val != "":
		t := im.sub(text.val, loc)
		expr := quoteKeepVars(t)
		if strings.Contains(strings.ToUpper(text.flags), "IC") {
			expr = "(?i)" + expr
		}
		if strings.Contains(strings.ToUpper(text.flags), "BIN") || strings.Contains(strings.ToUpper(text.flags), "DIG") || strings.Contains(strings.ToUpper(text.flags), "ALNUM") {
			status = model.Partial
			notes = append(notes, "Text/"+text.flags+" wildcards are not reproduced")
		}
		if found {
			status = model.Partial
			notes = append(notes, "Fail=Found with qualifiers has no regex negation; check imported as not_contains of the literal text")
			as = &model.Assertion{Type: model.AsNotContains, Value: t}
		} else {
			as = &model.Assertion{Type: model.AsRegex, Expr: expr}
		}
	case hasPfx && hasSfx:
		expr := quoteKeepVars(im.sub(pfx.val, loc)) + "(?s:.*?)" + quoteKeepVars(im.sub(sfx.val, loc))
		if found {
			status = model.Unsupported
			notes = append(notes, "Fail=Found with TextPfx/TextSfx cannot be expressed (no negative regex assertion)")
		} else {
			as = &model.Assertion{Type: model.AsRegex, Expr: expr}
			status = model.Partial
			notes = append(notes, "TextPfx/TextSfx converted to a regex assertion")
		}
	default:
		im.rep.Add(c.name, "", loc, model.Unsupported, "web_reg_find needs Text= or TextPfx=/TextSfx=")
		return
	}
	if sc, ok := a.get("SaveCount"); ok {
		im.known[sc.val] = true
		status = model.Partial
		notes = append(notes, fmt.Sprintf("SaveCount=%s: match count is not saved (parameter will be undefined)", sc.val))
		if !hasFail {
			as = nil
			notes = append(notes, "without Fail= LoadRunner does not fail the step, so no assertion was added")
		}
	}
	if s := strings.ToLower(a.val("Search")); s == "headers" {
		status = model.Partial
		notes = append(notes, "Search=Headers: Load Meter body assertions do not search headers")
	}
	for _, at := range a.attrs {
		switch strings.ToLower(at.key) {
		case "text", "textpfx", "textsfx", "fail", "savecount", "search":
		case "id", "relframeid":
			im.ignoredOnce("attr:find:"+strings.ToLower(at.key), "web_reg_find "+at.key+"=", "", loc, "log/frame metadata")
		default:
			status = model.Partial
			notes = append(notes, fmt.Sprintf("%s=%s not reproduced", at.key, at.val))
		}
	}
	if as != nil {
		im.pendAs = append(im.pendAs, *as)
		im.pendLoc = append(im.pendLoc, loc)
	}
	note := "assertion on the next request"
	if as != nil {
		note = as.Type + " " + note
	}
	if len(notes) > 0 {
		note += "; " + strings.Join(notes, "; ")
	}
	name := text.val
	if name == "" {
		name = pfx.val + "..." + sfx.val
	}
	im.rep.Add(c.name, name, loc, status, note)
}

// --- settings ---------------------------------------------------------------

func (im *importer) setTimeout(c *cCall, loc string) {
	if len(c.args) < 2 {
		im.unsupportedArgs(c, loc)
		return
	}
	kind := strings.ToUpper(strings.Trim(c.args[0].text, `"`))
	vs, _, ok := strArg(c.args[1])
	if !ok && c.args[1].kind == aNumber {
		vs, ok = c.args[1].text, true
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(vs), 64)
	if !ok || err != nil || v <= 0 {
		im.unsupportedArgs(c, loc)
		return
	}
	d := secs(v)
	if d > im.sc.Defaults.Timeout {
		im.sc.Defaults.Timeout = d
	}
	st, note := model.Supported, "defaults.timeout = "+time.Duration(im.sc.Defaults.Timeout).String()
	if kind != "STEP" && kind != "RECEIVE" {
		st, note = model.Partial, note+fmt.Sprintf("; Load Meter has a single request timeout (%s timeout folded in, largest value kept)", kind)
	}
	im.rep.Add(c.name, kind, loc, st, note)
}

func (im *importer) setUser(c *cCall, loc string) {
	var vals []string
	for _, a := range c.args {
		if a.kind == aIdent && a.text == "LAST" {
			break
		}
		s, eval, ok := strArg(a)
		if !ok {
			im.unsupportedArgs(c, loc)
			return
		}
		if eval {
			s = im.lb + strings.Trim(s, im.lb+im.rb) + im.rb
		}
		vals = append(vals, s)
	}
	if len(vals) < 2 {
		im.unsupportedArgs(c, loc)
		return
	}
	user, pass := vals[0], vals[1]
	host := ""
	if len(vals) > 2 {
		host = vals[2]
	}
	if strings.Contains(user+pass, im.lb) {
		im.rep.Add(c.name, user, loc, model.Unsupported, "credentials use parameters; Load Meter cannot base64-encode at run time")
		im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("%s: add an Authorization header for %s (web_set_user with parameters)", loc, host))
		return
	}
	tok := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	im.sc.Defaults.Headers = removeHeader(im.sc.Defaults.Headers, "Authorization")
	im.sc.Defaults.Headers = append(im.sc.Defaults.Headers, model.Header{Name: "Authorization", Value: "Basic " + tok})
	im.rep.Add(c.name, user, loc, model.Partial,
		fmt.Sprintf("converted to a preemptive Basic Authorization default header for all hosts (LoadRunner negotiates Basic/NTLM/Kerberos for %s only)", host))
}
