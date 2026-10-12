// Package safety enforces the target allowlist, load limits and audit logging.
//
// Loopback targets are always allowed. Any other host must be allowlisted by the
// operator (CLI flag, policy file or UI) — never by the scenario itself, because
// imported scenarios are untrusted input.
package safety

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Policy struct {
	AllowedHosts []string `json:"allowedHosts"` // exact host or *.suffix
	MaxVUs       int      `json:"maxVUs"`
	MaxRPS       float64  `json:"maxRPS"` // 0 = unlimited
	MaxDuration  string   `json:"maxDuration"`
	// RequireConfirmAbove: VU count above which a non-loopback test needs explicit confirmation.
	RequireConfirmAbove int `json:"requireConfirmAbove"`
}

func DefaultPolicy() Policy {
	return Policy{MaxVUs: 1000, MaxDuration: "4h", RequireConfirmAbove: 50}
}

// ConfigDir is ~/.phoenix (override with PHOENIX_HOME).
func ConfigDir() string {
	if d := os.Getenv("PHOENIX_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".phoenix"
	}
	return filepath.Join(h, ".phoenix")
}

// LoadPolicy reads ConfigDir()/policy.json; a missing file yields the default policy.
func LoadPolicy() (Policy, error) {
	p := DefaultPolicy()
	b, err := os.ReadFile(filepath.Join(ConfigDir(), "policy.json"))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("policy.json: %w", err)
	}
	return p, nil
}

func SavePolicy(p Policy) error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(filepath.Join(ConfigDir(), "policy.json"), b, 0o600)
}

func IsLoopback(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// HostAllowed reports whether host (no port) may be targeted.
func (p Policy) HostAllowed(host string) bool {
	if IsLoopback(host) {
		return true
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	for _, a := range p.AllowedHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == h {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(h, a[1:]) {
			return true
		}
	}
	return false
}

func (p Policy) MaxDur() time.Duration {
	d, err := time.ParseDuration(p.MaxDuration)
	if err != nil || d <= 0 {
		return 4 * time.Hour
	}
	return d
}

// ErrHostNotAllowed is returned by the guarded dialer.
type ErrHostNotAllowed struct{ Host string }

func (e *ErrHostNotAllowed) Error() string {
	return fmt.Sprintf("target host %q is not in the allowlist; add it with --allow-host %s (only for systems you are authorized to test)", e.Host, e.Host)
}

// GuardDialer wraps a dialer so every connection (including redirects) is
// checked against the policy before DNS resolution.
func (p Policy) GuardDialer(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if !p.HostAllowed(host) {
			return nil, &ErrHostNotAllowed{Host: host}
		}
		return d.DialContext(ctx, network, addr)
	}
}

// ---------- Audit log ----------

var auditMu sync.Mutex

// Audit appends a JSON line to ConfigDir()/audit.log. Never pass secrets in fields.
func Audit(event string, fields map[string]any) {
	auditMu.Lock()
	defer auditMu.Unlock()
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(ConfigDir(), "audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "event": event}
	for k, v := range fields {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	_, _ = f.Write(append(b, '\n'))
}

// ---------- Rate limiter ----------

// Limiter is a token bucket shared by all VUs (global request-rate cap).
type Limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func NewLimiter(rps float64) *Limiter {
	if rps <= 0 {
		return nil
	}
	return &Limiter{rate: rps, tokens: 1, last: time.Now()}
}

// Wait blocks until a token is available or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = min(max(l.rate, 1), l.tokens+now.Sub(l.last).Seconds()*l.rate) // burst ≥ 1 so rates < 1/s still progress
		if l.tokens < 1 {
			l.tokens = max(l.tokens, 0)
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
