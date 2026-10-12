package engine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/vars"
)

const (
	defaultConnectTimeout = 10 * time.Second
	defaultTLSTimeout     = 10 * time.Second
	defaultMaxConnLife    = 60 * time.Second // JMeter httpclient4.time_to_live
	// Per-VU pools: a VU sends one request at a time, so a couple of idle
	// connections per host (and a few hosts) is all it can use. This keeps
	// 10k VUs bounded to roughly one socket per VU per host.
	perVUIdlePerHost = 2
	perVUIdleTotal   = 8
)

// httpRuntime is the resolved HTTP-layer configuration of a run.
type httpRuntime struct {
	perVU          bool
	keepAlive      bool
	version        string // auto | 1.1 | 2
	maxLife        time.Duration
	failRetries    bool
	compression    bool // auto mode: negotiate + decode gzip/deflate
	connectTimeout time.Duration
	tlsTimeout     time.Duration
	ttfbTimeout    time.Duration
	tlsBackstop    time.Duration // transport-level TLS bound (max of all configured)
	maxConns       int

	dial   safety.DialFunc
	proxy  func(*http.Request) (*url.URL, error)
	tlsCfg *tls.Config
}

// lifeConn remembers when a connection was opened (max connection lifetime).
type lifeConn struct {
	net.Conn
	born time.Time
}

func unwrapConn(c net.Conn) *lifeConn {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	lc, _ := c.(*lifeConn)
	return lc
}

// vuRoundTripper records the request the transport is about to send (the
// http.Client may have cloned it to add cookies), so trace hooks can mark it.
type vuRoundTripper struct{ t *http.Transport }

func (rt vuRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if st := stateFrom(req.Context()); st != nil {
		st.mu.Lock()
		st.curReq = req
		st.mu.Unlock()
	}
	return rt.t.RoundTrip(req)
}

// connectionMode resolves the per-vu/shared default from the executor.
func connectionMode(sc *model.Scenario) string {
	if m := sc.Defaults.Connections; m != "" {
		return m
	}
	switch sc.Workload.Executor {
	case model.ExecArrivalRate, model.ExecRampingRate:
		return model.ConnShared
	}
	return model.ConnPerVU
}

// setupHTTP resolves the HTTP options, loads certificates and builds the
// guarded dialler, proxy function and (in shared mode) the shared transport.
func (r *Run) setupHTTP() error {
	d := r.sc.Defaults
	o := d.HTTPOptions
	h := &httpRuntime{
		perVU:          connectionMode(r.sc) == model.ConnPerVU,
		keepAlive:      o.KeepAlive == nil || *o.KeepAlive,
		version:        o.HTTPVersion,
		maxLife:        o.MaxConnLifetime.D(),
		failRetries:    o.HiddenRetries == "fail",
		compression:    o.Compression != "off",
		connectTimeout: o.ConnectTimeout.D(),
		tlsTimeout:     o.TLSTimeout.D(),
		ttfbTimeout:    o.ResponseHeaderTimeout.D(),
		maxConns:       max(r.sc.Workload.MaxPlannedVUs(), 16),
	}
	if h.version == "" {
		h.version = "auto"
	}
	if h.maxLife == 0 {
		h.maxLife = defaultMaxConnLife
	} else if h.maxLife < 0 {
		h.maxLife = 0
	}
	if h.connectTimeout <= 0 {
		h.connectTimeout = defaultConnectTimeout
	}
	if h.tlsTimeout <= 0 {
		h.tlsTimeout = defaultTLSTimeout
	}
	h.tlsBackstop = h.tlsTimeout
	for _, st := range r.sc.AllRequests() {
		h.tlsBackstop = max(h.tlsBackstop, st.Request.TLSTimeout.D())
	}

	// Values in proxy settings may reference scenario variables and __env.
	vc := vars.NewContext(0, r.opts.Seed)
	for k, v := range r.sc.Variables {
		vc.Vars[k] = v
	}
	expand := func(s string) string { v, _ := vc.Expand(s); return v }

	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	var hosts map[string]string
	if o.DNS != nil && len(o.DNS.Hosts) > 0 {
		hosts = map[string]string{}
		for name, ip := range o.DNS.Hosts {
			ip = strings.Trim(strings.TrimSpace(ip), "[]")
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("defaults.dns.hosts[%s]: %q is not an IP address", name, ip)
			}
			if !r.opts.Policy.HostAllowed(ip) {
				return fmt.Errorf("defaults.dns.hosts maps %s to %s, which is not in the safety allowlist: %w", name, ip, &safety.ErrHostNotAllowed{Host: ip})
			}
			hosts[strings.ToLower(name)] = ip
		}
	}
	h.dial = r.opts.Policy.GuardDialerWith(dialer, safety.DialOptions{Hosts: hosts, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		limit := h.connectTimeout
		if st := stateFrom(ctx); st != nil && st.connectLimit > 0 {
			limit = st.connectLimit
		}
		dctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		c, err := dialer.DialContext(dctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &lifeConn{Conn: c, born: time.Now()}, nil
	}})

	if px := o.Proxy; px != nil {
		pu, err := url.Parse(expand(px.URL))
		if err != nil {
			return fmt.Errorf("defaults.proxy.url: %w", err)
		}
		if px.Username != "" {
			pu.User = url.UserPassword(expand(px.Username), expand(px.Password))
		}
		var noProxy []string
		for _, n := range px.NoProxy {
			noProxy = append(noProxy, expand(n))
		}
		h.proxy, err = r.opts.Policy.GuardProxy(safety.ProxyConfig{URL: pu, NoProxy: noProxy})
		if err != nil {
			return fmt.Errorf("defaults.proxy: %w", err)
		}
	}

	cfg, err := r.loadTLS(d)
	if err != nil {
		return err
	}
	h.tlsCfg = cfg
	r.http = h
	if !h.perVU {
		r.transport = h.newTransport(false)
	}
	return nil
}

// newTransport builds a guarded transport. perVU transports keep only a few
// idle connections; the shared one is sized for every VU.
func (h *httpRuntime) newTransport(perVU bool) *http.Transport {
	t := &http.Transport{
		Proxy:                 h.proxy, // nil: never route load through a system proxy implicitly
		DialContext:           h.dial,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   h.tlsBackstop,
		DisableKeepAlives:     !h.keepAlive,
		DisableCompression:    true, // Load Meter negotiates and decodes itself (compress.go)
		TLSClientConfig:       h.tlsCfg.Clone(),
		ExpectContinueTimeout: time.Second,
	}
	if perVU {
		t.MaxIdleConns, t.MaxIdleConnsPerHost = perVUIdleTotal, perVUIdlePerHost
	} else {
		t.MaxIdleConns, t.MaxIdleConnsPerHost = h.maxConns*2, h.maxConns
	}
	p := new(http.Protocols)
	switch h.version {
	case "1.1":
		p.SetHTTP1(true)
	case "2":
		p.SetHTTP2(true)
		p.SetUnencryptedHTTP2(true) // http:// URLs use h2c with prior knowledge
	default:
		p.SetHTTP1(true)
		p.SetHTTP2(true)
	}
	t.Protocols = p
	return t
}

// loadTLS builds the base TLS config: verification settings, extra roots and
// client certificates (selected per target host at handshake time).
func (r *Run) loadTLS(d model.Defaults) (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: d.InsecureSkipVerify, MinVersion: tls.VersionTLS12}
	t := d.TLS
	if t == nil {
		return cfg, nil
	}
	if t.CAFile != "" {
		pemData, err := r.readScenarioFile("defaults.tls.caFile", t.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("defaults.tls.caFile: no PEM certificates found")
		}
		cfg.RootCAs = pool
	}
	var def *tls.Certificate
	if t.ClientCert != "" {
		c, err := r.loadKeyPair("defaults.tls", t.ClientCert, t.ClientKey)
		if err != nil {
			return nil, err
		}
		def = c
	}
	type hostCert struct {
		pattern string
		cert    *tls.Certificate
	}
	var perHost []hostCert
	for i, hc := range t.Hosts {
		c, err := r.loadKeyPair(fmt.Sprintf("defaults.tls.hosts[%d]", i), hc.ClientCert, hc.ClientKey)
		if err != nil {
			return nil, err
		}
		perHost = append(perHost, hostCert{strings.ToLower(hc.Host), c})
	}
	if def == nil && len(perHost) == 0 {
		return cfg, nil
	}
	cfg.GetClientCertificate = func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		host := ""
		if st := stateFrom(cri.Context()); st != nil {
			host = strings.ToLower(st.currentHost())
		}
		for _, hc := range perHost {
			if hc.pattern == host || (strings.HasPrefix(hc.pattern, "*.") && strings.HasSuffix(host, hc.pattern[1:])) {
				return hc.cert, nil
			}
		}
		if def != nil {
			return def, nil
		}
		return &tls.Certificate{}, nil // no certificate for this host
	}
	return cfg, nil
}

func (r *Run) readScenarioFile(what, rel string) ([]byte, error) {
	p, err := SafePath(r.opts.BaseDir, rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return b, nil
}

// loadKeyPair loads a PEM certificate and key. Encrypted keys are refused
// (stdlib cannot decrypt PKCS#8 encrypted keys; convert with
// `openssl pkcs8 -in enc.key -out key.pem -nocrypt`). Errors never include key material.
func (r *Run) loadKeyPair(what, certFile, keyFile string) (*tls.Certificate, error) {
	certPEM, err := r.readScenarioFile(what+".clientCert", certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := r.readScenarioFile(what+".clientKey", keyFile)
	if err != nil {
		return nil, err
	}
	for rest := keyPEM; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type == "ENCRYPTED PRIVATE KEY" || b.Headers["Proc-Type"] != "" {
			return nil, fmt.Errorf("%s.clientKey: encrypted private keys are not supported; decrypt it first (openssl pkcs8 -in key.pem -nocrypt -out plain.pem) and protect the file with permissions", what)
		}
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot load client certificate/key (PEM expected; convert PKCS#12 with `openssl pkcs12 -in c.p12 -nodes -out c.pem`): %v", what, err)
	}
	return &c, nil
}

// clientTransport returns the transport a new VU uses.
func (r *Run) clientTransport() (http.RoundTripper, *http.Transport) {
	if r.http.perVU {
		t := r.http.newTransport(true)
		return vuRoundTripper{t}, t
	}
	return vuRoundTripper{r.transport}, nil
}

// closeIdle releases the VU's own connections (per-vu mode).
func (vu *vuState) closeIdle() {
	if vu.ownTransport != nil {
		vu.ownTransport.CloseIdleConnections()
	}
}
