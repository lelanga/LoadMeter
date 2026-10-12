// Package app wires the engine, results and report into a workspace used by
// the CLI, the HTTP API and the MCP server.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lelanga/loadmeter/internal/engine"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/report"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/safety"
)

type Workspace struct {
	Dir string
}

func (w Workspace) RunsDir() string      { return filepath.Join(w.Dir, "runs") }
func (w Workspace) ScenariosDir() string { return filepath.Join(w.Dir, "scenarios") }

func (w Workspace) Init() error {
	for _, d := range []string{w.RunsDir(), w.ScenariosDir(), filepath.Join(w.Dir, "data"), filepath.Join(w.Dir, "history")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// LoadScenario reads a scenario JSON file.
func LoadScenario(path string) (*model.Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc model.Scenario
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if sc.Version == "" {
		sc.Version = model.SchemaVersion
	}
	return &sc, nil
}

func SaveScenario(path string, sc *model.Scenario) error {
	b, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

type Active struct {
	ID       string
	Dir      string
	Run      *engine.Run
	Scenario *model.Scenario
	baseline string
	ws       Workspace
	raw      *os.File
	once     sync.Once
	summary  *results.Summary
	err      error
	cmpOpts  results.CompareOptions
	finalize func(*results.Summary)
}

type RunOptions struct {
	BaseDir  string
	Policy   safety.Policy
	Baseline string // run ID to compare with
	Inspect  func(engine.Exchange)
	MaxIters int
	Logger   *slog.Logger
	NoRaw    bool
	Compare  results.CompareOptions // statistical options for the baseline comparison
	Finalize func(*results.Summary) // optional: adjust the summary before it is saved
}

// StartRun prepares and starts a run in the workspace.
func (w Workspace) StartRun(ctx context.Context, sc *model.Scenario, o RunOptions) (*Active, error) {
	if err := w.Init(); err != nil {
		return nil, err
	}
	id := results.NewRunID(sc.Name)
	dir := filepath.Join(w.RunsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &Active{ID: id, Dir: dir, Scenario: sc, baseline: o.Baseline, ws: w, cmpOpts: o.Compare, finalize: o.Finalize}
	opts := engine.Options{BaseDir: o.BaseDir, Policy: o.Policy, Inspect: o.Inspect, MaxIters: o.MaxIters, Logger: o.Logger, Seed: 42}
	if !o.NoRaw {
		f, err := os.Create(filepath.Join(dir, "raw.csv"))
		if err != nil {
			return nil, err
		}
		a.raw = f
		opts.Raw = f
	}
	r, err := engine.Prepare(sc, opts)
	if err != nil {
		if a.raw != nil {
			a.raw.Close()
		}
		os.RemoveAll(dir)
		return nil, err
	}
	_ = SaveScenario(filepath.Join(dir, "scenario.json"), sc)
	a.Run = r
	r.Start(ctx)
	return a, nil
}

// Wait blocks until the run ends, then persists summary.json, summary.csv and report.html.
func (a *Active) Wait() (*results.Summary, error) {
	<-a.Run.Done()
	a.once.Do(func() {
		if a.raw != nil {
			a.raw.Close()
		}
		status := string(a.Run.State())
		s := results.Build(a.ID, a.Scenario, a.Run.Col, a.Run.StartedAt, a.Run.EndedAt, status)
		if a.Run.Err() != nil {
			s.Notes = append(s.Notes, "Run error: "+a.Run.Err().Error())
		}
		if a.baseline != "" {
			if b, err := results.Load(filepath.Join(a.ws.RunsDir(), a.baseline)); err == nil {
				co := a.cmpOpts
				co.BaseDir, co.CurDir = filepath.Join(a.ws.RunsDir(), a.baseline), a.Dir
				s.Baseline = results.CompareWith(b, s, co)
			} else {
				s.Notes = append(s.Notes, "Baseline "+a.baseline+" could not be loaded: "+err.Error())
			}
		}
		if a.finalize != nil {
			a.finalize(s)
		}
		if err := results.Save(a.Dir, s); err != nil {
			a.err = err
			return
		}
		f, err := os.Create(filepath.Join(a.Dir, "report.html"))
		if err != nil {
			a.err = err
			return
		}
		defer f.Close()
		a.err = report.Render(f, s, a.Scenario.Description)
		a.summary = s
	})
	return a.summary, a.err
}

// ListRuns returns run IDs, newest first.
func (w Workspace) ListRuns() ([]string, error) {
	ents, err := os.ReadDir(w.RunsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(w.RunsDir(), e.Name(), "summary.json")); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// ValidRunID guards against path traversal in run IDs from API/MCP callers.
func ValidRunID(id string) bool {
	if id == "" || len(id) > 120 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
