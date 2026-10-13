package jmx

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

func ctrlEl(tc, gui, name, props, kids string) string {
	return `<` + tc + ` guiclass="` + gui + `" testclass="` + tc + `" testname="` + name + `" enabled="true">` + props +
		`</` + tc + `><hashTree>` + kids + `</hashTree>`
}

func strP(n, v string) string  { return `<stringProp name="` + n + `">` + v + `</stringProp>` }
func boolP(n, v string) string { return `<boolProp name="` + n + `">` + v + `</boolProp>` }
func intP(n, v string) string  { return `<intProp name="` + n + `">` + v + `</intProp>` }

func importCtl(t *testing.T, kids string) (*model.Scenario, *model.CompatReport) {
	t.Helper()
	return importString(t, plan("C", tg(`<stringProp name="ThreadGroup.num_threads">1</stringProp>`+loops("1"), kids), ""))
}

func itemByName(rep *model.CompatReport, element, name string) *model.CompatItem {
	for i := range rep.Items {
		if rep.Items[i].Element == element && rep.Items[i].Name == name {
			return &rep.Items[i]
		}
	}
	return nil
}

func TestImportLogicControllers(t *testing.T) {
	kids := ctrlEl("WhileController", "WhileControllerGui", "W1", strP("WhileController.condition", `${__jexl3("${more}" == "true")}`), sampler("w1", "", "")) +
		ctrlEl("WhileController", "WhileControllerGui", "W2", strP("WhileController.condition", `${continue_loop}`), sampler("w2", "", "")) +
		ctrlEl("WhileController", "WhileControllerGui", "W3", strP("WhileController.condition", ``), sampler("w3", "", "")) +
		ctrlEl("WhileController", "WhileControllerGui", "W4", strP("WhileController.condition", `${__jexl3(${counter} != 10,)}`), sampler("w4", "", "")) +
		ctrlEl("WhileController", "WhileControllerGui", "W5", strP("WhileController.condition", `${__groovy(vars.get("a").length() > 3)}`), sampler("w5", "", "")) +
		ctrlEl("ForeachController", "ForeachControlPanel", "FE", strP("ForeachController.inputVal", "ids")+strP("ForeachController.returnVal", "id")+
			boolP("ForeachController.useSeparator", "false")+strP("ForeachController.startIndex", "1")+strP("ForeachController.endIndex", "4"), sampler("fe", "", "")) +
		ctrlEl("SwitchController", "SwitchControllerGui", "SW", strP("SwitchController.value", "${kind}"),
			sampler("a", "", `<ConstantTimer guiclass="ConstantTimerGui" testclass="ConstantTimer" testname="pause" enabled="true">`+strP("ConstantTimer.delay", "100")+`</ConstantTimer><hashTree/>`)+
				sampler("default", "", "")) +
		ctrlEl("ThroughputController", "ThroughputControllerGui", "PCT", intP("ThroughputController.style", "1")+boolP("ThroughputController.perThread", "true")+
			`<FloatProperty><name>ThroughputController.percentThroughput</name><value>25.0</value><savedValue>0.0</savedValue></FloatProperty>`, sampler("p", "", "")) +
		ctrlEl("ThroughputController", "ThroughputControllerGui", "NUM", intP("ThroughputController.style", "0")+boolP("ThroughputController.perThread", "false")+
			intP("ThroughputController.maxThroughput", "3"), sampler("n", "", "")) +
		ctrlEl("InterleaveControl", "InterleaveControlGui", "IL", intP("InterleaveControl.style", "1")+boolP("InterleaveControl.accrossThreads", "true"), sampler("i1", "", "")+sampler("i2", "", "")) +
		ctrlEl("RandomController", "RandomControlGui", "RND", intP("InterleaveControl.style", "1"), sampler("r1", "", "")+sampler("r2", "", "")) +
		ctrlEl("RandomOrderController", "RandomOrderControllerGui", "RO", "", sampler("o1", "", "")+sampler("o2", "", "")) +
		ctrlEl("RunTime", "RunTimeGui", "RT", strP("RunTime.seconds", "5"), sampler("rt", "", "")) +
		ctrlEl("CriticalSectionController", "CriticalSectionControllerGui", "CS", strP("CriticalSectionController.lockName", "db"), sampler("cs", "", ""))
	sc, rep := importCtl(t, kids)
	if p := sc.Validate(); len(p) > 0 {
		t.Fatalf("imported scenario invalid: %v", p)
	}
	check := func(name, typ string, fn func(*model.Step) bool) {
		t.Helper()
		st := findStep(sc.Steps, name)
		if st == nil || st.Type != typ || st.Disabled || !fn(st) {
			t.Errorf("%s: got %+v, want type %s", name, st, typ)
		}
	}
	check("W1", model.StepWhile, func(s *model.Step) bool { return s.Cond == "${more} == true" })
	check("W2", model.StepWhile, func(s *model.Step) bool { return s.Cond == "${continue_loop} != false" })
	check("W3", model.StepWhile, func(s *model.Step) bool { return s.Cond == "" })
	check("W4", model.StepWhile, func(s *model.Step) bool { return s.Cond == "${counter} != 10" })
	check("FE", model.StepForeach, func(s *model.Step) bool {
		return s.Input == "ids" && s.Var == "id" && s.NoSeparator && s.Start == 1 && s.End == 4
	})
	check("SW", model.StepSwitch, func(s *model.Step) bool {
		// The sampler with a timer is wrapped so it stays one switch child.
		return s.Value == "${kind}" && len(s.Steps) == 2 && s.Steps[0].Type == model.StepGroup && s.Steps[0].Name == "a" && s.Steps[1].Name == "default"
	})
	check("PCT", model.StepPercent, func(s *model.Step) bool { return s.Percent == 25 && !s.Shared })
	check("NUM", model.StepPercent, func(s *model.Step) bool { return s.Executions == 3 && s.Shared })
	check("IL", model.StepGroup, func(s *model.Step) bool { return s.Select == model.SelectRoundRobin && s.Shared && len(s.Steps) == 2 })
	check("RND", model.StepGroup, func(s *model.Step) bool { return s.Select == model.SelectWeighted && len(s.Steps) == 2 })
	check("RO", model.StepGroup, func(s *model.Step) bool { return s.Select == model.SelectShuffle })
	check("RT", model.StepRuntime, func(s *model.Step) bool { return s.Duration.D() == 5*time.Second })
	check("CS", model.StepCritical, func(s *model.Step) bool { return s.Key == "db" })
	if st := findStep(sc.Steps, "W5"); st == nil || !st.Disabled {
		t.Errorf("unconvertible while condition must stay a disabled group: %+v", st)
	}
	for name, want := range map[string]string{"W1": model.Partial, "W5": model.Unsupported, "FE": model.Supported,
		"SW": model.Supported, "PCT": model.Supported, "IL": model.Supported, "RT": model.Supported, "CS": model.Supported} {
		el := map[string]string{"W1": "WhileController", "W5": "WhileController", "FE": "ForeachController", "SW": "SwitchController",
			"PCT": "ThroughputController", "IL": "InterleaveControl", "RT": "RunTime", "CS": "CriticalSectionController"}[name]
		it := itemByName(rep, el, name)
		if it == nil || it.Status != want {
			t.Errorf("%s: item %+v, want %s", name, it, want)
		}
	}
	if it := itemByName(rep, "WhileController", "W1"); it == nil || !strings.Contains(it.Note, "capped at") {
		t.Errorf("while note should mention the cap: %+v", it)
	}
}

func TestImportThroughputAndSyncTimers(t *testing.T) {
	ctt := `<ConstantThroughputTimer guiclass="TestBeanGUI" testclass="ConstantThroughputTimer" testname="CTT" enabled="true">` +
		`<doubleProp><name>throughput</name><value>120.0</value><savedValue>0.0</savedValue></doubleProp>` + intP("calcMode", "3") + `</ConstantThroughputTimer><hashTree/>`
	sync := `<SyncTimer guiclass="TestBeanGUI" testclass="SyncTimer" testname="Sync" enabled="true">` + intP("groupSize", "4") +
		`<longProp name="timeoutInMs">2000</longProp></SyncTimer><hashTree/>`
	sc, rep := importCtl(t, ctt+sync+sampler("a", "", "")+sampler("b", "", ""))
	var thr, rv []model.Step
	for _, s := range sc.Steps {
		switch s.Type {
		case model.StepThroughput:
			thr = append(thr, s)
		case model.StepRendezvous:
			rv = append(rv, s)
		}
	}
	if len(thr) != 2 || len(rv) != 2 {
		t.Fatalf("timers before each sampler expected, got %+v", sc.Steps)
	}
	if thr[0].PerMinute != 120 || thr[0].Scope != model.ThroughputShared || thr[0].Key == "" || thr[0].Key != thr[1].Key {
		t.Errorf("throughput step %+v", thr[0])
	}
	if rv[0].Group != 4 || rv[0].Timeout.D() != 2*time.Second || rv[0].Key != rv[1].Key {
		t.Errorf("rendezvous step %+v", rv[0])
	}
	if p := sc.Validate(); len(p) > 0 {
		t.Fatal(p)
	}
	if it := itemByName(rep, "ConstantThroughputTimer", "CTT"); it == nil || it.Status != model.Supported {
		t.Errorf("CTT item %+v", it)
	}
	// Legacy stringProp form, this-thread mode; SyncTimer without timeout is partial.
	ctt2 := `<ConstantThroughputTimer guiclass="TestBeanGUI" testclass="ConstantThroughputTimer" testname="CTT" enabled="true">` +
		strP("throughput", "60") + `</ConstantThroughputTimer><hashTree/>`
	sync2 := `<SyncTimer guiclass="TestBeanGUI" testclass="SyncTimer" testname="Sync" enabled="true">` + intP("groupSize", "0") + `</SyncTimer><hashTree/>`
	sc2, rep2 := importCtl(t, ctt2+sync2+sampler("a", "", ""))
	if st := findStep(sc2.Steps, "CTT"); st == nil || st.PerMinute != 60 || st.Scope != model.ThroughputVU {
		t.Errorf("legacy CTT %+v", st)
	}
	if it := itemByName(rep2, "SyncTimer", "Sync"); it == nil || it.Status != model.Partial || !strings.Contains(it.Note, "wait forever") {
		t.Errorf("sync without timeout %+v", it)
	}
}

func TestImportNewUserEachIteration(t *testing.T) {
	sc, _ := importString(t, plan("U", tg(`<stringProp name="ThreadGroup.num_threads">1</stringProp>`+loops("2")+
		boolP("ThreadGroup.same_user_on_next_iteration", "false"), sampler("a", "", "")), ""))
	if !sc.Workload.NewUserEachIteration {
		t.Fatal("same_user_on_next_iteration=false not mapped")
	}
	out, _, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `<boolProp name="ThreadGroup.same_user_on_next_iteration">false</boolProp>`) {
		t.Error("export did not write same_user_on_next_iteration=false")
	}
}

func TestControlStepsRoundTrip(t *testing.T) {
	yes := true // the importer makes the redirect setting explicit
	get := func(n string) model.Step {
		return model.Step{Type: model.StepRequest, Name: n, Request: &model.Request{Method: "GET", URL: "/" + n, FollowRedirects: &yes}}
	}
	sc := model.NewScenario("ctl")
	sc.Defaults.BaseURL = "http://api.local:8080"
	sc.Workload = model.Workload{Executor: model.ExecPerVUIterations, VUs: 2, Iterations: 3}
	sc.Steps = []model.Step{
		{Type: model.StepWhile, Name: "poll", Cond: "${state} != done", Steps: []model.Step{get("p")}},
		{Type: model.StepWhile, Name: "until-fail", Steps: []model.Step{get("u")}},
		{Type: model.StepForeach, Name: "each", StepControl: model.StepControl{Input: "ids", Var: "id", Start: 1, End: 5}, Steps: []model.Step{get("e")}},
		{Type: model.StepSwitch, Name: "sw", StepControl: model.StepControl{Value: "${kind}"}, Steps: []model.Step{get("a"), get("default")}},
		{Type: model.StepPercent, Name: "pct", StepControl: model.StepControl{Percent: 12.5}, Steps: []model.Step{get("x")}},
		{Type: model.StepPercent, Name: "first", StepControl: model.StepControl{Executions: 2, Shared: true}, Steps: []model.Step{get("y")}},
		{Type: model.StepRuntime, Name: "rt", StepControl: model.StepControl{Duration: model.Duration(3 * time.Second)}, Steps: []model.Step{get("r")}},
		{Type: model.StepCritical, Name: "cs", StepControl: model.StepControl{Key: "db"}, Steps: []model.Step{get("c")}},
		{Type: model.StepGroup, Name: "order", Select: model.SelectShuffle, Steps: []model.Step{get("o1"), get("o2")}},
		{Type: model.StepGroup, Name: "rr", Select: model.SelectRoundRobin, StepControl: model.StepControl{Shared: true}, Steps: []model.Step{get("k1"), get("k2")}},
		{Type: model.StepGroup, Name: "rnd", Select: model.SelectWeighted, Steps: []model.Step{get("w1"), get("w2")}},
	}
	if p := sc.Validate(); len(p) > 0 {
		t.Fatal(p)
	}
	out, erep, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range erep.Items {
		if it.Status == model.Unsupported {
			t.Errorf("export unsupported: %+v", it)
		}
	}
	got, _, err := Import(bytes.NewReader(out), "ctl.jmx")
	if err != nil {
		t.Fatalf("re-import: %v\n%s", err, out)
	}
	if len(got.Steps) != len(sc.Steps) {
		t.Fatalf("got %d steps, want %d", len(got.Steps), len(sc.Steps))
	}
	for i := range sc.Steps {
		if !reflect.DeepEqual(got.Steps[i], sc.Steps[i]) {
			w, _ := json.Marshal(sc.Steps[i])
			g, _ := json.Marshal(got.Steps[i])
			t.Errorf("step %d differs\nwant %s\ngot  %s", i, w, g)
		}
	}
}

func TestExportTimersAsCarriers(t *testing.T) {
	sc := model.NewScenario("t")
	sc.Defaults.BaseURL = "http://api.local"
	sc.Workload = model.Workload{Executor: model.ExecArrivalRate, Rate: 2, Duration: model.Duration(time.Minute), VUs: 2,
		WorkloadControl: model.WorkloadControl{Arrivals: model.ArrivalsPoisson}}
	sc.Steps = []model.Step{
		{Type: model.StepThroughput, Name: "cap", StepControl: model.StepControl{PerMinute: 90, Scope: model.ThroughputActive}},
		{Type: model.StepRendezvous, Name: "meet", StepControl: model.StepControl{Group: 3}},
		{Type: model.StepRequest, Name: "a", Request: &model.Request{Method: "GET", URL: "/a"}},
	}
	out, rep, err := Export(sc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`testclass="ConstantThroughputTimer"`, `<value>90</value>`, `<intProp name="calcMode">1</intProp>`,
		`testclass="SyncTimer"`, `<intProp name="groupSize">3</intProp>`, `<longProp name="timeoutInMs">30000</longProp>`} {
		if !strings.Contains(s, want) {
			t.Errorf("export lacks %s", want)
		}
	}
	got, _, err := Import(bytes.NewReader(out), "t.jmx")
	if err != nil {
		t.Fatal(err)
	}
	if st := findStep(got.Steps, "cap"); st == nil || st.Type != model.StepThroughput || st.PerMinute != 90 || st.Scope != model.ThroughputActive {
		t.Errorf("throughput did not round-trip: %+v", st)
	}
	if st := findStep(got.Steps, "meet"); st == nil || st.Type != model.StepRendezvous || st.Group != 3 || st.Timeout.D() != 30*time.Second {
		t.Errorf("rendezvous did not round-trip: %+v", st)
	}
	var poisson bool
	for _, it := range rep.Items {
		if it.Element == "workload" && strings.Contains(it.Note, "poisson") {
			poisson = true
		}
	}
	if !poisson {
		t.Error("poisson arrivals not reported in the export")
	}
}
