// Package assistant turns natural-language requests into a structured,
// validated plan of tool calls. The model never executes anything itself:
// every step is a typed tool call checked by the same registry as MCP, and
// load-generating or destructive steps wait for explicit confirmation.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/redact"
	"github.com/lelanga/loadmeter/internal/service"
	"github.com/lelanga/loadmeter/internal/tools"
)

type Step struct {
	Tool              string         `json:"tool"`
	Args              map[string]any `json:"args"`
	Why               string         `json:"why"`
	NeedsConfirmation bool           `json:"needsConfirmation"`
}

type Executed struct {
	Tool   string `json:"tool"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Result struct {
	Plan              struct{ Steps []Step } `json:"plan"`
	Executed          []Executed             `json:"executed"`
	NeedsConfirmation bool                   `json:"needsConfirmation"`
	Message           string                 `json:"message"`
	Planner           string                 `json:"planner"` // ai | rules
}

func catalog() string {
	var b strings.Builder
	for _, t := range tools.Registry() {
		s, _ := json.Marshal(t.InputSchema["properties"])
		fmt.Fprintf(&b, "- %s: %s Args: %s\n", t.Name, t.Description, s)
	}
	return b.String()
}

const systemPrompt = `You are the planning component of Load Meter, a load-testing tool. Convert the user's request into a JSON plan of tool calls.
Output exactly: {"message": "<one-sentence summary for the user>", "steps": [{"tool": "<name>", "args": {...}, "why": "<short reason>"}]}
Rules:
- Use only the tools listed. Use only argument names from each tool's schema. Never invent file names: use those from the scenario list or from an import step (later steps may use "$last" as the file argument to mean the file produced by the previous import).
- Never include "confirm": the application asks the human.
- Virtual users (vus) and arrival rate (rate) are different; use vus for "N users", rate for "N requests/iterations per second".
- If the request is unclear or unsafe, return an empty steps array and explain in message.
` + ai.GuardInstruction

// Plan asks the configured AI provider for a plan, falling back to rules.
func Plan(ctx context.Context, svc *service.Service, client *ai.Client, prompt, file string) ([]Step, string, string, error) {
	if client != nil && client.Cfg.Enabled {
		scen, _ := svc.ListScenarios()
		var names []string
		for _, s := range scen {
			names = append(names, s.File+" ("+s.Name+")")
		}
		runs, _ := svc.ListRuns()
		var rids []string
		for i, r := range runs {
			if i >= 5 {
				break
			}
			rids = append(rids, r.RunID)
		}
		user := fmt.Sprintf("Tools:\n%s\nScenarios: %s\nRecent runs (newest first): %s\nCurrently open scenario: %s\n\nUser request:\n%s",
			catalog(), strings.Join(names, ", "), strings.Join(rids, ", "), file, ai.Untrusted("user-request", prompt))
		resp, err := client.Complete(ctx, ai.Request{Task: "plan", System: systemPrompt, JSON: true, MaxTokens: 1500,
			Messages: []ai.Message{{Role: "user", Content: user}}})
		if err == nil {
			raw, jerr := ai.ExtractJSON(resp.Text)
			if jerr == nil {
				var p struct {
					Message string `json:"message"`
					Steps   []Step `json:"steps"`
				}
				if json.Unmarshal(raw, &p) == nil {
					return p.Steps, p.Message, "ai", nil
				}
			}
			err = fmt.Errorf("the AI plan could not be parsed: %v", jerr)
		}
		steps, msg := rulePlan(prompt, file, svc)
		if len(steps) > 0 {
			return steps, msg + " (AI unavailable: " + err.Error() + "; used the built-in rules)", "rules", nil
		}
		return nil, "", "", err
	}
	steps, msg := rulePlan(prompt, file, svc)
	return steps, msg, "rules", nil
}

var (
	reUsers = regexp.MustCompile(`(?i)(\d+)\s*-?\s*(?:virtual\s+)?(?:users?|vus?)\b`)
	reDur   = regexp.MustCompile(`(?i)(\d+)\s*-?\s*(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h)\b`)
	reRate  = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(?:(?:requests?|iterations?)\s*(?:/|per)\s*(?:s|sec|second)|rps\b|tps\b)`)
	rePath  = regexp.MustCompile(`([^\s"']+\.(?:har|jmx))`)
)

func durStr(n, unit string) string {
	u := strings.ToLower(unit)
	switch {
	case strings.HasPrefix(u, "h"):
		return n + "h"
	case strings.HasPrefix(u, "m"):
		return n + "m"
	}
	return n + "s"
}

// rulePlan handles common requests deterministically (works with AI disabled).
func rulePlan(prompt, file string, svc *service.Service) ([]Step, string) {
	p := strings.ToLower(prompt)
	var steps []Step
	add := func(tool string, args map[string]any, why string) {
		steps = append(steps, Step{Tool: tool, Args: args, Why: why})
	}
	if m := rePath.FindStringSubmatch(prompt); m != nil && strings.Contains(p, "import") || m != nil && strings.Contains(p, "analy") {
		if strings.HasSuffix(strings.ToLower(m[1]), ".har") {
			add("import_har", map[string]any{"path": m[1]}, "Import the HAR capture")
		} else {
			add("import_jmeter", map[string]any{"path": m[1]}, "Import the JMeter plan and report unsupported elements")
		}
		file = "$last"
	}
	if file == "" {
		if sc, _ := svc.ListScenarios(); len(sc) > 0 {
			file = sc[0].File
		}
	}
	if strings.Contains(p, "correlat") || strings.Contains(p, "dynamic") || strings.Contains(p, "token") {
		add("suggest_correlations", map[string]any{"file": file}, "Find dynamic values that need correlation")
	}
	if strings.Contains(p, "parameter") || strings.Contains(p, "test data") {
		add("suggest_parameters", map[string]any{"file": file}, "Find inputs that should come from test data")
	}
	wl := map[string]any{"file": file}
	if m := reRate.FindStringSubmatch(prompt); m != nil {
		r, _ := strconv.ParseFloat(m[1], 64)
		wl["rate"] = r
	} else if m := reUsers.FindStringSubmatch(prompt); m != nil {
		n, _ := strconv.Atoi(m[1])
		wl["vus"] = float64(n)
	}
	if m := reDur.FindStringSubmatch(reUsers.ReplaceAllString(prompt, "")); m != nil && (wl["vus"] != nil || wl["rate"] != nil || strings.Contains(p, "duration")) {
		wl["duration"] = durStr(m[1], m[2])
		if (strings.Contains(p, "ramp") || strings.Contains(p, "gradual")) && wl["vus"] != nil {
			if d, err := time.ParseDuration(wl["duration"].(string)); err == nil {
				wl["rampUp"] = (d / 5).String()
			}
		}
	}
	if len(wl) > 1 {
		add("set_workload", wl, "Configure the requested workload")
	}
	if strings.Contains(p, "validate") || strings.Contains(p, "check") {
		add("validate_scenario", map[string]any{"file": file}, "Validate the scenario and its targets")
	}
	if strings.Contains(p, "debug") || strings.Contains(p, "dry run") || strings.Contains(p, "one iteration") {
		add("debug_scenario", map[string]any{"file": file}, "Run one iteration to inspect requests")
	}
	if regexp.MustCompile(`\b(run|start|execute|launch)\b`).MatchString(p) && !strings.Contains(p, "dry run") {
		add("run_test", map[string]any{"file": file}, "Start the load test")
	}
	if strings.Contains(p, "compare") || strings.Contains(p, "baseline") || strings.Contains(p, "previous run") {
		if runs, _ := svc.ListRuns(); len(runs) >= 2 {
			add("compare_runs", map[string]any{"baseline": runs[1].RunID, "runId": runs[0].RunID}, "Compare the latest run with the previous one")
		}
	}
	if strings.Contains(p, "convert") && strings.Contains(p, "jmeter") || strings.Contains(p, "export") {
		add("export_jmeter", map[string]any{"file": file}, "Export to JMeter .jmx")
	}
	if strings.Contains(p, "report") {
		if runs, _ := svc.ListRuns(); len(runs) > 0 {
			add("get_report_path", map[string]any{"runId": runs[0].RunID}, "Locate the latest HTML report")
		}
	}
	if len(steps) == 0 && (strings.Contains(p, "list") || strings.Contains(p, "scenarios")) {
		add("list_scenarios", map[string]any{}, "List scenarios")
	}
	msg := fmt.Sprintf("Planned %d step(s) with the built-in rules.", len(steps))
	if len(steps) == 0 {
		msg = "I could not map that request to actions without AI. Try e.g. \"create a 100-user 10-minute test with ramp-up\", \"find dynamic tokens\", \"validate\", \"run\", \"compare with baseline\", or enable an AI provider in Settings."
	}
	return steps, msg
}

// Ask plans and executes. Steps needing confirmation stop execution unless confirm is set.
func Ask(ctx context.Context, svc *service.Service, client *ai.Client, prompt, file string, confirm bool) (*Result, error) {
	steps, msg, planner, err := Plan(ctx, svc, client, prompt, file)
	if err != nil {
		return nil, err
	}
	res := &Result{Message: msg, Planner: planner}
	// Validate the whole plan before executing anything.
	for i := range steps {
		t, ok := tools.Find(steps[i].Tool)
		if !ok {
			return nil, fmt.Errorf("plan step %d uses unknown tool %q; nothing was executed", i+1, steps[i].Tool)
		}
		if steps[i].Args == nil {
			steps[i].Args = map[string]any{}
		}
		delete(steps[i].Args, "confirm") // only the human can confirm
		steps[i].NeedsConfirmation = t.Confirm
	}
	res.Plan.Steps = steps
	last := file
	for _, st := range steps {
		if st.NeedsConfirmation && !confirm {
			res.NeedsConfirmation = true
			res.Message += " Waiting for your confirmation before: " + st.Tool + "."
			break
		}
		args := map[string]any{}
		for k, v := range st.Args {
			args[k] = v
		}
		if f, ok := args["file"].(string); ok && (f == "$last" || f == "") && last != "" {
			args["file"] = last
		}
		if st.NeedsConfirmation {
			args["confirm"] = true
		}
		out, err := tools.Call(ctx, svc, st.Tool, args, "assistant:"+planner)
		ex := Executed{Tool: st.Tool, OK: err == nil, Result: out}
		if err != nil {
			ex.Error = err.Error()
			res.Executed = append(res.Executed, ex)
			res.Message += " Stopped: " + st.Tool + " failed."
			break
		}
		if imp, ok := out.(*service.HARImport); ok {
			last = imp.File
		} else if imp, ok := out.(*service.BasicImport); ok && imp.File != "" {
			last = imp.File
		}
		res.Executed = append(res.Executed, ex)
	}
	return res, nil
}

// Explain asks the AI to explain a run's results; only aggregated, redacted
// metrics are sent (never raw request/response bodies).
func Explain(ctx context.Context, svc *service.Service, client *ai.Client, runID string) (*ai.Response, error) {
	sum, err := svc.Summary(runID)
	if err != nil {
		return nil, err
	}
	compact := map[string]any{"scenario": sum.Scenario, "workload": sum.Workload, "overall": sum.Snapshot.Requests,
		"transactions": sum.Snapshot.ByTx, "requests": sum.Snapshot.ByRequest, "slas": sum.SLAs, "findings": sum.Findings, "baseline": sum.Baseline}
	tl := sum.Snapshot.Timeline
	if len(tl) > 120 {
		step := len(tl) / 120
		var s []any
		for i := 0; i < len(tl); i += step + 1 {
			s = append(s, tl[i])
		}
		compact["timelineSample"] = s
	} else {
		compact["timeline"] = tl
	}
	b, _ := json.Marshal(compact)
	return client.Complete(ctx, ai.Request{Task: "root-cause", MaxTokens: 1200,
		System:   "You are a senior performance engineer. Explain these load test results for a performance team. Separate 'Observed facts' (cite exact numbers from the data) from 'Hypotheses' (clearly labelled, with what evidence would confirm them). Never claim a database, network or application bottleneck without evidence in the data. Finish with 3 concrete next steps. " + ai.GuardInstruction,
		Messages: []ai.Message{{Role: "user", Content: ai.Untrusted("run-summary", redact.Text(string(b)))}}})
}
