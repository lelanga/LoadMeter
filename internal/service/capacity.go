package service

// Capacity / breaking-point finder (see internal/capacity). One search at a
// time per service; it is refused while a normal run is active. The same
// policy, allowlist and confirmation rules as StartRun apply.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/capacity"
	"github.com/lelanga/loadmeter/internal/engine"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

// CapacityRequest starts a capacity search. Durations are Go duration strings.
type CapacityRequest struct {
	File        string            `json:"file"`
	Vars        map[string]string `json:"vars,omitempty"`
	Confirm     bool              `json:"confirm,omitempty"`
	StartRate   float64           `json:"startRate,omitempty"`
	StepPct     float64           `json:"stepPct,omitempty"`
	StepAdd     float64           `json:"stepAdd,omitempty"`
	Warmup      string            `json:"warmup,omitempty"`
	Hold        string            `json:"hold,omitempty"`
	Budget      string            `json:"budget,omitempty"`
	MaxRate     float64           `json:"maxRate,omitempty"`
	MaxVUs      int               `json:"maxVUs,omitempty"`
	BreachSteps int               `json:"breachSteps,omitempty"`
	RefineSteps *int              `json:"refineSteps,omitempty"`
	P95Ms       float64           `json:"p95Ms,omitempty"`
	MaxErrorPct *float64          `json:"maxErrorPct,omitempty"`
	TargetRate  float64           `json:"targetRate,omitempty"`
	HeadroomPct float64           `json:"headroomPct,omitempty"`
	Instances   int               `json:"instancesTested,omitempty"`
}

// CapacityInfo is the live state of a search.
type CapacityInfo struct {
	ID        string           `json:"id"`
	File      string           `json:"file"`
	State     string           `json:"state"` // running | finished | failed
	Started   time.Time        `json:"started"`
	Plan      *capacity.Plan   `json:"plan,omitempty"`
	Criteria  []model.SLA      `json:"criteria,omitempty"`
	Steps     []capacity.Step  `json:"steps"`
	Error     string           `json:"error,omitempty"`
	Result    *capacity.Result `json:"result,omitempty"`
	ResultDir string           `json:"resultDir,omitempty"`
}

type capSession struct {
	mu     sync.Mutex
	info   CapacityInfo
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *capSession) snapshot() CapacityInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.info
	i.Steps = append([]capacity.Step{}, c.info.Steps...)
	return i
}

var (
	capMu   sync.Mutex
	capBySv = map[*Service]*capSession{}
)

func parseDur(name, v string) (model.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, invalid("%s %q: use a value like 5s or 2m", name, v)
	}
	return model.Duration(d), nil
}

// CapacityBusy reports whether a capacity search is running.
func (s *Service) CapacityBusy() bool {
	capMu.Lock()
	cur := capBySv[s]
	capMu.Unlock()
	if cur == nil {
		return false
	}
	select {
	case <-cur.done:
		return false
	default:
		return true
	}
}

// StartCapacity validates the request under the safety policy and starts the
// search in the background. The result is saved under
// <workspace>/capacity/<id>/ when it ends.
func (s *Service) StartCapacity(req CapacityRequest) (*CapacityInfo, error) {
	sc, err := s.GetScenario(req.File)
	if err != nil {
		return nil, err
	}
	if len(req.Vars) > 0 && sc.Variables == nil {
		sc.Variables = map[string]string{}
	}
	for k, v := range req.Vars {
		sc.Variables[k] = v
	}
	plan := capacity.Plan{StartRate: req.StartRate, StepPct: req.StepPct, StepAdd: req.StepAdd, MaxRate: req.MaxRate,
		MaxVUs: req.MaxVUs, BreachSteps: req.BreachSteps, RefineSteps: capacity.DefaultRefineSteps}
	if req.RefineSteps != nil {
		plan.RefineSteps = *req.RefineSteps
	}
	if plan.Warmup, err = parseDur("warmup", req.Warmup); err != nil {
		return nil, err
	}
	if plan.Hold, err = parseDur("hold", req.Hold); err != nil {
		return nil, err
	}
	if plan.Budget, err = parseDur("budget", req.Budget); err != nil {
		return nil, err
	}
	maxErr := -1.0
	if req.MaxErrorPct != nil {
		maxErr = *req.MaxErrorPct
	}
	pol := s.Policy()
	// Validate criteria and plan up front so errors reach the caller synchronously.
	if _, _, err := capacity.Criteria(sc, req.P95Ms, maxErr); err != nil {
		return nil, invalid("%v", err)
	}
	if _, _, err := capacity.PreparePlan(sc, plan, pol, req.Confirm); err != nil {
		var hna *safety.ErrHostNotAllowed
		var nc *app.ErrNeedsConfirmation
		if errors.As(err, &hna) || errors.As(err, &nc) {
			return nil, err
		}
		return nil, invalid("%v", err)
	}

	capMu.Lock()
	defer capMu.Unlock()
	if cur := capBySv[s]; cur != nil {
		select {
		case <-cur.done:
		default:
			return nil, fmt.Errorf("%w: a capacity search is already running (id %s)", ErrBusy, cur.info.ID)
		}
	}
	if st := s.ActiveRun().State; st == string(engine.StateRunning) || st == string(engine.StateStopping) {
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess := &capSession{cancel: cancel, done: make(chan struct{}),
		info: CapacityInfo{ID: "pending", File: req.File, State: "running", Started: time.Now(), Steps: []capacity.Step{}}}
	capBySv[s] = sess
	opts := capacity.Options{Scenario: sc, BaseDir: s.WS.ScenariosDir(), Policy: pol, Confirmed: req.Confirm, Plan: plan,
		P95Ms: req.P95Ms, MaxErrorPct: maxErr, TargetRate: req.TargetRate, HeadroomPct: req.HeadroomPct, InstancesTested: req.Instances,
		OnStart: func(p capacity.Plan, c []model.SLA) {
			sess.mu.Lock()
			sess.info.Plan, sess.info.Criteria = &p, c
			sess.mu.Unlock()
		},
		OnStep: func(st capacity.Step) {
			sess.mu.Lock()
			sess.info.Steps = append(sess.info.Steps, st)
			sess.mu.Unlock()
		}}
	go func() {
		defer close(sess.done)
		defer cancel()
		res, err := capacity.Find(ctx, opts)
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if err != nil {
			sess.info.State, sess.info.Error = "failed", err.Error()
			return
		}
		dir := capacity.Dir(s.WS.Dir, res.ID)
		if err := capacity.Save(dir, res); err != nil {
			sess.info.Error = "saving result: " + err.Error()
		}
		sess.info.ID, sess.info.State, sess.info.Result, sess.info.ResultDir = res.ID, "finished", res, dir
	}()
	info := sess.snapshot()
	return &info, nil
}

// ActiveCapacity returns the current or last search of this service (nil if none).
func (s *Service) ActiveCapacity() *CapacityInfo {
	capMu.Lock()
	cur := capBySv[s]
	capMu.Unlock()
	if cur == nil {
		return nil
	}
	i := cur.snapshot()
	return &i
}

// StopCapacity cancels the running search; the partial result is still saved.
func (s *Service) StopCapacity() error {
	capMu.Lock()
	cur := capBySv[s]
	capMu.Unlock()
	if cur == nil {
		return fmt.Errorf("%w: no capacity search", ErrNotFound)
	}
	select {
	case <-cur.done:
		return nil
	default:
	}
	safety.Audit("capacity.stop_requested", nil)
	cur.cancel()
	return nil
}

// WaitCapacity waits for the running search (or ctx) and returns its state.
func (s *Service) WaitCapacity(ctx context.Context) *CapacityInfo {
	capMu.Lock()
	cur := capBySv[s]
	capMu.Unlock()
	if cur == nil {
		return nil
	}
	select {
	case <-cur.done:
	case <-ctx.Done():
	}
	i := cur.snapshot()
	return &i
}

// CapacityResult loads a saved result by ID.
func (s *Service) CapacityResult(id string) (*capacity.Result, error) {
	if !app.ValidRunID(id) {
		return nil, invalid("invalid capacity result id")
	}
	r, err := capacity.Load(capacity.Dir(s.WS.Dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: capacity result %s", ErrNotFound, id)
	}
	return r, err
}

// ListCapacity lists saved capacity result IDs, newest first.
func (s *Service) ListCapacity() ([]string, error) { return capacity.List(s.WS.Dir) }
