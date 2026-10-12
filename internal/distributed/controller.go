package distributed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lelanga/phoenix/internal/engine"
	"github.com/lelanga/phoenix/internal/metrics"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/results"
	"github.com/lelanga/phoenix/internal/safety"
)

// AgentSpec identifies an agent and pins its certificate.
type AgentSpec struct {
	ID          string `json:"id,omitempty"`
	Addr        string `json:"addr"`
	Fingerprint string `json:"fingerprint"`
}

// ParseAgentSpec parses "host:port#sha256fingerprint" (port defaults to 7979).
func ParseAgentSpec(s string) (AgentSpec, error) {
	s = strings.TrimSpace(s)
	addr, fp, _ := strings.Cut(s, "#")
	if addr == "" {
		return AgentSpec{}, fmt.Errorf("empty agent address")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), fmt.Sprint(DefaultPort))
	}
	sp := AgentSpec{Addr: addr, Fingerprint: NormalizeFingerprint(fp)}
	return sp, sp.check()
}

func (a AgentSpec) check() error {
	if len(a.Fingerprint) != 64 {
		return fmt.Errorf("agent %s: a SHA-256 certificate fingerprint is required (use host:port#<fingerprint>; 'phoenix agent' prints it at startup)", a.Addr)
	}
	return nil
}

// AgentsFile is the --agents-file format.
type AgentsFile struct {
	TokenFile string      `json:"tokenFile,omitempty"`
	Agents    []AgentSpec `json:"agents"`
}

// LoadAgentsFile reads and validates an agents.json file.
func LoadAgentsFile(path string) (AgentsFile, error) {
	var f AgentsFile
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	for i := range f.Agents {
		a := &f.Agents[i]
		a.Fingerprint = NormalizeFingerprint(a.Fingerprint)
		if _, _, err := net.SplitHostPort(a.Addr); err != nil {
			a.Addr = net.JoinHostPort(strings.Trim(a.Addr, "[]"), fmt.Sprint(DefaultPort))
		}
		if err := a.check(); err != nil {
			return f, err
		}
	}
	return f, nil
}

// Options configures a distributed run.
type Options struct {
	RunID       string
	Agents      []AgentSpec
	Token       string
	LocalShare  float64 // fraction of the load run on this machine (0 = agents only)
	BaseDir     string
	LocalPolicy safety.Policy // policy for the local share
	Confirmed   bool
	Raw         io.Writer // raw samples of the local share
	Logger      *slog.Logger
	StartDelay  time.Duration // default 2s
	SkewWarn    time.Duration // default 250ms
	Warn        func(string)  // live warnings (optional)
}

type node struct {
	id     string
	spec   AgentSpec
	local  bool
	plan   NodePlan
	client *http.Client
	skew   time.Duration
	col    *metrics.Collector

	mu      sync.Mutex
	status  string
	err     string
	started bool
	run     *engine.Run // local node
	body    io.ReadCloser
	dec     *json.Decoder
}

func (n *node) set(status, err string) {
	n.mu.Lock()
	n.status, n.err = status, err
	n.mu.Unlock()
}

// Controller coordinates one distributed run.
type Controller struct {
	opts      Options
	sc        *model.Scenario
	Col       *metrics.Collector // merged metrics of every node
	StartedAt time.Time
	EndedAt   time.Time
	nodes     []*node

	mu       sync.Mutex
	notes    []string
	findings []results.Finding

	ctx      context.Context
	cancel   context.CancelFunc
	stopReq  chan struct{}
	stopOnce sync.Once
	stops    atomic.Int32
	done     chan struct{}
}

func (c *Controller) note(s string) {
	c.mu.Lock()
	c.notes = append(c.notes, s)
	c.mu.Unlock()
	if c.opts.Warn != nil {
		c.opts.Warn(s)
	}
}

func (c *Controller) finding(f results.Finding) {
	c.mu.Lock()
	c.findings = append(c.findings, f)
	c.mu.Unlock()
}

func newClient(fp string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil, // agents are reached directly
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       ClientTLS(fp),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute, // agent prepares the run before answering
		ForceAttemptHTTP2:     false,
	}}
}

func (c *Controller) do(ctx context.Context, n *node, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+n.spec.Addr+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		var fe *FingerprintError
		if errors.As(err, &fe) {
			return nil, fe
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if resp.StatusCode == http.StatusUnauthorized {
			msg = "agent rejected the token (check --agent-token-file)"
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return resp, nil
}

// Start contacts every agent, plans the shares, and starts all nodes at a
// synchronized instant. Any agent failing before the start aborts the run
// on every node; after the start, a lost agent is reported and the run
// continues on the others.
func Start(ctx context.Context, sc *model.Scenario, o Options) (*Controller, error) {
	if len(o.Agents) == 0 {
		return nil, errors.New("no agents given")
	}
	if len(o.Token) < 32 {
		return nil, errors.New("a shared agent token of at least 32 characters is required (--agent-token-file)")
	}
	if o.LocalShare < 0 || o.LocalShare >= 1 {
		return nil, errors.New("--local-share must be in [0, 1)")
	}
	if o.StartDelay <= 0 {
		o.StartDelay = 2 * time.Second
	}
	if o.SkewWarn <= 0 {
		o.SkewWarn = 250 * time.Millisecond
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if probs := sc.Validate(); len(probs) > 0 {
		return nil, fmt.Errorf("invalid scenario: %v", probs)
	}
	if d := sc.Workload.PlannedDuration(); d > o.LocalPolicy.MaxDur() {
		return nil, fmt.Errorf("planned duration %s exceeds the safety policy maximum %s", d, o.LocalPolicy.MaxDur())
	}
	c := &Controller{opts: o, sc: sc, stopReq: make(chan struct{}), done: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(ctx)
	ok := false
	defer func() {
		if !ok {
			c.cancel()
		}
	}()

	var weights []float64
	var ids []string
	seen := map[string]bool{}
	if o.LocalShare > 0 {
		c.nodes = append(c.nodes, &node{id: "local", local: true})
		weights = append(weights, o.LocalShare)
		ids = append(ids, "local")
		seen["local"] = true
	}
	for _, sp := range o.Agents {
		if err := sp.check(); err != nil {
			return nil, err
		}
		id := sp.ID
		if id == "" {
			id = sp.Addr
		}
		if seen[id] {
			return nil, fmt.Errorf("agent %s listed twice", id)
		}
		seen[id] = true
		c.nodes = append(c.nodes, &node{id: id, spec: sp, client: newClient(sp.Fingerprint)})
		weights = append(weights, (1-o.LocalShare)/float64(len(o.Agents)))
		ids = append(ids, id)
	}
	for _, n := range c.nodes {
		n.col = metrics.NewCollector(time.Now(), nil)
		n.status = "pending"
	}

	// 1. Hello: authentication, certificate pin, version, clock skew.
	if err := c.forAgents(func(n *node) error { return c.hello(n) }); err != nil {
		return nil, err
	}

	// 2. Plan shares (workload split, inline data, unique partitions).
	plans, notes, err := Plan(sc, o.BaseDir, weights, ids)
	if err != nil {
		return nil, err
	}
	for _, s := range notes {
		c.note(s)
	}
	for i, n := range c.nodes {
		n.plan = plans[i]
		if n.plan.Scenario == nil {
			n.status = "idle"
		}
	}

	// 3. Prepare the local share first so local problems fail fast.
	for _, n := range c.nodes {
		if n.local && n.plan.Scenario != nil {
			r, err := engine.Prepare(n.plan.Scenario, engine.Options{BaseDir: o.BaseDir, Policy: o.LocalPolicy, Raw: o.Raw, Logger: o.Logger,
				Seed: n.plan.Seed, OnCollector: func(col *metrics.Collector) { col.TrackDeltas() }})
			if err != nil {
				return nil, fmt.Errorf("local share: %w", err)
			}
			n.run = r
		}
	}

	// 4. Send every agent its share with a common start instant (corrected
	// by each agent's measured clock offset) and wait until all are ready.
	startAt := time.Now().Add(o.StartDelay)
	c.StartedAt = startAt
	c.Col = metrics.NewCollector(startAt, nil)
	for _, n := range c.nodes {
		n.col = metrics.NewCollector(startAt, nil)
	}
	err = c.forAgents(func(n *node) error {
		if n.plan.Scenario == nil {
			return nil
		}
		req := StartRequest{Protocol: ProtocolVersion, RunID: o.RunID, Node: n.id, Scenario: n.plan.Scenario, Files: n.plan.Files,
			StartAt: startAt.Add(n.skew), Seed: n.plan.Seed, Confirmed: o.Confirmed}
		resp, err := c.do(c.ctx, n, http.MethodPost, "/v1/run", req)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(resp.Body)
		var m Message
		if err := dec.Decode(&m); err != nil {
			resp.Body.Close()
			return fmt.Errorf("no ready message: %w", err)
		}
		if m.Type != "ready" {
			resp.Body.Close()
			return fmt.Errorf("unexpected %q message: %s", m.Type, m.Error)
		}
		n.body, n.dec = resp.Body, dec
		n.status = "ready"
		return nil
	})
	if err != nil {
		for _, n := range c.nodes {
			if n.body != nil {
				n.body.Close() // closing the stream makes the agent abort before starting
			}
		}
		return nil, err
	}
	if late := time.Since(startAt); late > 0 {
		c.note(fmt.Sprintf("agents became ready %s after the planned start; nodes started as soon as they were ready", late.Round(time.Millisecond)))
	}
	safety.Audit("distributed.start", map[string]any{"runId": o.RunID, "scenario": sc.Name, "agents": ids, "localShare": o.LocalShare})

	// 5. Run.
	var wg sync.WaitGroup
	for _, n := range c.nodes {
		switch {
		case n.plan.Scenario == nil:
		case n.local:
			wg.Add(1)
			go func() { defer wg.Done(); c.runLocal(n, startAt) }()
		default:
			wg.Add(1)
			go func() { defer wg.Done(); c.readAgent(n) }()
		}
	}
	go func() {
		wg.Wait()
		c.EndedAt = time.Now()
		c.cancel()
		safety.Audit("distributed.end", map[string]any{"runId": o.RunID, "state": c.State()})
		close(c.done)
	}()
	ok = true
	return c, nil
}

// forAgents runs fn on every remote agent concurrently and joins the errors.
func (c *Controller) forAgents(fn func(*node) error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(c.nodes))
	for i, n := range c.nodes {
		if n.local {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(n); err != nil {
				errs[i] = fmt.Errorf("agent %s: %w", n.id, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (c *Controller) hello(n *node) error {
	ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	defer cancel()
	// Several round trips on the kept-alive connection; the one with the
	// smallest RTT gives the best clock-offset estimate (NTP-style).
	best := time.Duration(math.MaxInt64)
	for i := 0; i < 4; i++ {
		t0 := time.Now()
		resp, err := c.do(ctx, n, http.MethodGet, "/v1/hello", nil)
		if err != nil {
			return err
		}
		var h Hello
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h)
		resp.Body.Close()
		t1 := time.Now()
		if err != nil {
			return fmt.Errorf("bad hello: %w", err)
		}
		if h.Protocol != ProtocolVersion {
			return fmt.Errorf("agent speaks protocol %d, controller %d (use the same Phoenix version)", h.Protocol, ProtocolVersion)
		}
		if h.Busy {
			return errors.New("agent is busy with another run")
		}
		if rtt := t1.Sub(t0); rtt < best {
			best = rtt
			n.skew = h.Time.Sub(t0.Add(rtt / 2))
		}
	}
	if math.Abs(float64(n.skew)) > float64(c.opts.SkewWarn) {
		c.note(fmt.Sprintf("clock skew on agent %s is %s (measured, RTT %s); start time was corrected by this offset, but per-second timelines may be misaligned; sync clocks (NTP)",
			n.id, n.skew.Round(time.Millisecond), best.Round(time.Millisecond)))
	}
	return nil
}

func (c *Controller) merge(n *node, d *metrics.Delta) error {
	if d == nil {
		return nil
	}
	if err := c.Col.MergeDelta(n.id, d); err != nil {
		return err
	}
	return n.col.MergeDelta(n.id, d)
}

func (c *Controller) readAgent(n *node) {
	defer n.body.Close()
	n.set("running", "")
	for {
		var m Message
		if err := n.dec.Decode(&m); err != nil {
			c.Col.SetNodeActive(n.id, 0)
			if c.ctx.Err() != nil {
				n.set("stopped", "aborted by the controller")
				return
			}
			el := time.Since(c.StartedAt).Seconds()
			n.set("disconnected", err.Error())
			others := 0
			for _, o := range c.nodes {
				if o != n && o.plan.Scenario != nil {
					others++
				}
			}
			c.note(fmt.Sprintf("lost agent %s at t=%.1fs: %v; the run continues on the other node(s)", n.id, el, err))
			c.finding(results.Finding{Kind: "observation", Severity: "warning", Title: "Load agent disconnected: " + n.id,
				Evidence: fmt.Sprintf("The connection to agent %s (%s) was lost %.1fs into the run (%v). Its samples up to that point are included; the run continued on %d other node(s), so the applied load was lower than planned after the disconnect.",
					n.id, n.spec.Addr, el, err, others)})
			return
		}
		if m.Type == "error" {
			n.set("failed", m.Error)
			c.Col.SetNodeActive(n.id, 0)
			return
		}
		if err := c.merge(n, m.Delta); err != nil {
			n.set("failed", "invalid metrics: "+err.Error())
			c.Col.SetNodeActive(n.id, 0)
			return
		}
		if m.Type == "final" {
			c.Col.SetNodeActive(n.id, 0)
			n.set(m.State, m.Error)
			if m.Error != "" || m.State == string(engine.StateFailed) {
				c.finding(results.Finding{Kind: "observation", Severity: "critical", Title: "Load agent failed: " + n.id,
					Evidence: fmt.Sprintf("Agent %s ended in state %s: %s", n.id, m.State, m.Error)})
			}
			return
		}
	}
}

func (c *Controller) runLocal(n *node, startAt time.Time) {
	select {
	case <-time.After(time.Until(startAt)):
	case <-c.stopReq:
		n.set("stopped", "")
		return
	case <-c.ctx.Done():
		n.set("stopped", "")
		return
	}
	n.mu.Lock()
	n.started = true
	n.run.Start(c.ctx)
	n.mu.Unlock()
	n.set("running", "")
	select {
	case <-c.stopReq:
		n.run.Stop()
	default:
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-n.run.Done():
			d := n.run.Col.TakeDelta()
			d.ActiveVUs = 0
			_ = c.merge(n, d)
			msg := ""
			if n.run.Err() != nil {
				msg = n.run.Err().Error()
			}
			n.set(string(n.run.State()), msg)
			return
		case <-t.C:
			_ = c.merge(n, n.run.Col.TakeDelta())
		}
	}
}

// Stop requests a graceful stop on every node; a second call hard-stops
// (closing the streams, which makes agents cancel in-flight requests).
func (c *Controller) Stop() {
	if c.stops.Add(1) == 1 {
		c.stopOnce.Do(func() { close(c.stopReq) })
		for _, n := range c.nodes {
			if n.local {
				n.mu.Lock()
				if n.started {
					n.run.Stop()
				}
				n.mu.Unlock()
				continue
			}
			if n.plan.Scenario == nil {
				continue
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if resp, err := c.do(ctx, n, http.MethodPost, "/v1/stop", nil); err == nil {
					resp.Body.Close()
				}
			}()
		}
		return
	}
	c.cancel()
	for _, n := range c.nodes {
		if n.local {
			n.mu.Lock()
			if n.started {
				n.run.Stop()
			}
			n.mu.Unlock()
		}
	}
}

// Done is closed when every node has finished, failed or disconnected.
func (c *Controller) Done() <-chan struct{} { return c.done }

// State is "finished" when at least one node finished, otherwise "failed".
func (c *Controller) State() string {
	for _, n := range c.nodes {
		n.mu.Lock()
		s := n.status
		n.mu.Unlock()
		if s == string(engine.StateDone) {
			return string(engine.StateDone)
		}
	}
	return string(engine.StateFailed)
}

// Notes returns warnings collected during the run (plan notes, skew, disconnects).
func (c *Controller) Notes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.notes...)
}

// Findings returns findings about the distributed run itself (disconnects, failures).
func (c *Controller) Findings() []results.Finding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]results.Finding(nil), c.findings...)
}

// AgentStats is the per-node breakdown for the summary.
func (c *Controller) AgentStats() []results.AgentStat {
	end := c.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	var out []results.AgentStat
	for _, n := range c.nodes {
		n.mu.Lock()
		st := results.AgentStat{ID: n.id, Address: n.spec.Addr, Status: n.status, Error: n.err,
			ClockSkewMs: math.Round(float64(n.skew)/float64(time.Millisecond)*10)/10 + 0}
		n.mu.Unlock()
		if n.plan.Scenario != nil {
			st.VUs = n.plan.Scenario.Workload.MaxPlannedVUs()
			st.Rate = math.Round(n.plan.Scenario.Workload.Rate*100) / 100
		}
		r := n.col.Snapshot(end, 1<<30).Requests
		st.Requests, st.Errors, st.ErrorRate, st.P95Ms = r.Count, r.Errors, r.ErrorRate, r.P95Ms
		out = append(out, st)
	}
	return out
}

// Shares describes each node's planned share, for display before the run.
func (c *Controller) Shares() []string {
	var out []string
	for _, n := range c.nodes {
		if n.plan.Scenario == nil {
			out = append(out, fmt.Sprintf("%s: idle", n.id))
			continue
		}
		w := n.plan.Scenario.Workload
		s := fmt.Sprintf("%s: %s, up to %d VUs", n.id, w.Executor, w.MaxPlannedVUs())
		if w.Rate > 0 {
			s += fmt.Sprintf(", %.2f it/s", w.Rate)
		}
		if !n.local {
			s += fmt.Sprintf(", clock offset %s", n.skew.Round(time.Millisecond))
		}
		out = append(out, s)
	}
	return out
}
