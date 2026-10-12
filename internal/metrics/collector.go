package metrics

import (
	"bufio"
	"encoding/csv"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	KindRequest     = "request"
	KindTransaction = "transaction"
)

// Sample is one measured request or transaction.
type Sample struct {
	Kind      string
	Name      string
	URL       string
	Start     time.Time
	Latency   time.Duration // full time (request: until body read)
	TTFB      time.Duration
	Status    int
	OK        bool
	ErrKind   string // "", "connect", "timeout", "http_5xx", "http_4xx", "assertion", "extraction", "other"
	ErrMsg    string
	BytesIn   int64
	BytesOut  int64
	VU        int
	Iteration int
}

type Stats struct {
	Count    uint64
	Errors   uint64
	Hist     *Histogram
	Status   map[int]uint64
	Errs     map[string]uint64
	BytesIn  int64
	BytesOut int64
}

func newStats() *Stats {
	return &Stats{Hist: NewHistogram(), Status: map[int]uint64{}, Errs: map[string]uint64{}}
}

func (s *Stats) add(x *Sample) {
	s.Count++
	s.Hist.Record(uint64(x.Latency.Microseconds()))
	if !x.OK {
		s.Errors++
		s.Errs[x.ErrKind]++
	}
	if x.Kind == KindRequest {
		s.Status[x.Status]++
	}
	s.BytesIn += x.BytesIn
	s.BytesOut += x.BytesOut
}

// Window is one second of activity.
type Window struct {
	T            int // seconds since start
	Requests     uint64
	Errors       uint64
	Transactions uint64
	TxErrors     uint64
	Hist         Sparse
	SumUS        float64
	MaxUS        uint64
	BytesIn      int64
	ActiveVUs    int
	CPUPercent   float64
	MemMB        float64
	nodes        map[string]nodeGauge // merged collectors only
}

// Collector aggregates samples; safe for concurrent use.
type Collector struct {
	mu        sync.Mutex
	start     time.Time
	reqAll    *Stats
	txAll     *Stats
	byName    map[string]*Stats // key: kind + "\x00" + name
	windows   []*Window
	raw       *csv.Writer
	rawBuf    *bufio.Writer
	activeVUs int
	cpu, mem  float64
	Dropped   uint64 // arrival-rate iterations that could not start (no free VU)
	Iters     uint64
	IterFails uint64
	delta     *deltaAgg      // non-nil when TrackDeltas was called (distributed agents)
	nodeVUs   map[string]int // merged collectors: latest active VUs per node
}

func NewCollector(start time.Time, raw io.Writer) *Collector {
	c := &Collector{start: start, reqAll: newStats(), txAll: newStats(), byName: map[string]*Stats{}}
	if raw != nil {
		c.rawBuf = bufio.NewWriterSize(raw, 64<<10)
		c.raw = csv.NewWriter(c.rawBuf)
		// JMeter JTL-compatible leading columns, plus Phoenix extras.
		_ = c.raw.Write([]string{"timeStamp", "elapsed", "label", "responseCode", "responseMessage", "threadName",
			"success", "failureMessage", "bytes", "sentBytes", "Latency", "URL", "kind"})
	}
	return c
}

func (c *Collector) sec(t time.Time) int {
	sec := int(t.Sub(c.start) / time.Second)
	if sec < 0 {
		sec = 0
	}
	return sec
}

func (c *Collector) window(t time.Time) *Window {
	return c.windowAt(c.sec(t))
}

func (c *Collector) windowAt(sec int) *Window {
	for len(c.windows) <= sec {
		c.windows = append(c.windows, &Window{T: len(c.windows), Hist: Sparse{}, ActiveVUs: c.activeVUs})
	}
	return c.windows[sec]
}

func (c *Collector) Add(x Sample) {
	end := x.Start.Add(x.Latency)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := x.Kind + "\x00" + x.Name
	st := c.byName[key]
	if st == nil {
		st = newStats()
		c.byName[key] = st
	}
	st.add(&x)
	w := c.window(end)
	if x.Kind == KindRequest {
		c.reqAll.add(&x)
	} else {
		c.txAll.add(&x)
	}
	w.add(&x)
	if c.delta != nil {
		c.delta.add(&x, c.sec(end), c.activeVUs)
	}
	if c.raw != nil {
		msg := x.ErrMsg
		_ = c.raw.Write([]string{
			strconv.FormatInt(x.Start.UnixMilli(), 10), strconv.FormatInt(x.Latency.Milliseconds(), 10), x.Name,
			strconv.Itoa(x.Status), x.ErrKind, "vu-" + strconv.Itoa(x.VU), strconv.FormatBool(x.OK), msg,
			strconv.FormatInt(x.BytesIn, 10), strconv.FormatInt(x.BytesOut, 10),
			strconv.FormatInt(x.TTFB.Milliseconds(), 10), x.URL, x.Kind})
	}
}

// SetGauge records active VUs and generator resource usage for the current second.
func (c *Collector) SetGauge(now time.Time, vus int, cpu, memMB float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeVUs, c.cpu, c.mem = vus, cpu, memMB
	w := c.window(now)
	w.ActiveVUs = max(w.ActiveVUs, vus)
	w.CPUPercent = max(w.CPUPercent, cpu)
	w.MemMB = max(w.MemMB, memMB)
	if c.delta != nil {
		dw := c.delta.win(c.sec(now), vus)
		dw.ActiveVUs = max(dw.ActiveVUs, vus)
		dw.CPUPercent = max(dw.CPUPercent, cpu)
		dw.MemMB = max(dw.MemMB, memMB)
	}
}

func (c *Collector) IterationDone(ok bool) {
	c.mu.Lock()
	c.Iters++
	if !ok {
		c.IterFails++
	}
	if c.delta != nil {
		c.delta.iters++
		if !ok {
			c.delta.iterFails++
		}
	}
	c.mu.Unlock()
}

func (c *Collector) DroppedIteration() {
	c.mu.Lock()
	c.Dropped++
	if c.delta != nil {
		c.delta.dropped++
	}
	c.mu.Unlock()
}

func (c *Collector) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw != nil {
		c.raw.Flush()
		_ = c.rawBuf.Flush()
	}
}

// ---------- Output types ----------

type MetricSet struct {
	Count     uint64            `json:"count"`
	Errors    uint64            `json:"errors"`
	ErrorRate float64           `json:"errorRate"` // percent
	RPS       float64           `json:"rps"`
	MinMs     float64           `json:"minMs"`
	AvgMs     float64           `json:"avgMs"`
	P50Ms     float64           `json:"p50Ms"`
	P90Ms     float64           `json:"p90Ms"`
	P95Ms     float64           `json:"p95Ms"`
	P99Ms     float64           `json:"p99Ms"`
	MaxMs     float64           `json:"maxMs"`
	BytesIn   int64             `json:"bytesIn"`
	BytesOut  int64             `json:"bytesOut"`
	Status    map[string]uint64 `json:"status,omitempty"`
	ErrorKind map[string]uint64 `json:"errorKinds,omitempty"`
}

type Named struct {
	Name string `json:"name"`
	MetricSet
}

type WindowOut struct {
	T         int     `json:"t"`
	RPS       float64 `json:"rps"`
	TPS       float64 `json:"tps"`
	Errors    uint64  `json:"errors"`
	ErrorRate float64 `json:"errorRate"`
	AvgMs     float64 `json:"avgMs"`
	P50Ms     float64 `json:"p50Ms"`
	P95Ms     float64 `json:"p95Ms"`
	P99Ms     float64 `json:"p99Ms"`
	MaxMs     float64 `json:"maxMs"`
	VUs       int     `json:"vus"`
	BytesIn   int64   `json:"bytesIn"`
	CPU       float64 `json:"cpuPercent"`
	MemMB     float64 `json:"memMB"`
}

type Snapshot struct {
	ElapsedSec   float64     `json:"elapsedSec"`
	Requests     MetricSet   `json:"requests"`
	Transactions MetricSet   `json:"transactions"`
	ByRequest    []Named     `json:"byRequest"`
	ByTx         []Named     `json:"byTransaction"`
	Timeline     []WindowOut `json:"timeline"`
	Iterations   uint64      `json:"iterations"`
	IterFailures uint64      `json:"iterationFailures"`
	Dropped      uint64      `json:"droppedIterations"`
	ActiveVUs    int         `json:"activeVUs"`
}

func ms(us float64) float64 { return math.Round(us/10) / 100 }

func toSet(s *Stats, secs float64) MetricSet {
	m := MetricSet{Count: s.Count, Errors: s.Errors, BytesIn: s.BytesIn, BytesOut: s.BytesOut,
		Status: map[string]uint64{}, ErrorKind: map[string]uint64{}}
	if s.Count > 0 {
		m.ErrorRate = math.Round(float64(s.Errors)/float64(s.Count)*10000) / 100
		m.MinMs = ms(float64(s.Hist.Min))
		m.AvgMs = ms(s.Hist.Mean())
		m.P50Ms = ms(float64(s.Hist.Quantile(0.50)))
		m.P90Ms = ms(float64(s.Hist.Quantile(0.90)))
		m.P95Ms = ms(float64(s.Hist.Quantile(0.95)))
		m.P99Ms = ms(float64(s.Hist.Quantile(0.99)))
		m.MaxMs = ms(float64(s.Hist.Max))
	}
	if secs > 0 {
		m.RPS = math.Round(float64(s.Count)/secs*100) / 100
	}
	for k, v := range s.Status {
		m.Status[strconv.Itoa(k)] = v
	}
	for k, v := range s.Errs {
		if k != "" {
			m.ErrorKind[k] = v
		}
	}
	return m
}

// Snapshot computes current aggregates. fromWindow limits the timeline returned
// (use 0 for the full timeline).
func (c *Collector) Snapshot(now time.Time, fromWindow int) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	secs := now.Sub(c.start).Seconds()
	snap := Snapshot{ElapsedSec: math.Round(secs*10) / 10, Requests: toSet(c.reqAll, secs), Transactions: toSet(c.txAll, secs),
		Iterations: c.Iters, IterFailures: c.IterFails, Dropped: c.Dropped, ActiveVUs: c.activeVUs}
	keys := make([]string, 0, len(c.byName))
	for k := range c.byName {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kind, name := splitKey(k)
		n := Named{Name: name, MetricSet: toSet(c.byName[k], secs)}
		if kind == KindRequest {
			snap.ByRequest = append(snap.ByRequest, n)
		} else {
			snap.ByTx = append(snap.ByTx, n)
		}
	}
	for i := max(fromWindow, 0); i < len(c.windows); i++ {
		w := c.windows[i]
		o := WindowOut{T: w.T, RPS: float64(w.Requests), TPS: float64(w.Transactions), Errors: w.Errors, VUs: w.ActiveVUs,
			BytesIn: w.BytesIn, CPU: math.Round(w.CPUPercent*10) / 10, MemMB: math.Round(w.MemMB*10) / 10}
		if w.Requests > 0 {
			o.ErrorRate = math.Round(float64(w.Errors)/float64(w.Requests)*10000) / 100
			o.AvgMs = ms(w.SumUS / float64(w.Requests))
			o.P50Ms = ms(float64(w.Hist.Quantile(0.5)))
			o.P95Ms = ms(float64(w.Hist.Quantile(0.95)))
			o.P99Ms = ms(float64(w.Hist.Quantile(0.99)))
			o.MaxMs = ms(float64(w.MaxUS))
		}
		snap.Timeline = append(snap.Timeline, o)
	}
	return snap
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// Quantile exposes overall request quantiles in ms for SLA evaluation.
func (c *Collector) Quantile(kind, name string, q float64) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var s *Stats
	switch {
	case name == "" && kind == KindTransaction:
		s = c.txAll
	case name == "":
		s = c.reqAll
	default:
		s = c.byName[kind+"\x00"+name]
	}
	if s == nil || s.Count == 0 {
		return 0, false
	}
	return float64(s.Hist.Quantile(q)) / 1000, true
}
