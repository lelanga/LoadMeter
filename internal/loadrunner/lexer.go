package loadrunner

import (
	"fmt"
	"strings"
)

// This file is a small C tokenizer for VuGen script text. It is deliberately
// not a C compiler: it only needs enough fidelity to find statements,
// function calls and their (mostly string-literal) arguments, with line
// numbers for the compatibility report. Imported code is never executed.

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString    // val holds the decoded C string
	tChar      // character literal; text is the raw source
	tPunct     // operators and punctuation
	tDirective // a whole preprocessor line, e.g. #include "globals.h"
)

type token struct {
	kind tokKind
	text string // raw source text
	val  string // decoded value for strings
	line int
	pos  int // byte offset of the first character
	end  int // byte offset after the last character
}

func (t token) is(kind tokKind, text string) bool { return t.kind == kind && t.text == text }

type lexError struct {
	line int
	msg  string
}

func (e lexError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

var twoCharOps = map[string]bool{
	"++": true, "--": true, "->": true, "==": true, "!=": true, "<=": true, ">=": true,
	"&&": true, "||": true, "+=": true, "-=": true, "*=": true, "/=": true, "%=": true,
	"&=": true, "|=": true, "^=": true, "<<": true, ">>": true, "::": true,
}

// lex tokenizes src. It never fails hard: malformed input (an unterminated
// string or comment) produces an error entry and lexing continues.
func lex(src []byte) ([]token, []lexError) {
	s := string(src)
	s = strings.TrimPrefix(s, "\xef\xbb\xbf")
	var toks []token
	var errs []lexError
	line := 1
	lineStart := true // only whitespace seen since the last newline
	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '\n':
			line++
			lineStart = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
			continue
		case c == '/' && i+1 < n && s[i+1] == '/':
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		case c == '/' && i+1 < n && s[i+1] == '*':
			startLine := line
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				errs = append(errs, lexError{startLine, "unterminated /* comment"})
				line += strings.Count(s[i:], "\n")
				i = n
				continue
			}
			line += strings.Count(s[i:i+2+j], "\n")
			i += j + 4
			continue
		case c == '#' && lineStart:
			start, startLine := i, line
			for i < n && s[i] != '\n' {
				if s[i] == '\\' && i+1 < n && s[i+1] == '\n' {
					line++
					i += 2
					continue
				}
				if s[i] == '\\' && i+2 < n && s[i+1] == '\r' && s[i+2] == '\n' {
					line++
					i += 3
					continue
				}
				i++
			}
			toks = append(toks, token{kind: tDirective, text: strings.TrimSpace(s[start:i]), line: startLine, pos: start, end: i})
			continue
		}
		lineStart = false
		start := i
		switch {
		case c == '"':
			val, j, nl, ok := scanString(s, i)
			if !ok {
				errs = append(errs, lexError{line, "unterminated string literal"})
			}
			toks = append(toks, token{kind: tString, text: s[start:j], val: val, line: line, pos: start, end: j})
			line += nl
			i = j
		case c == '\'':
			j := i + 1
			for j < n && s[j] != '\'' && s[j] != '\n' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < n && s[j] == '\'' {
				j++
			} else {
				errs = append(errs, lexError{line, "unterminated character literal"})
			}
			if j > n {
				j = n
			}
			toks = append(toks, token{kind: tChar, text: s[start:j], line: line, pos: start, end: j})
			i = j
		case isIdentStart(c):
			for i < n && isIdentChar(s[i]) {
				i++
			}
			// Wide/UTF-8 string prefixes: L"..", u8"..": treat as a plain string.
			if w := s[start:i]; (w == "L" || w == "u8" || w == "u" || w == "U") && i < n && s[i] == '"' {
				val, j, nl, ok := scanString(s, i)
				if !ok {
					errs = append(errs, lexError{line, "unterminated string literal"})
				}
				toks = append(toks, token{kind: tString, text: s[start:j], val: val, line: line, pos: start, end: j})
				line += nl
				i = j
				continue
			}
			toks = append(toks, token{kind: tIdent, text: s[start:i], line: line, pos: start, end: i})
		case c >= '0' && c <= '9' || c == '.' && i+1 < n && s[i+1] >= '0' && s[i+1] <= '9':
			i++
			for i < n {
				d := s[i]
				if isIdentChar(d) || d == '.' {
					i++
					continue
				}
				if (d == '+' || d == '-') && (s[i-1] == 'e' || s[i-1] == 'E') && !strings.HasPrefix(s[start:i], "0x") {
					i++
					continue
				}
				break
			}
			toks = append(toks, token{kind: tNumber, text: s[start:i], line: line, pos: start, end: i})
		default:
			if i+1 < n && twoCharOps[s[i:i+2]] {
				i += 2
			} else {
				i++
			}
			toks = append(toks, token{kind: tPunct, text: s[start:i], line: line, pos: start, end: i})
		}
	}
	toks = append(toks, token{kind: tEOF, line: line, pos: n, end: n})
	return toks, errs
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentChar(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// scanString decodes the C string literal starting at s[i] == '"'. It returns
// the decoded value, the index after the closing quote, the number of
// newlines consumed (line continuations) and whether it was terminated.
func scanString(s string, i int) (string, int, int, bool) {
	var b strings.Builder
	nl := 0
	j := i + 1
	n := len(s)
	for j < n {
		c := s[j]
		switch c {
		case '"':
			return b.String(), j + 1, nl, true
		case '\n':
			return b.String(), j, nl, false
		case '\\':
			if j+1 >= n {
				return b.String(), n, nl, false
			}
			e := s[j+1]
			j += 2
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte(7)
			case 'b':
				b.WriteByte(8)
			case 'f':
				b.WriteByte(12)
			case 'v':
				b.WriteByte(11)
			case '\\', '"', '\'', '?':
				b.WriteByte(e)
			case '\n': // line continuation
				nl++
			case '\r':
				if j < n && s[j] == '\n' {
					j++
				}
				nl++
			case 'x':
				v, k := 0, 0
				for k < 2 && j < n && isHex(s[j]) {
					v = v*16 + hexVal(s[j])
					j++
					k++
				}
				if k == 0 {
					b.WriteString(`\x`)
				} else {
					b.WriteByte(byte(v))
				}
			default:
				if e >= '0' && e <= '7' {
					v, k := int(e-'0'), 1
					for k < 3 && j < n && s[j] >= '0' && s[j] <= '7' {
						v = v*8 + int(s[j]-'0')
						j++
						k++
					}
					b.WriteByte(byte(v))
				} else {
					// Unknown escape: keep it verbatim (compilers warn, LR scripts rarely rely on it).
					b.WriteByte('\\')
					b.WriteByte(e)
				}
			}
		default:
			b.WriteByte(c)
			j++
		}
	}
	return b.String(), n, nl, false
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// decodeBinaryEscapes decodes LoadRunner BodyBinary text, where the C source
// "\\x00" leaves the literal characters \x00 after C unescaping.
func decodeBinaryEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' && isHex(s[i+2]) && isHex(s[i+3]) {
			b.WriteByte(byte(hexVal(s[i+2])*16 + hexVal(s[i+3])))
			i += 3
			continue
		}
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\\' {
			b.WriteByte('\\')
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
