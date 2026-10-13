package engine

import (
	"context"
	"math"
	mrand "math/rand/v2"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// This file implements the control-flow steps (model/control.go): while,
// foreach, switch, percent, runtime, throughput, rendezvous, critical and the
// shuffle/roundRobin group modes, plus Poisson arrivals and
// newUserEachIteration. Every repeating step is capped by maxIterations and
// stops on run cancellation or graceful stop.

// ctlKey identifies the state of one control step. Steps with an explicit key
// share state per type; otherwise the step itself (its address inside the
// run's scenario, which is never copied after Prepare) is the identity.
type ctlKey struct {
	typ string
	key string
	st  *model.Step
}

func keyOf(st *model.Step) ctlKey {
	if st.Key != "" {
		return ctlKey{typ: st.Type, key: st.Key}
	}
	return ctlKey{typ: st.Type, st: st}
}

// vuControl is per-VU control state (only touched by the VU's goroutine).
type vuControl struct {
	lastOK, hasLast bool                 // outcome of the last request (while "" / LAST)
	counters        map[ctlKey]*ctr      // percent / roundRobin (per VU)
	prev            map[ctlKey]time.Time // throughput vu/active scopes
}

type ctr struct{ passes, execs int }

// note records a request outcome and returns it unchanged.
func (c *vuControl) note(ok bool) bool {
	c.lastOK, c.hasLast = ok, true
	return ok
}

func (c *vuControl) counter(k ctlKey) *ctr {
	if c.counters == nil {
		c.counters = map[ctlKey]*ctr{}
	}
	x := c.counters[k]
	if x == nil {
		x = &ctr{}
		c.counters[k] = x
	}
	return x
}

// runControl is run-wide control state shared by all VUs.
type runControl struct {
	mu       sync.Mutex
	counters map[ctlKey]*ctr
	next     map[ctlKey]time.Time // throughput shared scope
	barriers map[ctlKey]*barrier
	locks    map[string]chan struct{}
	warned   map[ctlKey]bool
	arrRand  *mrand.Rand
}

func (rc *runControl) sharedCounter(k ctlKey, fn func(*ctr)) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.counters == nil {
		rc.counters = map[ctlKey]*ctr{}
	}
	x := rc.counters[k]
	if x == nil {
		x = &ctr{}
		rc.counters[k] = x
	}
	fn(x)
}

func (rc *runControl) barrier(k ctlKey) *barrier {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.barriers == nil {
		rc.barriers = map[ctlKey]*barrier{}
	}
	b := rc.barriers[k]
	if b == nil {
		b = &barrier{}
		rc.barriers[k] = b
	}
	return b
}

func (rc *runControl) lock(name string) chan struct{} {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.locks == nil {
		rc.locks = map[string]chan struct{}{}
	}
	l := rc.locks[name]
	if l == nil {
		l = make(chan struct{}, 1)
		rc.locks[name] = l
	}
	return l
}

// warnOnce reports whether this is the first warning for k in the run.
func (rc *runControl) warnOnce(k ctlKey) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.warned == nil {
		rc.warned = map[ctlKey]bool{}
	}
	if rc.warned[k] {
		return false
	}
	rc.warned[k] = true
	return true
}

// arrivalGap returns the time to the next arrival at rate rt (per second).
func (r *Run) arrivalGap(rt float64) time.Duration {
	mean := float64(time.Second) / rt
	if r.sc.Workload.Arrivals != model.ArrivalsPoisson {
		return time.Duration(mean)
	}
	r.ctl.mu.Lock()
	if r.ctl.arrRand == nil {
		r.ctl.arrRand = mrand.New(mrand.NewPCG(r.opts.Seed, 0xa441))
	}
	e := r.ctl.arrRand.ExpFloat64()
	r.ctl.mu.Unlock()
	return time.Duration(math.Min(e*mean, float64(time.Hour)))
}

// maxDurationGuard stops the run when the safety policy's maximum duration
// has elapsed. Prepare already refuses longer planned durations; this also
// bounds iteration-based workloads (per-vu-iterations without a duration)
// whose runtime/while/throughput steps would otherwise run longer.
func (r *Run) maxDurationGuard() { r.durationTimer(r.opts.Policy.MaxDur()) }

// stopRequested reports run cancellation, or a graceful stop while running
// the main/setup steps (teardown runs under its own context after the stop,
// so its loops still run, bounded by that context and maxIterations).
func (vu *vuState) stopRequested(ctx context.Context) bool {
	return ctx.Err() != nil || (ctx == vu.r.hardCtx && vu.r.stopping())
}

// stopChan is closed on graceful stop for the main/setup steps; nil (never
// ready) for teardown.
func (vu *vuState) stopChan(ctx context.Context) <-chan struct{} {
	if ctx == vu.r.hardCtx {
		return vu.r.stopIters
	}
	return nil
}

// execControl runs the control-flow steps; handled is false for the
// historical step types, which execSteps runs itself.
func (vu *vuState) execControl(ctx context.Context, st *model.Step) (ok, abort, handled bool) {
	switch st.Type {
	case model.StepWhile:
		ok, abort = vu.execWhile(ctx, st)
	case model.StepForeach:
		ok, abort = vu.execForeach(ctx, st)
	case model.StepSwitch:
		ok, abort = vu.execSwitch(ctx, st)
	case model.StepPercent:
		ok, abort = vu.execPercent(ctx, st)
	case model.StepRuntime:
		ok, abort = vu.execRuntime(ctx, st)
	case model.StepThroughput:
		vu.throughput(ctx, st)
		ok = true
	case model.StepRendezvous:
		vu.rendezvous(ctx, st)
		ok = true
	case model.StepCritical:
		ok, abort = vu.execCritical(ctx, st)
	case model.StepGroup:
		switch st.Select {
		case model.SelectShuffle:
			ok, abort = vu.execShuffle(ctx, st)
		case model.SelectRoundRobin:
			ok, abort = vu.execRoundRobin(ctx, st)
		default:
			return false, false, false
		}
	default:
		return false, false, false
	}
	if abort {
		ok = false
	}
	return ok, abort, true
}

func (vu *vuState) capped(st *model.Step, n int) {
	if vu.r.ctl.warnOnce(keyOf(st)) {
		vu.r.log.Warn("control step stopped at maxIterations", "step", st.Name, "type", st.Type, "maxIterations", n, "vu", vu.id)
	}
}

// whileHolds evaluates a while condition. Unlike JMeter, a condition that
// references an unresolved variable is false, so a typo ends the loop.
func (vu *vuState) whileHolds(cond string) bool {
	if _, res := vu.vc.Expand(cond); len(res.Unresolved) > 0 {
		return false
	}
	return vu.vc.EvalCondition(cond)
}

func (vu *vuState) execWhile(ctx context.Context, st *model.Step) (bool, bool) {
	ok := true
	maxN := st.EffectiveMaxIterations()
	cond := strings.TrimSpace(st.Cond)
	var deadline time.Time
	if st.Duration > 0 {
		deadline = time.Now().Add(st.Duration.D())
	}
	for n := 0; ; n++ {
		switch {
		case cond == "" || strings.EqualFold(cond, "LAST"):
			// JMeter: stop after a pass whose last sample failed; LAST also
			// checks the sample before the loop.
			if (n > 0 || cond != "") && vu.ctl.hasLast && !vu.ctl.lastOK {
				return ok, false
			}
		case !vu.whileHolds(cond):
			return ok, false
		}
		if n >= maxN {
			vu.capped(st, maxN)
			return ok, false
		}
		if vu.stopRequested(ctx) || (!deadline.IsZero() && !time.Now().Before(deadline)) {
			return ok, ctx.Err() != nil
		}
		sub, abort := vu.execSteps(ctx, st.Steps)
		ok = ok && sub
		if abort {
			return false, true
		}
	}
}

func (vu *vuState) execForeach(ctx context.Context, st *model.Step) (bool, bool) {
	ok := true
	prefix, _ := vu.vc.Expand(st.Input)
	if !st.NoSeparator {
		prefix += "_"
	}
	maxN := st.EffectiveMaxIterations()
	for i, n := st.Start+1, 0; st.End == 0 || i <= st.End; i, n = i+1, n+1 {
		v, found := vu.vc.Vars[prefix+strconv.Itoa(i)]
		if !found {
			break
		}
		if n >= maxN {
			vu.capped(st, maxN)
			break
		}
		if vu.stopRequested(ctx) {
			return ok, ctx.Err() != nil
		}
		vu.vc.Vars[st.Var] = v
		sub, abort := vu.execSteps(ctx, st.Steps)
		ok = ok && sub
		if abort {
			return false, true
		}
	}
	return ok, false
}

// enabledIdx returns the indexes of the enabled children.
func enabledIdx(steps []model.Step) []int {
	var out []int
	for i := range steps {
		if !steps[i].Disabled {
			out = append(out, i)
		}
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// switchIndex applies JMeter's Switch Controller rules to the enabled children.
func switchIndex(steps []model.Step, en []int, val string) int {
	if len(en) == 0 {
		return -1
	}
	if val == "" {
		return en[0]
	}
	if isDigits(val) {
		if n, err := strconv.Atoi(val); err == nil && n < len(en) {
			return en[n]
		}
		return en[0]
	}
	def := -1
	for _, i := range en {
		if steps[i].Name == val {
			return i
		}
		if def < 0 && strings.EqualFold(steps[i].Name, "default") {
			def = i
		}
	}
	return def
}

func (vu *vuState) execSwitch(ctx context.Context, st *model.Step) (bool, bool) {
	val, _ := vu.vc.Expand(st.Value)
	i := switchIndex(st.Steps, enabledIdx(st.Steps), strings.TrimSpace(val))
	if i < 0 {
		return true, false
	}
	return vu.execSteps(ctx, st.Steps[i:i+1])
}

// percentDecide is JMeter's Throughput Controller rule for a 0-based pass.
func percentDecide(st *model.Step, c *ctr) bool {
	run := false
	if st.Executions > 0 {
		run = c.execs < st.Executions
	} else {
		run = (100*float64(c.execs)+50)/float64(c.passes+1) < st.Percent
	}
	c.passes++
	if run {
		c.execs++
	}
	return run
}

func (vu *vuState) execPercent(ctx context.Context, st *model.Step) (bool, bool) {
	var run bool
	if st.Shared {
		vu.r.ctl.sharedCounter(keyOf(st), func(c *ctr) { run = percentDecide(st, c) })
	} else {
		run = percentDecide(st, vu.ctl.counter(keyOf(st)))
	}
	if !run {
		return true, false
	}
	return vu.execSteps(ctx, st.Steps)
}

// execRuntime repeats the children until the duration has elapsed, checking
// between children like JMeter (a child may overrun the deadline).
func (vu *vuState) execRuntime(ctx context.Context, st *model.Step) (bool, bool) {
	ok := true
	en := enabledIdx(st.Steps)
	if len(en) == 0 || st.Duration <= 0 {
		return true, false
	}
	deadline := time.Now().Add(st.Duration.D())
	maxN := st.EffectiveMaxIterations()
	for n := 0; ; n++ {
		if n >= maxN {
			vu.capped(st, maxN)
			return ok, false
		}
		for _, i := range en {
			if !time.Now().Before(deadline) {
				return ok, false
			}
			if vu.stopRequested(ctx) {
				return ok, ctx.Err() != nil
			}
			sub, abort := vu.execSteps(ctx, st.Steps[i:i+1])
			ok = ok && sub
			if abort {
				return false, true
			}
		}
	}
}

func (vu *vuState) execShuffle(ctx context.Context, st *model.Step) (bool, bool) {
	ok := true
	en := enabledIdx(st.Steps)
	vu.vc.Rand.Shuffle(len(en), func(a, b int) { en[a], en[b] = en[b], en[a] })
	for _, i := range en {
		sub, abort := vu.execSteps(ctx, st.Steps[i:i+1])
		ok = ok && sub
		if abort {
			return false, true
		}
	}
	return ok, false
}

func (vu *vuState) execRoundRobin(ctx context.Context, st *model.Step) (bool, bool) {
	en := enabledIdx(st.Steps)
	if len(en) == 0 {
		return true, false
	}
	var n int
	if st.Shared {
		vu.r.ctl.sharedCounter(keyOf(st), func(c *ctr) { n = c.passes; c.passes++ })
	} else {
		c := vu.ctl.counter(keyOf(st))
		n = c.passes
		c.passes++
	}
	i := en[n%len(en)]
	return vu.execSteps(ctx, st.Steps[i:i+1])
}

// pause waits d (counted as think time, so transactions exclude it) unless
// the run stops first.
func (vu *vuState) pause(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	start := time.Now()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	case <-vu.stopChan(ctx):
	}
	vu.thinkAccum += time.Since(start)
}

// throughputDelay implements JMeter's Constant Throughput Timer rules: the
// per-VU modes never burst to catch up (a late VU loses the deficit); the
// shared mode hands out slots max(now, last+interval) under a lock.
func (vu *vuState) throughputDelay(st *model.Step, now time.Time) time.Duration {
	interval := time.Duration(float64(time.Minute) / st.PerMinute)
	k := keyOf(st)
	if st.Scope == model.ThroughputShared {
		rc := &vu.r.ctl
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.next == nil {
			rc.next = map[ctlKey]time.Time{}
		}
		next := now
		if last, ok := rc.next[k]; ok && last.Add(interval).After(now) {
			next = last.Add(interval)
		}
		rc.next[k] = next
		return next.Sub(now)
	}
	if st.Scope == model.ThroughputActive {
		interval *= time.Duration(max(vu.r.active.Load(), 1))
	}
	if vu.ctl.prev == nil {
		vu.ctl.prev = map[ctlKey]time.Time{}
	}
	prev, seen := vu.ctl.prev[k]
	target := prev.Add(interval)
	if !seen || now.After(target) {
		vu.ctl.prev[k] = now
		return 0
	}
	vu.ctl.prev[k] = target
	return target.Sub(now)
}

func (vu *vuState) throughput(ctx context.Context, st *model.Step) {
	vu.pause(ctx, vu.throughputDelay(st, time.Now()))
}

// barrier is a reusable rendezvous point with generations: the arrival that
// fills a generation (or a timeout/stop) releases everyone in it, and the next
// arrival starts a new generation.
type barrier struct {
	mu  sync.Mutex
	gen *barrierGen
}

type barrierGen struct {
	n  int
	ch chan struct{}
}

// release closes g if it is still the current generation.
func (b *barrier) release(g *barrierGen) {
	b.mu.Lock()
	if b.gen == g {
		close(g.ch)
		b.gen = nil
	}
	b.mu.Unlock()
}

// wait blocks until size arrivals, timeout, ctx cancellation or stop; it
// returns true when the generation filled.
func (b *barrier) wait(ctx context.Context, stop <-chan struct{}, size func() int, timeout time.Duration) bool {
	b.mu.Lock()
	g := b.gen
	if g == nil {
		g = &barrierGen{ch: make(chan struct{})}
		b.gen = g
	}
	g.n++
	if g.n >= max(size(), 1) {
		close(g.ch)
		b.gen = nil
		b.mu.Unlock()
		return true
	}
	b.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-g.ch:
		return true
	case <-t.C:
	case <-ctx.Done():
	case <-stop:
	}
	b.release(g)
	return false
}

func (vu *vuState) rendezvous(ctx context.Context, st *model.Step) {
	size := func() int {
		if st.Group > 0 {
			return st.Group
		}
		return int(vu.r.active.Load())
	}
	start := time.Now()
	if !vu.r.ctl.barrier(keyOf(st)).wait(ctx, vu.stopChan(ctx), size, st.EffectiveTimeout()) && !vu.stopRequested(ctx) {
		if vu.r.ctl.warnOnce(keyOf(st)) {
			vu.r.log.Warn("rendezvous timed out; releasing waiting VUs", "step", st.Name, "timeout", st.EffectiveTimeout().String())
		}
	}
	vu.thinkAccum += time.Since(start)
}

func (vu *vuState) execCritical(ctx context.Context, st *model.Step) (bool, bool) {
	name := st.Key
	if name == "" {
		name = st.Name
	}
	if name == "" {
		name = "global_lock"
	}
	l := vu.r.ctl.lock(name)
	select {
	case l <- struct{}{}:
	case <-ctx.Done():
		return false, true
	}
	defer func() { <-l }()
	return vu.execSteps(ctx, st.Steps)
}

// freshUser implements workload.newUserEachIteration: a new cookie jar, no
// cached digest challenges and (per-vu connection mode) no kept connections.
func (vu *vuState) freshUser() {
	if !vu.r.sc.Workload.NewUserEachIteration || vu.vc.Iter <= 1 {
		return
	}
	if vu.client.Jar != nil {
		jar, _ := cookiejar.New(nil)
		vu.client.Jar = jar
	}
	vu.auth = authCache{}
	vu.closeIdle()
}
