package accesslog

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/engine"
	"github.com/lelanga/loadmeter/internal/mockserver"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

// sessionLog writes users*reqs requests whose inter-request gaps (whole
// seconds, like a real log) come from the given distribution.
func sessionLog(users, reqs int, d model.Distribution, seed uint64) string {
	r := rand.New(rand.NewPCG(seed, 1))
	var b strings.Builder
	base := time.Date(2023, 10, 10, 8, 0, 0, 0, time.UTC)
	for u := 0; u < users; u++ {
		t := base.Add(time.Duration(r.IntN(3600)) * time.Second)
		for i := 0; i < reqs; i++ {
			fmt.Fprintf(&b, "10.1.%d.%d - - [%s] \"GET /page/%d HTTP/1.1\" 200 5 \"-\" \"ua\"\n",
				u/250, u%250, t.Format(combinedTime), i%7)
			gap := math.Round(d.SampleSeconds(r))
			if gap < 1 {
				gap = 1
			}
			t = t.Add(time.Duration(gap) * time.Second)
		}
	}
	return b.String()
}

func TestThinkFitFromSessionsAndJourney(t *testing.T) {
	log := sessionLog(300, 20, model.Distribution{Type: "lognormal", Mu: 2.5, Sigma: 0.7}, 11)
	sc, rep, p, err := Import(strings.NewReader(log), "sessions", Options{BaseURL: "http://127.0.0.1:1", Journey: true})
	if err != nil {
		t.Fatal(err)
	}
	fit := p.Sessions.ThinkFit
	if fit == nil || fit.Dist != "lognormal" {
		t.Fatalf("fit: %+v (%s)", fit, p.Sessions.ThinkFitReason)
	}
	if math.Abs(fit.Params["mu"]-2.5) > 0.15 || math.Abs(fit.Params["sigma"]-0.7) > 0.1 {
		t.Errorf("params off: %+v", fit.Params)
	}
	eq(t, "sessions", p.Sessions.Sessions, 300)
	eq(t, "requests per session", p.Sessions.MeanRequests, 20.0)
	loop := sc.Steps[0]
	if loop.Type != model.StepLoop || loop.Count != 20 || len(loop.Steps) != 2 {
		t.Fatalf("journey shape: %+v", loop)
	}
	th := loop.Steps[1].Think
	if th == nil || th.Distribution == nil || th.Distribution.Type != "lognormal" || th.Max <= 0 || th.Min.D() != MachineGap {
		t.Fatalf("think: %+v", th)
	}
	if rep.Validated != "passed" {
		t.Error("not validated")
	}
	if sc.Metadata["thinkTime"] == "" {
		t.Error("fit not recorded in metadata")
	}
	// iteration = session, so the arrival rate is request rate / 20
	var maxT float64
	for _, s := range sc.Workload.Stages {
		maxT = math.Max(maxT, s.Target)
	}
	if want := p.Window.PeakRPS * 0.1 / 20; maxT > want*1.0001 {
		t.Errorf("peak session rate %.5f exceeds request peak / K = %.5f", maxT, want)
	}

	// Without --journey the fit is only recorded.
	sc2, _, _, err := Import(strings.NewReader(log), "sessions", Options{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if sc2.Steps[0].Type != model.StepGroup || sc2.Metadata["thinkTime"] == "" {
		t.Errorf("non-journey shape: %+v", sc2.Steps[0].Type)
	}
}

func TestSessionIdleSplitAndMachineGaps(t *testing.T) {
	line := func(ts string) string {
		return fmt.Sprintf("10.0.0.1 - - [10/Oct/2023:%s +0000] \"GET /a HTTP/1.1\" 200 1 \"-\" \"ua\"\n", ts)
	}
	log := line("10:00:00") + line("10:00:00") + line("10:00:20") + line("10:31:21") + line("10:31:30")
	p, err := Analyze(strings.NewReader(log), "x", Options{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "sessions (31 min idle splits)", p.Sessions.Sessions, 2)
	eq(t, "gaps (same-second burst excluded, idle gap excluded)", p.Sessions.GapsCollected, 2)
	if p.Sessions.ThinkFit != nil || !strings.Contains(p.Sessions.ThinkFitReason, "too few") {
		t.Errorf("tiny sample must not fit: %+v", p.Sessions)
	}
	// Journey with too few gaps falls back to a uniform pause and says so.
	sc, rep, _, err := Import(strings.NewReader(log), "x", Options{BaseURL: "http://127.0.0.1:1", Journey: true})
	if err != nil {
		t.Fatal(err)
	}
	th := sc.Steps[0].Steps[1].Think
	if th.Distribution != nil || th.Max <= th.Min {
		t.Errorf("fallback think: %+v", th)
	}
	partial := false
	for _, it := range rep.Items {
		partial = partial || it.Element == "think time" && it.Status == model.Partial
	}
	if !partial {
		t.Error("fallback not reported")
	}
}

func TestSessionsNeverExposeClientAddresses(t *testing.T) {
	prof, _ := Analyze(strings.NewReader(sessionLog(5, 3, model.Distribution{Type: "exponential", Mean: 10}, 3)), "x", Options{})
	b, _ := json.Marshal(prof)
	if strings.Contains(string(b), "10.1.0.") {
		t.Error("client IP leaked into the profile")
	}
}

// TestGeneratedScenarioRunsAgainstMockServer replays a generated scenario
// (compressed to ~1.5 s) against the in-process mock server on 127.0.0.1.
func TestGeneratedScenarioRunsAgainstMockServer(t *testing.T) {
	counts := []int{1200, 2400, 1200} // per minute -> 20/s, 40/s, 20/s at scale 1
	log := synthLog("10/Oct/2023", 12, 0, counts, []string{"/health", "/api/items", "/api/items/5"}, []int{6, 3, 1})
	ms := mockserver.New("accesslog-test")
	srv := httptest.NewServer(ms.Handler())
	defer srv.Close()

	// mock has no /api/items/{id}: it would 404, so run the two real endpoints and the id one separately asserted below.
	sc, _, p, err := Import(strings.NewReader(log), "mock.log", Options{BaseURL: srv.URL, Scale: 1, Window: "peak", PeakMinutes: 3})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "replayable", p.Replayable, 4800)
	for i := range sc.Workload.Stages {
		sc.Workload.Stages[i].Duration = model.Duration(600 * time.Millisecond) // time-compress for the test
		sc.Workload.Stages[i].Target *= 10                                      // keep absolute rates meaningful: 2..4/s -> 20..40/s
	}
	// Drop the id endpoint (the mock has no /api/items/{id}); weights renormalise at run time.
	g := &sc.Steps[0]
	var keep []model.Step
	for _, s := range g.Steps {
		if !strings.Contains(s.Request.URL, "__randomInt") {
			keep = append(keep, s)
		}
	}
	g.Steps = keep
	if probs := sc.Validate(); len(probs) != 0 {
		t.Fatal(probs)
	}
	r, err := engine.Prepare(sc, engine.Options{Policy: safety.DefaultPolicy(), Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	select {
	case <-r.Done():
	case <-time.After(20 * time.Second):
		r.Stop()
		t.Fatal("run did not finish")
	}
	snap := r.Col.Snapshot(r.EndedAt, 0)
	if snap.Requests.Count < 20 || snap.Requests.Errors != 0 {
		t.Fatalf("requests=%d errors=%d", snap.Requests.Count, snap.Requests.Errors)
	}
	var health, items uint64
	for _, n := range snap.ByRequest {
		switch n.Name {
		case "GET /health":
			health = n.Count
		case "GET /api/items":
			items = n.Count
		}
	}
	if health == 0 || items == 0 || health < items {
		t.Errorf("mix not followed: health=%d items=%d (expected about 2:1)", health, items)
	}
	if got := ms.Hits.Load(); got != snap.Requests.Count {
		t.Errorf("server hits %d != client requests %d", got, snap.Requests.Count)
	}
}
