package engine

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/lelanga/loadmeter/internal/model"
)

// digestServer verifies RFC 7616 digest credentials (qop=auth) and records
// what it saw. Nonces rotate every `rotate` successful requests (stale=true).
type digestServer struct {
	mu         sync.Mutex
	user, pass string
	realm      string
	algs       []string // offered algorithms, in header order
	nonce      int
	used       int
	rotate     int
	challenges int
	ok         int
	lastNC     map[string]string
	authz      []string
}

func (d *digestServer) curNonce() string { return fmt.Sprintf("nonce-%d", d.nonce) }

func (d *digestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := r.Header.Get("Authorization")
	d.authz = append(d.authz, h)
	stale := false
	if strings.HasPrefix(h, "Digest ") {
		p := parseChallenges([]string{h})[0].params
		alg := p["algorithm"]
		newH := map[string]func() hash.Hash{"MD5": md5.New, "SHA-256": sha256.New}[alg]
		hx := func(s string) string { x := newH(); x.Write([]byte(s)); return hex.EncodeToString(x.Sum(nil)) }
		ha1 := hx(d.user + ":" + d.realm + ":" + d.pass)
		ha2 := hx(r.Method + ":" + r.URL.RequestURI())
		want := hx(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
		switch {
		case p["nonce"] != d.curNonce():
			stale = true
		case p["response"] == want && p["uri"] == r.URL.RequestURI() && p["username"] == d.user && p["qop"] == "auth" &&
			p["nc"] > d.lastNC[p["nonce"]] && p["opaque"] == "op4que":
			d.lastNC[p["nonce"]] = p["nc"]
			d.ok++
			d.used++
			if d.rotate > 0 && d.used%d.rotate == 0 {
				d.nonce++
			}
			fmt.Fprint(w, `{"auth":"ok"}`)
			return
		}
	}
	d.challenges++
	for _, a := range d.algs {
		v := fmt.Sprintf(`Digest realm="%s", qop="auth,auth-int", algorithm=%s, nonce="%s", opaque="op4que"`, d.realm, a, d.curNonce())
		if stale {
			v += ", stale=true"
		}
		w.Header().Add("WWW-Authenticate", v)
	}
	w.WriteHeader(401)
}

func newDigest(t *testing.T, algs ...string) (*digestServer, string) {
	d := &digestServer{user: "alice", pass: "s3cr3t", realm: "loadmeter@test", algs: algs, lastNC: map[string]string{}}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return d, srv.URL
}

func authSc(base string, rules []model.AuthRule, w model.Workload, steps ...model.Step) *model.Scenario {
	sc := newSc(base, w, steps...)
	sc.Defaults.Auth = rules
	sc.Variables["secret_pw"] = "s3cr3t"
	return sc
}

func TestDigestAuth(t *testing.T) {
	t.Parallel()
	for _, algs := range [][]string{{"MD5"}, {"SHA-256"}, {"MD5", "SHA-256"}} {
		d, base := newDigest(t, algs...)
		d.rotate = 3
		rec := &recorder{}
		rules := []model.AuthRule{{URLPrefix: "/", Type: model.AuthDigest, Username: "alice", Password: "${secret_pw}"}}
		st := withAssert(get("api", "/api/x?q=1"), model.Assertion{Type: model.AsContains, Value: `"auth":"ok"`})
		sc := authSc(base, rules, perVU(1, 7), st)
		_, snap := runSc(t, sc, opts(rec))
		if snap.Requests.Errors != 0 || snap.Requests.Count != 7 {
			t.Fatalf("algs=%v: %d errors %v", algs, snap.Requests.Errors, snap.Requests.ErrorKind)
		}
		d.mu.Lock()
		// One initial challenge, then the cached challenge is reused with an
		// incrementing nc; every 3rd success rotates the nonce (stale) -> 2 more.
		if d.ok != 7 || d.challenges != 3 {
			t.Errorf("algs=%v: ok=%d challenges=%d (want 7 / 3)", algs, d.ok, d.challenges)
		}
		var ncs []string
		for _, a := range d.authz {
			if a == "" {
				continue
			}
			p := parseChallenges([]string{a})[0].params
			ncs = append(ncs, p["nc"])
			if want := algs[len(algs)-1]; p["algorithm"] != want {
				t.Errorf("algs=%v: client chose %s, want the strongest %s", algs, p["algorithm"], want)
			}
		}
		d.mu.Unlock()
		if len(ncs) < 2 || ncs[0] != "00000001" || ncs[1] != "00000002" {
			t.Errorf("nonce count must increment on reuse: %v", ncs)
		}
		for _, ex := range rec.all() {
			if a := ex.ReqHeaders["Authorization"]; a != "" && a != "[REDACTED digest]" {
				t.Errorf("credentials leaked to the inspector: %q", a)
			}
		}
	}
}

func TestDigestWrongPasswordReports401(t *testing.T) {
	t.Parallel()
	d, base := newDigest(t, "MD5")
	rules := []model.AuthRule{{Type: model.AuthDigest, Username: "alice", Password: "${secret_pw}"}}
	sc := authSc(base, rules, perVU(1, 1), get("api", "/x"))
	sc.Variables["secret_pw"] = "wrong"
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.Count != 1 || snap.Requests.ErrorKind["http_4xx"] != 1 {
		t.Errorf("one sample with the final 401: %+v", snap.Requests)
	}
	if d.challenges != 2 {
		t.Errorf("exactly one authenticated resend: %d challenges", d.challenges)
	}
}

func TestBasicAndBearerByPrefix(t *testing.T) { // not parallel: uses t.Setenv
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen["other"] = r.Header.Get("Authorization")
		mu.Unlock()
	}))
	defer other.Close()
	t.Setenv("LOADMETER_TEST_TOKEN", "tok-from-env")
	rules := []model.AuthRule{
		{URLPrefix: "/api", Type: model.AuthBasic, Username: "bob", Password: "${secret_pw}"},
		{URLPrefix: "/api/v2", Type: model.AuthBearer, Token: "${__env(LOADMETER_TEST_TOKEN)}"},
	}
	rec := &recorder{}
	explicit := get("explicit", "/api/own")
	explicit.Request.Headers = []model.Header{{Name: "Authorization", Value: "Custom x"}}
	sc := authSc(srv.URL, rules, perVU(1, 1),
		get("basic", "/api/users"), get("bearer", "/api/v2/users"), get("none", "/apix"), get("public", "/public"),
		explicit, get("redir", "/redir?to="+url.QueryEscape(other.URL+"/api/users")))
	_, snap := runSc(t, sc, opts(rec))
	if snap.Requests.Errors != 0 {
		t.Fatalf("%v", snap.Requests.ErrorKind)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:s3cr3t")); seen["/api/users"] != want {
		t.Errorf("basic: %q", seen["/api/users"])
	}
	if seen["/api/v2/users"] != "Bearer tok-from-env" {
		t.Errorf("longest prefix must pick bearer: %q", seen["/api/v2/users"])
	}
	if seen["/apix"] != "" || seen["/public"] != "" {
		t.Errorf("prefix must end on a path boundary: %q %q", seen["/apix"], seen["/public"])
	}
	if seen["/api/own"] != "Custom x" {
		t.Errorf("an explicit Authorization header wins: %q", seen["/api/own"])
	}
	if seen["other"] != "" {
		t.Errorf("credentials must not follow a redirect to a host no rule matches: %q", seen["other"])
	}
	for _, ex := range rec.all() {
		a := ex.ReqHeaders["Authorization"]
		if strings.Contains(a, "s3cr3t") || strings.Contains(a, "tok-from-env") || strings.Contains(a, base64.StdEncoding.EncodeToString([]byte("bob:s3cr3t"))) {
			t.Errorf("%s: secret visible in inspector output: %q", ex.Step, a)
		}
	}
}

func TestAuthValidation(t *testing.T) {
	sc := newSc("http://127.0.0.1", perVU(1, 1), get("x", "/"))
	sc.Defaults.Auth = []model.AuthRule{
		{Type: model.AuthBasic, Username: "u", Password: "literal"},
		{Type: model.AuthBearer, Token: "Bearer-${secret_t}"},
		{Type: "ntlm"},
		{Type: model.AuthDigest, Username: "u", Password: "${secret_ok}${__env(LOADMETER_X)}"},
	}
	probs := strings.Join(sc.Validate(), "\n")
	for _, want := range []string{"auth[0]: password/token must be given only", "auth[1]: password/token must be given only", `auth[2]: unknown type "ntlm"`} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing problem %q in:\n%s", want, probs)
		}
	}
	if strings.Contains(probs, "auth[3]") {
		t.Errorf("secret/env references are valid: %s", probs)
	}
}

func TestParseChallenges(t *testing.T) {
	cs := parseChallenges([]string{`Basic realm="a, b", Digest realm="r\"q", nonce="n1", qop="auth,auth-int", algorithm=SHA-256`, `Bearer error="invalid_token"`})
	if len(cs) != 3 || cs[0].scheme != "Basic" || cs[0].params["realm"] != "a, b" {
		t.Fatalf("%+v", cs)
	}
	if cs[1].params["realm"] != `r"q` || cs[1].params["algorithm"] != "SHA-256" || cs[1].params["qop"] != "auth,auth-int" {
		t.Errorf("%+v", cs[1])
	}
	if cs[2].scheme != "Bearer" || cs[2].params["error"] != "invalid_token" {
		t.Errorf("%+v", cs[2])
	}
}

func TestPrefixMatch(t *testing.T) {
	for _, c := range []struct {
		full, prefix string
		want         bool
	}{
		{"https://api.example.com/x", "https://api.example.com", true},
		{"https://api.example.com.evil.test/x", "https://api.example.com", false},
		{"https://api.example.com/apix", "https://api.example.com/api", false},
		{"https://api.example.com/api?x", "https://api.example.com/api", true},
		{"https://api.example.com/api/", "https://api.example.com/api/", true},
	} {
		if got := prefixMatch(c.full, c.prefix); got != c.want {
			t.Errorf("%s ~ %s = %v", c.full, c.prefix, got)
		}
	}
}
