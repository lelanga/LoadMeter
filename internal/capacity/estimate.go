package capacity

import (
	"fmt"
	"math"
	"runtime"
	"sort"
)

// DefaultHeadroomPct is the share of each instance's measured capacity kept free.
const DefaultHeadroomPct = 30

// Estimate is a planning aid derived from a capacity Result. Every figure is
// an ESTIMATE that holds only under the listed assumptions. Load Meter never
// invents prices: multiply the counts by your own unit costs.
type Estimate struct {
	Label           string  `json:"label"`
	TargetRate      float64 `json:"targetRate,omitempty"` // iterations/s
	TargetRPS       float64 `json:"targetRPS,omitempty"`  // requests/s at the measured requests/iteration
	HeadroomPct     float64 `json:"headroomPct"`
	InstancesTested int     `json:"instancesTested"`
	// PerInstanceRate is the measured sustainable rate divided by InstancesTested.
	PerInstanceRate float64 `json:"perInstanceRate"`
	UsablePerInst   float64 `json:"usablePerInstance"` // PerInstanceRate x (1 - headroom)
	Instances       int     `json:"instances,omitempty"`
	InstancesNote   string  `json:"instancesNote,omitempty"`
	// Generator calibration on this machine.
	RequestsPerIteration float64  `json:"requestsPerIteration,omitempty"`
	GenReqPerCore        float64  `json:"generatorRequestsPerCoreSec,omitempty"`
	GenCPUs              int      `json:"generatorCPUs"`
	GenCoresForTarget    float64  `json:"generatorCoresForTarget,omitempty"`
	GenMachinesForTarget int      `json:"generatorMachinesForTarget,omitempty"`
	GenNote              string   `json:"generatorNote,omitempty"`
	Assumptions          []string `json:"assumptions"`
}

// BuildEstimate computes:
//
//	instances = ceil(targetRate / (perInstanceRate x (1 - headroom/100)))
//
// where perInstanceRate = maxSustainableRate / instancesTested, and the
// generator calibration requests/s per fully used core = median over valid
// steps of requestRate / (cpuPercent/100).
func BuildEstimate(res *Result, targetRate, headroomPct float64, instancesTested int) *Estimate {
	if headroomPct <= 0 || headroomPct >= 100 {
		headroomPct = DefaultHeadroomPct
	}
	if instancesTested <= 0 {
		instancesTested = 1
	}
	e := &Estimate{Label: "ESTIMATE (assumption-based, not a measurement)", TargetRate: targetRate, HeadroomPct: headroomPct,
		InstancesTested: instancesTested, GenCPUs: runtime.NumCPU()}
	if res.Env.CPUs > 0 {
		e.GenCPUs = res.Env.CPUs
	}
	var reqs, iters uint64
	var perCore []float64
	for _, s := range res.Steps {
		if s.Verdict == VerdictGenerator {
			continue
		}
		reqs += s.Requests
		iters += s.Iterations
		if s.CPUPercent >= 5 && s.RequestRate > 0 {
			perCore = append(perCore, s.RequestRate/(s.CPUPercent/100))
		}
	}
	if iters > 0 {
		e.RequestsPerIteration = r2(float64(reqs) / float64(iters))
	}
	e.PerInstanceRate = r2(res.MaxSustainableRate / float64(instancesTested))
	e.UsablePerInst = r2(e.PerInstanceRate * (1 - headroomPct/100))
	switch {
	case targetRate <= 0:
		e.InstancesNote = "no --target-rate given"
	case e.UsablePerInst <= 0:
		e.InstancesNote = "no sustainable rate was measured, so instances cannot be estimated"
	default:
		e.Instances = int(math.Ceil(targetRate / e.UsablePerInst))
		if res.Bound != "measured" {
			e.InstancesNote = "per-instance rate is a lower bound, so this count may be too high"
		}
	}
	if targetRate > 0 && e.RequestsPerIteration > 0 {
		e.TargetRPS = r2(targetRate * e.RequestsPerIteration)
	}
	if len(perCore) > 0 {
		sort.Float64s(perCore)
		e.GenReqPerCore = math.Round(perCore[len(perCore)/2])
		if e.TargetRPS > 0 {
			e.GenCoresForTarget = r2(e.TargetRPS / e.GenReqPerCore / (1 - headroomPct/100))
			e.GenMachinesForTarget = int(math.Ceil(e.GenCoresForTarget / float64(e.GenCPUs)))
		}
	} else {
		e.GenNote = "generator CPU was not measured (needs Linux/macOS and steps using at least 5% of a core)"
	}
	e.Assumptions = []string{
		fmt.Sprintf("Linear scaling: N instances serve N x %.2f it/s. Shared dependencies (database, cache, network, locks) often break this.", e.PerInstanceRate),
		fmt.Sprintf("The tested target was %d instance(s) (--instances-tested).", instancesTested),
		fmt.Sprintf("Headroom %g%%: only %g%% of an instance's measured capacity is planned for (safety factor %.2f).", headroomPct, 100-headroomPct, 1-headroomPct/100),
		"Same traffic mix, payload sizes, data volume and cache state as this test.",
		"Generator calibration counts this process's CPU on this machine (it includes an in-process mock server if one runs); other hardware, TLS or payloads change it.",
		"No prices are known to Load Meter: multiply the counts by your own unit costs.",
	}
	return e
}
