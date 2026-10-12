package loadrunner

import (
	"os"
	"strings"
	"testing"
)

func TestLexStringsAndComments(t *testing.T) {
	src := "/* block { \"not a string\" \n spans */ a(\"x\\\"y\\n\" \"z\", 'c'); // tail \"q\"\n#include \"globals.h\"\nb(\"\\x41\\101\");"
	toks, errs := lex([]byte(src))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var strs []string
	var lines []int
	var dirs []string
	for _, tk := range toks {
		switch tk.kind {
		case tString:
			strs = append(strs, tk.val)
			lines = append(lines, tk.line)
		case tDirective:
			dirs = append(dirs, tk.text)
		}
	}
	want := []string{"x\"y\n", "z", "AA"}
	if strings.Join(strs, "|") != strings.Join(want, "|") {
		t.Fatalf("strings = %q, want %q", strs, want)
	}
	if lines[0] != 2 || lines[2] != 4 {
		t.Fatalf("string lines = %v, want [2 2 4]", lines)
	}
	if len(dirs) != 1 || dirs[0] != `#include "globals.h"` {
		t.Fatalf("directives = %q", dirs)
	}
}

func TestLexUnterminated(t *testing.T) {
	_, errs := lex([]byte("a(\"open\n);\n/* never closed"))
	if len(errs) != 2 {
		t.Fatalf("want 2 errors (string, comment), got %v", errs)
	}
}

func TestDecodeBinaryEscapes(t *testing.T) {
	if got := decodeBinaryEscapes(`\x41\x42-\\x\x4`); got != `AB-\x\x4` {
		t.Fatalf("got %q", got)
	}
}

func TestParseCallsAndControlFlow(t *testing.T) {
	src := `#include "as_web.h"
int g = 1;
helper(int n) { return n * 2; }
Action()
{
	char *p;
	web_url("a",
		"URL=http://h/"
		"x?y={P}",   /* adjacent literals */
		LAST);
	if (g > 0) {
		web_url("b", "URL=http://h/b", LAST);
	} else
		lr_think_time(1);
	rc = web_url("c", "URL=http://h/c", LAST);
	lr_save_string(lr_eval_string("{P}"), "Q");
	switch (g) { case 1: break; default: break; }
	do { g--; } while (g > 0);
	return 0;
}
`
	f := parseC("Action.c", []byte(src))
	if len(f.errors) != 0 {
		t.Fatalf("errors: %v", f.errors)
	}
	if len(f.funcs) != 2 || f.funcs[0].name != "helper" || f.funcs[1].name != "Action" {
		t.Fatalf("funcs = %+v", f.funcs)
	}
	if len(f.topLevel) != 1 || f.topLevel[0].kind != sDecl || f.topLevel[0].line != 2 {
		t.Fatalf("topLevel = %+v", f.topLevel)
	}
	body := f.fn("Action").body
	kinds := []stmtKind{}
	for _, s := range body {
		kinds = append(kinds, s.kind)
	}
	want := []stmtKind{sDecl, sCall, sControl, sCall, sCall, sControl, sControl, sReturn}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("stmt %d kind = %v, want %v", i, kinds[i], want[i])
		}
	}
	c := body[1].call
	if c.name != "web_url" || c.line != 7 || len(c.args) != 3 {
		t.Fatalf("call = %+v", c)
	}
	if c.args[1].kind != aString || c.args[1].str != "URL=http://h/x?y={P}" {
		t.Fatalf("concatenated arg = %+v", c.args[1])
	}
	if c.args[2].kind != aIdent || c.args[2].text != "LAST" {
		t.Fatalf("LAST arg = %+v", c.args[2])
	}
	ifs := body[2]
	if ifs.keyword != "if" || ifs.line != 11 || ifs.elseAt != 1 || len(ifs.body) != 2 {
		t.Fatalf("if = %+v", ifs)
	}
	if body[3].keyword != "assign:rc" || body[3].call.name != "web_url" {
		t.Fatalf("assignment call = %+v", body[3])
	}
	nested := body[4].call.args[0]
	if nested.kind != aCall || nested.call.name != "lr_eval_string" {
		t.Fatalf("nested = %+v", nested)
	}
	if s, eval, ok := strArg(nested); !ok || !eval || s != "{P}" {
		t.Fatalf("strArg(lr_eval_string) = %q %v %v", s, eval, ok)
	}
	if body[7].keyword != "0" {
		t.Fatalf("return value = %q", body[7].keyword)
	}
}

// The parser must survive arbitrary truncation of a real script.
func TestParseTruncatedNeverPanics(t *testing.T) {
	src, err := os.ReadFile("../../testdata/loadrunner/webtours_sample/Action.c")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= len(src); i += 7 {
		f := parseC("Action.c", src[:i])
		_ = f
	}
	files := map[string][]byte{"Action.c": nil}
	for i := 0; i <= len(src); i += 97 {
		files["Action.c"] = src[:i]
		if _, _, err := ImportFiles(files, "trunc"); err != nil {
			t.Fatalf("truncated at %d: %v", i, err)
		}
	}
}

func FuzzParseC(f *testing.F) {
	f.Add([]byte(`Action(){ web_url("a","URL=http://x/",LAST); return 0; }`))
	f.Add([]byte(`x(){ if (a) { b(); } else for(;;) {} "unterminated`))
	f.Add([]byte("#define X \\\n 1\n}}}{{{(((;;;"))
	f.Fuzz(func(t *testing.T, b []byte) {
		parseC("fuzz.c", b)
		_, _, _ = ImportFiles(map[string][]byte{"Action.c": b}, "fuzz")
	})
}
