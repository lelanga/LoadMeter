package accesslog

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/thinktime"
)

// Limits.
const (
	MaxInputBytes   = 200 << 20 // uncompressed (and compressed) bytes read
	maxLineBytes    = 64 << 10
	maxEndpoints    = 50000
	maxSessions     = 500000
	maxGapSamples   = 100000
	maxQueryVariant = 20
	maxSpanMinutes  = 100000
	detectLines     = 200
	// SessionIdle is the idle time that ends a reconstructed session.
	SessionIdle = 30 * time.Minute
	// MachineGap: gaps below this are request bursts (subresources), not think time.
	MachineGap = 300 * time.Millisecond
)

// Options controls parsing, selection and scenario generation.
type Options struct {
	Format        string // auto (default), nginx, apache, alb, cloudfront
	Name          string // scenario name
	BaseURL       string // replay target (required to generate)
	AllowHosts    []string
	Scale         float64 // fraction of the logged rate (default 0.1)
	Window        string  // peak (default), avg, or HH:MM-HH:MM
	PeakMinutes   int     // length of the "peak" window (default 15)
	Top           int     // endpoints kept in the mix (default 20)
	IncludeWrites bool    // POST/PUT/PATCH/DELETE (bodies are not in logs: sent empty)
	KeepQuery     bool
	IncludeStatic bool
	IncludeErrors bool   // replay requests that returned 4xx/5xx
	Journey       bool   // one iteration = one reconstructed session with fitted think time
	SessionCookie string // cookie name (CloudFront cs(Cookie)) used to identify sessions instead of IP+UA
	MaxBytes      int64  // input cap (default MaxInputBytes)
}

func (o *Options) defaults() {
	if o.Format == "" {
		o.Format = FormatAuto
	}
	if o.Scale == 0 {
		o.Scale = 0.1
	}
	if o.Window == "" {
		o.Window = "peak"
	}
	if o.PeakMinutes <= 0 {
		o.PeakMinutes = 15
	}
	if o.Top <= 0 {
		o.Top = 20
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = MaxInputBytes
	}
}

// Endpoint is one normalised method + path template.
type Endpoint struct {
	Method   string         `json:"method"`
	Path     string         `json:"path"`
	Count    int            `json:"count"` // replayable requests
	Seen     int            `json:"seen"`  // all requests incl. filtered statuses
	Share    float64        `json:"sharePercent"`
	Statuses map[string]int `json:"statuses,omitempty"`
	IDs      []IDSlot       `json:"ids,omitempty"`
	Query    string         `json:"query,omitempty"` // most common query (redacted); only with KeepQuery
	queries  map[string]int
}

// Key is "METHOD /path/{id}".
func (e *Endpoint) Key() string { return e.Method + " " + e.Path }

// RatePoint is one minute of the selected window (counts are per minute, unscaled).
type RatePoint struct {
	Minute   string  `json:"minute"` // log-local clock "2006-01-02 15:04" (HH:MM for multi-day averages)
	Requests float64 `json:"requests"`
}

// RateSummary describes the whole log.
type RateSummary struct {
	Minutes    int     `json:"minutes"` // span of the log in minutes
	AvgRPS     float64 `json:"avgRps"`
	PeakRPS    float64 `json:"peakRps"` // busiest single minute
	PeakMinute string  `json:"peakMinute,omitempty"`
}

// WindowInfo describes the selected replay window.
type WindowInfo struct {
	Mode    string  `json:"mode"` // peak | avg | range
	Spec    string  `json:"spec"`
	Start   string  `json:"start,omitempty"`
	Minutes int     `json:"minutes"`
	AvgRPS  float64 `json:"avgRps"` // unscaled, replayable requests per second in the window
	PeakRPS float64 `json:"peakRps"`
	Days    int     `json:"days,omitempty"` // days averaged for HH:MM-HH:MM windows
}

// SessionStats come from IP+user-agent (or cookie) session reconstruction.
type SessionStats struct {
	Sessions       int               `json:"sessions"`
	MeanRequests   float64           `json:"meanRequestsPerSession"`
	IdleMinutes    int               `json:"idleTimeoutMinutes"`
	Keyed          string            `json:"keyedBy"`
	Approximate    bool              `json:"approximate"`
	TrackingFull   bool              `json:"trackingTableFull,omitempty"`
	GapsCollected  int               `json:"gapsCollected"`
	ThinkFit       *thinktime.Result `json:"thinkFit,omitempty"`
	ThinkFitReason string            `json:"thinkFitNote,omitempty"`
}

// Profile is the analysis result. It never contains client addresses.
type Profile struct {
	Source         string         `json:"source"`
	Format         string         `json:"format"`
	Lines          int            `json:"lines"`
	Parsed         int            `json:"parsed"`
	Malformed      int            `json:"malformed"`
	FirstBad       []int          `json:"firstMalformedLines,omitempty"`
	Comments       int            `json:"commentLines,omitempty"`
	Truncated      bool           `json:"truncatedAtCap,omitempty"`
	ReadError      string         `json:"readError,omitempty"`
	First          string         `json:"first,omitempty"`
	Last           string         `json:"last,omitempty"`
	Methods        map[string]int `json:"methods"`
	Statuses       map[string]int `json:"statuses"`
	Hosts          map[string]int `json:"hosts,omitempty"`
	Ignored        map[string]int `json:"ignored"`
	Replayable     int            `json:"replayable"`
	Endpoints      []*Endpoint    `json:"endpoints"` // replayable, most frequent first
	Rate           RateSummary    `json:"rate"`
	Window         WindowInfo     `json:"window"`
	Curve          []RatePoint    `json:"curve,omitempty"`
	Sessions       SessionStats   `json:"sessions"`
	QueryCount     int            `json:"requestsWithQuery"`
	QueriesDropped int            `json:"queriesDropped,omitempty"`
	Gzip           bool           `json:"gzip,omitempty"`

	window []float64 // per-minute replayable counts for the selected window (nil for avg)
}

type sess struct {
	last int64 // unix milliseconds
	n    int32
}

type analyzer struct {
	o       Options
	p       *Profile
	eps     map[string]*Endpoint
	allMin  map[int64]int // all parsed requests per log-local minute
	repMin  map[int64]int // replayable requests
	sessMap map[uint64]*sess
	closed  int
	closedN int
	gaps    []float64
	gapSeen int
	rng     *rand.Rand
	first   time.Time
	last    time.Time
}

type capReader struct {
	r   io.Reader
	max int64
	n   int64
	hit bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.hit {
		return 0, io.EOF
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		n -= int(c.n - c.max)
		c.hit = true
		return n, io.EOF
	}
	return n, err
}

// Analyze streams the log (plain or gzip, sniffed by magic bytes) and builds a
// Profile. Malformed lines are counted, never fatal.
func Analyze(r io.Reader, source string, o Options) (*Profile, error) {
	o.defaults()
	a := &analyzer{o: o, eps: map[string]*Endpoint{}, allMin: map[int64]int{}, repMin: map[int64]int{},
		sessMap: map[uint64]*sess{}, rng: rand.New(rand.NewPCG(1, 1)),
		p: &Profile{Source: source, Methods: map[string]int{}, Statuses: map[string]int{}, Hosts: map[string]int{}, Ignored: map[string]int{}}}

	raw := &capReader{r: r, max: o.MaxBytes}
	br := bufio.NewReaderSize(raw, 64<<10)
	var src io.Reader = br
	var dec *capReader
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("gzip input is corrupt: %w", err)
		}
		a.p.Gzip = true
		dec = &capReader{r: gz, max: o.MaxBytes}
		src = dec
	}
	lr := bufio.NewReaderSize(src, 64<<10)

	var ps parser
	format := o.Format
	if format != FormatAuto {
		var err error
		if ps, err = newParser(format); err != nil {
			return nil, err
		}
	}
	unknown := 0
	lineNo := 0
	for {
		line, err := lr.ReadSlice('\n')
		tooLong := false
		if errors.Is(err, bufio.ErrBufferFull) {
			tooLong = true
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = lr.ReadSlice('\n')
			}
		}
		if len(line) > 0 || tooLong {
			lineNo++
			a.p.Lines++
			s := strings.TrimRight(string(line), "\r\n")
			switch {
			case tooLong:
				a.bad(lineNo)
			case strings.TrimSpace(s) == "":
				a.p.Lines--
			default:
				if ps == nil { // auto-detect
					f := detect(s)
					if f == "" {
						if strings.HasPrefix(s, "#") {
							a.p.Comments++
						} else {
							a.bad(lineNo)
							if unknown++; unknown >= detectLines {
								return nil, fmt.Errorf("could not detect the log format in the first %d lines; pass --format nginx|apache|alb|cloudfront", detectLines)
							}
						}
					} else {
						format = f
						ps, _ = newParser(f)
					}
				}
				if ps != nil {
					a.line(ps, s, lineNo)
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				a.p.ReadError = err.Error()
			}
			break
		}
	}
	a.p.Truncated = raw.hit || (dec != nil && dec.hit)
	if ps == nil {
		return nil, errors.New("no recognisable access-log lines found (formats: nginx/Apache combined or common, ALB, CloudFront)")
	}
	a.p.Format = format
	if a.p.Parsed == 0 {
		return nil, fmt.Errorf("no valid %s lines found (%d malformed)", format, a.p.Malformed)
	}
	if err := a.finish(); err != nil {
		return nil, err
	}
	return a.p, nil
}

func (a *analyzer) bad(lineNo int) {
	a.p.Malformed++
	if len(a.p.FirstBad) < 10 {
		a.p.FirstBad = append(a.p.FirstBad, lineNo)
	}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func minuteKey(t time.Time) int64 {
	_, off := t.Zone()
	return floorDiv(t.Unix()+int64(off), 60)
}

func isWrite(m string) bool {
	switch m {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

func (a *analyzer) line(ps parser, s string, lineNo int) {
	rec, skip, err := ps.parse(s)
	if skip {
		a.p.Comments++
		return
	}
	if err != nil {
		if strings.HasPrefix(s, "#") {
			a.p.Comments++
			return
		}
		a.bad(lineNo)
		return
	}
	p := a.p
	p.Parsed++
	p.Methods[rec.Method]++
	p.Statuses[fmt.Sprint(rec.Status)]++
	if rec.Host != "" && (len(p.Hosts) < 50 || p.Hosts[rec.Host] > 0) {
		p.Hosts[rec.Host]++
	}
	if a.first.IsZero() || rec.Time.Before(a.first) {
		a.first = rec.Time
	}
	if rec.Time.After(a.last) {
		a.last = rec.Time
	}
	mk := minuteKey(rec.Time)
	a.allMin[mk]++

	tmpl, query, ids := Normalize(rec.Target)
	static := isStatic(tmpl)
	if !static {
		a.session(rec)
	}
	switch {
	case strings.Contains(rec.Target, "${"):
		p.Ignored["paths containing template syntax ${...}"]++
		return
	case static && !a.o.IncludeStatic:
		p.Ignored["static assets"]++
		return
	case rec.Method == "OPTIONS":
		p.Ignored["OPTIONS (CORS preflight)"]++
		return
	case isWrite(rec.Method) && !a.o.IncludeWrites:
		p.Ignored["write requests ("+rec.Method+")"]++
		return
	case rec.Method != "GET" && rec.Method != "HEAD" && !isWrite(rec.Method):
		p.Ignored["unsupported method "+rec.Method]++
		return
	}
	key := rec.Method + " " + tmpl
	ep := a.eps[key]
	if ep == nil {
		if len(a.eps) >= maxEndpoints {
			p.Ignored["endpoints beyond the table limit"]++
			return
		}
		ep = &Endpoint{Method: rec.Method, Path: tmpl, Statuses: map[string]int{}}
		a.eps[key] = ep
	}
	ep.Seen++
	ep.Statuses[fmt.Sprint(rec.Status)]++
	if rec.Status >= 400 && !a.o.IncludeErrors {
		p.Ignored["requests that returned 4xx/5xx"]++
		return
	}
	ep.Count++
	p.Replayable++
	a.repMin[mk]++
	for _, id := range ids {
		merged := false
		for i := range ep.IDs {
			if ep.IDs[i].Index == id.Index {
				merged = true
				if id.Kind == IDNumeric && ep.IDs[i].Kind == IDNumeric {
					ep.IDs[i].Min, ep.IDs[i].Max = min(ep.IDs[i].Min, id.Min), max(ep.IDs[i].Max, id.Max)
				}
				if l := len(id.Example); l > ep.IDs[i].Len {
					ep.IDs[i].Len = l
				}
			}
		}
		if !merged {
			id.Len = len(id.Example)
			ep.IDs = append(ep.IDs, id)
		}
	}
	if query != "" {
		p.QueryCount++
		if a.o.KeepQuery {
			if ep.queries == nil {
				ep.queries = map[string]int{}
			}
			if _, ok := ep.queries[query]; ok || len(ep.queries) < maxQueryVariant {
				ep.queries[query]++
			}
		}
	}
}

func cookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == name {
			return v
		}
	}
	return ""
}

// session updates the session table and collects think-time gaps.
func (a *analyzer) session(rec Record) {
	var key uint64
	h := fnv.New64a()
	if a.o.SessionCookie != "" && rec.Cookie != "" {
		if v := cookieValue(rec.Cookie, a.o.SessionCookie); v != "" {
			h.Write([]byte("c\x00" + v))
		} else {
			h.Write([]byte(rec.Client + "\x00" + rec.UA))
		}
	} else {
		h.Write([]byte(rec.Client + "\x00" + rec.UA))
	}
	key = h.Sum64()
	ms := rec.Time.UnixMilli()
	s := a.sessMap[key]
	if s == nil {
		if len(a.sessMap) >= maxSessions {
			a.p.Sessions.TrackingFull = true
			return
		}
		a.sessMap[key] = &sess{last: ms, n: 1}
		return
	}
	gap := time.Duration(ms-s.last) * time.Millisecond
	switch {
	case gap > SessionIdle:
		a.closed++
		a.closedN += int(s.n)
		s.n = 1
		s.last = ms
		return
	case gap < 0:
		s.n++ // out-of-order line: count it, keep the later timestamp
		return
	}
	s.n++
	s.last = ms
	if gap >= MachineGap {
		a.gapSeen++
		v := gap.Seconds()
		if len(a.gaps) < maxGapSamples {
			a.gaps = append(a.gaps, v)
		} else if j := a.rng.IntN(a.gapSeen); j < maxGapSamples {
			a.gaps[j] = v
		}
	}
}

func (a *analyzer) finish() error {
	p := a.p
	p.First, p.Last = a.first.Format(time.RFC3339), a.last.Format(time.RFC3339)
	for _, e := range a.eps {
		if e.Count == 0 {
			continue
		}
		if len(e.queries) > 0 {
			// Raw queries never leave this package: redact now.
			if q, ok := safeQuery(bestQuery(e.queries)); ok {
				e.Query = q
			} else {
				p.QueriesDropped++
			}
		}
		p.Endpoints = append(p.Endpoints, e)
	}
	sort.Slice(p.Endpoints, func(i, j int) bool {
		if p.Endpoints[i].Count != p.Endpoints[j].Count {
			return p.Endpoints[i].Count > p.Endpoints[j].Count
		}
		return p.Endpoints[i].Key() < p.Endpoints[j].Key()
	})
	for _, e := range p.Endpoints {
		e.Share = round(100*float64(e.Count)/float64(p.Replayable), 2)
	}

	// sessions
	for _, s := range a.sessMap {
		a.closed++
		a.closedN += int(s.n)
	}
	ss := &p.Sessions
	ss.Sessions, ss.IdleMinutes, ss.Approximate = a.closed, int(SessionIdle/time.Minute), true
	ss.Keyed = "client IP + user agent (hashed, in memory only)"
	if a.o.SessionCookie != "" {
		ss.Keyed = "cookie " + a.o.SessionCookie + " when logged, else client IP + user agent"
	}
	if a.closed > 0 {
		ss.MeanRequests = round(float64(a.closedN)/float64(a.closed), 2)
	}
	ss.GapsCollected = len(a.gaps)
	if fit, err := thinktime.Fit(a.gaps); err == nil {
		ss.ThinkFit = fit
	} else {
		ss.ThinkFitReason = err.Error()
	}

	// rates
	if err := a.rates(); err != nil {
		return err
	}
	return nil
}

func round(v float64, d int) float64 {
	m := 1.0
	for i := 0; i < d; i++ {
		m *= 10
	}
	return float64(int64(v*m+0.5*sign(v))) / m
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func bestQuery(q map[string]int) string {
	best, n := "", -1
	for k, v := range q {
		if v > n || v == n && k < best {
			best, n = k, v
		}
	}
	return best
}
