package tools

import (
	"context"
	"fmt"

	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/service"
)

func init() {
	Register(
		Tool{Name: "suggest_slas", ReadOnly: true,
			Description: "Propose SLA thresholds (p95, p99, error-rate ceiling, optionally per-transaction p95) from one or more finished baseline runs, with the variability observed and the reasoning for each value. Read-only: nothing is written to the scenario; show the proposal to the user, who can apply it with `loadmeter suggest-slas --write` or by editing the scenario.",
			InputSchema: obj(map[string]any{
				"runId":        str("Baseline run ID"),
				"runs":         strs("Additional baseline run IDs (same scenario and workload)"),
				"marginPct":    num("Headroom added to the observed upper bound, percent (default 20)"),
				"transactions": boolean("Also suggest per-transaction p95 SLAs (default false)"),
			}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				var runs []*results.Summary
				for _, id := range append([]string{getStr(a, "runId")}, getStrs(a, "runs")...) {
					sum, err := s.Summary(id)
					if err != nil {
						return nil, err
					}
					runs = append(runs, sum)
				}
				return results.SuggestSLAs(runs, results.SuggestOptions{MarginPct: getNum(a, "marginPct"), IncludeTx: getBool(a, "transactions")}), nil
			}},
		Tool{Name: "gate_run", ReadOnly: true,
			Description: "CI quality gate for a finished run: combines its SLA results with a statistical regression check against a baseline run (Mann-Whitney U on per-second window percentiles after warm-up, bootstrap confidence intervals, Cliff's delta, Holm correction, minimum practical effect). Returns status pass | fail | inconclusive, the matching exit code (0, 3, 4), every check with its reason, and a markdown summary for a pull-request comment. Report verdicts as computed; do not upgrade 'inconclusive' to pass or fail.",
			InputSchema: obj(map[string]any{
				"runId":        str("Run ID to gate"),
				"baseline":     str("Baseline run ID (omit to gate on SLAs only)"),
				"alpha":        num("Significance level (default 0.05)"),
				"minEffectPct": num("Smallest latency change in percent that counts (default 5)"),
				"minEffectMs":  num("Smallest latency change in ms that counts (default 2)"),
			}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				cur, err := s.Summary(getStr(a, "runId"))
				if err != nil {
					return nil, err
				}
				curDir, err := s.RunDir(getStr(a, "runId"))
				if err != nil {
					return nil, err
				}
				o := results.CompareOptions{Alpha: getNum(a, "alpha"), MinEffectPct: getNum(a, "minEffectPct"), MinEffectMs: getNum(a, "minEffectMs"), CurDir: curDir}
				var base *results.Summary
				if id := getStr(a, "baseline"); id != "" {
					if base, err = s.Summary(id); err != nil {
						return nil, fmt.Errorf("baseline: %w", err)
					}
					if o.BaseDir, err = s.RunDir(id); err != nil {
						return nil, err
					}
				}
				g := results.Gate(cur, base, o)
				return map[string]any{"gate": g, "markdown": g.Markdown()}, nil
			}},
	)
}
