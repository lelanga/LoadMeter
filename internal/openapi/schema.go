package openapi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// omitT marks a value dropped because of a cyclic $ref.
type omitT struct{}

// gen builds example values from JSON schemas. Notes collect everything that
// was approximated so the compatibility report can mention it.
type gen struct {
	im    *importer
	notes map[string]bool
	unsup bool
}

func (g *gen) note(s string) {
	if g.notes == nil {
		g.notes = map[string]bool{}
	}
	g.notes[s] = true
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case interface{ Float64() (float64, error) }:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func firstExample(m map[string]any) (any, bool) {
	if v, ok := m["example"]; ok {
		return v, true
	}
	if ex := asMap(m["examples"]); ex != nil { // media type / parameter examples map
		keys := make([]string, 0, len(ex))
		for k := range ex {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if em := asMap(ex[k]); em != nil {
				if v, ok := em["value"]; ok {
					return v, true
				}
			}
		}
	}
	if arr, ok := m["examples"].([]any); ok && len(arr) > 0 { // JSON schema 2020-12 style
		return arr[0], true
	}
	return nil, false
}

// schema returns an example value for s. dyn allows ${__function()} values
// (request bodies); parameters use static values.
func (g *gen) schema(s any, seen map[string]bool, depth int, dyn bool) any {
	if depth > 8 {
		g.note("schema nesting deeper than 8 levels truncated")
		return omitT{}
	}
	m := asMap(s)
	if m == nil {
		return nil
	}
	if ref, ok := m["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			g.note("external $ref " + ref + " is not supported")
			g.unsup = true
			return omitT{}
		}
		if seen[ref] {
			g.note("cyclic $ref " + ref + " truncated")
			return omitT{}
		}
		t, err := g.im.pointer(ref)
		if err != nil {
			g.note(err.Error())
			g.unsup = true
			return omitT{}
		}
		seen[ref] = true
		defer delete(seen, ref)
		return g.schema(t, seen, depth+1, dyn)
	}
	if v, ok := firstExample(m); ok {
		return v
	}
	for _, k := range []string{"default", "const"} {
		if v, ok := m[k]; ok {
			return v
		}
	}
	if e, ok := m["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	if all, ok := m["allOf"].([]any); ok {
		out := map[string]any{}
		for _, p := range all {
			if pm, ok := g.schema(p, seen, depth+1, dyn).(map[string]any); ok {
				for k, v := range pm {
					out[k] = v
				}
			}
		}
		if _, has := m["properties"]; has {
			if pm, ok := g.objectOf(m, seen, depth, dyn).(map[string]any); ok {
				for k, v := range pm {
					out[k] = v
				}
			}
		}
		return out
	}
	for _, k := range []string{"oneOf", "anyOf"} {
		if opts, ok := m[k].([]any); ok && len(opts) > 0 {
			g.note(k + ": first variant used")
			for _, o := range opts {
				if om := asMap(o); om != nil && om["type"] == "null" {
					continue
				}
				return g.schema(o, seen, depth+1, dyn)
			}
		}
	}
	typ := ""
	switch t := m["type"].(type) {
	case string:
		typ = t
	case []any:
		for _, x := range t {
			if s, _ := x.(string); s != "" && s != "null" {
				typ = s
				break
			}
		}
	}
	if typ == "" {
		switch {
		case m["properties"] != nil:
			typ = "object"
		case m["items"] != nil:
			typ = "array"
		}
	}
	switch typ {
	case "object":
		return g.objectOf(m, seen, depth, dyn)
	case "array":
		n := 1
		if mi, ok := toFloat(m["minItems"]); ok && mi > 1 {
			n = int(mi)
			if n > 3 {
				n = 3
			}
		}
		item := g.schema(m["items"], seen, depth+1, dyn)
		out := []any{}
		if _, drop := item.(omitT); !drop {
			for i := 0; i < n; i++ {
				out = append(out, item)
			}
		}
		return out
	case "integer", "number":
		v := 1.0
		if mn, ok := toFloat(m["minimum"]); ok {
			v = mn
			if ex, ok := toFloat(m["exclusiveMinimum"]); ok && ex >= mn {
				v = ex + 1
			} else if b, _ := m["exclusiveMinimum"].(bool); b {
				v = mn + 1
			}
		} else if ex, ok := toFloat(m["exclusiveMinimum"]); ok {
			v = ex + 1
		}
		if mx, ok := toFloat(m["maximum"]); ok && v > mx {
			v = mx
		}
		if typ == "integer" {
			return int64(v)
		}
		return v
	case "boolean":
		return true
	case "string", "":
		if typ == "" {
			return "string"
		}
		return g.str(m, dyn)
	}
	return nil
}

func (g *gen) objectOf(m map[string]any, seen map[string]bool, depth int, dyn bool) any {
	props := asMap(m["properties"])
	req := map[string]bool{}
	var reqList []string
	if r, ok := m["required"].([]any); ok {
		for _, x := range r {
			if s, ok := x.(string); ok {
				req[s] = true
				reqList = append(reqList, s)
			}
		}
	}
	names := []string{}
	if len(req) > 0 {
		names = reqList
	} else {
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		if len(names) > 25 {
			names = names[:25]
			g.note("object has more than 25 properties; first 25 used")
		}
	}
	out := map[string]any{}
	for _, k := range names {
		ps, ok := props[k]
		if !ok {
			out[k] = "string"
			continue
		}
		if pm := asMap(ps); pm != nil && pm["readOnly"] == true {
			continue
		}
		v := g.schema(ps, seen, depth+1, dyn)
		if _, drop := v.(omitT); drop {
			continue
		}
		out[k] = v
	}
	return out
}

func (g *gen) str(m map[string]any, dyn bool) any {
	f, _ := m["format"].(string)
	pick := func(d, s string) string {
		if dyn {
			return d
		}
		return s
	}
	switch f {
	case "uuid":
		return pick("${__uuid()}", "00000000-0000-4000-8000-000000000001")
	case "date-time":
		return pick("${__timestamp(yyyy-MM-dd)}T${__timestamp(HH:mm:ss)}Z", "2024-01-01T00:00:00Z")
	case "date":
		return pick("${__timestamp(yyyy-MM-dd)}", "2024-01-01")
	case "time":
		return "12:00:00"
	case "email":
		return pick("user${__counter()}@example.com", "user@example.com")
	case "uri", "url", "iri":
		return "https://example.com/resource"
	case "hostname":
		return "example.com"
	case "ipv4":
		return "127.0.0.1"
	case "ipv6":
		return "::1"
	case "byte":
		return "c3RyaW5n"
	case "binary":
		g.note("binary string cannot be generated")
		return ""
	case "password":
		return "Passw0rd!"
	}
	if _, ok := m["pattern"]; ok {
		g.note("string pattern ignored; placeholder value used")
	}
	v := "string"
	if mn, ok := toFloat(m["minLength"]); ok && int(mn) > len(v) {
		n := int(mn)
		if n > 1024 {
			n = 1024
		}
		v += strings.Repeat("x", n-len(v))
	}
	if mx, ok := toFloat(m["maxLength"]); ok && int(mx) >= 0 && int(mx) < len(v) {
		v = v[:int(mx)]
	}
	return v
}

// scalarString renders a generated value as a plain string for params.
func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case omitT:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		if len(x) > 0 {
			return scalarString(x[0])
		}
		return ""
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v)
}
