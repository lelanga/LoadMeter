package vars

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"html"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// call is one function invocation: already-expanded arguments plus the context.
type call struct {
	c     *Context
	args  []string
	depth int
	res   *Result
}

func (k *call) arg(i int) string {
	if i < len(k.args) {
		return k.args[i]
	}
	return ""
}

// store saves v into the variable named by argument i, when given (JMeter's
// optional "name of variable to store the result" argument). It returns v.
func (k *call) store(i int, v string) string {
	if n := strings.TrimSpace(k.arg(i)); n != "" {
		k.c.SetRaw(n, v)
	}
	return v
}

type fn func(k *call) (string, error)

var registry map[string]fn

// aliases maps JMeter function names to Load Meter built-ins with the same
// behaviour, so pasted or imported JMeter expressions keep working.
var aliases = map[string]string{
	"__UUID":                   "__uuid",
	"__threadNum":              "__vu",
	"__V":                      "__v",
	"__RandomDate":             "__randomDate",
	"__escapeOroRegexpChars":   "__escapeRegex",
	"__RandomFromMultipleVars": "__randomFromVars",
}

func init() {
	registry = map[string]fn{
		"__uuid":            fnUUID,
		"__randomInt":       fnRandomInt,
		"__randomString":    fnRandomString,
		"__timestamp":       fnTimestamp,
		"__counter":         fnCounter,
		"__vu":              func(k *call) (string, error) { return strconv.Itoa(k.c.VU), nil },
		"__iteration":       func(k *call) (string, error) { return strconv.Itoa(k.c.Iter), nil },
		"__env":             fnEnv,
		"__urlencode":       fnURLEncode,
		"__urldecode":       fnURLDecode,
		"__v":               fnV,
		"__eval":            fnEval,
		"__evalVar":         fnEvalVar,
		"__isVarDefined":    fnIsVarDefined,
		"__split":           fnSplit,
		"__intSum":          fnSum(32),
		"__longSum":         fnSum(64),
		"__changeCase":      fnChangeCase,
		"__digest":          fnDigest,
		"__base64Encode":    fnBase64Encode,
		"__base64Decode":    fnBase64Decode,
		"__escapeHtml":      oneArg(html.EscapeString),
		"__unescapeHtml":    oneArg(html.UnescapeString),
		"__escapeXml":       oneArg(escapeXML),
		"__escapeRegex":     oneArg(regexp.QuoteMeta),
		"__unescape":        oneArg(unescapeJava),
		"__char":            fnChar,
		"__timeShift":       fnTimeShift,
		"__randomDate":      fnRandomDate,
		"__dateTimeConvert": fnDateTimeConvert,
		"__randomFromList":  fnRandomFromList,
		"__randomFromVars":  fnRandomFromVars,
	}
	for a, n := range aliases {
		registry[a] = registry[n]
	}
}

// Functions lists built-ins for docs/UI.
var Functions = []string{"__uuid()", "__randomInt(min,max)", "__randomString(n)", "__timestamp([format|/divisor][,var])",
	"__counter([TRUE|FALSE][,var])", "__vu()", "__iteration()", "__env(LOADMETER_NAME[,default])", "__urlencode(value)",
	"__urldecode(value)", "__v(name[,default])", "__eval(text)", "__evalVar(name)", "__isVarDefined(name)",
	"__split(text,var[,delimiters])", "__intSum(a,b[,...][,var])", "__longSum(a,b[,...][,var])",
	"__changeCase(text[,UPPER|LOWER|CAPITALIZE][,var])", "__digest(algorithm,value[,salt[,upper[,var]]])",
	"__base64Encode(value[,var])", "__base64Decode(value[,var])", "__escapeHtml(value)", "__unescapeHtml(value)",
	"__escapeXml(value)", "__escapeRegex(value)", "__unescape(value)", "__char(code[,code...])",
	"__timeShift([format],[date],[shift][,locale][,var])", "__randomDate([format],[start],end[,locale][,var])",
	"__dateTimeConvert(date,[sourceFormat],targetFormat[,var])", "__randomFromList(a,b[,...])",
	"__randomFromVars(var1|var2[,var])"}

func oneArg(f func(string) string) fn {
	return func(k *call) (string, error) {
		if len(k.args) < 1 {
			return "", fmt.Errorf("needs 1 argument")
		}
		return k.store(1, f(k.args[0])), nil
	}
}

func fnUUID(*call) (string, error) {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func fnRandomInt(k *call) (string, error) {
	if len(k.args) < 2 {
		return "", fmt.Errorf("__randomInt(min,max) needs 2 args")
	}
	lo, err1 := strconv.Atoi(k.args[0])
	hi, err2 := strconv.Atoi(k.args[1])
	if err1 != nil || err2 != nil || hi < lo {
		return "", fmt.Errorf("__randomInt: invalid range")
	}
	return k.store(2, strconv.Itoa(lo+k.c.Rand.IntN(hi-lo+1))), nil
}

func fnRandomString(k *call) (string, error) {
	n := 8
	if v, err := strconv.Atoi(k.arg(0)); err == nil && v > 0 && v <= 4096 {
		n = v
	}
	const al = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = al[k.c.Rand.IntN(len(al))]
	}
	return string(b), nil
}

// namedTimeFormats are JMeter's __time shorthands.
var namedTimeFormats = map[string]string{"YMD": "yyyyMMdd", "HMS": "HHmmss", "YMDHMS": "yyyyMMdd-HHmmss"}

func fnTimestamp(k *call) (string, error) {
	now := time.Now()
	f := k.arg(0)
	if nf, ok := namedTimeFormats[f]; ok {
		f = nf
	}
	switch {
	case f == "":
		return k.store(1, strconv.FormatInt(now.UnixMilli(), 10)), nil
	case strings.HasPrefix(f, "/"):
		d, err := strconv.ParseInt(f[1:], 10, 64)
		if err != nil || d <= 0 {
			return "", fmt.Errorf("__timestamp: invalid divisor %q", f)
		}
		return k.store(1, strconv.FormatInt(now.UnixMilli()/d, 10)), nil
	}
	s, err := FormatJavaDate(now, f)
	if err != nil {
		return "", err
	}
	return k.store(1, s), nil
}

func fnCounter(k *call) (string, error) {
	var n int64
	if strings.EqualFold(k.arg(0), "true") {
		k.c.vuCounter++
		n = k.c.vuCounter
	} else {
		if k.c.Counter == nil {
			k.c.Counter = new(atomic.Int64)
		}
		n = k.c.Counter.Add(1)
	}
	return k.store(1, strconv.FormatInt(n, 10)), nil
}

func fnEnv(k *call) (string, error) {
	if len(k.args) < 1 || !strings.HasPrefix(k.args[0], EnvPrefix) {
		return "", fmt.Errorf("__env only reads %s* variables", EnvPrefix)
	}
	v, ok := os.LookupEnv(k.args[0])
	if !ok && len(k.args) > 1 {
		return k.args[1], nil
	}
	return v, nil
}

func fnURLEncode(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__urlencode needs 1 arg")
	}
	return urlEncode(strings.Join(k.args, ",")), nil
}

func fnURLDecode(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__urldecode needs 1 arg")
	}
	v, err := url.QueryUnescape(strings.Join(k.args, ","))
	if err != nil {
		return "", fmt.Errorf("__urldecode: %w", err)
	}
	return v, nil
}

// fnV reads the variable whose name is the (expanded) argument: ${__v(id_${n})}.
func fnV(k *call) (string, error) {
	if len(k.args) < 1 || k.args[0] == "" {
		return "", fmt.Errorf("__v needs a variable name")
	}
	if v, ok := k.c.lookup(k.args[0], k.depth, k.res); ok {
		k.res.Used = append(k.res.Used, k.args[0])
		return v, nil
	}
	if len(k.args) > 1 {
		return k.args[1], nil
	}
	return "", fmt.Errorf("variable %s is not defined", k.args[0])
}

func fnEval(k *call) (string, error) {
	if k.depth+1 >= MaxDepth {
		return "", fmt.Errorf("__eval nested too deeply")
	}
	out, r := k.c.expand(strings.Join(k.args, ","), k.depth+1)
	k.res.merge(r)
	return out, nil
}

func fnEvalVar(k *call) (string, error) {
	if k.depth+1 >= MaxDepth {
		return "", fmt.Errorf("__evalVar nested too deeply")
	}
	v, ok := k.c.Vars[k.arg(0)]
	if !ok {
		return "", fmt.Errorf("variable %s is not defined", k.arg(0))
	}
	out, r := k.c.expand(v, k.depth+1)
	k.res.merge(r)
	return out, nil
}

func fnIsVarDefined(k *call) (string, error) {
	_, ok := k.c.Vars[k.arg(0)]
	return strconv.FormatBool(ok), nil
}

// fnSplit follows JMeter's __split: every character of the delimiter
// argument is a delimiter (default ","), empty fields between or after
// delimiters become "?", and a leading delimiter is ignored. It sets var
// (the whole text), var_n (field count) and var_1..var_N, and returns the text.
func fnSplit(k *call) (string, error) {
	if len(k.args) < 2 || strings.TrimSpace(k.args[1]) == "" {
		return "", fmt.Errorf("__split(text,var[,delimiters]) needs 2 args")
	}
	text, name, delims := k.args[0], strings.TrimSpace(k.args[1]), k.arg(2)
	if delims == "" {
		delims = ","
	}
	var parts []string
	var cur strings.Builder
	lastDelim, started := false, false
	for _, r := range text {
		if strings.ContainsRune(delims, r) {
			if started && !lastDelim {
				parts = append(parts, cur.String())
				cur.Reset()
			} else if lastDelim {
				parts = append(parts, "?")
			}
			lastDelim = true
			continue
		}
		started, lastDelim = true, false
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	} else if lastDelim {
		parts = append(parts, "?")
	}
	k.c.SetRaw(name, text)
	k.c.SetRaw(name+"_n", strconv.Itoa(len(parts)))
	for i, p := range parts {
		k.c.SetRaw(name+"_"+strconv.Itoa(i+1), p)
	}
	k.c.Delete(name + "_" + strconv.Itoa(len(parts)+1))
	return text, nil
}

func fnSum(bits int) fn {
	return func(k *call) (string, error) {
		if len(k.args) < 2 {
			return "", fmt.Errorf("needs at least 2 numbers")
		}
		var sum int64
		n := len(k.args)
		last := strings.TrimSpace(k.args[n-1])
		varName := ""
		if _, err := strconv.ParseInt(last, 10, bits); err != nil {
			varName, n = last, n-1
		}
		for _, a := range k.args[:n] {
			v, err := strconv.ParseInt(strings.TrimSpace(a), 10, bits)
			if err != nil {
				return "", fmt.Errorf("%q is not an integer", a)
			}
			sum += v
		}
		if bits == 32 {
			sum = int64(int32(sum)) // Java int overflow semantics
		}
		out := strconv.FormatInt(sum, 10)
		if varName != "" {
			k.c.SetRaw(varName, out)
		}
		return out, nil
	}
}

func fnChangeCase(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__changeCase needs 1 arg")
	}
	s := k.args[0]
	switch strings.ToUpper(strings.TrimSpace(k.arg(1))) {
	case "", "UPPER":
		s = strings.ToUpper(s)
	case "LOWER":
		s = strings.ToLower(s)
	case "CAPITALIZE":
		if r, n := utf8.DecodeRuneInString(s); n > 0 {
			s = string(unicode.ToUpper(r)) + s[n:]
		}
	default:
		return "", fmt.Errorf("__changeCase: unknown mode %q", k.arg(1))
	}
	return k.store(2, s), nil
}

// NewHash returns a hash for a JMeter/Java digest algorithm name (MD5, SHA-1,
// SHA-224, SHA-256, SHA-384, SHA-512; case and dash insensitive).
func NewHash(alg string) (hash.Hash, error) {
	switch strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(alg)), "-", "") {
	case "MD5":
		return md5.New(), nil
	case "SHA1", "SHA":
		return sha1.New(), nil
	case "SHA224":
		return sha256.New224(), nil
	case "SHA256":
		return sha256.New(), nil
	case "SHA384":
		return sha512.New384(), nil
	case "SHA512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("unsupported digest algorithm %q", alg)
}

func fnDigest(k *call) (string, error) {
	if len(k.args) < 2 {
		return "", fmt.Errorf("__digest(algorithm,value) needs 2 args")
	}
	h, err := NewHash(k.args[0])
	if err != nil {
		return "", err
	}
	h.Write([]byte(k.args[1]))
	h.Write([]byte(k.arg(2))) // salt (JMeter appends it after the value)
	out := hex.EncodeToString(h.Sum(nil))
	if strings.EqualFold(strings.TrimSpace(k.arg(3)), "true") {
		out = strings.ToUpper(out)
	}
	return k.store(4, out), nil
}

func fnBase64Encode(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__base64Encode needs 1 arg")
	}
	return k.store(1, base64.StdEncoding.EncodeToString([]byte(k.args[0]))), nil
}

func fnBase64Decode(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__base64Decode needs 1 arg")
	}
	s := strings.TrimSpace(k.args[0])
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err != nil {
			return "", fmt.Errorf("__base64Decode: invalid input")
		}
	}
	return k.store(1, string(b)), nil
}

func escapeXML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// unescapeJava resolves Java string escapes (\n \t \r \b \f \" \' \\ \uXXXX).
func unescapeJava(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '"', '\'', '\\':
			b.WriteByte(s[i])
		case 'u':
			if i+4 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+5], 16, 16); err == nil {
					b.WriteRune(rune(v))
					i += 4
					continue
				}
			}
			b.WriteString(`\u`)
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func fnChar(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__char needs at least 1 code")
	}
	var b strings.Builder
	for _, a := range k.args {
		v, err := strconv.ParseInt(strings.TrimSpace(a), 0, 32)
		if err != nil || v < 0 || v > unicode.MaxRune {
			return "", fmt.Errorf("__char: invalid code %q", a)
		}
		b.WriteRune(rune(v))
	}
	return b.String(), nil
}

// fnTimeShift: __timeShift(format,date,shift[,locale][,var]). Empty format means
// epoch milliseconds (for both input and output); empty date means now; shift
// is an ISO-8601 duration such as P1DT2H or -PT30M.
func fnTimeShift(k *call) (string, error) {
	f, d, shift := strings.TrimSpace(k.arg(0)), strings.TrimSpace(k.arg(1)), strings.TrimSpace(k.arg(2))
	t := time.Now()
	if d != "" {
		var err error
		if t, err = parseDateArg(d, f); err != nil {
			return "", err
		}
	}
	if shift != "" {
		dur, err := ParseISODuration(shift)
		if err != nil {
			return "", err
		}
		t = t.Add(dur)
	}
	out, err := formatDateArg(t, f)
	if err != nil {
		return "", err
	}
	return k.store(4, out), nil
}

// fnRandomDate: __randomDate(format,start,end[,locale][,var]). A random day in
// [start, end) formatted with format (default yyyy-MM-dd); start defaults to today.
func fnRandomDate(k *call) (string, error) {
	f := strings.TrimSpace(k.arg(0))
	if f == "" {
		f = "yyyy-MM-dd"
	}
	layout, err := JavaToGoLayout(f)
	if err != nil {
		return "", err
	}
	day := func(t time.Time) int64 {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
	}
	start := day(time.Now())
	if s := strings.TrimSpace(k.arg(1)); s != "" {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err != nil {
			return "", fmt.Errorf("__randomDate: start %q does not match %q", s, f)
		}
		start = day(t)
	}
	e := strings.TrimSpace(k.arg(2))
	te, err := time.ParseInLocation(layout, e, time.Local)
	if err != nil {
		return "", fmt.Errorf("__randomDate: end %q does not match %q", e, f)
	}
	end := day(te)
	if end <= start {
		return "", fmt.Errorf("__randomDate: end must be after start")
	}
	pick := start + k.c.Rand.Int64N(end-start)
	out, err := FormatJavaDate(time.Unix(pick*86400, 0).UTC(), f)
	if err != nil {
		return "", err
	}
	return k.store(4, out), nil
}

// fnDateTimeConvert: __dateTimeConvert(date,sourceFormat,targetFormat[,var]).
// An empty source format means the date is epoch milliseconds.
func fnDateTimeConvert(k *call) (string, error) {
	if len(k.args) < 3 {
		return "", fmt.Errorf("__dateTimeConvert(date,sourceFormat,targetFormat) needs 3 args")
	}
	t, err := parseDateArg(strings.TrimSpace(k.args[0]), strings.TrimSpace(k.args[1]))
	if err != nil {
		return "", err
	}
	out, err := formatDateArg(t, strings.TrimSpace(k.args[2]))
	if err != nil {
		return "", err
	}
	return k.store(3, out), nil
}

func parseDateArg(d, f string) (time.Time, error) {
	if f == "" {
		ms, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("date %q is not epoch milliseconds", d)
		}
		return time.UnixMilli(ms), nil
	}
	layout, err := JavaToGoLayout(f)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.ParseInLocation(layout, d, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q does not match format %q", d, f)
	}
	return t, nil
}

func formatDateArg(t time.Time, f string) (string, error) {
	if f == "" {
		return strconv.FormatInt(t.UnixMilli(), 10), nil
	}
	return FormatJavaDate(t, f)
}

// fnRandomFromList returns one of its arguments at random.
func fnRandomFromList(k *call) (string, error) {
	if len(k.args) < 1 {
		return "", fmt.Errorf("__randomFromList needs at least 1 value")
	}
	return k.args[k.c.Rand.IntN(len(k.args))], nil
}

// fnRandomFromVars follows JMeter's __RandomFromMultipleVars: the first
// argument lists variable names separated by "|"; a name with name_matchNr
// contributes name_1..name_N (the output of an "all" extractor), otherwise
// its own value. One value is returned at random.
func fnRandomFromVars(k *call) (string, error) {
	var vals []string
	for _, n := range strings.Split(k.arg(0), "|") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if cnt, err := strconv.Atoi(k.c.Vars[n+"_matchNr"]); err == nil {
			for i := 1; i <= cnt; i++ {
				if v, ok := k.c.Vars[n+"_"+strconv.Itoa(i)]; ok {
					vals = append(vals, v)
				}
			}
			continue
		}
		if v, ok := k.c.Vars[n]; ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return "", fmt.Errorf("__randomFromVars: no values in %q", k.arg(0))
	}
	return k.store(1, vals[k.c.Rand.IntN(len(vals))]), nil
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
