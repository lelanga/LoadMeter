// Package vars implements ${name} substitution and built-in functions.
//
// Syntax: ${name} reads a variable; ${__fn(arg1,arg2)} calls a built-in.
// Unknown variables are left verbatim (JMeter behaviour) and reported as
// unresolved so the engine and validators can flag them.
package vars

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var globalCounter atomic.Int64

// EnvPrefix restricts ${__env(NAME)} to variables starting with this prefix so an
// imported (untrusted) script cannot read arbitrary secrets from the environment.
const EnvPrefix = "LOADMETER_"

type Context struct {
	Vars map[string]string
	VU   int
	Iter int
	Rand *mrand.Rand
}

func NewContext(vu int, seed uint64) *Context {
	return &Context{Vars: map[string]string{}, VU: vu, Rand: mrand.New(mrand.NewPCG(seed, uint64(vu)+1))}
}

// Result of an expansion.
type Result struct {
	Used       []string // variable names that were substituted
	Unresolved []string // ${name} references with no value
}

// Expand substitutes variables and functions in s.
func (c *Context) Expand(s string) (string, Result) {
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
			v, nested, err := c.call(expr)
			res.Used = append(res.Used, nested.Used...)
			res.Unresolved = append(res.Unresolved, nested.Unresolved...)
			if err != nil {
				b.WriteString(s[i : end+1])
				res.Unresolved = append(res.Unresolved, expr)
			} else {
				b.WriteString(v)
			}
		} else if v, ok := c.Vars[expr]; ok {
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

// matchBrace finds the closing } for an expression starting at i, honouring
// nested ${...} inside function arguments.
func matchBrace(s string, i int) int {
	depth := 1
	for j := i; j < len(s); j++ {
		switch s[j] {
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

func (c *Context) call(expr string) (string, Result, error) {
	var nested Result
	v, err := c.callInto(expr, &nested)
	return v, nested, err
}

func (c *Context) callInto(expr string, nested *Result) (string, error) {
	name, argStr := expr, ""
	if p := strings.IndexByte(expr, '('); p >= 0 {
		if !strings.HasSuffix(expr, ")") {
			return "", fmt.Errorf("malformed function %q", expr)
		}
		name, argStr = expr[:p], expr[p+1:len(expr)-1]
	}
	var args []string
	if argStr != "" {
		for _, a := range strings.Split(argStr, ",") {
			v, r := c.Expand(strings.TrimSpace(a))
			nested.Used = append(nested.Used, r.Used...)
			nested.Unresolved = append(nested.Unresolved, r.Unresolved...)
			args = append(args, v)
		}
	}
	switch name {
	case "__uuid":
		var u [16]byte
		_, _ = rand.Read(u[:])
		u[6] = (u[6] & 0x0f) | 0x40
		u[8] = (u[8] & 0x3f) | 0x80
		h := hex.EncodeToString(u[:])
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
	case "__randomInt":
		if len(args) != 2 {
			return "", fmt.Errorf("__randomInt(min,max) needs 2 args")
		}
		lo, err1 := strconv.Atoi(args[0])
		hi, err2 := strconv.Atoi(args[1])
		if err1 != nil || err2 != nil || hi < lo {
			return "", fmt.Errorf("__randomInt: invalid range")
		}
		return strconv.Itoa(lo + c.Rand.IntN(hi-lo+1)), nil
	case "__randomString":
		n := 8
		if len(args) > 0 {
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 && v <= 4096 {
				n = v
			}
		}
		const al = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = al[c.Rand.IntN(len(al))]
		}
		return string(b), nil
	case "__timestamp":
		if len(args) > 0 && args[0] != "" {
			return time.Now().Format(goLayout(args[0])), nil
		}
		return strconv.FormatInt(time.Now().UnixMilli(), 10), nil
	case "__counter":
		return strconv.FormatInt(globalCounter.Add(1), 10), nil
	case "__vu":
		return strconv.Itoa(c.VU), nil
	case "__iteration":
		return strconv.Itoa(c.Iter), nil
	case "__env":
		if len(args) < 1 || !strings.HasPrefix(args[0], EnvPrefix) {
			return "", fmt.Errorf("__env only reads %s* variables", EnvPrefix)
		}
		v, ok := os.LookupEnv(args[0])
		if !ok && len(args) > 1 {
			return args[1], nil
		}
		return v, nil
	case "__urlencode":
		if len(args) < 1 {
			return "", fmt.Errorf("__urlencode needs 1 arg")
		}
		return urlEncode(strings.Join(args, ",")), nil
	}
	return "", fmt.Errorf("unknown function %s", name)
}

// goLayout converts a small subset of Java SimpleDateFormat to Go layout.
func goLayout(f string) string {
	r := strings.NewReplacer("yyyy", "2006", "MM", "01", "dd", "02", "HH", "15", "mm", "04", "ss", "05", "SSS", "000")
	return r.Replace(f)
}

func urlEncode(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexd[ch>>4])
			b.WriteByte(hexd[ch&15])
		}
	}
	return b.String()
}

// Functions lists built-ins for docs/UI.
var Functions = []string{"__uuid()", "__randomInt(min,max)", "__randomString(n)", "__timestamp([format])",
	"__counter()", "__vu()", "__iteration()", "__env(LOADMETER_NAME[,default])", "__urlencode(value)"}

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
