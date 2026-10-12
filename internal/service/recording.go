package service

// Script recording sessions: a loopback-only recording proxy (HTTP + HTTPS via
// the local recording CA), an optional launched browser, transaction markers
// and a one-step finish into a scenario with correlation candidates.
//
// The safety allowlist does not apply while recording: the user browses
// manually. Recorded hosts must be allowlisted before a load test can target
// them; StopRecording reports which ones are not.

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/phoenix/internal/browser"
	"github.com/lelanga/phoenix/internal/har"
	"github.com/lelanga/phoenix/internal/safety"
)

// MaxRecordingDuration is the auto-stop limit for a recording session.
var MaxRecordingDuration = 2 * time.Hour

type RecordingOptions struct {
	Listen           string   `json:"listen,omitempty"` // default 127.0.0.1:0
	Browser          bool     `json:"browser"`
	StartURL         string   `json:"startUrl,omitempty"`
	IncludeHosts     []string `json:"includeHosts,omitempty"`
	ExcludeHosts     []string `json:"excludeHosts,omitempty"`
	InsecureUpstream bool     `json:"insecureUpstream,omitempty"`
	// UpstreamRootCAs replaces the system roots for upstream verification
	// (tests only; not settable over the API).
	UpstreamRootCAs *x509.CertPool `json:"-"`
	// CADir overrides the CA location (tests only).
	CADir string `json:"-"`
}

type RecordingInfo struct {
	ID            string        `json:"id"`
	ProxyAddr     string        `json:"proxyAddr"`
	CASPKI        string        `json:"caSPKI"`
	CACertPath    string        `json:"caCertPath"`
	CACreated     bool          `json:"caCreated,omitempty"`
	Browser       bool          `json:"browser"`
	BrowserPath   string        `json:"browserPath,omitempty"`
	BrowserExited bool          `json:"browserExited,omitempty"`
	Started       time.Time     `json:"started"`
	AutoStopAt    time.Time     `json:"autoStopAt"`
	Captured      int           `json:"captured"`
	Transaction   string        `json:"transaction,omitempty"`
	Requests      []har.Summary `json:"requests,omitempty"`
}

type RecordingResult struct {
	ID             string     `json:"id"`
	HARFile        string     `json:"harFile"`
	Captured       int        `json:"captured"`
	Hosts          []string   `json:"hosts"`
	NotAllowlisted []string   `json:"notAllowlisted,omitempty"`
	Import         *HARImport `json:"import,omitempty"`
	Note           string     `json:"note,omitempty"`
}

type recSession struct {
	id       string
	opts     RecordingOptions
	rec      *har.Recorder
	ca       *har.CA
	srv      *http.Server
	addr     string
	started  time.Time
	browser  *browser.Session
	bpath    string
	created  bool
	done     chan struct{} // closed on browser exit or auto-stop: callers should stop
	doneOnce sync.Once
	timer    *time.Timer
}

func (r *recSession) signalDone() { r.doneOnce.Do(func() { close(r.done) }) }

var (
	recMu   sync.Mutex
	recByID = map[*Service]*recSession{}
)

func newRecID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartRecording starts the recording proxy (and optionally a browser). Only
// one recording may be active per service.
func (s *Service) StartRecording(o RecordingOptions) (*RecordingInfo, error) {
	recMu.Lock()
	defer recMu.Unlock()
	if cur := recByID[s]; cur != nil {
		return nil, fmt.Errorf("%w: a recording is already active (id %s); stop it first", ErrBusy, cur.id)
	}
	if o.Listen == "" {
		o.Listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(o.Listen)
	if err != nil || !safety.IsLoopback(host) || host == "localhost" {
		return nil, invalid("the recording proxy only listens on a loopback IP address (e.g. 127.0.0.1:8888)")
	}
	if o.StartURL != "" && !strings.HasPrefix(o.StartURL, "http://") && !strings.HasPrefix(o.StartURL, "https://") {
		return nil, invalid("start URL must begin with http:// or https://")
	}
	o.IncludeHosts, o.ExcludeHosts = cleanHosts(o.IncludeHosts), cleanHosts(o.ExcludeHosts)
	dir := o.CADir
	if dir == "" {
		dir = har.CADir()
	}
	ca, created, err := har.LoadOrCreateCA(dir)
	if err != nil {
		return nil, err
	}
	if created {
		safety.Audit("record.ca_created", map[string]any{"dir": dir})
	}
	bpath := ""
	if o.Browser {
		if bpath, err = browser.Find(); err != nil {
			return nil, invalid("%v", err)
		}
	}
	rec := har.NewRecorder(har.UpstreamTransport(o.UpstreamRootCAs, o.InsecureUpstream))
	rec.CA = ca
	rec.Filter = &har.HostFilter{Include: o.IncludeHosts, Exclude: o.ExcludeHosts, DefaultExclude: true}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", o.Listen, err)
	}
	r := &recSession{id: newRecID(), opts: o, rec: rec, ca: ca, addr: ln.Addr().String(), started: time.Now(), bpath: bpath,
		created: created, done: make(chan struct{})}
	r.srv = &http.Server{Handler: rec.Proxy(), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = r.srv.Serve(ln) }()
	if o.Browser {
		b, err := browser.Launch(context.Background(), browser.Options{Path: bpath, ProxyAddr: r.addr, SPKI: ca.SPKIHash(), StartURL: o.StartURL})
		if err != nil {
			_ = r.srv.Close()
			r.rec.Close()
			return nil, err
		}
		r.browser = b
		go func() {
			<-b.Done
			r.signalDone()
		}()
	}
	id := r.id
	r.timer = time.AfterFunc(MaxRecordingDuration, func() {
		r.signalDone()
		// Give an attached CLI a moment to finish the recording itself.
		time.AfterFunc(30*time.Second, func() { _, _ = s.StopRecording(id, "") })
	})
	recByID[s] = r
	safety.Audit("record.start", map[string]any{"id": r.id, "proxy": r.addr, "browser": o.Browser, "includeHosts": o.IncludeHosts,
		"excludeHosts": o.ExcludeHosts, "insecureUpstream": o.InsecureUpstream})
	return r.info(0), nil
}

func cleanHosts(in []string) []string {
	var out []string
	for _, h := range in {
		for _, p := range strings.Split(h, ",") {
			if p = strings.TrimSpace(har.ParseHostURL(strings.TrimSpace(p))); p != "" {
				out = append(out, strings.ToLower(p))
			}
		}
	}
	return out
}

func (r *recSession) info(since int) *RecordingInfo {
	in := &RecordingInfo{ID: r.id, ProxyAddr: r.addr, CASPKI: r.ca.SPKIHash(), CACertPath: filepath.Join(r.ca.Dir, "ca.pem"), CACreated: r.created,
		Browser: r.browser != nil, BrowserPath: r.bpath, Started: r.started, AutoStopAt: r.started.Add(MaxRecordingDuration),
		Captured: r.rec.Count(), Transaction: r.rec.CurrentPage(), Requests: r.rec.Recent(since)}
	if r.browser != nil {
		select {
		case <-r.browser.Done:
			in.BrowserExited = true
		default:
		}
	}
	return in
}

func (s *Service) recording(id string) (*recSession, error) {
	recMu.Lock()
	defer recMu.Unlock()
	r := recByID[s]
	if r == nil || (id != "" && r.id != id) {
		return nil, fmt.Errorf("%w: no active recording with id %q", ErrNotFound, id)
	}
	return r, nil
}

// ActiveRecording returns the active recording (requests with seq > since) or nil.
func (s *Service) ActiveRecording(since int) *RecordingInfo {
	recMu.Lock()
	r := recByID[s]
	recMu.Unlock()
	if r == nil {
		return nil
	}
	return r.info(since)
}

// RecordingDone returns a channel closed when the recording should be stopped
// (browser closed or the 2h limit reached).
func (s *Service) RecordingDone(id string) (<-chan struct{}, error) {
	r, err := s.recording(id)
	if err != nil {
		return nil, err
	}
	return r.done, nil
}

// MarkTransaction starts a named transaction; an empty name ends the current one.
func (s *Service) MarkTransaction(id, name string) (*RecordingInfo, error) {
	r, err := s.recording(id)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		return nil, invalid("transaction name is too long (max 100 characters)")
	}
	if name == "" {
		r.rec.EndPage()
	} else {
		r.rec.Page(name)
	}
	return r.info(1 << 30), nil
}

// StopRecording stops the proxy and browser, saves the HAR under the
// workspace recordings folder (0600) and, when toScenario is set, imports it
// and runs correlation detection.
func (s *Service) StopRecording(id, toScenario string) (*RecordingResult, error) {
	recMu.Lock()
	r := recByID[s]
	if r == nil || (id != "" && r.id != id) {
		recMu.Unlock()
		return nil, fmt.Errorf("%w: no active recording with id %q", ErrNotFound, id)
	}
	delete(recByID, s)
	recMu.Unlock()
	r.timer.Stop()
	r.signalDone()
	if r.browser != nil {
		r.browser.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = r.srv.Shutdown(ctx)
	cancel()
	_ = r.srv.Close()
	r.rec.Close()

	dir := filepath.Join(s.WS.Dir, "recordings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := "rec-" + r.started.Format("20060102-150405") + "-" + r.id[:6] + ".har"
	p := filepath.Join(dir, name)
	if err := r.rec.Save(p); err != nil {
		return nil, err
	}
	hosts := r.rec.Hosts()
	pol := s.Policy()
	res := &RecordingResult{ID: r.id, HARFile: p, Captured: r.rec.Count(), Hosts: hosts}
	for _, h := range hosts {
		if !pol.HostAllowed(h) {
			res.NotAllowlisted = append(res.NotAllowlisted, h)
		}
	}
	if len(res.NotAllowlisted) > 0 {
		res.Note = "Recording is not restricted by the allowlist, but load tests are: allowlist " + strings.Join(res.NotAllowlisted, ", ") +
			" (only systems you are authorized to test) before running this scenario."
	}
	safety.Audit("record.stop", map[string]any{"id": r.id, "captured": res.Captured, "hosts": hosts, "toScenario": toScenario != ""})
	if toScenario = strings.TrimSpace(toScenario); toScenario != "" {
		if res.Captured == 0 {
			return res, invalid("no requests were captured, so no scenario was created (HAR saved to %s)", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return res, err
		}
		imp, err := s.ImportHAR(toScenario, b, har.Options{ExcludeHosts: r.opts.ExcludeHosts})
		if err != nil {
			return res, err
		}
		res.Import = imp
	}
	return res, nil
}

// StopActiveRecording saves and stops any active recording (shutdown paths).
func (s *Service) StopActiveRecording() {
	if r := s.ActiveRecording(1 << 30); r != nil {
		_, _ = s.StopRecording(r.ID, "")
	}
}
