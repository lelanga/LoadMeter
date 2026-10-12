package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/summary"
	"github.com/lelanga/loadmeter/internal/triage"
)

func init() {
	register("triage", "Cluster a run's failures and say which side is at fault: loadmeter triage <run-id> [--top 10 --json]", cmdTriage)
	register("summarize", "Plain-English run summary with verified numbers: loadmeter summarize <run-id> [--baseline <run-id> --ai --json]", cmdSummarize)
}

func cmdTriage(args []string) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	top := fs.Int("top", 10, "show the N highest-priority clusters (0 = all)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter triage <run-id> [--top 10 --json]")
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	_, tri, err := summary.ForRun(svc, fs.Arg(0), "")
	if err != nil {
		return err
	}
	res := tri.Top(*top)
	if *asJSON {
		return printJSON(res)
	}
	printTriage(res)
	return nil
}

func printTriage(r *triage.Result) {
	if r.TotalFailures == 0 {
		fmt.Printf("Run %s: no failed requests (%d requests).\n", r.RunID, r.TotalRequests)
		return
	}
	fmt.Printf("Run %s: %d of %d requests failed (%.2f%%), %d clusters", r.RunID, r.TotalFailures, r.TotalRequests,
		float64(r.TotalFailures)/float64(max(r.TotalRequests, 1))*100, len(r.Clusters)+r.ClustersHidden)
	if !r.Coarse {
		fmt.Printf(", %d message templates", r.Templates)
	}
	fmt.Println()
	if len(r.ChangePoints) > 0 {
		var cp []string
		for _, c := range r.ChangePoints {
			cp = append(cp, fmt.Sprintf("%.0fs", c))
		}
		fmt.Printf("Change point(s) in errors/latency: %s\n", strings.Join(cp, ", "))
	}
	if r.Note != "" {
		fmt.Println("Note:", r.Note)
	}
	fmt.Printf("Failures by side: %s\n", sideLine(r))
	for _, c := range r.Clusters {
		fmt.Printf("\n#%d  priority %d  %s (%s confidence)\n", c.Rank, c.Priority, strings.ToUpper(c.Side), c.Confidence)
		what := c.Request
		if c.Transaction != "" {
			what += " [in " + c.Transaction + "]"
		}
		if c.Status != 0 {
			what += fmt.Sprintf("  HTTP %d", c.Status)
		}
		fmt.Printf("    %s  (%s)\n", what, c.ErrKind)
		if r.Coarse {
			fmt.Printf("    %d failures (%.2f%%)\n", c.Count, c.Percent)
		} else {
			cp := ""
			if c.BeganAtChangePoint {
				cp = ", began at a change point"
			}
			crit := ""
			if c.Critical {
				crit = ", critical request"
			}
			fmt.Printf("    %d failures (%.2f%%), first seen %.1fs, last seen %.1fs%s%s\n", c.Count, c.Percent, c.FirstSeenSec, c.LastSeenSec, cp, crit)
			if c.Spark != nil {
				fmt.Printf("    over time: |%s|\n", triage.Sparkline(c.Spark))
			}
			fmt.Printf("    template: %s\n    example:  %s\n", c.Template, c.Example.Message)
		}
		fmt.Printf("    why:      %s\n    next:     %s\n", c.Explanation, c.NextCheck)
	}
	if r.ClustersHidden > 0 {
		fmt.Printf("\n%d lower-priority clusters not shown (use --top 0 for all).\n", r.ClustersHidden)
	}
	for _, s := range r.Signals {
		fmt.Printf("\n[%s] %s\n", s.Side, s.Text)
	}
}

func sideLine(r *triage.Result) string {
	var parts []string
	for _, s := range []string{triage.SideTarget, triage.SideScript, triage.SideGenerator, triage.SidePolicy, triage.SideUnknown} {
		if n := r.SideCounts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d (%.1f%%)", s, n, float64(n)/float64(r.TotalFailures)*100))
		}
	}
	return strings.Join(parts, ", ")
}

func cmdSummarize(args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	base := fs.String("baseline", "", "baseline run ID to compare against")
	useAI := fs.Bool("ai", false, "let the configured AI reword the summary (numbers are verified; needs AI enabled)")
	asJSON := fs.Bool("json", false, "print JSON (summary text plus the facts it is based on)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter summarize <run-id> [--baseline <run-id> --ai --json]")
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	facts, _, err := summary.ForRun(svc, fs.Arg(0), *base)
	if err != nil {
		return err
	}
	var client *ai.Client
	if *useAI {
		cfg, err := ai.LoadConfig()
		if err != nil {
			return err
		}
		client = ai.NewClient(cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := summary.Summarize(ctx, client, facts, *useAI)
	if *asJSON {
		return printJSON(res)
	}
	fmt.Println(res.Text)
	if res.Fallback != "" {
		fmt.Fprintln(os.Stderr, "\nnote:", res.Fallback)
	}
	if res.Source == summary.SourceAI {
		fmt.Fprintf(os.Stderr, "\n(worded by %s; every number was checked against the run's facts)\n", res.Model)
	}
	return nil
}
