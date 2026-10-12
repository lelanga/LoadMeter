package loadrunner

import (
	"fmt"
	"strings"
)

// The parser turns the token stream into a shallow statement tree: function
// definitions at top level, and inside them call statements, blocks, control
// flow, declarations and other expressions. Anything it does not understand
// is kept as an opaque statement with its line and source excerpt so the
// importer can report it; the parser never executes or evaluates code.

type stmtKind int

const (
	sCall    stmtKind = iota // name(args);
	sReturn                  // return ...;
	sBlock                   // { ... }
	sControl                 // if / for / while / do / switch
	sDecl                    // variable declaration
	sExpr                    // any other expression statement
	sLabel                   // label:, case x:, default:
	sEmpty                   // ;
)

type stmt struct {
	kind    stmtKind
	line    int
	excerpt string // first source line of the statement, trimmed
	call    *cCall // sCall
	body    []stmt // sBlock; sControl: body (and else branch after elseAt)
	keyword string // sControl keyword, sReturn value, sDecl/sExpr raw text
	header  []token
	elseAt  int // sControl if: index in body where the else branch begins (-1 = none)
}

type argKind int

const (
	aString argKind = iota
	aIdent
	aNumber
	aCall
	aExpr
)

type cArg struct {
	kind argKind
	str  string // aString: decoded and concatenated
	text string // raw source text
	call *cCall
	line int
}

type cCall struct {
	name string
	line int
	args []cArg
	// calls nested anywhere in the argument list (for analysis counting)
}

type cFunc struct {
	name    string
	line    int
	endLine int
	body    []stmt
}

type cFile struct {
	name       string
	src        string
	lines      []string
	funcs      []*cFunc
	directives []token
	topLevel   []stmt // declarations/expressions outside functions
	errors     []lexError
	toks       []token
}

func (f *cFile) fn(name string) *cFunc {
	for _, fn := range f.funcs {
		if fn.name == name {
			return fn
		}
	}
	return nil
}

func (f *cFile) excerpt(line int) string {
	if line < 1 || line > len(f.lines) {
		return ""
	}
	e := strings.TrimSpace(f.lines[line-1])
	if len(e) > 120 {
		e = e[:117] + "..."
	}
	return e
}

type parser struct {
	f    *cFile
	toks []token
	i    int
	errs []lexError
}

// parseC tokenizes and parses one C source file. It recovers from malformed
// input and never panics; problems are recorded in cFile.errors.
func parseC(name string, src []byte) (f *cFile) {
	text := strings.TrimPrefix(string(src), "\xef\xbb\xbf")
	f = &cFile{name: name, src: text, lines: strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")}
	toks, lerrs := lex([]byte(text))
	f.errors = append(f.errors, lerrs...)
	f.toks = toks
	p := &parser{f: f, toks: toks}
	defer func() {
		if r := recover(); r != nil {
			f.errors = append(f.errors, lexError{p.peek().line, fmt.Sprintf("parser stopped: %v", r)})
		}
		f.errors = append(f.errors, p.errs...)
	}()
	p.parseTop()
	return f
}

func (p *parser) peek() token {
	if p.i >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i]
}

func (p *parser) peekAt(k int) token {
	if p.i+k >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+k]
}

func (p *parser) next() token {
	t := p.peek()
	if p.i < len(p.toks) {
		p.i++
	}
	return t
}

func (p *parser) errorf(line int, format string, a ...any) {
	p.errs = append(p.errs, lexError{line, fmt.Sprintf(format, a...)})
}

// matchClose returns the index of the token closing the bracket at index
// open, or the EOF index when unbalanced.
func (p *parser) matchClose(open int) int {
	depth := 0
	for k := open; k < len(p.toks); k++ {
		t := p.toks[k]
		if t.kind == tEOF {
			return k
		}
		if t.kind != tPunct {
			continue
		}
		switch t.text {
		case "(", "{", "[":
			depth++
		case ")", "}", "]":
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return len(p.toks) - 1
}

func (p *parser) parseTop() {
	for p.peek().kind != tEOF {
		t := p.peek()
		if t.kind == tDirective {
			p.f.directives = append(p.f.directives, p.next())
			continue
		}
		// Look ahead to the first ';' or '{' at depth 0 to classify.
		k := p.i
		depth := 0
		isFunc := false
		for ; k < len(p.toks) && p.toks[k].kind != tEOF; k++ {
			tk := p.toks[k]
			if tk.kind == tDirective && depth == 0 {
				break
			}
			if tk.kind != tPunct {
				continue
			}
			if tk.text == "(" || tk.text == "[" {
				depth++
			} else if tk.text == ")" || tk.text == "]" {
				depth--
			} else if depth == 0 && tk.text == ";" {
				break
			} else if depth == 0 && tk.text == "{" {
				isFunc = k > p.i && p.toks[k-1].is(tPunct, ")")
				break
			}
		}
		if isFunc {
			// name = identifier right before the first '(' of the declarator
			name := ""
			for j := p.i; j < k; j++ {
				if p.toks[j].is(tPunct, "(") && j > p.i && p.toks[j-1].kind == tIdent {
					name = p.toks[j-1].text
					break
				}
			}
			fn := &cFunc{name: name, line: t.line}
			p.i = k
			fn.body = p.parseBlockBody()
			fn.endLine = p.toks[max(p.i-1, 0)].line
			p.f.funcs = append(p.f.funcs, fn)
			continue
		}
		if k >= len(p.toks) || p.toks[k].kind == tEOF || p.toks[k].kind == tDirective {
			// trailing junk without ';'
			if k > p.i {
				p.f.topLevel = append(p.f.topLevel, p.rawStmt(sExpr, p.i, k))
			}
			p.i = k
			continue
		}
		tk := p.toks[k]
		if tk.text == "{" { // struct/enum/initializer at top level
			c := p.matchClose(k)
			for c+1 < len(p.toks) && !p.toks[c+1].is(tPunct, ";") && p.toks[c+1].kind != tEOF && p.toks[c+1].kind != tDirective {
				c++
			}
			p.f.topLevel = append(p.f.topLevel, p.rawStmt(sDecl, p.i, c+1))
			p.i = min(c+2, len(p.toks)-1)
			continue
		}
		p.f.topLevel = append(p.f.topLevel, p.rawStmt(sDecl, p.i, k+1))
		p.i = k + 1
	}
}

func (p *parser) rawStmt(kind stmtKind, from, to int) stmt {
	if to > len(p.toks) {
		to = len(p.toks)
	}
	if from >= to {
		return stmt{kind: kind, line: p.peek().line}
	}
	first, last := p.toks[from], p.toks[to-1]
	raw := ""
	if last.end >= first.pos && last.end <= len(p.f.src) {
		raw = p.f.src[first.pos:last.end]
	}
	return stmt{kind: kind, line: first.line, excerpt: p.f.excerpt(first.line), keyword: collapseSpace(raw)}
}

// parseBlockBody expects the current token to be '{' and parses until the
// matching '}'.
func (p *parser) parseBlockBody() []stmt {
	open := p.next() // '{'
	var out []stmt
	for {
		t := p.peek()
		if t.kind == tEOF {
			p.errorf(open.line, "unclosed '{'")
			return out
		}
		if t.is(tPunct, "}") {
			p.next()
			return out
		}
		out = append(out, p.parseStmt())
	}
}

var declKeywords = map[string]bool{
	"int": true, "char": true, "long": true, "short": true, "double": true, "float": true,
	"unsigned": true, "signed": true, "void": true, "const": true, "static": true, "struct": true,
	"extern": true, "typedef": true, "register": true, "volatile": true, "enum": true, "union": true,
	"auto": true, "size_t": true, "time_t": true, "BOOL": true, "DWORD": true, "FILE": true,
	"merc_timer_handle_t": true, "lr_timer_t": true, "long long": true,
}

func (p *parser) parseStmt() stmt {
	t := p.peek()
	start := p.i
	switch {
	case t.kind == tDirective:
		p.next()
		return stmt{kind: sDecl, line: t.line, excerpt: t.text, keyword: t.text}
	case t.is(tPunct, ";"):
		p.next()
		return stmt{kind: sEmpty, line: t.line}
	case t.is(tPunct, "{"):
		body := p.parseBlockBody()
		return stmt{kind: sBlock, line: t.line, body: body}
	case t.kind == tIdent:
		switch t.text {
		case "if", "while", "for", "switch":
			p.next()
			s := stmt{kind: sControl, line: t.line, excerpt: p.f.excerpt(t.line), keyword: t.text, elseAt: -1}
			if p.peek().is(tPunct, "(") {
				c := p.matchClose(p.i)
				s.header = p.toks[p.i+1 : c]
				p.i = min(c+1, len(p.toks)-1)
			}
			s.body = append(s.body, p.parseStmt())
			if t.text == "if" && p.peek().is(tIdent, "else") {
				p.next()
				s.elseAt = len(s.body)
				s.body = append(s.body, p.parseStmt())
			}
			return s
		case "do":
			p.next()
			s := stmt{kind: sControl, line: t.line, excerpt: p.f.excerpt(t.line), keyword: "do", elseAt: -1}
			s.body = append(s.body, p.parseStmt())
			if p.peek().is(tIdent, "while") {
				p.next()
				if p.peek().is(tPunct, "(") {
					c := p.matchClose(p.i)
					s.header = p.toks[p.i+1 : c]
					p.i = min(c+1, len(p.toks)-1)
				}
			}
			if p.peek().is(tPunct, ";") {
				p.next()
			}
			return s
		case "return":
			p.skipToSemi()
			s := p.rawStmt(sReturn, start, p.i)
			s.keyword = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s.keyword, "return"), ";"))
			return s
		case "case", "default":
			for p.peek().kind != tEOF && !p.peek().is(tPunct, ":") && !p.peek().is(tPunct, "}") {
				p.next()
			}
			if p.peek().is(tPunct, ":") {
				p.next()
			}
			return p.rawStmt(sLabel, start, p.i)
		case "else":
			p.next()
			p.errorf(t.line, "'else' without 'if'")
			return p.parseStmt()
		}
		if p.peekAt(1).is(tPunct, ":") {
			p.next()
			p.next()
			return p.rawStmt(sLabel, start, p.i)
		}
		if p.peekAt(1).is(tPunct, "(") {
			c := p.matchClose(p.i + 1)
			if c+1 < len(p.toks) && p.toks[c+1].is(tPunct, ";") {
				call := p.parseCall(p.i, c)
				p.i = c + 2
				return stmt{kind: sCall, line: t.line, excerpt: p.f.excerpt(t.line), call: call}
			}
		}
		// rc = web_url(...);  -> call statement whose result is assigned
		if p.peekAt(1).is(tPunct, "=") && p.peekAt(2).kind == tIdent && p.peekAt(3).is(tPunct, "(") {
			c := p.matchClose(p.i + 3)
			if c+1 < len(p.toks) && p.toks[c+1].is(tPunct, ";") {
				call := p.parseCall(p.i+2, c)
				p.i = c + 2
				return stmt{kind: sCall, line: t.line, excerpt: p.f.excerpt(t.line), call: call, keyword: "assign:" + t.text}
			}
		}
		if declKeywords[t.text] || p.peekAt(1).kind == tIdent ||
			(p.peekAt(1).is(tPunct, "*") && p.peekAt(2).kind == tIdent && (p.peekAt(3).is(tPunct, "=") || p.peekAt(3).is(tPunct, ";") || p.peekAt(3).is(tPunct, "["))) {
			p.skipToSemi()
			return p.rawStmt(sDecl, start, p.i)
		}
	}
	p.skipToSemi()
	if p.i == start { // stray '}' or similar; consume one token to make progress
		p.next()
	}
	return p.rawStmt(sExpr, start, p.i)
}

// skipToSemi advances past the next ';' at depth 0, stopping before an
// unmatched '}' (so a missing ';' does not swallow the enclosing block).
func (p *parser) skipToSemi() {
	depth := 0
	for {
		t := p.peek()
		if t.kind == tEOF {
			return
		}
		if t.kind == tPunct {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]":
				depth--
			case "}":
				if depth <= 0 {
					return
				}
				depth--
			case ";":
				if depth <= 0 {
					p.next()
					return
				}
			}
		}
		p.next()
	}
}

// parseCall parses tokens[from] = name, tokens[from+1] = '(' ... tokens[close] = ')'.
func (p *parser) parseCall(from, close int) *cCall {
	c := &cCall{name: p.toks[from].text, line: p.toks[from].line}
	j := from + 2
	argStart := j
	depth := 0
	for ; j <= close; j++ {
		t := p.toks[j]
		if t.kind == tPunct {
			switch t.text {
			case "(", "[", "{":
				depth++
				continue
			case ")", "]", "}":
				if depth > 0 {
					depth--
					continue
				}
			case ",":
				if depth > 0 {
					continue
				}
			default:
				continue
			}
			// ',' at depth 0 or the final ')'
			if j > argStart || t.text == "," {
				c.args = append(c.args, p.parseArg(argStart, j))
			}
			argStart = j + 1
		}
	}
	return c
}

func (p *parser) parseArg(from, to int) cArg {
	if from >= to {
		return cArg{kind: aExpr, line: p.toks[min(from, len(p.toks)-1)].line}
	}
	toks := p.toks[from:to]
	a := cArg{line: toks[0].line}
	if toks[len(toks)-1].end <= len(p.f.src) && toks[0].pos <= toks[len(toks)-1].end {
		a.text = p.f.src[toks[0].pos:toks[len(toks)-1].end]
	}
	allStr := true
	for _, t := range toks {
		if t.kind != tString {
			allStr = false
			break
		}
	}
	switch {
	case allStr:
		var b strings.Builder
		for _, t := range toks {
			b.WriteString(t.val)
		}
		a.kind, a.str = aString, b.String()
	case len(toks) == 1 && toks[0].kind == tIdent:
		a.kind = aIdent
	case len(toks) == 1 && toks[0].kind == tNumber,
		len(toks) == 2 && toks[0].is(tPunct, "-") && toks[1].kind == tNumber:
		a.kind = aNumber
	case len(toks) >= 3 && toks[0].kind == tIdent && toks[1].is(tPunct, "(") && p.matchClose(from+1) == to-1:
		a.kind = aCall
		a.call = p.parseCall(from, to-1)
	default:
		a.kind = aExpr
	}
	return a
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// walkCalls visits every call in a statement list, including calls nested in
// arguments, control-flow bodies and blocks.
func walkCalls(stmts []stmt, fn func(*cCall)) {
	var visit func(c *cCall)
	visit = func(c *cCall) {
		fn(c)
		for _, a := range c.args {
			if a.call != nil {
				visit(a.call)
			}
		}
	}
	for i := range stmts {
		s := &stmts[i]
		if s.call != nil {
			visit(s.call)
		}
		walkCalls(s.body, fn)
	}
}
