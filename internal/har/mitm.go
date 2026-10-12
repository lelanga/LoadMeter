package har

// HTTPS recording through a local man-in-the-middle proxy.
//
// Load Meter generates a per-user recording CA on first use (ECDSA P-256, one
// year) under <ConfigDir>/recording-ca and mints per-host leaf certificates in
// memory. The browser either trusts that CA (installed by the user) or, when
// Load Meter launches it, is told to accept certificates chaining to the CA's
// public key only (--ignore-certificate-errors-spki-list). Upstream TLS is
// verified normally unless the user explicitly asks for --insecure-upstream.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lelanga/loadmeter/internal/safety"
)

// CA is the local recording certificate authority.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	Dir     string

	mu      sync.Mutex
	leafKey *ecdsa.PrivateKey
	leaves  map[string]*tls.Certificate
}

// CADir is <ConfigDir>/recording-ca.
func CADir() string { return filepath.Join(safety.ConfigDir(), "recording-ca") }

func caPaths(dir string) (string, string) {
	return filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
}

// LoadOrCreateCA loads the recording CA from dir, creating it when missing or
// expiring within a day. created reports whether a new CA was generated.
func LoadOrCreateCA(dir string) (ca *CA, created bool, err error) {
	certPath, keyPath := caPaths(dir)
	if ca, err := loadCA(dir); err == nil {
		if time.Until(ca.Cert.NotAfter) > 24*time.Hour {
			return ca, false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("recording CA in %s is unreadable (%v); delete it with 'loadmeter record ca --delete'", dir, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, false, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: "Load Meter Local Recording CA " + host, Organization: []string{"Load Meter (local recording only)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, false, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	_ = os.Chmod(dir, 0o700)
	if err := writeFile0600(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})); err != nil {
		return nil, false, err
	}
	if err := writeFile0600(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, false, err
	}
	ca, err = loadCA(dir)
	if err != nil {
		return nil, false, err
	}
	return ca, true, nil
}

func writeFile0600(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func loadCA(dir string) (*CA, error) {
	certPath, keyPath := caPaths(dir)
	cp, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	kp, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(cp)
	kb, _ := pem.Decode(kp)
	if cb == nil || kb == nil {
		return nil, errors.New("invalid PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("CA key does not match certificate")
	}
	return &CA{Cert: cert, Key: key, CertPEM: cp, Dir: dir, leaves: map[string]*tls.Certificate{}}, nil
}

// DeleteCA removes the recording CA files. Missing files are not an error.
func DeleteCA(dir string) error {
	certPath, keyPath := caPaths(dir)
	for _, p := range []string{keyPath, certPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil // directory holds other files; leave it
	}
	return nil
}

// SPKIHash is base64(sha256(SubjectPublicKeyInfo)) of the CA, the format
// Chromium's --ignore-certificate-errors-spki-list expects.
func (ca *CA) SPKIHash() string {
	sum := sha256.Sum256(ca.Cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Pool returns a cert pool containing only the CA (for tests and clients).
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

func randSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n.Add(n, big.NewInt(1))
}

// Leaf returns (and caches) a certificate for host signed by the CA.
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leaves[host]; ok && time.Until(c.Leaf.NotAfter) > time.Hour {
		return c, nil
	}
	if ca.leafKey == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		ca.leafKey = k
	}
	now := time.Now()
	notAfter := now.Add(30 * 24 * time.Hour)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: host, Organization: []string{"Load Meter recording"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &ca.leafKey.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	c := &tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: ca.leafKey, Leaf: leaf}
	if len(ca.leaves) > 2000 {
		ca.leaves = map[string]*tls.Certificate{}
	}
	ca.leaves[host] = c
	return c, nil
}

// UpstreamTransport is the transport the recorder uses to reach real servers.
// Certificates are verified against the system roots (or rootCAs when set);
// insecure disables verification and must only be used for self-signed test
// servers the user controls.
func UpstreamTransport(rootCAs *x509.CertPool, insecure bool) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: rootCAs, InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12}, // #nosec G402 -- opt-in flag only
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

var discardLog = log.New(io.Discard, "", 0)

// handleConnect terminates the client's TLS with a leaf for the requested
// host and serves the decrypted HTTP/1.1 requests through the recorder.
func (r *Recorder) handleConnect(w http.ResponseWriter, req *http.Request) {
	authority := req.Host
	if authority == "" {
		authority = req.URL.Host
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = authority, "443"
		authority = net.JoinHostPort(host, port)
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy cannot hijack connection", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	if !r.track(conn) {
		conn.Close()
		return
	}
	defer r.untrack(conn)
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	var c net.Conn = conn
	if n := brw.Reader.Buffered(); n > 0 {
		peek, _ := brw.Reader.Peek(n)
		c = &prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(append([]byte(nil), peek...)), conn)}
	}
	if r.Filter != nil && !r.Filter.AllowHost(host) {
		blindTunnel(c, authority)
		return
	}
	tc := tls.Server(c, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return r.CA.Leaf(name)
		},
	})
	_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.HandshakeContext(context.Background()); err != nil {
		tc.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})
	urlHost := authority
	if port == "443" {
		urlHost = host
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			urlHost = "[" + host + "]"
		}
	}
	ln := &oneConnListener{c: tc, done: make(chan struct{})}
	srv := &http.Server{
		ReadHeaderTimeout: 60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          discardLog,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, in *http.Request) {
			target := *in.URL
			target.Scheme, target.Host = "https", urlHost
			r.forward(w, in, &target)
		}),
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				ln.Close()
			}
		},
	}
	_ = srv.Serve(ln)
}

func blindTunnel(client net.Conn, authority string) {
	defer client.Close()
	up, err := net.DialTimeout("tcp", authority, 15*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
}

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// oneConnListener hands a single connection to http.Server.Serve.
type oneConnListener struct {
	c    net.Conn
	once sync.Once
	cl   sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.cl.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// ParseHostURL is a helper for callers that accept "host" or a URL.
func ParseHostURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return s
}

// track registers a hijacked client connection so Close can end it; it
// returns false when the recorder is already closed.
func (r *Recorder) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.conns == nil {
		r.conns = map[net.Conn]struct{}{}
	}
	r.conns[c] = struct{}{}
	return true
}

func (r *Recorder) untrack(c net.Conn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
	c.Close()
}

// Close ends all intercepted HTTPS tunnels and refuses new ones. Call it after
// shutting down the proxy's http.Server (which does not track hijacked conns).
func (r *Recorder) Close() {
	r.mu.Lock()
	r.closed = true
	cs := r.conns
	r.conns = nil
	r.mu.Unlock()
	for c := range cs {
		c.Close()
	}
}
