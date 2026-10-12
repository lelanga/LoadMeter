package tools

import (
	"context"
	"time"

	"github.com/lelanga/loadmeter/internal/service"
)

func init() {
	Register(
		Tool{Name: "find_capacity", Confirm: true,
			Description: "Find the highest arrival rate (scenario iterations/s) the target sustains while meeting the scenario's SLAs: runs a staircase of open-model steps (warm-up discarded, percentiles from each step's own histogram), stops on SLA breach, error storm, generator saturation or the rate cap, then refines by bisection. Reports max sustainable rate, first breached criterion, latency knee and throughput plateau, confidence, limitations and an assumption-labelled instance estimate. GENERATES INCREASING LOAD: requires confirm=true after a human approved the plan. Obeys the safety policy (allowlist, maxVUs, maxRPS, maxDuration). Needs SLAs in the scenario or p95Ms/maxErrorPct.",
			InputSchema: obj(map[string]any{
				"file":        fileProp,
				"startRate":   num("First step rate in iterations/s (default 10)"),
				"stepPct":     num("Multiplicative step increase in percent (default 25)"),
				"stepAdd":     num("Additive step increase in iterations/s (instead of stepPct)"),
				"hold":        str("Measured time per step, e.g. 20s (default 20s)"),
				"warmup":      str("Discarded warm-up per step, e.g. 5s (default 5s)"),
				"maxRate":     num("Hard upper rate in iterations/s (required for non-loopback targets; default 500 for loopback)"),
				"budget":      str("Hard limit for the whole search, e.g. 15m (default 30m, never above the policy maxDuration)"),
				"p95Ms":       num("Pass criterion: overall p95 latency at most this many ms"),
				"maxErrorPct": num("Pass criterion: overall error rate at most this percent"),
				"targetRate":  num("Rate (iterations/s) to size instances for in the estimate"),
				"headroomPct": num("Headroom kept free per instance in the estimate (default 30)"),
				"wait":        boolean("Wait for the result (up to the budget, max 60 minutes); default false returns at once, poll capacity_status"),
				"confirm":     boolean("Must be true: a human approved generating increasing load"),
			}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				req := service.CapacityRequest{File: getStr(a, "file"), Confirm: true, StartRate: getNum(a, "startRate"), StepPct: getNum(a, "stepPct"),
					StepAdd: getNum(a, "stepAdd"), Hold: getStr(a, "hold"), Warmup: getStr(a, "warmup"), MaxRate: getNum(a, "maxRate"),
					Budget: getStr(a, "budget"), P95Ms: getNum(a, "p95Ms"), TargetRate: getNum(a, "targetRate"), HeadroomPct: getNum(a, "headroomPct")}
				if _, ok := a["maxErrorPct"]; ok {
					v := getNum(a, "maxErrorPct")
					req.MaxErrorPct = &v
				}
				info, err := s.StartCapacity(req)
				if err != nil {
					return nil, err
				}
				if !getBool(a, "wait") {
					return info, nil
				}
				wctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
				defer cancel()
				return s.WaitCapacity(wctx), nil
			}},
		Tool{Name: "capacity_status", ReadOnly: true,
			Description: "State of the current or last capacity search: plan, criteria, finished steps (rate, throughput, error %, p50/p95/p99, verdict) and, when done, the full result. With id, load a saved result instead.",
			InputSchema: obj(map[string]any{"id": str("Saved capacity result id (optional)")}),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				if id := getStr(a, "id"); id != "" {
					return s.CapacityResult(id)
				}
				info := s.ActiveCapacity()
				if info == nil {
					return map[string]any{"active": false}, nil
				}
				return info, nil
			}},
		Tool{Name: "stop_capacity", Description: "Stop the running capacity search; the partial result is saved.",
			InputSchema: obj(map[string]any{}),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return map[string]any{"stopping": true}, s.StopCapacity()
			}},
	)
}
