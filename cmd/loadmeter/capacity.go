package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/capacity"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

func init() {
	register("capacity", "Find the max sustainable rate (breaking point): loadmeter capacity scenario.json [--p95 MS --max-error-pct P --max-rate N]", cmdCapacity)
}

func cmdCapacity(args []string) error {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	var (
		workspace                       string
		startRate, stepPct, stepAdd     float64
		maxRate, p95, maxErr            float64
		headroom, targetRate            float64
		hold, warmup, budget            time.Duration
		maxVUs, breach, refine, instTst int
		jsonOut, quiet, yes             bool
		allow, set                      stringList
	)
	fs.StringVar(&workspace, "workspace", defaultWorkspace(), "workspace directory (results go to <workspace>/capacity/<id>/)")
	fs.Float64Var(&startRate, "start-rate", capacity.DefaultStartRate, "first step rate (scenario iterations/s)")
	fs.Float64Var(&stepPct, "step-pct", capacity.DefaultStepPct, "multiplicative step increase in percent")
	fs.Float64Var(&stepAdd, "step-add", 0, "additive step increase in iterations/s (overrides --step-pct)")
	fs.DurationVar(&hold, "hold", capacity.DefaultHold, "measured time per step")
	fs.DurationVar(&warmup, "warmup", capacity.DefaultWarmup, "discarded warm-up per step")
	fs.Float64Var(&maxRate, "max-rate", 0, "hard upper rate in iterations/s (required for non-loopback targets; default 500 for loopback)")
	fs.DurationVar(&budget, "budget", capacity.DefaultBudget, "hard time limit for the whole search (never above the policy maxDuration)")
	fs.IntVar(&maxVUs, "max-vus", 0, "VU pool per step (default: policy maxVUs)")
	fs.IntVar(&breach, "breach-steps", capacity.DefaultBreachSteps, "consecutive failing steps that end the staircase")
	fs.IntVar(&refine, "refine", capacity.DefaultRefineSteps, "bisection steps between the last passing and first failing rate (0 = none)")
	fs.Float64Var(&p95, "p95", 0, "criterion: overall p95 latency at most this many ms (adds to / replaces the scenario's p95 SLA)")
	fs.Float64Var(&maxErr, "max-error-pct", -1, "criterion: overall error rate at most this percent")
	fs.Float64Var(&headroom, "headroom", capacity.DefaultHeadroomPct, "estimate: percent of each instance's capacity kept free")
	fs.Float64Var(&targetRate, "target-rate", 0, "estimate: size instances for this rate (iterations/s)")
	fs.IntVar(&instTst, "instances-tested", 1, "estimate: number of target instances behind the tested endpoint")
	fs.BoolVar(&jsonOut, "json", false, "print the result as JSON")
	fs.BoolVar(&quiet, "quiet", false, "no per-step progress")
	fs.BoolVar(&yes, "yes", false, "confirm ramping load against non-loopback hosts")
	fs.Var(&allow, "allow-host", "allow a non-loopback target host (repeatable; only hosts you are authorized to test)")
	fs.Var(&set, "set", "set a scenario variable, e.g. --set baseUrl=http://127.0.0.1:8099 (repeatable)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter capacity scenario.json [flags]")
	}
	path := fs.Arg(0)
	sc, err := app.LoadScenario(path)
	if err != nil {
		return err
	}
	for _, kv := range set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--set expects name=value, got %q", kv)
		}
		if sc.Variables == nil {
			sc.Variables = map[string]string{}
		}
		sc.Variables[k] = v
	}
	pol, err := safety.LoadPolicy()
	if err != nil {
		return err
	}
	pol.AllowedHosts = append(pol.AllowedHosts, allow...)
	if len(allow) > 0 {
		safety.Audit("policy.allow_host_cli", map[string]any{"hosts": []string(allow)})
	}
	if warmup == 0 {
		warmup = -1 // explicit "no warm-up" (Plan treats 0 as default)
	}
	plan := capacity.Plan{StartRate: startRate, StepPct: stepPct, StepAdd: stepAdd, Hold: model.Duration(hold), Warmup: model.Duration(warmup),
		MaxRate: maxRate, Budget: model.Duration(budget), MaxVUs: maxVUs, BreachSteps: breach, RefineSteps: refine}
	if stepAdd > 0 {
		plan.StepPct = 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		n := 0
		for range sig {
			n++
			if n == 1 {
				fmt.Fprintln(os.Stderr, "\nstopping the capacity search (current step stops gracefully; Ctrl+C again to exit at once)...")
				cancel()
				continue
			}
			os.Exit(130)
		}
	}()
	progress := !quiet && !jsonOut
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	res, err := capacity.Find(ctx, capacity.Options{Scenario: sc, BaseDir: filepath.Dir(path), Policy: pol, Confirmed: yes, Plan: plan,
		P95Ms: p95, MaxErrorPct: maxErr, TargetRate: targetRate, HeadroomPct: headroom, InstancesTested: instTst, Logger: logger,
		OnStart: func(p capacity.Plan, crit []model.SLA) {
			for _, h := range app.TargetHosts(sc) {
				if !safety.IsLoopback(h) {
					fmt.Fprintln(os.Stderr, "WARNING: this search ramps load against a non-local system. Only test systems you own or are authorized to test.")
					break
				}
			}
			if progress {
				inc := fmt.Sprintf("x%.2f", 1+p.StepPct/100)
				if p.StepAdd > 0 {
					inc = fmt.Sprintf("+%g", p.StepAdd)
				}
				fmt.Fprintf(os.Stderr, "capacity search: start %g it/s, step %s, warm-up %s + hold %s per step, max %.2f it/s (%s), %d VUs max, budget %s\n",
					p.StartRate, inc, p.WarmupD(), p.Hold.D(), p.MaxRate, p.RateCap, p.MaxVUs, p.Budget.D())
				fmt.Fprintln(os.Stderr, capacity.StepHeader())
			}
		},
		OnStep: func(s capacity.Step) {
			if progress {
				fmt.Fprintln(os.Stderr, capacity.StepRow(s))
			}
		}})
	if err != nil {
		var nc *app.ErrNeedsConfirmation
		if errors.As(err, &nc) {
			return fmt.Errorf("%v (pass --yes)", err)
		}
		return err
	}
	dir := capacity.Dir(workspace, res.ID)
	saveErr := capacity.Save(dir, res)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		capacity.WriteText(os.Stdout, res)
		if saveErr == nil {
			fmt.Printf("\nresult: %s\nreport: %s\n", filepath.Join(dir, "capacity.json"), filepath.Join(dir, "report.html"))
		}
	}
	if saveErr != nil {
		return fmt.Errorf("saving result: %w", saveErr)
	}
	if res.StopReason == capacity.StopError {
		return fmt.Errorf("capacity search failed: %s", res.StopDetail)
	}
	return nil
}
