package har

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/model"
)

func TestCAGenerateReuseAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recording-ca")
	ca, created, err := LoadOrCreateCA(dir)
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	if !strings.HasPrefix(ca.Cert.Subject.CommonName, "Phoenix Performance Local Recording CA ") {
		t.Errorf("CN = %q", ca.Cert.Subject.CommonName)
	}
	if !ca.Cert.IsCA || ca.Cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("not an ECDSA CA")
	}
	if d := ca.Cert.NotAfter.Sub(ca.Cert.NotBefore); d < 364*24*time.Hour || d > 367*24*time.Hour {
		t.Errorf("validity %v, want ~1 year", d)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "ca-key.pem"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("key perms: %v %v", fi.Mode().Perm(), err)
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir perms %v", di.Mode().Perm())
		}
	}
	ca2, created2, err := LoadOrCreateCA(dir)
	if err != nil || created2 {
		t.Fatalf("reuse: %v created=%v", err, created2)
	}
	if ca2.Cert.SerialNumber.Cmp(ca.Cert.SerialNumber) != 0 || ca2.SPKIHash() != ca.SPKIHash() {
		t.Error("CA was not reused")
	}
	if len(ca.SPKIHash()) != 44 {
		t.Errorf("SPKI hash %q is not base64 sha256", ca.SPKIHash())
	}
	// Leaf certificates chain to the CA and are cached.
	l1, err := ca.Leaf("shop.example")
	if err != nil {
		t.Fatal(err)
	}
	l2, _ := ca.Leaf("shop.example")
	if l1 != l2 {
		t.Error("leaf not cached")
	}
	if _, err := l1.Leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: "shop.example"}); err != nil {
		t.Errorf("leaf does not verify: %v", err)
	}
	if err := DeleteCA(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca-key.pem")); !os.IsNotExist(err) {
		t.Error("key not deleted")
	}
}

type harDoc struct {
	Log struct {
		Pages   []map[string]any `json:"pages"`
		Entries []struct {
			PageRef string `json:"pageref"`
			Request struct {
				Method   string `json:"method"`
				URL      string `json:"url"`
				PostData *struct {
					Text string `json:"text"`
				} `json:"postData"`
			} `json:"request"`
			Response struct {
				Status  int `json:"status"`
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"response"`
		} `json:"entries"`
	} `json:"log"`
}

func parseHAR(t *testing.T, b []byte) harDoc {
	t.Helper()
	var d harDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func upstreamTLS(t *testing.T) (*httptest.Server, *x509.CertPool) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"token":"tok-` + strings.ToUpper(string(b[:min(4, len(b))])) + `-123","echo":` + jsonString(string(b)) + `}`))
		case "/big":
			w.Write(bytes.Repeat([]byte("x"), 300_000))
		default:
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>secret page " + r.URL.Path + "</html>"))
		}
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func proxyClient(t *testing.T, proxyURL string, roots *x509.CertPool) *http.Client {
	pu, _ := url.Parse(proxyURL)
	tr := &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func TestMITMEndToEnd(t *testing.T) {
	up, upPool := upstreamTLS(t)
	ca, _, err := LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(UpstreamTransport(upPool, false))
	rec.CA = ca
	rec.MaxBody = 1000
	px := httptest.NewServer(rec.Proxy())
	defer px.Close()
	defer rec.Close()
	c := proxyClient(t, px.URL, ca.Pool()) // trusts ONLY the recording CA

	resp, err := c.Get(up.URL + "/home?q=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "secret page /home") {
		t.Fatalf("body %q", body)
	}
	if resp.TLS == nil || resp.TLS.PeerCertificates[0].Issuer.CommonName != ca.Cert.Subject.CommonName {
		t.Fatalf("client did not see the recording CA's leaf")
	}
	resp, err = c.Post(up.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader("user=alice&password=pw1"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Large bodies pass through untruncated although only MaxBody is recorded.
	resp, err = c.Get(up.URL + "/big")
	if err != nil {
		t.Fatal(err)
	}
	big, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(big) != 300_000 {
		t.Errorf("big body truncated to %d bytes", len(big))
	}

	d := parseHAR(t, rec.HAR())
	if len(d.Log.Entries) != 3 {
		t.Fatalf("entries = %d", len(d.Log.Entries))
	}
	e := d.Log.Entries[0]
	if !strings.HasPrefix(e.Request.URL, "https://127.0.0.1:") || !strings.HasSuffix(e.Request.URL, "/home?q=1") {
		t.Errorf("url %q", e.Request.URL)
	}
	if !strings.Contains(e.Response.Content.Text, "secret page /home") {
		t.Errorf("decrypted response not recorded: %q", e.Response.Content.Text)
	}
	l := d.Log.Entries[1]
	if l.Request.PostData == nil || l.Request.PostData.Text != "user=alice&password=pw1" || !strings.Contains(l.Response.Content.Text, `"token":"tok-USER-123"`) {
		t.Errorf("login exchange not recorded: %+v", l)
	}
	if n := len(d.Log.Entries[2].Response.Content.Text); n != 1000 {
		t.Errorf("recorded big body %d bytes, want MaxBody", n)
	}
}

func TestMITMVerifiesUpstreamByDefault(t *testing.T) {
	up, _ := upstreamTLS(t)
	ca, _, _ := LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	rec := NewRecorder(UpstreamTransport(nil, false)) // system roots: httptest cert is untrusted
	rec.CA = ca
	px := httptest.NewServer(rec.Proxy())
	defer px.Close()
	defer rec.Close()
	resp, err := proxyClient(t, px.URL, ca.Pool()).Get(up.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 for an unverifiable upstream", resp.StatusCode)
	}
	if rec.Count() != 0 {
		t.Error("failed exchange recorded")
	}
	// Explicit opt-in for self-signed test servers.
	rec2 := NewRecorder(UpstreamTransport(nil, true))
	rec2.CA = ca
	px2 := httptest.NewServer(rec2.Proxy())
	defer px2.Close()
	defer rec2.Close()
	resp, err = proxyClient(t, px2.URL, ca.Pool()).Get(up.URL + "/x")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("insecure upstream: %v %v", err, resp)
	}
	resp.Body.Close()
}

func TestConnectRefusedWithoutCA(t *testing.T) {
	rec := NewRecorder(nil)
	req := httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	rec.Proxy().ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("code %d", w.Code)
	}
}

func TestProxyRefusesNonLoopbackClients(t *testing.T) {
	called := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer up.Close()
	rec := NewRecorder(nil)
	for _, m := range []string{http.MethodGet, http.MethodConnect} {
		req := httptest.NewRequest(m, up.URL+"/", nil)
		req.RemoteAddr = "192.168.1.20:40000"
		w := httptest.NewRecorder()
		rec.Proxy().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s from LAN: code %d, want 403", m, w.Code)
		}
	}
	if called || rec.Count() != 0 {
		t.Error("non-loopback request was forwarded")
	}
}

func TestTransactionMarkersAndHostFilter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("ok " + r.URL.Path))
	}))
	defer up.Close()
	rec := NewRecorder(nil)
	u, _ := url.Parse(up.URL)
	rec.Filter = &HostFilter{Exclude: []string{"localhost"}, DefaultExclude: true}
	px := httptest.NewServer(rec.Proxy())
	defer px.Close()
	c := proxyClient(t, px.URL, nil)
	get := func(base, p string) {
		resp, err := c.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	get(up.URL, "/")
	rec.Page("Login")
	get(up.URL, "/login")
	get(up.URL, "/login/submit")
	// Same server through an excluded host name: forwarded, not recorded.
	get("http://localhost:"+u.Port(), "/excluded")
	rec.EndPage()
	get(up.URL, "/about")
	rec.Page("Checkout")
	get(up.URL, "/cart")
	if rec.CurrentPage() != "Checkout" {
		t.Errorf("current page %q", rec.CurrentPage())
	}
	if got := rec.Count(); got != 5 {
		t.Fatalf("recorded %d entries, want 5 (excluded host must not be recorded)", got)
	}
	if rs := rec.Recent(0); len(rs) != 5 || rs[1].Transaction != "Login" || rs[0].Transaction != "" {
		t.Errorf("recent: %+v", rs)
	}
	sc, _, _, err := Import(bytes.NewReader(rec.HAR()), "rec.har", Options{Name: "rec"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, st := range sc.Steps {
		if st.Type == model.StepTransaction {
			names = append(names, st.Name)
		}
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "_Login") || !strings.Contains(got, "_Checkout") || len(names) != 4 {
		t.Fatalf("transactions = %s; want root, Login, about, Checkout", got)
	}
	for _, st := range sc.Steps {
		if strings.HasSuffix(st.Name, "_Login") && len(st.Steps) != 2 {
			t.Errorf("Login has %d requests, want 2", len(st.Steps))
		}
	}
}

func TestHostFilter(t *testing.T) {
	f := &HostFilter{Include: []string{"*.shop.test"}, Exclude: []string{"cdn.shop.test"}, DefaultExclude: true}
	for in, want := range map[string]bool{"https://www.shop.test/a": true, "https://shop.test/": true, "https://cdn.shop.test/x": false,
		"https://other.test/": false, "https://www.google-analytics.com/collect": false} {
		u, _ := url.Parse(in)
		if f.Allow(u) != want {
			t.Errorf("Allow(%s) = %v", in, !want)
		}
	}
	all := &HostFilter{DefaultExclude: true}
	for _, bg := range []string{"https://update.googleapis.com/service/update2", "https://accounts.google.com/ListAccounts?gpsia=1"} {
		u, _ := url.Parse(bg)
		if all.Allow(u) {
			t.Errorf("browser background traffic recorded: %s", bg)
		}
	}
}
