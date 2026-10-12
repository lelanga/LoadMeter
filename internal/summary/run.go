package summary

import (
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/service"
	"github.com/lelanga/loadmeter/internal/triage"
)

// ForRun loads a finished run from the workspace and builds its facts. When
// baseline is empty, the comparison stored in the run's summary (if any) is
// used. It returns the triage result too so callers need not recompute it.
func ForRun(svc *service.Service, runID, baseline string) (Facts, *triage.Result, error) {
	sum, err := svc.Summary(runID)
	if err != nil {
		return Facts{}, nil, err
	}
	dir, err := svc.RunDir(runID)
	if err != nil {
		return Facts{}, nil, err
	}
	tri, err := triage.AnalyzeRun(dir, sum)
	if err != nil {
		return Facts{}, nil, err
	}
	var cmp *results.Comparison
	if baseline != "" {
		if cmp, err = svc.Compare(baseline, runID); err != nil {
			return Facts{}, nil, err
		}
	} else {
		cmp = sum.Baseline
	}
	return BuildFacts(sum, tri, cmp), tri, nil
}
