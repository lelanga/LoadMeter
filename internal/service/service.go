// Package service is the application API shared by the HTTP server, the MCP
// server and the AI assistant. All entry points go through the same validation,
// safety policy and audit logging.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/phoenix/examples"
	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/correlate"
	"github.com/lelanga/phoenix/internal/engine"
	"github.com/lelanga/phoenix/internal/har"
	"github.com/lelanga/phoenix/internal/jmx"
	"github.com/lelanga/phoenix/internal/loadrunner"
	"github.com/lelanga/phoenix/internal/mockserver"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/param"
	"github.com/lelanga/phoenix/internal/redact"
	"github.com/lelanga/phoenix/internal/results"
	"github.com/lelanga/phoenix/internal/safety"
)

type Service struct {
	WS        app.Workspace
	mu        sync.Mutex
	runs      map[string]*app.Active
	activeID  string
	mockURL   string
	mockClose func()
	// ExtraAllowed are hosts allowlisted for this process only (CLI --allow-host).
	ExtraAllowed []string
}

func New(ws app.Workspace) (*Service, error) {
	if err := ws.Init(); err != nil {
		return nil, err
	}
	for _, d := range []string{"recordings", "imports"} {
		if err := os.MkdirAll(filepath.Join(ws.Dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Service{WS: ws, runs: map[string]*app.Active{}}, nil
}

// Error kinds the transports map to status codes.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	ErrBusy     = errors.New("a test is already running; stop it first")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var fileRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,100}\.json$`)

func (s *Service) scenarioPath(file string) (string, error) {
	if !fileRe.MatchString(file) || strings.Contains(file, "..") {
		return "", invalid("invalid scenario file name %q", file)
	}
	return filepath.Join(s.WS.ScenariosDir(), file), nil
}

func (s *Service) Policy() safety.Policy {
	p, err := safety.LoadPolicy()
	if err != nil {
		p = safety.DefaultPolicy()
	}
	p.AllowedHosts = append(p.AllowedHosts, s.ExtraAllowed...)
	return p
}

// ---------- Scenarios ----------

type ScenarioInfo struct {
	File        string    `json:"file"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Requests    int       `json:"requests"`
	Executor    string    `json:"executor"`
	MaxVUs      int       `json:"maxVUs"`
	Duration    string    `json:"duration"`
	Modified    time.Time `json:"modified"`
	Error       string    `json:"error,omitempty"`
}

func (s *Service) ListScenarios() ([]ScenarioInfo, error) {
	ents, err := os.ReadDir(s.WS.ScenariosDir())
	if err != nil {
		return nil, err
	}
	out := []ScenarioInfo{}
	for _, e := range ents {
		if e.IsDir() || !fileRe.MatchString(e.Name()) {
			continue
		}
		info := ScenarioInfo{File: e.Name()}
		if fi, err := e.Info(); err == nil {
			info.Modified = fi.ModTime()
		}
		sc, err := app.LoadScenario(filepath.Join(s.WS.ScenariosDir(), e.Name()))
		if err != nil {
			info.Error = err.Error()
		} else {
			info.Name, info.Description, info.Requests = sc.Name, sc.Description, len(sc.AllRequests())
			info.Executor, info.MaxVUs, info.Duration = sc.Workload.Executor, sc.Workload.MaxPlannedVUs(), sc.Workload.PlannedDuration().String()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

func (s *Service) GetScenario(file string) (*model.Scenario, error) {
	p, err := s.scenarioPath(file)
	if err != nil {
		return nil, err
	}
	sc, err := app.LoadScenario(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: scenario %s", ErrNotFound, file)
	}
	return sc, err
}

// SaveScenario snapshots the previous version into history, then writes.
func (s *Service) SaveScenario(file string, sc *model.Scenario, reason string) ([]string, error) {
	p, err := s.scenarioPath(file)
	if err != nil {
		return nil, err
	}
	if sc.Version == "" {
		sc.Version = model.SchemaVersion
	}
	if _, err := os.Stat(p); err == nil {
		if err := s.snapshot(file, reason); err != nil {
			return nil, err
		}
	}
	if err := app.SaveScenario(p, sc); err != nil {
		return nil, err
	}
	return sc.Validate(), nil
}

func (s *Service) DeleteScenario(file string) error {
	p, err := s.scenarioPath(file)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("%w: scenario %s", ErrNotFound, file)
	}
	if err := s.snapshot(file, "deleted"); err != nil {
		return err
	}
	safety.Audit("scenario.deleted", map[string]any{"file": file})
	return os.Remove(p)
}

// uniqueFile returns a free scenario file name derived from name.
func (s *Service) uniqueFile(name string) string {
	base := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "scenario"
	}
	if len(base) > 60 {
		base = base[:60]
	}
	f := base + ".json"
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(s.WS.ScenariosDir(), f)); os.IsNotExist(err) {
			return f
		}
		f = fmt.Sprintf("%s-%d.json", base, i)
	}
}

// ---------- History ----------

type HistoryEntry struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Reason string    `json:"reason"`
}

func (s *Service) historyDir(file string) string {
	return filepath.Join(s.WS.Dir, "history", strings.TrimSuffix(file, ".json"))
}

func (s *Service) snapshot(file, reason string) error {
	p, _ := s.scenarioPath(file)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	d := s.historyDir(file)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	reason = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(reason), "-")
	if reason == "" {
		reason = "edit"
	}
	id := time.Now().UTC().Format("20060102T150405.000000000") + "_" + reason
	return os.WriteFile(filepath.Join(d, id+".json"), b, 0o644)
}

func (s *Service) History(file string) ([]HistoryEntry, error) {
	if _, err := s.scenarioPath(file); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(s.historyDir(file))
	if os.IsNotExist(err) {
		return []HistoryEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []HistoryEntry{}
	for _, e := range ents {
		id := strings.TrimSuffix(e.Name(), ".json")
		ts, reason, _ := strings.Cut(id, "_")
		t, _ := time.Parse("20060102T150405.000000000", ts)
		out = append(out, HistoryEntry{ID: id, Time: t, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

var histRe = regexp.MustCompile(`^[0-9T.]+_[a-z0-9-]+$`)

func (s *Service) historyScenario(file, id string) (*model.Scenario, error) {
	if !histRe.MatchString(id) {
		return nil, invalid("invalid history id")
	}
	sc, err := app.LoadScenario(filepath.Join(s.historyDir(file), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: history %s", ErrNotFound, id)
	}
	return sc, err
}

func (s *Service) Restore(file, id string) error {
	sc, err := s.historyScenario(file, id)
	if err != nil {
		return err
	}
	_, err = s.SaveScenario(file, sc, "restore")
	return err
}

type Change struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// Diff compares a history snapshot with the current scenario (JSON paths).
func (s *Service) Diff(file, id string) ([]Change, error) {
	old, err := s.historyScenario(file, id)
	if err != nil {
		return nil, err
	}
	cur, err := s.GetScenario(file)
	if err != nil {
		cur = &model.Scenario{}
	}
	var a, b any
	ja, _ := json.Marshal(old)
	jb, _ := json.Marshal(cur)
	json.Unmarshal(ja, &a)
	json.Unmarshal(jb, &b)
	var out []Change
	diff("$", a, b, &out)
	return out, nil
}

func diff(p string, a, b any, out *[]Change) {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, Change{p, a, b})
			return
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			diff(p+"."+k, x[k], y[k], out)
		}
	case []any:
		y, ok := b.([]any)
		if !ok {
			*out = append(*out, Change{p, a, b})
			return
		}
		for i := 0; i < max(len(x), len(y)); i++ {
			var av, bv any
			if i < len(x) {
				av = x[i]
			}
			if i < len(y) {
				bv = y[i]
			}
			diff(fmt.Sprintf("%s[%d]", p, i), av, bv, out)
		}
	default:
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) {
			*out = append(*out, Change{p, a, b})
		}
	}
}

// ---------- Imports ----------

type HARImport struct {
	File         string                  `json:"file"`
	Report       *model.CompatReport     `json:"report"`
	Correlations []correlate.Candidate   `json:"correlations"`
	Unexplained  []correlate.Unexplained `json:"dynamicUnexplained"`
	Parameters   []param.Suggestion      `json:"parameters"`
}

func (s *Service) recordingPath(file string) string {
	return filepath.Join(s.WS.Dir, "recordings", strings.TrimSuffix(file, ".json")+".recording.json")
}

func (s *Service) ImportHAR(name string, data []byte, opts har.Options) (*HARImport, error) {
	opts.Name = name
	sc, rep, rec, err := har.Import(bytes.NewReader(data), name+".har", opts)
	if err != nil {
		return nil, invalid("%v", err)
	}
	file := s.uniqueFile(sc.Name)
	if _, err := s.SaveScenario(file, sc, "import"); err != nil {
		return nil, err
	}
	if err := correlate.SaveRecording(s.recordingPath(file), rec); err != nil {
		return nil, err
	}
	safety.Audit("import.har", map[string]any{"file": file, "entries": len(rec.Exchanges), "keepCredentials": opts.KeepCredentials})
	res := correlate.Detect(rec, nil)
	return &HARImport{File: file, Report: rep, Correlations: res.Candidates, Unexplained: res.Unexplained, Parameters: param.Detect(sc)}, nil
}

type BasicImport struct {
	File     string              `json:"file"`
	Report   *model.CompatReport `json:"report"`
	Analysis any                 `json:"analysis,omitempty"`
}

func (s *Service) ImportJMX(name string, data []byte) (*BasicImport, error) {
	sc, rep, err := jmx.Import(bytes.NewReader(data), name+".jmx")
	if err != nil {
		return nil, invalid("%v", err)
	}
	if sc.Name == "" {
		sc.Name = name
	}
	file := s.uniqueFile(sc.Name)
	if _, err := s.SaveScenario(file, sc, "import"); err != nil {
		return nil, err
	}
	safety.Audit("import.jmx", map[string]any{"file": file, "confidence": rep.Confidence})
	return &BasicImport{File: file, Report: rep}, nil
}

func (s *Service) ImportLoadRunner(name string, files map[string][]byte) (*BasicImport, error) {
	if len(files) == 0 {
		return nil, invalid("no files uploaded")
	}
	clean := map[string][]byte{}
	for k, v := range files {
		clean[filepath.Base(strings.ReplaceAll(k, `\`, "/"))] = v
	}
	sc, rep, err := loadrunner.ImportFiles(clean, name)
	an, _ := loadrunner.Analyze(clean, name)
	if errors.Is(err, loadrunner.ErrUnsupportedProtocol) && rep != nil {
		// Nothing executable to save, but the compatibility report explains why.
		return &BasicImport{Report: rep, Analysis: an}, nil
	}
	if err != nil {
		return nil, invalid("%v", err)
	}
	file := s.uniqueFile(sc.Name)
	// Data files referenced by data sources are written next to the scenario.
	for _, ds := range sc.DataSources {
		if ds.File != "" {
			if b, ok := clean[filepath.Base(ds.File)]; ok {
				dst := filepath.Join(s.WS.ScenariosDir(), filepath.Base(ds.File))
				if _, err := os.Stat(dst); os.IsNotExist(err) {
					_ = os.WriteFile(dst, b, 0o600)
				}
			}
		}
	}
	if _, err := s.SaveScenario(file, sc, "import"); err != nil {
		return nil, err
	}
	safety.Audit("import.loadrunner", map[string]any{"file": file, "confidence": rep.Confidence})
	return &BasicImport{File: file, Report: rep, Analysis: an}, nil
}

func (s *Service) ExportJMX(file string) ([]byte, *model.CompatReport, error) {
	sc, err := s.GetScenario(file)
	if err != nil {
		return nil, nil, err
	}
	return jmx.Export(sc)
}

// ---------- Correlation / parameterization ----------

func (s *Service) Correlate(file string) (*correlate.Result, error) {
	if _, err := s.GetScenario(file); err != nil {
		return nil, err
	}
	rec, err := correlate.LoadRecording(s.recordingPath(file))
	if err != nil {
		return nil, invalid("no recording is stored for %s (correlation analysis needs a HAR import with responses)", file)
	}
	return correlate.Detect(rec, nil), nil
}

type ApplyResult struct {
	Applied    []string              `json:"applied"`
	Notes      []string              `json:"notes,omitempty"`
	Validation *correlate.Validation `json:"validation,omitempty"`
	RolledBack bool                  `json:"rolledBack"`
	File       string                `json:"file"`
}

// ApplyCorrelations follows an approval policy:
// suggest (no change), manual (apply as selected), validated (apply, replay, roll
// back on failure), auto (apply all candidates with confidence >= 0.7, validate).
func (s *Service) ApplyCorrelations(ctx context.Context, file string, ids []string, policy string) (*ApplyResult, error) {
	sc, err := s.GetScenario(file)
	if err != nil {
		return nil, err
	}
	res, err := s.Correlate(file)
	if err != nil {
		return nil, err
	}
	sel := map[string]bool{}
	switch policy {
	case "suggest":
		return &ApplyResult{File: file, Notes: []string{"Policy 'suggest': no changes applied."}}, nil
	case "auto":
		for _, c := range res.Candidates {
			if c.Confidence >= 0.7 {
				sel[c.ID] = true
			}
		}
	case "", "validated", "manual":
		for _, id := range ids {
			sel[id] = true
		}
	default:
		return nil, invalid("unknown approval policy %q", policy)
	}
	if len(sel) == 0 {
		return nil, invalid("no correlation candidates selected")
	}
	original := sc.Clone()
	applied, notes := correlate.Apply(sc, res.Candidates, sel)
	out := &ApplyResult{Applied: applied, Notes: notes, File: file}
	if policy != "manual" {
		var chosen []correlate.Candidate
		for _, c := range res.Candidates {
			if sel[c.ID] {
				chosen = append(chosen, c)
			}
		}
		vctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		out.Validation = correlate.Validate(vctx, original, sc, chosen, s.WS.ScenariosDir(), s.Policy())
		if !out.Validation.OK {
			out.RolledBack = true
			out.Notes = append(out.Notes, "Validation failed, so the scenario was not changed. Review the replay details, or apply with the 'manual' policy.")
			safety.Audit("correlation.rejected", map[string]any{"file": file, "ids": applied})
			return out, nil
		}
	}
	if _, err := s.SaveScenario(file, sc, "correlation"); err != nil {
		return nil, err
	}
	safety.Audit("correlation.applied", map[string]any{"file": file, "ids": applied, "policy": policy})
	return out, nil
}

func (s *Service) Parameterize(file string) ([]param.Suggestion, error) {
	sc, err := s.GetScenario(file)
	if err != nil {
		return nil, err
	}
	return param.Detect(sc), nil
}

func (s *Service) ApplyParameters(file string, ids []string, rows int) ([]string, string, error) {
	sc, err := s.GetScenario(file)
	if err != nil {
		return nil, "", err
	}
	sel := map[string]bool{}
	for _, id := range ids {
		sel[id] = true
	}
	data := strings.TrimSuffix(file, ".json") + "-params.csv"
	applied, err := param.Apply(sc, param.Detect(sc), sel, s.WS.ScenariosDir(), data, rows)
	if err != nil {
		return nil, "", invalid("%v", err)
	}
	if _, err := s.SaveScenario(file, sc, "parameterization"); err != nil {
		return nil, "", err
	}
	return applied, data, nil
}

// ---------- Debug ----------

// Debug runs one iteration with one VU and returns redacted exchanges.
func (s *Service) Debug(ctx context.Context, file string, vars map[string]string) ([]engine.Exchange, error) {
	sc, err := s.GetScenario(file)
	if err != nil {
		return nil, err
	}
	for k, v := range vars {
		sc.Variables[k] = v
	}
	if _, err := app.CheckTargets(sc, s.Policy(), true); err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	exs, err := correlate.ReplayOnce(dctx, sc, s.WS.ScenariosDir(), s.Policy())
	for i := range exs {
		e := &exs[i]
		e.ReqHeaders = redact.Headers(e.ReqHeaders)
		e.RespHeaders = redact.Headers(e.RespHeaders)
		e.ReqBody = redact.Body(e.ReqBody)
		e.URL = redact.URL(e.URL)
		for k, v := range e.Extracted {
			if redact.SensitiveName(k) || len(v) > 24 {
				e.Extracted[k] = redact.Mask(v)
			}
		}
	}
	return exs, err
}

// ---------- Runs ----------

type RunRequest struct {
	File      string            `json:"file"`
	Overrides app.Overrides     `json:"overrides"`
	Baseline  string            `json:"baseline,omitempty"`
	Confirm   bool              `json:"confirm,omitempty"`
	Vars      map[string]string `json:"vars,omitempty"`
}

func (s *Service) StartRun(req RunRequest) (string, error) {
	sc, err := s.GetScenario(req.File)
	if err != nil {
		return "", err
	}
	for k, v := range req.Vars {
		sc.Variables[k] = v
	}
	if err := app.ApplyOverrides(sc, req.Overrides); err != nil {
		return "", invalid("%v", err)
	}
	if req.Baseline != "" && !app.ValidRunID(req.Baseline) {
		return "", invalid("invalid baseline run id")
	}
	pol := s.Policy()
	if _, err := app.CheckTargets(sc, pol, req.Confirm); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeID != "" {
		if a := s.runs[s.activeID]; a != nil {
			select {
			case <-a.Run.Done():
			default:
				return "", ErrBusy
			}
		}
	}
	a, err := s.WS.StartRun(context.Background(), sc, app.RunOptions{BaseDir: s.WS.ScenariosDir(), Policy: pol, Baseline: req.Baseline})
	if err != nil {
		return "", invalid("%v", err)
	}
	s.runs[a.ID] = a
	s.activeID = a.ID
	go a.Wait() // persist summary + report as soon as the run ends
	return a.ID, nil
}

func (s *Service) Active(id string) *app.Active {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

type ActiveInfo struct {
	RunID    string `json:"runId,omitempty"`
	State    string `json:"state,omitempty"`
	Scenario string `json:"scenario,omitempty"`
	Planned  string `json:"plannedDuration,omitempty"`
}

func (s *Service) ActiveRun() ActiveInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.runs[s.activeID]
	if a == nil {
		return ActiveInfo{}
	}
	return ActiveInfo{RunID: a.ID, State: string(a.Run.State()), Scenario: a.Scenario.Name, Planned: a.Scenario.Workload.PlannedDuration().String()}
}

func (s *Service) StopRun(id string) error {
	a := s.Active(id)
	if a == nil {
		return fmt.Errorf("%w: active run %s", ErrNotFound, id)
	}
	safety.Audit("run.stop_requested", map[string]any{"runId": id})
	a.Run.Stop()
	return nil
}

// WaitSummary waits for an active run (if any) and returns its summary.
func (s *Service) Summary(id string) (*results.Summary, error) {
	if !app.ValidRunID(id) {
		return nil, invalid("invalid run id")
	}
	if a := s.Active(id); a != nil {
		select {
		case <-a.Run.Done():
			return a.Wait()
		default:
			return nil, invalid("run %s is still in progress", id)
		}
	}
	sum, err := results.Load(filepath.Join(s.WS.RunsDir(), id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	return sum, err
}

func (s *Service) RunDir(id string) (string, error) {
	if !app.ValidRunID(id) {
		return "", invalid("invalid run id")
	}
	return filepath.Join(s.WS.RunsDir(), id), nil
}

type RunInfo struct {
	RunID     string    `json:"runId"`
	Scenario  string    `json:"scenario"`
	StartedAt time.Time `json:"startedAt"`
	DurationS float64   `json:"durationSec"`
	Requests  uint64    `json:"requests"`
	P95       float64   `json:"p95Ms"`
	ErrorRate float64   `json:"errorRate"`
	SLAPassed bool      `json:"slaPassed"`
	HasSLAs   bool      `json:"hasSLAs"`
	Status    string    `json:"status"`
}

func (s *Service) ListRuns() ([]RunInfo, error) {
	ids, err := s.WS.ListRuns()
	if err != nil {
		return nil, err
	}
	out := []RunInfo{}
	for _, id := range ids {
		sm, err := results.Load(filepath.Join(s.WS.RunsDir(), id))
		if err != nil {
			continue
		}
		out = append(out, RunInfo{RunID: id, Scenario: sm.Scenario, StartedAt: sm.StartedAt, DurationS: sm.DurationS, Requests: sm.Snapshot.Requests.Count,
			P95: sm.Snapshot.Requests.P95Ms, ErrorRate: sm.Snapshot.Requests.ErrorRate, SLAPassed: sm.SLAPassed, HasSLAs: len(sm.SLAs) > 0, Status: sm.Status})
	}
	return out, nil
}

func (s *Service) Compare(base, run string) (*results.Comparison, error) {
	b, err := s.Summary(base)
	if err != nil {
		return nil, err
	}
	r, err := s.Summary(run)
	if err != nil {
		return nil, err
	}
	return results.Compare(b, r), nil
}

// ---------- Demo ----------

// Demo starts the bundled mock server (once) and writes demo-shop.json.
func (s *Service) Demo() (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mockURL == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", "", err
		}
		srv := &http.Server{Handler: mockserver.New("phoenix-demo").Handler(), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
		s.mockURL, s.mockClose = "http://"+ln.Addr().String(), func() { srv.Close() }
	}
	b, err := examples.FS.ReadFile("shop-login.json")
	if err != nil {
		return "", "", err
	}
	var sc model.Scenario
	if err := json.Unmarshal(b, &sc); err != nil {
		return "", "", err
	}
	sc.Variables["baseUrl"] = s.mockURL
	sc.Name = "Demo shop (bundled mock server)"
	csv, _ := examples.FS.ReadFile("users.csv")
	if err := os.WriteFile(filepath.Join(s.WS.ScenariosDir(), "users.csv"), csv, 0o644); err != nil {
		return "", "", err
	}
	p := filepath.Join(s.WS.ScenariosDir(), "demo-shop.json")
	if err := app.SaveScenario(p, &sc); err != nil {
		return "", "", err
	}
	return "demo-shop.json", s.mockURL, nil
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.runs {
		a.Run.Stop()
	}
	if s.mockClose != nil {
		s.mockClose()
	}
}
