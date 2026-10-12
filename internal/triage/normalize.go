package triage

import "strings"

// maxMsgBytes and maxTokens bound the work done per failure message.
const (
	maxMsgBytes = 1024
	maxTokens   = 48
)

// Normalize turns a failure message into a token sequence in which every
// variable part (numbers, durations, UUIDs, hex ids, hosts, IP addresses,
// URLs, paths, timestamps and quoted values) is replaced by a typed
// placeholder such as <NUM>, <UUID> or <STR>. Two messages that differ only in
// those parts normalise to the same sequence, which is what the Drain parser
// (drain.go) then groups into templates. It is a hand-written scanner rather
// than a stack of regular expressions so it stays fast on millions of rows.
func Normalize(msg string) []string {
	if len(msg) > maxMsgBytes {
		msg = msg[:maxMsgBytes]
	}
	msg = stripQuoted(msg)
	toks := make([]string, 0, 12)
	for _, f := range strings.Fields(msg) {
		if t := normToken(f); t != "" {
			toks = append(toks, t)
			if len(toks) >= maxTokens {
				break
			}
		}
	}
	return toks
}

// stripQuoted replaces "double", `back-tick` and 'single' quoted spans with
// <STR>. A single quote only opens a span at a word start and only closes
// before a non-letter, so apostrophes ("can't") are left alone. Backslash
// escaped quotes inside a span (as written by Go's %q) do not close it.
func stripQuoted(s string) string {
	if !strings.ContainsAny(s, "\"'`") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c == '"' || c == '`' || (c == '\'' && (i == 0 || strings.IndexByte(" \t(=:[{,", s[i-1]) >= 0)) {
			end := -1
			for j := i + 1; j < len(s) && j-i <= 400; j++ {
				if s[j] == '\\' && c == '"' {
					j++
					continue
				}
				if s[j] == c && (c != '\'' || j+1 == len(s) || !isLetter(s[j+1])) {
					end = j
					break
				}
			}
			if end > 0 {
				b.WriteString("<STR>")
				i = end + 1
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isHex(c byte) bool    { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func normToken(t string) string {
	t = strings.TrimLeft(t, "([{")
	t = strings.TrimRight(t, ".,;:)]}!?")
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">") {
		return t
	}
	// a->b (connection endpoints) and key=value: classify each side.
	if i := strings.Index(t, "->"); i > 0 && i+2 < len(t) {
		return normCore(t[:i]) + "->" + normCore(t[i+2:])
	}
	if i := strings.IndexByte(t, '='); i > 0 && i+1 < len(t) && isLetter(t[0]) {
		return t[:i+1] + normCore(t[i+1:])
	}
	return normCore(t)
}

func normCore(c string) string {
	if c == "" {
		return c
	}
	if c[0] == '<' && c[len(c)-1] == '>' {
		return c
	}
	switch {
	case isUUID(c):
		return "<UUID>"
	case hasAnyPrefix(c, "http://", "https://", "ws://", "wss://"):
		return "<URL>"
	case c[0] == '/' && len(c) > 1:
		return "<PATH>"
	case isTimestamp(c):
		return "<TS>"
	case isIPv4(c) || isIPv6ish(c):
		return "<IP>"
	case isHostPort(c):
		return "<HOST>"
	case isHexID(c):
		return "<HEX>"
	case isNumberish(c):
		return "<NUM>"
	case isLongID(c):
		return "<ID>"
	}
	return digitRuns(c)
}

func hasAnyPrefix(s string, ps ...string) bool {
	for _, p := range ps {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			return true
		}
	}
	return false
}

func isUUID(c string) bool {
	if len(c) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c[i] != '-' {
				return false
			}
		} else if !isHex(c[i]) {
			return false
		}
	}
	return true
}

// isTimestamp matches YYYY-MM-DD[...] and HH:MM:SS[.fff] forms.
func isTimestamp(c string) bool {
	if len(c) >= 10 && isDigit(c[0]) && isDigit(c[1]) && isDigit(c[2]) && isDigit(c[3]) && c[4] == '-' && isDigit(c[5]) && isDigit(c[6]) && c[7] == '-' && isDigit(c[8]) && isDigit(c[9]) {
		return true
	}
	return len(c) >= 8 && isDigit(c[0]) && isDigit(c[1]) && c[2] == ':' && isDigit(c[3]) && isDigit(c[4]) && c[5] == ':' && isDigit(c[6]) && isDigit(c[7])
}

func isIPv4(c string) bool {
	if i := strings.LastIndexByte(c, ':'); i > 0 && allDigits(c[i+1:]) {
		c = c[:i]
	}
	parts := strings.Split(c, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || !allDigits(p) {
			return false
		}
	}
	return true
}

func isIPv6ish(c string) bool {
	if strings.Count(c, ":") < 2 {
		return false
	}
	colons := 0
	for i := 0; i < len(c); i++ {
		switch {
		case c[i] == ':':
			colons++
		case isHex(c[i]), c[i] == '.', c[i] == '[', c[i] == ']', c[i] == '%':
		default:
			return false
		}
	}
	return colons >= 2 && (strings.Contains(c, "::") || colons >= 5)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// isHostPort matches lower-case DNS names (optionally with :port) and localhost.
func isHostPort(c string) bool {
	if i := strings.LastIndexByte(c, ':'); i > 0 && allDigits(c[i+1:]) {
		c = c[:i]
	}
	if c == "localhost" {
		return true
	}
	if !strings.Contains(c, ".") {
		return false
	}
	for i := 0; i < len(c); i++ {
		ch := c[i]
		if !(ch >= 'a' && ch <= 'z' || isDigit(ch) || ch == '.' || ch == '-') {
			return false
		}
	}
	last := c[strings.LastIndexByte(c, '.')+1:]
	if len(last) < 2 || len(last) > 24 {
		return false
	}
	for i := 0; i < len(last); i++ {
		if !(last[i] >= 'a' && last[i] <= 'z') {
			return false
		}
	}
	return true
}

func isHexID(c string) bool {
	if len(c) > 2 && c[0] == '0' && (c[1] == 'x' || c[1] == 'X') {
		for i := 2; i < len(c); i++ {
			if !isHex(c[i]) {
				return false
			}
		}
		return true
	}
	if len(c) < 8 {
		return false
	}
	digit := false
	for i := 0; i < len(c); i++ {
		if !isHex(c[i]) {
			return false
		}
		digit = digit || isDigit(c[i])
	}
	return digit && !allDigits(c)
}

// isNumberish matches numbers, durations and sizes: 12, -3.5, 1,234, 30s, 5m0s, 100ms, 12MB, 99%.
func isNumberish(c string) bool {
	i := 0
	if c[0] == '-' || c[0] == '+' {
		i = 1
	}
	if i >= len(c) || !isDigit(c[i]) {
		return false
	}
	run := 0
	for ; i < len(c); i++ {
		ch := c[i]
		switch {
		case isDigit(ch) || ch == '.' || ch == ',' || ch == '%' || ch == ':':
			run = 0
		case isLetter(ch) || ch == 0xc2 || ch == 0xb5: // letters and the micro sign (UTF-8 C2 B5)
			run++
			if run > 3 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isLongID(c string) bool {
	if len(c) < 12 {
		return false
	}
	d, l := false, false
	for i := 0; i < len(c); i++ {
		ch := c[i]
		switch {
		case isDigit(ch):
			d = true
		case isLetter(ch):
			l = true
		case ch == '-' || ch == '_':
		default:
			return false
		}
	}
	return d && l
}

// digitRuns replaces every digit run inside a word: user42 -> user<NUM>.
func digitRuns(c string) string {
	has := false
	for i := 0; i < len(c); i++ {
		if isDigit(c[i]) {
			has = true
			break
		}
	}
	if !has {
		return c
	}
	var b strings.Builder
	for i := 0; i < len(c); {
		if isDigit(c[i]) {
			for i < len(c) && isDigit(c[i]) {
				i++
			}
			b.WriteString("<NUM>")
			continue
		}
		b.WriteByte(c[i])
		i++
	}
	return b.String()
}
