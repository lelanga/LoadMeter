package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/service"
)

// ProgressFunc reports progress to the MCP client (a no-op unless the client
// sent a progressToken with the request).
type ProgressFunc func(progress, total float64, message string)

type progressKey struct{}

// WithProgress attaches a progress reporter to ctx.
func WithProgress(ctx context.Context, f ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// ReportProgress sends progress if ctx carries a reporter.
func ReportProgress(ctx context.Context, progress, total float64, message string) {
	if f, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && f != nil {
		f(progress, total, message)
	}
}

// ProgressInterval is how often wait_for_run reports progress.
var ProgressInterval = 2 * time.Second

// MaxWait is the longest wait_for_run will block.
const MaxWait = 10 * time.Minute

func init() {
	Register(Tool{Name: "wait_for_run", ReadOnly: true,
		Description: "Block until a started test run ends (max 10 minutes), sending progress notifications, then return its summary. Returns status \"running\" if the timeout hits first.",
		InputSchema: obj(map[string]any{"runId": str("Run ID returned by run_test"), "timeoutSec": num("Maximum seconds to wait (default and max 600)")}, "runId"),
		Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
			id := getStr(a, "runId")
			if !app.ValidRunID(id) {
				return nil, fmt.Errorf("%w: invalid run id", service.ErrInvalid)
			}
			act := s.Active(id)
			if act == nil {
				return s.Summary(id) // finished earlier (or unknown)
			}
			timeout := MaxWait
			if t := getNum(a, "timeoutSec"); t > 0 && time.Duration(t*float64(time.Second)) < timeout {
				timeout = time.Duration(t * float64(time.Second))
			}
			planned := act.Scenario.Workload.PlannedDuration().Seconds()
			start := time.Now()
			tick := time.NewTicker(ProgressInterval)
			defer tick.Stop()
			deadline := time.NewTimer(timeout)
			defer deadline.Stop()
			report := func() {
				el := time.Since(start).Seconds()
				if !act.Run.StartedAt.IsZero() {
					el = time.Since(act.Run.StartedAt).Seconds()
				}
				ReportProgress(ctx, el, planned, fmt.Sprintf("run %s: %.0fs of %.0fs planned", id, el, planned))
			}
			report()
			for {
				select {
				case <-act.Run.Done():
					return s.Summary(id)
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-deadline.C:
					return map[string]any{"runId": id, "status": "running", "note": "timeout reached; call wait_for_run again"}, nil
				case <-tick.C:
					report()
				}
			}
		}})
}
