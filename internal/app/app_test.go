package app

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/mockserver"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/safety"
)

func scWith(w model.Workload) *model.Scenario {
	sc := model.NewScenario("t")
	sc.Workload = w
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "r", Request: &model.Request{Method: "GET", URL: "http://127.0.0.1/x"}}}
	return sc
}

func TestApplyOverrides(t *testing.T) {
	d := func(s string) model.Duration { v, _ := time.ParseDuration(s); return model.Duration(v) }
	constant := model.Workload{Executor: model.ExecConstantVUs, VUs: 2, Duration: d("20s")}
	ramping := model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{{Duration: d("1m"), Target: 5}}}
	perVU := model.Workload{Executor: model.ExecPerVUIterations, VUs: 2, Iterations: 3}
	arrival := model.Workload{Executor: model.ExecArrivalRate, Rate: 3, Duration: d("20s"), VUs: 1, MaxVUs: 4}
	tests := []struct {
		name    string
		start   model.Workload
		o       Overrides
		wantErr string
		check   func(t *testing.T, w model.Workload)
	}{
		{"empty is no-op", constant, Overrides{}, "", func(t *testing.T, w model.Workload) {
			if !reflect.DeepEqual(w, constant) {
				t.Errorf("%+v", w)
			}
		}},
		{"vus + duration", constant, Overrides{VUs: 10, Duration: "1m"}, "", func(t *testing.T, w model.Workload) {
			if w.Executor != model.ExecConstantVUs || w.VUs != 10 || w.Duration != d("1m") {
				t.Errorf("%+v", w)
			}
		}},
		{"vus keeps existing duration", constant, Overrides{VUs: 7}, "", func(t *testing.T, w model.Workload) {
			if w.VUs != 7 || w.Duration != d("20s") {
				t.Errorf("%+v", w)
			}
		}},
		{"vus from ramping uses planned duration", ramping, Overrides{VUs: 7}, "", func(t *testing.T, w model.Workload) {
			if w.Executor != model.ExecConstantVUs || w.Duration != d("1m") {
				t.Errorf("%+v", w)
			}
		}},
		{"vus defaults to 30s with no duration", model.Workload{Executor: model.ExecPerVUIterations, VUs: 1, Iterations: 1}, Overrides{VUs: 3}, "", func(t *testing.T, w model.Workload) {
			if w.Duration != d("30s") || w.VUs != 3 {
				t.Errorf("%+v", w)
			}
		}},
		{"vus + rampUp", constant, Overrides{VUs: 100, Duration: "10m", RampUp: "2m"}, "", func(t *testing.T, w model.Workload) {
			if w.Executor != model.ExecRampingVUs || len(w.Stages) != 2 ||
				w.Stages[0].Duration != d("2m") || w.Stages[0].Target != 100 ||
				w.Stages[1].Duration != d("8m") || w.Stages[1].Target != 100 || w.MaxPlannedVUs() != 100 || w.PlannedDuration() != 10*time.Minute {
				t.Errorf("%+v", w)
			}
		}},
		{"rampUp zero means constant", constant, Overrides{VUs: 5, Duration: "1m", RampUp: "0s"}, "", func(t *testing.T, w model.Workload) {
			if w.Executor != model.ExecConstantVUs {
				t.Errorf("%+v", w)
			}
		}},
		{"rampUp not shorter than duration", constant, Overrides{VUs: 5, Duration: "1m", RampUp: "1m"}, "shorter", nil},
		{"rampUp longer than duration", constant, Overrides{VUs: 5, Duration: "30s", RampUp: "2m"}, "shorter", nil},
		{"rampUp invalid", constant, Overrides{VUs: 5, Duration: "1m", RampUp: "soon"}, "ramp-up", nil},
		{"rate", constant, Overrides{Rate: 25, Duration: "30s"}, "", func(t *testing.T, w model.Workload) {
			if w.Executor != model.ExecArrivalRate || w.Rate != 25 || w.Duration != d("30s") || w.VUs != 1 || w.MaxVUs < 50 {
				t.Errorf("%+v", w)
			}
		}},
		{"rate with vus", constant, Overrides{Rate: 2, VUs: 30, Duration: "30s"}, "", func(t *testing.T, w model.Workload) {
			if w.VUs != 30 || w.MaxVUs < 30 {
				t.Errorf("%+v", w)
			}
		}},
		{"rate with maxVUs", constant, Overrides{Rate: 1, MaxVUs: 500}, "", func(t *testing.T, w model.Workload) {
			if w.MaxVUs != 500 || w.Duration != d("20s") {
				t.Errorf("%+v", w)
			}
		}},
		{"rate with no duration anywhere", model.Workload{Executor: model.ExecPerVUIterations, VUs: 1, Iterations: 1}, Overrides{Rate: 1}, "", func(t *testing.T, w model.Workload) {
			if w.Duration != d("1m") {
				t.Errorf("%+v", w)
			}
		}},
		{"duration only: constant", constant, Overrides{Duration: "5m"}, "", func(t *testing.T, w model.Workload) {
			if w.Duration != d("5m") || w.VUs != 2 {
				t.Errorf("%+v", w)
			}
		}},
		{"duration only: arrival", arrival, Overrides{Duration: "5m"}, "", func(t *testing.T, w model.Workload) {
			if w.Duration != d("5m") || w.Rate != 3 {
				t.Errorf("%+v", w)
			}
		}},
		{"duration only: per-vu-iterations", perVU, Overrides{Duration: "5m"}, "", func(t *testing.T, w model.Workload) {
			if w.Duration != d("5m") || w.Iterations != 3 {
				t.Errorf("%+v", w)
			}
		}},
		{"duration only: ramping errors", ramping, Overrides{Duration: "5m"}, "duration alone", nil},
		{"duration only: ramping-rate errors", model.Workload{Executor: model.ExecRampingRate, Stages: []model.Stage{{Duration: d("1m"), Target: 5}}}, Overrides{Duration: "5m"}, "duration alone", nil},
		{"invalid duration", constant, Overrides{VUs: 1, Duration: "abc"}, "duration", nil},
		{"negative duration", constant, Overrides{Duration: "-5s"}, "duration", nil},
		{"zero duration", constant, Overrides{Duration: "0s"}, "duration", nil},
		{"bare number duration", constant, Overrides{Duration: "30"}, "duration", nil},
		{"negative vus", constant, Overrides{VUs: -1}, "negative", nil},
		{"negative rate", constant, Overrides{Rate: -1}, "negative", nil},
		{"negative maxVUs", constant, Overrides{MaxVUs: -1, Rate: 1}, "negative", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := scWith(tc.start)
			err := ApplyOverrides(sc, tc.o)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want containing %q", err, tc.wantErr)
				}
				if !reflect.DeepEqual(sc.Workload, tc.start) {
					t.Errorf("workload mutated despite error: %+v", sc.Workload)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, sc.Workload)
			if probs := sc.Validate(); len(probs) > 0 {
				t.Errorf("result is not a valid scenario: %v", probs)
			}
		})
	}
}

func TestApplyOverridesKeepsTuning(t *testing.T) {
	scale := 0.5
	w := model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(10 * time.Second),
		Pacing: model.Duration(time.Second), GracefulStop: model.Duration(2 * time.Second), ThinkTimeScale: &scale, OnError: "abort-iteration"}
	for _, o := range []Overrides{{VUs: 5}, {VUs: 5, RampUp: "2s"}, {Rate: 3}} {
		sc := scWith(w)
		if err := ApplyOverrides(sc, o); err != nil {
			t.Fatal(err)
		}
		got := sc.Workload
		if got.Pacing != w.Pacing || got.GracefulStop != w.GracefulStop || got.ThinkTimeScale != &scale || got.OnError != "abort-iteration" {
			t.Errorf("%+v lost tuning: %+v", o, got)
		}
	}
}

func TestOverridesEmpty(t *testing.T) {
	if !(Overrides{}).Empty() {
		t.Error("zero value should be empty")
	}
	for _, o := range []Overrides{{VUs: 1}, {Duration: "1s"}, {RampUp: "1s"}, {Rate: 1}} {
		if o.Empty() {
			t.Errorf("%+v should not be empty", o)
		}
	}
}

func hostScenario(base string, vars map[string]string, urls ...string) *model.Scenario {
	sc := model.NewScenario("h")
	sc.Defaults.BaseURL = base
	sc.Variables = vars
	for _, u := range urls {
		sc.Steps = append(sc.Steps, model.Step{Type: model.StepRequest, Request: &model.Request{Method: "GET", URL: u}})
	}
	return sc
}

func TestTargetHosts(t *testing.T) {
	tests := []struct {
		name string
		sc   *model.Scenario
		want []string
	}{
		{"absolute urls sorted and deduped", hostScenario("", nil, "https://b.example.com/x", "http://a.example.com:8080/y?z=1", "https://b.example.com/z"), []string{"a.example.com", "b.example.com"}},
		{"relative uses base", hostScenario("http://shop.test:9000/", nil, "/a", "b"), []string{"shop.test"}},
		{"variable in host", hostScenario("", map[string]string{"host": "api.example.com"}, "https://${host}/v1"), []string{"api.example.com"}},
		{"variable base url", hostScenario("http://${h}:81", map[string]string{"h": "base.example.com"}, "/p"), []string{"base.example.com"}},
		{"nested variable", hostScenario("", map[string]string{"a": "${b}", "b": "deep.example.com"}, "http://${a}/"), []string{"deep.example.com"}},
		{"unresolved variable host skipped", hostScenario("", nil, "http://${unknown}/x", "http://known.test/"), []string{"known.test"}},
		{"unresolved base skipped", hostScenario("${nope}", nil, "/x"), []string{}},
		{"ipv6", hostScenario("", nil, "http://[::1]:8080/a", "http://[2001:db8::5]/b"), []string{"2001:db8::5", "::1"}},
		{"query/fragment terminate host", hostScenario("", nil, "http://q.test?x=1", "http://f.test#frag"), []string{"f.test", "q.test"}},
		{"variable only in path", hostScenario("", map[string]string{"id": "7"}, "http://p.test/u/${id}"), []string{"p.test"}},
		{"no requests", hostScenario("http://x.test", nil), []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TargetHosts(tc.sc)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
	// requests in setup/teardown count too
	sc := hostScenario("", nil, "http://main.test/")
	sc.Setup = []model.Step{{Type: model.StepRequest, Request: &model.Request{Method: "GET", URL: "http://setup.test/"}}}
	sc.Teardown = []model.Step{{Type: model.StepGroup, Steps: []model.Step{{Type: model.StepRequest, Request: &model.Request{Method: "GET", URL: "http://td.test/"}}}}}
	if got := TargetHosts(sc); !reflect.DeepEqual(got, []string{"main.test", "setup.test", "td.test"}) {
		t.Errorf("setup/teardown: %v", got)
	}
}

func TestTargetHostsUserinfo(t *testing.T) {
	got := TargetHosts(hostScenario("", nil, "http://user:pw@api.example.com:8080/x"))
	if !reflect.DeepEqual(got, []string{"api.example.com"}) {
		t.Errorf("got %v", got)
	}
}

func TestCheckTargets(t *testing.T) {
	pol := safety.Policy{AllowedHosts: []string{"api.example.com", "*.corp.test"}, RequireConfirmAbove: 50}
	vus := func(n int) model.Workload {
		return model.Workload{Executor: model.ExecConstantVUs, VUs: n, Duration: model.Duration(time.Second)}
	}
	tests := []struct {
		name      string
		urls      []string
		w         model.Workload
		confirmed bool
		pol       safety.Policy
		remote    bool
		errType   string // "", "notallowed", "confirm"
		wantHost  string
	}{
		{"loopback many VUs", []string{"http://127.0.0.1:8080/"}, vus(5000), false, pol, false, "", ""},
		{"localhost", []string{"http://localhost/"}, vus(500), false, pol, false, "", ""},
		{"not allowlisted", []string{"http://evil.example.net/"}, vus(1), true, pol, false, "notallowed", "evil.example.net"},
		{"mixed with disallowed", []string{"http://127.0.0.1/", "http://10.0.0.5/"}, vus(1), true, pol, false, "notallowed", "10.0.0.5"},
		{"allowlisted exact small", []string{"https://api.example.com/"}, vus(10), false, pol, true, "", ""},
		{"allowlisted wildcard small", []string{"https://x.corp.test/"}, vus(50), false, pol, true, "", ""},
		{"above threshold needs confirmation", []string{"https://api.example.com/"}, vus(51), false, pol, true, "confirm", ""},
		{"above threshold confirmed", []string{"https://api.example.com/"}, vus(51), true, pol, true, "", ""},
		{"rate above threshold", []string{"https://api.example.com/"}, model.Workload{Executor: model.ExecArrivalRate, Rate: 100, Duration: model.Duration(time.Second), VUs: 1, MaxVUs: 10}, false, pol, true, "confirm", ""},
		{"arrival maxVUs above threshold", []string{"https://api.example.com/"}, model.Workload{Executor: model.ExecArrivalRate, Rate: 1, Duration: model.Duration(time.Second), VUs: 1, MaxVUs: 500}, false, pol, true, "confirm", ""},
		{"ramping above threshold", []string{"https://api.example.com/"}, model.Workload{Executor: model.ExecRampingVUs, Stages: []model.Stage{{Duration: model.Duration(time.Second), Target: 200}}}, false, pol, true, "confirm", ""},
		{"zero threshold confirms any remote", []string{"https://api.example.com/"}, vus(1), false, safety.Policy{AllowedHosts: pol.AllowedHosts}, true, "confirm", ""},
		{"loopback never needs confirmation", []string{"http://127.0.0.1/"}, vus(1), false, safety.Policy{}, false, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := hostScenario("", nil, tc.urls...)
			sc.Workload = tc.w
			remote, err := CheckTargets(sc, tc.pol, tc.confirmed)
			switch tc.errType {
			case "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			case "notallowed":
				var e *safety.ErrHostNotAllowed
				if !errors.As(err, &e) || e.Host != tc.wantHost {
					t.Fatalf("err=%v", err)
				}
			case "confirm":
				var e *ErrNeedsConfirmation
				if !errors.As(err, &e) || !strings.Contains(e.Error(), "non-local") {
					t.Fatalf("err=%v", err)
				}
			}
			if tc.errType != "notallowed" && remote != tc.remote {
				t.Errorf("remote=%v want %v", remote, tc.remote)
			}
		})
	}
}

func TestValidRunID(t *testing.T) {
	good := []string{"20240101-120000-shop", "a", "run_1", "A-b_C-9", strings.Repeat("a", 120)}
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, "a b", "a.b", "run\x00", "run\n", "%2e%2e", "é", strings.Repeat("a", 121), "a:b", "~root"}
	for _, id := range good {
		if !ValidRunID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range bad {
		if ValidRunID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func TestScenarioFileRoundTripAndStrictness(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deep", "s.json")
	sc := scWith(model.Workload{Executor: model.ExecConstantVUs, VUs: 2, Duration: model.Duration(5 * time.Second)})
	sc.Name = "RT"
	if err := SaveScenario(p, sc); err != nil {
		t.Fatal(err)
	}
	got, err := LoadScenario(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "RT" || got.Workload.VUs != 2 || got.Workload.Duration.D() != 5*time.Second || len(got.Steps) != 1 {
		t.Errorf("%+v", got)
	}
	os.WriteFile(p, []byte(`{"name":"x","bogusField":1,"steps":[]}`), 0o600)
	if _, err := LoadScenario(p); err == nil || !strings.Contains(err.Error(), "bogusField") {
		t.Errorf("unknown fields must be rejected: %v", err)
	}
	os.WriteFile(p, []byte(`{"name":"x","steps":[]}`), 0o600)
	if g, err := LoadScenario(p); err != nil || g.Version != model.SchemaVersion {
		t.Errorf("version default: %v %v", g, err)
	}
	if _, err := LoadScenario(filepath.Join(dir, "none.json")); err == nil {
		t.Error("missing file")
	}
}

func TestWorkspaceStartRunAndWait(t *testing.T) {
	t.Setenv("PHOENIX_HOME", t.TempDir())
	ms := mockserver.New("s")
	srv := httptest.NewServer(ms.Handler())
	defer srv.Close()
	ws := Workspace{Dir: filepath.Join(t.TempDir(), "ws")}

	if ids, err := ws.ListRuns(); err != nil || len(ids) != 0 {
		t.Fatalf("empty workspace: %v %v", ids, err)
	}
	mk := func(name string) *model.Scenario {
		sc := model.NewScenario(name)
		sc.Defaults.BaseURL = srv.URL
		sc.Workload = model.Workload{Executor: model.ExecPerVUIterations, VUs: 2, Iterations: 3}
		sc.Steps = []model.Step{{Type: model.StepRequest, Name: "h", Request: &model.Request{Method: "GET", URL: "/health"}}}
		return sc
	}
	a, err := ws.StartRun(context.Background(), mk("first"), RunOptions{Policy: safety.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := a.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Snapshot.Iterations != 6 || sum.Status != "finished" || !sum.SLAPassed {
		t.Errorf("%+v", sum)
	}
	for _, f := range []string{"summary.json", "summary.csv", "report.html", "raw.csv", "scenario.json"} {
		if st, err := os.Stat(filepath.Join(a.Dir, f)); err != nil || st.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	// Wait is idempotent
	if s2, err := a.Wait(); err != nil || s2 != sum {
		t.Errorf("second Wait: %v", err)
	}
	// second run with baseline + no raw output
	time.Sleep(1100 * time.Millisecond) // run IDs have 1s resolution
	b, err := ws.StartRun(context.Background(), mk("second"), RunOptions{Policy: safety.DefaultPolicy(), Baseline: a.ID, NoRaw: true})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sb.Baseline == nil || sb.Baseline.BaselineRun != a.ID {
		t.Errorf("baseline comparison missing: %+v", sb.Baseline)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "raw.csv")); !os.IsNotExist(err) {
		t.Error("NoRaw should not create raw.csv")
	}
	ids, err := ws.ListRuns()
	if err != nil || len(ids) != 2 || ids[0] != b.ID || ids[1] != a.ID {
		t.Errorf("ListRuns newest first: %v %v", ids, err)
	}
	// missing baseline => note, not failure
	time.Sleep(1100 * time.Millisecond)
	c, _ := ws.StartRun(context.Background(), mk("third"), RunOptions{Policy: safety.DefaultPolicy(), Baseline: "does-not-exist", NoRaw: true})
	sc3, err := c.Wait()
	if err != nil || sc3.Baseline != nil || len(sc3.Notes) == 0 {
		t.Errorf("missing baseline: %v %+v", err, sc3.Notes)
	}
	// directories without summary.json are not runs
	os.MkdirAll(filepath.Join(ws.RunsDir(), "partial"), 0o755)
	if ids, _ := ws.ListRuns(); len(ids) != 3 {
		t.Errorf("partial dir listed: %v", ids)
	}
}

func TestStartRunPolicyFailureCleansUp(t *testing.T) {
	t.Setenv("PHOENIX_HOME", t.TempDir())
	ws := Workspace{Dir: t.TempDir()}
	sc := scWith(model.Workload{Executor: model.ExecConstantVUs, VUs: 100, Duration: model.Duration(time.Second)})
	_, err := ws.StartRun(context.Background(), sc, RunOptions{Policy: safety.Policy{MaxVUs: 5}})
	if err == nil || !strings.Contains(err.Error(), "safety policy") {
		t.Fatalf("err=%v", err)
	}
	if ents, _ := os.ReadDir(ws.RunsDir()); len(ents) != 0 {
		t.Errorf("failed start left run dirs behind: %v", ents)
	}
}
