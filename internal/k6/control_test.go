package k6

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

func ctlSc() *model.Scenario {
	sc := model.NewScenario("ctl")
	sc.Defaults.BaseURL = "http://127.0.0.1:8080"
	get := func(n, p string) model.Step {
		return model.Step{Type: model.StepRequest, Name: n, Request: &model.Request{Method: "GET", URL: p}}
	}
	w := model.Step{Type: model.StepWhile, Name: "poll", Cond: "${state} != done", StepControl: model.StepControl{MaxIterations: 50, Duration: model.Duration(time.Second)}, Steps: []model.Step{get("p", "/poll")}}
	last := model.Step{Type: model.StepWhile, Name: "last", Cond: "LAST", Steps: []model.Step{get("l", "/l")}}
	fe := model.Step{Type: model.StepForeach, Name: "each", StepControl: model.StepControl{Input: "id", Var: "cur"}, Steps: []model.Step{get("i", "/i/${cur}")}}
	sw := model.Step{Type: model.StepSwitch, Name: "sw", StepControl: model.StepControl{Value: "${kind}"}, Steps: []model.Step{get("a", "/a"), get("default", "/d")}}
	pc := model.Step{Type: model.StepPercent, Name: "pct", StepControl: model.StepControl{Percent: 25, Shared: true}, Steps: []model.Step{get("x", "/x")}}
	rt := model.Step{Type: model.StepRuntime, Name: "rt", StepControl: model.StepControl{Duration: model.Duration(2 * time.Second)}, Steps: []model.Step{get("r1", "/r1"), get("r2", "/r2")}}
	thr := model.Step{Type: model.StepThroughput, Name: "ctt", StepControl: model.StepControl{PerMinute: 120, Scope: model.ThroughputShared}}
	rv := model.Step{Type: model.StepRendezvous, Name: "meet", StepControl: model.StepControl{Group: 5}}
	cr := model.Step{Type: model.StepCritical, Name: "lock", Steps: []model.Step{get("c", "/c")}}
	sh := model.Step{Type: model.StepGroup, Name: "order", Select: model.SelectShuffle, Steps: []model.Step{get("s1", "/s1"), get("s2", "/s2")}}
	rr := model.Step{Type: model.StepGroup, Name: "rr", Select: model.SelectRoundRobin, Steps: []model.Step{get("k1", "/k1"), get("k2", "/k2")}}
	sc.Steps = []model.Step{w, last, fe, sw, pc, rt, thr, rv, cr, sh, rr}
	sc.Workload = model.Workload{Executor: model.ExecArrivalRate, Rate: 5, Duration: model.Duration(10 * time.Second), VUs: 2, MaxVUs: 5,
		WorkloadControl: model.WorkloadControl{Arrivals: model.ArrivalsPoisson}}
	return sc
}

func TestExportControlSteps(t *testing.T) {
	b, rep, err := Export(ctlSc(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"_ctlCond(\"${state} != done\", V)", "< 50 &&", "Date.now() + 1000",
		"V[_expand(\"id\", V) + \"_\" +", "_ctlSwitch(_expand(\"${kind}\", V), [\"a\",\"default\"])",
		"_ctlPct(", "_ctlCTT(", "500);", "_ctlShuffle(2)", "_ctlRR(", "// rendezvous \"meet\" skipped",
		"function _ctlSwitch", "const _ctl = {};",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q", want)
		}
	}
	status := map[string]string{}
	for _, it := range rep.Items {
		status[it.Element+"/"+it.Name] = it.Status
	}
	for k, want := range map[string]string{
		"while/poll": model.Supported, "while/last": model.Unsupported, "foreach/each": model.Supported,
		"switch/sw": model.Supported, "percent/pct": model.Partial, "runtime/rt": model.Supported,
		"throughput/ctt": model.Partial, "rendezvous/meet": model.Unsupported, "critical/lock": model.Partial,
		"group/order": model.Supported, "group/rr": model.Supported, "workload.arrivals/poisson": model.Partial,
	} {
		if status[k] != want {
			t.Errorf("%s status %q, want %q", k, status[k], want)
		}
	}
	nodeCheck(t, "control", b)
}

func TestExportWithoutControlStepsHasNoHelpers(t *testing.T) {
	sc := model.NewScenario("plain")
	sc.Defaults.BaseURL = "http://127.0.0.1:8080"
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "a", Request: &model.Request{Method: "GET", URL: "/"}}}
	b, _, err := Export(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "_ctl") {
		t.Error("control helpers emitted for a scenario without control steps")
	}
}

// The JS helpers follow the engine's rules (run in node when available).
func TestControlHelpersSemantics(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	prog := "const http = {}; function sleep() {}\n" + runtimeJS + "\n" + controlJS + `
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) { console.error(m, a, b); process.exit(1); } };
const names = ['a', 'b', 'Default'];
eq(_ctlSwitch('', names), 0, 'empty');
eq(_ctlSwitch('1', names), 1, 'index');
eq(_ctlSwitch('9', names), 0, 'out of range');
eq(_ctlSwitch('b', names), 1, 'name');
eq(_ctlSwitch('zz', names), 2, 'default');
eq(_ctlSwitch('zz', ['a']), -1, 'none');
const runs = []; for (let k = 0; k < 12; k++) if (_ctlPct('p', 25, 0)) runs.push(k);
eq(runs, [2, 6, 10], 'percent');
let n = 0; for (let k = 0; k < 10; k++) if (_ctlPct('e', 0, 3)) n++;
eq(n, 3, 'executions');
eq([_ctlRR('r', 3), _ctlRR('r', 3), _ctlRR('r', 3), _ctlRR('r', 3)], [0, 1, 2, 0], 'rr');
eq(_ctlShuffle(5).slice().sort(), [0, 1, 2, 3, 4], 'shuffle');
eq(_ctlCond('${missing} != x', {}), false, 'unresolved');
eq(_ctlCond('${a} != x', {a: 'y'}), true, 'cond');
`
	f := filepath.Join(t.TempDir(), "h.mjs")
	if err := os.WriteFile(f, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("helper semantics: %v\n%s", err, out)
	}
}
