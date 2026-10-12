package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/har"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	t.Setenv("LOADMETER_HOME", t.TempDir())
	s, err := New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func simple(name string) *model.Scenario {
	sc := model.NewScenario(name)
	sc.Defaults.BaseURL = "http://127.0.0.1:1"
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "r", Request: &model.Request{Method: "GET", URL: "/x"}}}
	return sc
}

func waitRun(t *testing.T, s *Service, id string) {
	t.Helper()
	a := s.Active(id)
	if a == nil {
		t.Fatalf("run %s not active", id)
	}
	select {
	case <-a.Run.Done():
	case <-time.After(30 * time.Second):
		a.Run.Stop()
		a.Run.Stop()
		t.Fatalf("run %s did not finish", id)
	}
	a.Wait() // wait until summary/report files are written (idempotent)
}

func TestNewCreatesWorkspaceLayout(t *testing.T) {
	s := newSvc(t)
	for _, d := range []string{"runs", "scenarios", "data", "history", "recordings", "imports"} {
		if st, err := os.Stat(filepath.Join(s.WS.Dir, d)); err != nil || !st.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(s.WS.Dir, "recordings")); st.Mode().Perm() != 0o700 {
		t.Errorf("recordings perms %v", st.Mode().Perm())
	}
}

func TestInvalidFileNamesRejected(t *testing.T) {
	s := newSvc(t)
	bad := []string{"", "../x.json", "..\\x.json", "a/b.json", "/abs.json", "x.txt", "x", ".hidden.json", "a..b.json", "x.json\x00", "x.json/", "-x.json",
		"has space.json", strings.Repeat("a", 120) + ".json", "..", "%2e%2e%2fx.json", "x.JSON"}
	sentinel := filepath.Join(filepath.Dir(s.WS.Dir), "x.json")
	for _, f := range bad {
		if _, err := s.GetScenario(f); !errors.Is(err, ErrInvalid) {
			t.Errorf("GetScenario(%q): %v", f, err)
		}
		if _, err := s.SaveScenario(f, simple("evil"), "t"); !errors.Is(err, ErrInvalid) {
			t.Errorf("SaveScenario(%q): %v", f, err)
		}
		if err := s.DeleteScenario(f); !errors.Is(err, ErrInvalid) {
			t.Errorf("DeleteScenario(%q): %v", f, err)
		}
		if _, err := s.History(f); !errors.Is(err, ErrInvalid) {
			t.Errorf("History(%q): %v", f, err)
		}
		if _, err := s.StartRun(RunRequest{File: f}); !errors.Is(err, ErrInvalid) {
			t.Errorf("StartRun(%q): %v", f, err)
		}
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Error("a traversal write escaped the workspace")
	}
	for _, f := range []string{"ok.json", "A_b-1.json", "a.b.json", "_x.json"} {
		if _, err := s.SaveScenario(f, simple("n"), "t"); err != nil {
			t.Errorf("valid name %q rejected: %v", f, err)
		}
	}
}

func TestSaveCreatesHistorySnapshots(t *testing.T) {
	s := newSvc(t)
	sc := simple("v1")
	if probs, err := s.SaveScenario("s.json", sc, "create"); err != nil || len(probs) != 0 {
		t.Fatalf("%v %v", probs, err)
	}
	if h, _ := s.History("s.json"); len(h) != 0 {
		t.Fatalf("first save must not create history: %v", h)
	}
	sc.Name = "v2"
	s.SaveScenario("s.json", sc, "Rename Step!")
	sc.Name = "v3"
	s.SaveScenario("s.json", sc, "")
	h, err := s.History("s.json")
	if err != nil || len(h) != 2 {
		t.Fatalf("%v %v", h, err)
	}
	if h[0].ID <= h[1].ID {
		t.Errorf("history not newest-first: %v", h)
	}
	if h[0].Reason != "edit" || h[1].Reason != "rename-step-" {
		t.Errorf("reasons %q %q", h[0].Reason, h[1].Reason)
	}
	if h[0].Time.IsZero() || time.Since(h[0].Time) > time.Minute {
		t.Errorf("time %v", h[0].Time)
	}
	// each snapshot holds the *previous* contents
	old, _ := s.historyScenario("s.json", h[1].ID)
	mid, _ := s.historyScenario("s.json", h[0].ID)
	if old.Name != "v1" || mid.Name != "v2" {
		t.Errorf("snapshot names %q %q", old.Name, mid.Name)
	}
	// validation problems are returned, but the file is still saved
	bad := model.NewScenario("bad")
	probs, err := s.SaveScenario("bad.json", bad, "t")
	if err != nil || len(probs) == 0 {
		t.Errorf("probs=%v err=%v", probs, err)
	}
	if _, err := s.GetScenario("bad.json"); err != nil {
		t.Errorf("invalid-but-saved: %v", err)
	}
	if got, _ := s.GetScenario("s.json"); got.Version != model.SchemaVersion {
		t.Errorf("version %q", got.Version)
	}
}

func TestDiffAndRestore(t *testing.T) {
	s := newSvc(t)
	sc := simple("orig")
	s.SaveScenario("s.json", sc, "create")
	sc.Name = "changed"
	sc.Workload.VUs = 9
	sc.Steps[0].Request.URL = "/y"
	s.SaveScenario("s.json", sc, "tweak")
	h, _ := s.History("s.json")
	id := h[0].ID

	ch, err := s.Diff("s.json", id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, c := range ch {
		got[c.Path] = c
	}
	if c := got["$.name"]; c.Before != "orig" || c.After != "changed" {
		t.Errorf("name change %+v", c)
	}
	if c := got["$.workload.vus"]; c.Before != float64(1) || c.After != float64(9) {
		t.Errorf("vus change %+v", c)
	}
	if c := got["$.steps[0].request.url"]; c.Before != "/x" || c.After != "/y" {
		t.Errorf("url change %+v (%v)", c, ch)
	}
	if len(ch) != 3 {
		t.Errorf("unexpected extra changes: %+v", ch)
	}

	if err := s.Restore("s.json", id); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.GetScenario("s.json")
	if cur.Name != "orig" || cur.Workload.VUs != 1 || cur.Steps[0].Request.URL != "/x" {
		t.Errorf("not restored: %+v", cur)
	}
	h2, _ := s.History("s.json")
	if len(h2) != 2 || h2[0].Reason != "restore" {
		t.Errorf("restore should snapshot the replaced version: %v", h2)
	}
	if ch, _ := s.Diff("s.json", id); len(ch) != 0 {
		t.Errorf("diff between the restored snapshot and current must be empty: %+v", ch)
	}
	// unknown / malformed ids
	if err := s.Restore("s.json", "20200101T000000.000000000_edit"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	for _, bad := range []string{"../../etc/passwd", "x", "", "20200101T000000.000000000_Edit", "1_a/../b"} {
		if err := s.Restore("s.json", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Restore id %q: %v", bad, err)
		}
		if _, err := s.Diff("s.json", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Diff id %q: %v", bad, err)
		}
	}
}

func TestDeleteKeepsHistory(t *testing.T) {
	s := newSvc(t)
	sc := simple("keep")
	s.SaveScenario("d.json", sc, "create")
	sc.Name = "keep2"
	s.SaveScenario("d.json", sc, "edit")
	if err := s.DeleteScenario("d.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetScenario("d.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	h, _ := s.History("d.json")
	if len(h) != 2 || h[0].Reason != "deleted" {
		t.Fatalf("history after delete: %v", h)
	}
	// the deleted snapshot can be diffed (against empty) and restored
	if ch, err := s.Diff("d.json", h[0].ID); err != nil || len(ch) == 0 {
		t.Errorf("diff after delete: %v %v", ch, err)
	}
	if err := s.Restore("d.json", h[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetScenario("d.json"); err != nil || got.Name != "keep2" {
		t.Errorf("restored after delete: %v %v", got, err)
	}
	if err := s.DeleteScenario("nope.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing: %v", err)
	}
	if h, err := s.History("never.json"); err != nil || h == nil || len(h) != 0 {
		t.Errorf("history of unknown file: %v %v", h, err)
	}
}

func TestHistoryFileTraversalRejected(t *testing.T) {
	s := newSvc(t)
	// A snapshot-looking file placed outside history/ must not be readable via ../ in the file name.
	outside := filepath.Join(s.WS.Dir, "evil")
	os.MkdirAll(outside, 0o755)
	id := "20240101T000000.000000000_edit"
	os.WriteFile(filepath.Join(outside, id+".json"), []byte(`{"name":"LEAKED","steps":[]}`), 0o644)
	if ch, err := s.Diff("../evil.json", id); !errors.Is(err, ErrInvalid) {
		t.Fatalf("BUG: service.go Diff/Restore (historyScenario) never validate `file`, so Diff(\"../evil.json\", id) reads %s/<id>.json outside history/ (changes=%d err=%v)", outside, len(ch), err)
	}
	if err := s.Restore("../evil.json", id); !errors.Is(err, ErrInvalid) {
		t.Errorf("Restore traversal: %v", err)
	}
}

func TestListScenarios(t *testing.T) {
	s := newSvc(t)
	s.SaveScenario("good.json", simple("Good"), "c")
	os.WriteFile(filepath.Join(s.WS.ScenariosDir(), "broken.json"), []byte(`{"name": `), 0o644)
	os.WriteFile(filepath.Join(s.WS.ScenariosDir(), "notes.txt"), []byte(`x`), 0o644)
	os.WriteFile(filepath.Join(s.WS.ScenariosDir(), "users.csv"), []byte(`x`), 0o644)
	os.MkdirAll(filepath.Join(s.WS.ScenariosDir(), "dir.json"), 0o755)
	list, err := s.ListScenarios()
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %v", list, err)
	}
	by := map[string]ScenarioInfo{}
	for _, i := range list {
		by[i.File] = i
	}
	g := by["good.json"]
	if g.Name != "Good" || g.Requests != 1 || g.Executor != model.ExecConstantVUs || g.MaxVUs != 1 || g.Duration != "30s" || g.Error != "" || g.Modified.IsZero() {
		t.Errorf("%+v", g)
	}
	if by["broken.json"].Error == "" {
		t.Errorf("broken scenario should report an error: %+v", by["broken.json"])
	}
}

func TestImportHARAndCorrelate(t *testing.T) {
	s := newSvc(t)
	data, err := os.ReadFile("../../testdata/har/shop.har")
	if err != nil {
		t.Fatal(err)
	}
	imp, err := s.ImportHAR("Shop Recording", data, har.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(imp.File, ".json") || imp.Report == nil {
		t.Fatalf("%+v", imp)
	}
	sc, err := s.GetScenario(imp.File)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sc.AllRequests()); n < 5 {
		t.Errorf("imported %d requests", n)
	}
	if _, err := os.Stat(s.recordingPath(imp.File)); err != nil {
		t.Errorf("recording not stored: %v", err)
	}
	// static assets are filtered by default
	for _, st := range sc.AllRequests() {
		if strings.HasSuffix(st.Request.URL, ".js") {
			t.Errorf("static asset imported: %s", st.Request.URL)
		}
	}
	res, err := s.Correlate(imp.File)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("expected correlation candidates (csrf/token/orderId) in shop.har")
	}
	if len(imp.Correlations) != len(res.Candidates) {
		t.Errorf("import-time and on-demand correlation differ: %d vs %d", len(imp.Correlations), len(res.Candidates))
	}
	vars := map[string]bool{}
	for _, c := range res.Candidates {
		vars[c.Variable] = true
		if c.Confidence <= 0 || c.Confidence > 1 || len(c.Destinations) == 0 || c.ID == "" {
			t.Errorf("bad candidate %+v", c)
		}
	}
	t.Logf("correlation variables: %v", vars)

	// a second import of the same name gets a distinct file
	imp2, err := s.ImportHAR("Shop Recording", data, har.Options{})
	if err != nil || imp2.File == imp.File {
		t.Errorf("unique file names: %v %v", imp2, err)
	}

	// suggest => no change; manual with nothing selected => invalid; manual with ids => applied + snapshot
	r, err := s.ApplyCorrelations(context.Background(), imp.File, nil, "suggest")
	if err != nil || len(r.Applied) != 0 {
		t.Errorf("suggest: %v %v", r, err)
	}
	if _, err := s.ApplyCorrelations(context.Background(), imp.File, nil, "manual"); !errors.Is(err, ErrInvalid) {
		t.Errorf("manual/no ids: %v", err)
	}
	if _, err := s.ApplyCorrelations(context.Background(), imp.File, []string{"x"}, "yolo"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown policy: %v", err)
	}
	before, _ := s.GetScenario(imp.File)
	ar, err := s.ApplyCorrelations(context.Background(), imp.File, []string{res.Candidates[0].ID}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetScenario(imp.File)
	if len(ar.Applied) == 0 || ar.RolledBack || ar.Validation != nil {
		t.Errorf("%+v", ar)
	}
	ja, jb := marshal(before), marshal(after)
	if ja == jb {
		t.Error("manual apply did not change the scenario")
	}
	if h, _ := s.History(imp.File); len(h) < 1 || h[0].Reason != "correlation" {
		t.Errorf("history %v", h)
	}
}

func TestImportHARInvalid(t *testing.T) {
	s := newSvc(t)
	for _, in := range []string{"", "not json", `{"log":{}}`, `{"log":{"entries":[]}}`} {
		if _, err := s.ImportHAR("x", []byte(in), har.Options{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", in, err)
		}
	}
	if list, _ := s.ListScenarios(); len(list) != 0 {
		t.Errorf("failed import left scenarios: %v", list)
	}
}

func TestCorrelateErrors(t *testing.T) {
	s := newSvc(t)
	if _, err := s.Correlate("missing.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	s.SaveScenario("manual.json", simple("m"), "c")
	if _, err := s.Correlate("manual.json"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no recording") {
		t.Errorf("no recording: %v", err)
	}
}

func TestPolicyIncludesExtraAllowed(t *testing.T) {
	s := newSvc(t)
	s.ExtraAllowed = []string{"extra.example.test"}
	if err := safety.SavePolicy(safety.Policy{AllowedHosts: []string{"saved.example.test"}, MaxVUs: 7}); err != nil {
		t.Fatal(err)
	}
	p := s.Policy()
	if !p.HostAllowed("extra.example.test") || !p.HostAllowed("saved.example.test") || p.HostAllowed("other.example.test") || p.MaxVUs != 7 {
		t.Errorf("%+v", p)
	}
	// corrupt policy falls back to defaults (never to "allow everything")
	os.WriteFile(filepath.Join(safety.ConfigDir(), "policy.json"), []byte("{bad"), 0o600)
	p = s.Policy()
	if p.HostAllowed("saved.example.test") || p.MaxVUs != 1000 {
		t.Errorf("corrupt policy: %+v", p)
	}
}

func TestStartRunValidation(t *testing.T) {
	s := newSvc(t)
	if _, err := s.StartRun(RunRequest{File: "missing.json"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	sc := simple("remote")
	sc.Defaults.BaseURL = "http://not-allowlisted.example.test"
	s.SaveScenario("remote.json", sc, "c")
	_, err := s.StartRun(RunRequest{File: "remote.json"})
	var hna *safety.ErrHostNotAllowed
	if !errors.As(err, &hna) || hna.Host != "not-allowlisted.example.test" {
		t.Errorf("non-allowlisted: %v", err)
	}
	// allowlisted + large => needs confirmation
	s.ExtraAllowed = []string{"not-allowlisted.example.test"}
	var nc *app.ErrNeedsConfirmation
	_, err = s.StartRun(RunRequest{File: "remote.json", Overrides: app.Overrides{VUs: 200, Duration: "1s"}})
	if !errors.As(err, &nc) {
		t.Errorf("confirmation: %v", err)
	}
	// bad overrides / baseline
	s.SaveScenario("local.json", simple("local"), "c")
	if _, err := s.StartRun(RunRequest{File: "local.json", Overrides: app.Overrides{Duration: "xyz"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("overrides: %v", err)
	}
	if _, err := s.StartRun(RunRequest{File: "local.json", Baseline: "../../x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("baseline: %v", err)
	}
	// policy caps are enforced by the engine
	safety.SavePolicy(safety.Policy{MaxVUs: 3, MaxDuration: "1h", RequireConfirmAbove: 50})
	if _, err := s.StartRun(RunRequest{File: "local.json", Overrides: app.Overrides{VUs: 10, Duration: "1s"}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "safety policy") {
		t.Errorf("policy cap: %v", err)
	}
	if info := s.ActiveRun(); info.RunID != "" {
		t.Errorf("failed starts must not become active: %+v", info)
	}
}

func TestStartRunWithVarsOnScenarioWithoutVariables(t *testing.T) {
	s := newSvc(t)
	sc := simple("novars")
	sc.Variables = nil
	s.SaveScenario("nv.json", sc, "c")
	got, _ := s.GetScenario("nv.json")
	if got.Variables != nil {
		t.Skip("scenario round trip produced a non-nil Variables map; nothing to test")
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BUG: service.go StartRun/Debug write `sc.Variables[k] = v` on a nil map (scenario JSON without \"variables\") and panic: %v", r)
			}
		}()
		id, err := s.StartRun(RunRequest{File: "nv.json", Vars: map[string]string{"a": "b"}, Overrides: app.Overrides{Duration: "1s"}})
		if err == nil {
			s.StopRun(id)
		}
	}()
}

func TestDemoRunLifecycle(t *testing.T) {
	s := newSvc(t)
	file, url, err := s.Demo()
	if err != nil {
		t.Fatal(err)
	}
	if file != "demo-shop.json" || !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("%q %q", file, url)
	}
	if _, url2, _ := s.Demo(); url2 != url {
		t.Errorf("mock server should be reused: %s vs %s", url, url2)
	}
	if _, err := os.Stat(filepath.Join(s.WS.ScenariosDir(), "users.csv")); err != nil {
		t.Errorf("users.csv: %v", err)
	}
	sc, err := s.GetScenario(file)
	if err != nil || sc.Variables["baseUrl"] != url {
		t.Fatalf("%v %v", sc, err)
	}

	id, err := s.StartRun(RunRequest{File: file, Overrides: app.Overrides{VUs: 2, Duration: "2s"}})
	if err != nil {
		t.Fatal(err)
	}
	if !app.ValidRunID(id) {
		t.Errorf("run id %q", id)
	}
	if info := s.ActiveRun(); info.RunID != id || info.State != "running" || info.Scenario == "" || info.Planned != "2s" {
		t.Errorf("%+v", info)
	}
	// ErrBusy while running; summary not available yet
	if _, err := s.StartRun(RunRequest{File: file, Overrides: app.Overrides{VUs: 1, Duration: "1s"}}); !errors.Is(err, ErrBusy) {
		t.Errorf("second start: %v", err)
	}
	if _, err := s.Summary(id); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "in progress") {
		t.Errorf("summary during run: %v", err)
	}
	waitRun(t, s, id)
	sum, err := s.Summary(id)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != "finished" || sum.Snapshot.Requests.Count < 10 || sum.Snapshot.Iterations < 1 {
		t.Errorf("summary %+v", sum.Snapshot.Requests)
	}
	if sum.Snapshot.Requests.Errors != 0 {
		t.Errorf("demo run against the bundled mock should be clean: %v", sum.Snapshot.Requests.ErrorKind)
	}
	for _, f := range []string{"summary.json", "summary.csv", "report.html", "raw.csv", "scenario.json"} {
		if _, err := os.Stat(filepath.Join(s.WS.RunsDir(), id, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	// after completion a new run may start; list/compare work
	time.Sleep(1100 * time.Millisecond) // run IDs have 1-second resolution (see TestRunIDsUniqueWithinOneSecond)
	id2, err := s.StartRun(RunRequest{File: file, Overrides: app.Overrides{VUs: 1, Duration: "1s"}, Baseline: id})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, s, id2)
	sum2, err := s.Summary(id2)
	if err != nil || sum2.Baseline == nil {
		t.Fatalf("baseline: %v %v", sum2, err)
	}
	runs, err := s.ListRuns()
	if err != nil || len(runs) != 2 || runs[0].RunID != id2 || runs[0].Requests == 0 {
		t.Errorf("%v %v", runs, err)
	}
	cmp, err := s.Compare(id, id2)
	if err != nil || cmp.BaselineRun != id {
		t.Errorf("%v %v", cmp, err)
	}
	// validation of ids
	if _, err := s.Summary("../x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid id: %v", err)
	}
	if _, err := s.Summary("20200101-000000-nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := s.RunDir("a/b"); !errors.Is(err, ErrInvalid) {
		t.Errorf("RunDir: %v", err)
	}
	if err := s.StopRun("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("StopRun unknown: %v", err)
	}
}

func TestStopRun(t *testing.T) {
	s := newSvc(t)
	file, _, _ := s.Demo()
	id, err := s.StartRun(RunRequest{File: file, Overrides: app.Overrides{VUs: 1, Duration: "30s"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := s.StopRun(id); err != nil {
		t.Fatal(err)
	}
	waitRun(t, s, id)
	if time.Since(start) > 10*time.Second {
		t.Errorf("stop took %v", time.Since(start))
	}
	sum, err := s.Summary(id)
	if err != nil || sum.DurationS > 20 {
		t.Errorf("%v %v", sum, err)
	}
}

func TestRunIDsUniqueWithinOneSecond(t *testing.T) {
	s := newSvc(t)
	file, _, _ := s.Demo()
	id1, err := s.StartRun(RunRequest{File: file, Overrides: app.Overrides{VUs: 1, Duration: "1s"}})
	if err != nil {
		t.Fatal(err)
	}
	s.StopRun(id1)
	waitRun(t, s, id1)
	id2, err := s.StartRun(RunRequest{File: file, Overrides: app.Overrides{VUs: 1, Duration: "1s"}})
	if err != nil {
		t.Fatal(err)
	}
	s.StopRun(id2)
	waitRun(t, s, id2)
	if id1 == id2 {
		t.Fatalf("BUG: results.NewRunID has 1-second resolution, so two runs of the same scenario started within one second share run id %q and overwrite each other's directory/Active entry", id1)
	}
}

func TestDebugRedactsSecrets(t *testing.T) {
	s := newSvc(t)
	file, _, _ := s.Demo()
	exs, err := s.Debug(context.Background(), file, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(exs) < 5 {
		t.Fatalf("exchanges=%d", len(exs))
	}
	sawAuth := false
	for _, e := range exs {
		if a, ok := e.ReqHeaders["Authorization"]; ok {
			sawAuth = true
			if !strings.HasPrefix(a, "[REDACTED") {
				t.Errorf("Authorization leaked: %q", a)
			}
		}
		if strings.Contains(e.ReqBody, "secret") && strings.Contains(e.ReqBody, "password") && !strings.Contains(e.ReqBody, "REDACTED") {
			t.Errorf("password leaked in %q", e.ReqBody)
		}
	}
	if !sawAuth {
		t.Error("no request carried an Authorization header; correlation of token failed?")
	}
}

func TestExportJMXRoundTripSmoke(t *testing.T) {
	s := newSvc(t)
	file, _, _ := s.Demo()
	b, rep, err := s.ExportJMX(file)
	if err != nil || rep == nil || !strings.Contains(string(b), "jmeterTestPlan") {
		t.Errorf("%v %v", err, rep)
	}
	if _, _, err := s.ExportJMX("nope.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func marshal(sc *model.Scenario) string {
	b, _ := jsonMarshal(sc)
	return string(b)
}
