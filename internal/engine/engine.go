// Package engine executes scenarios. It is independent of the UI: the CLI,
// HTTP API and MCP server all drive it through Run.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/sysres"
)

type Options struct {
	BaseDir   string        // resolves data/multipart files
	Raw       io.Writer     // optional JTL-compatible raw sample CSV
	Policy    safety.Policy // allowlist + limits
	Seed      uint64
	Logger    *slog.Logger
	Inspect   func(Exchange) // optional per-request detail (debug/validation runs)
	MaxIters  int            // optional hard cap on total iterations (validation runs)
	Overrides *model.Workload
	// OnCollector, when set, is called with the run's collector before any
	// sample is recorded (distributed agents use it to enable delta tracking).
	OnCollector func(*metrics.Collector)
}

// Exchange is the full detail of one request, for the inspector and correlation validation.
type Exchange struct {
	VU          int               `json:"vu"`
	Iteration   int               `json:"iteration"`
	Step        string            `json:"step"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	ReqHeaders  map[string]string `json:"requestHeaders"`
	ReqBody     string            `json:"requestBody,omitempty"`
	Status      int               `json:"status"`
	RespHeaders map[string]string `json:"responseHeaders,omitempty"`
	RespBody    string            `json:"responseBody,omitempty"`
	LatencyMs   float64           `json:"latencyMs"`
	Extracted   map[string]string `json:"extracted,omitempty"`
	Missing     []string          `json:"missingExtractions,omitempty"`
	Assertions  []AssertResult    `json:"assertions,omitempty"`
	VarsUsed    []string          `json:"varsUsed,omitempty"`
	Unresolved  []string          `json:"unresolved,omitempty"`
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	// HTTP-layer detail (exchange.go): timing breakdown, wire vs decoded
	// response bytes, decoded Content-Encoding and compatibility notes.
	Timing          *TimingDetail `json:"timing,omitempty"`
	WireBytes       int64         `json:"wireBytes,omitempty"` // response body bytes on the wire
	BodyBytes       int64         `json:"bodyBytes,omitempty"` // decoded response body bytes
	ContentEncoding string        `json:"contentEncoding,omitempty"`
	Notes           []string      `json:"notes,omitempty"`
}

type AssertResult struct {
	Type    string `json:"type"`
	Expr    string `json:"expr,omitempty"`
	Value   string `json:"value,omitempty"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
}

type State string

const (
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateDone     State = "finished"
	StateFailed   State = "failed"
)

// Run is a live execution.
type Run struct {
	sc        *model.Scenario
	opts      Options
	Col       *metrics.Collector
	StartedAt time.Time
	EndedAt   time.Time
	feeds     map[string]*feed
	transport *http.Transport // shared mode only (see transport.go)
	http      *httpRuntime
	counter   atomic.Int64 // ${__counter()} for this run
	limiter   *safety.Limiter
	active    atomic.Int64
	vuSeq     atomic.Int64
	itersLeft atomic.Int64
	ctl       runControl // control-flow step state (control.go)

	stopIters chan struct{} // closed: no new iterations
	stopOnce  sync.Once
	hardCtx   context.Context
	hardStop  context.CancelFunc
	done      chan struct{}
	state     atomic.Value // State
	err       error
	log       *slog.Logger
}

// Prepare validates the scenario and policy and returns a Run ready to Start.
func Prepare(sc *model.Scenario, opts Options) (*Run, error) {
	if opts.Overrides != nil {
		sc.Workload = *opts.Overrides
	}
	if probs := sc.Validate(); len(probs) > 0 {
		return nil, fmt.Errorf("invalid scenario: %v", probs)
	}
	w := sc.Workload
	if max := w.MaxPlannedVUs(); opts.Policy.MaxVUs > 0 && max > opts.Policy.MaxVUs {
		return nil, fmt.Errorf("workload needs %d VUs but the safety policy allows %d (raise maxVUs in the policy if authorized)", max, opts.Policy.MaxVUs)
	}
	if d := w.PlannedDuration(); d > opts.Policy.MaxDur() {
		return nil, fmt.Errorf("planned duration %s exceeds the safety policy maximum %s", d, opts.Policy.MaxDur())
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &Run{sc: sc, opts: opts, feeds: map[string]*feed{}, stopIters: make(chan struct{}), done: make(chan struct{}), log: opts.Logger}
	for _, ds := range sc.DataSources {
		f, err := loadFeed(ds, opts.BaseDir)
		if err != nil {
			return nil, err
		}
		r.feeds[ds.Name] = f
	}
	if err := r.setupHTTP(); err != nil {
		return nil, err
	}
	r.limiter = safety.NewLimiter(opts.Policy.MaxRPS)
	r.state.Store(StateRunning)
	if opts.MaxIters > 0 {
		r.itersLeft.Store(int64(opts.MaxIters))
	} else {
		r.itersLeft.Store(math.MaxInt64)
	}
	return r, nil
}

func (r *Run) State() State              { return r.state.Load().(State) }
func (r *Run) Done() <-chan struct{}     { return r.done }
func (r *Run) Err() error                { return r.err }
func (r *Run) Scenario() *model.Scenario { return r.sc }

// Stop requests a graceful stop: no new iterations; in-flight iterations get
// GracefulStop (default 5s) before being cancelled. Calling Stop twice hard-stops.
func (r *Run) Stop() {
	first := false
	r.stopOnce.Do(func() {
		first = true
		r.state.Store(StateStopping)
		close(r.stopIters)
		grace := r.sc.Workload.GracefulStop.D()
		if grace <= 0 {
			grace = 5 * time.Second
		}
		go func() {
			select {
			case <-time.After(grace):
				r.hardStop()
			case <-r.done:
			}
		}()
	})
	if !first {
		r.hardStop()
	}
}

func (r *Run) stopping() bool {
	select {
	case <-r.stopIters:
		return true
	default:
		return false
	}
}

// Start launches the workload. ctx cancellation hard-stops the run.
func (r *Run) Start(ctx context.Context) {
	r.StartedAt = time.Now()
	r.hardCtx, r.hardStop = context.WithCancel(ctx)
	r.Col = metrics.NewCollector(r.StartedAt, r.opts.Raw)
	r.Col.SetThreads(func() int { return int(r.active.Load()) }) // JTL grpThreads/allThreads
	if r.opts.OnCollector != nil {
		r.opts.OnCollector(r.Col)
	}
	safety.Audit("run.start", map[string]any{"scenario": r.sc.Name, "executor": r.sc.Workload.Executor,
		"maxVUs": r.sc.Workload.MaxPlannedVUs(), "duration": r.sc.Workload.PlannedDuration().String()})
	go r.sampleResources()
	go func() {
		defer close(r.done)
		defer func() {
			if p := recover(); p != nil {
				r.err = fmt.Errorf("engine panic: %v", p)
				r.state.Store(StateFailed)
			}
		}()
		r.execute()
		r.EndedAt = time.Now()
		r.Col.Flush()
		if r.transport != nil {
			r.transport.CloseIdleConnections()
		}
		r.hardStop()
		if r.State() != StateFailed {
			r.state.Store(StateDone)
		}
		safety.Audit("run.end", map[string]any{"scenario": r.sc.Name, "seconds": r.EndedAt.Sub(r.StartedAt).Seconds()})
	}()
}

func (r *Run) sampleResources() {
	s := sysres.NewSampler()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case now := <-t.C:
			cpu, mem := s.Sample()
			r.Col.SetGauge(now, int(r.active.Load()), cpu, mem)
		}
	}
}

// durationTimer closes stopIters when the planned duration elapses.
func (r *Run) durationTimer(d time.Duration) {
	if d <= 0 {
		return
	}
	go func() {
		select {
		case <-time.After(d):
			r.Stop()
		case <-r.done:
		case <-r.hardCtx.Done():
		}
	}()
}

func (r *Run) execute() {
	w := r.sc.Workload
	r.maxDurationGuard() // control.go: hard stop at the policy maximum
	switch w.Executor {
	case model.ExecConstantVUs:
		r.durationTimer(w.Duration.D())
		r.runVUs(w.VUs, func(time.Duration) int { return w.VUs }, 0)
	case model.ExecRampingVUs:
		total := w.PlannedDuration()
		r.durationTimer(total)
		r.runVUs(w.MaxPlannedVUs(), func(el time.Duration) int { return int(math.Round(stageTarget(float64(w.VUs), w.Stages, el))) }, 0)
	case model.ExecPerVUIterations:
		r.durationTimer(w.Duration.D()) // optional max duration
		r.runVUs(w.VUs, func(time.Duration) int { return w.VUs }, w.Iterations)
	case model.ExecArrivalRate, model.ExecRampingRate:
		var rate func(time.Duration) float64
		var total time.Duration
		if w.Executor == model.ExecArrivalRate {
			rate, total = func(time.Duration) float64 { return w.Rate }, w.Duration.D()
		} else {
			rate, total = func(el time.Duration) float64 { return stageTarget(w.Rate, w.Stages, el) }, w.PlannedDuration()
		}
		r.durationTimer(total)
		r.runArrival(rate)
	}
}

// stageTarget linearly interpolates the target at elapsed time from a start value.
func stageTarget(start float64, stages []model.Stage, el time.Duration) float64 {
	from := start
	var t time.Duration
	for _, s := range stages {
		d := s.Duration.D()
		if el < t+d && d > 0 {
			frac := float64(el-t) / float64(d)
			return from + (s.Target-from)*frac
		}
		t += d
		from = s.Target
	}
	return from
}

// runVUs starts `pool` VU goroutines; VU i runs while i < target(elapsed).
func (r *Run) runVUs(pool int, target func(time.Duration) int, itersPerVU int) {
	var wg sync.WaitGroup
	for i := 1; i <= pool; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			vu := r.newVU(idx)
			defer vu.closeIdle()
			setupDone := false
			iters := 0
			for {
				if r.stopping() || r.hardCtx.Err() != nil {
					break
				}
				if idx > target(time.Since(r.StartedAt)) {
					// Not scheduled yet (ramp-up) or ramped down: idle.
					select {
					case <-time.After(50 * time.Millisecond):
					case <-r.stopIters:
					case <-r.hardCtx.Done():
					}
					continue
				}
				if !setupDone {
					setupDone = true
					r.active.Add(1)
					if !vu.runPhase(r.hardCtx, r.sc.Setup, "setup") {
						r.active.Add(-1)
						return
					}
					r.active.Add(-1)
				}
				if r.itersLeft.Add(-1) < 0 {
					break
				}
				r.active.Add(1)
				start := time.Now()
				cont := vu.iteration(r.hardCtx)
				r.active.Add(-1)
				iters++
				if !cont || (itersPerVU > 0 && iters >= itersPerVU) {
					break
				}
				r.pace(start)
			}
			if setupDone && len(r.sc.Teardown) > 0 && r.hardCtx.Err() == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				vu.runPhase(ctx, r.sc.Teardown, "teardown")
				cancel()
			}
		}(i)
	}
	wg.Wait()
}

func (r *Run) pace(start time.Time) {
	p := r.sc.Workload.Pacing.D()
	if p <= 0 {
		return
	}
	if rem := p - time.Since(start); rem > 0 {
		select {
		case <-time.After(rem):
		case <-r.stopIters:
		case <-r.hardCtx.Done():
		}
	}
}

// runArrival starts iterations at the target rate from a VU pool (open model).
func (r *Run) runArrival(rate func(time.Duration) float64) {
	w := r.sc.Workload
	maxVUs := w.MaxVUs
	if maxVUs <= 0 {
		maxVUs = max(w.VUs, 1)
	}
	pre := max(min(w.VUs, maxVUs), 1)
	idle := make(chan *vuState, maxVUs)
	// A VU whose setup fails is retired (its slot freed for a fresh VU with
	// a new id) instead of being lost from the pool for the rest of the run.
	retired := make(chan struct{}, maxVUs)
	allocated, nextID := 0, 0
	newVU := func() *vuState {
		allocated++
		nextID++
		return r.newVU(nextID)
	}
	for i := 0; i < pre; i++ {
		idle <- newVU()
	}
	var wg sync.WaitGroup
	next := time.Now()
	for {
		if r.stopping() || r.hardCtx.Err() != nil {
			break
		}
		rt := rate(time.Since(r.StartedAt))
		if rt <= 0 {
			next = time.Now().Add(10 * time.Millisecond)
		} else {
			next = next.Add(r.arrivalGap(rt)) // even or poisson (control.go)
		}
		if d := time.Until(next); d > 0 {
			select {
			case <-time.After(d):
			case <-r.stopIters:
				continue
			case <-r.hardCtx.Done():
				continue
			}
		} else if d < -time.Second {
			next = time.Now() // fell far behind: do not burst-catch-up beyond 1s
		}
		if rt <= 0 {
			continue
		}
		for drained := false; !drained; {
			select {
			case <-retired:
				allocated--
			default:
				drained = true
			}
		}
		var vu *vuState
		select {
		case vu = <-idle:
		default:
			if allocated < maxVUs {
				vu = newVU()
			}
		}
		if vu == nil {
			r.Col.DroppedIteration()
			continue
		}
		if r.itersLeft.Add(-1) < 0 {
			idle <- vu
			break
		}
		wg.Add(1)
		go func(vu *vuState) {
			defer wg.Done()
			if !vu.setupDone {
				vu.setupDone = true
				if !vu.runPhase(r.hardCtx, r.sc.Setup, "setup") {
					vu.closeIdle()
					retired <- struct{}{}
					return
				}
			}
			r.active.Add(1)
			vu.iteration(r.hardCtx)
			r.active.Add(-1)
			idle <- vu
		}(vu)
	}
	wg.Wait()
	for {
		select {
		case vu := <-idle:
			vu.closeIdle()
		default:
			return
		}
	}
}

// IsCancel reports errors caused by run cancellation (not counted as failures).
func IsCancel(err error) bool {
	return errors.Is(err, context.Canceled)
}
