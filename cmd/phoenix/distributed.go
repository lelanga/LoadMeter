package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/distributed"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/report"
	"github.com/lelanga/phoenix/internal/results"
	"github.com/lelanga/phoenix/internal/safety"
)

func init() {
	register("agent", "Run a load agent for distributed tests: phoenix agent --token-file F [--listen 0.0.0.0:7979 --allow-host H]", cmdAgent)
}

// ---------- controller side: phoenix run --agents ... ----------

type distFlags struct {
	agents     stringList
	agentsFile string
	tokenFile  string
	localShare float64
}

func addDistFlags(fs *flag.FlagSet, d *distFlags) {
	fs.Var(&d.agents, "agents", "distributed: load agents as host:port#sha256-fingerprint (comma-separated or repeatable)")
	fs.StringVar(&d.agentsFile, "agents-file", "", `distributed: agents.json {"tokenFile": "...", "agents": [{"id","addr","fingerprint"}]}`)
	fs.StringVar(&d.tokenFile, "agent-token-file", "", "distributed: shared agent token file (default: tokenFile from --agents-file or $PHOENIX_AGENT_TOKEN_FILE)")
	fs.Float64Var(&d.localShare, "local-share", 0, "distributed: fraction of the load to also run on this machine, 0 to 0.9 (default 0: agents only)")
}

func (d distFlags) enabled() bool { return len(d.agents) > 0 || d.agentsFile != "" }

func runDistributed(sc *model.Scenario, baseDir string, f *runFlags, pol safety.Policy) error {
	var specs []distributed.AgentSpec
	tokenFile := f.dist.tokenFile
	if f.dist.agentsFile != "" {
		af, err := distributed.LoadAgentsFile(f.dist.agentsFile)
		if err != nil {
			return err
		}
		specs = append(specs, af.Agents...)
		if tokenFile == "" && af.TokenFile != "" {
			tokenFile = af.TokenFile
			if !filepath.IsAbs(tokenFile) {
				tokenFile = filepath.Join(filepath.Dir(f.dist.agentsFile), tokenFile)
			}
		}
	}
	for _, a := range f.dist.agents {
		if a == "" {
			continue
		}
		sp, err := distributed.ParseAgentSpec(a)
		if err != nil {
			return err
		}
		specs = append(specs, sp)
	}
	if tokenFile == "" {
		tokenFile = os.Getenv("PHOENIX_AGENT_TOKEN_FILE")
	}
	if tokenFile == "" {
		return fmt.Errorf("distributed runs need the agents' shared token: pass --agent-token-file (a copy of the agent's --token-file)")
	}
	token, err := distributed.ReadToken(tokenFile)
	if err != nil {
		return err
	}
	if f.dist.localShare > 0.9 {
		return fmt.Errorf("--local-share must be between 0 and 0.9")
	}

	ws := app.Workspace{Dir: f.workspace}
	if err := ws.Init(); err != nil {
		return err
	}
	id := results.NewRunID(sc.Name)
	dir := filepath.Join(ws.RunsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var raw *os.File
	if f.dist.localShare > 0 {
		if raw, err = os.Create(filepath.Join(dir, "raw.csv")); err != nil {
			return err
		}
		defer raw.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	opts := distributed.Options{RunID: id, Agents: specs, Token: token, LocalShare: f.dist.localShare, BaseDir: baseDir,
		LocalPolicy: pol, Confirmed: f.yes, Logger: logger,
		Warn: func(s string) { fmt.Fprintln(os.Stderr, "warning:", s) }}
	if raw != nil {
		opts.Raw = raw
	}
	full := sc.Clone()
	ctrl, err := distributed.Start(ctx, sc, opts)
	if err != nil {
		if raw != nil {
			raw.Close()
		}
		os.RemoveAll(dir)
		return err
	}
	_ = app.SaveScenario(filepath.Join(dir, "scenario.json"), full)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		for range sig {
			fmt.Fprintln(os.Stderr, "\nstopping all nodes gracefully (Ctrl+C again to abort in-flight requests)...")
			ctrl.Stop()
		}
	}()
	if !f.quiet && !f.jsonOut {
		fmt.Fprintf(os.Stderr, "distributed run %s: %s, planned %s, up to %d VUs in total\n", id, sc.Workload.Executor, sc.Workload.PlannedDuration(), sc.Workload.MaxPlannedVUs())
		for _, s := range ctrl.Shares() {
			fmt.Fprintln(os.Stderr, "  node", s)
		}
		fmt.Fprintf(os.Stderr, "  synchronized start at %s\n", ctrl.StartedAt.Format("15:04:05.000"))
		t := time.NewTicker(2 * time.Second)
	loop:
		for {
			select {
			case <-ctrl.Done():
				break loop
			case now := <-t.C:
				s := ctrl.Col.Snapshot(now, 1<<30)
				fmt.Fprintf(os.Stderr, "  t=%5.0fs vus=%-4d reqs=%-7d rps=%-8.1f p95=%7.1fms err=%.2f%%\n",
					s.ElapsedSec, s.ActiveVUs, s.Requests.Count, s.Requests.RPS, s.Requests.P95Ms, s.Requests.ErrorRate)
			}
		}
		t.Stop()
	} else {
		<-ctrl.Done()
	}
	if raw != nil {
		raw.Close()
	}

	s := results.Build(id, full, ctrl.Col, ctrl.StartedAt, ctrl.EndedAt, ctrl.State())
	s.Agents = ctrl.AgentStats()
	s.Findings = append(s.Findings, ctrl.Findings()...)
	s.Notes = append(s.Notes, fmt.Sprintf("Distributed run on %d node(s); metrics are merged from every node's full histograms (not averaged).", len(s.Agents)))
	s.Notes = append(s.Notes, ctrl.Notes()...)
	if f.dist.localShare == 0 {
		s.Notes = append(s.Notes, "raw.csv is not collected from agents; per-sample data is only recorded for a local share (--local-share).")
	}
	if f.baseline != "" {
		if b, err := results.Load(filepath.Join(ws.RunsDir(), f.baseline)); err == nil {
			s.Baseline = results.Compare(b, s)
		} else {
			s.Notes = append(s.Notes, "Baseline "+f.baseline+" could not be loaded: "+err.Error())
		}
	}
	if err := results.Save(dir, s); err != nil {
		return err
	}
	fh, err := os.Create(filepath.Join(dir, "report.html"))
	if err != nil {
		return err
	}
	err = report.Render(fh, s, full.Description)
	fh.Close()
	if err != nil {
		return err
	}
	if f.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	fmt.Printf("\n%-24s %-22s %-13s %5s %9s %7s %9s %8s\n", "node", "address", "status", "vus", "requests", "errors", "p95ms", "skewms")
	for _, a := range s.Agents {
		addr := a.Address
		if addr == "" {
			addr = "(this machine)"
		}
		fmt.Printf("%-24s %-22s %-13s %5d %9d %7d %9.1f %8.1f\n", trunc(a.ID, 24), trunc(addr, 22), a.Status, a.VUs, a.Requests, a.Errors, a.P95Ms, a.ClockSkewMs)
	}
	printSummary(s, dir)
	if len(s.SLAs) > 0 && !s.SLAPassed {
		return exitCode(3)
	}
	return nil
}

// ---------- agent side: phoenix agent ----------

func cmdAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	listen := fs.String("listen", fmt.Sprintf("0.0.0.0:%d", distributed.DefaultPort), "listen address (agents are the only Phoenix component meant to listen on non-loopback)")
	tokenFile := fs.String("token-file", "", "shared token file (required; generated with 32 random bytes, mode 0600, if it does not exist)")
	id := fs.String("id", "", "agent name shown in reports (default: hostname)")
	certDir := fs.String("cert-dir", filepath.Join(safety.ConfigDir(), "agent"), "directory for the agent's self-signed TLS certificate")
	printFP := fs.Bool("fingerprint", false, "print the certificate fingerprint and exit")
	var allow stringList
	fs.Var(&allow, "allow-host", "allow a non-loopback target host on THIS agent (repeatable; the controller cannot widen this)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	cert, fp, err := distributed.LoadOrCreateCert(*certDir)
	if err != nil {
		return err
	}
	if *printFP {
		fmt.Println(fp)
		return nil
	}
	if *tokenFile == "" {
		return fmt.Errorf("refusing to start without a token: pass --token-file FILE (it is created with a random token if missing)")
	}
	token, created, err := distributed.LoadOrCreateToken(*tokenFile)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && distributed.TokenFileTooOpen(*tokenFile) {
		return fmt.Errorf("token file %s is readable by other users; run: chmod 600 %s", *tokenFile, *tokenFile)
	}
	pol, err := safety.LoadPolicy()
	if err != nil {
		return err
	}
	pol.AllowedHosts = append(pol.AllowedHosts, allow...)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ag, err := distributed.NewAgent(distributed.AgentConfig{ID: *id, Token: token, Policy: pol, Logger: logger})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv, errc := ag.Serve(ln, cert)
	defer srv.Close()
	safety.Audit("agent.listen", map[string]any{"addr": ln.Addr().String(), "fingerprint": fp, "allowedHosts": pol.AllowedHosts, "maxVUs": pol.MaxVUs})
	if created {
		fmt.Fprintf(os.Stderr, "generated a new shared token in %s (mode 0600); copy this file securely to the controller\n", *tokenFile)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "<this-host>"
	}
	fmt.Printf("phoenix agent listening on https://%s (TLS, token required)\n", ln.Addr())
	fmt.Printf("certificate fingerprint (sha256): %s\n", fp)
	fmt.Printf("agent policy: maxVUs %d, maxDuration %s, allowlisted hosts %v (loopback always allowed)\n", pol.MaxVUs, pol.MaxDur(), pol.AllowedHosts)
	fmt.Printf("controller: phoenix run scenario.json --agents %s#%s --agent-token-file <copy of %s>\n", net.JoinHostPort(host, port), fp, *tokenFile)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		fmt.Fprintln(os.Stderr, "agent shutting down")
		return nil
	case err := <-errc:
		return err
	}
}
