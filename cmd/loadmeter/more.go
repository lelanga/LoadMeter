package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/assistant"
	"github.com/lelanga/loadmeter/internal/correlate"
	"github.com/lelanga/loadmeter/internal/har"
	"github.com/lelanga/loadmeter/internal/mcp"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/server"
	"github.com/lelanga/loadmeter/internal/service"
)

func init() {
	register("serve", "Start the local web UI: loadmeter serve [--addr 127.0.0.1:7878] [--open]", cmdServe)
	register("mcp", "Run the MCP server on stdio for AI assistants (Claude, ChatGPT, Gemini CLI, Cursor, VS Code)", cmdMCP)
	register("import-har", "Import a HAR capture: loadmeter import-har capture.har [--name N --include-static --keep-credentials]", cmdImportHAR)
	register("import-jmx", "Import a JMeter plan with a compatibility report: loadmeter import-jmx plan.jmx", cmdImportJMX)
	register("import-lr", "Statically analyze/convert a LoadRunner VuGen web script folder: loadmeter import-lr ./MyScript", cmdImportLR)
	register("export-jmx", "Export a workspace scenario to JMeter .jmx: loadmeter export-jmx scenario.json [-o out.jmx]", cmdExportJMX)
	register("correlate", "Suggest/apply correlation rules for an imported scenario: loadmeter correlate scenario.json [--apply ids|all --policy validated]", cmdCorrelate)
	register("parameterize", "Suggest/apply test-data parameters: loadmeter parameterize scenario.json [--apply ids|all --rows 100]", cmdParameterize)
	register("record", "Record a script (HTTP/HTTPS) via a local proxy: loadmeter record [--browser --url URL] [--to-scenario NAME] | record ca [--export F|--delete]", cmdRecord)
	register("ask", "Natural-language control: loadmeter ask \"create a 100-user 10-minute test with ramp-up\" [--file f.json --confirm]", cmdAsk)
	register("ai", "AI provider settings: loadmeter ai status | enable <light-id> <strong-id> [--cloud-consent] | disable | set-key <id> | test <id>", cmdAI)
	register("explain", "Ask the configured AI to explain a run: loadmeter explain <run-id>", cmdExplain)
}

func newService(ws string, allow []string) (*service.Service, error) {
	s, err := service.New(app.Workspace{Dir: ws})
	if err != nil {
		return nil, err
	}
	s.ExtraAllowed = allow
	return s, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// defaultServeAddr is the UI address used when --addr is not given.
const defaultServeAddr = "127.0.0.1:7878"

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	addr := fs.String("addr", defaultServeAddr, "listen address (loopback only)")
	open := fs.Bool("open", false, "open the browser")
	var allow stringList
	fs.Var(&allow, "allow-host", "allowlist a host for this session (repeatable)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if !safety.IsLoopback(host) {
		return fmt.Errorf("the UI only listens on loopback addresses (got %s); use SSH port-forwarding for remote access", host)
	}
	svc, err := newService(*ws, allow)
	if err != nil {
		return err
	}
	defer svc.Close()
	srv := server.New(svc)
	ln, err := net.Listen("tcp", *addr)
	if err != nil && *addr == defaultServeAddr {
		// default port taken (often another Load Meter): use any free loopback port
		fmt.Printf("port %s is busy; using a free port instead\n", defaultServeAddr)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w (is another Load Meter running? try --addr 127.0.0.1:0)", *addr, err)
	}
	url := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), srv.Token)
	fmt.Printf("Load Meter UI: %s\nworkspace: %s\nPress Ctrl+C to stop.\n", url, *ws)
	if tf := os.Getenv("LOADMETER_TOKEN_FILE"); tf != "" {
		_ = os.WriteFile(tf, []byte(url), 0o600)
	}
	if *open {
		openBrowser(url)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}()
	if err := hs.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdMCP(args []string) error {
	if len(args) > 0 && (args[0] == "install" || args[0] == "uninstall") {
		return cmdMCPInstall(args)
	}
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	httpAddr := fs.String("http", "", "serve Streamable HTTP on this loopback address (e.g. 127.0.0.1:7880) instead of stdio")
	if err := fs.Parse(args); err != nil {
		return err
	}
	abs, _ := filepath.Abs(*ws)
	svc, err := newService(abs, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	if *httpAddr != "" {
		return runMCPHTTP(svc, *httpAddr)
	}
	fmt.Fprintf(os.Stderr, "loadmeter MCP server on stdio (workspace %s)\n", abs)
	return (&mcp.Server{Svc: svc}).Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr)
}

func printCompat(r *model.CompatReport) {
	if r == nil {
		return
	}
	c := r.Counts()
	fmt.Printf("compatibility: supported %d, partial %d, unsupported %d, ignored %d; confidence %s; validation %s\n",
		c[model.Supported], c[model.Partial], c[model.Unsupported], c[model.Ignored], r.Confidence, r.Validated)
	for _, it := range r.Items {
		if it.Status == model.Partial || it.Status == model.Unsupported {
			fmt.Printf("  [%s] %s %s %s — %s\n", it.Status, it.Element, it.Name, it.Location, it.Note)
		}
	}
	for _, w := range r.Warnings {
		fmt.Println("  warning:", w)
	}
	for _, m := range r.Manual {
		fmt.Println("  manual change:", m)
	}
}

func cmdImportHAR(args []string) error {
	fs := flag.NewFlagSet("import-har", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	name := fs.String("name", "", "scenario name")
	static := fs.Bool("include-static", false, "keep static resources")
	keep := fs.Bool("keep-credentials", false, "keep recorded credentials (only with explicit authorization)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter import-har capture.har")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		n = strings.TrimSuffix(filepath.Base(fs.Arg(0)), filepath.Ext(fs.Arg(0)))
	}
	if *keep {
		safety.Audit("import.keep_credentials", map[string]any{"source": filepath.Base(fs.Arg(0))})
	}
	res, err := svc.ImportHAR(n, b, har.Options{IncludeStatic: *static, KeepCredentials: *keep})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	fmt.Printf("scenario: %s\n", filepath.Join(svc.WS.ScenariosDir(), res.File))
	printCompat(res.Report)
	printCandidates(res.Correlations, res.Unexplained)
	if len(res.Parameters) > 0 {
		fmt.Println("parameter suggestions:")
		for _, p := range res.Parameters {
			fmt.Printf("  %s  %-14s %-10s mode=%s uses=%d\n", p.ID, p.Column, p.Kind, p.Mode, len(p.Uses))
		}
	}
	fmt.Printf("next: loadmeter correlate %s --apply all   then: loadmeter run %s\n", res.File, filepath.Join(svc.WS.ScenariosDir(), res.File))
	return nil
}

func printCandidates(cs []correlate.Candidate, un []correlate.Unexplained) {
	if len(cs) == 0 {
		fmt.Println("no correlation candidates found")
	} else {
		fmt.Println("correlation candidates:")
	}
	for _, c := range cs {
		fmt.Printf("  %s  %-28s conf=%.2f %-10s %s ← %s (%s) → %d use(s)\n", c.ID, "${"+c.Variable+"}", c.Confidence, c.Security, c.Masked, c.Source.Step, c.Source.Location, len(c.Destinations))
	}
	for _, u := range un {
		fmt.Printf("  dynamic value not from any response: %s %s %s — %s\n", u.Step, u.Field, u.Masked, u.Suggestion)
	}
}

func cmdImportJMX(args []string) error {
	fs := flag.NewFlagSet("import-jmx", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	name := fs.String("name", "", "scenario name")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter import-jmx plan.jmx")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		n = strings.TrimSuffix(filepath.Base(fs.Arg(0)), filepath.Ext(fs.Arg(0)))
	}
	res, err := svc.ImportJMX(n, b)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	fmt.Printf("scenario: %s\n", filepath.Join(svc.WS.ScenariosDir(), res.File))
	printCompat(res.Report)
	return nil
}

func cmdImportLR(args []string) error {
	fs := flag.NewFlagSet("import-lr", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	name := fs.String("name", "", "scenario name")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter import-lr <vugen-script-folder>")
	}
	dir := fs.Arg(0)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, e := range ents {
		if e.Type().IsRegular() {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return err
			}
			files[e.Name()] = b
		}
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		n = filepath.Base(filepath.Clean(dir))
	}
	res, err := svc.ImportLoadRunner(n, files)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	if res.File != "" {
		fmt.Printf("scenario: %s\n", filepath.Join(svc.WS.ScenariosDir(), res.File))
	} else {
		fmt.Println("no executable scenario was produced (see report)")
	}
	printCompat(res.Report)
	if b, err := json.Marshal(res.Analysis); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if v, ok := m["verdict"]; ok {
				fmt.Printf("portability verdict: %v\n", v)
			}
		}
	}
	return nil
}

func cmdExportJMX(args []string) error {
	fs := flag.NewFlagSet("export-jmx", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	out := fs.String("o", "", "output file (default <scenario>.jmx)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter export-jmx scenario.json")
	}
	file := filepath.Base(fs.Arg(0))
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	b, rep, err := svc.ExportJMX(file)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = strings.TrimSuffix(file, ".json") + ".jmx"
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	printCompat(rep)
	return nil
}

func selectIDs(arg string, all []string) []string {
	if arg == "all" {
		return all
	}
	var out []string
	for _, s := range strings.Split(arg, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func cmdCorrelate(args []string) error {
	fs := flag.NewFlagSet("correlate", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	apply := fs.String("apply", "", "candidate IDs to apply (comma-separated) or 'all'")
	policy := fs.String("policy", "validated", "approval policy: validated | manual | auto | suggest")
	asJSON := fs.Bool("json", false, "print JSON")
	var allow, set stringList
	fs.Var(&allow, "allow-host", "allowlist a host for validation replay")
	fs.Var(&set, "set", "persist a scenario variable before validating, e.g. --set baseUrl=http://127.0.0.1:8099 (repeatable)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter correlate scenario.json [--apply all]")
	}
	file := filepath.Base(fs.Arg(0))
	svc, err := newService(*ws, allow)
	if err != nil {
		return err
	}
	if len(set) > 0 {
		sc, err := svc.GetScenario(file)
		if err != nil {
			return err
		}
		for _, kv := range set {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--set expects name=value")
			}
			sc.Variables[k] = v
		}
		if _, err := svc.SaveScenario(file, sc, "variables"); err != nil {
			return err
		}
	}
	res, err := svc.Correlate(file)
	if err != nil {
		return err
	}
	if *apply == "" && *policy != "auto" {
		if *asJSON {
			return printJSON(res)
		}
		printCandidates(res.Candidates, res.Unexplained)
		fmt.Printf("apply with: loadmeter correlate %s --apply all   (policy %s replays the scenario to validate)\n", file, *policy)
		return nil
	}
	var ids []string
	for _, c := range res.Candidates {
		ids = append(ids, c.ID)
	}
	out, err := svc.ApplyCorrelations(context.Background(), file, selectIDs(*apply, ids), *policy)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	fmt.Printf("applied %d rule(s)%s\n", len(out.Applied), map[bool]string{true: " — ROLLED BACK", false: ""}[out.RolledBack])
	for _, n := range out.Notes {
		fmt.Println("  note:", n)
	}
	if v := out.Validation; v != nil {
		fmt.Println("validation:", v.Summary)
		for _, d := range v.Details {
			fmt.Printf("  %-30s ok=%-5v %s\n", "${"+d.Variable+"}", d.OK, d.Message)
		}
		if v.WithRules != nil {
			for _, st := range v.WithRules.Steps {
				if !st.OK {
					fmt.Printf("  failing step: %s (status %d) %s\n", st.Step, st.Status, st.Error)
				}
			}
		}
	}
	if out.RolledBack {
		return exitCode(4)
	}
	return nil
}

func cmdParameterize(args []string) error {
	fs := flag.NewFlagSet("parameterize", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	apply := fs.String("apply", "", "suggestion IDs or 'all'")
	rows := fs.Int("rows", 100, "synthetic rows to generate")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter parameterize scenario.json [--apply all]")
	}
	file := filepath.Base(fs.Arg(0))
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	sugg, err := svc.Parameterize(file)
	if err != nil {
		return err
	}
	if *apply == "" {
		for _, p := range sugg {
			fmt.Printf("%s  %-14s %-10s recorded=%-16s mode=%-10s %s\n", p.ID, p.Column, p.Kind, p.Recorded, p.Mode, p.Generator)
		}
		if len(sugg) == 0 {
			fmt.Println("no parameter suggestions")
		}
		return nil
	}
	var ids []string
	for _, p := range sugg {
		ids = append(ids, p.ID)
	}
	applied, data, err := svc.ApplyParameters(file, selectIDs(*apply, ids), *rows)
	if err != nil {
		return err
	}
	fmt.Printf("applied %d parameter(s); synthetic data: %s\n", len(applied), filepath.Join(svc.WS.ScenariosDir(), data))
	return nil
}

func cmdRecord(args []string) error { return runRecord(args) } // see record.go

func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	file := fs.String("file", "", "scenario file the request refers to")
	confirm := fs.Bool("confirm", false, "approve steps that generate load or delete data")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf(`usage: loadmeter ask "find dynamic tokens and suggest correlation rules"`)
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	cfg, err := ai.LoadConfig()
	if err != nil {
		return err
	}
	res, err := assistant.Ask(context.Background(), svc, ai.NewClient(cfg), prompt, *file, *confirm)
	if err != nil {
		return err
	}
	fmt.Printf("planner: %s\n%s\n\nplan:\n", res.Planner, res.Message)
	for i, s := range res.Plan.Steps {
		a, _ := json.Marshal(s.Args)
		c := ""
		if s.NeedsConfirmation {
			c = "  [needs --confirm]"
		}
		fmt.Printf("  %d. %s %s — %s%s\n", i+1, s.Tool, a, s.Why, c)
	}
	for _, e := range res.Executed {
		if e.OK {
			b, _ := json.MarshalIndent(e.Result, "     ", "  ")
			if len(b) > 1500 {
				b = append(b[:1500], []byte("\n     …")...)
			}
			fmt.Printf("\n✓ %s\n     %s\n", e.Tool, b)
		} else {
			fmt.Printf("\n✗ %s: %s\n", e.Tool, e.Error)
		}
	}
	return nil
}

func cmdAI(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(os.Stderr, "usage: loadmeter ai status | enable [--cloud-consent] <light> <strong> | disable | set-key <provider> | test <provider>")
		return nil
	}
	cfg, err := ai.LoadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		fmt.Printf("enabled=%v cloudConsent=%v localOnly=%v routing light=%s strong=%s keys=%s\n", cfg.Enabled, cfg.CloudConsent, cfg.LocalOnly, cfg.Routing.Light, cfg.Routing.Strong, ai.KeyBackend())
		for _, p := range cfg.Providers {
			fmt.Printf("  %-18s %-10s %-24s local=%-5v key=%v\n", p.ID, p.Kind, p.Model, p.IsLocal(), p.HasKey())
		}
	case "enable":
		fs := flag.NewFlagSet("ai enable", flag.ContinueOnError)
		consent := fs.Bool("cloud-consent", false, "allow sending redacted data to cloud providers")
		if err := fs.Parse(reorder(fs, args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: loadmeter ai enable <light-provider-id> <strong-provider-id> [--cloud-consent]")
		}
		for _, id := range fs.Args() {
			if _, ok := cfg.Provider(id); !ok {
				return fmt.Errorf("unknown provider %q (see loadmeter ai status)", id)
			}
		}
		cfg.Enabled, cfg.CloudConsent = true, *consent
		cfg.Routing.Light, cfg.Routing.Strong = fs.Arg(0), fs.Arg(1)
		if err := ai.SaveConfig(cfg); err != nil {
			return err
		}
		fmt.Println("AI enabled")
	case "disable":
		cfg.Enabled = false
		return ai.SaveConfig(cfg)
	case "set-key":
		if len(args) != 2 {
			return fmt.Errorf("usage: loadmeter ai set-key <provider-id>   (reads the key from stdin)")
		}
		fmt.Fprint(os.Stderr, "paste API key and press Enter: ")
		var key string
		fmt.Fscanln(os.Stdin, &key)
		if err := ai.SetKey(args[1], strings.TrimSpace(key)); err != nil {
			return err
		}
		fmt.Println("key stored in", ai.KeyBackend())
	case "test":
		if len(args) != 2 {
			return fmt.Errorf("usage: loadmeter ai test <provider-id>")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		r, err := ai.NewClient(cfg).Test(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("ok: %s replied %q in %s\n", r.Model, strings.TrimSpace(r.Text), r.Latency.Round(time.Millisecond))
	default:
		return fmt.Errorf("unknown ai subcommand %q", args[0])
	}
	return nil
}

func cmdExplain(args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter explain <run-id>")
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	cfg, err := ai.LoadConfig()
	if err != nil {
		return err
	}
	r, err := assistant.Explain(context.Background(), svc, ai.NewClient(cfg), fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("%s\n\n(%s/%s, %d bytes of aggregated, redacted metrics sent)\n", r.Text, r.Provider, r.Model, r.SentBytes)
	return nil
}
