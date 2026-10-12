package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lelanga/loadmeter/internal/accesslog"
)

func init() {
	register("import-log", "Build a replay scenario from an access log (nginx/Apache/ALB/CloudFront, .gz ok): loadmeter import-log access.log --base-url http://127.0.0.1:8099 [--format auto|nginx|apache|alb|cloudfront --scale 0.1 --window peak|avg|HH:MM-HH:MM --top 20 --include-writes --keep-query --include-static --include-errors --journey --session-cookie NAME --name N --allow-host H --json]", cmdImportLog)
}

func cmdImportLog(args []string) error {
	fs := flag.NewFlagSet("import-log", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	format := fs.String("format", "auto", "log format: auto, nginx, apache, alb, cloudfront")
	base := fs.String("base-url", "", "replay target (required; logs rarely contain the host)")
	scale := fs.Float64("scale", 0.1, "fraction of the logged request rate to replay (0.1 = 10%)")
	window := fs.String("window", "peak", "peak (busiest 15 minutes), avg, or HH:MM-HH:MM in the log's clock")
	top := fs.Int("top", 20, "keep the N most frequent endpoints in the mix")
	writes := fs.Bool("include-writes", false, "also replay POST/PUT/PATCH/DELETE (sent with an empty body)")
	query := fs.Bool("keep-query", false, "keep the most common query string per endpoint (secrets redacted)")
	static := fs.Bool("include-static", false, "replay static assets")
	errs := fs.Bool("include-errors", false, "replay requests that returned 4xx/5xx")
	journey := fs.Bool("journey", false, "one iteration = one reconstructed session, with fitted think time between requests")
	cookie := fs.String("session-cookie", "", "cookie name that identifies sessions (CloudFront cs(Cookie)); default client IP + user agent")
	name := fs.String("name", "", "scenario name")
	asJSON := fs.Bool("json", false, "print JSON")
	var allow stringList
	fs.Var(&allow, "allow-host", "authorize a non-loopback --base-url host (only for systems you are authorized to test)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter import-log access.log --base-url URL [flags]")
	}
	opts := accesslog.Options{Format: *format, BaseURL: *base, Scale: *scale, Window: *window, Top: *top,
		IncludeWrites: *writes, KeepQuery: *query, IncludeStatic: *static, IncludeErrors: *errs,
		Journey: *journey, SessionCookie: *cookie, Name: *name, AllowHosts: allow}
	// Fail on a bad target before reading a large file.
	if _, err := accesslog.CheckTarget(opts); err != nil {
		return err
	}
	var in io.Reader = os.Stdin
	src := "stdin"
	if fs.Arg(0) != "-" {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		in, src = f, filepath.Base(fs.Arg(0))
	}
	n := *name
	if n == "" {
		n = strings.TrimSuffix(strings.TrimSuffix(src, ".gz"), filepath.Ext(strings.TrimSuffix(src, ".gz")))
		if n == "" || n == "stdin" || n == "-" {
			n = "access-log-replay"
		}
	}
	svc, err := newService(*ws, allow)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "WARNING: "+accesslog.ProductionWarning)
	res, err := svc.ImportAccessLog(n, in, src, opts)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	printLogProfile(res.Profile, opts)
	fmt.Printf("\nscenario: %s\n", filepath.Join(svc.WS.ScenariosDir(), res.File))
	printCompat(res.Report)
	return nil
}

func pct(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(total))
}

func printLogProfile(p *accesslog.Profile, o accesslog.Options) {
	enc := ""
	if p.Gzip {
		enc = ", gzip"
	}
	fmt.Printf("access log: %s (format %s%s)\n", p.Source, p.Format, enc)
	fmt.Printf("  lines %d, parsed %d, malformed %d", p.Lines, p.Parsed, p.Malformed)
	if p.Malformed > 0 {
		fmt.Printf(" (first at line %v)", p.FirstBad)
	}
	fmt.Println()
	if p.Truncated {
		fmt.Printf("  input truncated at the %d MB cap\n", accesslog.MaxInputBytes>>20)
	}
	if p.ReadError != "" {
		fmt.Println("  read error:", p.ReadError)
	}
	fmt.Printf("  period %s .. %s (%d min)\n", p.First, p.Last, p.Rate.Minutes)

	fmt.Print("methods:")
	for _, k := range topKeys(p.Methods, 8) {
		fmt.Printf(" %s %s", k, pct(p.Methods[k], p.Parsed))
	}
	fmt.Println()
	cls := map[string]int{}
	for code, n := range p.Statuses {
		cls[code[:1]+"xx"] += n
	}
	fmt.Print("status: ")
	for _, k := range topKeys(cls, 6) {
		fmt.Printf(" %s %s", k, pct(cls[k], p.Parsed))
	}
	fmt.Println()

	fmt.Printf("endpoints: %d replayable (%d requests)\n", len(p.Endpoints), p.Replayable)
	shown := p.Endpoints
	if len(shown) > o.Top && o.Top > 0 {
		shown = shown[:o.Top]
	}
	var kept float64
	for _, e := range shown {
		kept += e.Share
		fmt.Printf("  %6.2f%% %8d  %s\n", e.Share, e.Count, e.Key())
	}
	if len(shown) < len(p.Endpoints) {
		fmt.Printf("  ... %d more endpoints (%.2f%%) not in the mix\n", len(p.Endpoints)-len(shown), 100-kept)
	}

	fmt.Printf("request rate (all requests): avg %.2f/s, peak %.2f/s at %s\n", p.Rate.AvgRPS, p.Rate.PeakRPS, p.Rate.PeakMinute)
	w := p.Window
	fmt.Printf("replay window: %s from %s for %d min; replayable rate avg %.2f/s peak %.2f/s -> x%g = avg %.3f/s peak %.3f/s\n",
		w.Spec, w.Start, w.Minutes, w.AvgRPS, w.PeakRPS, o.Scale, w.AvgRPS*o.Scale, w.PeakRPS*o.Scale)
	if len(p.Curve) > 0 {
		fmt.Printf("rate curve (per minute): %s\n", spark(p.Curve))
	}

	if len(p.Ignored) > 0 {
		fmt.Println("ignored:")
		for _, k := range topKeys(p.Ignored, 20) {
			fmt.Printf("  %-44s %d\n", k, p.Ignored[k])
		}
	}
	if p.QueryCount > 0 && !o.KeepQuery {
		fmt.Printf("  query strings dropped from %d requests (--keep-query keeps the most common per endpoint)\n", p.QueryCount)
	}
	s := p.Sessions
	fmt.Printf("sessions (approximate, %s, %d min idle): %d, %.1f requests each\n", s.Keyed, s.IdleMinutes, s.Sessions, s.MeanRequests)
	if f := s.ThinkFit; f != nil {
		fmt.Printf("think time: %d gaps, best fit %s %v (AIC %.1f), median %.1fs, p99.5 %.1fs", f.N, f.Dist, fmtParams(f.Params), f.AIC, f.MedianSec, f.P995Sec)
		if f.N < accesslog.MinThinkSamples {
			fmt.Printf(" [too few samples (<%d): scenario uses a uniform pause]", accesslog.MinThinkSamples)
		}
		fmt.Println()
		for _, c := range f.Candidates {
			fmt.Printf("    %-12s AIC %10.1f  %v\n", c.Dist, c.AIC, fmtParams(c.Params))
		}
		if !o.Journey {
			fmt.Println("  (not used: pass --journey to replay sessions with this think time)")
		}
	} else {
		fmt.Printf("think time: %s\n", s.ThinkFitReason)
	}
}

func fmtParams(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%.4g", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func topKeys(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// spark renders up to 60 minutes of the curve as block characters.
func spark(c []accesslog.RatePoint) string {
	const blocks = "▁▂▃▄▅▆▇█"
	rs := []rune(blocks)
	maxV := 0.0
	for _, p := range c {
		maxV = max(maxV, p.Requests)
	}
	var b strings.Builder
	for i, p := range c {
		if i == 60 {
			b.WriteString("…")
			break
		}
		idx := 0
		if maxV > 0 {
			idx = int(p.Requests / maxV * float64(len(rs)-1))
		}
		b.WriteRune(rs[idx])
	}
	return fmt.Sprintf("%s  (min %.0f, max %.0f req/min)", b.String(), minCurve(c), maxV)
}

func minCurve(c []accesslog.RatePoint) float64 {
	m := c[0].Requests
	for _, p := range c {
		m = min(m, p.Requests)
	}
	return m
}
