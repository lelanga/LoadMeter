package extract

import "testing"

func TestRegexAllAndTemplate(t *testing.T) {
	ms, err := RegexAll("a=1;b=22;c=333", `(\w)=(\d+)`, 2)
	if err != nil || len(ms) != 2 {
		t.Fatalf("%v %v", ms, err)
	}
	m := ms[1]
	for tpl, want := range map[string]string{"$2$-$1$": "22-b", "$0$": "b=22", "[$9$]": "[]", "$x$1$": "$xb", "no groups": "no groups",
		"$1": "$1", "$$1$$": "$b$", "$12$": ""} {
		if got := m.Template(tpl); got != want {
			t.Errorf("Template(%q) = %q want %q", tpl, got, want)
		}
	}
	if m.Group(2) != "22" || m.Group(5) != "" || m.Group(-1) != "" {
		t.Errorf("Group: %v", m.Groups)
	}
	if all, _ := RegexAll("a=1;b=22;c=333", `(\w)=(\d+)`, 0); len(all) != 3 {
		t.Errorf("unlimited: %d", len(all))
	}
	if _, err := RegexAll("x", `(`, 0); err == nil {
		t.Error("bad regex accepted")
	}
}
