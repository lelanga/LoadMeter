package har

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/loadmeter/internal/safety"
)

// Recorder is an http.RoundTripper that captures traffic as HAR 1.2. It backs
// the recording proxy (plain HTTP and, with a CA, HTTPS via local MITM) and is
// used to produce realistic test fixtures.
type Recorder struct {
	Next    http.RoundTripper
	MaxBody int64
	// CA enables HTTPS interception for CONNECT requests. Without it, CONNECT
	// is refused.
	CA *CA
	// Filter decides which hosts are recorded. Traffic to hosts it rejects is
	// still forwarded (HTTPS as a blind tunnel) but not stored.
	Filter *HostFilter

	mu      sync.Mutex
	entries []map[string]any
	page    string
	pages   []map[string]any
	recent  []Summary
	hosts   map[string]int
	conns   map[net.Conn]struct{}
	closed  bool
}

// Summary is a body-free description of one captured request for live views.
type Summary struct {
	Seq         int    `json:"seq"`
	Time        string `json:"time"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	Transaction string `json:"transaction,omitempty"`
	DurationMs  int64  `json:"durationMs"`
}

func NewRecorder(next http.RoundTripper) *Recorder {
	if next == nil {
		next = http.DefaultTransport
	}
	return &Recorder{Next: next, MaxBody: 2 << 20}
}

// Page starts a new HAR page (used as a transaction boundary on import).
func (r *Recorder) Page(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.page = "page_" + itoa(len(r.pages)+1)
	r.pages = append(r.pages, map[string]any{"id": r.page, "title": title, "startedDateTime": time.Now().UTC().Format(time.RFC3339Nano), "pageTimings": map[string]any{}})
}

// EndPage ends the current transaction; later entries carry no pageref and are
// grouped by think-time gaps on import.
func (r *Recorder) EndPage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.page = ""
}

// CurrentPage returns the title of the open transaction ("" when none).
func (r *Recorder) CurrentPage() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.page == "" {
		return ""
	}
	for _, p := range r.pages {
		if p["id"] == r.page {
			s, _ := p["title"].(string)
			return s
		}
	}
	return ""
}

// Count returns the number of recorded entries.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Recent returns summaries with Seq > since (at most the last 500 are kept).
func (r *Recorder) Recent(since int) []Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Summary{}
	for _, s := range r.recent {
		if s.Seq > since {
			out = append(out, s)
		}
	}
	return out
}

// Hosts returns the recorded hosts (no paths, no bodies), sorted by name.
func (r *Recorder) Hosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.hosts))
	for h := range r.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func nvs(h http.Header) []map[string]string {
	out := []map[string]string{}
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, map[string]string{"name": k, "value": v})
		}
	}
	return out
}

// readPrefix reads up to max bytes of rc and returns them plus a reader that
// yields the complete stream (prefix + remainder), so large bodies are passed
// through untruncated while only the prefix is recorded.
func readPrefix(rc io.ReadCloser, max int64) ([]byte, io.ReadCloser) {
	b, _ := io.ReadAll(io.LimitReader(rc, max))
	return b, struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), rc), rc}
}

func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	var reqBody []byte
	if req.Body != nil && req.Body != http.NoBody {
		reqBody, req.Body = readPrefix(req.Body, r.MaxBody)
	}
	resp, err := r.Next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if r.Filter != nil && !r.Filter.Allow(req.URL) {
		return resp, nil
	}
	var body []byte
	body, resp.Body = readPrefix(resp.Body, r.MaxBody)
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	qs := []map[string]string{}
	for k, vs := range req.URL.Query() {
		for _, v := range vs {
			qs = append(qs, map[string]string{"name": k, "value": v})
		}
	}
	hdr := req.Header.Clone()
	if req.Host != "" {
		hdr.Set("Host", req.Host)
	}
	e := map[string]any{
		"startedDateTime": start.UTC().Format(time.RFC3339Nano), "time": elapsed,
		"request": map[string]any{"method": req.Method, "url": req.URL.String(), "httpVersion": "HTTP/1.1", "headers": nvs(hdr),
			"queryString": qs, "cookies": []any{}, "headersSize": -1, "bodySize": len(reqBody)},
		"response": map[string]any{"status": resp.StatusCode, "statusText": http.StatusText(resp.StatusCode), "httpVersion": "HTTP/1.1",
			"headers": nvs(resp.Header), "cookies": []any{}, "redirectURL": resp.Header.Get("Location"), "headersSize": -1, "bodySize": len(body),
			"content": map[string]any{"size": len(body), "mimeType": resp.Header.Get("Content-Type"), "text": string(body)}},
		"cache": map[string]any{}, "timings": map[string]any{"send": 0, "wait": elapsed, "receive": 0},
	}
	if len(reqBody) > 0 {
		e["request"].(map[string]any)["postData"] = map[string]any{"mimeType": req.Header.Get("Content-Type"), "text": string(reqBody)}
	}
	r.mu.Lock()
	tx := ""
	if r.page != "" {
		e["pageref"] = r.page
		for _, p := range r.pages {
			if p["id"] == r.page {
				tx, _ = p["title"].(string)
			}
		}
	}
	r.entries = append(r.entries, e)
	if r.hosts == nil {
		r.hosts = map[string]int{}
	}
	r.hosts[req.URL.Hostname()]++
	u := *req.URL
	u.RawQuery, u.User = "", nil // live view: no query strings (may hold tokens)
	r.recent = append(r.recent, Summary{Seq: len(r.entries), Time: start.Format("15:04:05"), Method: req.Method, URL: u.String(),
		Status: resp.StatusCode, Transaction: tx, DurationMs: time.Since(start).Milliseconds()})
	if len(r.recent) > 500 {
		r.recent = append([]Summary(nil), r.recent[len(r.recent)-500:]...)
	}
	r.mu.Unlock()
	return resp, nil
}

// HAR returns the captured traffic as a HAR document.
func (r *Recorder) HAR() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	pages, entries := r.pages, r.entries
	if pages == nil {
		pages = []map[string]any{}
	}
	if entries == nil {
		entries = []map[string]any{}
	}
	doc := map[string]any{"log": map[string]any{"version": "1.2", "creator": map[string]string{"name": "Load Meter Recorder", "version": "0.2"},
		"pages": pages, "entries": entries}}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return b
}

// Save writes the HAR with owner-only permissions (recordings contain secrets).
func (r *Recorder) Save(path string) error {
	if err := os.WriteFile(path, r.HAR(), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

var hopHeaders = []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// clientIsLoopback reports whether the proxy client connected from loopback.
func clientIsLoopback(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	return safety.IsLoopback(h)
}

// Proxy returns a forward-proxy handler that records traffic. Plain HTTP is
// always supported; HTTPS (CONNECT) is intercepted when r.CA is set and
// refused otherwise. Clients that do not connect from loopback are refused.
func (r *Recorder) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !clientIsLoopback(req.RemoteAddr) {
			http.Error(w, "the recording proxy only accepts connections from this machine", http.StatusForbidden)
			return
		}
		if req.Method == http.MethodConnect {
			if r.CA == nil {
				http.Error(w, "HTTPS recording needs the recording CA: use 'loadmeter record' (which enables HTTPS interception) or export a HAR from your browser", http.StatusNotImplemented)
				return
			}
			r.handleConnect(w, req)
			return
		}
		if !req.URL.IsAbs() {
			http.Error(w, "configure this address as your HTTP proxy (absolute request URIs expected)", http.StatusBadRequest)
			return
		}
		r.forward(w, req, req.URL)
	})
}

// forward sends req to target (absolute URL) through the recorder and copies
// the response back.
func (r *Recorder) forward(w http.ResponseWriter, req *http.Request, target *url.URL) {
	out := req.Clone(req.Context())
	out.RequestURI = ""
	out.URL = target
	if out.Host == "" {
		out.Host = target.Host
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	// Let the transport negotiate gzip and decompress, so recorded bodies are
	// readable text (browsers otherwise ask for br/zstd).
	out.Header.Del("Accept-Encoding")
	if req.ContentLength == 0 {
		out.Body = nil
	}
	resp, err := r.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// HostFilter selects recorded hosts. Include (if non-empty) is an allowlist;
// Exclude always wins. Entries are exact hosts or "*.suffix"; DefaultExcludes
// adds the analytics/telemetry list used by Import plus browser background
// services.
type HostFilter struct {
	Include        []string
	Exclude        []string
	DefaultExclude bool
}

var browserBackground = []string{"clients2.google.com", "clients1.google.com", "update.googleapis.com", "safebrowsing.googleapis.com",
	"optimizationguide-pa.googleapis.com", "content-autofill.googleapis.com", "edgedl.me.gvt1.com", "redirector.gvt1.com",
	"accounts.google.com/ListAccounts", "www.google.com/complete", "android.clients.google.com",
	"msedge.api.cdp.microsoft.com", "edge.microsoft.com", "config.edge.skype.com", "self.events.data.microsoft.com"}

func hostMatch(pattern, host string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	h := strings.ToLower(host)
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "*.") {
		return strings.HasSuffix(h, p[1:]) || h == p[2:]
	}
	return p == h
}

// Allow reports whether a request to u should be recorded.
func (f *HostFilter) Allow(u *url.URL) bool {
	if f == nil {
		return true
	}
	host := u.Hostname()
	for _, x := range f.Exclude {
		if hostMatch(x, host) {
			return false
		}
	}
	if f.DefaultExclude {
		if isAnalytics(u) {
			return false
		}
		s := strings.ToLower(u.Host + u.Path)
		for _, b := range browserBackground {
			if b = strings.ToLower(b); strings.HasPrefix(s, b) || strings.HasPrefix(strings.ToLower(host), b) {
				return false
			}
		}
	}
	if len(f.Include) == 0 {
		return true
	}
	for _, x := range f.Include {
		if hostMatch(x, host) {
			return true
		}
	}
	return false
}

// AllowHost is Allow for a bare host (used to decide whether a CONNECT tunnel
// is intercepted or passed through untouched).
func (f *HostFilter) AllowHost(host string) bool {
	return f.Allow(&url.URL{Scheme: "https", Host: host, Path: "/"})
}
