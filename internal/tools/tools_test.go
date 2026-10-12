package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/results"
	"github.com/lelanga/phoenix/internal/safety"
	"github.com/lelanga/phoenix/internal/service"
)

func newSvc(t *testing.T) *service.Service {
	t.Helper()
	t.Setenv("PHOENIX_HOME", t.TempDir())
	s, err := service.New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestImportPathRoots(t *testing.T) {
	s := newSvc(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	outside := t.TempDir()
	imports := filepath.Join(s.WS.Dir, "imports")
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	cwdReal, _ := os.Getwd() // may differ from cwd by symlinks (macOS /private)

	allowed := []string{
		filepath.Join(imports, "a.har"),
		filepath.Join(imports, "sub", "b.har"),
		filepath.Join(imports, "sub", "..", "c.har"),
		"x.har",
		"./sub/y.jmx",
		filepath.Join(cwd, "z.har"),
		filepath.Join(cwdReal, "w.har"),
	}
	for _, p := range allowed {
		got, err := importPath(s, p)
		if err != nil {
			t.Errorf("importPath(%q) rejected: %v", p, err)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("importPath(%q) = %q, want absolute", p, got)
		}
	}
	denied := []string{
		"",
		filepath.Join(outside, "a.har"),
		"/etc/passwd",
		"../a.har",
		"../../etc/passwd",
		"sub/../../a.har",
		filepath.Join(imports, "..", "scenarios", "s.json"),
		filepath.Join(imports, "..", "..", "x"),
		imports + "-evil/a.har",         // shared prefix, different directory
		filepath.Join(cwd+"-evil", "a"), // ditto for cwd
		abs(filepath.Join(os.Getenv("PHOENIX_HOME"), "keys.json")),
		filepath.Join(s.WS.Dir, "scenarios", "s.json"),
		filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"),
	}
	for _, p := range denied {
		if got, err := importPath(s, p); err == nil {
			t.Errorf("importPath(%q) = %q, want rejection", p, got)
		}
	}
	// the error names the allowed folder but is not a file oracle (same text whether or not the file exists)
	_, e1 := importPath(s, "/etc/passwd")
	_, e2 := importPath(s, "/etc/this-does-not-exist")
	if e1 == nil || e2 == nil || strings.ReplaceAll(e1.Error(), "passwd", "X") != strings.ReplaceAll(e2.Error(), "this-does-not-exist", "X") {
		t.Errorf("errors differ: %v / %v", e1, e2)
	}
}

func TestImportPathSymlinkEscape(t *testing.T) {
	s := newSvc(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	outside := filepath.Join(t.TempDir(), "secret.har")
	os.WriteFile(outside, []byte("TOP SECRET"), 0o600)
	link := filepath.Join(cwd, "link.har")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := importPath(s, "link.har"); err == nil {
		t.Fatal("BUG (hardening): tools.go importPath checks the lexical path only, so a symlink inside the current directory/imports folder pointing outside it passes the allowed-roots check and is then read by os.ReadFile")
	}
}

func TestImportToolsEnforceRoots(t *testing.T) {
	s := newSvc(t)
	harPath, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "har", "shop.har"))
	t.Chdir(t.TempDir())
	outside := filepath.Join(t.TempDir(), "x.har")
	os.WriteFile(outside, []byte(`{}`), 0o600)
	for _, tool := range []string{"import_har", "import_jmeter", "import_loadrunner"} {
		_, err := Call(context.Background(), s, tool, map[string]any{"path": outside}, "test")
		if err == nil || !strings.Contains(err.Error(), "outside the allowed import folders") {
			t.Errorf("%s: %v", tool, err)
		}
		if _, err := Call(context.Background(), s, tool, map[string]any{"path": "/etc/passwd"}, "test"); err == nil {
			t.Errorf("%s read /etc/passwd", tool)
		}
		if _, err := Call(context.Background(), s, tool, map[string]any{}, "test"); err == nil || !strings.Contains(err.Error(), "missing required") {
			t.Errorf("%s without path: %v", tool, err)
		}
	}
	// the happy path: a HAR copied into the allowed folder
	har, err := os.ReadFile(harPath)
	if err != nil {
		t.Fatal(err)
	}
	imports := filepath.Join(s.WS.Dir, "imports")
	os.WriteFile(filepath.Join(imports, "shop.har"), har, 0o600)
	out, err := Call(context.Background(), s, "import_har", map[string]any{"path": filepath.Join(imports, "shop.har"), "name": "Imported"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	imp, ok := out.(*service.HARImport)
	if !ok || imp.File == "" {
		t.Fatalf("%T %+v", out, out)
	}
	if _, err := s.GetScenario(imp.File); err != nil {
		t.Error(err)
	}
}

func schemaOf(t *testing.T, name string) Tool {
	t.Helper()
	tl, ok := Find(name)
	if !ok {
		t.Fatalf("tool %s missing", name)
	}
	return tl
}

func TestCheckArgs(t *testing.T) {
	run := schemaOf(t, "run_test")
	apply := schemaOf(t, "apply_correlations")
	params := schemaOf(t, "apply_parameters")
	list := schemaOf(t, "list_scenarios")
	tests := []struct {
		name string
		tool Tool
		args map[string]any
		want string // substring of error; "" = ok
	}{
		{"ok minimal", run, map[string]any{"file": "a.json"}, ""},
		{"ok all", run, map[string]any{"file": "a.json", "vus": 5.0, "duration": "1m", "rampUp": "10s", "rate": 2.5, "baseline": "x", "wait": true, "confirm": true}, ""},
		{"int accepted as number", run, map[string]any{"file": "a.json", "vus": 5}, ""},
		{"missing required", run, map[string]any{"vus": 5.0}, `missing required argument "file"`},
		{"unknown arg", run, map[string]any{"file": "a.json", "bogus": 1}, `unknown argument "bogus"`},
		{"string given number", run, map[string]any{"file": 5.0}, `"file" must be a string`},
		{"number given string", run, map[string]any{"file": "a.json", "vus": "5"}, `"vus" must be a number`},
		{"bool given string", run, map[string]any{"file": "a.json", "confirm": "true"}, `"confirm" must be a boolean`},
		{"bool given number", run, map[string]any{"file": "a.json", "wait": 1.0}, `"wait" must be a boolean`},
		{"null value", run, map[string]any{"file": nil}, `"file" must be a string`},
		{"array given string", params, map[string]any{"file": "a.json", "ids": "x"}, `"ids" must be a array`},
		{"array ok", params, map[string]any{"file": "a.json", "ids": []any{"a"}}, ""},
		{"missing second required", params, map[string]any{"file": "a.json"}, `missing required argument "ids"`},
		{"enum ok", apply, map[string]any{"file": "a.json", "policy": "manual"}, ""},
		{"enum violation", apply, map[string]any{"file": "a.json", "policy": "yolo"}, "must be one of"},
		{"enum wrong type", apply, map[string]any{"file": "a.json", "policy": 3.0}, `"policy" must be a string`},
		{"no-arg tool rejects args", list, map[string]any{"x": 1}, "unknown argument"},
		{"no-arg tool ok", list, map[string]any{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkArgs(tc.tool, tc.args)
			if tc.want == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("err=%v want containing %q", err, tc.want)
			}
		})
	}
}

func TestRegistryInvariants(t *testing.T) {
	seen := map[string]bool{}
	nameRe := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, tl := range Registry() {
		if !nameRe.MatchString(tl.Name) {
			t.Errorf("bad name %q", tl.Name)
		}
		if seen[tl.Name] {
			t.Errorf("duplicate tool %s", tl.Name)
		}
		seen[tl.Name] = true
		if len(tl.Description) < 20 || tl.Handler == nil {
			t.Errorf("%s: description/handler missing", tl.Name)
		}
		if tl.InputSchema["type"] != "object" || tl.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: schema must be a closed object: %v", tl.Name, tl.InputSchema)
		}
		props, _ := tl.InputSchema["properties"].(map[string]any)
		if props == nil {
			t.Errorf("%s: no properties map", tl.Name)
		}
		if req, ok := tl.InputSchema["required"].([]string); ok {
			for _, r := range req {
				if _, ok := props[r]; !ok {
					t.Errorf("%s: required %q not in properties", tl.Name, r)
				}
			}
		}
		for pn, pv := range props {
			pm, _ := pv.(map[string]any)
			if pm["type"] == nil {
				t.Errorf("%s.%s: no type", tl.Name, pn)
			}
		}
		if tl.Confirm {
			if _, ok := props["confirm"]; !ok {
				t.Errorf("%s: Confirm tool must expose a confirm property", tl.Name)
			}
			if tl.ReadOnly {
				t.Errorf("%s: cannot be both ReadOnly and Confirm", tl.Name)
			}
		}
	}
	for _, name := range []string{"list_scenarios", "get_scenario", "import_har", "suggest_correlations", "apply_correlations", "set_workload",
		"debug_scenario", "run_test", "run_status", "stop_test", "list_runs", "get_results", "compare_runs", "delete_scenario", "create_demo"} {
		if !seen[name] {
			t.Errorf("expected tool %s", name)
		}
	}
	// load-generating and destructive tools must be confirm-gated
	for _, name := range []string{"run_test", "delete_scenario"} {
		if !schemaOf(t, name).Confirm {
			t.Errorf("%s must require confirmation", name)
		}
	}
	// read-only tools must not be able to mutate: none are Confirm
	for _, name := range []string{"list_scenarios", "get_scenario", "validate_scenario", "suggest_correlations", "debug_scenario", "run_status", "get_results"} {
		if !schemaOf(t, name).ReadOnly {
			t.Errorf("%s should be ReadOnly", name)
		}
	}
}

func TestCallEnforcesConfirm(t *testing.T) {
	s := newSvc(t)
	file, _, err := s.Demo()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range []map[string]any{
		{"file": file},
		{"file": file, "confirm": false},
	} {
		if _, err := Call(ctx, s, "run_test", args, "test"); !errors.Is(err, ErrNeedsConfirm) {
			t.Errorf("run_test %v: %v", args, err)
		}
	}
	if info := s.ActiveRun(); info.RunID != "" {
		t.Fatalf("a run started without confirmation: %+v", info)
	}
	if _, err := Call(ctx, s, "delete_scenario", map[string]any{"file": file}, "test"); !errors.Is(err, ErrNeedsConfirm) {
		t.Errorf("delete_scenario: %v", err)
	}
	if _, err := s.GetScenario(file); err != nil {
		t.Errorf("scenario deleted without confirmation: %v", err)
	}
	// argument validation happens before confirmation (so bad calls fail clearly)
	if _, err := Call(ctx, s, "run_test", map[string]any{"file": file, "vus": "many"}, "test"); err == nil || errors.Is(err, ErrNeedsConfirm) {
		t.Errorf("type error expected: %v", err)
	}
	// confirm=true with a run that waits for completion
	out, err := Call(ctx, s, "run_test", map[string]any{"file": file, "confirm": true, "vus": 1.0, "duration": "1s", "wait": true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	sum, ok := out.(*results.Summary)
	if !ok || sum.Status != "finished" || sum.Snapshot.Requests.Count == 0 {
		t.Fatalf("%T %+v", out, out)
	}
	// run_status of a finished run returns its summary; list_runs shows it
	if out, err := Call(ctx, s, "run_status", map[string]any{"runId": sum.RunID}, "test"); err != nil || out == nil {
		t.Errorf("run_status: %v", err)
	}
	if out, err := Call(ctx, s, "list_runs", nil, "test"); err != nil || len(out.([]service.RunInfo)) != 1 {
		t.Errorf("list_runs: %v %v", out, err)
	}
	// delete with confirm works and keeps history
	if _, err := Call(ctx, s, "delete_scenario", map[string]any{"file": file, "confirm": true}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetScenario(file); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("not deleted: %v", err)
	}
	if h, _ := s.History(file); len(h) == 0 {
		t.Error("delete should leave a history snapshot")
	}
}

func TestCallBasics(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	if _, err := Call(ctx, s, "no_such_tool", nil, "test"); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("unknown tool: %v", err)
	}
	if _, err := Call(ctx, s, "get_scenario", map[string]any{"file": "../x.json"}, "test"); !errors.Is(err, service.ErrInvalid) {
		t.Errorf("traversal: %v", err)
	}
	if _, err := Call(ctx, s, "get_scenario", map[string]any{"file": "missing.json"}, "test"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if out, err := Call(ctx, s, "list_scenarios", nil, "test"); err != nil || out == nil {
		t.Errorf("list_scenarios: %v", err)
	}
	// the caller name and tool are audited for non-read-only tools only
	if _, err := Call(ctx, s, "create_demo", nil, "unit-test-caller"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(safety.ConfigDir(), "audit.log"))
	if !strings.Contains(string(b), `"tool":"create_demo"`) || !strings.Contains(string(b), "unit-test-caller") {
		t.Errorf("audit log: %s", b)
	}
	before := len(b)
	Call(ctx, s, "list_scenarios", nil, "ro-caller")
	b2, _ := os.ReadFile(filepath.Join(safety.ConfigDir(), "audit.log"))
	if len(b2) != before {
		t.Errorf("read-only tool call should not be audited: %s", b2[before:])
	}
}

func TestSetWorkloadTool(t *testing.T) {
	s := newSvc(t)
	file, _, _ := s.Demo()
	ctx := context.Background()
	if _, err := Call(ctx, s, "set_workload", map[string]any{"file": file}, "test"); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Errorf("empty workload: %v", err)
	}
	out, err := Call(ctx, s, "set_workload", map[string]any{"file": file, "vus": 100.0, "duration": "10m", "rampUp": "2m"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := s.GetScenario(file)
	w := sc.Workload
	if w.Executor != model.ExecRampingVUs || w.MaxPlannedVUs() != 100 || w.PlannedDuration().Minutes() != 10 {
		t.Errorf("%+v (%v)", w, out)
	}
	if h, _ := s.History(file); len(h) == 0 || h[0].Reason != "workload" {
		t.Errorf("history %v", h)
	}
	if _, err := Call(ctx, s, "set_workload", map[string]any{"file": file, "duration": "banana"}, "test"); err == nil {
		t.Error("bad duration accepted")
	}
	// rate => open model
	if _, err := Call(ctx, s, "set_workload", map[string]any{"file": file, "rate": 20.0, "duration": "30s"}, "test"); err != nil {
		t.Fatal(err)
	}
	sc, _ = s.GetScenario(file)
	if sc.Workload.Executor != model.ExecArrivalRate || sc.Workload.Rate != 20 {
		t.Errorf("%+v", sc.Workload)
	}
}

func TestValidateScenarioTool(t *testing.T) {
	s := newSvc(t)
	sc := model.NewScenario("remote")
	sc.Defaults.BaseURL = "http://blocked.example.test"
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "r", Request: &model.Request{Method: "GET", URL: "/x"}}}
	s.SaveScenario("remote.json", sc, "c")
	out, err := Call(context.Background(), s, "validate_scenario", map[string]any{"file": "remote.json"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if b, _ := m["blocked"].([]string); len(b) != 1 || b[0] != "blocked.example.test" {
		t.Errorf("%v", m)
	}
}

func TestRunTestToolRefusesBlockedHostEvenWhenConfirmed(t *testing.T) {
	s := newSvc(t)
	sc := model.NewScenario("remote")
	sc.Defaults.BaseURL = "http://blocked.example.test"
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "r", Request: &model.Request{Method: "GET", URL: "/x"}}}
	s.SaveScenario("remote.json", sc, "c")
	_, err := Call(context.Background(), s, "run_test", map[string]any{"file": "remote.json", "confirm": true}, "test")
	var hna *safety.ErrHostNotAllowed
	if !errors.As(err, &hna) {
		t.Fatalf("confirm must not bypass the allowlist: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	a := map[string]any{"s": "x", "n": 3.0, "i": 4, "b": true, "arr": []any{"a", 2, "c"}}
	if getStr(a, "s") != "x" || getStr(a, "n") != "" || getStr(a, "none") != "" {
		t.Error("getStr")
	}
	if getNum(a, "n") != 3 || getNum(a, "i") != 4 || getNum(a, "s") != 0 {
		t.Error("getNum")
	}
	if !getBool(a, "b") || getBool(a, "s") {
		t.Error("getBool")
	}
	if got := getStrs(a, "arr"); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("getStrs %v", got)
	}
}
