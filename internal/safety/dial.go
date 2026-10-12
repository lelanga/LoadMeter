package safety

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// DialFunc is the signature of http.Transport.DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// DialOptions extends GuardDialer with a static host map (DNS override).
type DialOptions struct {
	// Hosts maps a logical host name (case-insensitive) to an IP literal.
	Hosts map[string]string
	// Dial, when set, replaces d.DialContext for the final connect (tests and
	// connection wrappers). It is only called after the policy checks passed.
	Dial DialFunc
}

// ErrDNSOverrideInvalid is returned when an override target is not an IP literal.
type ErrDNSOverrideInvalid struct{ Host, Target string }

func (e *ErrDNSOverrideInvalid) Error() string {
	return fmt.Sprintf("DNS override for %q must be an IP address, got %q", e.Host, e.Target)
}

// LookupOverride returns the IP a host is pinned to, if any.
func (o DialOptions) LookupOverride(host string) (string, bool) {
	if len(o.Hosts) == 0 {
		return "", false
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	for k, v := range o.Hosts {
		if strings.EqualFold(strings.Trim(k, "[]"), h) {
			return strings.Trim(strings.TrimSpace(v), "[]"), true
		}
	}
	return "", false
}

// GuardDialerWith is GuardDialer plus a DNS override map. The allowlist is
// checked twice: for the logical host the request names (as before, before
// any resolution) and for the IP the override sends the connection to. A
// pinned IP must itself be allowlisted (or loopback), so an override cannot
// redirect an allowlisted name to a non-allowlisted machine.
func (p Policy) GuardDialerWith(d *net.Dialer, o DialOptions) DialFunc {
	dial := o.Dial
	if dial == nil {
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			host, port = addr, ""
		}
		if !p.HostAllowed(host) {
			return nil, &ErrHostNotAllowed{Host: host}
		}
		if ip, ok := o.LookupOverride(host); ok {
			if net.ParseIP(ip) == nil {
				return nil, &ErrDNSOverrideInvalid{Host: host, Target: ip}
			}
			if !p.HostAllowed(ip) {
				return nil, &ErrHostNotAllowed{Host: ip}
			}
			if port != "" {
				addr = net.JoinHostPort(ip, port)
			} else {
				addr = ip
			}
		}
		return dial(ctx, network, addr)
	}
}

// ProxyConfig is an explicit forward proxy.
type ProxyConfig struct {
	URL     *url.URL // http:// or https://, may carry user:password
	NoProxy []string // exact host or *.suffix; matched hosts connect directly
}

func matchHostList(list []string, host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == h || (strings.HasPrefix(a, "*.") && len(a) > 2 && strings.HasSuffix(h, a[1:])) {
			return true
		}
	}
	return false
}

// GuardProxy returns an http.Transport.Proxy function. With a proxy the
// dialler only sees the proxy address, so the logical target is checked here
// for every request (redirect hops included), and the proxy host itself must
// be allowlisted too (GuardDialer checks it again when connecting). Loopback
// targets always connect directly.
func (p Policy) GuardProxy(cfg ProxyConfig) (func(*http.Request) (*url.URL, error), error) {
	if cfg.URL == nil || cfg.URL.Host == "" || (cfg.URL.Scheme != "http" && cfg.URL.Scheme != "https") {
		return nil, fmt.Errorf("proxy URL must be http:// or https://")
	}
	if !p.HostAllowed(cfg.URL.Hostname()) {
		return nil, &ErrHostNotAllowed{Host: cfg.URL.Hostname()}
	}
	return func(req *http.Request) (*url.URL, error) {
		target := req.URL.Hostname()
		if !p.HostAllowed(target) {
			return nil, &ErrHostNotAllowed{Host: target}
		}
		// Loopback always means this machine: never send it through a (possibly
		// remote) proxy, where 127.0.0.1 would be the proxy's own services.
		if IsLoopback(target) || matchHostList(cfg.NoProxy, target) {
			return nil, nil
		}
		return cfg.URL, nil
	}, nil
}
