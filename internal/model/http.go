package model

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// HTTPOptions are scenario-wide HTTP-layer settings. They are embedded in
// Defaults, so their JSON fields appear directly under "defaults". Every field
// is optional; the zero value keeps the historical behaviour except where the
// field documentation says otherwise.
type HTTPOptions struct {
	// Compression: "auto" (default) advertises gzip when the request has no
	// Accept-Encoding, removes codings Load Meter cannot decode (br, zstd) from
	// an explicit one, and decodes gzip/deflate bodies; "off" sends headers as
	// written and keeps bodies raw.
	Compression string `json:"compression,omitempty"`
	// ConnectTimeout bounds DNS + TCP connect (default 10s).
	ConnectTimeout Duration `json:"connectTimeout,omitempty"`
	// TLSTimeout bounds the TLS handshake (default 10s).
	TLSTimeout Duration `json:"tlsTimeout,omitempty"`
	// ResponseHeaderTimeout bounds the wait from request written to the first
	// response byte (time to first byte). 0 = only the overall timeout applies.
	ResponseHeaderTimeout Duration `json:"responseHeaderTimeout,omitempty"`

	// Connections: "per-vu" (each VU owns its connections, like JMeter threads
	// and real clients) or "shared" (one pool for all VUs). Default: per-vu for
	// closed-model executors, shared for arrival-rate executors.
	Connections string `json:"connections,omitempty"`
	// KeepAlive reuses connections between requests (default true).
	KeepAlive *bool `json:"keepAlive,omitempty"`
	// HTTPVersion: "auto" (default: HTTP/2 when the server offers it over TLS),
	// "1.1" (never HTTP/2) or "2" (HTTP/2 only; cleartext http:// uses h2c
	// with prior knowledge).
	HTTPVersion string `json:"httpVersion,omitempty"`
	// MaxConnLifetime closes a connection after use once it is older than this
	// (default 60s like JMeter's httpclient4.time_to_live; "-1s" disables).
	MaxConnLifetime Duration `json:"maxConnLifetime,omitempty"`
	// HiddenRetries controls Go's transparent resend of idempotent requests on
	// a broken keep-alive connection: "report" (default) lets the resend happen
	// and counts it on the sample; "fail" fails the request instead, like
	// JMeter (retry count 0).
	HiddenRetries string `json:"hiddenRetries,omitempty"`

	// Auth applies credentials by URL prefix (longest prefix wins).
	Auth []AuthRule `json:"auth,omitempty"`
	// TLS configures client certificates and trusted CAs.
	TLS *TLSOptions `json:"tls,omitempty"`
	// DNS overrides name resolution (static hosts).
	DNS *DNSOptions `json:"dns,omitempty"`
	// Proxy routes requests through an explicit HTTP proxy (never implicit).
	Proxy *ProxyOptions `json:"proxy,omitempty"`
}

// RequestHTTPOptions are per-request overrides, embedded in Request.
type RequestHTTPOptions struct {
	ConnectTimeout        Duration `json:"connectTimeout,omitempty"`
	TLSTimeout            Duration `json:"tlsTimeout,omitempty"`
	ResponseHeaderTimeout Duration `json:"responseHeaderTimeout,omitempty"`
}

// Auth types.
const (
	AuthBasic  = "basic"
	AuthDigest = "digest"
	AuthBearer = "bearer"
)

// AuthRule attaches credentials to requests whose URL starts with URLPrefix.
// A relative prefix ("/api") is resolved against defaults.baseUrl; an empty
// prefix means the whole baseUrl. Password and Token must come from
// ${secret_*} variables or ${__env(LOADMETER_*)}; literal secrets are refused.
type AuthRule struct {
	URLPrefix string `json:"urlPrefix,omitempty"`
	Type      string `json:"type"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	Token     string `json:"token,omitempty"`
	// Realm, when set, restricts a digest rule to challenges for that realm.
	Realm string `json:"realm,omitempty"`
}

// TLSOptions: PEM client certificates (paths relative to the scenario file).
type TLSOptions struct {
	ClientCert string `json:"clientCert,omitempty"`
	ClientKey  string `json:"clientKey,omitempty"`
	// CAFile adds trusted roots (PEM) on top of the system pool.
	CAFile string `json:"caFile,omitempty"`
	// Hosts selects a different client certificate per target host.
	Hosts []TLSHostCert `json:"hosts,omitempty"`
}

type TLSHostCert struct {
	Host       string `json:"host"` // exact host name or *.suffix
	ClientCert string `json:"clientCert"`
	ClientKey  string `json:"clientKey"`
}

// DNSOptions maps host names to fixed IP addresses (like /etc/hosts). The
// safety allowlist checks both the host name and the IP.
type DNSOptions struct {
	Hosts map[string]string `json:"hosts,omitempty"`
}

// ProxyOptions configures an explicit HTTP(S) proxy. Both the proxy host and
// every target host must pass the safety allowlist.
type ProxyOptions struct {
	URL      string   `json:"url"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	NoProxy  []string `json:"noProxy,omitempty"` // exact host or *.suffix
}

// Connection modes.
const (
	ConnPerVU  = "per-vu"
	ConnShared = "shared"
)

// Timing names usable by the "timing" extractor and max_latency's expr.
var TimingNames = []string{"total", "ttfb", "connect", "dns", "tls"}

var secretRef = regexp.MustCompile(`\$\{(secret_[A-Za-z0-9_.\-]*|__env\([^}]*\))\}`)

// SecretOnly reports whether v consists only of ${secret_*} / ${__env(...)}
// references (or is empty).
func SecretOnly(v string) bool {
	return strings.TrimSpace(secretRef.ReplaceAllString(v, "")) == ""
}

func validTiming(name string) bool {
	for _, n := range TimingNames {
		if n == name {
			return true
		}
	}
	return false
}

func (o RequestHTTPOptions) validate(where string) []string {
	var p []string
	if o.ConnectTimeout < 0 || o.TLSTimeout < 0 || o.ResponseHeaderTimeout < 0 {
		p = append(p, where+": timeouts must not be negative")
	}
	return p
}

// validateHTTP checks the HTTP-layer defaults.
func (d Defaults) validateHTTP() []string {
	var p []string
	o := d.HTTPOptions
	if o.ConnectTimeout < 0 || o.TLSTimeout < 0 || o.ResponseHeaderTimeout < 0 {
		p = append(p, "defaults: timeouts must not be negative")
	}
	switch o.Compression {
	case "", "auto", "off":
	default:
		p = append(p, fmt.Sprintf("defaults.compression: unknown value %q (auto|off)", o.Compression))
	}
	switch o.Connections {
	case "", ConnPerVU, ConnShared:
	default:
		p = append(p, fmt.Sprintf("defaults.connections: unknown value %q (per-vu|shared)", o.Connections))
	}
	switch o.HTTPVersion {
	case "", "auto", "1.1", "2":
	default:
		p = append(p, fmt.Sprintf("defaults.httpVersion: unknown value %q (auto|1.1|2)", o.HTTPVersion))
	}
	switch o.HiddenRetries {
	case "", "report", "fail":
	default:
		p = append(p, fmt.Sprintf("defaults.hiddenRetries: unknown value %q (report|fail)", o.HiddenRetries))
	}
	for i, a := range o.Auth {
		where := fmt.Sprintf("defaults.auth[%d]", i)
		switch a.Type {
		case AuthBasic, AuthDigest:
			if a.Username == "" {
				p = append(p, where+": username is required")
			}
			if a.Token != "" {
				p = append(p, where+": token is only valid for bearer")
			}
		case AuthBearer:
			if a.Token == "" {
				p = append(p, where+": token is required")
			}
			if a.Password != "" {
				p = append(p, where+": password is not used by bearer")
			}
		default:
			p = append(p, fmt.Sprintf("%s: unknown type %q (basic|digest|bearer)", where, a.Type))
		}
		if !SecretOnly(a.Password) || !SecretOnly(a.Token) {
			p = append(p, where+": password/token must be given only as ${secret_*} variables or ${__env(LOADMETER_*)}, never as literal text")
		}
		if !strings.Contains(a.URLPrefix, "://") && d.BaseURL == "" {
			p = append(p, where+": a relative urlPrefix needs defaults.baseUrl")
		}
	}
	if t := o.TLS; t != nil {
		if (t.ClientCert == "") != (t.ClientKey == "") {
			p = append(p, "defaults.tls: clientCert and clientKey must be set together")
		}
		for i, h := range t.Hosts {
			if h.Host == "" || h.ClientCert == "" || h.ClientKey == "" {
				p = append(p, fmt.Sprintf("defaults.tls.hosts[%d]: host, clientCert and clientKey are required", i))
			}
		}
	}
	if o.DNS != nil {
		for h, ip := range o.DNS.Hosts {
			if h == "" || strings.ContainsAny(h, ":/ ") {
				p = append(p, fmt.Sprintf("defaults.dns.hosts: invalid host name %q", h))
			}
			if net.ParseIP(strings.Trim(ip, "[]")) == nil {
				p = append(p, fmt.Sprintf("defaults.dns.hosts[%s]: %q is not an IP address", h, ip))
			}
		}
	}
	if px := o.Proxy; px != nil {
		if !strings.Contains(px.URL, "${") {
			u, err := url.Parse(px.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				p = append(p, fmt.Sprintf("defaults.proxy.url: %q must be an http:// or https:// URL", px.URL))
			} else if u.User != nil {
				p = append(p, "defaults.proxy.url: put credentials in proxy.username/password, not in the URL")
			}
		}
		if !SecretOnly(px.Password) {
			p = append(p, "defaults.proxy.password must be given only as ${secret_*} variables or ${__env(LOADMETER_*)}")
		}
	}
	return p
}
