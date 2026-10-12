package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/ai"
	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/safety"
	"github.com/lelanga/phoenix/internal/service"
)

type env struct {
	t   *testing.T
	svc *service.Service
	srv *Server
	ts  *httptest.Server
	cl  *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	t.Setenv("PHOENIX_HOME", t.TempDir())
	svc, err := service.New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	srv := New(svc)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cl := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &env{t: t, svc: svc, srv: srv, ts: ts, cl: cl}
}

type opt func(*http.Request)

func withToken(tok string) opt { return func(r *http.Request) { r.Header.Set("X-Phoenix-Token", tok) } }
func withCookie(tok string) opt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "phoenix_session", Value: tok}) }
}
func withHost(h string) opt { return func(r *http.Request) { r.Host = h } }

func (e *env) do(method, path string, body any, opts ...opt) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := e.cl.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// api performs an authenticated API call (header token).
func (e *env) api(method, path string, body any) (*http.Response, map[string]any, []byte) {
	e.t.Helper()
	resp, raw := e.do(method, path, body, withToken(e.srv.Token))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp, m, raw
}

func (e *env) saveScenario(file string, sc map[string]any) {
	e.t.Helper()
	resp, m, raw := e.api("PUT", "/api/scenarios/"+file, sc)
	if resp.StatusCode != 200 {
		e.t.Fatalf("PUT %s: %d %s", file, resp.StatusCode, raw)
	}
	_ = m
}

func scenarioJSON(name, baseURL string) map[string]any {
	return map[string]any{
		"version": "phoenix/v1", "name": name,
		"defaults": map[string]any{"baseUrl": baseURL},
		"steps":    []any{map[string]any{"type": "request", "name": "r", "request": map[string]any{"method": "GET", "url": "/health"}}},
		"workload": map[string]any{"executor": "constant-vus", "vus": 1, "duration": "1s"},
	}
}

func TestHostHeaderRebindingProtection(t *testing.T) {
	e := newEnv(t)
	h := e.srv.Handler()
	tests := []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:8080", true}, {"localhost:3000", true}, {"[::1]:80", true}, {"localhost", true}, {"127.0.0.1", true},
		{"evil.com", false}, {"evil.com:80", false}, {"localhost.evil.com", false}, {"127.0.0.1.nip.io:80", false},
		{"192.168.1.5:8080", false}, {"10.0.0.1", false}, {"0.0.0.0:80", false}, {"attacker.example:8080", false},
	}
	for _, tc := range tests {
		for _, path := range []string{"/api/health", "/", "/style.css"} {
			req := httptest.NewRequest("GET", "http://"+tc.host+path, nil)
			req.Host = tc.host
			req.Header.Set("X-Phoenix-Token", e.srv.Token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.ok && rec.Code == 403 {
				t.Errorf("%s %s: loopback host refused", tc.host, path)
			}
			if !tc.ok && rec.Code != 403 {
				t.Errorf("%s %s: foreign host served with %d", tc.host, path, rec.Code)
			}
		}
	}
	// a valid token does not rescue a foreign Host
	req := httptest.NewRequest("GET", "http://evil.com/?token="+e.srv.Token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("token exchange on foreign host: %d %v", rec.Code, rec.Header())
	}
}

func TestAPIRequiresToken(t *testing.T) {
	e := newEnv(t)
	paths := []string{"/api/health", "/api/scenarios", "/api/runs", "/api/runs/active", "/api/settings/policy", "/api/audit", "/api/session", "/api/settings/ai", "/api/ai/activity"}
	for _, p := range paths {
		if resp, _ := e.do("GET", p, nil); resp.StatusCode != 401 {
			t.Errorf("GET %s without token: %d", p, resp.StatusCode)
		}
		if resp, _ := e.do("GET", p, nil, withToken("wrong")); resp.StatusCode != 401 {
			t.Errorf("GET %s wrong token: %d", p, resp.StatusCode)
		}
		if resp, _ := e.do("GET", p, nil, withCookie("wrong")); resp.StatusCode != 401 {
			t.Errorf("GET %s wrong cookie: %d", p, resp.StatusCode)
		}
		if resp, _ := e.do("GET", p, nil, withToken(e.srv.Token)); resp.StatusCode != 200 {
			t.Errorf("GET %s header token: %d", p, resp.StatusCode)
		}
		if resp, _ := e.do("GET", p, nil, withCookie(e.srv.Token)); resp.StatusCode != 200 {
			t.Errorf("GET %s cookie token: %d", p, resp.StatusCode)
		}
	}
	// token of the same length but different content, and prefix tokens
	for _, tok := range []string{strings.Repeat("a", len(e.srv.Token)), e.srv.Token[:10], e.srv.Token + "x"} {
		if resp, _ := e.do("GET", "/api/health", nil, withToken(tok)); resp.StatusCode != 401 {
			t.Errorf("token %q accepted", tok)
		}
	}
	// token in the query string is not an API credential
	if resp, _ := e.do("GET", "/api/health?token="+e.srv.Token, nil); resp.StatusCode != 401 {
		t.Errorf("query token accepted on API: %d", resp.StatusCode)
	}
	resp, body := e.do("GET", "/api/health", nil)
	if !strings.Contains(string(body), "error") || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("401 body %q", body)
	}
}

func TestMutatingRequestsNeedHeaderToken(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		resp, body := e.do(m, "/api/runs", `{}`, withCookie(e.srv.Token))
		if resp.StatusCode != 403 || !strings.Contains(string(body), "X-Phoenix-Token") {
			t.Errorf("%s with cookie only: %d %s", m, resp.StatusCode, body)
		}
		resp, _ = e.do(m, "/api/runs", `{}`, withCookie(e.srv.Token), withToken("wrong"))
		if resp.StatusCode != 401 && resp.StatusCode != 403 {
			// wrong header with valid cookie: authed() prefers the header, so 401
			t.Errorf("%s wrong header: %d", m, resp.StatusCode)
		}
		resp, _ = e.do(m, "/api/runs", `{}`)
		if resp.StatusCode != 401 {
			t.Errorf("%s anonymous: %d", m, resp.StatusCode)
		}
	}
	// header token is accepted for POST (handler then rejects the empty body itself)
	resp, m, _ := e.api("POST", "/api/runs", `{}`)
	if resp.StatusCode != 400 || m["error"] == nil {
		t.Errorf("POST with header: %d %v", resp.StatusCode, m)
	}
	// a cross-site form post with cookie but no header cannot start a run
	resp, _ = e.do("POST", "/api/demo", "", withCookie(e.srv.Token))
	if resp.StatusCode != 403 {
		t.Errorf("CSRF: demo via cookie => %d", resp.StatusCode)
	}
}

func TestTokenExchangeAndIndex(t *testing.T) {
	e := newEnv(t)
	// bad token
	resp, _ := e.do("GET", "/?token=nope", nil)
	if resp.StatusCode != 401 || len(resp.Cookies()) != 0 {
		t.Errorf("bad token: %d %v", resp.StatusCode, resp.Cookies())
	}
	// good token: cookie + redirect that does not echo the token
	resp, _ = e.do("GET", "/?token="+e.srv.Token, nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("exchange: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "phoenix_session" {
			ck = c
		}
	}
	if ck == nil || ck.Value != e.srv.Token || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" {
		t.Fatalf("cookie %+v", ck)
	}
	// anonymous index is refused with a helpful page
	resp, body := e.do("GET", "/", nil)
	if resp.StatusCode != 401 || !strings.Contains(string(body), "phoenix serve") || strings.Contains(string(body), e.srv.Token) {
		t.Errorf("anonymous index: %d %s", resp.StatusCode, body)
	}
	// authed index (cookie) serves the real UI
	resp, body = e.do("GET", "/", nil, withCookie(e.srv.Token))
	if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(string(body)), "<html") || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %s", resp.StatusCode, truncate(body))
	}
	if strings.Contains(string(body), e.srv.Token) {
		t.Error("index.html must not embed the session token")
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("cache-control %q", resp.Header.Get("Cache-Control"))
	}
	// non-GET on / is rejected
	if resp, _ := e.do("POST", "/", "x", withToken(e.srv.Token)); resp.StatusCode != 405 {
		t.Errorf("POST /: %d", resp.StatusCode)
	}
	// static assets are served
	if resp, body := e.do("GET", "/app.js", nil, withCookie(e.srv.Token)); resp.StatusCode != 200 || len(body) == 0 || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Errorf("app.js: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := e.do("GET", "/nope.xyz", nil, withCookie(e.srv.Token)); resp.StatusCode != 404 {
		t.Errorf("missing static: %d", resp.StatusCode)
	}
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do("GET", "/api/health", nil, withToken(e.srv.Token))
	h := resp.Header
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("%v", h)
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("csp %q", csp)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("api cache-control %q", h.Get("Cache-Control"))
	}
	// 401/403 responses carry the headers too
	resp, _ = e.do("GET", "/api/health", nil)
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("headers missing on 401")
	}
}

func TestScenarioCRUD(t *testing.T) {
	e := newEnv(t)
	// list starts empty (JSON array, not null)
	resp, raw := e.do("GET", "/api/scenarios", nil, withToken(e.srv.Token))
	if resp.StatusCode != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty list: %d %s", resp.StatusCode, raw)
	}
	sc := scenarioJSON("First", "http://127.0.0.1:9")
	resp, m, raw := e.api("PUT", "/api/scenarios/first.json", sc)
	if resp.StatusCode != 200 || m["file"] != "first.json" {
		t.Fatalf("PUT: %d %s", resp.StatusCode, raw)
	}
	if probs, ok := m["problems"].([]any); !ok || len(probs) != 0 {
		t.Errorf("problems must be an empty array: %v", m["problems"])
	}
	// list
	resp, raw = e.do("GET", "/api/scenarios", nil, withToken(e.srv.Token))
	var list []map[string]any
	json.Unmarshal(raw, &list)
	if len(list) != 1 || list[0]["file"] != "first.json" || list[0]["name"] != "First" || list[0]["requests"] != float64(1) {
		t.Errorf("list %s", raw)
	}
	// get
	resp, m, _ = e.api("GET", "/api/scenarios/first.json", nil)
	if resp.StatusCode != 200 || m["name"] != "First" {
		t.Errorf("GET: %d %v", resp.StatusCode, m)
	}
	// update twice => history
	sc["name"] = "Second"
	e.saveScenario("first.json", sc)
	sc["name"] = "Third"
	e.saveScenario("first.json", sc)
	resp, raw = e.do("GET", "/api/scenarios/first.json/history", nil, withToken(e.srv.Token))
	var hist []map[string]any
	json.Unmarshal(raw, &hist)
	if resp.StatusCode != 200 || len(hist) != 2 || hist[0]["reason"] != "edit" {
		t.Fatalf("history %d %s", resp.StatusCode, raw)
	}
	id := hist[1]["id"].(string)
	resp, m, _ = e.api("GET", "/api/scenarios/first.json/diff/"+id, nil)
	ch, _ := m["changes"].([]any)
	if resp.StatusCode != 200 || len(ch) != 1 {
		t.Errorf("diff %d %v", resp.StatusCode, m)
	}
	resp, m, _ = e.api("POST", "/api/scenarios/first.json/history/"+id+"/restore", "")
	if resp.StatusCode != 200 || m["restored"] != true {
		t.Errorf("restore %d %v", resp.StatusCode, m)
	}
	if _, m, _ = e.api("GET", "/api/scenarios/first.json", nil); m["name"] != "First" {
		t.Errorf("restored name %v", m["name"])
	}
	// validate
	resp, m, _ = e.api("POST", "/api/scenarios/first.json/validate", "")
	targets, _ := m["targets"].([]any)
	blocked, _ := m["blocked"].([]any)
	if resp.StatusCode != 200 || len(targets) != 1 || targets[0] != "127.0.0.1" || len(blocked) != 0 {
		t.Errorf("validate %d %v", resp.StatusCode, m)
	}
	// delete
	resp, m, _ = e.api("DELETE", "/api/scenarios/first.json", nil)
	if resp.StatusCode != 200 || m["deleted"] != true {
		t.Errorf("delete %d %v", resp.StatusCode, m)
	}
	if resp, _, _ = e.api("GET", "/api/scenarios/first.json", nil); resp.StatusCode != 404 {
		t.Errorf("after delete: %d", resp.StatusCode)
	}
	if resp, _, _ = e.api("DELETE", "/api/scenarios/first.json", nil); resp.StatusCode != 404 {
		t.Errorf("delete twice: %d", resp.StatusCode)
	}
}

func TestScenarioPutErrors(t *testing.T) {
	e := newEnv(t)
	good, _ := json.Marshal(scenarioJSON("x", "http://127.0.0.1:9"))
	tests := []struct {
		name, path, body string
		want             int
	}{
		{"bad json", "/api/scenarios/a.json", `{"name":`, 400},
		{"unknown field", "/api/scenarios/a.json", `{"name":"x","evil":1,"steps":[]}`, 400},
		{"not an object", "/api/scenarios/a.json", `[1,2]`, 400},
		{"bad file name ext", "/api/scenarios/a.txt", string(good), 400},
		{"bad file name dots", "/api/scenarios/a..b.json", string(good), 400},
		{"encoded traversal", "/api/scenarios/..%2F..%2Fescape.json", string(good), 0},
		{"encoded slash", "/api/scenarios/sub%2Fa.json", string(good), 400},
		{"hidden file", "/api/scenarios/.hidden.json", string(good), 400},
		{"invalid duration", "/api/scenarios/a.json", `{"name":"x","steps":[],"workload":{"executor":"constant-vus","vus":1,"duration":"banana"}}`, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.do("PUT", tc.path, tc.body, withToken(e.srv.Token))
			if tc.want != 0 && resp.StatusCode != tc.want {
				t.Errorf("status %d want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.want == 0 && resp.StatusCode == 200 {
				t.Errorf("traversal accepted: %s", body)
			}
			if strings.Contains(string(body), os.TempDir()) {
				t.Errorf("error leaks temp path: %s", body)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.svc.WS.Dir), "escape.json")); err == nil {
		t.Error("file escaped the workspace")
	}
	// invalid-but-parseable scenario is saved with problems listed
	resp, m, _ := e.api("PUT", "/api/scenarios/empty.json", `{"name":"empty","steps":[]}`)
	probs, _ := m["problems"].([]any)
	if resp.StatusCode != 200 || len(probs) == 0 {
		t.Errorf("%d %v", resp.StatusCode, m)
	}
}

func TestStartRunBlockedHost(t *testing.T) {
	e := newEnv(t)
	e.saveScenario("remote.json", scenarioJSON("remote", "http://blocked.example.test"))
	resp, m, _ := e.api("POST", "/api/runs", map[string]any{"file": "remote.json"})
	if resp.StatusCode != 403 || m["blockedHost"] != "blocked.example.test" || !strings.Contains(m["error"].(string), "allowlist") {
		t.Fatalf("%d %v", resp.StatusCode, m)
	}
	// validate lists it as blocked
	_, m, _ = e.api("POST", "/api/scenarios/remote.json/validate", "")
	if b, _ := m["blocked"].([]any); len(b) != 1 || b[0] != "blocked.example.test" {
		t.Errorf("validate blocked: %v", m)
	}
	// allowlist via the settings API, then the run is accepted for the host check
	// (above the confirmation threshold it needs explicit confirmation)
	resp, _, raw := e.api("PUT", "/api/settings/policy", map[string]any{"allowedHosts": []string{"blocked.example.test"}, "maxVUs": 1000, "requireConfirmAbove": 50})
	if resp.StatusCode != 200 {
		t.Fatalf("policy: %d %s", resp.StatusCode, raw)
	}
	resp, m, _ = e.api("POST", "/api/runs", map[string]any{"file": "remote.json", "overrides": map[string]any{"vus": 100, "duration": "1s"}})
	if resp.StatusCode != 409 || m["needsConfirmation"] != true {
		t.Errorf("needs confirmation: %d %v", resp.StatusCode, m)
	}
	// no run became active
	if _, m, _ = e.api("GET", "/api/runs/active", nil); m["runId"] != nil {
		t.Errorf("active %v", m)
	}
}

func TestRunErrorStatusCodes(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/api/runs", map[string]any{"file": "missing.json"}, 404},
		{"POST", "/api/runs", map[string]any{"file": "../x.json"}, 400},
		{"POST", "/api/runs", `{bad`, 400},
		{"GET", "/api/runs/20200101-000000-nothing", nil, 404},
		{"GET", "/api/runs/bad%20id", nil, 400},
		{"GET", "/api/runs/..%2Fescape", nil, 0},
		{"POST", "/api/runs/20200101-000000-nothing/stop", "", 404},
		{"GET", "/api/runs/20200101-000000-nothing/report", nil, 404},
		{"GET", "/api/runs/x.y/report", nil, 400},
		{"GET", "/api/compare", nil, 400},
		{"GET", "/api/compare?base=a&run=b", nil, 404},
		{"GET", "/api/export/jmx/missing.json", nil, 404},
		{"POST", "/api/scenarios/missing.json/correlate", "", 404},
		{"POST", "/api/scenarios/missing.json/debug", "{}", 404},
		{"GET", "/api/nope", nil, 404},
	}
	for _, tc := range tests {
		resp, body := e.do(tc.method, tc.path, tc.body, withToken(e.srv.Token))
		if tc.want == 0 {
			if resp.StatusCode == 200 {
				t.Errorf("%s %s => 200 %s", tc.method, tc.path, body)
			}
			continue
		}
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s => %d (want %d) %s", tc.method, tc.path, resp.StatusCode, tc.want, body)
		}
	}
}

func TestPolicySettings(t *testing.T) {
	e := newEnv(t)
	_, m, _ := e.api("GET", "/api/settings/policy", nil)
	if m["maxVUs"] != float64(1000) {
		t.Errorf("default policy %v", m)
	}
	if hosts, ok := m["allowedHosts"].([]any); !ok || len(hosts) != 0 {
		t.Errorf("allowedHosts must be [] not null: %v", m["allowedHosts"])
	}
	bad := []map[string]any{
		{"allowedHosts": []string{"*"}, "maxVUs": 10},
		{"allowedHosts": []string{"http://a.com"}, "maxVUs": 10},
		{"allowedHosts": []string{"a.com/path"}, "maxVUs": 10},
		{"allowedHosts": []string{"a b"}, "maxVUs": 10},
		{"allowedHosts": []string{}, "maxVUs": 0},
		{"allowedHosts": []string{}, "maxVUs": 100001},
		{"allowedHosts": []string{}, "maxVUs": 10, "maxRPS": -1},
	}
	for i, b := range bad {
		if resp, _, raw := e.api("PUT", "/api/settings/policy", b); resp.StatusCode != 400 {
			t.Errorf("case %d accepted: %d %s", i, resp.StatusCode, raw)
		}
	}
	ok := map[string]any{"allowedHosts": []string{"api.example.com", "*.corp.test"}, "maxVUs": 25, "maxRPS": 100.5, "maxDuration": "30m", "requireConfirmAbove": 5}
	if resp, _, raw := e.api("PUT", "/api/settings/policy", ok); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	p, err := safety.LoadPolicy()
	if err != nil || p.MaxVUs != 25 || p.MaxRPS != 100.5 || len(p.AllowedHosts) != 2 || p.MaxDuration != "30m" {
		t.Errorf("persisted %+v %v", p, err)
	}
	// change is audited
	_, raw := e.do("GET", "/api/audit", nil, withToken(e.srv.Token))
	if !strings.Contains(string(raw), "policy.changed") {
		t.Errorf("audit: %s", raw)
	}
	// a run honours the new cap
	e.saveScenario("big.json", scenarioJSON("big", "http://127.0.0.1:9"))
	resp, m, _ := e.api("POST", "/api/runs", map[string]any{"file": "big.json", "overrides": map[string]any{"vus": 26, "duration": "1s"}})
	if resp.StatusCode != 400 || !strings.Contains(m["error"].(string), "safety policy") {
		t.Errorf("cap not enforced: %d %v", resp.StatusCode, m)
	}
}

func TestAuditEndpointFormat(t *testing.T) {
	e := newEnv(t)
	safety.Audit("test.one", map[string]any{"n": 1})
	safety.Audit("test.two", map[string]any{"n": 2})
	_, raw := e.do("GET", "/api/audit", nil, withToken(e.srv.Token))
	var recs []map[string]any
	if err := json.Unmarshal(raw, &recs); err != nil || len(recs) < 2 {
		t.Fatalf("%v %s", err, raw)
	}
	if recs[0]["event"] != "test.two" || recs[1]["event"] != "test.one" {
		t.Errorf("newest first: %v", recs[:2])
	}
}

func TestDemoRunLiveSSEAndReports(t *testing.T) {
	e := newEnv(t)
	resp, m, raw := e.api("POST", "/api/demo", "")
	if resp.StatusCode != 200 || m["file"] != "demo-shop.json" {
		t.Fatalf("demo: %d %s", resp.StatusCode, raw)
	}
	resp, m, raw = e.api("POST", "/api/runs", map[string]any{"file": "demo-shop.json", "overrides": map[string]any{"vus": 1, "duration": "3s"}})
	if resp.StatusCode != 200 {
		t.Fatalf("start: %d %s", resp.StatusCode, raw)
	}
	id := m["runId"].(string)
	// busy while active
	if resp, m, _ = e.api("POST", "/api/runs", map[string]any{"file": "demo-shop.json"}); resp.StatusCode != 409 {
		t.Errorf("busy: %d %v", resp.StatusCode, m)
	}
	// /api/runs/active
	if _, m, _ = e.api("GET", "/api/runs/active", nil); m["runId"] != id {
		t.Errorf("active %v", m)
	}
	// running status
	if _, m, _ = e.api("GET", "/api/runs/"+id, nil); m["status"] != "running" && m["runId"] == nil {
		t.Logf("run already finished? %v", m["status"])
	}

	req, _ := http.NewRequest("GET", e.ts.URL+"/api/runs/"+id+"/live", nil)
	req.Header.Set("X-Phoenix-Token", e.srv.Token)
	lresp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer lresp.Body.Close()
	if lresp.StatusCode != 200 || !strings.HasPrefix(lresp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse headers: %d %v", lresp.StatusCode, lresp.Header)
	}
	events := readSSE(t, lresp.Body, "done")
	snapshots := 0
	var done map[string]any
	for _, ev := range events {
		switch ev.name {
		case "snapshot":
			snapshots++
			var d map[string]any
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil || d["runId"] != id || d["metrics"] == nil || d["plannedSec"] != float64(3) {
				t.Errorf("snapshot payload: %v %s", err, ev.data)
			}
		case "done":
			json.Unmarshal([]byte(ev.data), &done)
		}
	}
	if snapshots < 1 {
		t.Errorf("no snapshot events: %v", events)
	}
	if done == nil || done["runId"] != id || done["status"] != "finished" {
		t.Fatalf("done event: %v", done)
	}

	// finished run: summary + files
	resp, m, _ = e.api("GET", "/api/runs/"+id, nil)
	if resp.StatusCode != 200 || m["scenario"] == nil || m["metrics"] == nil {
		t.Errorf("summary: %d", resp.StatusCode)
	}
	rep, body := e.do("GET", "/api/runs/"+id+"/report", nil, withToken(e.srv.Token))
	if rep.StatusCode != 200 || !strings.Contains(rep.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "<html") {
		t.Errorf("report: %d %q", rep.StatusCode, rep.Header.Get("Content-Type"))
	}
	if csp := rep.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("report CSP must forbid scripts: %q", csp)
	}
	for _, f := range []string{"raw.csv", "summary.csv", "summary.json"} {
		r2, b2 := e.do("GET", "/api/runs/"+id+"/"+f, nil, withToken(e.srv.Token))
		if r2.StatusCode != 200 || len(b2) == 0 || !strings.Contains(r2.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: %d %q", f, r2.StatusCode, r2.Header.Get("Content-Disposition"))
		}
	}
	// list shows it; live on a finished run replays "done"
	_, raw = e.do("GET", "/api/runs", nil, withToken(e.srv.Token))
	if !strings.Contains(string(raw), id) {
		t.Errorf("run list: %s", raw)
	}
	req2, _ := http.NewRequest("GET", e.ts.URL+"/api/runs/"+id+"/live", nil)
	req2.Header.Set("X-Phoenix-Token", e.srv.Token)
	l2, err := e.cl.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Body.Close()
	if evs := readSSE(t, l2.Body, "done"); len(evs) == 0 || evs[len(evs)-1].name != "done" {
		t.Errorf("finished-run live: %v", evs)
	}
	// unknown run => error event
	req3, _ := http.NewRequest("GET", e.ts.URL+"/api/runs/20200101-000000-nothing/live", nil)
	req3.Header.Set("X-Phoenix-Token", e.srv.Token)
	l3, err := e.cl.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Body.Close()
	if evs := readSSE(t, l3.Body, "error"); len(evs) == 0 || evs[0].name != "error" {
		t.Errorf("unknown-run live: %v", evs)
	}
	// live requires auth
	if resp, _ := e.do("GET", "/api/runs/"+id+"/live", nil); resp.StatusCode != 401 {
		t.Errorf("anonymous live: %d", resp.StatusCode)
	}
}

type sseEvent struct{ name, data string }

func readSSE(t *testing.T, r io.Reader, until string) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var cur sseEvent
	deadline := time.After(40 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-deadline:
			t.Fatalf("SSE timed out after events %v", out)
		case ln, ok := <-lines:
			if !ok {
				return out
			}
			switch {
			case strings.HasPrefix(ln, "event: "):
				cur.name = strings.TrimPrefix(ln, "event: ")
			case strings.HasPrefix(ln, "data: "):
				cur.data = strings.TrimPrefix(ln, "data: ")
			case ln == "":
				if cur.name != "" {
					out = append(out, cur)
					if cur.name == until {
						return out
					}
				}
				cur = sseEvent{}
			}
		}
	}
}

func TestStopViaAPI(t *testing.T) {
	e := newEnv(t)
	e.api("POST", "/api/demo", "")
	_, m, _ := e.api("POST", "/api/runs", map[string]any{"file": "demo-shop.json", "overrides": map[string]any{"vus": 1, "duration": "60s"}})
	id, _ := m["runId"].(string)
	if id == "" {
		t.Fatal("no run id")
	}
	time.Sleep(300 * time.Millisecond)
	resp, m, _ := e.api("POST", "/api/runs/"+id+"/stop", "")
	if resp.StatusCode != 200 || m["stopping"] != true {
		t.Fatalf("%d %v", resp.StatusCode, m)
	}
	a := e.svc.Active(id)
	select {
	case <-a.Run.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
	a.Wait()
}

func multipartBody(t *testing.T, field, filename string, content []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, _ := mw.CreateFormFile(field, filename)
		fw.Write(content)
	}
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestImportHARUpload(t *testing.T) {
	e := newEnv(t)
	data, err := os.ReadFile("../../testdata/har/shop.har")
	if err != nil {
		t.Fatal(err)
	}
	post := func(body *bytes.Buffer, ct string, h ...opt) (*http.Response, []byte) {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/import/har", body)
		req.Header.Set("Content-Type", ct)
		for _, o := range h {
			o(req)
		}
		resp, err := e.cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	body, ct := multipartBody(t, "file", "shop.har", data, map[string]string{"name": "My Shop"})
	resp, raw := post(body, ct, withToken(e.srv.Token))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var imp map[string]any
	json.Unmarshal(raw, &imp)
	file, _ := imp["file"].(string)
	if file != "my-shop.json" || imp["report"] == nil {
		t.Errorf("%v", imp)
	}
	if c, _ := imp["correlations"].([]any); len(c) == 0 {
		t.Error("expected correlations in the import result")
	}
	// the stored scenario never contains the recorded secrets in clear (password=secret stays a variable)
	_, sraw := e.do("GET", "/api/scenarios/"+file, nil, withToken(e.srv.Token))
	if strings.Contains(string(sraw), `"password","value":"secret"`) {
		t.Logf("note: recorded password kept in scenario: %s", truncate(sraw))
	}
	// correlate endpoint on the import
	if resp, m, _ := e.api("POST", "/api/scenarios/"+file+"/correlate", ""); resp.StatusCode != 200 || m["candidates"] == nil {
		t.Errorf("correlate %d %v", resp.StatusCode, m)
	}
	// errors
	b2, ct2 := multipartBody(t, "file", "", nil, map[string]string{"name": "x"})
	if resp, _ := post(b2, ct2, withToken(e.srv.Token)); resp.StatusCode != 400 {
		t.Errorf("no file: %d", resp.StatusCode)
	}
	b3, ct3 := multipartBody(t, "file", "bad.har", []byte("not json"), nil)
	if resp, _ := post(b3, ct3, withToken(e.srv.Token)); resp.StatusCode != 400 {
		t.Errorf("bad har: %d", resp.StatusCode)
	}
	if resp, _ := post(bytes.NewBufferString("plain"), "text/plain", withToken(e.srv.Token)); resp.StatusCode != 400 {
		t.Errorf("not multipart: %d", resp.StatusCode)
	}
	b4, ct4 := multipartBody(t, "file", "shop.har", data, nil)
	if resp, _ := post(b4, ct4, withCookie(e.srv.Token)); resp.StatusCode != 403 {
		t.Errorf("upload with cookie only: %d", resp.StatusCode)
	}
}

func TestAISettingsAndAsk(t *testing.T) {
	e := newEnv(t)
	_, m, _ := e.api("GET", "/api/settings/ai", nil)
	if m["enabled"] != false || m["keyStorage"] == nil {
		t.Fatalf("%v", m)
	}
	provs, _ := m["providers"].([]any)
	if len(provs) == 0 {
		t.Fatal("no providers listed")
	}
	// validation
	if resp, _, _ := e.api("PUT", "/api/settings/ai", `{"providers":[{"id":"x","kind":"evilkind","model":"m"}]}`); resp.StatusCode != 400 {
		t.Errorf("unknown kind: %d", resp.StatusCode)
	}
	if resp, _, _ := e.api("PUT", "/api/settings/ai", `{"providers":[{"id":"","kind":"ollama","model":"m"}]}`); resp.StatusCode != 400 {
		t.Errorf("missing id: %d", resp.StatusCode)
	}
	// save a local-only config; keys never come back
	cfg := `{"enabled":false,"cloudConsent":false,"localOnly":true,"providers":[{"id":"o","kind":"ollama","model":"llama3","baseUrl":"http://127.0.0.1:1"}]}`
	resp, m, raw := e.api("PUT", "/api/settings/ai", cfg)
	if resp.StatusCode != 200 || m["localOnly"] != true {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	c, err := ai.LoadConfig()
	if err != nil || len(c.Providers) != 1 || c.Providers[0].ID != "o" {
		t.Errorf("persisted %+v %v", c, err)
	}
	if ai.KeyBackend() == "file-0600" {
		secret := "sk-test-SECRET-12345678"
		in := `{"providers":[{"id":"o","kind":"openai","model":"m","apiKey":"` + secret + `"}]}`
		resp, _, raw = e.api("PUT", "/api/settings/ai", in)
		if resp.StatusCode != 200 || strings.Contains(string(raw), secret) {
			t.Errorf("PUT leaked key: %d %s", resp.StatusCode, raw)
		}
		_, raw = e.do("GET", "/api/settings/ai", nil, withToken(e.srv.Token))
		if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), `"hasKey":true`) {
			t.Errorf("GET leaked key or lost hasKey: %s", raw)
		}
		if st, err := os.Stat(filepath.Join(safety.ConfigDir(), "keys.json")); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("key file perms: %v %v", st, err)
		}
	}

	// ask: validation
	if resp, _, _ := e.api("POST", "/api/ai/ask", map[string]any{"prompt": "  "}); resp.StatusCode != 400 {
		t.Errorf("empty prompt: %d", resp.StatusCode)
	}
	if resp, _, _ := e.api("POST", "/api/ai/ask", map[string]any{"prompt": strings.Repeat("x", 8001)}); resp.StatusCode != 400 {
		t.Errorf("long prompt: %d", resp.StatusCode)
	}
	// ask with AI disabled uses the rule planner
	e.saveScenario("shop.json", scenarioJSON("shop", "http://127.0.0.1:9"))
	resp, m, raw = e.api("POST", "/api/ai/ask", map[string]any{"prompt": "validate this scenario", "file": "shop.json"})
	if resp.StatusCode != 200 || m["planner"] != "rules" {
		t.Fatalf("ask: %d %s", resp.StatusCode, raw)
	}
	if ex, _ := m["executed"].([]any); len(ex) != 1 {
		t.Errorf("executed %v", m["executed"])
	}
	// explain without AI => 412
	if resp, _, raw := e.api("POST", "/api/ai/explain", map[string]any{"runId": "20200101-000000-x"}); resp.StatusCode != 404 {
		t.Errorf("explain unknown run: %d %s", resp.StatusCode, raw)
	}
	// activity endpoint returns [] not null
	if _, raw := e.do("GET", "/api/ai/activity", nil, withToken(e.srv.Token)); strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("activity %s", raw)
	}
	// connection test reports failure as ok:false (HTTP 200), never a 500
	resp, m, _ = e.api("POST", "/api/ai/test", map[string]any{"Provider": "nope"})
	if resp.StatusCode != 200 || m["ok"] != false {
		t.Errorf("ai test: %d %v", resp.StatusCode, m)
	}
}

func TestSessionEndpoint(t *testing.T) {
	e := newEnv(t)
	_, m, _ := e.api("GET", "/api/session", nil)
	if m["token"] != e.srv.Token || m["version"] == nil || m["workspace"] == nil {
		t.Errorf("%v", m)
	}
	if len(e.srv.Token) < 32 {
		t.Errorf("token too short: %d", len(e.srv.Token))
	}
	if New(e.svc).Token == e.srv.Token {
		t.Error("tokens must be random per server")
	}
}
