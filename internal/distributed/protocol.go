// Package distributed runs one scenario across several Phoenix load agents.
//
// The controller (phoenix run --agents ...) splits the workload, ships the
// scenario with its data to every agent over HTTPS (self-signed agent
// certificate pinned by SHA-256 fingerprint, shared bearer token), starts all
// nodes at a synchronized instant and merges the per-second metric deltas
// (serialized histograms) into one Collector, so percentiles are exact across
// agents. Agents enforce their own local safety policy; nothing the
// controller sends can widen it.
//
// Wire protocol (all JSON, all endpoints require Authorization: Bearer):
//
//	GET  /v1/hello  -> Hello
//	POST /v1/run    StartRequest -> NDJSON stream of Message
//	                ("ready", then "delta" every second, then "final")
//	POST /v1/stop   graceful stop of the current run
//
// Closing the /v1/run stream hard-stops the agent's run, so a lost controller
// never leaves orphaned load behind.
package distributed

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/engine"
	"github.com/lelanga/phoenix/internal/metrics"
	"github.com/lelanga/phoenix/internal/model"
)

// ProtocolVersion must match between controller and agent.
const ProtocolVersion = 1

// DefaultPort is the agent's default listen port.
const DefaultPort = 7979

const (
	maxStartBody    = 512 << 20 // scenario + inline CSV rows + files
	maxShippedFiles = 64 << 20  // multipart upload files shipped to agents
	maxStartDelay   = 2 * time.Minute
)

// Hello is the agent's identity and clock, used for skew measurement.
type Hello struct {
	Protocol     int       `json:"protocol"`
	Version      string    `json:"version"`
	AgentID      string    `json:"agentId"`
	Hostname     string    `json:"hostname"`
	CPUs         int       `json:"cpus"`
	Time         time.Time `json:"time"`
	Busy         bool      `json:"busy"`
	MaxVUs       int       `json:"maxVUs"`
	AllowedHosts int       `json:"allowedHosts"`
}

// StartRequest asks an agent to run its share of the scenario.
type StartRequest struct {
	Protocol  int               `json:"protocol"`
	RunID     string            `json:"runId"`
	Node      string            `json:"node"`
	Scenario  *model.Scenario   `json:"scenario"`
	Files     map[string][]byte `json:"files,omitempty"` // multipart upload files, relative path -> content
	StartAt   time.Time         `json:"startAt"`         // in the agent's clock
	Seed      uint64            `json:"seed"`
	Confirmed bool              `json:"confirmed"` // operator confirmed a high-impact test on the controller
}

// Message is one NDJSON line of the /v1/run stream.
type Message struct {
	Type      string         `json:"type"` // ready | delta | final | error
	Error     string         `json:"error,omitempty"`
	Delta     *metrics.Delta `json:"delta,omitempty"`
	AgentTime time.Time      `json:"agentTime,omitempty"`
	State     string         `json:"state,omitempty"`
	StartedAt time.Time      `json:"startedAt,omitempty"`
	EndedAt   time.Time      `json:"endedAt,omitempty"`
}

// apportion splits total into integer parts proportional to weights using the
// largest-remainder method; the parts always add up to total.
func apportion(total int, weights []float64) []int {
	out := make([]int, len(weights))
	if total <= 0 || len(weights) == 0 {
		return out
	}
	var sum float64
	for _, w := range weights {
		sum += w
	}
	if sum <= 0 {
		return out
	}
	type rem struct {
		i int
		r float64
	}
	rems := make([]rem, len(weights))
	given := 0
	for i, w := range weights {
		exact := float64(total) * w / sum
		out[i] = int(math.Floor(exact))
		given += out[i]
		rems[i] = rem{i, exact - float64(out[i])}
	}
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].r > rems[b].r })
	for k := 0; given < total; k++ {
		out[rems[k%len(rems)].i]++
		given++
	}
	return out
}

// SplitWorkload divides a workload across nodes by weight. A nil entry means
// the node gets no load (for example 3 VUs over 4 nodes).
func SplitWorkload(w model.Workload, weights []float64) []*model.Workload {
	n := len(weights)
	out := make([]*model.Workload, n)
	clone := func() *model.Workload {
		c := w
		c.Stages = append([]model.Stage(nil), w.Stages...)
		return &c
	}
	var sum float64
	for _, x := range weights {
		sum += x
	}
	switch w.Executor {
	case model.ExecConstantVUs, model.ExecPerVUIterations:
		vus := apportion(w.VUs, weights)
		for i := range out {
			if vus[i] > 0 {
				out[i] = clone()
				out[i].VUs = vus[i]
			}
		}
	case model.ExecRampingVUs:
		start := apportion(w.VUs, weights)
		targets := make([][]int, len(w.Stages))
		for s, st := range w.Stages {
			targets[s] = apportion(int(math.Round(st.Target)), weights)
		}
		for i := range out {
			c := clone()
			c.VUs = start[i]
			peak := c.VUs
			for s := range c.Stages {
				c.Stages[s].Target = float64(targets[s][i])
				peak = max(peak, targets[s][i])
			}
			if peak > 0 {
				out[i] = c
			}
		}
	case model.ExecArrivalRate, model.ExecRampingRate:
		pool := w.MaxVUs
		if pool <= 0 {
			pool = max(w.VUs, 1)
		}
		pools := apportion(pool, weights)
		pre := apportion(w.VUs, weights)
		for i := range out {
			if weights[i] <= 0 || sum <= 0 {
				continue
			}
			f := weights[i] / sum
			c := clone()
			c.Rate = w.Rate * f
			for s := range c.Stages {
				c.Stages[s].Target = w.Stages[s].Target * f
			}
			c.MaxVUs = max(pools[i], 1)
			c.VUs = min(max(pre[i], 1), c.MaxVUs)
			out[i] = c
		}
	default:
		for i := range out {
			out[i] = clone()
		}
	}
	return out
}

// NodePlan is one node's share of a distributed run.
type NodePlan struct {
	Scenario *model.Scenario
	Files    map[string][]byte
	Seed     uint64
}

// Plan builds each node's scenario: the split workload, every data source
// converted to inline rows (CSV contents travel with the scenario) and, for
// mode=unique, a disjoint partition of the rows per node so values never
// collide across agents. mode=per-vu rows are rotated by the node's global VU
// offset so VU n on node 2 does not reuse VU n's row on node 1. Nodes that get
// no load have a nil Scenario.
func Plan(sc *model.Scenario, baseDir string, weights []float64, nodeIDs []string) ([]NodePlan, []string, error) {
	var notes []string
	wls := SplitWorkload(sc.Workload, weights)
	plans := make([]NodePlan, len(weights))
	var active []int
	var activeW []float64
	for i, w := range wls {
		if w == nil {
			notes = append(notes, fmt.Sprintf("node %s gets no share of this workload (too few VUs to split) and stays idle", nodeIDs[i]))
			continue
		}
		c := sc.Clone()
		c.Workload = *w
		c.DataSources = nil
		h := fnv.New64a()
		h.Write([]byte(nodeIDs[i]))
		plans[i] = NodePlan{Scenario: c, Seed: 42 + h.Sum64()%1_000_000}
		active = append(active, i)
		activeW = append(activeW, weights[i])
	}
	if len(active) == 0 {
		return nil, notes, fmt.Errorf("the workload is too small to run on any node")
	}
	for _, ds := range sc.DataSources {
		cols, rows, err := engine.LoadRows(ds, baseDir)
		if err != nil {
			return nil, notes, err
		}
		base := ds
		base.File, base.Delimiter, base.Columns, base.Rows = "", "", cols, nil
		switch ds.Mode {
		case "unique":
			parts := apportion(len(rows), activeW)
			off := 0
			for k, i := range active {
				if parts[k] == 0 {
					return nil, notes, fmt.Errorf("data source %q (mode=unique) has %d rows, too few to give every node at least one", ds.Name, len(rows))
				}
				d := base
				d.Rows = rows[off : off+parts[k]]
				off += parts[k]
				plans[i].Scenario.DataSources = append(plans[i].Scenario.DataSources, d)
			}
			notes = append(notes, fmt.Sprintf("data source %q (unique): %d rows partitioned across %d nodes", ds.Name, len(rows), len(active)))
		case "per-vu":
			vuOff := 0
			for _, i := range active {
				d := base
				k := vuOff % len(rows)
				d.Rows = append(append([][]string(nil), rows[k:]...), rows[:k]...)
				plans[i].Scenario.DataSources = append(plans[i].Scenario.DataSources, d)
				vuOff += plans[i].Scenario.Workload.MaxPlannedVUs()
			}
		default:
			for _, i := range active {
				d := base
				d.Rows = rows
				plans[i].Scenario.DataSources = append(plans[i].Scenario.DataSources, d)
			}
		}
	}
	files, fnotes, err := collectFiles(sc, baseDir)
	if err != nil {
		return nil, notes, err
	}
	notes = append(notes, fnotes...)
	for _, i := range active {
		plans[i].Files = files
	}
	return plans, notes, nil
}

// collectFiles reads multipart upload files referenced by literal paths.
func collectFiles(sc *model.Scenario, baseDir string) (map[string][]byte, []string, error) {
	var notes []string
	files := map[string][]byte{}
	total := 0
	for _, st := range sc.AllRequests() {
		for _, p := range st.Request.Multipart {
			if p.File == "" {
				continue
			}
			if strings.Contains(p.File, "${") {
				notes = append(notes, fmt.Sprintf("multipart file %q uses variables and is not shipped to agents; those uploads will fail on agents", p.File))
				continue
			}
			if _, ok := files[p.File]; ok {
				continue
			}
			path, err := engine.SafePath(baseDir, p.File)
			if err != nil {
				return nil, notes, err
			}
			b, err := readFileLimited(path, maxShippedFiles-total)
			if err != nil {
				return nil, notes, fmt.Errorf("multipart file %q: %w", p.File, err)
			}
			total += len(b)
			files[p.File] = b
		}
	}
	if len(files) == 0 {
		return nil, notes, nil
	}
	return files, notes, nil
}
