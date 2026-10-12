package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

func TestDNSOverrideHonoursAllowlist(t *testing.T) {
	t.Parallel()
	srv, ms := newMock(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	// Allowlisted name pinned to loopback: works, and the logical Host is kept.
	o := opts(nil)
	o.Policy.AllowedHosts = []string{"api.test"}
	sc := newSc("http://api.test:"+port, perVU(1, 2), get("h", "/health"))
	sc.Defaults.DNS = &model.DNSOptions{Hosts: map[string]string{"api.test": "127.0.0.1"}}
	_, snap := runSc(t, sc, o)
	if snap.Requests.Errors != 0 || ms.Hits.Load() != 2 {
		t.Fatalf("override to loopback: %v hits=%d", snap.Requests.ErrorKind, ms.Hits.Load())
	}

	// BYPASS attempt 1: a non-allowlisted name pinned to loopback is still refused.
	sc = newSc("http://evil.test:"+port, perVU(1, 1), get("h", "/health"))
	sc.Defaults.DNS = &model.DNSOptions{Hosts: map[string]string{"evil.test": "127.0.0.1"}}
	_, snap = runSc(t, sc, o)
	if snap.Requests.ErrorKind["blocked_by_policy"] != 1 {
		t.Errorf("logical host must be checked: %v", snap.Requests.ErrorKind)
	}

	// BYPASS attempt 2: an allowlisted (or loopback) name pinned to a
	// non-allowlisted IP is refused before the run starts.
	for _, name := range []string{"api.test", "localhost", "127.0.0.1"} {
		sc = newSc("http://"+name+":"+port, perVU(1, 1), get("h", "/health"))
		sc.Defaults.DNS = &model.DNSOptions{Hosts: map[string]string{name: "10.255.255.1"}}
		if _, err := Prepare(sc, o); err == nil || !strings.Contains(err.Error(), "allowlist") {
			t.Errorf("%s -> 10.255.255.1 must be refused at Prepare: %v", name, err)
		}
	}
	// Validation rejects host names as override targets.
	sc = newSc(srv.URL, perVU(1, 1), get("h", "/health"))
	sc.Defaults.DNS = &model.DNSOptions{Hosts: map[string]string{"api.test": "evil.example.com"}}
	if _, err := Prepare(sc, o); err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Errorf("hostname target: %v", err)
	}
}

// forwardProxy is a minimal HTTP forward proxy that answers itself and
// records what it received (no upstream connection is made).
type forwardProxy struct {
	srv   *httptest.Server
	hits  atomic.Int64
	mu    sync.Mutex
	auth  []string
	hosts []string
}

func newForwardProxy(t *testing.T) *forwardProxy {
	p := &forwardProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		p.mu.Lock()
		p.auth = append(p.auth, r.Header.Get("Proxy-Authorization"))
		p.hosts = append(p.hosts, r.URL.Host)
		p.mu.Unlock()
		fmt.Fprintf(w, "via-proxy %s", r.URL.String())
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func TestProxyHonoursAllowlist(t *testing.T) {
	t.Parallel()
	px := newForwardProxy(t)
	o := opts(nil)
	o.Policy.AllowedHosts = []string{"api.test"}
	rec := &recorder{}
	o.Inspect = rec.fn
	sc := newSc("http://api.test", perVU(1, 1),
		withAssert(get("ok", "/x"), model.Assertion{Type: model.AsContains, Value: "via-proxy http://api.test/x"}),
		get("evil", "http://evil.test/x"))
	sc.Defaults.Proxy = &model.ProxyOptions{URL: px.srv.URL, Username: "pu", Password: "${secret_proxy}"}
	sc.Variables["secret_proxy"] = "pp"
	_, snap := runSc(t, sc, o)
	if ex := rec.byStep("ok")[0]; !ex.OK {
		t.Errorf("allowlisted target through the proxy: %s", ex.Error)
	}
	if snap.Requests.ErrorKind["blocked_by_policy"] != 1 || px.hits.Load() != 1 {
		t.Errorf("BYPASS: non-allowlisted target must not reach the proxy: kinds=%v proxyHits=%d", snap.Requests.ErrorKind, px.hits.Load())
	}
	px.mu.Lock()
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pu:pp")); len(px.auth) != 1 || px.auth[0] != want {
		t.Errorf("proxy credentials: %v", px.auth)
	}
	px.mu.Unlock()

	// Loopback targets never go through the proxy (it would be the proxy's localhost).
	mock, ms := newMock(t)
	sc = newSc(mock.URL, perVU(1, 1), get("h", "/health"))
	sc.Defaults.Proxy = &model.ProxyOptions{URL: px.srv.URL}
	_, snap = runSc(t, sc, o)
	if snap.Requests.Errors != 0 || ms.Hits.Load() != 1 || px.hits.Load() != 1 {
		t.Errorf("loopback must connect directly: %v mockHits=%d proxyHits=%d", snap.Requests.ErrorKind, ms.Hits.Load(), px.hits.Load())
	}

	// A non-allowlisted proxy host is refused at Prepare.
	sc = newSc("http://api.test", perVU(1, 1), get("ok", "/x"))
	sc.Defaults.Proxy = &model.ProxyOptions{URL: "http://proxy.evil.test:3128"}
	if _, err := Prepare(sc, o); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("non-allowlisted proxy: %v", err)
	}
	// BYPASS attempt: DNS override repointing an allowlisted proxy name elsewhere.
	o2 := o
	o2.Policy.AllowedHosts = []string{"api.test", "proxy.test"}
	sc.Defaults.Proxy = &model.ProxyOptions{URL: "http://proxy.test:3128"}
	sc.Defaults.DNS = &model.DNSOptions{Hosts: map[string]string{"proxy.test": "10.66.66.66"}}
	if _, err := Prepare(sc, o2); err == nil {
		t.Error("DNS override of the proxy host to a non-allowlisted IP must be refused")
	}
}

// ---------- mTLS ----------

type pki struct {
	dir               string
	caPEM             []byte
	ca                *x509.Certificate
	caKey             *ecdsa.PrivateKey
	serverTLS         tls.Certificate
	clientCN, otherCN string
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *pki) issue(t *testing.T, cn string, server bool) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{dir: t.TempDir()}
	p.caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "loadmeter test CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &p.caKey.PublicKey, p.caKey)
	p.ca, _ = x509.ParseCertificate(der)
	writePEM(t, filepath.Join(p.dir, "ca.pem"), "CERTIFICATE", der)
	sder, skey := p.issue(t, "server", true)
	skeyDER, _ := x509.MarshalECPrivateKey(skey)
	p.serverTLS, _ = tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sder}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: skeyDER}))
	for _, cn := range []string{"vu-client", "other-client"} {
		cder, ckey := p.issue(t, cn, false)
		kder, _ := x509.MarshalPKCS8PrivateKey(ckey)
		writePEM(t, filepath.Join(p.dir, cn+".pem"), "CERTIFICATE", cder)
		writePEM(t, filepath.Join(p.dir, cn+".key"), "PRIVATE KEY", kder)
	}
	return p
}

func TestClientCertificates(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "cn=%s", r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.serverTLS}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	defer srv.Close()

	o := opts(nil)
	o.BaseDir = p.dir
	run := func(tlsOpt *model.TLSOptions) (*recorder, string) {
		rec := &recorder{}
		o.Inspect = rec.fn
		sc := newSc(srv.URL, perVU(1, 1), get("m", "/"))
		sc.Defaults.TLS = tlsOpt
		r, err := Prepare(sc, o)
		if err != nil {
			return rec, err.Error()
		}
		r.Start(t.Context())
		waitDone(t, r, 10*time.Second)
		return rec, ""
	}
	// Verified against the scenario CA, presenting the default client cert.
	rec, err := run(&model.TLSOptions{CAFile: "ca.pem", ClientCert: "vu-client.pem", ClientKey: "vu-client.key"})
	if err != "" || !rec.all()[0].OK || rec.all()[0].RespBody != "cn=vu-client" {
		t.Fatalf("mTLS default cert: %s %+v", err, rec.all())
	}
	// Per-host certificate wins over the default.
	rec, _ = run(&model.TLSOptions{CAFile: "ca.pem", ClientCert: "vu-client.pem", ClientKey: "vu-client.key",
		Hosts: []model.TLSHostCert{{Host: "127.0.0.1", ClientCert: "other-client.pem", ClientKey: "other-client.key"}}})
	if rec.all()[0].RespBody != "cn=other-client" {
		t.Errorf("per-host cert: %+v", rec.all()[0])
	}
	// Without a client certificate the handshake fails and is classified tls.
	rec, _ = run(&model.TLSOptions{CAFile: "ca.pem"})
	if ex := rec.all()[0]; ex.OK {
		t.Errorf("server requires a client cert: %+v", ex)
	}
	// Without the CA the server certificate is not trusted.
	rec, _ = run(nil)
	if ex := rec.all()[0]; ex.OK || !strings.Contains(ex.Error, "certificate") {
		t.Errorf("untrusted server: %+v", ex)
	}
	// Files must stay inside the scenario directory.
	if _, err := run(&model.TLSOptions{ClientCert: "../x.pem", ClientKey: "vu-client.key"}); !strings.Contains(err, "escapes") {
		t.Errorf("SafePath: %s", err)
	}
	if _, err := run(&model.TLSOptions{ClientCert: "/etc/passwd", ClientKey: "vu-client.key"}); !strings.Contains(err, "absolute") {
		t.Errorf("SafePath: %s", err)
	}
	// Encrypted keys are refused with a clear message.
	os.WriteFile(filepath.Join(p.dir, "enc.key"), pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1, 2, 3}}), 0o600)
	if _, err := run(&model.TLSOptions{ClientCert: "vu-client.pem", ClientKey: "enc.key"}); !strings.Contains(err, "encrypted") {
		t.Errorf("encrypted key: %s", err)
	}
}

func TestTLSErrorClassification(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	sc := newSc(srv.URL, perVU(1, 1), get("x", "/"))
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["tls"] != 1 {
		t.Errorf("untrusted certificate must be classified tls: %v", snap.Requests.ErrorKind)
	}
}
