// Package extract implements deterministic value extraction from responses:
// JSONPath (subset), XPath (subset), regular expressions and LB/RB boundaries.
package extract

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------- JSON ----------

// ParseJSON decodes with UseNumber so large IDs keep their exact text.
func ParseJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

type pathTok struct {
	key       string
	index     int
	wildcard  bool
	recursive bool
	isIndex   bool
}

func parsePath(p string) ([]pathTok, error) {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "$") {
		return nil, fmt.Errorf("jsonpath must start with $")
	}
	p = p[1:]
	var toks []pathTok
	for len(p) > 0 {
		switch {
		case strings.HasPrefix(p, ".."):
			p = p[2:]
			name := readName(p)
			if name == "" {
				return nil, fmt.Errorf("jsonpath: expected name after ..")
			}
			p = p[len(name):]
			toks = append(toks, pathTok{key: name, recursive: true, wildcard: name == "*"})
		case p[0] == '.':
			p = p[1:]
			name := readName(p)
			if name == "" {
				return nil, fmt.Errorf("jsonpath: expected name after .")
			}
			p = p[len(name):]
			toks = append(toks, pathTok{key: name, wildcard: name == "*"})
		case p[0] == '[':
			end := strings.IndexByte(p, ']')
			if end < 0 {
				return nil, fmt.Errorf("jsonpath: unclosed [")
			}
			in := strings.TrimSpace(p[1:end])
			p = p[end+1:]
			switch {
			case in == "*":
				toks = append(toks, pathTok{wildcard: true})
			case len(in) >= 2 && (in[0] == '\'' || in[0] == '"'):
				toks = append(toks, pathTok{key: in[1 : len(in)-1]})
			default:
				n, err := strconv.Atoi(in)
				if err != nil {
					return nil, fmt.Errorf("jsonpath: unsupported selector [%s] (filters are not supported)", in)
				}
				toks = append(toks, pathTok{index: n, isIndex: true})
			}
		default:
			return nil, fmt.Errorf("jsonpath: unexpected %q", p)
		}
	}
	return toks, nil
}

func readName(p string) string {
	i := 0
	for i < len(p) && p[i] != '.' && p[i] != '[' {
		i++
	}
	return p[:i]
}

// JSONPath evaluates a JSONPath subset ($, .k, ['k'], [n], [-n], [*], ..k) on doc.
func JSONPath(doc any, path string) ([]any, error) {
	toks, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	cur := []any{doc}
	for _, t := range toks {
		var next []any
		for _, n := range cur {
			if t.recursive {
				collectRecursive(n, t, &next)
				continue
			}
			next = append(next, step(n, t)...)
		}
		cur = next
	}
	return cur, nil
}

func step(n any, t pathTok) []any {
	switch v := n.(type) {
	case map[string]any:
		if t.wildcard {
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out := make([]any, 0, len(keys))
			for _, k := range keys {
				out = append(out, v[k])
			}
			return out
		}
		if x, ok := v[t.key]; ok && !t.isIndex {
			return []any{x}
		}
	case []any:
		if t.wildcard {
			return v
		}
		if t.isIndex {
			i := t.index
			if i < 0 {
				i += len(v)
			}
			if i >= 0 && i < len(v) {
				return []any{v[i]}
			}
		}
	}
	return nil
}

func collectRecursive(n any, t pathTok, out *[]any) {
	*out = append(*out, step(n, pathTok{key: t.key, wildcard: t.wildcard})...)
	switch v := n.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectRecursive(v[k], t, out)
		}
	case []any:
		for _, x := range v {
			collectRecursive(x, t, out)
		}
	}
}

// Stringify renders a JSON value as the text a user would substitute.
func Stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// Leaf is a scalar value in a JSON document with its JSONPath.
type Leaf struct {
	Path  string
	Value string
}

// Leaves lists scalar leaves with canonical paths (used for correlation discovery).
func Leaves(doc any) []Leaf {
	var out []Leaf
	var rec func(n any, p string)
	rec = func(n any, p string) {
		switch v := n.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if isIdent(k) {
					rec(v[k], p+"."+k)
				} else {
					rec(v[k], p+"['"+k+"']")
				}
			}
		case []any:
			for i, x := range v {
				rec(x, p+"["+strconv.Itoa(i)+"]")
			}
		default:
			out = append(out, Leaf{Path: p, Value: Stringify(v)})
		}
	}
	rec(doc, "$")
	return out
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// ---------- Regex / boundary ----------

var reCache sync.Map

func compile(expr string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(expr); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	reCache.Store(expr, re)
	return re, nil
}

// Regex returns all matches of group (default: 1 if the pattern has groups, else 0).
func Regex(body, expr string, group int) ([]string, error) {
	re, err := compile(expr)
	if err != nil {
		return nil, err
	}
	if group == 0 && re.NumSubexp() > 0 {
		group = 1
	}
	if group > re.NumSubexp() {
		return nil, fmt.Errorf("regex has %d groups, requested %d", re.NumSubexp(), group)
	}
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, m[group])
	}
	return out, nil
}

func Match(body, expr string) (bool, error) {
	re, err := compile(expr)
	if err != nil {
		return false, err
	}
	return re.MatchString(body), nil
}

// Boundary returns every substring between left and right (LoadRunner LB/RB style).
func Boundary(body, left, right string) []string {
	var out []string
	for {
		i := strings.Index(body, left)
		if i < 0 {
			return out
		}
		body = body[i+len(left):]
		if right == "" {
			return append(out, body)
		}
		j := strings.Index(body, right)
		if j < 0 {
			return out
		}
		out = append(out, body[:j])
		body = body[j+len(right):]
	}
}

// ---------- XML / XPath subset ----------

type Node struct {
	Name     string // local name
	Attrs    map[string]string
	Text     strings.Builder
	Children []*Node
}

// ParseXML builds a tree; DOCTYPE/entities are rejected (XXE-safe by construction).
func ParseXML(b []byte) (*Node, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	root := &Node{Name: "#root"}
	stack := []*Node{root}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, fmt.Errorf("xml directives (DOCTYPE) are not allowed")
		case xml.StartElement:
			n := &Node{Name: t.Name.Local, Attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.Attrs[a.Name.Local] = a.Value
			}
			top := stack[len(stack)-1]
			top.Children = append(top.Children, n)
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			stack[len(stack)-1].Text.Write(t)
		}
	}
	return root, nil
}

// XPath supports: /a/b, //b, a/b[2], trailing /text() and /@attr. Namespaces
// prefixes are ignored (local-name matching).
func XPath(root *Node, expr string) ([]string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty xpath")
	}
	attr := ""
	if i := strings.LastIndex(expr, "/@"); i >= 0 {
		attr, expr = expr[i+2:], expr[:i]
	}
	expr = strings.TrimSuffix(expr, "/text()")
	cur := []*Node{root}
	for expr != "" {
		desc := false
		if strings.HasPrefix(expr, "//") {
			desc, expr = true, expr[2:]
		} else if strings.HasPrefix(expr, "/") {
			expr = expr[1:]
		}
		seg := expr
		if i := strings.IndexByte(expr, '/'); i >= 0 {
			seg, expr = expr[:i], expr[i:]
		} else {
			expr = ""
		}
		idx := 0
		if i := strings.IndexByte(seg, '['); i >= 0 && strings.HasSuffix(seg, "]") {
			n, err := strconv.Atoi(seg[i+1 : len(seg)-1])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("xpath: only numeric predicates are supported: %s", seg)
			}
			idx, seg = n, seg[:i]
		}
		if c := strings.IndexByte(seg, ':'); c >= 0 {
			seg = seg[c+1:]
		}
		var next []*Node
		for _, n := range cur {
			var found []*Node
			if desc {
				descendants(n, seg, &found)
			} else {
				for _, ch := range n.Children {
					if seg == "*" || ch.Name == seg {
						found = append(found, ch)
					}
				}
			}
			if idx > 0 {
				if idx <= len(found) {
					next = append(next, found[idx-1])
				}
			} else {
				next = append(next, found...)
			}
		}
		cur = next
	}
	var out []string
	for _, n := range cur {
		if attr != "" {
			if v, ok := n.Attrs[attr]; ok {
				out = append(out, v)
			}
		} else {
			out = append(out, strings.TrimSpace(n.Text.String()))
		}
	}
	return out, nil
}

func descendants(n *Node, name string, out *[]*Node) {
	for _, ch := range n.Children {
		if name == "*" || ch.Name == name {
			*out = append(*out, ch)
		}
		descendants(ch, name, out)
	}
}

// Pick chooses a match: 0 → first, n>0 → nth (1-based), -1 → random via rnd.
func Pick(matches []string, n int, rnd func(int) int) (string, bool) {
	if len(matches) == 0 {
		return "", false
	}
	switch {
	case n == 0:
		return matches[0], true
	case n < 0:
		return matches[rnd(len(matches))], true
	case n <= len(matches):
		return matches[n-1], true
	}
	return "", false
}
