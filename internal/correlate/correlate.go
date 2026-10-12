// Package correlate finds dynamic values that flow from earlier responses into
// later requests and proposes deterministic extraction + substitution rules.
//
// It never treats a string match alone as proof: every rule is checked against
// the recording (the extractor must reproduce the recorded value) and can be
// validated by replaying the scenario (see Validate).
package correlate

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/lelanga/loadmeter/internal/extract"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/redact"
)

type Exchange struct {
	Step        string         `json:"step"`
	Method      string         `json:"method"`
	URL         string         `json:"url"`
	ReqHeaders  []model.Header `json:"requestHeaders,omitempty"`
	ReqBody     string         `json:"requestBody,omitempty"`
	Status      int            `json:"status"`
	RespHeaders []model.Header `json:"responseHeaders,omitempty"`
	RespBody    string         `json:"responseBody,omitempty"`
}

// Recording is raw captured traffic. It may contain credentials: store it with
// owner-only permissions and redact before display or AI use.
type Recording struct {
	Source    string     `json:"source"`
	Exchanges []Exchange `json:"exchanges"`
}

func SaveRecording(path string, r *Recording) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func LoadRecording(path string) (*Recording, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Recording
	return &r, json.Unmarshal(b, &r)
}

type Location struct {
	Step     string `json:"step"`
	Field    string `json:"field"`              // header:Name | url | form:name | body | multipart:name
	Template string `json:"template,omitempty"` // recorded field value with the variable substituted
}

type Source struct {
	Step      string          `json:"step"`
	Location  string          `json:"location"` // e.g. "json $.token", "header X-CSRF-Token", "html hidden input", "cookie SESSIONID"
	Extractor model.Extractor `json:"extractor"`
}

type Explanation struct {
	What         string `json:"what"`
	Why          string `json:"why"`
	Risk         string `json:"risk"`
	HowValidated string `json:"howValidated"`
	ManualReview bool   `json:"manualReview"`
}

type Candidate struct {
	ID           string      `json:"id"`
	Variable     string      `json:"variable"`
	Value        string      `json:"-"`
	Masked       string      `json:"value"`
	Source       Source      `json:"source"`
	Destinations []Location  `json:"destinations"`
	Evidence     []string    `json:"evidence"`
	Confidence   float64     `json:"confidence"`
	Security     string      `json:"security"` // secret | session | identifier | other
	Replacement  string      `json:"suggestedReplacement"`
	Validation   string      `json:"validation"`
	Explanation  Explanation `json:"explanation"`
}

// Unexplained is a request value that looks generated but was not found in any earlier response.
type Unexplained struct {
	Step       string `json:"step"`
	Field      string `json:"field"`
	Masked     string `json:"value"`
	Kind       string `json:"kind"` // uuid | timestamp | random-token
	Suggestion string `json:"suggestion"`
}

type Result struct {
	Candidates  []Candidate   `json:"candidates"`
	Unexplained []Unexplained `json:"dynamicUnexplained"`
}

type slot struct {
	field string
	value string
	whole string // the complete field value (for templates)
}

var (
	skipReqHeaders = map[string]bool{"host": true, "cookie": true, "content-length": true, "user-agent": true, "accept": true,
		"accept-language": true, "accept-encoding": true, "referer": true, "origin": true, "connection": true, "content-type": true,
		"cache-control": true, "pragma": true, "if-none-match": true, "if-modified-since": true}
	skipRespHeaders = map[string]bool{"date": true, "content-length": true, "content-type": true, "server": true, "connection": true,
		"cache-control": true, "expires": true, "vary": true, "content-encoding": true, "transfer-encoding": true, "keep-alive": true,
		"last-modified": true, "accept-ranges": true, "age": true, "strict-transport-security": true, "x-content-type-options": true,
		"x-frame-options": true, "content-security-policy": true, "referrer-policy": true, "access-control-allow-origin": true}
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tsRe     = regexp.MustCompile(`^1[5-9]\d{8}(\d{3})?$`)
	tokenRe  = regexp.MustCompile(`^[A-Za-z0-9_\-.~+/=]{16,}$`)
	hintRe   = regexp.MustCompile(`(?i)(token|csrf|xsrf|session|sid|nonce|state|verification|viewstate|ticket|auth|key|code|id$|_id|Id$)`)
	common   = map[string]bool{"true": true, "false": true, "null": true, "none": true, "json": true, "html": true, "text": true, "utf-8": true, "gzip": true, "keep-alive": true, "application/json": true}
	inputRe  = regexp.MustCompile(`(?is)<input[^>]*>`)
	attrName = regexp.MustCompile(`(?i)\bname\s*=\s*["']([^"']+)["']`)
	metaRe   = regexp.MustCompile(`(?is)<meta[^>]*>`)
)

func trivial(v string) bool {
	if len(v) < 4 || common[strings.ToLower(v)] {
		return true
	}
	if isDigits(v) && len(v) < 4 {
		return true
	}
	// plain words (letters only, short) are usually static business data
	if len(v) < 8 && isLetters(v) {
		return true
	}
	return false
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func isLetters(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func requestSlots(ex Exchange) []slot {
	var out []slot
	if u, err := url.Parse(ex.URL); err == nil {
		for _, seg := range strings.Split(u.Path, "/") {
			// Only id-like path segments (with a digit); plain words are routes.
			if s, err := url.PathUnescape(seg); err == nil && strings.ContainsAny(s, "0123456789") {
				out = append(out, slot{"url", s, ""})
			}
		}
		for k, vs := range u.Query() {
			_ = k
			for _, v := range vs {
				out = append(out, slot{"url", v, ""})
			}
		}
	}
	for _, h := range ex.ReqHeaders {
		ln := strings.ToLower(h.Name)
		if skipReqHeaders[ln] || strings.HasPrefix(ln, "sec-") || strings.HasPrefix(ln, ":") {
			continue
		}
		v := h.Value
		if sp := strings.IndexByte(v, ' '); sp > 0 && (strings.EqualFold(v[:sp], "bearer") || strings.EqualFold(v[:sp], "token")) {
			v = v[sp+1:]
		}
		out = append(out, slot{"header:" + h.Name, v, h.Value})
	}
	body := strings.TrimSpace(ex.ReqBody)
	ct := ""
	for _, h := range ex.ReqHeaders {
		if strings.EqualFold(h.Name, "Content-Type") {
			ct = strings.ToLower(h.Value)
		}
	}
	switch {
	case body == "":
	case strings.HasPrefix(body, "{") || strings.HasPrefix(body, "["):
		if doc, err := extract.ParseJSON([]byte(body)); err == nil {
			for _, l := range extract.Leaves(doc) {
				out = append(out, slot{"body", l.Value, ""})
			}
		}
	case strings.Contains(ct, "x-www-form-urlencoded") || (!strings.ContainsAny(body, " <\n") && strings.Contains(body, "=")):
		if q, err := url.ParseQuery(body); err == nil {
			for k, vs := range q {
				for _, v := range vs {
					out = append(out, slot{"form:" + k, v, ""})
				}
			}
		}
	}
	return out
}

type srcHit struct {
	idx        int
	location   string
	extractor  model.Extractor
	key        string // basis for the variable name
	structured bool
}

// findSource locates v in responses before j, preferring structured sources.
func findSource(rec *Recording, j int, v string, cache map[int]any) (srcHit, bool) {
	for k := 0; k < j; k++ {
		ex := rec.Exchanges[k]
		// JSON body
		doc, ok := cache[k]
		if !ok {
			b := strings.TrimSpace(ex.RespBody)
			if strings.HasPrefix(b, "{") || strings.HasPrefix(b, "[") {
				if d, err := extract.ParseJSON([]byte(b)); err == nil {
					doc = d
				}
			}
			cache[k] = doc
		}
		if doc != nil {
			for _, l := range extract.Leaves(doc) {
				if l.Value == v {
					return srcHit{k, "json " + l.Path, model.Extractor{Type: model.ExJSONPath, Expr: l.Path}, lastKey(l.Path), true}, true
				}
			}
		}
		// Response headers (exact)
		for _, h := range ex.RespHeaders {
			ln := strings.ToLower(h.Name)
			if ln == "set-cookie" {
				name, rest, _ := strings.Cut(h.Value, "=")
				val, _, _ := strings.Cut(rest, ";")
				if val == v {
					return srcHit{k, "cookie " + name, model.Extractor{Type: model.ExCookie, Expr: strings.TrimSpace(name)}, name, true}, true
				}
				continue
			}
			if skipRespHeaders[ln] {
				continue
			}
			if h.Value == v {
				return srcHit{k, "header " + h.Name, model.Extractor{Type: model.ExHeader, Expr: h.Name}, h.Name, true}, true
			}
		}
		// HTML hidden inputs / meta tags, then generic boundary.
		if len(v) >= 6 && strings.Contains(ex.RespBody, v) {
			key := "value"
			for _, tag := range append(inputRe.FindAllString(ex.RespBody, -1), metaRe.FindAllString(ex.RespBody, -1)...) {
				if strings.Contains(tag, v) {
					if m := attrName.FindStringSubmatch(tag); m != nil {
						key = m[1]
					}
					break
				}
			}
			if e, ok := regexExtractor(ex.RespBody, v); ok {
				loc := "body text"
				structured := false
				if key != "value" {
					loc, structured = "html "+key, true
				}
				return srcHit{k, loc, e, key, structured}, true
			}
		}
	}
	return srcHit{}, false
}

func lastKey(p string) string {
	p = strings.TrimSuffix(p, "']")
	if i := strings.LastIndexAny(p, ".['"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// regexExtractor builds a left-boundary regex that reproduces v from body.
func regexExtractor(body, v string) (model.Extractor, bool) {
	pos := strings.Index(body, v)
	if pos < 0 {
		return model.Extractor{}, false
	}
	class := `A-Za-z0-9`
	for _, c := range "-_.~+/=:%" {
		if strings.ContainsRune(v, c) {
			class += regexp.QuoteMeta(string(c))
		}
	}
	class = "[" + strings.ReplaceAll(class, `\-`, `\-`) + "]+"
	for _, n := range []int{12, 20, 32, 48} {
		start := max(pos-n, 0)
		lb := body[start:pos]
		if nl := strings.LastIndexAny(lb, "\n\r"); nl >= 0 {
			lb = lb[nl+1:]
		}
		if lb == "" {
			continue
		}
		expr := regexp.QuoteMeta(lb) + "(" + class + ")"
		ms, err := extract.Regex(body, expr, 1)
		if err != nil {
			continue
		}
		for i, m := range ms {
			if m == v {
				match := 0
				if i > 0 {
					match = i + 1
				}
				return model.Extractor{Type: model.ExRegex, Expr: expr, Group: 1, Match: match}, true
			}
		}
	}
	return model.Extractor{}, false
}

func varName(key string, used map[string]bool) string {
	b := strings.Builder{}
	upNext := false
	for i, r := range strings.TrimLeft(key, "_-$") {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			if upNext && b.Len() > 0 {
				b.WriteString(strings.ToUpper(string(r)))
			} else if i == 0 || b.Len() == 0 {
				b.WriteString(strings.ToLower(string(r)))
			} else {
				b.WriteRune(r)
			}
			upNext = false
		default:
			upNext = true
		}
	}
	n := b.String()
	if n == "" || n[0] >= '0' && n[0] <= '9' {
		n = "v" + n
	}
	n = "c_" + n
	base := n
	for i := 2; used[n]; i++ {
		n = fmt.Sprintf("%s_%d", base, i)
	}
	used[n] = true
	return n
}

func classify(key, location string) string {
	k := strings.ToLower(key + " " + location)
	switch {
	case strings.Contains(k, "auth") || strings.Contains(k, "bearer") || strings.Contains(k, "access") || strings.Contains(k, "refresh") || strings.Contains(k, "jwt") || (strings.Contains(k, "token") && !strings.Contains(k, "csrf") && !strings.Contains(k, "verification")):
		return "secret"
	case strings.Contains(k, "session") || strings.Contains(k, "csrf") || strings.Contains(k, "xsrf") || strings.Contains(k, "nonce") || strings.Contains(k, "verification") || strings.Contains(k, "state") || strings.Contains(k, "cookie"):
		return "session"
	case strings.Contains(k, "id"):
		return "identifier"
	}
	return "other"
}

func entropy(s string) float64 {
	f := map[rune]float64{}
	for _, c := range s {
		f[c]++
	}
	h := 0.0
	n := float64(len([]rune(s)))
	for _, c := range f {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// Detect analyses a recording. second is an optional second capture of the same
// flow: values that differ between captures are strong evidence of dynamism.
func Detect(rec *Recording, second *Recording) *Result {
	res := &Result{}
	cache := map[int]any{}
	byValue := map[string]*Candidate{}
	var order []string
	used := map[string]bool{}
	for j, ex := range rec.Exchanges {
		for _, s := range requestSlots(ex) {
			v := s.value
			if trivial(v) {
				continue
			}
			hit, ok := findSource(rec, j, v, cache)
			if !ok {
				if kind, sug := dynamicKind(v); kind != "" {
					res.Unexplained = append(res.Unexplained, Unexplained{Step: ex.Step, Field: s.field, Masked: redact.Mask(v), Kind: kind, Suggestion: sug})
				}
				continue
			}
			c := byValue[v]
			if c == nil {
				name := varName(hit.key, used)
				ext := hit.extractor
				ext.Name = name
				ext.Required = true
				c = &Candidate{Variable: name, Value: v, Masked: redact.Mask(v), Validation: "not-run",
					Source: Source{Step: rec.Exchanges[hit.idx].Step, Location: hit.location, Extractor: ext}}
				c.Security = classify(hit.key, hit.location)
				score := 0.4
				if hit.structured {
					score += 0.2
					c.Evidence = append(c.Evidence, "Found in a structured location ("+hit.location+") of the response to "+c.Source.Step+".")
				} else {
					score -= 0.1
					c.Evidence = append(c.Evidence, "Found as text in the response body of "+c.Source.Step+" (boundary/regex extraction; less robust).")
				}
				if hintRe.MatchString(hit.key) {
					score += 0.15
					c.Evidence = append(c.Evidence, fmt.Sprintf("The field name %q suggests a server-generated value.", hit.key))
				}
				if len(v) >= 16 && entropy(v) > 3.5 {
					score += 0.1
					c.Evidence = append(c.Evidence, fmt.Sprintf("High-entropy value (%d chars).", len(v)))
				}
				if len(v) < 6 {
					score -= 0.2
					c.Evidence = append(c.Evidence, "Short value: higher chance of a coincidental match.")
				}
				if verifyExtractor(rec.Exchanges[hit.idx], ext, v) {
					c.Evidence = append(c.Evidence, "The proposed extractor reproduces the recorded value from the recorded response.")
				} else {
					score -= 0.3
					c.Evidence = append(c.Evidence, "WARNING: the proposed extractor did not reproduce the recorded value; review manually.")
				}
				if second != nil {
					if d, ok := differs(second, c.Source.Step, ext, v); ok {
						if d {
							score += 0.2
							c.Evidence = append(c.Evidence, "The value differs in the second recording: it is dynamic.")
						} else {
							score -= 0.3
							c.Evidence = append(c.Evidence, "The value is identical in the second recording: it may be static data (consider parameterization instead).")
						}
					}
				}
				c.Confidence = math.Round(math.Max(0, math.Min(1, score))*100) / 100
				byValue[v] = c
				order = append(order, v)
			}
			loc := Location{Step: ex.Step, Field: s.field}
			if strings.HasPrefix(s.field, "header:") && s.whole != "" {
				loc.Template = strings.ReplaceAll(s.whole, v, "${"+c.Variable+"}")
			}
			if !hasLoc(c.Destinations, loc) {
				c.Destinations = append(c.Destinations, loc)
			}
		}
	}
	for _, v := range order {
		c := byValue[v]
		if len(c.Destinations) > 1 {
			c.Confidence = math.Min(1, math.Round((c.Confidence+0.1)*100)/100)
			c.Evidence = append(c.Evidence, fmt.Sprintf("Used by %d later request fields.", len(c.Destinations)))
		}
		h := sha1.Sum([]byte(c.Source.Step + "|" + c.Source.Location + "|" + v))
		c.ID = hex.EncodeToString(h[:])[:10]
		c.Replacement = "${" + c.Variable + "}"
		dests := make([]string, 0, len(c.Destinations))
		for _, d := range c.Destinations {
			dests = append(dests, d.Step+" ("+d.Field+")")
		}
		c.Explanation = Explanation{
			What:         fmt.Sprintf("Extract %s from %s (%s) into ${%s} and use it in: %s.", c.Masked, c.Source.Step, c.Source.Location, c.Variable, strings.Join(dests, "; ")),
			Why:          "The recorded value was returned by the server and later sent back by the client. Replaying the recorded literal would reuse a stale value.",
			Risk:         riskText(c),
			HowValidated: "Checked against the recording now; replay validation runs when you apply with validation enabled.",
			ManualReview: c.Confidence < 0.7 || c.Security == "secret",
		}
		res.Candidates = append(res.Candidates, *c)
	}
	sort.SliceStable(res.Candidates, func(i, j int) bool { return res.Candidates[i].Confidence > res.Candidates[j].Confidence })
	return res
}

func riskText(c *Candidate) string {
	switch {
	case c.Confidence < 0.5:
		return "Low confidence: this may be static data that coincidentally matches. Applying it could make the script fail if the server stops returning it."
	case c.Source.Extractor.Type == model.ExRegex:
		return "Text-boundary extraction can break if the page layout changes; the extraction is marked required so failures are visible."
	case c.Security == "secret":
		return "This is a credential or token. It is kept in memory only during the run and masked in reports."
	}
	return "If the server changes the field name or location, extraction fails and the request is reported as an error (not silently ignored)."
}

func hasLoc(ls []Location, l Location) bool {
	for _, x := range ls {
		if x.Step == l.Step && x.Field == l.Field {
			return true
		}
	}
	return false
}

func dynamicKind(v string) (string, string) {
	switch {
	case uuidRe.MatchString(v):
		return "uuid", "Client-generated UUID: replace with ${__uuid()}"
	case tsRe.MatchString(v):
		return "timestamp", "Looks like an epoch timestamp: replace with ${__timestamp()}"
	case tokenRe.MatchString(v) && entropy(v) > 3.8 && len(v) >= 20:
		return "random-token", "High-entropy value not returned by any earlier response: it may be client-generated or come from an excluded request (static/third-party). Review manually."
	}
	return "", ""
}

func verifyExtractor(ex Exchange, e model.Extractor, want string) bool {
	switch e.Type {
	case model.ExJSONPath:
		doc, err := extract.ParseJSON([]byte(ex.RespBody))
		if err != nil {
			return false
		}
		vals, err := extract.JSONPath(doc, e.Expr)
		return err == nil && len(vals) > 0 && extract.Stringify(vals[0]) == want
	case model.ExRegex:
		ms, err := extract.Regex(ex.RespBody, e.Expr, e.Group)
		if err != nil {
			return false
		}
		v, ok := extract.Pick(ms, e.Match, func(int) int { return 0 })
		return ok && v == want
	case model.ExHeader:
		return model.HeaderValue(ex.RespHeaders, e.Expr) == want
	case model.ExCookie:
		for _, h := range ex.RespHeaders {
			if strings.EqualFold(h.Name, "Set-Cookie") {
				n, rest, _ := strings.Cut(h.Value, "=")
				val, _, _ := strings.Cut(rest, ";")
				if strings.TrimSpace(n) == e.Expr && val == want {
					return true
				}
			}
		}
	}
	return false
}

// differs runs the extractor on the second recording's matching step.
func differs(second *Recording, step string, e model.Extractor, v string) (bool, bool) {
	for _, ex := range second.Exchanges {
		if ex.Step != step {
			continue
		}
		var got string
		switch e.Type {
		case model.ExJSONPath:
			doc, err := extract.ParseJSON([]byte(ex.RespBody))
			if err != nil {
				return false, false
			}
			vals, _ := extract.JSONPath(doc, e.Expr)
			if len(vals) == 0 {
				return false, false
			}
			got = extract.Stringify(vals[0])
		case model.ExRegex:
			ms, _ := extract.Regex(ex.RespBody, e.Expr, e.Group)
			if len(ms) == 0 {
				return false, false
			}
			got = ms[0]
		case model.ExHeader:
			got = model.HeaderValue(ex.RespHeaders, e.Expr)
		default:
			return false, false
		}
		return got != v, true
	}
	return false, false
}

// Apply adds extractors and substitutions for the selected candidates. It
// returns the IDs actually applied and per-candidate notes.
func Apply(sc *model.Scenario, cands []Candidate, ids map[string]bool) (applied []string, notes []string) {
	steps := map[string]*model.Step{}
	for _, st := range sc.AllRequests() {
		steps[st.Name] = st
	}
	for _, c := range cands {
		if !ids[c.ID] {
			continue
		}
		src := steps[c.Source.Step]
		if src == nil {
			notes = append(notes, fmt.Sprintf("%s: source step %q not found in scenario", c.Variable, c.Source.Step))
			continue
		}
		exists := false
		for _, e := range src.Request.Extract {
			if e.Name == c.Variable {
				exists = true
			}
		}
		if !exists {
			src.Request.Extract = append(src.Request.Extract, c.Source.Extractor)
		}
		n := 0
		for _, d := range c.Destinations {
			st := steps[d.Step]
			if st == nil {
				notes = append(notes, fmt.Sprintf("%s: destination %q not found", c.Variable, d.Step))
				continue
			}
			if replaceIn(st.Request, d, c.Value, "${"+c.Variable+"}") {
				n++
			} else {
				notes = append(notes, fmt.Sprintf("%s: value not found in %s (%s); it may already be parameterized", c.Variable, d.Step, d.Field))
			}
		}
		if n > 0 {
			applied = append(applied, c.ID)
		}
	}
	return applied, notes
}

func replaceAllForms(s, v, repl string) (string, bool) {
	out := s
	for _, form := range []string{v, url.QueryEscape(v), url.PathEscape(v), jsonEscape(v)} {
		if form != "" && strings.Contains(out, form) {
			out = strings.ReplaceAll(out, form, repl)
		}
	}
	return out, out != s
}

func jsonEscape(v string) string {
	b, _ := json.Marshal(v)
	return string(b[1 : len(b)-1])
}

func replaceIn(r *model.Request, d Location, v, repl string) bool {
	kind, name, _ := strings.Cut(d.Field, ":")
	changed := false
	switch kind {
	case "header":
		for i := range r.Headers {
			if !strings.EqualFold(r.Headers[i].Name, name) {
				continue
			}
			if nv, ok := replaceAllForms(r.Headers[i].Value, v, repl); ok {
				r.Headers[i].Value, changed = nv, true
			} else if d.Template != "" && strings.Contains(r.Headers[i].Value, "${secret_") {
				r.Headers[i].Value, changed = d.Template, true
			}
		}
	case "url":
		r.URL, changed = replaceAllForms(r.URL, v, repl)
	case "form":
		for i := range r.Form {
			if r.Form[i].Name == name {
				if nv, ok := replaceAllForms(r.Form[i].Value, v, repl); ok {
					r.Form[i].Value, changed = nv, true
				}
			}
		}
	case "body":
		r.Body, changed = replaceAllForms(r.Body, v, repl)
		if !changed {
			for i := range r.Form {
				if nv, ok := replaceAllForms(r.Form[i].Value, v, repl); ok {
					r.Form[i].Value, changed = nv, true
				}
			}
		}
	}
	return changed
}
