package safety

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestIsLoopback(t *testing.T) {
	tests := map[string]bool{
		"localhost": true, "LOCALHOST": true, "app.localhost": true, "127.0.0.1": true, "127.5.5.5": true,
		"::1": true, "[::1]": true, "::ffff:127.0.0.1": true,
		"10.0.0.1": false, "192.168.1.1": false, "example.com": false, "localhost.evil.com": false,
		"0.0.0.0": false, "": false, "127.0.0.1.evil.com": false, "8.8.8.8": false,
	}
	for h, want := range tests {
		if got := IsLoopback(h); got != want {
			t.Errorf("IsLoopback(%q)=%v want %v", h, got, want)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	p := Policy{AllowedHosts: []string{"api.example.com", "*.corp.test", " Staging.Example.ORG ", "10.1.2.3"}}
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true}, {"localhost", true}, {"::1", true}, {"[::1]", true},
		{"api.example.com", true}, {"API.EXAMPLE.COM", true}, {"staging.example.org", true},
		{"x.api.example.com", false}, {"example.com", false},
		{"a.corp.test", true}, {"a.b.corp.test", true}, {"corp.test", false}, {"evilcorp.test", false}, {"a.corp.test.evil.com", false},
		{"10.1.2.3", true}, {"10.1.2.4", false}, {"", false}, {"anything.else", false},
	}
	for _, tc := range tests {
		if got := p.HostAllowed(tc.host); got != tc.want {
			t.Errorf("HostAllowed(%q)=%v want %v", tc.host, got, tc.want)
		}
	}
}

func TestHostAllowedNoBareWildcard(t *testing.T) {
	for _, entry := range []string{"*", "*.", ".", ""} {
		p := Policy{AllowedHosts: []string{entry}}
		for _, h := range []string{"example.com", "10.0.0.1", "8.8.8.8", "evil"} {
			if p.HostAllowed(h) {
				t.Errorf("allowlist entry %q must not allow %q", entry, h)
			}
		}
	}
	if !(Policy{}).HostAllowed("127.0.0.1") {
		t.Error("loopback always allowed")
	}
}

func TestGuardDialerRefusal(t *testing.T) {
	p := Policy{}
	dial := p.GuardDialer(&net.Dialer{Timeout: 200 * time.Millisecond})
	start := time.Now()
	for _, addr := range []string{"10.255.255.1:80", "example.invalid:443", "[2001:db8::1]:80", "8.8.8.8"} {
		_, err := dial(context.Background(), "tcp", addr)
		var e *ErrHostNotAllowed
		if !errors.As(err, &e) {
			t.Errorf("%s: want ErrHostNotAllowed got %v", addr, err)
			continue
		}
		if e.Host == "" || e.Error() == "" {
			t.Errorf("empty error info %+v", e)
		}
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Error("refusal should happen before any network I/O")
	}
}

func TestGuardDialerAllowsLoopbackAndAllowlisted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dial := Policy{}.GuardDialer(&net.Dialer{Timeout: time.Second})
	c, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	c.Close()
	// allowlisted name passes the guard (dial itself fails: .invalid never resolves)
	p := Policy{AllowedHosts: []string{"example.invalid"}}
	_, err = p.GuardDialer(&net.Dialer{Timeout: 300 * time.Millisecond})(context.Background(), "tcp", "example.invalid:80")
	var e *ErrHostNotAllowed
	if errors.As(err, &e) {
		t.Errorf("allowlisted host was refused: %v", err)
	}
}

func TestLimiterRate(t *testing.T) {
	if NewLimiter(0) != nil || NewLimiter(-1) != nil {
		t.Fatal("non-positive rate => nil limiter")
	}
	if err := (*Limiter)(nil).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	l := NewLimiter(50)
	start := time.Now()
	n := 0
	for time.Since(start) < 600*time.Millisecond {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		n++
	}
	// 1 initial token + ~50/s * 0.6s = ~31
	if n < 20 || n > 45 {
		t.Errorf("got %d acquisitions in 600ms at 50 rps, want ~31", n)
	}
}

func TestLimiterContextCancel(t *testing.T) {
	l := NewLimiter(1)
	_ = l.Wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("Wait did not honour ctx")
	}
}

func TestLimiterFractionalRate(t *testing.T) {
	l := NewLimiter(0.5)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first Wait at 0.5 rps should succeed immediately: %v", err)
	}
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("second Wait at 0.5 rps should succeed within ~2s: %v", err)
	}
}

func TestLoadPolicyDefaultWhenMissing(t *testing.T) {
	t.Setenv("PHOENIX_HOME", t.TempDir())
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultPolicy()
	if p.MaxVUs != d.MaxVUs || p.MaxDuration != "4h" || p.RequireConfirmAbove != 50 || len(p.AllowedHosts) != 0 {
		t.Errorf("%+v", p)
	}
	if p.MaxDur() != 4*time.Hour {
		t.Errorf("MaxDur %v", p.MaxDur())
	}
}

func TestPolicySaveLoadAndInvalid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PHOENIX_HOME", home)
	in := Policy{AllowedHosts: []string{"a.example.com"}, MaxVUs: 7, MaxRPS: 12.5, MaxDuration: "90s", RequireConfirmAbove: 3}
	if err := SavePolicy(in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxVUs != 7 || out.MaxRPS != 12.5 || out.MaxDur() != 90*time.Second || len(out.AllowedHosts) != 1 {
		t.Errorf("%+v", out)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(home, "policy.json"))
		if st.Mode().Perm() != 0o600 {
			t.Errorf("policy perms %v", st.Mode().Perm())
		}
	}
	// partial file keeps defaults for missing keys
	_ = os.WriteFile(filepath.Join(home, "policy.json"), []byte(`{"allowedHosts":["x.test"]}`), 0o600)
	p, err := LoadPolicy()
	if err != nil || p.MaxVUs != 1000 || p.AllowedHosts[0] != "x.test" {
		t.Errorf("partial: %+v %v", p, err)
	}
	_ = os.WriteFile(filepath.Join(home, "policy.json"), []byte(`{not json`), 0o600)
	if _, err := LoadPolicy(); err == nil {
		t.Error("corrupt policy must error")
	}
}

func TestMaxDurFallback(t *testing.T) {
	for _, s := range []string{"", "junk", "0s", "-5m"} {
		if got := (Policy{MaxDuration: s}).MaxDur(); got != 4*time.Hour {
			t.Errorf("%q -> %v", s, got)
		}
	}
}

func TestConfigDirEnv(t *testing.T) {
	d := t.TempDir()
	t.Setenv("PHOENIX_HOME", d)
	if ConfigDir() != d {
		t.Errorf("ConfigDir=%q", ConfigDir())
	}
}

func TestAuditWritesJSONLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PHOENIX_HOME", filepath.Join(home, "nested"))
	Audit("run.start", map[string]any{"runId": "r1", "vus": 5})
	Audit("run.stop", nil)
	// concurrent writers must not interleave lines
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 20; j++ {
				Audit("c", map[string]any{"j": j})
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	path := filepath.Join(home, "nested", "audit.log")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("audit perms %v want 0600", st.Mode().Perm())
	}
	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	var recs []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		recs = append(recs, m)
	}
	if len(recs) != 2+160 {
		t.Fatalf("records=%d", len(recs))
	}
	if recs[0]["event"] != "run.start" || recs[0]["runId"] != "r1" || recs[0]["vus"] != float64(5) {
		t.Errorf("rec0 %v", recs[0])
	}
	if _, err := time.Parse(time.RFC3339, recs[0]["time"].(string)); err != nil {
		t.Errorf("time: %v", err)
	}
	if recs[1]["event"] != "run.stop" {
		t.Errorf("rec1 %v", recs[1])
	}
	if st, _ := os.Stat(filepath.Join(home, "nested")); runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Errorf("dir perms %v", st.Mode().Perm())
	}
}
