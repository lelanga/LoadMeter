package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/results"
)

// seedRun writes a finished run (summary.json + raw.csv) into the temp workspace.
func seedRun(t *testing.T, e *env, id string, withRaw bool) {
	t.Helper()
	dir := filepath.Join(e.svc.WS.RunsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	sum := &results.Summary{RunID: id, Scenario: "demo", Status: "finished", StartedAt: start, DurationS: 60}
	sum.Snapshot.Requests = metrics.MetricSet{Count: 100, Errors: 12, ErrorRate: 12, RPS: 1.7, P50Ms: 10, P95Ms: 40, P99Ms: 50, MaxMs: 60,
		ErrorKind: map[string]uint64{"http_5xx": 10, "extraction": 2}}
	if err := results.Save(dir, sum); err != nil {
		t.Fatal(err)
	}
	if !withRaw {
		return
	}
	var b strings.Builder
	b.WriteString("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind\n")
	ms := start.UnixMilli()
	for i := 0; i < 88; i++ {
		b.WriteString(strings.Join([]string{itoa(ms + int64(i*100)), "5", "GET /ok", "200", "", "vu-1", "true", "", "1", "1", "1", "http://127.0.0.1:1/ok", "request"}, ",") + "\n")
	}
	for i := 0; i < 10; i++ {
		b.WriteString(strings.Join([]string{itoa(ms + int64(5000+i*100)), "5", "GET /flaky", "500", "http_5xx", "vu-2", "false", "500 Internal Server Error", "1", "1", "1", "http://127.0.0.1:1/flaky?token=s3cret", "request"}, ",") + "\n")
	}
	for i := 0; i < 2; i++ {
		b.WriteString(strings.Join([]string{itoa(ms + int64(8000+i*100)), "5", "POST /login", "200", "extraction", "vu-3", "false", `"required extraction ""csrf"" found nothing"`, "1", "1", "1", "http://127.0.0.1:1/login", "request"}, ",") + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "raw.csv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestTriageAndSummaryRoutes(t *testing.T) {
	e := newEnv(t)
	seedRun(t, e, "20261012-101010-aaaa-demo", true)

	resp, m, raw := e.api("GET", "/api/runs/20261012-101010-aaaa-demo/triage", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	cls, _ := m["clusters"].([]any)
	if len(cls) != 2 || m["totalFailures"].(float64) != 12 {
		t.Fatalf("clusters: %s", raw)
	}
	first := cls[0].(map[string]any)
	if first["request"] != "GET /flaky" || first["side"] != "target" || first["firstSeenSec"].(float64) != 5 {
		t.Errorf("first cluster: %v", first)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Error("secret query value leaked in the example URL")
	}
	// ?top=1
	_, m, _ = e.api("GET", "/api/runs/20261012-101010-aaaa-demo/triage?top=1", nil)
	if len(m["clusters"].([]any)) != 1 || m["clustersHidden"].(float64) != 1 {
		t.Errorf("top=1: %v", m)
	}
	if resp, _, _ := e.api("GET", "/api/runs/20261012-101010-aaaa-demo/triage?top=abc", nil); resp.StatusCode != 400 {
		t.Errorf("bad top: %d", resp.StatusCode)
	}

	resp, m, raw = e.api("GET", "/api/runs/20261012-101010-aaaa-demo/summary", nil)
	if resp.StatusCode != 200 || m["source"] != "template" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if txt, _ := m["text"].(string); !strings.Contains(txt, "12% of them failed") || !strings.Contains(txt, "GET /flaky") {
		t.Errorf("summary text: %q", txt)
	}
	// ai=1 with AI disabled: template plus a how-to-enable note, never an error
	t.Setenv("LOADMETER_HOME", t.TempDir())
	resp, m, raw = e.api("GET", "/api/runs/20261012-101010-aaaa-demo/summary?ai=1", nil)
	if resp.StatusCode != 200 || m["source"] != "template" || !strings.Contains(m["fallback"].(string), "not enabled") {
		t.Errorf("ai=1: %d %s", resp.StatusCode, raw)
	}
}

func TestSummaryRouteWithMockAI(t *testing.T) {
	e := newEnv(t)
	seedRun(t, e, "20261012-101010-bbbb-demo", true)
	old := ai.MockReply
	t.Cleanup(func() { ai.MockReply = old })
	cfg := ai.DefaultConfig()
	cfg.Enabled = true
	cfg.Providers = []ai.ProviderConfig{{ID: "mock", Kind: "mock", Model: "mock-1"}}
	cfg.Routing.Light, cfg.Routing.Strong = "mock", "mock"
	if err := ai.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ai.MockReply = func(ai.Request) string { return "Out of 100 requests, 12% failed, mostly on GET /flaky." }
	_, m, raw := e.api("GET", "/api/runs/20261012-101010-bbbb-demo/summary?ai=1", nil)
	if m["source"] != "ai" || m["numbersVerified"] != true {
		t.Fatalf("%s", raw)
	}
	ai.MockReply = func(ai.Request) string { return "Out of 4,000 requests, 12% failed." }
	_, m, raw = e.api("GET", "/api/runs/20261012-101010-bbbb-demo/summary?ai=1", nil)
	if m["source"] != "template" || !strings.Contains(m["fallback"].(string), "4,000") {
		t.Fatalf("fabricated number must be rejected: %s", raw)
	}
}

func TestTriageRouteErrors(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/runs/..%2f..%2fetc/triage", "/api/runs/bad.id/triage", "/api/runs/bad.id/summary"} {
		if resp, _, _ := e.api("GET", p, nil); resp.StatusCode != 400 && resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _, _ := e.api("GET", "/api/runs/20260101-000000-zzzz-none/triage", nil); resp.StatusCode != 404 {
		t.Errorf("unknown run: %d", resp.StatusCode)
	}
	if resp, _, _ := e.api("GET", "/api/runs/20260101-000000-zzzz-none/summary?baseline=..%2fx", nil); resp.StatusCode != 400 {
		t.Errorf("bad baseline: %d", resp.StatusCode)
	}
	// token required
	resp, _ := e.do("GET", "/api/runs/20260101-000000-zzzz-none/triage", nil)
	if resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
}

func TestTriageCoarseWithoutRaw(t *testing.T) {
	e := newEnv(t)
	seedRun(t, e, "20261012-101010-cccc-demo", false)
	_, m, raw := e.api("GET", "/api/runs/20261012-101010-cccc-demo/triage", nil)
	if m["coarse"] != true {
		t.Errorf("%s", raw)
	}
}
