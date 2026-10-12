package tools

import (
	"context"
	"fmt"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/service"
	"github.com/lelanga/loadmeter/internal/summary"
)

func init() {
	Register(
		Tool{Name: "triage_run", ReadOnly: true,
			Description: "Cluster the failed requests of a finished run by similar message, error kind, status and request, and say which side is most likely at fault (script, target, generator or safety policy). Deterministic and read-only; examples are redacted. Each cluster has count, percent, first/last seen second, a priority score, a short explanation and a suggested next check.",
			InputSchema: obj(map[string]any{"runId": str("Run ID"), "top": num("Number of highest-priority clusters to return (default 10)")}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				id := getStr(a, "runId")
				if !app.ValidRunID(id) {
					return nil, fmt.Errorf("%w: invalid run id", service.ErrInvalid)
				}
				top := int(getNum(a, "top"))
				if top <= 0 {
					top = 10
				}
				_, tri, err := summary.ForRun(s, id, "")
				if err != nil {
					return nil, err
				}
				return tri.Top(top), nil
			}},
		Tool{Name: "summarize_run", ReadOnly: true,
			Description: "Plain-English summary (at most 12 lines) of a finished run for non-programmers, plus the facts document it is built from. Every number in the text comes from the facts. Deterministic template; does not call any AI provider.",
			InputSchema: obj(map[string]any{"runId": str("Run ID"), "baseline": str("Optional baseline run ID to compare against")}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				id, base := getStr(a, "runId"), getStr(a, "baseline")
				if !app.ValidRunID(id) || base != "" && !app.ValidRunID(base) {
					return nil, fmt.Errorf("%w: invalid run id", service.ErrInvalid)
				}
				facts, _, err := summary.ForRun(s, id, base)
				if err != nil {
					return nil, err
				}
				return summary.Summarize(ctx, nil, facts, false), nil
			}},
	)
}
