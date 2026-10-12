package safety

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// recordingDial captures the address actually dialled (no network I/O).
type recordingDial struct {
	mu    sync.Mutex
	addrs []string
}

var errDialed = errors.New("dialled")

func (r *recordingDial) dial(_ context.Context, _, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, addr)
	r.mu.Unlock()
	return nil, errDialed
}

func TestGuardDialerWithDNSOverride(t *testing.T) {
	p := Policy{AllowedHosts: []string{"api.example.com", "10.1.2.3"}}
	cases := []struct {
		name     string
		hosts    map[string]string
		addr     string
		wantAddr string // dialled address when allowed
		blocked  string // host named in ErrHostNotAllowed
	}{
		{"allowed name to allowed IP", map[string]string{"api.example.com": "10.1.2.3"}, "api.example.com:443", "10.1.2.3:443", ""},
		{"case-insensitive override", map[string]string{"API.Example.com": "10.1.2.3"}, "api.example.com:443", "10.1.2.3:443", ""},
		{"allowed name to loopback", map[string]string{"api.example.com": "127.0.0.1"}, "api.example.com:80", "127.0.0.1:80", ""},
		{"BYPASS: allowed name to non-allowlisted IP", map[string]string{"api.example.com": "10.9.9.9"}, "api.example.com:443", "", "10.9.9.9"},
		{"BYPASS: loopback name to remote IP", map[string]string{"localhost": "10.9.9.9"}, "localhost:8080", "", "10.9.9.9"},
		{"BYPASS: 127.0.0.1 literal to remote IP", map[string]string{"127.0.0.1": "8.8.8.8"}, "127.0.0.1:80", "", "8.8.8.8"},
		{"BYPASS: unspecified address", map[string]string{"api.example.com": "0.0.0.0"}, "api.example.com:80", "", "0.0.0.0"},
		{"non-allowlisted name even to loopback IP", map[string]string{"evil.test": "127.0.0.1"}, "evil.test:80", "", "evil.test"},
		{"no override keeps the name", nil, "api.example.com:443", "api.example.com:443", ""},
		{"IPv6 loopback target", map[string]string{"api.example.com": "::1"}, "api.example.com:443", "[::1]:443", ""},
	}
	for _, c := range cases {
		rd := &recordingDial{}
		dial := p.GuardDialerWith(&net.Dialer{}, DialOptions{Hosts: c.hosts, Dial: rd.dial})
		_, err := dial(context.Background(), "tcp", c.addr)
		var hna *ErrHostNotAllowed
		if c.blocked != "" {
			if !errors.As(err, &hna) || hna.Host != c.blocked {
				t.Errorf("%s: want refusal naming %s, got %v", c.name, c.blocked, err)
			}
			if len(rd.addrs) != 0 {
				t.Errorf("%s: dialled %v despite refusal", c.name, rd.addrs)
			}
			continue
		}
		if !errors.Is(err, errDialed) || len(rd.addrs) != 1 || rd.addrs[0] != c.wantAddr {
			t.Errorf("%s: dialled %v err %v, want %s", c.name, rd.addrs, err, c.wantAddr)
		}
	}
	// An override that is not an IP literal is refused (no second DNS hop).
	rd := &recordingDial{}
	dial := p.GuardDialerWith(&net.Dialer{}, DialOptions{Hosts: map[string]string{"api.example.com": "other.example.com"}, Dial: rd.dial})
	var inv *ErrDNSOverrideInvalid
	if _, err := dial(context.Background(), "tcp", "api.example.com:443"); !errors.As(err, &inv) || len(rd.addrs) != 0 {
		t.Errorf("hostname override must be refused: %v %v", err, rd.addrs)
	}
}

func TestGuardProxy(t *testing.T) {
	p := Policy{AllowedHosts: []string{"proxy.corp.test", "api.example.com", "*.internal.test"}}
	if _, err := p.GuardProxy(ProxyConfig{URL: mustURL("http://evil-proxy.test:3128")}); err == nil {
		t.Error("a non-allowlisted proxy must be refused")
	}
	if _, err := p.GuardProxy(ProxyConfig{URL: mustURL("socks5://proxy.corp.test:1080")}); err == nil {
		t.Error("only http/https proxies")
	}
	proxy, err := p.GuardProxy(ProxyConfig{URL: mustURL("http://proxy.corp.test:3128"), NoProxy: []string{"*.internal.test"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		target    string
		via       bool
		blockedAt string
	}{
		{"https://api.example.com/x", true, ""},
		{"https://evil.test/x", false, "evil.test"}, // BYPASS attempt: the proxy would reach it, the guard does not
		{"http://a.internal.test/", false, ""},      // noProxy: direct (dialler checks it)
		{"http://127.0.0.1:8080/", false, ""},       // loopback is this machine: never via the proxy
		{"http://localhost/", false, ""},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", c.target, nil)
		u, err := proxy(req)
		var hna *ErrHostNotAllowed
		switch {
		case c.blockedAt != "":
			if !errors.As(err, &hna) || hna.Host != c.blockedAt {
				t.Errorf("%s: want refusal, got %v %v", c.target, u, err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.target, err)
		case c.via && (u == nil || u.Host != "proxy.corp.test:3128"):
			t.Errorf("%s: want proxy, got %v", c.target, u)
		case !c.via && u != nil:
			t.Errorf("%s: want direct, got %v", c.target, u)
		}
	}
	// The proxy connection itself still goes through the guarded dialler,
	// where a DNS override cannot repoint the proxy to a non-allowlisted IP.
	rd := &recordingDial{}
	dial := p.GuardDialerWith(&net.Dialer{}, DialOptions{Hosts: map[string]string{"proxy.corp.test": "10.66.66.66"}, Dial: rd.dial})
	if _, err := dial(context.Background(), "tcp", "proxy.corp.test:3128"); err == nil || len(rd.addrs) != 0 {
		t.Errorf("proxy host override to a non-allowlisted IP must be refused: %v", err)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
