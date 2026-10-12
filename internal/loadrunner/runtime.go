package loadrunner

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// Run-time settings (default.cfg / default.usp). Key names follow commonly
// observed VuGen-generated files; every key that is read but cannot be
// mapped is reported, and unknown sections are listed as ignored.

type runtimeSettings struct {
	iterations int
	itLoc      string

	paceType string // asap | const | random | constafter | randomafter
	pace1    float64
	pace2    float64
	paceLoc  string

	thinkMode string // recorded | nothink | multiply | random
	factor    float64
	low, high float64 // percent, random mode
	limit     float64 // seconds; 0 = none
	thinkLoc  string
}

func atof(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil && v >= 0
}

func secs(f float64) model.Duration {
	return model.Duration(time.Duration(f * float64(time.Second)))
}

func (im *importer) loadCfg(name string, data []byte) {
	ini := parseINI(data)
	rs := &im.rts
	if s := ini.section("ThinkTime"); s != nil {
		rs.thinkLoc = fmt.Sprintf("%s:%d", name, s.line)
		switch o := norm(s.val("Options")); o {
		case "nothink", "ignore":
			rs.thinkMode = "nothink"
		case "recorded", "asrecorded", "":
			rs.thinkMode = "recorded"
		case "multiply", "multiplied":
			rs.thinkMode = "multiply"
		case "random", "randomize":
			rs.thinkMode = "random"
		default:
			rs.thinkMode = "recorded"
			im.rep.Add("ThinkTime.Options", s.val("Options"), rs.thinkLoc, model.Partial, "unknown think-time option; using recorded think times")
		}
		rs.factor = 1
		if f, ok := atof(s.val("Factor")); ok {
			rs.factor = f
		}
		rs.low, rs.high = 50, 150
		if v, ok := atof(s.val("ThinkTimeRandomLow")); ok {
			rs.low = v
		}
		if v, ok := atof(s.val("ThinkTimeRandomHigh")); ok && v >= rs.low {
			rs.high = v
		}
		if s.val("LimitFlag") == "1" {
			if v, ok := atof(s.val("Limit")); ok && v > 0 {
				rs.limit = v
			}
		}
	}
	if s := ini.section("Iterations"); s != nil && rs.iterations == 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(s.val("NumOfIterations"))); err == nil && n > 0 {
			rs.iterations, rs.itLoc = n, fmt.Sprintf("%s:%d", name, s.line)
		}
	}
	for _, s := range ini.sections {
		switch strings.ToLower(s.name) {
		case "", "thinktime", "iterations":
			continue
		case "general", "log", "web", "internet protocol", "browser", "modem speed", "rendezvous",
			"webrecorder", "pagedownload", "proxy", "cmd", "vu", "thread", "javavm":
			im.ignoredOnce("cfg:"+strings.ToLower(s.name), "run-time settings ["+s.name+"]", "", fmt.Sprintf("%s:%d", name, s.line),
				"run-time setting section not mapped (logging, browser emulation, proxy, bandwidth and similar settings have no Load Meter equivalent or are engine-level options)")
		default:
			im.ignoredOnce("cfg:"+strings.ToLower(s.name), "run-time settings ["+s.name+"]", "", fmt.Sprintf("%s:%d", name, s.line), "unrecognized run-time setting section")
		}
	}
}

func (im *importer) loadUsp(name string, data []byte) {
	ini := parseINI(data)
	rs := &im.rts
	root := ini.section("RunLogicRunRoot")
	if root == nil {
		im.rep.Add("run logic", name, name, model.Ignored, "no [RunLogicRunRoot] section found")
		return
	}
	loc := fmt.Sprintf("%s:%d", name, root.line)
	if v, ok := root.get("RunLogicNumOfIterations"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			rs.iterations, rs.itLoc = n, loc
		}
	}
	if v, ok := root.get("RunLogicPaceType"); ok {
		rs.paceLoc = loc
		switch norm(v) {
		case "asap", "":
			rs.paceType = "asap"
		case "const", "constant", "fixed":
			rs.paceType = "const"
			rs.pace1, _ = atof(root.val("RunLogicPaceConstTime"))
		case "random":
			rs.paceType = "random"
			rs.pace1, _ = atof(root.val("RunLogicRandomPaceMin"))
			rs.pace2, _ = atof(root.val("RunLogicRandomPaceMax"))
		case "constafter", "constantafter":
			rs.paceType = "constafter"
			rs.pace1, _ = atof(root.val("RunLogicPaceConstAfterTime"))
		case "randomafter":
			rs.paceType = "randomafter"
			rs.pace1, _ = atof(root.val("RunLogicAfterPaceMin"))
			rs.pace2, _ = atof(root.val("RunLogicAfterPaceMax"))
		default:
			im.rep.Add("RunLogicPaceType", v, loc, model.Unsupported, "unknown pacing type; pacing not imported")
		}
	}
	if m := norm(root.val("RunLogicRunMode")); m != "" && m != "sequential" {
		im.rep.Add("RunLogicRunMode", root.val("RunLogicRunMode"), loc, model.Partial,
			"actions run in .usr order every iteration; random/weighted action selection is not reproduced")
	}
	for _, s := range ini.sections {
		n := strings.ToLower(s.name)
		if !strings.HasPrefix(n, "runlogicrunroot:") {
			continue
		}
		if norm(s.val("RunLogicObjectKind")) == "group" {
			note := "run-logic block flattened into the action sequence"
			if it := s.val("RunLogicNumOfIterations"); it != "" && it != "1" {
				note += fmt.Sprintf("; block iterations (%s) not reproduced", it)
			}
			if m := norm(s.val("RunLogicRunMode")); m != "" && m != "sequential" {
				note += "; block run mode " + s.val("RunLogicRunMode") + " not reproduced"
			}
			im.rep.Add("run logic block", s.name, fmt.Sprintf("%s:%d", name, s.line), model.Partial, note)
		}
	}
}

// applyRuntime maps run-time settings onto the workload and think steps.
func (im *importer) applyRuntime() {
	rs := &im.rts
	w := &im.sc.Workload
	w.Executor = model.ExecPerVUIterations
	w.VUs = 1
	w.Duration = 0
	w.Iterations = 1
	if rs.iterations > 0 {
		w.Iterations = rs.iterations
		im.rep.Add("iterations", strconv.Itoa(rs.iterations), rs.itLoc, model.Supported,
			"per-vu-iterations; VU count comes from the Controller scenario (not part of a VuGen script) and defaults to 1")
	} else {
		im.rep.Add("iterations", "1", "", model.Partial, "no iteration count in run-time settings; defaulted to 1 iteration for 1 VU (VuGen replay)")
	}
	switch rs.thinkMode {
	case "nothink":
		z := 0.0
		w.ThinkTimeScale = &z
		im.rep.Add("ThinkTime.Options", "ignore", rs.thinkLoc, model.Supported, "thinkTimeScale=0")
	case "recorded":
		one := 1.0
		w.ThinkTimeScale = &one
		im.rep.Add("ThinkTime.Options", "as recorded", rs.thinkLoc, model.Supported, "thinkTimeScale=1")
	case "multiply":
		f := rs.factor
		w.ThinkTimeScale = &f
		im.rep.Add("ThinkTime.Options", "multiply", rs.thinkLoc, model.Supported, fmt.Sprintf("thinkTimeScale=%g", f))
	case "random":
		one := 1.0
		w.ThinkTimeScale = &one
		im.rep.Add("ThinkTime.Options", "random", rs.thinkLoc, model.Supported,
			fmt.Sprintf("each think step is a uniform range [%g%%, %g%%] of the recorded value", rs.low, rs.high))
	default:
		im.rep.Add("ThinkTime.Options", "", "", model.Partial, "no think-time run-time setting found; recorded think times kept and the engine default scale applies")
	}
	if rs.limit > 0 {
		im.rep.Add("ThinkTime.Limit", fmt.Sprintf("%gs", rs.limit), rs.thinkLoc, model.Supported, "think times clamped at import")
	}
	switch rs.paceType {
	case "", "asap":
	case "const":
		w.Pacing = secs(rs.pace1)
		im.rep.Add("pacing", fmt.Sprintf("every %gs", rs.pace1), rs.paceLoc, model.Supported, "workload.pacing")
	case "random":
		w.Pacing = secs(rs.pace1)
		im.rep.Add("pacing", fmt.Sprintf("random %g-%gs", rs.pace1, rs.pace2), rs.paceLoc, model.Partial,
			"Load Meter pacing is fixed; the lower bound was used")
	case "constafter", "randomafter":
		th := &model.Think{Min: secs(rs.pace1)}
		label := fmt.Sprintf("%gs after each iteration", rs.pace1)
		if rs.paceType == "randomafter" && rs.pace2 > rs.pace1 {
			th.Max = secs(rs.pace2)
			label = fmt.Sprintf("%g-%gs after each iteration", rs.pace1, rs.pace2)
		}
		im.sc.Steps = append(im.sc.Steps, model.Step{Type: model.StepThink, Name: "pacing", Think: th, Comment: "pacing delay from " + rs.paceLoc})
		im.rep.Add("pacing", label, rs.paceLoc, model.Partial,
			"converted to a think step at the end of the iteration (it is affected by thinkTimeScale)")
	}
}

// think converts an lr_think_time value using the think-time settings.
func (im *importer) think(sec float64) *model.Think {
	rs := &im.rts
	if rs.limit > 0 && sec > rs.limit {
		sec = rs.limit
	}
	if rs.thinkMode == "random" {
		lo, hi := sec*rs.low/100, sec*rs.high/100
		if rs.limit > 0 && hi > rs.limit {
			hi = rs.limit
		}
		if lo > hi {
			lo = hi
		}
		return &model.Think{Min: secs(lo), Max: secs(hi)}
	}
	return &model.Think{Min: secs(sec)}
}
