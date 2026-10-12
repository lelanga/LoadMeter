// Package model defines Load Meter's canonical scenario representation (IR).
// Every importer (HAR, JMX, LoadRunner) produces a Scenario and every exporter
// consumes one; the engine executes it. The JSON form is the on-disk format.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion = "loadmeter/v1"

// Duration is a time.Duration that marshals as a Go duration string ("30s").
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var ms float64
	if err := json.Unmarshal(b, &ms); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or milliseconds")
	}
	*d = Duration(time.Duration(ms * float64(time.Millisecond)))
	return nil
}

type Scenario struct {
	Version     string            `json:"version"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Defaults    Defaults          `json:"defaults,omitempty"`
	Variables   map[string]string `json:"variables,omitempty"`
	DataSources []DataSource      `json:"dataSources,omitempty"`
	Setup       []Step            `json:"setup,omitempty"`
	Steps       []Step            `json:"steps"`
	Teardown    []Step            `json:"teardown,omitempty"`
	Workload    Workload          `json:"workload"`
	SLAs        []SLA             `json:"slas,omitempty"`
	// Metadata records provenance (importer, source file, warnings); never executed.
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Defaults struct {
	BaseURL            string   `json:"baseUrl,omitempty"`
	Headers            []Header `json:"headers,omitempty"`
	Timeout            Duration `json:"timeout,omitempty"`
	FollowRedirects    *bool    `json:"followRedirects,omitempty"`
	InsecureSkipVerify bool     `json:"insecureSkipVerify,omitempty"`
	DisableCookies     bool     `json:"disableCookies,omitempty"`
	MaxBodyBytes       int64    `json:"maxBodyBytes,omitempty"` // response bytes kept for extraction/assertions
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DataSource is CSV-backed parameter data.
type DataSource struct {
	Name      string     `json:"name"`
	File      string     `json:"file,omitempty"` // relative to the scenario file
	Delimiter string     `json:"delimiter,omitempty"`
	Columns   []string   `json:"columns,omitempty"` // when empty, first row is the header
	Rows      [][]string `json:"rows,omitempty"`    // inline data (synthetic/test)
	// Mode: sequential (shared cursor), random, unique (each row used once), per-vu (row = vu index).
	Mode string `json:"mode,omitempty"`
	// OnExhaust: recycle (default), stop (VU stops), error (iteration fails).
	OnExhaust string `json:"onExhaust,omitempty"`
	// Scope: iteration (new row each iteration, default) or vu (row fixed per VU).
	Scope     string `json:"scope,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"` // mask values in logs/reports
}

const (
	StepRequest     = "request"
	StepTransaction = "transaction"
	StepThink       = "think"
	StepLoop        = "loop"
	StepIf          = "if"
	StepGroup       = "group"
	StepSetVar      = "set"
)

type Step struct {
	Type     string   `json:"type"`
	Name     string   `json:"name,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	Request  *Request `json:"request,omitempty"`
	Steps    []Step   `json:"steps,omitempty"` // transaction, loop, if, group
	Think    *Think   `json:"think,omitempty"`
	Count    int      `json:"count,omitempty"`     // loop
	Cond     string   `json:"condition,omitempty"` // if: `${a} == b`, `${a} != b`, `${a}` (non-empty)
	Set      []KV     `json:"set,omitempty"`       // set variables
	Comment  string   `json:"comment,omitempty"`
}

type Think struct {
	Min Duration `json:"min"`
	Max Duration `json:"max,omitempty"` // uniform random in [min,max] when max > min
}

type Request struct {
	Method          string      `json:"method"`
	URL             string      `json:"url"`
	Headers         []Header    `json:"headers,omitempty"`
	Body            string      `json:"body,omitempty"`
	Form            []KV        `json:"form,omitempty"` // application/x-www-form-urlencoded
	Multipart       []Part      `json:"multipart,omitempty"`
	Timeout         Duration    `json:"timeout,omitempty"`
	FollowRedirects *bool       `json:"followRedirects,omitempty"`
	Extract         []Extractor `json:"extract,omitempty"`
	Assert          []Assertion `json:"assert,omitempty"`
}

type Part struct {
	Name        string `json:"name"`
	Value       string `json:"value,omitempty"`
	File        string `json:"file,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

// Extractor types.
const (
	ExJSONPath = "jsonpath"
	ExRegex    = "regex"
	ExXPath    = "xpath"
	ExHeader   = "header"
	ExCookie   = "cookie"
	ExBoundary = "boundary"
	ExStatus   = "status"
)

type Extractor struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Expr    string `json:"expr,omitempty"`  // jsonpath, regex, xpath, header/cookie name
	Left    string `json:"left,omitempty"`  // boundary
	Right   string `json:"right,omitempty"` // boundary
	Group   int    `json:"group,omitempty"` // regex capture group (default 1, or 0 if none)
	Match   int    `json:"match,omitempty"` // 1-based match index; 0 = first; -1 = random
	Default string `json:"default,omitempty"`
	// Required marks the iteration failed when nothing is extracted.
	Required bool `json:"required,omitempty"`
}

// Assertion types.
const (
	AsStatus      = "status"       // Value: "200" or "2xx" or "200,201"
	AsContains    = "contains"     // body contains Value
	AsNotContains = "not_contains" // body does not contain Value
	AsRegex       = "regex"        // body matches Expr
	AsJSONPath    = "jsonpath"     // Expr exists; equals Value when set
	AsMaxLatency  = "max_latency"  // Value: duration
	AsHeader      = "header"       // header Expr equals/contains Value
)

type Assertion struct {
	Type  string `json:"type"`
	Expr  string `json:"expr,omitempty"`
	Value string `json:"value,omitempty"`
}

// Executors.
const (
	ExecConstantVUs     = "constant-vus"
	ExecRampingVUs      = "ramping-vus"
	ExecArrivalRate     = "constant-arrival-rate"
	ExecRampingRate     = "ramping-arrival-rate"
	ExecPerVUIterations = "per-vu-iterations"
)

type Workload struct {
	Executor   string   `json:"executor"`
	VUs        int      `json:"vus,omitempty"`
	Duration   Duration `json:"duration,omitempty"`
	Iterations int      `json:"iterations,omitempty"` // per VU, per-vu-iterations
	Stages     []Stage  `json:"stages,omitempty"`     // ramping-*: Target = VUs or iterations/s
	Rate       float64  `json:"rate,omitempty"`       // iterations per second (arrival-rate)
	MaxVUs     int      `json:"maxVUs,omitempty"`     // pool for arrival-rate executors
	// Pacing is the minimum time between iteration starts per VU (LoadRunner-style).
	Pacing       Duration `json:"pacing,omitempty"`
	GracefulStop Duration `json:"gracefulStop,omitempty"`
	// ThinkTimeScale multiplies recorded think times (0 = ignore, 1 = as recorded).
	ThinkTimeScale *float64 `json:"thinkTimeScale,omitempty"`
	// OnError: continue (default), abort-iteration, stop-vu.
	OnError string `json:"onError,omitempty"`
}

type Stage struct {
	Duration Duration `json:"duration"`
	Target   float64  `json:"target"`
}

// SLA is a pass/fail criterion evaluated after the run.
type SLA struct {
	Metric    string  `json:"metric"`          // p50,p90,p95,p99,avg,max (ms) | error_rate (%) | rps
	Scope     string  `json:"scope,omitempty"` // "" = all; else transaction/request name
	Op        string  `json:"op"`              // <, <=, >, >=
	Threshold float64 `json:"threshold"`
}

// Validate returns human-readable problems; empty means the scenario is executable.
func (s *Scenario) Validate() []string {
	var p []string
	if s.Version != "" && s.Version != SchemaVersion {
		p = append(p, fmt.Sprintf("unsupported version %q (expected %q)", s.Version, SchemaVersion))
	}
	if len(s.Steps) == 0 {
		p = append(p, "scenario has no steps")
	}
	walk := func(prefix string, steps []Step) {}
	walk = func(prefix string, steps []Step) {
		for i, st := range steps {
			where := fmt.Sprintf("%s[%d]", prefix, i)
			if st.Name != "" {
				where += " (" + st.Name + ")"
			}
			switch st.Type {
			case StepRequest:
				if st.Request == nil {
					p = append(p, where+": request step without request")
					continue
				}
				if st.Request.URL == "" {
					p = append(p, where+": request URL is empty")
				}
				if st.Request.Method == "" {
					p = append(p, where+": request method is empty")
				}
				for _, e := range st.Request.Extract {
					if e.Name == "" {
						p = append(p, where+": extractor without variable name")
					}
					switch e.Type {
					case ExJSONPath, ExRegex, ExXPath, ExHeader, ExCookie, ExBoundary, ExStatus:
					default:
						p = append(p, fmt.Sprintf("%s: unknown extractor type %q", where, e.Type))
					}
				}
				for _, a := range st.Request.Assert {
					switch a.Type {
					case AsStatus, AsContains, AsNotContains, AsRegex, AsJSONPath, AsMaxLatency, AsHeader:
					default:
						p = append(p, fmt.Sprintf("%s: unknown assertion type %q", where, a.Type))
					}
				}
			case StepTransaction, StepGroup, StepIf:
				if st.Type == StepTransaction && st.Name == "" {
					p = append(p, where+": transaction needs a name")
				}
				walk(where+".steps", st.Steps)
			case StepLoop:
				if st.Count < 1 {
					p = append(p, where+": loop count must be >= 1")
				}
				walk(where+".steps", st.Steps)
			case StepThink:
				if st.Think == nil {
					p = append(p, where+": think step without think time")
				}
			case StepSetVar:
			default:
				p = append(p, fmt.Sprintf("%s: unknown step type %q", where, st.Type))
			}
		}
	}
	walk("setup", s.Setup)
	walk("steps", s.Steps)
	walk("teardown", s.Teardown)
	w := s.Workload
	switch w.Executor {
	case ExecConstantVUs:
		if w.VUs < 1 || w.Duration <= 0 {
			p = append(p, "constant-vus needs vus >= 1 and duration > 0")
		}
	case ExecRampingVUs:
		if len(w.Stages) == 0 {
			p = append(p, "ramping-vus needs at least one stage")
		}
	case ExecArrivalRate:
		if w.Rate <= 0 || w.Duration <= 0 {
			p = append(p, "constant-arrival-rate needs rate > 0 and duration > 0")
		}
	case ExecRampingRate:
		if len(w.Stages) == 0 {
			p = append(p, "ramping-arrival-rate needs at least one stage")
		}
	case ExecPerVUIterations:
		if w.VUs < 1 || w.Iterations < 1 {
			p = append(p, "per-vu-iterations needs vus >= 1 and iterations >= 1")
		}
	case "":
		p = append(p, "workload.executor is required")
	default:
		p = append(p, fmt.Sprintf("unknown executor %q", w.Executor))
	}
	for _, d := range s.DataSources {
		if d.Name == "" {
			p = append(p, "data source without name")
		}
		if d.File == "" && len(d.Rows) == 0 {
			p = append(p, fmt.Sprintf("data source %q has no file and no rows", d.Name))
		}
		switch d.Mode {
		case "", "sequential", "random", "unique", "per-vu":
		default:
			p = append(p, fmt.Sprintf("data source %q: unknown mode %q", d.Name, d.Mode))
		}
	}
	for _, sla := range s.SLAs {
		switch sla.Op {
		case "<", "<=", ">", ">=":
		default:
			p = append(p, fmt.Sprintf("SLA %s: invalid op %q", sla.Metric, sla.Op))
		}
	}
	return p
}

// MaxPlannedVUs returns the largest number of concurrently active VUs the workload can create.
func (w Workload) MaxPlannedVUs() int {
	switch w.Executor {
	case ExecRampingVUs:
		m := w.VUs
		for _, s := range w.Stages {
			if int(s.Target) > m {
				m = int(s.Target)
			}
		}
		return m
	case ExecArrivalRate, ExecRampingRate:
		if w.MaxVUs > 0 {
			return w.MaxVUs
		}
		return w.VUs
	default:
		return w.VUs
	}
}

// PlannedDuration is the scheduled duration excluding graceful stop.
func (w Workload) PlannedDuration() time.Duration {
	switch w.Executor {
	case ExecRampingVUs, ExecRampingRate:
		var t time.Duration
		for _, s := range w.Stages {
			t += s.Duration.D()
		}
		return t
	default:
		return w.Duration.D()
	}
}

// Walk visits every step depth-first.
func Walk(steps []Step, fn func(*Step)) {
	for i := range steps {
		fn(&steps[i])
		Walk(steps[i].Steps, fn)
	}
}

// AllRequests returns pointers to every request step (setup, steps, teardown).
func (s *Scenario) AllRequests() []*Step {
	var out []*Step
	f := func(st *Step) {
		if st.Type == StepRequest && st.Request != nil {
			out = append(out, st)
		}
	}
	Walk(s.Setup, f)
	Walk(s.Steps, f)
	Walk(s.Teardown, f)
	return out
}

func HeaderValue(h []Header, name string) string {
	for _, x := range h {
		if strings.EqualFold(x.Name, name) {
			return x.Value
		}
	}
	return ""
}

func NewScenario(name string) *Scenario {
	return &Scenario{Version: SchemaVersion, Name: name, Variables: map[string]string{},
		Workload: Workload{Executor: ExecConstantVUs, VUs: 1, Duration: Duration(30 * time.Second)}}
}

// Clone returns a deep copy (via JSON) of the scenario.
func (s *Scenario) Clone() *Scenario {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var c Scenario
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return &c
}
