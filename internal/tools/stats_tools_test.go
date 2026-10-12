package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/service"
)

func TestStatsTools(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	ws := app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")}
	s, err := service.New(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, n := range []string{"suggest_slas", "gate_run"} {
		found := false
		for _, tl := range Registry() {
			if tl.Name == n {
				found = tl.ReadOnly && !tl.Confirm
			}
		}
		if !found {
			t.Errorf("%s missing or not read-only", n)
		}
	}
	mk := func(id string, p95 float64) {
		sum := &results.Summary{RunID: id, Scenario: "x", Status: "finished"}
		for i := 0; i < 40; i++ {
			v := p95 + float64(i%5)
			sum.Snapshot.Timeline = append(sum.Snapshot.Timeline, metrics.WindowOut{T: i, RPS: 50, P50Ms: v / 2, P95Ms: v, P99Ms: v * 1.3, VUs: 5})
		}
		sum.Snapshot.Requests = metrics.MetricSet{Count: 2000, P95Ms: p95 + 4, P99Ms: (p95 + 4) * 1.3, RPS: 50}
		if err := results.Save(filepath.Join(ws.RunsDir(), id), sum); err != nil {
			t.Fatal(err)
		}
	}
	mk("base", 20)
	mk("slow", 40)
	ctx := context.Background()
	out, err := Call(ctx, s, "gate_run", map[string]any{"runId": "slow", "baseline": "base"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	g := out.(map[string]any)["gate"].(*results.GateResult)
	if g.Status != results.GateFail || g.ExitCode != 3 || out.(map[string]any)["markdown"] == "" {
		t.Errorf("%+v", g)
	}
	out, err = Call(ctx, s, "suggest_slas", map[string]any{"runId": "base", "marginPct": 10.0}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if sg := out.(*results.Suggestion); len(sg.SLAs) < 3 || sg.MarginPct != 10 {
		t.Errorf("%+v", sg)
	}
	if _, err := Call(ctx, s, "gate_run", map[string]any{"runId": "../etc"}, "test"); err == nil {
		t.Error("invalid run id must fail")
	}
}
