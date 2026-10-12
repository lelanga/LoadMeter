package service

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
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

	"github.com/lelanga/loadmeter/internal/model"
)

func TestRecordingSessionToScenario(t *testing.T) {
	s := newSvc(t)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"csrf":"f3a9c2e81b7d4c56a0e9"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer up.Close()
	pool := x509.NewCertPool()
	pool.AddCert(up.Certificate())

	info, err := s.StartRecording(RecordingOptions{UpstreamRootCAs: pool, CADir: filepath.Join(t.TempDir(), "ca")})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || !strings.HasPrefix(info.ProxyAddr, "127.0.0.1:") || info.CASPKI == "" {
		t.Fatalf("info %+v", info)
	}
	if _, err := s.StartRecording(RecordingOptions{}); !errors.Is(err, ErrBusy) {
		t.Errorf("second recording: %v, want ErrBusy", err)
	}
	if _, err := s.StartRecording(RecordingOptions{Listen: "0.0.0.0:0"}); err == nil {
		t.Error("non-loopback listen accepted")
	}
	ca, _ := os.ReadFile(info.CACertPath)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	pu, _ := url.Parse("http://" + info.ProxyAddr)
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: roots}}}
	do := func(method, p, body string) {
		req, _ := http.NewRequest(method, up.URL+p, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if _, err := s.MarkTransaction(info.ID, "Login"); err != nil {
		t.Fatal(err)
	}
	do("GET", "/login", "")
	do("POST", "/session", `{"user":"a","csrf":"f3a9c2e81b7d4c56a0e9"}`)
	if _, err := s.MarkTransaction("wrong-id", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong id: %v", err)
	}
	s.MarkTransaction(info.ID, "Browse")
	do("GET", "/products", "")
	a := s.ActiveRecording(0)
	if a == nil || a.Captured != 3 || a.Transaction != "Browse" || len(a.Requests) != 3 {
		t.Fatalf("active: %+v", a)
	}
	if got := s.ActiveRecording(2); len(got.Requests) != 1 {
		t.Errorf("since=2 returned %d requests", len(got.Requests))
	}
	res, err := s.StopRecording(info.ID, "recorded-shop")
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveRecording(0) != nil {
		t.Error("still active after stop")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(res.HARFile)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("HAR perms: %v %v", fi, err)
		}
	}
	if filepath.Dir(res.HARFile) != filepath.Join(s.WS.Dir, "recordings") {
		t.Errorf("HAR saved to %s", res.HARFile)
	}
	if res.Import == nil || res.Import.File == "" {
		t.Fatalf("no import: %+v", res)
	}
	sc, err := s.GetScenario(res.Import.File)
	if err != nil {
		t.Fatal(err)
	}
	var tx []string
	for _, st := range sc.Steps {
		if st.Type == model.StepTransaction {
			tx = append(tx, st.Name)
		}
	}
	if strings.Join(tx, ",") != "T01_Login,T02_Browse" {
		t.Errorf("transactions %v", tx)
	}
	found := false
	for _, cand := range res.Import.Correlations {
		found = found || strings.Contains(strings.ToLower(cand.Variable), "csrf")
	}
	if !found {
		t.Errorf("csrf correlation not detected: %+v", res.Import.Correlations)
	}
	// Recording no longer active: proxy is closed.
	if _, err := c.Get(up.URL + "/after"); err == nil {
		t.Error("proxy still serving after stop")
	}
	if _, err := s.StopRecording(info.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("double stop: %v", err)
	}
}

func TestRecordingAutoStop(t *testing.T) {
	s := newSvc(t)
	old := MaxRecordingDuration
	MaxRecordingDuration = 50 * time.Millisecond
	defer func() { MaxRecordingDuration = old }()
	info, err := s.StartRecording(RecordingOptions{CADir: filepath.Join(t.TempDir(), "ca")})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := s.RecordingDone(info.ID)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("auto-stop did not signal")
	}
	if _, err := s.StopRecording(info.ID, ""); err != nil {
		t.Fatal(err)
	}
}
