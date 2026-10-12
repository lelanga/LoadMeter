package distributed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/metrics"
	"github.com/lelanga/phoenix/internal/mockserver"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/safety"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "phoenix-dist-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("PHOENIX_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

type testAgent struct {
	spec AgentSpec
	srv  *http.Server
}

// startAgent runs an in-process agent on a random 127.0.0.1 port.
func startAgent(t *testing.T, id string, pol safety.Policy) *testAgent {
	t.Helper()
	cert, fp, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAgent(AgentConfig{ID: id, Token: testToken, Policy: pol})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := a.Serve(ln, cert)
	t.Cleanup(func() { srv.Close() })
	return &testAgent{spec: AgentSpec{ID: id, Addr: ln.Addr().String(), Fingerprint: fp}, srv: srv}
}

// valueRecorder wraps the mock server and records the "u" query parameter.
type valueRecorder struct {
	mu   sync.Mutex
	seen map[string]int
	hits int
}

func startMock(t *testing.T) (*httptest.Server, *valueRecorder) {
	t.Helper()
	rec := &valueRecorder{seen: map[string]int{}}
	h := mockserver.New("dist-test").Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.hits++
		if u := r.URL.Query().Get("u"); u != "" {
			rec.seen[u]++
		}
		rec.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("mock must be on 127.0.0.1, got %s", srv.URL)
	}
	return srv, rec
}

func scenario(base string, vus int, dur time.Duration) *model.Scenario {
	sc := model.NewScenario("dist-test")
	sc.Variables["baseUrl"] = base
	sc.Workload = model.Workload{Executor: model.ExecConstantVUs, VUs: vus, Duration: model.Duration(dur), GracefulStop: model.Duration(2 * time.Second)}
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "delay", Request: &model.Request{Method: "GET", URL: "${baseUrl}/delay?ms=3&u=${user}",
		Assert: []model.Assertion{{Type: model.AsStatus, Value: "200"}}}}}
	return sc
}

func writeUsersCSV(t *testing.T, dir string, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("user\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "user%06d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.csv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func baseOpts(dir string, agents ...*testAgent) Options {
	o := Options{RunID: "test-run", Token: testToken, BaseDir: dir, LocalPolicy: safety.DefaultPolicy(), StartDelay: 500 * time.Millisecond}
	for _, a := range agents {
		o.Agents = append(o.Agents, a.spec)
	}
	return o
}

func waitDone(t *testing.T, c *Controller, d time.Duration) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(d):
		c.Stop()
		c.Stop()
		t.Fatalf("distributed run did not finish within %s", d)
	}
}

func TestEndToEndTwoAgentsAndLocal(t *testing.T) {
	mock, rec := startMock(t)
	a1 := startAgent(t, "a1", safety.DefaultPolicy())
	a2 := startAgent(t, "a2", safety.DefaultPolicy())
	dir := t.TempDir()
	writeUsersCSV(t, dir, 200000)
	sc := scenario(mock.URL, 6, 3*time.Second)
	sc.DataSources = []model.DataSource{{Name: "users", File: "users.csv", Mode: "unique"}}
	o := baseOpts(dir, a1, a2)
	o.LocalShare = 1.0 / 3
	c, err := Start(context.Background(), sc, o)
	if err != nil {
		t.Fatal(err)
	}
	// Live view: merged collector receives data while running.
	time.Sleep(o.StartDelay + 2*time.Second)
	if live := c.Col.Snapshot(time.Now(), 0); live.Requests.Count == 0 || live.ActiveVUs == 0 {
		t.Errorf("no live merged data mid-run: count=%d vus=%d", live.Requests.Count, live.ActiveVUs)
	}
	waitDone(t, c, 20*time.Second)

	snap := c.Col.Snapshot(c.EndedAt, 0)
	stats := c.AgentStats()
	if len(stats) != 3 {
		t.Fatalf("want 3 nodes, got %+v", stats)
	}
	var sum, sumVUs uint64
	for _, s := range stats {
		if s.Status != "finished" || s.Requests == 0 {
			t.Errorf("node %s: %+v", s.ID, s)
		}
		sum += s.Requests
		sumVUs += uint64(s.VUs)
	}
	if sumVUs != 6 {
		t.Errorf("VUs split %d, want 6", sumVUs)
	}
	if snap.Requests.Count != sum {
		t.Errorf("merged count %d != sum of nodes %d", snap.Requests.Count, sum)
	}
	rec.mu.Lock()
	hits, distinct := rec.hits, len(rec.seen)
	var dups []string
	for v, n := range rec.seen {
		if n > 1 {
			dups = append(dups, v)
		}
	}
	rec.mu.Unlock()
	if uint64(hits) != snap.Requests.Count {
		t.Errorf("mock saw %d requests, merged count %d", hits, snap.Requests.Count)
	}
	if len(dups) > 0 {
		t.Errorf("%d unique CSV values were used more than once across nodes, e.g. %v", len(dups), dups[:min(3, len(dups))])
	}
	if distinct != hits {
		t.Errorf("distinct values %d != requests %d", distinct, hits)
	}
	if snap.Requests.Errors != 0 {
		t.Errorf("unexpected errors: %+v", snap.Requests)
	}
	// Percentiles come from merged histograms: rebuild them from each node's data.
	all := metrics.NewHistogram()
	for _, n := range c.nodes {
		all.Merge(n.col.Histogram(metrics.KindRequest))
	}
	if got, want := snap.Requests.P95Ms, float64(all.Quantile(0.95))/1000; abs(got-want) > 0.01 {
		t.Errorf("merged p95 %.2f != p95 of combined node histograms %.2f", got, want)
	}
	if snap.Requests.P50Ms < 3 {
		t.Errorf("p50 %.2f ms below the 3 ms server delay", snap.Requests.P50Ms)
	}
	if c.State() != "finished" {
		t.Errorf("state %s", c.State())
	}
	if len(c.Findings()) != 0 {
		t.Errorf("unexpected findings: %+v", c.Findings())
	}
	t.Logf("requests=%d p95=%.2fms nodes=%+v notes=%v", snap.Requests.Count, snap.Requests.P95Ms, stats, c.Notes())
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestWrongTokenRejected(t *testing.T) {
	mock, _ := startMock(t)
	a1 := startAgent(t, "a1", safety.DefaultPolicy())
	o := baseOpts(t.TempDir(), a1)
	o.Token = strings.Repeat("x", 48)
	_, err := Start(context.Background(), scenario(mock.URL, 1, time.Second), o)
	if err == nil || !strings.Contains(err.Error(), "rejected the token") {
		t.Fatalf("expected token rejection, got %v", err)
	}
}

func TestShortTokenRefused(t *testing.T) {
	if _, err := NewAgent(AgentConfig{Token: "short"}); err == nil {
		t.Fatal("agent accepted a short token")
	}
	if _, err := NewAgent(AgentConfig{}); err == nil {
		t.Fatal("agent accepted no token")
	}
}

func TestWrongFingerprintRejected(t *testing.T) {
	mock, rec := startMock(t)
	a1 := startAgent(t, "a1", safety.DefaultPolicy())
	o := baseOpts(t.TempDir(), a1)
	o.Agents[0].Fingerprint = strings.Repeat("ab", 32)
	_, err := Start(context.Background(), scenario(mock.URL, 1, time.Second), o)
	var fe *FingerprintError
	if !errors.As(err, &fe) {
		t.Fatalf("expected fingerprint error, got %v", err)
	}
	if rec.hits != 0 {
		t.Fatalf("load was generated despite the pin failure")
	}
	if _, err := ParseAgentSpec("127.0.0.1:7979"); err == nil {
		t.Fatal("spec without fingerprint accepted")
	}
}

// The controller's policy allows the host, the agent's does not: the agent refuses.
func TestAgentRefusesNonAllowlistedTarget(t *testing.T) {
	a1 := startAgent(t, "a1", safety.DefaultPolicy()) // loopback only
	o := baseOpts(t.TempDir(), a1)
	o.LocalPolicy.AllowedHosts = []string{"target.example"}
	sc := scenario("http://target.example", 1, time.Second)
	_, err := Start(context.Background(), sc, o)
	if err == nil || !strings.Contains(err.Error(), "not in the allowlist") || !strings.Contains(err.Error(), "agent a1 policy") {
		t.Fatalf("expected agent policy refusal, got %v", err)
	}
	// Agent caps are its own too.
	small := safety.DefaultPolicy()
	small.MaxVUs = 2
	a2 := startAgent(t, "a2", small)
	mock, rec := startMock(t)
	_, err = Start(context.Background(), scenario(mock.URL, 10, time.Second), baseOpts(t.TempDir(), a2))
	if err == nil || !strings.Contains(err.Error(), "safety policy allows 2") {
		t.Fatalf("expected agent VU cap refusal, got %v", err)
	}
	if rec.hits != 0 {
		t.Fatal("load generated despite refusal")
	}
}

func TestAgentDisconnectMidRun(t *testing.T) {
	mock, rec := startMock(t)
	a1 := startAgent(t, "a1", safety.DefaultPolicy())
	a2 := startAgent(t, "a2", safety.DefaultPolicy())
	sc := scenario(mock.URL, 4, 4*time.Second)
	sc.Steps[0].Request.URL = "${baseUrl}/delay?ms=3"
	o := baseOpts(t.TempDir(), a1, a2)
	c, err := Start(context.Background(), sc, o)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(o.StartDelay + 1500*time.Millisecond)
	a2.srv.Close() // simulate the agent host going away
	closedAt := time.Now()
	waitDone(t, c, 20*time.Second)
	stats := c.AgentStats()
	if stats[0].Status != "finished" || stats[1].Status != "disconnected" {
		t.Fatalf("statuses: %+v", stats)
	}
	if stats[1].Requests == 0 || stats[0].Requests <= stats[1].Requests {
		t.Errorf("expected partial data from a2 and more from a1: %+v", stats)
	}
	f := c.Findings()
	if len(f) != 1 || !strings.Contains(f[0].Title, "disconnected: a2") {
		t.Fatalf("findings: %+v", f)
	}
	snap := c.Col.Snapshot(c.EndedAt, 0)
	if snap.Requests.Count != stats[0].Requests+stats[1].Requests {
		t.Errorf("merged %d != %d+%d", snap.Requests.Count, stats[0].Requests, stats[1].Requests)
	}
	if c.State() != "finished" {
		t.Errorf("state %s", c.State())
	}
	// The disconnected agent hard-stopped its share: no more hits shortly after closing.
	time.Sleep(300 * time.Millisecond)
	rec.mu.Lock()
	h1 := rec.hits
	rec.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	rec.mu.Lock()
	h2 := rec.hits
	rec.mu.Unlock()
	if h2 != h1 {
		t.Errorf("load continued after the run ended (%d -> %d hits), closed at %s", h1, h2, closedAt.Format(time.StampMilli))
	}
}

func TestStopPropagatesToAgents(t *testing.T) {
	mock, _ := startMock(t)
	a1 := startAgent(t, "a1", safety.DefaultPolicy())
	a2 := startAgent(t, "a2", safety.DefaultPolicy())
	sc := scenario(mock.URL, 4, time.Minute)
	sc.Steps[0].Request.URL = "${baseUrl}/delay?ms=3"
	o := baseOpts(t.TempDir(), a1, a2)
	o.LocalShare = 0.5
	c, err := Start(context.Background(), sc, o)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(o.StartDelay + time.Second)
	t0 := time.Now()
	c.Stop()
	waitDone(t, c, 10*time.Second)
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("graceful stop took %s", d)
	}
	for _, s := range c.AgentStats() {
		if s.Status != "finished" || s.Requests == 0 {
			t.Errorf("node after stop: %+v", s)
		}
	}
}

func TestStopBeforeStart(t *testing.T) {
	mock, rec := startMock(t)
	a1 := startAgent(t, "a1", safety.DefaultPolicy())
	o := baseOpts(t.TempDir(), a1)
	o.StartDelay = 2 * time.Second
	o.LocalShare = 0.5
	sc := scenario(mock.URL, 2, 5*time.Second)
	sc.Steps[0].Request.URL = "${baseUrl}/delay?ms=3"
	c, err := Start(context.Background(), sc, o)
	if err != nil {
		t.Fatal(err)
	}
	c.Stop()
	waitDone(t, c, 5*time.Second)
	time.Sleep(2500 * time.Millisecond) // past the planned start
	if rec.hits != 0 {
		t.Fatalf("load generated after a stop before start: %d", rec.hits)
	}
}

func TestSplitAndPlan(t *testing.T) {
	if got := apportion(10, []float64{1, 1, 1}); got[0]+got[1]+got[2] != 10 {
		t.Fatalf("apportion %v", got)
	}
	w := model.Workload{Executor: model.ExecRampingVUs, VUs: 0, Stages: []model.Stage{{Duration: model.Duration(time.Second), Target: 7}, {Duration: model.Duration(time.Second), Target: 3}}}
	parts := SplitWorkload(w, []float64{1, 1})
	if parts[0].Stages[0].Target+parts[1].Stages[0].Target != 7 || parts[0].Stages[1].Target+parts[1].Stages[1].Target != 3 {
		t.Errorf("ramping split %+v %+v", parts[0], parts[1])
	}
	r := SplitWorkload(model.Workload{Executor: model.ExecArrivalRate, Rate: 30, MaxVUs: 9, VUs: 3, Duration: model.Duration(time.Second)}, []float64{2, 1})
	if r[0].Rate != 20 || r[1].Rate != 10 || r[0].MaxVUs+r[1].MaxVUs != 9 {
		t.Errorf("rate split %+v %+v", r[0], r[1])
	}
	few := SplitWorkload(model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(time.Second)}, []float64{1, 1})
	if few[0] == nil || few[1] != nil {
		t.Errorf("1 VU over 2 nodes: %+v %+v", few[0], few[1])
	}
	// per-vu rows rotate by global VU offset.
	sc := model.NewScenario("p")
	sc.Workload = model.Workload{Executor: model.ExecConstantVUs, VUs: 4, Duration: model.Duration(time.Second)}
	sc.Steps = []model.Step{{Type: model.StepRequest, Request: &model.Request{Method: "GET", URL: "http://127.0.0.1/"}}}
	sc.DataSources = []model.DataSource{{Name: "d", Columns: []string{"v"}, Rows: [][]string{{"0"}, {"1"}, {"2"}, {"3"}}, Mode: "per-vu"},
		{Name: "u", Columns: []string{"v"}, Rows: [][]string{{"a"}, {"b"}, {"c"}}, Mode: "unique"}}
	plans, _, err := Plan(sc, t.TempDir(), []float64{1, 1}, []string{"x", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if plans[1].Scenario.DataSources[0].Rows[0][0] != "2" {
		t.Errorf("per-vu rotation: %v", plans[1].Scenario.DataSources[0].Rows)
	}
	if len(plans[0].Scenario.DataSources[1].Rows)+len(plans[1].Scenario.DataSources[1].Rows) != 3 {
		t.Errorf("unique partition")
	}
	sc.DataSources[1].Rows = [][]string{{"a"}}
	if _, _, err := Plan(sc, t.TempDir(), []float64{1, 1}, []string{"x", "y"}); err == nil {
		t.Error("expected error: fewer unique rows than nodes")
	}
}
