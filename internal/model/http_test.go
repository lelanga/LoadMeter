package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func httpSc() *Scenario {
	sc := NewScenario("t")
	sc.Defaults.BaseURL = "http://127.0.0.1"
	sc.Steps = []Step{{Type: StepRequest, Request: &Request{Method: "GET", URL: "/"}}}
	return sc
}

func TestHTTPOptionsJSONFlatAndBackwardCompatible(t *testing.T) {
	in := `{"version":"loadmeter/v1","name":"x","defaults":{"baseUrl":"http://127.0.0.1","timeout":"5s",
	 "connectTimeout":"2s","responseHeaderTimeout":"1s","compression":"off","connections":"shared","keepAlive":false,
	 "httpVersion":"1.1","maxConnLifetime":"30s","hiddenRetries":"fail",
	 "auth":[{"urlPrefix":"/api","type":"bearer","token":"${secret_tok}"}],
	 "dns":{"hosts":{"api.test":"127.0.0.1"}},"proxy":{"url":"http://127.0.0.1:3128"}},
	 "steps":[{"type":"request","request":{"method":"GET","url":"/","tlsTimeout":"3s",
	   "extract":[{"name":"t","type":"timing","expr":"ttfb"}],"assert":[{"type":"max_latency","expr":"ttfb","value":"1s"}]}}],
	 "workload":{"executor":"constant-vus","vus":1,"duration":"1s"}}`
	var sc Scenario
	if err := json.Unmarshal([]byte(in), &sc); err != nil {
		t.Fatal(err)
	}
	d := sc.Defaults
	if d.ConnectTimeout.D() != 2*time.Second || d.Compression != "off" || *d.KeepAlive || d.HTTPVersion != "1.1" || d.Auth[0].Token != "${secret_tok}" ||
		d.DNS.Hosts["api.test"] != "127.0.0.1" || sc.Steps[0].Request.TLSTimeout.D() != 3*time.Second {
		t.Fatalf("%+v", d)
	}
	if p := sc.Validate(); len(p) != 0 {
		t.Fatalf("valid scenario: %v", p)
	}
	// Round trip keeps the fields flat under defaults (no "HTTPOptions" key).
	b, _ := json.Marshal(sc.Clone())
	if strings.Contains(string(b), "HTTPOptions") || !strings.Contains(string(b), `"connectTimeout":"2s"`) {
		t.Errorf("%s", b)
	}
	// An old scenario without any of the new fields marshals as before.
	old := httpSc()
	b, _ = json.Marshal(old.Defaults)
	if string(b) != `{"baseUrl":"http://127.0.0.1"}` {
		t.Errorf("zero options must be omitted: %s", b)
	}
}

func TestHTTPOptionsValidate(t *testing.T) {
	sc := httpSc()
	sc.Defaults.Compression = "br"
	sc.Defaults.Connections = "pooled"
	sc.Defaults.HTTPVersion = "3"
	sc.Defaults.HiddenRetries = "retry"
	sc.Defaults.ConnectTimeout = -1
	sc.Defaults.TLS = &TLSOptions{ClientCert: "c.pem"}
	sc.Defaults.DNS = &DNSOptions{Hosts: map[string]string{"api.test": "api2.test"}}
	sc.Defaults.Proxy = &ProxyOptions{URL: "socks5://p:1", Password: "pw"}
	sc.Steps[0].Request.ResponseHeaderTimeout = -1
	sc.Steps[0].Request.Extract = []Extractor{{Name: "x", Type: ExTiming, Expr: "latency"}}
	sc.Steps[0].Request.Assert = []Assertion{{Type: AsMaxLatency, Expr: "body", Value: "1s"}}
	p := strings.Join(sc.Validate(), "\n")
	for _, want := range []string{"compression", "connections", "httpVersion", "hiddenRetries", "timeouts must not be negative",
		"clientCert and clientKey", "not an IP address", "proxy.url", "proxy.password", "timing extractor", "max_latency expr"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
}

func TestSecretOnly(t *testing.T) {
	for v, want := range map[string]bool{"": true, "${secret_pw}": true, "${__env(LOADMETER_PW)}": true, "${secret_a}${secret_b}": true,
		"pw": false, "${password}": false, "x${secret_pw}": false, "${__env(LOADMETER_PW)}!": false} {
		if SecretOnly(v) != want {
			t.Errorf("SecretOnly(%q) != %v", v, want)
		}
	}
}
