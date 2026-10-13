// Package vars implements ${name} substitution and built-in functions.
//
// Syntax: ${name} reads a variable; ${__fn(arg1,arg2)} calls a built-in.
// Unknown variables are left verbatim (JMeter behaviour) and reported as
// unresolved so the engine and validators can flag them.
//
// Variable values that themselves contain ${...} are expanded again (bounded
// by MaxDepth), so scenario variables can be chained ("base": "https://${host}").
// Values stored with SetRaw (extracted from responses, function results) are
// never re-expanded: text that came from the system under test is data.
//
// Function arguments are split on commas at parenthesis depth 0, outside
// nested ${...}. A backslash escapes the next character for "\,", "\\", "\$",
// "\(" and "\)"; any other backslash is kept as written (so regex arguments
// such as "\d" survive).
package vars

import (
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"sync/atomic"
)

// EnvPrefix restricts ${__env(NAME)} to variables starting with this prefix so an
// imported (untrusted) script cannot read arbitrary secrets from the environment.
const EnvPrefix = "LOADMETER_"

// MaxDepth bounds recursive expansion of variable values (and __eval nesting).
const MaxDepth = 8

type Context struct {
	Vars map[string]string
	VU   int
	Iter int
	Rand *mrand.Rand
	// Counter backs ${__counter()}. The engine points every VU of a run at
	// the run's counter, so the sequence restarts at 1 for each run (it used
	// to be a process-wide global that kept counting across runs in `serve`).
	// When nil, the context lazily uses its own counter.
	Counter *atomic.Int64
	// vuCounter backs ${__counter(TRUE)} (one sequence per VU, JMeter "per user").
	vuCounter int64
	// raw marks variables whose value must not be re-expanded.
	raw map[string]bool
}

func NewContext(vu int, seed uint64) *Context {
	return &Context{Vars: map[string]string{}, VU: vu, Rand: mrand.New(mrand.NewPCG(seed, uint64(vu)+1))}
}

// Set stores a variable whose value may contain ${...} references that are
// expanded when the variable is read.
func (c *Context) Set(name, v string) {
	c.Vars[name] = v
	delete(c.raw, name)
}

// SetRaw stores a variable whose value is used verbatim (never re-expanded).
// Use it for values that came from a response.
func (c *Context) SetRaw(name, v string) {
	c.Vars[name] = v
	if c.raw == nil {
		c.raw = map[string]bool{}
	}
	c.raw[name] = true
}

// Delete removes a variable.
func (c *Context) Delete(name string) {
	delete(c.Vars, name)
	delete(c.raw, name)
}

// Result of an expansion.
type Result struct {
	Used       []string // variable names that were substituted
	Unresolved []string // ${name} references with no value
}

func (r *Result) merge(o Result) {
	r.Used = append(r.Used, o.Used...)
	r.Unresolved = append(r.Unresolved, o.Unresolved...)
}

// Expand substitutes variables and functions in s.
func (c *Context) Expand(s string) (string, Result) {
	return c.expand(s, 0)
}

func (c *Context) expand(s string, depth int) (string, Result) {
	var res Result
	if !strings.Contains(s, "${") {
		return s, res
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		end := matchBrace(s, i+2)
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		expr := s[i+2 : end]
		if strings.HasPrefix(expr, "__") {
			var nested Result
			v, err := c.callInto(expr, depth, &nested)
			res.merge(nested)
			if err != nil {
				b.WriteString(s[i : end+1])
				res.Unresolved = append(res.Unresolved, expr)
			} else {
				b.WriteString(v)
			}
		} else if v, ok := c.lookup(expr, depth, &res); ok {
			b.WriteString(v)
			res.Used = append(res.Used, expr)
		} else {
			b.WriteString(s[i : end+1])
			res.Unresolved = append(res.Unresolved, expr)
		}
		s = s[end+1:]
	}
	return b.String(), res
}

// lookup returns a variable's value, expanding ${...} inside it unless the
// variable is raw or the depth bound is reached.
func (c *Context) lookup(name string, depth int, res *Result) (string, bool) {
	v, ok := c.Vars[name]
	if !ok {
		return "", false
	}
	if depth+1 < MaxDepth && !c.raw[name] && strings.Contains(v, "${") {
		out, r := c.expand(v, depth+1)
		res.merge(r)
		return out, true
	}
	return v, true
}

// matchBrace finds the closing } for an expression starting at i, honouring
// nested ${...} inside function arguments and backslash escapes.
func matchBrace(s string, i int) int {
	depth := 1
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '\\':
			if j+1 < len(s) && (s[j+1] == '}' || s[j+1] == '{') {
				j++
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// splitArgs splits a raw argument list on commas at parenthesis depth 0,
// outside nested ${...}, keeping escapes for unescapeArg.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	paren := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && i+1 < len(s):
			cur.WriteByte(ch)
			cur.WriteByte(s[i+1])
			i++
			continue
		case ch == '$' && i+1 < len(s) && s[i+1] == '{':
			if end := matchBrace(s, i+2); end >= 0 {
				cur.WriteString(s[i : end+1])
				i = end
				continue
			}
		case ch == '(':
			paren++
		case ch == ')':
			if paren > 0 {
				paren--
			}
		case ch == ',' && paren == 0:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	return append(out, cur.String())
}

// dollar stands in for an escaped "\$" while the argument is expanded.
const dollar = ""

// unescapeArg resolves \, \\ \( \) and turns \$ into a placeholder that
// expansion leaves alone; other backslashes are kept.
func unescapeArg(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case ',', '\\', '(', ')', '{', '}':
				b.WriteByte(s[i+1])
				i++
				continue
			case '$':
				b.WriteString(dollar)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (c *Context) callInto(expr string, depth int, nested *Result) (string, error) {
	name, argStr := expr, ""
	hasParens := false
	if p := strings.IndexByte(expr, '('); p >= 0 {
		if !strings.HasSuffix(expr, ")") {
			return "", fmt.Errorf("malformed function %q", expr)
		}
		name, argStr, hasParens = expr[:p], expr[p+1:len(expr)-1], true
	}
	var args []string
	if hasParens && argStr != "" {
		for _, a := range splitArgs(argStr) {
			v, r := c.expand(unescapeArg(strings.TrimSpace(a)), depth)
			nested.merge(r)
			args = append(args, strings.ReplaceAll(v, dollar, "$"))
		}
	}
	f, ok := registry[name]
	if !ok {
		return "", fmt.Errorf("unknown function %s", name)
	}
	return f(&call{c: c, args: args, depth: depth, res: nested})
}

// EvalCondition evaluates `a == b`, `a != b`, or a single operand (true when
// non-empty and not "false"/"0") after expansion.
func (c *Context) EvalCondition(cond string) bool {
	for _, op := range []string{"==", "!="} {
		if i := strings.Index(cond, op); i >= 0 {
			l, _ := c.Expand(strings.TrimSpace(cond[:i]))
			r, _ := c.Expand(strings.TrimSpace(cond[i+2:]))
			l, r = strings.Trim(l, `"'`), strings.Trim(r, `"'`)
			if op == "==" {
				return l == r
			}
			return l != r
		}
	}
	v, res := c.Expand(strings.TrimSpace(cond))
	if len(res.Unresolved) > 0 {
		return false
	}
	v = strings.TrimSpace(v)
	return v != "" && v != "false" && v != "0"
}
