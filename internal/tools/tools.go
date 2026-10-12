// Package tools exposes Phoenix operations as typed tools with JSON schemas.
// The same registry backs the MCP server (Claude, ChatGPT, Gemini CLI, Cursor,
// VS Code) and the built-in natural-language assistant, so AI callers go
// through exactly the same validation and safety checks as the UI.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/har"
	"github.com/lelanga/phoenix/internal/safety"
	"github.com/lelanga/phoenix/internal/service"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Confirm: the action generates load, deletes data, or sends data outside the
	// machine; callers must pass confirm=true after showing the plan to a human.
	Confirm  bool                                                                            `json:"-"`
	ReadOnly bool                                                                            `json:"-"`
	Handler  func(ctx context.Context, s *service.Service, args map[string]any) (any, error) `json:"-"`
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var fileProp = str("Scenario file name in the workspace, e.g. shop-login.json (see list_scenarios)")

// ErrNeedsConfirm is returned when a confirm-gated tool is called without confirm=true.
var ErrNeedsConfirm = errors.New("this action needs explicit human confirmation: show the plan to the user and call again with confirm=true")

func getStr(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func getNum(a map[string]any, k string) float64 {
	switch v := a[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func getBool(a map[string]any, k string) bool { v, _ := a[k].(bool); return v }

func getStrs(a map[string]any, k string) []string {
	var out []string
	if arr, ok := a[k].([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// importPath resolves a user-supplied import path. Only files inside the
// workspace "imports" folder or the current directory tree are readable, so an
// AI caller cannot read arbitrary files (e.g. ~/.ssh) through the tool.
func importPath(s *service.Service, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	roots := []string{filepath.Join(s.WS.Dir, "imports")}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	for _, r := range roots {
		ra, _ := filepath.Abs(r)
		if rel, err := filepath.Rel(ra, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %s is outside the allowed import folders (%s or the current directory)", p, roots[0])
}

// extra holds tools contributed by other files in this package (see Register).
var extra []Tool

// Register adds tools to the registry; call it from an init function.
func Register(t ...Tool) { extra = append(extra, t...) }

func Registry() []Tool {
	return append(coreTools(), extra...)
}

func coreTools() []Tool {
	return []Tool{
		{Name: "list_scenarios", ReadOnly: true, Description: "List test scenarios in the workspace with request counts and workload settings.",
			InputSchema: obj(map[string]any{}),
			Handler:     func(ctx context.Context, s *service.Service, a map[string]any) (any, error) { return s.ListScenarios() }},
		{Name: "get_scenario", ReadOnly: true, Description: "Return the full scenario JSON (requests, extractors, assertions, data sources, workload, SLAs).",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.GetScenario(getStr(a, "file"))
			}},
		{Name: "validate_scenario", ReadOnly: true, Description: "Validate a scenario and list its target hosts and whether the safety allowlist permits them.",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				sc, err := s.GetScenario(getStr(a, "file"))
				if err != nil {
					return nil, err
				}
				pol := s.Policy()
				var blocked []string
				hosts := app.TargetHosts(sc)
				for _, h := range hosts {
					if !pol.HostAllowed(h) {
						blocked = append(blocked, h)
					}
				}
				return map[string]any{"problems": sc.Validate(), "targets": hosts, "blocked": blocked}, nil
			}},
		{Name: "import_har", Description: "Import a HAR capture into a new scenario: filters static/analytics noise, groups transactions, replaces credentials with variables, and returns correlation and parameterization suggestions.",
			InputSchema: obj(map[string]any{"path": str("Path to the .har file (inside the workspace imports folder or current directory)"), "name": str("Scenario name"),
				"includeStatic": boolean("Keep static resources (default false)")}, "path"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				p, err := importPath(s, getStr(a, "path"))
				if err != nil {
					return nil, err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, err
				}
				name := getStr(a, "name")
				if name == "" {
					name = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
				}
				return s.ImportHAR(name, b, har.Options{IncludeStatic: getBool(a, "includeStatic")})
			}},
		{Name: "import_jmeter", Description: "Import an Apache JMeter .jmx test plan and return a compatibility report listing supported, partial and unsupported elements.",
			InputSchema: obj(map[string]any{"path": str("Path to the .jmx file"), "name": str("Scenario name")}, "path"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				p, err := importPath(s, getStr(a, "path"))
				if err != nil {
					return nil, err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, err
				}
				name := getStr(a, "name")
				if name == "" {
					name = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
				}
				return s.ImportJMX(name, b)
			}},
		{Name: "import_loadrunner", Description: "Statically analyze and convert a LoadRunner VuGen web (HTTP/HTML) script folder. Never executes C code; reports proprietary/unsupported dependencies with file:line.",
			InputSchema: obj(map[string]any{"path": str("Path to the VuGen script folder"), "name": str("Scenario name")}, "path"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				p, err := importPath(s, getStr(a, "path"))
				if err != nil {
					return nil, err
				}
				ents, err := os.ReadDir(p)
				if err != nil {
					return nil, err
				}
				files := map[string][]byte{}
				for _, e := range ents {
					if e.Type().IsRegular() {
						if b, err := os.ReadFile(filepath.Join(p, e.Name())); err == nil {
							files[e.Name()] = b
						}
					}
				}
				name := getStr(a, "name")
				if name == "" {
					name = filepath.Base(p)
				}
				return s.ImportLoadRunner(name, files)
			}},
		{Name: "export_jmeter", Description: "Export a scenario to a JMeter .jmx file in the workspace exports folder, with a report of anything that could not be preserved.",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				b, rep, err := s.ExportJMX(getStr(a, "file"))
				if err != nil {
					return nil, err
				}
				dir := filepath.Join(s.WS.Dir, "exports")
				os.MkdirAll(dir, 0o755)
				out := filepath.Join(dir, strings.TrimSuffix(getStr(a, "file"), ".json")+".jmx")
				if err := os.WriteFile(out, b, 0o644); err != nil {
					return nil, err
				}
				return map[string]any{"path": out, "report": rep}, nil
			}},
		{Name: "suggest_correlations", ReadOnly: true, Description: "Find dynamic values (session IDs, CSRF tokens, auth tokens, server-generated IDs) that flow from responses into later requests, with evidence and confidence. Values are masked.",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.Correlate(getStr(a, "file"))
			}},
		{Name: "apply_correlations", Description: "Apply selected correlation rules. Policy 'validated' (default) replays the scenario against the target and keeps the change only if it passes; 'auto' applies all candidates with confidence >= 0.7 and validates. Changes are reversible via history.",
			InputSchema: obj(map[string]any{"file": fileProp, "ids": strs("Candidate IDs from suggest_correlations"), "policy": map[string]any{"type": "string", "enum": []string{"validated", "manual", "auto", "suggest"}}}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.ApplyCorrelations(ctx, getStr(a, "file"), getStrs(a, "ids"), getStr(a, "policy"))
			}},
		{Name: "suggest_parameters", ReadOnly: true, Description: "Detect business inputs (usernames, search terms, product IDs, amounts, dates) that should come from synthetic test data.",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.Parameterize(getStr(a, "file"))
			}},
		{Name: "apply_parameters", Description: "Generate a synthetic CSV dataset for the selected parameter suggestions and rewrite the requests to use it.",
			InputSchema: obj(map[string]any{"file": fileProp, "ids": strs("Suggestion IDs"), "rows": num("Rows to generate (default 100)")}, "file", "ids"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				ids, data, err := s.ApplyParameters(getStr(a, "file"), getStrs(a, "ids"), int(getNum(a, "rows")))
				return map[string]any{"applied": ids, "dataFile": data}, err
			}},
		{Name: "set_workload", Description: "Configure the workload. Use vus (+ optional rampUp) for a closed model (fixed concurrency) or rate for an open model (iterations per second). They are not interchangeable.",
			InputSchema: obj(map[string]any{"file": fileProp, "vus": num("Virtual users (concurrency)"), "duration": str("e.g. 10m"), "rampUp": str("e.g. 2m"),
				"rate": num("Iterations per second (open model)"), "maxVUs": num("VU pool for rate-based tests")}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				sc, err := s.GetScenario(getStr(a, "file"))
				if err != nil {
					return nil, err
				}
				o := app.Overrides{VUs: int(getNum(a, "vus")), Duration: getStr(a, "duration"), RampUp: getStr(a, "rampUp"), Rate: getNum(a, "rate"), MaxVUs: int(getNum(a, "maxVUs"))}
				if o.Empty() {
					return nil, errors.New("set at least one of vus, rate or duration")
				}
				if err := app.ApplyOverrides(sc, o); err != nil {
					return nil, err
				}
				probs, err := s.SaveScenario(getStr(a, "file"), sc, "workload")
				return map[string]any{"workload": sc.Workload, "problems": probs}, err
			}},
		{Name: "debug_scenario", ReadOnly: true, Description: "Run ONE iteration with one virtual user and return each request/response (redacted), extracted variables and assertion results.",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.Debug(ctx, getStr(a, "file"), nil)
			}},
		{Name: "run_test", Confirm: true, Description: "Start a load test (generates traffic). Requires confirm=true after a human approved it. Non-allowlisted hosts are always refused.",
			InputSchema: obj(map[string]any{"file": fileProp, "vus": num("Override VUs"), "duration": str("Override duration"), "rampUp": str("Override ramp-up"),
				"rate": num("Override arrival rate"), "baseline": str("Run ID to compare with"), "wait": boolean("Wait for completion and return the summary (max 15 minutes)"),
				"confirm": boolean("Must be true: a human approved generating load")}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				id, err := s.StartRun(service.RunRequest{File: getStr(a, "file"), Baseline: getStr(a, "baseline"), Confirm: true,
					Overrides: app.Overrides{VUs: int(getNum(a, "vus")), Duration: getStr(a, "duration"), RampUp: getStr(a, "rampUp"), Rate: getNum(a, "rate")}})
				if err != nil {
					return nil, err
				}
				if !getBool(a, "wait") {
					return map[string]any{"runId": id, "status": "running"}, nil
				}
				act := s.Active(id)
				select {
				case <-act.Run.Done():
				case <-ctx.Done():
					return map[string]any{"runId": id, "status": "running"}, nil
				case <-time.After(15 * time.Minute):
					return map[string]any{"runId": id, "status": "running"}, nil
				}
				return s.Summary(id)
			}},
		{Name: "run_status", ReadOnly: true, Description: "Live metrics of a running test (or the active one when runId is empty).",
			InputSchema: obj(map[string]any{"runId": str("Run ID")}),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				id := getStr(a, "runId")
				if id == "" {
					id = s.ActiveRun().RunID
				}
				act := s.Active(id)
				if act == nil {
					return s.Summary(id)
				}
				snap := act.Run.Col.Snapshot(time.Now(), 1<<30)
				return map[string]any{"runId": id, "state": act.Run.State(), "metrics": snap}, nil
			}},
		{Name: "stop_test", Description: "Gracefully stop a running test (call twice to force).",
			InputSchema: obj(map[string]any{"runId": str("Run ID")}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return map[string]any{"stopping": true}, s.StopRun(getStr(a, "runId"))
			}},
		{Name: "list_runs", ReadOnly: true, Description: "List completed runs with p95, error rate and SLA status.",
			InputSchema: obj(map[string]any{}),
			Handler:     func(ctx context.Context, s *service.Service, a map[string]any) (any, error) { return s.ListRuns() }},
		{Name: "get_results", ReadOnly: true, Description: "Full summary of a run: percentiles, per-transaction stats, SLA results, timeline and evidence-based findings (observations vs hypotheses).",
			InputSchema: obj(map[string]any{"runId": str("Run ID")}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.Summary(getStr(a, "runId"))
			}},
		{Name: "compare_runs", ReadOnly: true, Description: "Compare a run with a baseline run and flag latency/error regressions.",
			InputSchema: obj(map[string]any{"baseline": str("Baseline run ID"), "runId": str("Run ID")}, "baseline", "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.Compare(getStr(a, "baseline"), getStr(a, "runId"))
			}},
		{Name: "get_report_path", ReadOnly: true, Description: "Return the path of a run's HTML report, CSV and JSON exports.",
			InputSchema: obj(map[string]any{"runId": str("Run ID")}, "runId"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				d, err := s.RunDir(getStr(a, "runId"))
				if err != nil {
					return nil, err
				}
				return map[string]string{"html": filepath.Join(d, "report.html"), "csv": filepath.Join(d, "summary.csv"), "json": filepath.Join(d, "summary.json"), "raw": filepath.Join(d, "raw.csv")}, nil
			}},
		{Name: "delete_scenario", Confirm: true, Description: "Delete a scenario (a copy is kept in history). Requires confirm=true.",
			InputSchema: obj(map[string]any{"file": fileProp, "confirm": boolean("Must be true")}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return map[string]bool{"deleted": true}, s.DeleteScenario(getStr(a, "file"))
			}},
		{Name: "create_demo", Description: "Start the bundled local mock shop server and create the demo-shop.json scenario that targets it.",
			InputSchema: obj(map[string]any{}),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				f, u, err := s.Demo()
				return map[string]string{"file": f, "mockUrl": u}, err
			}},
	}
}

// Find returns a tool by name.
func Find(name string) (Tool, bool) {
	for _, t := range Registry() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Call validates arguments against the schema basics and enforces confirmation.
func Call(ctx context.Context, s *service.Service, name string, args map[string]any, caller string) (any, error) {
	t, ok := Find(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := checkArgs(t, args); err != nil {
		return nil, err
	}
	if t.Confirm && !getBool(args, "confirm") {
		return nil, ErrNeedsConfirm
	}
	if !t.ReadOnly {
		safety.Audit("tool.call", map[string]any{"tool": name, "caller": caller})
	}
	return t.Handler(ctx, s, args)
}

func checkArgs(t Tool, args map[string]any) error {
	props, _ := t.InputSchema["properties"].(map[string]any)
	for k, v := range args {
		p, ok := props[k].(map[string]any)
		if !ok {
			return fmt.Errorf("%s: unknown argument %q", t.Name, k)
		}
		want, _ := p["type"].(string)
		okType := true
		switch want {
		case "string":
			_, okType = v.(string)
		case "number":
			switch v.(type) {
			case float64, int, json.Number:
			default:
				okType = false
			}
		case "boolean":
			_, okType = v.(bool)
		case "array":
			_, okType = v.([]any)
		}
		if !okType {
			return fmt.Errorf("%s: argument %q must be a %s", t.Name, k, want)
		}
		if enum, ok := p["enum"].([]string); ok {
			s, _ := v.(string)
			found := false
			for _, e := range enum {
				found = found || e == s
			}
			if !found {
				return fmt.Errorf("%s: argument %q must be one of %v", t.Name, k, enum)
			}
		}
	}
	if req, ok := t.InputSchema["required"].([]string); ok {
		for _, r := range req {
			if _, ok := args[r]; !ok {
				return fmt.Errorf("%s: missing required argument %q", t.Name, r)
			}
		}
	}
	return nil
}
