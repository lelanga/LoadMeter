package distributed

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/engine"
	"github.com/lelanga/phoenix/internal/metrics"
	"github.com/lelanga/phoenix/internal/results"
	"github.com/lelanga/phoenix/internal/safety"
)

// AgentConfig configures a load agent.
type AgentConfig struct {
	ID     string
	Token  string        // shared secret; required
	Policy safety.Policy // the agent's OWN policy; never taken from the controller
	Logger *slog.Logger
	// TickEvery is the metric delta interval (default 1s).
	TickEvery time.Duration
}

// Agent executes run shares on behalf of an authenticated controller.
type Agent struct {
	cfg    AgentConfig
	mu     sync.Mutex
	busy   bool
	id     string // current run ID
	stopFn func() // requests a graceful stop of the current run (safe before start)
}

// NewAgent refuses to create an agent without a strong token.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if len(cfg.Token) < 32 {
		return nil, errors.New("agent refuses to start without a shared token of at least 32 characters")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.TickEvery <= 0 {
		cfg.TickEvery = time.Second
	}
	if cfg.ID == "" {
		cfg.ID, _ = os.Hostname()
	}
	return &Agent{cfg: cfg}, nil
}

// Handler returns the authenticated HTTP API (serve it over TLS).
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", a.hello)
	mux.HandleFunc("POST /v1/run", a.runHandler)
	mux.HandleFunc("POST /v1/stop", a.stop)
	return a.auth(mux)
}

// Serve serves the API over TLS on ln until srv is closed.
func (a *Agent) Serve(ln net.Listener, cert tls.Certificate) (*http.Server, <-chan error) {
	srv := &http.Server{Handler: a.Handler(), TLSConfig: ServerTLS(cert), ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: log.New(io.Discard, "", 0)}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	return srv, errc
}

func (a *Agent) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + a.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			safety.Audit("agent.auth_failed", map[string]any{"remote": r.RemoteAddr, "path": r.URL.Path})
			a.cfg.Logger.Warn("rejected request with a wrong or missing token", "remote", r.RemoteAddr, "path", r.URL.Path)
			time.Sleep(200 * time.Millisecond) // slow down guessing
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Agent) hello(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	busy := a.busy
	a.mu.Unlock()
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, Hello{Protocol: ProtocolVersion, Version: results.Version, AgentID: a.cfg.ID, Hostname: host,
		CPUs: runtime.NumCPU(), Time: time.Now(), Busy: busy, MaxVUs: a.cfg.Policy.MaxVUs, AllowedHosts: len(a.cfg.Policy.AllowedHosts)})
}

func (a *Agent) stop(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	stop, id := a.stopFn, a.id
	a.mu.Unlock()
	if stop != nil {
		safety.Audit("agent.run.stop", map[string]any{"remote": r.RemoteAddr, "runId": id})
		stop()
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopping": stop != nil})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// validateShare applies the agent's own policy to the received scenario.
func (a *Agent) validateShare(req *StartRequest) error {
	if req.Protocol != ProtocolVersion {
		return fmt.Errorf("protocol version %d not supported by this agent (speaks %d)", req.Protocol, ProtocolVersion)
	}
	if req.Scenario == nil {
		return errors.New("no scenario")
	}
	if !app.ValidRunID(req.RunID) {
		return errors.New("invalid run id")
	}
	if d := time.Until(req.StartAt); d > maxStartDelay {
		return fmt.Errorf("start time is %s in the future (max %s)", d.Round(time.Second), maxStartDelay)
	}
	for _, ds := range req.Scenario.DataSources {
		if ds.File != "" {
			return fmt.Errorf("data source %q references a file; distributed runs must ship data inline", ds.Name)
		}
	}
	// The agent's own allowlist decides, whatever the controller allowed.
	if _, err := app.CheckTargets(req.Scenario, a.cfg.Policy, req.Confirmed); err != nil {
		return fmt.Errorf("agent %s policy: %w", a.cfg.ID, err)
	}
	return nil
}

func (a *Agent) runHandler(w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStartBody))
	if err := dec.Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid start request: "+err.Error())
		return
	}
	if err := a.validateShare(&req); err != nil {
		safety.Audit("agent.run.refused", map[string]any{"remote": r.RemoteAddr, "reason": err.Error()})
		a.cfg.Logger.Warn("refused run", "remote", r.RemoteAddr, "reason", err.Error())
		errJSON(w, http.StatusForbidden, err.Error())
		return
	}
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		errJSON(w, http.StatusConflict, "agent is busy with run "+a.id)
		return
	}
	dir, err := os.MkdirTemp("", "phoenix-agent-")
	if err != nil {
		a.mu.Unlock()
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	if err := writeFiles(dir, req.Files); err != nil {
		a.mu.Unlock()
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := engine.Prepare(req.Scenario, engine.Options{BaseDir: dir, Policy: a.cfg.Policy, Seed: req.Seed, Logger: a.cfg.Logger,
		OnCollector: func(c *metrics.Collector) { c.TrackDeltas() }})
	if err != nil {
		a.mu.Unlock()
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("agent %s: %v", a.cfg.ID, err))
		return
	}
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	a.busy, a.id, a.stopFn = true, req.RunID, func() { stopOnce.Do(func() { close(stopCh) }) }
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy, a.id, a.stopFn = false, "", nil
		a.mu.Unlock()
	}()
	safety.Audit("agent.run.accept", map[string]any{"remote": r.RemoteAddr, "runId": req.RunID, "node": req.Node,
		"scenario": req.Scenario.Name, "targets": app.TargetHosts(req.Scenario), "maxVUs": req.Scenario.Workload.MaxPlannedVUs(),
		"duration": req.Scenario.Workload.PlannedDuration().String()})

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(m Message) error {
		if err := enc.Encode(m); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	if err := send(Message{Type: "ready", AgentTime: time.Now()}); err != nil {
		return
	}
	ctx := r.Context()
	select {
	case <-time.After(time.Until(req.StartAt)):
	case <-stopCh:
		_ = send(Message{Type: "final", State: "stopped", AgentTime: time.Now()})
		return
	case <-ctx.Done():
		safety.Audit("agent.run.aborted_before_start", map[string]any{"runId": req.RunID})
		a.cfg.Logger.Warn("controller went away before the start; run aborted", "runId", req.RunID)
		return
	}
	run.Start(context.Background())
	safety.Audit("agent.run.started", map[string]any{"runId": req.RunID, "node": req.Node})
	a.cfg.Logger.Info("run started", "runId", req.RunID, "node", req.Node, "scenario", req.Scenario.Name,
		"executor", req.Scenario.Workload.Executor, "maxVUs", req.Scenario.Workload.MaxPlannedVUs(), "targets", app.TargetHosts(req.Scenario))
	go func() {
		select {
		case <-stopCh: // controller asked for a graceful stop
			run.Stop()
		case <-run.Done():
			return
		case <-ctx.Done():
		}
		select {
		case <-ctx.Done():
			// Controller gone (stream closed) => hard stop: never leave orphaned load.
			run.Stop()
			run.Stop()
		case <-run.Done():
		}
	}()
	t := time.NewTicker(a.cfg.TickEvery)
	defer t.Stop()
	for {
		select {
		case <-run.Done():
			d := run.Col.TakeDelta()
			d.ActiveVUs = 0
			msg := Message{Type: "final", Delta: d, State: string(run.State()), StartedAt: run.StartedAt, EndedAt: run.EndedAt, AgentTime: time.Now()}
			if run.Err() != nil {
				msg.Error = run.Err().Error()
			}
			_ = send(msg)
			safety.Audit("agent.run.end", map[string]any{"runId": req.RunID, "state": string(run.State())})
			a.cfg.Logger.Info("run ended", "runId", req.RunID, "state", string(run.State()), "requests", run.Col.Snapshot(time.Now(), 1<<30).Requests.Count)
			return
		case <-t.C:
			if err := send(Message{Type: "delta", Delta: run.Col.TakeDelta(), AgentTime: time.Now()}); err != nil {
				a.cfg.Logger.Warn("lost the controller; hard-stopping the run", "runId", req.RunID, "err", err.Error())
				run.Stop()
				run.Stop()
				<-run.Done()
				return
			}
		}
	}
}

func writeFiles(dir string, files map[string][]byte) error {
	total := 0
	for rel, b := range files {
		total += len(b)
		if total > maxShippedFiles {
			return errors.New("shipped files too large")
		}
		p, err := engine.SafePath(dir, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
