package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// This file supports distributed execution: an agent's collector tracks what
// changed since the last TakeDelta, the delta travels as JSON (histograms are
// serialized bucket by bucket), and the controller merges every node's deltas
// into one Collector with MergeDelta. Histograms are always merged bucket-wise,
// never averaged, so merged percentiles equal the percentiles of the combined
// samples (within the histogram's bucket error).

func (w *Window) add(x *Sample) {
	us := uint64(x.Latency.Microseconds())
	if x.Kind == KindRequest {
		w.Requests++
		if !x.OK {
			w.Errors++
		}
		w.Hist.Record(us)
		w.SumUS += float64(us)
		w.MaxUS = max(w.MaxUS, us)
		w.BytesIn += x.BytesIn
	} else {
		w.Transactions++
		if !x.OK {
			w.TxErrors++
		}
	}
}

type deltaAgg struct {
	reqAll, txAll             *Stats
	byName                    map[string]*Stats
	windows                   map[int]*Window
	iters, iterFails, dropped uint64
}

func newDeltaAgg() *deltaAgg {
	return &deltaAgg{reqAll: newStats(), txAll: newStats(), byName: map[string]*Stats{}, windows: map[int]*Window{}}
}

func (d *deltaAgg) win(sec, vus int) *Window {
	w := d.windows[sec]
	if w == nil {
		w = &Window{T: sec, Hist: Sparse{}, ActiveVUs: vus}
		d.windows[sec] = w
	}
	return w
}

func (d *deltaAgg) add(x *Sample, sec, vus int) {
	key := x.Kind + "\x00" + x.Name
	st := d.byName[key]
	if st == nil {
		st = newStats()
		d.byName[key] = st
	}
	st.add(x)
	if x.Kind == KindRequest {
		d.reqAll.add(x)
	} else {
		d.txAll.add(x)
	}
	d.win(sec, vus).add(x)
}

// ---------- Histogram serialization ----------

type histWire struct {
	Total   uint64   `json:"n"`
	Sum     float64  `json:"sum"`
	Min     uint64   `json:"min"`
	Max     uint64   `json:"max"`
	Buckets []uint64 `json:"b,omitempty"` // flattened [index, count, index, count, ...]
}

// MarshalJSON writes the non-empty buckets only.
func (h *Histogram) MarshalJSON() ([]byte, error) {
	w := histWire{Total: h.Total, Sum: h.Sum, Min: h.Min, Max: h.Max}
	if h.Total == 0 {
		w.Min = 0
	}
	for i, c := range h.counts {
		if c > 0 {
			w.Buckets = append(w.Buckets, uint64(i), c)
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON validates bucket indexes and that bucket counts add up to Total.
func (h *Histogram) UnmarshalJSON(b []byte) error {
	var w histWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if len(w.Buckets)%2 != 0 {
		return fmt.Errorf("histogram: odd bucket list")
	}
	counts := make([]uint64, nBuckets)
	var total uint64
	for i := 0; i < len(w.Buckets); i += 2 {
		idx, c := w.Buckets[i], w.Buckets[i+1]
		if idx >= nBuckets {
			return fmt.Errorf("histogram: bucket index %d out of range", idx)
		}
		counts[idx] += c
		total += c
	}
	if total != w.Total {
		return fmt.Errorf("histogram: bucket counts %d do not match total %d", total, w.Total)
	}
	h.counts, h.Total, h.Sum, h.Min, h.Max = counts, w.Total, w.Sum, w.Min, w.Max
	if w.Total == 0 {
		h.Min = math.MaxUint64
	}
	return nil
}

// ---------- Delta ----------

// StatsDelta is the wire form of one Stats aggregate.
type StatsDelta struct {
	Kind     string            `json:"kind,omitempty"`
	Name     string            `json:"name,omitempty"`
	Count    uint64            `json:"count"`
	Errors   uint64            `json:"errors"`
	Hist     *Histogram        `json:"hist"`
	Status   map[int]uint64    `json:"status,omitempty"`
	Errs     map[string]uint64 `json:"errs,omitempty"`
	BytesIn  int64             `json:"bytesIn"`
	BytesOut int64             `json:"bytesOut"`
}

// Delta is everything a node recorded since its previous delta.
type Delta struct {
	Requests     *StatsDelta  `json:"requests,omitempty"`
	Transactions *StatsDelta  `json:"transactions,omitempty"`
	ByName       []StatsDelta `json:"byName,omitempty"`
	Windows      []*Window    `json:"windows,omitempty"`
	Iters        uint64       `json:"iters"`
	IterFails    uint64       `json:"iterFails"`
	Dropped      uint64       `json:"dropped"`
	ActiveVUs    int          `json:"activeVUs"`
}

// Empty reports whether the delta carries no samples or counters.
func (d *Delta) Empty() bool {
	return d.Requests == nil && d.Transactions == nil && len(d.ByName) == 0 && len(d.Windows) == 0 &&
		d.Iters == 0 && d.IterFails == 0 && d.Dropped == 0
}

func statsWire(kind, name string, s *Stats) *StatsDelta {
	if s.Count == 0 {
		return nil
	}
	return &StatsDelta{Kind: kind, Name: name, Count: s.Count, Errors: s.Errors, Hist: s.Hist, Status: s.Status, Errs: s.Errs,
		BytesIn: s.BytesIn, BytesOut: s.BytesOut}
}

// TrackDeltas makes the collector remember changes for TakeDelta. Call it
// before any sample is recorded.
func (c *Collector) TrackDeltas() {
	c.mu.Lock()
	if c.delta == nil {
		c.delta = newDeltaAgg()
	}
	c.mu.Unlock()
}

// TakeDelta returns the changes since the previous call and resets them.
// It returns nil when TrackDeltas was not called.
func (c *Collector) TakeDelta() *Delta {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.delta
	if d == nil {
		return nil
	}
	c.delta = newDeltaAgg()
	out := &Delta{Requests: statsWire("", "", d.reqAll), Transactions: statsWire("", "", d.txAll),
		Iters: d.iters, IterFails: d.iterFails, Dropped: d.dropped, ActiveVUs: c.activeVUs}
	keys := make([]string, 0, len(d.byName))
	for k := range d.byName {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kind, name := splitKey(k)
		if sw := statsWire(kind, name, d.byName[k]); sw != nil {
			out.ByName = append(out.ByName, *sw)
		}
	}
	for _, w := range d.windows {
		out.Windows = append(out.Windows, w)
	}
	sort.Slice(out.Windows, func(i, j int) bool { return out.Windows[i].T < out.Windows[j].T })
	return out
}

func mergeStats(dst *Stats, s *StatsDelta) {
	dst.Count += s.Count
	dst.Errors += s.Errors
	if s.Hist != nil && s.Hist.counts != nil {
		dst.Hist.Merge(s.Hist)
	}
	for k, v := range s.Status {
		dst.Status[k] += v
	}
	for k, v := range s.Errs {
		dst.Errs[k] += v
	}
	dst.BytesIn += s.BytesIn
	dst.BytesOut += s.BytesOut
}

// maxMergedWindows bounds the timeline a remote node can make us allocate.
const maxMergedWindows = 7 * 24 * 3600

type nodeGauge struct {
	vus      int
	cpu, mem float64
}

// MergeDelta adds a node's delta to this collector. Per-second windows are
// aligned by index (each node's second 0 is the synchronized start). Active
// VUs are summed across nodes; generator CPU and memory are the maximum of
// any node (the busiest generator).
func (c *Collector) MergeDelta(node string, d *Delta) error {
	if d == nil {
		return nil
	}
	for _, w := range d.Windows {
		if w == nil || w.T < 0 || w.T >= maxMergedWindows {
			return fmt.Errorf("delta window index out of range")
		}
		for k := range w.Hist {
			if int(k) >= nBuckets {
				return fmt.Errorf("delta window bucket out of range")
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.Requests != nil {
		mergeStats(c.reqAll, d.Requests)
	}
	if d.Transactions != nil {
		mergeStats(c.txAll, d.Transactions)
	}
	for i := range d.ByName {
		s := &d.ByName[i]
		key := s.Kind + "\x00" + s.Name
		st := c.byName[key]
		if st == nil {
			st = newStats()
			c.byName[key] = st
		}
		mergeStats(st, s)
	}
	for _, w := range d.Windows {
		for len(c.windows) <= w.T {
			c.windows = append(c.windows, &Window{T: len(c.windows), Hist: Sparse{}})
		}
		cw := c.windows[w.T]
		cw.Requests += w.Requests
		cw.Errors += w.Errors
		cw.Transactions += w.Transactions
		cw.TxErrors += w.TxErrors
		for k, v := range w.Hist {
			cw.Hist[k] += v
		}
		cw.SumUS += w.SumUS
		cw.MaxUS = max(cw.MaxUS, w.MaxUS)
		cw.BytesIn += w.BytesIn
		if cw.nodes == nil {
			cw.nodes = map[string]nodeGauge{}
		}
		g := cw.nodes[node]
		g.vus, g.cpu, g.mem = max(g.vus, w.ActiveVUs), max(g.cpu, w.CPUPercent), max(g.mem, w.MemMB)
		cw.nodes[node] = g
		cw.ActiveVUs, cw.CPUPercent, cw.MemMB = 0, 0, 0
		for _, g := range cw.nodes {
			cw.ActiveVUs += g.vus
			cw.CPUPercent = max(cw.CPUPercent, g.cpu)
			cw.MemMB = max(cw.MemMB, g.mem)
		}
	}
	c.Iters += d.Iters
	c.IterFails += d.IterFails
	c.Dropped += d.Dropped
	c.setNodeActiveLocked(node, d.ActiveVUs)
	return nil
}

// SetNodeActive records the current active VUs of a node (0 when it ended or disconnected).
func (c *Collector) SetNodeActive(node string, vus int) {
	c.mu.Lock()
	c.setNodeActiveLocked(node, vus)
	c.mu.Unlock()
}

func (c *Collector) setNodeActiveLocked(node string, vus int) {
	if c.nodeVUs == nil {
		c.nodeVUs = map[string]int{}
	}
	c.nodeVUs[node] = vus
	c.activeVUs = 0
	for _, v := range c.nodeVUs {
		c.activeVUs += v
	}
}

// Histogram returns a copy of the overall request (or transaction) histogram.
func (c *Collector) Histogram(kind string) *Histogram {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.reqAll
	if kind == KindTransaction {
		s = c.txAll
	}
	h := NewHistogram()
	h.Merge(s.Hist)
	return h
}
