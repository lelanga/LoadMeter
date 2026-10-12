package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/service"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	t.Setenv("LOADMETER_HOME", t.TempDir())
	svc, err := service.New(app.Workspace{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	s := New(svc)
	return s, s.Handler() // must not panic on route registration
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:7878"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthAndCSRF(t *testing.T) {
	s, h := newTestServer(t)
	cookie := "loadmeter_session=" + s.Token

	if w := do(h, "GET", "/api/scenarios", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", w.Code)
	}
	if w := do(h, "GET", "/api/scenarios", map[string]string{"Cookie": cookie}); w.Code != http.StatusOK {
		t.Fatalf("cookie GET: got %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/demo", map[string]string{"Cookie": cookie}); w.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF header: got %d", w.Code)
	}
	if w := do(h, "GET", "/?token=wrong", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d", w.Code)
	}
	w := do(h, "GET", "/?token="+s.Token, nil)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("good token: got %d cookie %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := do(h, "POST", "/", map[string]string{"Cookie": cookie}); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to static: got %d", w.Code)
	}
}

func TestRejectsForeignHost(t *testing.T) {
	s, h := newTestServer(t)
	r := httptest.NewRequest("GET", "/api/scenarios", nil)
	r.Host = "evil.example:7878"
	r.Header.Set("Cookie", "loadmeter_session="+s.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("DNS-rebinding host was served")
	}
}
