// Package triage clusters the failures of a finished run and says which side
// they most likely come from: the test script, the target system, the load
// generator or Load Meter's own safety policy.
//
// It streams the run's raw samples CSV (raw.csv, JTL-compatible columns) with
// bounded memory: failure messages are normalised (normalize.go), mined into
// templates with a Drain-style fixed-depth prefix tree (drain.go), and grouped
// by template + error kind + HTTP status + request. Everything is
// deterministic: the same CSV and summary always give the same clusters,
// ranks, scores and explanations. No AI is involved.
package triage

import (
	"encoding/csv"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/redact"
)

// Sides a failure can be attributed to.
const (
	SideScript    = "script"
	SideTarget    = "target"
	SideGenerator = "generator"
	SidePolicy    = "policy"
	SideUnknown   = "unknown"
)

// Options tunes Analyze. The zero value is usable.
type Options struct {
	// Start is the run start; offsets are measured from it. When zero, the
	// timestamp of the first CSV row is used.
	Start time.Time
	// DurationSec is the run length (0 = unknown; then no sparkline, and the
	// last observed sample time is used for scoring).
	DurationSec float64
	// Critical names requests that matter most (SLA scopes, expanded).
	Critical map[string]bool
	// TxOf maps a request name to its enclosing transaction.
	TxOf map[string]string
	// ChangePoints are second offsets where the run's error rate or latency
	// shifted (see ChangePoints). A cluster that begins at one is ranked higher.
	ChangePoints []float64
	// GeneratorBusy[i] is true when the load generator's CPU was saturated in
	// second i of the run.
	GeneratorBusy []bool
	// MaxTemplates caps distinct templates (default 256); MaxClusters caps
	// distinct clusters (default 1000). Overflow is counted in "other".
	MaxTemplates, MaxClusters int
	// Sim is the Drain similarity threshold (default 0.7).
	Sim float64
}

// PriorityParts shows how a cluster's 0-100 priority was computed.
type PriorityParts struct {
	Share       float64 `json:"share"`       // 0..50: 50 x share of all failures
	Early       float64 `json:"early"`       // 0..15: 15 x (1 - firstSeen / duration)
	ChangePoint float64 `json:"changePoint"` // 20 when the cluster began at a change point
	Critical    float64 `json:"critical"`    // 15 when it hit a critical request/transaction
}

type Example struct {
	Message   string  `json:"message"`
	URL       string  `json:"url,omitempty"`
	Status    int     `json:"status"`
	OffsetSec float64 `json:"offsetSec"`
}

type Cluster struct {
	Rank               int           `json:"rank"`
	ID                 string        `json:"id"`
	Template           string        `json:"template"`
	ErrKind            string        `json:"errorKind"`
	Status             int           `json:"status"`
	Request            string        `json:"request"`
	Transaction        string        `json:"transaction,omitempty"`
	Count              uint64        `json:"count"`
	Percent            float64       `json:"percent"` // of all failed requests
	FirstSeenSec       float64       `json:"firstSeenSec"`
	LastSeenSec        float64       `json:"lastSeenSec"`
	Example            Example       `json:"example"` // redacted
	Side               string        `json:"side"`
	Confidence         string        `json:"confidence"` // high | medium | low
	Reason             string        `json:"reason"`     // stable rule code
	Explanation        string        `json:"explanation"`
	NextCheck          string        `json:"nextCheck"`
	Priority           int           `json:"priority"` // 0..100
	Parts              PriorityParts `json:"priorityParts"`
	BeganAtChangePoint bool          `json:"beganAtChangePoint,omitempty"`
	Critical           bool          `json:"critical,omitempty"`
	AfterExtractionPct float64       `json:"afterExtractionFailurePercent,omitempty"`
	Spark              []int         `json:"timeline,omitempty"` // failures per 1/24 of the run

	afterExtract uint64
	rawLower     string
	first, last  int64
}

// Signal is run-level evidence that is not a failed request (for example
// dropped iterations or a saturated generator).
type Signal struct {
	Side string `json:"side"`
	Text string `json:"text"`
}

type Result struct {
	RunID          string            `json:"runId,omitempty"`
	TotalRequests  uint64            `json:"totalRequests"`
	TotalFailures  uint64            `json:"totalFailures"`
	TxFailures     map[string]uint64 `json:"transactionFailures,omitempty"`
	Templates      int               `json:"templates"`
	OverflowCount  uint64            `json:"overflowFailures,omitempty"` // failures counted in "other" because a cap was reached
	DurationSec    float64           `json:"durationSec"`
	ChangePoints   []float64         `json:"changePointsSec,omitempty"`
	SideCounts     map[string]uint64 `json:"sideCounts"`
	Signals        []Signal          `json:"signals,omitempty"`
	Clusters       []Cluster         `json:"clusters"`
	Coarse         bool              `json:"coarse,omitempty"` // built from summary counts only (no raw.csv)
	Note           string            `json:"note,omitempty"`
	TruncatedTo    int               `json:"truncatedTo,omitempty"`
	ClustersHidden int               `json:"clustersHidden,omitempty"`
}

// Top returns a copy of the result limited to the n highest-priority clusters.
func (r *Result) Top(n int) *Result {
	if n <= 0 || n >= len(r.Clusters) {
		return r
	}
	c := *r
	c.Clusters = append([]Cluster(nil), r.Clusters[:n]...)
	c.TruncatedTo = n
	c.ClustersHidden = len(r.Clusters) - n
	return &c
}

type colIdx struct{ ts, label, code, kind, ok, msg, url, cat, vu int }

func findCols(hdr []string) (colIdx, error) {
	c := colIdx{-1, -1, -1, -1, -1, -1, -1, -1, -1}
	for i, h := range hdr {
		switch strings.ToLower(strings.TrimPrefix(h, "\ufeff")) {
		case "timestamp":
			c.ts = i
		case "label":
			c.label = i
		case "responsecode":
			c.code = i
		case "responsemessage":
			c.kind = i
		case "success":
			c.ok = i
		case "failuremessage":
			c.msg = i
		case "url":
			c.url = i
		case "kind":
			c.cat = i
		case "threadname":
			c.vu = i
		}
	}
	if c.ts < 0 || c.label < 0 || c.ok < 0 {
		return c, errors.New("not a Load Meter raw samples CSV (need timeStamp, label and success columns)")
	}
	return c, nil
}

type ckey struct {
	g      int
	status int
	kind   string
	label  string
}

func get(rec []string, i int) string {
	if i >= 0 && i < len(rec) {
		return rec[i]
	}
	return ""
}

const sparkBuckets = 24

// Analyze streams a raw samples CSV and returns ranked failure clusters.
func Analyze(r io.Reader, opt Options) (*Result, error) {
	if opt.MaxTemplates <= 0 {
		opt.MaxTemplates = 256
	}
	if opt.MaxClusters <= 0 {
		opt.MaxClusters = 1000
	}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	cr.LazyQuotes = true
	hdr, err := cr.Read()
	if err != nil {
		if err == io.EOF {
			return nil, errors.New("raw samples CSV is empty")
		}
		return nil, err
	}
	col, err := findCols(hdr)
	if err != nil {
		return nil, err
	}
	dr := NewDrain(opt.MaxTemplates, opt.Sim)
	res := &Result{SideCounts: map[string]uint64{}, DurationSec: opt.DurationSec}
	clusters := map[ckey]*Cluster{}
	var order []*Cluster // creation order, for deterministic iteration
	var overflow *Cluster
	cache := map[string]*group{}
	lastExtract := map[string]int64{}
	var startMs int64
	haveStart := false
	if !opt.Start.IsZero() {
		startMs, haveStart = opt.Start.UnixMilli(), true
	}
	var maxOff int64

	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				continue // skip a malformed row, keep streaming
			}
			return nil, err
		}
		succ := get(rec, col.ok)
		isTx := col.cat >= 0 && get(rec, col.cat) == "transaction"
		if succ == "true" {
			if !isTx {
				res.TotalRequests++
			}
			continue
		}
		label := get(rec, col.label)
		if isTx {
			if res.TxFailures == nil {
				res.TxFailures = map[string]uint64{}
			}
			res.TxFailures[label]++
			continue
		}
		res.TotalRequests++
		res.TotalFailures++
		ts, _ := strconv.ParseInt(get(rec, col.ts), 10, 64)
		if !haveStart {
			startMs, haveStart = ts, true
		}
		off := ts - startMs
		if off < 0 {
			off = 0
		}
		if off > maxOff {
			maxOff = off
		}
		kind := get(rec, col.kind)
		status, _ := strconv.Atoi(get(rec, col.code))
		msg := get(rec, col.msg)
		g, ok := cache[msg]
		if !ok {
			g = dr.Add(Normalize(msg))
			if len(cache) < 4096 {
				cache[msg] = g
			}
		} else {
			g.count++
		}
		k := ckey{g.id, status, kind, label}
		c := clusters[k]
		if c == nil {
			if len(clusters) >= opt.MaxClusters {
				if overflow == nil {
					overflow = &Cluster{ID: "other", Template: "<other: cluster limit reached>", ErrKind: "other", Request: "(various)", first: off, rawLower: ""}
				}
				res.OverflowCount++
				overflow.Count++
				overflow.last = off
				continue
			}
			c = &Cluster{ErrKind: kind, Status: status, Request: label, first: off,
				Template: "", rawLower: strings.ToLower(msg)}
			c.Example = Example{Message: clip(redact.PII(msg), 300), URL: clip(redact.URL(get(rec, col.url)), 200), Status: status, OffsetSec: sec1(off)}
			c.ID = clusterID(g, status, kind, label)
			c.Template = "\x00" + strconv.Itoa(g.id) // resolved after the pass (templates keep generalising)
			if opt.DurationSec > 0 {
				c.Spark = make([]int, sparkBuckets)
			}
			clusters[k] = c
			order = append(order, c)
		}
		c.Count++
		c.last = off
		if c.Spark != nil {
			b := int(float64(off) / 1000 / opt.DurationSec * sparkBuckets)
			c.Spark[min(max(b, 0), sparkBuckets-1)]++
		}
		if col.vu >= 0 && (kind == "extraction" || kind == "http_4xx" || kind == "assertion") {
			vu := rec[col.vu]
			if kind == "extraction" {
				if len(lastExtract) < 100000 || lastExtract[vu] != 0 {
					lastExtract[vu] = off + 1 // +1 so a failure at offset 0 is distinguishable from "none"
				}
			} else if t := lastExtract[vu]; t != 0 && off+1 >= t && off+1-t <= 10000 {
				c.afterExtract++
			}
		}
	}
	if overflow != nil {
		order = append(order, overflow)
	}
	res.Templates = len(dr.Groups())
	byID := dr.Groups()
	for _, c := range order {
		if strings.HasPrefix(c.Template, "\x00") {
			id, _ := strconv.Atoi(c.Template[1:])
			switch {
			case id < 0:
				c.Template = OtherTemplate
			default:
				c.Template = byID[id].Template()
			}
		}
	}
	dur := opt.DurationSec
	if dur <= 0 {
		dur = float64(maxOff) / 1000
	}
	res.DurationSec = math.Round(dur*10) / 10
	res.ChangePoints = opt.ChangePoints
	finish(res, order, opt, dur)
	return res, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func sec1(ms int64) float64 { return math.Round(float64(ms)/100) / 10 }

func clusterID(g *group, status int, kind, label string) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%d|%s|%s", g.Template(), status, kind, label)
	return fmt.Sprintf("%08x", h.Sum32())
}

// finish classifies, scores and ranks clusters, then fills the result.
func finish(res *Result, order []*Cluster, opt Options, dur float64) {
	ctx := classifyCtx{extractFirst: -1, busy: opt.GeneratorBusy}
	for _, c := range order {
		if c.ErrKind == "extraction" && (ctx.extractFirst < 0 || c.first < ctx.extractFirst) {
			ctx.extractFirst = c.first
		}
	}
	total := float64(res.TotalFailures)
	for _, c := range order {
		c.FirstSeenSec, c.LastSeenSec = sec1(c.first), sec1(c.last)
		if total > 0 {
			c.Percent = math.Round(float64(c.Count)/total*10000) / 100
			c.AfterExtractionPct = math.Round(float64(c.afterExtract)/float64(c.Count)*10000) / 100
		}
		c.Transaction = opt.TxOf[c.Request]
		c.Critical = opt.Critical[c.Request]
		for _, cp := range opt.ChangePoints {
			if c.FirstSeenSec >= cp-2 && c.FirstSeenSec <= cp+math.Max(5, 0.05*dur) {
				c.BeganAtChangePoint = true
			}
		}
		v := classify(c, &ctx)
		c.Side, c.Confidence, c.Reason, c.Explanation, c.NextCheck = v.side, v.conf, v.reason, v.explain, v.next
		p := PriorityParts{}
		if total > 0 {
			p.Share = 50 * float64(c.Count) / total
		}
		if dur > 0 {
			p.Early = 15 * math.Min(1, math.Max(0, 1-float64(c.first)/1000/dur))
		}
		if c.BeganAtChangePoint {
			p.ChangePoint = 20
		}
		if c.Critical {
			p.Critical = 15
		}
		p.Share, p.Early = round1(p.Share), round1(p.Early)
		c.Parts = p
		c.Priority = int(math.Round(p.Share + p.Early + p.ChangePoint + p.Critical))
		res.SideCounts[c.Side] += c.Count
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Template != b.Template {
			return a.Template < b.Template
		}
		return a.ID < b.ID
	})
	res.Clusters = make([]Cluster, len(order))
	for i, c := range order {
		c.Rank = i + 1
		res.Clusters[i] = *c
	}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
