package accesslog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/redact"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/thinktime"
)

// MinThinkSamples is the number of gaps required before a distribution is fitted;
// with fewer, journey mode falls back to a uniform pause around the median.
const MinThinkSamples = 50

// ProductionWarning is shown by every front end before replaying.
const ProductionWarning = "Replaying production traffic patterns generates real load: only run this against systems you own or are explicitly authorized to test, and never against production without written approval."

// CheckTarget validates the replay target before any log is read. Logs rarely
// carry the host, so a base URL is mandatory; non-loopback hosts must be named
// in AllowHosts (the engine's allowlist still applies at run time).
func CheckTarget(o Options) (*url.URL, error) {
	if strings.TrimSpace(o.BaseURL) == "" {
		return nil, errors.New("--base-url is required: access logs rarely contain the target host, so say where to replay (for example http://127.0.0.1:8099)")
	}
	u, err := url.Parse(strings.TrimSpace(o.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("--base-url %q must be an http(s) URL with a host", o.BaseURL)
	}
	if u.User != nil {
		return nil, errors.New("--base-url must not contain credentials")
	}
	if !safety.IsLoopback(u.Hostname()) && !(safety.Policy{AllowedHosts: o.AllowHosts}).HostAllowed(u.Hostname()) {
		return nil, fmt.Errorf("refusing non-loopback target %q: replaying a production traffic profile there needs authorization. Pass --allow-host %s only for systems you are authorized to test (the run-time allowlist still applies)", u.Hostname(), u.Hostname())
	}
	return u, nil
}

// Import is Analyze + Generate with the target checked first.
func Import(r io.Reader, source string, o Options) (*model.Scenario, *model.CompatReport, *Profile, error) {
	o.defaults()
	if _, err := CheckTarget(o); err != nil {
		return nil, nil, nil, err
	}
	if o.Scale <= 0 || o.Scale > 10 {
		return nil, nil, nil, fmt.Errorf("--scale %g out of range (0 < scale <= 10; default 0.1 = 10%% of logged traffic)", o.Scale)
	}
	p, err := Analyze(r, source, o)
	if err != nil {
		return nil, nil, nil, err
	}
	sc, rep, err := Generate(p, o)
	return sc, rep, p, err
}

func plural(n int, w string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, w)
	}
	return fmt.Sprintf("%d %ss", n, w)
}

// idURL substitutes {id} slots with expressions the engine understands.
func idURL(e *Endpoint) string {
	segs := strings.Split(e.Path, "/")
	slot := 0
	for i, s := range segs {
		if s != "{id}" {
			continue
		}
		var ident *IDSlot
		for j := range e.IDs {
			if e.IDs[j].Index == i-1 {
				ident = &e.IDs[j]
			}
		}
		slot++
		switch {
		case ident == nil:
			segs[i] = "1"
		case ident.Kind == IDNumeric:
			segs[i] = fmt.Sprintf("${__randomInt(%d,%d)}", ident.Min, max(ident.Max, ident.Min))
		case ident.Kind == IDUUID:
			segs[i] = "${__uuid}"
		default:
			segs[i] = fmt.Sprintf("${__randomString(%d)}", min(max(ident.Len, 8), 64))
		}
	}
	return strings.Join(segs, "/")
}

// safeQuery redacts a logged query string; ok=false means it is dropped.
func safeQuery(q string) (string, bool) {
	if q == "" || strings.Contains(q, "${") {
		return "", false
	}
	if _, err := url.ParseQuery(q); err != nil {
		return "", false
	}
	const pre = "http://x/?"
	red := redact.PII(redact.URL(pre + q))
	return strings.TrimPrefix(red, pre), strings.HasPrefix(red, pre)
}

// Generate builds the scenario and compatibility report from a profile.
func Generate(p *Profile, o Options) (*model.Scenario, *model.CompatReport, error) {
	o.defaults()
	if _, err := CheckTarget(o); err != nil {
		return nil, nil, err
	}
	if o.Scale <= 0 || o.Scale > 10 {
		return nil, nil, fmt.Errorf("--scale %g out of range (0 < scale <= 10)", o.Scale)
	}
	rep := &model.CompatReport{Source: p.Source, Format: "accesslog"}
	rep.Warnings = append(rep.Warnings, ProductionWarning)
	if len(p.Endpoints) == 0 {
		return nil, nil, fmt.Errorf("no replayable requests in %d parsed lines (ignored: %s); try --include-writes, --include-errors or --include-static", p.Parsed, ignoredSummary(p.Ignored))
	}
	u, _ := url.Parse(strings.TrimSpace(o.BaseURL))
	if p.Hosts[strings.ToLower(u.Hostname())] > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the replay target %s also appears as a host in the log: this would replay production traffic at the system that served it", u.Hostname()))
	}
	if o.Scale > 1 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("--scale %g is above 1: the test will exceed the observed production rate", o.Scale))
	}

	name := o.Name
	if name == "" {
		name = "access-log-replay"
	}
	sc := model.NewScenario(name)
	sc.Metadata = map[string]string{}
	sc.Defaults.BaseURL = strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	sc.Defaults.Timeout = model.Duration(30 * time.Second)
	sc.Description = fmt.Sprintf("Replay of %s (%s, %d requests) at %.0f%% of the logged rate, window %s. %s",
		p.Source, p.Format, p.Replayable, o.Scale*100, p.Window.Spec, ProductionWarning)

	// Weighted endpoint mix.
	top := p.Endpoints
	tail := 0
	if len(top) > o.Top {
		for _, e := range top[o.Top:] {
			tail += e.Count
		}
		rep.Add("long tail", fmt.Sprintf("%d endpoints", len(top)-o.Top), "", model.Ignored,
			fmt.Sprintf("%d requests (%.1f%%) are not in the mix; their load is spread over the kept endpoints. Raise --top to keep more", tail, 100*float64(tail)/float64(p.Replayable)))
		top = top[:o.Top]
	}
	kept := 0
	for _, e := range top {
		kept += e.Count
	}
	group := model.Step{Type: model.StepGroup, Name: "Traffic mix", Select: model.SelectWeighted,
		Comment: "one request per iteration, chosen at random in proportion to its observed share"}
	hasWrite, hasID := false, false
	for _, e := range top {
		w := math.Round(1000*100*float64(e.Count)/float64(kept)) / 1000
		if w <= 0 {
			w = 0.001
		}
		u := idURL(e)
		if len(e.IDs) > 0 {
			hasID = true
		}
		if o.KeepQuery && e.Query != "" {
			u += "?" + e.Query
		}
		step := model.Step{Type: model.StepRequest, Name: e.Key(), Weight: &w,
			Comment: fmt.Sprintf("%.2f%% of replayable requests (%d); statuses %s", e.Share, e.Count, statusMix(e.Statuses)),
			Request: &model.Request{Method: e.Method, URL: u}}
		note := fmt.Sprintf("weight %.3f%%", w)
		status := model.Supported
		if isWrite(e.Method) {
			hasWrite = true
			status = model.Partial
			note += "; request bodies are not in access logs, so the request is sent empty"
		}
		group.Steps = append(group.Steps, step)
		rep.Add("endpoint", e.Key(), "", status, note)
	}
	if hasWrite {
		rep.Manual = append(rep.Manual, "write requests are sent without a body (access logs do not record bodies): add payloads before running, and only against a disposable environment")
	}
	if hasID {
		rep.Manual = append(rep.Manual, "{id} path segments were replaced by random ids from the observed numeric range, random UUIDs or random strings; most will not exist on the target (expect 404s). Replace them with a data source of real ids for a faithful replay")
	}

	// Ignored categories (never silent).
	for _, k := range sortedKeys(p.Ignored) {
		n := p.Ignored[k]
		note := fmt.Sprintf("%d lines", n)
		switch {
		case strings.HasPrefix(k, "write requests"):
			note += "; replay with --include-writes (bodies are not in logs)"
		case k == "static assets":
			note += "; replay with --include-static"
		case strings.HasPrefix(k, "requests that returned"):
			note += "; replay with --include-errors"
		}
		rep.Add("log lines", k, "", model.Ignored, note)
	}
	if p.Malformed > 0 {
		rep.Add("malformed lines", "", fmt.Sprintf("first at line %v", p.FirstBad), model.Ignored, fmt.Sprintf("%d of %d lines could not be parsed and were skipped", p.Malformed, p.Lines))
	}
	if p.QueryCount > 0 {
		if o.KeepQuery {
			rep.Add("query strings", "", "", model.Partial, fmt.Sprintf("%d requests had a query string; the most common one per endpoint is kept with sensitive values redacted (%d dropped as unparseable)", p.QueryCount, p.QueriesDropped))
		} else {
			rep.Add("query strings", "", "", model.Ignored, fmt.Sprintf("dropped from %d requests; pass --keep-query to keep the most common one per endpoint (values redacted)", p.QueryCount))
		}
	}
	if p.Truncated {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("input exceeded the %d MB cap; only the first %d MB were analysed", MaxInputBytes>>20, MaxInputBytes>>20))
		rep.Add("input", p.Source, "", model.Partial, "truncated at the size cap")
	}
	if p.ReadError != "" {
		rep.Warnings = append(rep.Warnings, "read error, analysis covers the data before it: "+p.ReadError)
	}
	if p.Parsed < 100 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("only %d parsed lines: the mix and rate curve are weak estimates", p.Parsed))
	}

	// Workload.
	perIter := 1.0
	var thinkStep *model.Step
	ss := p.Sessions
	if o.Journey {
		k := math.Min(math.Max(math.Round(ss.MeanRequests), 1), 20)
		perIter = k
		thinkStep = a2think(&ss, rep, &sc.Metadata)
		sc.Steps = []model.Step{{Type: model.StepLoop, Name: "Session", Count: int(k),
			Comment: fmt.Sprintf("one iteration = one session of ~%d requests (mean %.1f from reconstructed sessions; approximate)", int(k), ss.MeanRequests),
			Steps:   []model.Step{group, *thinkStep}}}
		rep.Add("journey", "sessions", "", model.Partial, fmt.Sprintf("session = %d requests drawn from the mix, separated by fitted think time; the arrival rate is sessions/s (request rate / %d)", int(k), int(k)))
	} else {
		sc.Steps = []model.Step{group}
		describeThink(&ss, rep, &sc.Metadata)
	}
	peakIter := buildWorkload(sc, p, o, perIter, thinkStep)
	rep.Add("workload", sc.Workload.Executor, "", model.Supported,
		fmt.Sprintf("window %s (%s), %s, scale %.2f, peak %.2f iterations/s, maxVUs %d", p.Window.Spec, windowSpan(p), plural(len(sc.Workload.Stages), "stage"), o.Scale, peakIter, sc.Workload.MaxVUs))
	if len(sc.Workload.Stages) > 0 {
		rep.Warnings = append(rep.Warnings, "stages follow the per-minute request counts; the engine interpolates linearly between minute points, which smooths the logged curve slightly")
	}

	sc.Metadata["importer"] = "accesslog"
	sc.Metadata["source"] = p.Source
	sc.Metadata["format"] = p.Format
	sc.Metadata["scale"] = strconv.FormatFloat(o.Scale, 'g', -1, 64)
	sc.Metadata["window"] = p.Window.Spec
	sc.Metadata["warning"] = ProductionWarning

	if probs := sc.Validate(); len(probs) > 0 {
		rep.Validated = "failed"
		return nil, rep, fmt.Errorf("generated scenario is invalid: %s", strings.Join(probs, "; "))
	}
	rep.ComputeConfidence()
	rep.Validated = "passed"
	return sc, rep, nil
}

func windowSpan(p *Profile) string {
	return fmt.Sprintf("%d min from %s", p.Window.Minutes, p.Window.Start)
}

func sortedKeys(m map[string]int) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

func ignoredSummary(m map[string]int) string {
	if len(m) == 0 {
		return "nothing"
	}
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s: %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func statusMix(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	var all []kv
	total := 0
	for k, n := range m {
		all = append(all, kv{k, n})
		total += n
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].k < all[j].k
	})
	var parts []string
	for i, x := range all {
		if i == 4 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", x.k, 100*float64(x.n)/float64(total)))
	}
	return strings.Join(parts, ", ")
}

// buildWorkload sets the arrival-rate executor and returns the peak iteration rate.
func buildWorkload(sc *model.Scenario, p *Profile, o Options, perIter float64, think *model.Step) float64 {
	w := model.Workload{}
	var peak float64
	// Concurrency estimate (Little's law): iterations/s x seconds per iteration.
	iterSecs := 1.0
	if think != nil && think.Think != nil {
		iterSecs = perIter * (1 + thinkMean(think.Think))
	}
	if p.window == nil { // avg: one constant rate
		rate := p.Window.AvgRPS * o.Scale / perIter
		peak = rate
		d := time.Duration(min(max(p.Window.Minutes, 1), 10)) * time.Minute
		w = model.Workload{Executor: model.ExecArrivalRate, Rate: rate, Duration: model.Duration(d)}
	} else {
		w.Executor = model.ExecRampingRate
		for _, c := range p.window {
			t := c / 60 * o.Scale / perIter
			peak = math.Max(peak, t)
			w.Stages = append(w.Stages, model.Stage{Duration: model.Duration(time.Minute), Target: math.Round(t*1e6) / 1e6})
		}
		w.Rate = math.Floor(w.Stages[0].Target)
	}
	w.MaxVUs = int(math.Min(500, math.Max(20, math.Ceil(peak*iterSecs*3)+10)))
	w.VUs = min(10, w.MaxVUs)
	sc.Workload = w
	return peak
}

// a2think returns the think step for journey mode and records how it was chosen.
func a2think(ss *SessionStats, rep *model.CompatReport, meta *map[string]string) *model.Step {
	if *meta == nil {
		*meta = map[string]string{}
	}
	if fit := ss.ThinkFit; fit != nil && fit.N >= MinThinkSamples {
		th := ThinkFromFit(fit)
		(*meta)["thinkTime"] = describeFit(fit)
		rep.Add("think time", fit.Dist, "", model.Supported, describeFit(fit))
		return &model.Step{Type: model.StepThink, Name: "think", Think: th, Comment: "fitted from session gaps: " + describeFit(fit)}
	}
	med := 5.0
	if ss.ThinkFit != nil {
		med = ss.ThinkFit.MedianSec
	}
	note := fmt.Sprintf("only %d gaps (need %d to fit): uniform pause around the median %.1fs", ss.GapsCollected, MinThinkSamples, med)
	(*meta)["thinkTime"] = note
	rep.Add("think time", "uniform", "", model.Partial, note)
	return &model.Step{Type: model.StepThink, Name: "think", Comment: note,
		Think: &model.Think{Min: dur(med * 0.5), Max: dur(med * 1.5)}}
}

// describeThink records the fit (when any) without using it, for non-journey runs.
func describeThink(ss *SessionStats, rep *model.CompatReport, meta *map[string]string) {
	if *meta == nil {
		*meta = map[string]string{}
	}
	if fit := ss.ThinkFit; fit != nil && fit.N >= MinThinkSamples {
		(*meta)["thinkTime"] = describeFit(fit)
		rep.Add("think time", fit.Dist, "", model.Ignored, describeFit(fit)+"; not used because each iteration is a single request (open model). Re-run with --journey to replay sessions with this think time")
	}
}

func dur(sec float64) model.Duration {
	return model.Duration(time.Duration(sec * float64(time.Second)).Round(10 * time.Millisecond))
}

// ThinkFromFit converts a fit into a model.Think clamped to [MachineGap, p99.5].
func ThinkFromFit(fit *thinktime.Result) *model.Think {
	d := &model.Distribution{Type: fit.Dist}
	switch fit.Dist {
	case model.DistLogNormal:
		d.Mu, d.Sigma = fit.Params["mu"], fit.Params["sigma"]
	case model.DistExponential:
		d.Mean = fit.Params["mean"]
	case model.DistGamma:
		d.Shape, d.Scale = fit.Params["shape"], fit.Params["scale"]
	}
	hi := math.Min(math.Max(fit.P995Sec, 1), 300)
	return &model.Think{Min: model.Duration(MachineGap), Max: dur(hi), Distribution: d}
}

func describeFit(f *thinktime.Result) string {
	b, _ := json.Marshal(f.Params)
	return fmt.Sprintf("%s %s (n=%d, AIC %.1f, median %.1fs, p99.5 %.1fs)", f.Dist, b, f.N, f.AIC, f.MedianSec, f.P995Sec)
}

// thinkMean is the expected pause in seconds (clamps ignored for the estimate).
func thinkMean(t *model.Think) float64 {
	if d := t.Distribution; d != nil {
		switch d.Type {
		case model.DistLogNormal:
			return math.Exp(d.Mu + d.Sigma*d.Sigma/2)
		case model.DistExponential:
			return d.Mean
		case model.DistGamma:
			return d.Shape * d.Scale
		}
	}
	return (t.Min.D() + t.Max.D()).Seconds() / 2
}
