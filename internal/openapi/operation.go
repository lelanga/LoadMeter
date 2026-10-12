package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

type param struct {
	name, in string
	m        map[string]any
}

// collectParams merges path-level and operation-level parameters (operation wins).
func (im *importer) collectParams(item, op map[string]any, g *gen) []param {
	idx := map[string]int{}
	var out []param
	for _, src := range []any{item["parameters"], op["parameters"]} {
		arr, _ := src.([]any)
		for _, raw := range arr {
			v, err := im.deref(raw)
			m := asMap(v)
			if err != nil || m == nil {
				g.note("parameter skipped: " + errString(err))
				g.unsup = true
				continue
			}
			n, _ := m["name"].(string)
			in, _ := m["in"].(string)
			k := in + ":" + n
			if i, ok := idx[k]; ok {
				out[i] = param{n, in, m}
			} else {
				idx[k] = len(out)
				out = append(out, param{n, in, m})
			}
		}
	}
	return out
}

func (im *importer) paramValue(p param, g *gen) string {
	if v, ok := firstExample(p.m); ok {
		return scalarString(v)
	}
	if sch, ok := p.m["schema"]; ok {
		v := g.schema(sch, map[string]bool{}, 0, false)
		return scalarString(v)
	}
	return "1"
}

// escQuery percent-encodes s but leaves ${...} expressions intact.
func escQuery(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(url.QueryEscape(s))
			return b.String()
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			b.WriteString(url.QueryEscape(s))
			return b.String()
		}
		b.WriteString(url.QueryEscape(s[:i]))
		b.WriteString(s[i : i+j+1])
		s = s[i+j+1:]
	}
}

var pathVar = regexp.MustCompile(`\{([^{}]+)\}`)

func jsonString(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

func (im *importer) operation(path, method string, op, item map[string]any, label string) (*model.Step, string, []string) {
	g := &gen{im: im}
	req := &model.Request{Method: strings.ToUpper(method)}
	params := im.collectParams(item, op, g)

	// Path parameters -> ${var} seeded from examples/defaults.
	pm := map[string]param{}
	for _, p := range params {
		if p.in == "path" {
			pm[p.name] = p
		}
	}
	urlPath := pathVar.ReplaceAllStringFunc(path, func(m string) string {
		n := m[1 : len(m)-1]
		val := "1"
		if p, ok := pm[n]; ok {
			val = url.PathEscape(im.paramValue(p, g))
		} else {
			g.note("path parameter {" + n + "} is not declared; seeded with 1")
		}
		return "${" + im.setVar(n, val) + "}"
	})

	var query []string
	var cookies []string
	for _, p := range params {
		switch p.in {
		case "query":
			required, _ := p.m["required"].(bool)
			_, hasEx := firstExample(p.m)
			if sm := asMap(p.m["schema"]); !required && !hasEx && (sm == nil || sm["default"] == nil) {
				continue
			}
			query = append(query, url.QueryEscape(p.name)+"="+escQuery(im.paramValue(p, g)))
		case "header":
			switch strings.ToLower(p.name) {
			case "accept", "content-type", "authorization":
				g.note("header parameter " + p.name + " ignored (managed by Load Meter/security)")
				continue
			}
			required, _ := p.m["required"].(bool)
			if _, hasEx := firstExample(p.m); required || hasEx {
				req.Headers = append(req.Headers, model.Header{Name: p.name, Value: im.paramValue(p, g)})
			}
		case "cookie":
			if required, _ := p.m["required"].(bool); required {
				cookies = append(cookies, p.name+"="+im.paramValue(p, g))
			}
		}
	}
	if len(cookies) > 0 {
		req.Headers = append(req.Headers, model.Header{Name: "Cookie", Value: strings.Join(cookies, "; ")})
	}

	im.requestBody(op, req, g)
	im.security(op, req, &query, g)

	req.URL = urlPath
	if len(query) > 0 {
		req.URL += "?" + strings.Join(query, "&")
	}
	if s, ok := op["servers"]; ok && s != nil {
		g.note("operation-level servers ignored")
	}
	if cb := asMap(op["callbacks"]); len(cb) > 0 {
		g.note("callbacks ignored")
	}

	// Status assertion from the first 2xx response.
	code := ""
	if resp := asMap(op["responses"]); resp != nil {
		keys := make([]string, 0, len(resp))
		for k := range resp {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if len(k) == 3 && k[0] == '2' {
				if _, err := strconv.Atoi(k); err == nil {
					code = k
					break
				}
				code = strings.ToLower(k) // 2XX
				break
			}
		}
	}
	if code == "" {
		g.note("no 2xx response declared; asserting 2xx")
		code = "2xx"
	}
	req.Assert = append(req.Assert, model.Assertion{Type: model.AsStatus, Value: code})

	status := model.Supported
	if len(g.notes) > 0 {
		status = model.Partial
	}
	notes := make([]string, 0, len(g.notes))
	for n := range g.notes {
		notes = append(notes, n)
	}
	sort.Strings(notes)
	step := &model.Step{Type: model.StepRequest, Name: label, Request: req}
	if s, _ := op["summary"].(string); s != "" {
		step.Comment = s
	}
	return step, status, notes
}

func (im *importer) requestBody(op map[string]any, req *model.Request, g *gen) {
	raw, ok := op["requestBody"]
	if !ok {
		return
	}
	v, err := im.deref(raw)
	rb := asMap(v)
	if err != nil || rb == nil {
		g.note("requestBody omitted: " + errString(err))
		g.unsup = true
		return
	}
	content := asMap(rb["content"])
	if len(content) == 0 {
		return
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pick := ""
	for _, k := range keys {
		lk := strings.ToLower(k)
		if lk == "application/json" {
			pick = k
			break
		}
	}
	for _, k := range keys {
		if pick == "" && strings.Contains(strings.ToLower(k), "json") {
			pick = k
		}
	}
	if pick == "" {
		pick = keys[0]
	}
	if len(keys) > 1 {
		g.note("multiple request media types; using " + pick)
	}
	media := asMap(content[pick])
	lp := strings.ToLower(pick)
	seen := map[string]bool{}
	var val any
	have := false
	if media != nil {
		if ex, ok := firstExample(media); ok {
			val, have = ex, true
		} else if sch, ok := media["schema"]; ok {
			val, have = g.schema(sch, seen, 0, true), true
			if _, drop := val.(omitT); drop {
				val = map[string]any{}
			}
		}
	}
	switch {
	case strings.Contains(lp, "json"):
		req.Headers = append(req.Headers, model.Header{Name: "Content-Type", Value: "application/json"})
		if have {
			req.Body = jsonString(val)
		} else {
			req.Body = "{}"
			g.note("no schema or example for request body; empty object used")
		}
	case lp == "application/x-www-form-urlencoded":
		if m, ok := val.(map[string]any); ok {
			names := make([]string, 0, len(m))
			for k := range m {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				req.Form = append(req.Form, model.KV{Name: k, Value: scalarString(m[k])})
			}
		} else {
			g.note("form body could not be generated")
		}
	case strings.HasPrefix(lp, "multipart/"):
		if m, ok := val.(map[string]any); ok {
			names := make([]string, 0, len(m))
			for k := range m {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				req.Multipart = append(req.Multipart, model.Part{Name: k, Value: scalarString(m[k])})
			}
		}
		g.note("multipart parts are text placeholders; attach real files manually")
	case strings.HasPrefix(lp, "text/") || lp == "application/xml":
		req.Headers = append(req.Headers, model.Header{Name: "Content-Type", Value: pick})
		if s, ok := val.(string); ok && have {
			req.Body = s
		} else {
			g.note(pick + " body not generated; fill in manually")
		}
		if lp == "application/xml" || lp == "text/xml" {
			g.note("XML body requires manual content")
		}
	default:
		g.note("request media type " + pick + " is not supported; body omitted")
		g.unsup = true
	}
}

// security maps the effective security requirement to headers/query params with
// ${secret_*} placeholders. Real credentials are never read from the document.
func (im *importer) security(op map[string]any, req *model.Request, query *[]string, g *gen) {
	sec, ok := op["security"].([]any)
	if !ok {
		sec, _ = im.doc["security"].([]any)
	}
	if len(sec) == 0 {
		return
	}
	if len(sec) > 1 {
		g.note("multiple security alternatives; first one used")
	}
	reqmt := asMap(sec[0])
	names := make([]string, 0, len(reqmt))
	for n := range reqmt {
		names = append(names, n)
	}
	sort.Strings(names)
	schemes := asMap(asMap(im.doc["components"])["securitySchemes"])
	for _, n := range names {
		sv, err := im.deref(schemes[n])
		s := asMap(sv)
		if err != nil || s == nil {
			g.note("security scheme " + n + " is not defined")
			continue
		}
		typ, _ := s["type"].(string)
		base := "secret_" + strings.ToLower(ident(n))
		switch typ {
		case "http":
			sch, _ := s["scheme"].(string)
			switch strings.ToLower(sch) {
			case "bearer":
				req.Headers = append(req.Headers, model.Header{Name: "Authorization", Value: "Bearer ${" + im.setVar(base, "") + "}"})
			case "basic":
				req.Headers = append(req.Headers, model.Header{Name: "Authorization", Value: "Basic ${" + im.setVar(base, "") + "}"})
				g.note("basic auth: set " + base + " to base64(user:password)")
			default:
				req.Headers = append(req.Headers, model.Header{Name: "Authorization", Value: sch + " ${" + im.setVar(base, "") + "}"})
				g.note("http scheme " + sch + " mapped to a generic Authorization header")
			}
		case "apiKey":
			pn, _ := s["name"].(string)
			in, _ := s["in"].(string)
			v := im.setVar(base, "")
			switch in {
			case "header":
				req.Headers = append(req.Headers, model.Header{Name: pn, Value: "${" + v + "}"})
			case "query":
				*query = append(*query, url.QueryEscape(pn)+"=${"+v+"}")
			case "cookie":
				req.Headers = append(req.Headers, model.Header{Name: "Cookie", Value: pn + "=${" + v + "}"})
			}
		case "oauth2", "openIdConnect":
			req.Headers = append(req.Headers, model.Header{Name: "Authorization", Value: "Bearer ${" + im.setVar(base+"_token", "") + "}"})
			g.note(typ + ": token acquisition is not generated; supply an access token in " + base + "_token")
		default:
			g.note(fmt.Sprintf("security scheme %s (%s) is not supported", n, typ))
			g.unsup = true
		}
	}
}
