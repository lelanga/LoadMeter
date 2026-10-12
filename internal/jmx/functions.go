package jmx

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

// alnum is the alphabet Load Meter's __randomString uses; JMeter __RandomString
// calls with exactly this character set map without loss.
const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// fnIssue is a function-mapping problem reported as its own CompatItem.
type fnIssue struct {
	Fn, Status, Note string
}

// ref is one ${...} reference: a variable (${name}) or a function call
// (${__name} / ${__name(arg,...)}).
type ref struct {
	start, end int
	name       string
	args       []string // raw (unconverted) arguments
	parens     bool
}

// parseRef parses the reference starting at s[i:] == "${". It follows JMeter's
// function-parser rules closely enough for mapping: arguments are split on
// unescaped commas at parenthesis depth 0, `\x` escapes are preserved and nested
// ${...} references are kept intact inside arguments.
func parseRef(s string, i int) (ref, bool) {
	j := i + 2
	k := j
	for k < len(s) && s[k] != '(' && s[k] != '}' {
		if s[k] == '$' && k+1 < len(s) && s[k+1] == '{' {
			return ref{}, false
		}
		k++
	}
	if k >= len(s) {
		return ref{}, false
	}
	r := ref{start: i, name: s[j:k]}
	if s[k] == '}' {
		r.end = k + 1
		return r, true
	}
	r.parens = true
	k++
	depth := 0
	var cur strings.Builder
	for k < len(s) {
		c := s[k]
		switch {
		case c == '\\' && k+1 < len(s):
			cur.WriteByte(c)
			cur.WriteByte(s[k+1])
			k += 2
			continue
		case c == '$' && k+1 < len(s) && s[k+1] == '{':
			if sub, ok := parseRef(s, k); ok {
				cur.WriteString(s[k:sub.end])
				k = sub.end
				continue
			}
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				r.args = append(r.args, cur.String())
				k++
				if k < len(s) && s[k] == '}' {
					r.end = k + 1
					if len(r.args) == 1 && strings.TrimSpace(r.args[0]) == "" {
						r.args = nil
					}
					return r, true
				}
				return ref{}, false
			}
			depth--
		case c == ',' && depth == 0:
			r.args = append(r.args, cur.String())
			cur.Reset()
			k++
			continue
		}
		cur.WriteByte(c)
		k++
	}
	return ref{}, false
}

// rewrite replaces every parseable ${...} reference in s with fn's result.
func rewrite(s string, fn func(r ref, raw string) string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			if r, ok := parseRef(s, i); ok {
				b.WriteString(fn(r, s[r.start:r.end]))
				i = r.end
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func unescapeArg(s string) string {
	return strings.NewReplacer(`\,`, ",", `\(`, "(", `\)`, ")", `\\`, `\`).Replace(s)
}

// importFuncs maps JMeter functions in s to Load Meter syntax. ${__P(name,def)}
// becomes ${name} and records def in vars (only when name is not set yet).
func importFuncs(s string, vars map[string]string) (string, []fnIssue) {
	var issues []fnIssue
	add := func(fn, status, format string, a ...any) {
		issues = append(issues, fnIssue{Fn: fn, Status: status, Note: fmt.Sprintf(format, a...)})
	}
	out := rewrite(s, func(r ref, raw string) string {
		if !strings.HasPrefix(r.name, "__") {
			return raw // plain ${variable}
		}
		arg := func(i int) string {
			if i < len(r.args) {
				return strings.TrimSpace(r.args[i])
			}
			return ""
		}
		switch r.name {
		case "__UUID":
			return "${__uuid()}"
		case "__Random":
			if arg(0) == "" || arg(1) == "" {
				add(r.name, model.Unsupported, "%s needs min,max; left as text", raw)
				return raw
			}
			if arg(2) != "" {
				add(r.name, model.Partial, "%s: result variable %q is not stored by Load Meter __randomInt", raw, arg(2))
			}
			return "${__randomInt(" + arg(0) + "," + arg(1) + ")}"
		case "__time":
			out := "${__timestamp()}"
			if f := arg(0); f != "" {
				if strings.HasPrefix(f, "/") {
					add(r.name, model.Unsupported, "%s: divisor form has no Load Meter equivalent; left as text", raw)
					return raw
				}
				out = "${__timestamp(" + f + ")}"
				add(r.name, model.Partial, "%s: Load Meter __timestamp(format) understands only a SimpleDateFormat subset (yyyy MM dd HH mm ss SSS)", raw)
			}
			if arg(1) != "" {
				add(r.name, model.Partial, "%s: result variable %q is not stored", raw, arg(1))
			}
			return out
		case "__counter":
			if !strings.EqualFold(arg(0), "FALSE") {
				add(r.name, model.Partial, "%s: JMeter per-thread counter (TRUE) mapped to Load Meter __counter(), which is global across VUs", raw)
			}
			if arg(1) != "" {
				add(r.name, model.Partial, "%s: result variable %q is not stored", raw, arg(1))
			}
			return "${__counter()}"
		case "__threadNum":
			return "${__vu()}"
		case "__urlencode":
			return "${__urlencode(" + strings.Join(r.args, ",") + ")}"
		case "__RandomString":
			if arg(0) == "" {
				add(r.name, model.Unsupported, "%s needs a length; left as text", raw)
				return raw
			}
			if c := unescapeArg(arg(1)); c != "" && c != alnum {
				add(r.name, model.Partial, "%s: custom character set %q not supported; Load Meter uses [a-zA-Z0-9]", raw, c)
			}
			if arg(2) != "" {
				add(r.name, model.Partial, "%s: result variable %q is not stored", raw, arg(2))
			}
			return "${__randomString(" + arg(0) + ")}"
		case "__P", "__property":
			name := arg(0)
			if name == "" {
				add(r.name, model.Unsupported, "%s without a property name; left as text", raw)
				return raw
			}
			def := arg(1)
			if r.name == "__property" {
				def = arg(2) // __property(name,var,default)
			}
			def = unescapeArg(def)
			if _, ok := vars[name]; !ok {
				vars[name] = def
			}
			add(r.name, model.Partial, "%s: JMeter property mapped to scenario variable ${%s} (default %q); set it in Load Meter variables instead of -J%s", raw, name, def, name)
			return "${" + name + "}"
		}
		add(r.name, model.Unsupported, "JMeter function %s has no Load Meter equivalent; left as text (will not resolve at runtime)", raw)
		return raw
	})
	return out, issues
}

// exportFuncs maps Load Meter built-ins in s to JMeter functions.
func exportFuncs(s string) (string, []fnIssue) {
	var issues []fnIssue
	out := rewrite(s, func(r ref, raw string) string {
		if !strings.HasPrefix(r.name, "__") {
			return raw
		}
		arg := func(i int) string {
			if i < len(r.args) {
				return strings.TrimSpace(r.args[i])
			}
			return ""
		}
		switch r.name {
		case "__uuid":
			return "${__UUID()}"
		case "__randomInt":
			return "${__Random(" + arg(0) + "," + arg(1) + ")}"
		case "__timestamp":
			if f := arg(0); f != "" {
				return "${__time(" + f + ",)}"
			}
			return "${__time()}"
		case "__counter":
			return "${__counter(FALSE,)}"
		case "__vu":
			return "${__threadNum}"
		case "__randomString":
			n := arg(0)
			if n == "" {
				n = "8"
			}
			return "${__RandomString(" + n + "," + alnum + ",)}"
		case "__urlencode":
			return "${__urlencode(" + strings.Join(r.args, ",") + ")}"
		}
		issues = append(issues, fnIssue{Fn: r.name, Status: model.Partial,
			Note: fmt.Sprintf("Load Meter function %s has no JMeter mapping; left as text", raw)})
		return raw
	})
	return out, issues
}

var varRefRE = regexp.MustCompile(`\$\{[^{}]*\}`)

// splitVars calls lit for literal runs and keeps ${...} references verbatim.
func splitVars(s string, lit func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range varRefRE.FindAllStringIndex(s, -1) {
		b.WriteString(lit(s[last:m[0]]))
		b.WriteString(s[m[0]:m[1]])
		last = m[1]
	}
	b.WriteString(lit(s[last:]))
	return b.String()
}

func quoteKeepVars(s string) string  { return splitVars(s, regexp.QuoteMeta) }
func encodeKeepVars(s string) string { return splitVars(s, url.QueryEscape) }

// re2Error reports whether p (with ${vars} replaced by a literal) compiles as Go RE2.
func re2Error(p string) error {
	_, err := regexp.Compile(varRefRE.ReplaceAllString(p, "x"))
	return err
}

func isLiteralPattern(p string) bool { return regexp.QuoteMeta(p) == p }
