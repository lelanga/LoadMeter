package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

// connCounter is an httptest server that counts new TCP connections and
// echoes the protocol it was spoken to with.
type connCounter struct {
	srv   *httptest.Server
	conns atomic.Int64
}

func newConnCounter(t *testing.T, tlsOn bool, configure func(*http.Server)) *connCounter {
	t.Helper()
	cc := &connCounter{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ms, _ := strconv.Atoi(r.URL.Query().Get("ms")); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		fmt.Fprintf(w, `{"proto":"%s"}`, r.Proto)
	})
	cc.srv = httptest.NewUnstartedServer(h)
	cc.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			cc.conns.Add(1)
		}
	}
	if configure != nil {
		configure(cc.srv.Config)
	}
	if tlsOn {
		cc.srv.EnableHTTP2 = true
		cc.srv.StartTLS()
	} else {
		cc.srv.Start()
	}
	t.Cleanup(cc.srv.Close)
	return cc
}

func protoStep(name string) model.Step {
	return withExtract(get(name, "/"), model.Extractor{Name: "proto", Type: model.ExJSONPath, Expr: "$.proto"})
}

func TestConnectionsPerVUAndShared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode      string
		keepAlive *bool
		check     func(conns, vus, reqs int64) bool
	}{
		{model.ConnPerVU, nil, func(c, v, _ int64) bool { return c == v }},
		{model.ConnShared, nil, func(c, v, _ int64) bool { return c >= 1 && c <= v }},
		{model.ConnPerVU, ptr(false), func(c, _, r int64) bool { return c == r }},
	} {
		cc := newConnCounter(t, false, nil)
		const vus, iters = 5, 6
		st := get("x", "/?ms=5")
		sc := newSc(cc.srv.URL, perVU(vus, iters), st)
		sc.Defaults.Connections = tc.mode
		sc.Defaults.KeepAlive = tc.keepAlive
		_, snap := runSc(t, sc, opts(nil))
		if snap.Requests.Errors != 0 {
			t.Fatalf("%s: %v", tc.mode, snap.Requests.ErrorKind)
		}
		if got := cc.conns.Load(); !tc.check(got, vus, vus*iters) {
			t.Errorf("mode=%s keepAlive=%v: %d connections for %d VUs / %d requests", tc.mode, tc.keepAlive, got, vus, vus*iters)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestConnectionModeDefaults(t *testing.T) {
	sc := newSc("http://127.0.0.1", constVUs(1, time.Second), get("x", "/"))
	if connectionMode(sc) != model.ConnPerVU {
		t.Error("closed model must default to per-vu")
	}
	sc.Workload = model.Workload{Executor: model.ExecArrivalRate, Rate: 1, Duration: dur(time.Second), MaxVUs: 1}
	if connectionMode(sc) != model.ConnShared {
		t.Error("open model must default to shared")
	}
	sc.Defaults.Connections = model.ConnPerVU
	if connectionMode(sc) != model.ConnPerVU {
		t.Error("explicit mode wins")
	}
}

func TestHTTPVersion(t *testing.T) {
	t.Parallel()
	tlsSrv := newConnCounter(t, true, nil)
	h2c := newConnCounter(t, false, func(s *http.Server) {
		s.Protocols = new(http.Protocols)
		s.Protocols.SetHTTP1(true)
		s.Protocols.SetUnencryptedHTTP2(true)
	})
	cases := []struct {
		base, version, want string
	}{
		{tlsSrv.srv.URL, "", "HTTP/2.0"},
		{tlsSrv.srv.URL, "auto", "HTTP/2.0"},
		{tlsSrv.srv.URL, "1.1", "HTTP/1.1"},
		{tlsSrv.srv.URL, "2", "HTTP/2.0"},
		{h2c.srv.URL, "", "HTTP/1.1"},
		{h2c.srv.URL, "2", "HTTP/2.0"},
	}
	for _, c := range cases {
		rec := &recorder{}
		sc := newSc(c.base, perVU(1, 1), protoStep("p"))
		sc.Defaults.InsecureSkipVerify = true
		sc.Defaults.HTTPVersion = c.version
		runSc(t, sc, opts(rec))
		ex := rec.all()[0]
		if ex.Extracted["proto"] != c.want {
			t.Errorf("%s version=%q: got %q (%s) want %s", c.base, c.version, ex.Extracted["proto"], ex.Error, c.want)
		}
	}
}

func TestMaxConnLifetime(t *testing.T) {
	t.Parallel()
	for _, tlsOn := range []bool{false, true} {
		cc := newConnCounter(t, tlsOn, nil)
		th := model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(60 * time.Millisecond)}}
		sc := newSc(cc.srv.URL, perVU(1, 12), get("x", "/"), th)
		sc.Defaults.InsecureSkipVerify = true
		sc.Defaults.MaxConnLifetime = dur(150 * time.Millisecond)
		_, snap := runSc(t, sc, opts(nil))
		if snap.Requests.Errors != 0 || snap.Requests.Count != 12 {
			t.Fatalf("tls=%v: lifetime recycling must not fail requests: %v", tlsOn, snap.Requests.ErrorKind)
		}
		// ~12 x 60ms = 720ms of activity with a 150ms lifetime: several connections, not 1 or 12.
		if got := cc.conns.Load(); got < 3 || got > 8 {
			t.Errorf("tls=%v: %d connections with a 150ms lifetime", tlsOn, got)
		}
	}
	// Disabled with a negative value: one connection.
	cc := newConnCounter(t, false, nil)
	th := model.Step{Type: model.StepThink, Think: &model.Think{Min: dur(30 * time.Millisecond)}}
	sc := newSc(cc.srv.URL, perVU(1, 8), get("x", "/"), th)
	sc.Defaults.MaxConnLifetime = dur(-time.Second)
	runSc(t, sc, opts(nil))
	if cc.conns.Load() != 1 {
		t.Errorf("lifetime disabled: %d connections", cc.conns.Load())
	}
}

// closingServer answers the first request on each connection with keep-alive
// and closes the connection when a second request arrives on it, which makes
// Go's transport resend idempotent requests on a fresh connection.
func closingServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				req.Body.Close()
				fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nok")
				_, _ = http.ReadRequest(br) // second request on this connection: drop it
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestHiddenRetriesReportedAndFailed(t *testing.T) {
	t.Parallel()
	base := closingServer(t)
	var raw bytes.Buffer
	o := opts(nil)
	o.Raw = &raw
	sc := newSc(base, perVU(1, 3), get("x", "/"))
	r, snap := runSc(t, sc, o)
	if snap.Requests.Errors != 0 {
		t.Fatalf("report mode keeps Go's resend: %v", snap.Requests.ErrorKind)
	}
	if r.Col.Retries < 1 {
		t.Errorf("hidden retries not counted: %d", r.Col.Retries)
	}
	rows, _ := csv.NewReader(&raw).ReadAll()
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	retried := 0
	for _, row := range rows[1:] {
		if row[col["retries"]] != "0" {
			retried++
		}
	}
	if retried == 0 {
		t.Error("raw CSV must show the retried samples")
	}

	sc = newSc(base, perVU(1, 3), get("x", "/"))
	sc.Defaults.HiddenRetries = "fail"
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["connection_closed"] < 1 {
		t.Errorf("fail mode must surface the broken connection: %v", snap.Requests.ErrorKind)
	}
}

func TestTimingBreakdownAndRawColumns(t *testing.T) {
	t.Parallel()
	cc := newConnCounter(t, true, nil)
	var raw bytes.Buffer
	o := opts(nil)
	rec := &recorder{}
	o.Inspect, o.Raw = rec.fn, &raw
	st := withExtract(get("x", "/?ms=30"), model.Extractor{Name: "ttfb", Type: model.ExTiming, Expr: "ttfb"},
		model.Extractor{Name: "conn", Type: model.ExTiming, Expr: "connect"})
	st = withAssert(st, model.Assertion{Type: model.AsMaxLatency, Expr: "ttfb", Value: "5s"})
	sc := newSc(cc.srv.URL, perVU(1, 2), st)
	sc.Defaults.InsecureSkipVerify = true
	sc.Defaults.HTTPVersion = "1.1"
	_, snap := runSc(t, sc, o)
	if snap.Requests.Errors != 0 {
		t.Fatalf("%v", snap.Requests.ErrorKind)
	}
	exs := rec.all()
	first, second := exs[0], exs[1]
	if first.Timing == nil || first.Timing.ConnectMs <= 0 || first.Timing.TLSMs <= 0 || first.Timing.Reused {
		t.Errorf("first request opens a TLS connection: %+v", first.Timing)
	}
	if second.Timing.ConnectMs != 0 || !second.Timing.Reused || second.Timing.TLSMs != 0 {
		t.Errorf("second request reuses it: %+v", second.Timing)
	}
	if first.Timing.TTFBMs < 30 || first.Timing.TTFBMs < first.Timing.ConnectMs {
		t.Errorf("ttfb %v must include the server delay and the connect time", first.Timing)
	}
	if v, _ := strconv.ParseFloat(first.Extracted["ttfb"], 64); v < 30 {
		t.Errorf("timing extractor ttfb=%q", first.Extracted["ttfb"])
	}
	if v, _ := strconv.ParseFloat(first.Extracted["conn"], 64); v <= 0 {
		t.Errorf("timing extractor connect=%q", first.Extracted["conn"])
	}
	rows, err := csv.NewReader(&raw).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("raw rows %d %v", len(rows), err)
	}
	hdr := strings.Join(rows[0], ",")
	if !strings.HasPrefix(hdr, "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind,Connect,") {
		t.Errorf("raw header %s", hdr)
	}
	if rows[2][16] != "true" || rows[1][16] != "false" {
		t.Errorf("connReused column: %v / %v", rows[1], rows[2])
	}

	// max_latency on ttfb fails when the server is slower than the limit.
	rec2 := &recorder{}
	st = withAssert(get("slow", "/?ms=80"), model.Assertion{Type: model.AsMaxLatency, Expr: "ttfb", Value: "20ms"})
	sc = newSc(cc.srv.URL, perVU(1, 1), st)
	sc.Defaults.InsecureSkipVerify = true
	runSc(t, sc, opts(rec2))
	if ex := rec2.all()[0]; ex.OK || !strings.Contains(ex.Error, "ttfb time") {
		t.Errorf("ttfb assertion: ok=%v %s", ex.OK, ex.Error)
	}
}

func TestSplitTimeouts(t *testing.T) {
	t.Parallel()
	srv, _ := newMock(t)

	// Response-header (TTFB) timeout from defaults and per-request override.
	sc := newSc(srv.URL, perVU(1, 1), get("slow", "/delay?ms=2000"), get("fast", "/health"))
	sc.Defaults.ResponseHeaderTimeout = dur(150 * time.Millisecond)
	start := time.Now()
	_, snap := runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["ttfb_timeout"] != 1 || snap.Requests.Errors != 1 {
		t.Errorf("ttfb timeout: %v", snap.Requests.ErrorKind)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("ttfb timeout not enforced: %v", time.Since(start))
	}
	st := get("slow", "/delay?ms=300")
	st.Request.ResponseHeaderTimeout = dur(2 * time.Second)
	sc = newSc(srv.URL, perVU(1, 1), st)
	sc.Defaults.ResponseHeaderTimeout = dur(50 * time.Millisecond)
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.Errors != 0 {
		t.Errorf("per-request override ignored: %v", snap.Requests.ErrorKind)
	}

	// Connect timeout (a 1ns budget cannot complete any dial).
	st = get("c", "/health")
	st.Request.ConnectTimeout = dur(time.Nanosecond)
	sc = newSc(srv.URL, perVU(1, 1), st)
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["connect_timeout"] != 1 {
		t.Errorf("connect timeout: %v", snap.Requests.ErrorKind)
	}

	// TLS handshake timeout: the peer accepts TCP but never answers TLS.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held.Add(1)
			go func() { defer held.Done(); time.Sleep(2 * time.Second); c.Close() }()
		}
	}()
	sc = newSc("https://"+ln.Addr().String(), perVU(1, 1), get("tls", "/"))
	sc.Defaults.TLSTimeout = dur(200 * time.Millisecond)
	start = time.Now()
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["tls"] != 1 {
		t.Errorf("tls timeout: %v", snap.Requests.ErrorKind)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("tls timeout not enforced: %v", time.Since(start))
	}
	// Per-request TLS timeout shorter than the default.
	st = get("tls", "/")
	st.Request.TLSTimeout = dur(100 * time.Millisecond)
	sc = newSc("https://"+ln.Addr().String(), perVU(1, 1), st)
	start = time.Now()
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["tls"] != 1 || time.Since(start) > 1500*time.Millisecond {
		t.Errorf("per-request tls timeout: %v in %v", snap.Requests.ErrorKind, time.Since(start))
	}

	// Read timeout: headers arrive, the body stalls past the overall timeout.
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer stall.Close()
	sc = newSc(stall.URL, perVU(1, 1), withTimeout(get("body", "/"), 200*time.Millisecond))
	_, snap = runSc(t, sc, opts(nil))
	if snap.Requests.ErrorKind["read_timeout"] != 1 {
		t.Errorf("read timeout: %v", snap.Requests.ErrorKind)
	}
}

func TestClassifyExchange(t *testing.T) {
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	st := &reqState{}
	cases := []struct {
		err, cause error
		after      bool
		want       string
	}{
		{dialTimeout, nil, false, "connect_timeout"},
		{context.Canceled, errTTFBTimeout, false, "ttfb_timeout"},
		{context.Canceled, errTLSTimeout, false, "tls"},
		{context.Canceled, errHiddenRetry, false, "connection_closed"},
		{context.DeadlineExceeded, nil, true, "read_timeout"},
		{fmt.Errorf("x: %w", errDecompressed), nil, true, "decompression"},
		{errors.New("net/http: TLS handshake timeout"), nil, false, "tls"},
		{errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), nil, false, "tls"},
		{errors.New("dial tcp: connection refused"), nil, false, "connect"},
	}
	for _, c := range cases {
		if got := classifyExchange(c.err, c.cause, st, c.after); got != c.want {
			t.Errorf("%v / %v -> %s want %s", c.err, c.cause, got, c.want)
		}
	}
	// Overall deadline while connected and waiting for headers stays "timeout".
	st = &reqState{haveConn: true}
	if got := classifyExchange(context.DeadlineExceeded, nil, st, false); got != "timeout" {
		t.Errorf("waiting headers: %s", got)
	}
}

// 10k VUs in per-vu mode must stay bounded: a transport per VU allocates
// nothing until it dials, and idle pools are capped per VU.
func TestManyPerVUTransportsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 10k VUs")
	}
	sc := newSc("http://127.0.0.1:1", constVUs(10000, time.Second), get("x", "/"))
	o := opts(nil)
	o.Policy.MaxVUs = 10000
	r, err := Prepare(sc, o)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	vus := make([]*vuState, 10000)
	for i := range vus {
		vus[i] = r.newVU(i + 1)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	perVU := float64(after.HeapAlloc-before.HeapAlloc) / float64(len(vus))
	if perVU > 16<<10 {
		t.Errorf("%.0f bytes per VU", perVU)
	}
	t.Logf("heap per idle VU (client, cookie jar, own transport): %.0f bytes", perVU)
	if tr := vus[0].ownTransport; tr == nil || tr.MaxIdleConnsPerHost != perVUIdlePerHost || tr.MaxIdleConns != perVUIdleTotal {
		t.Errorf("per-VU pool limits: %+v", tr)
	}
	for _, vu := range vus {
		vu.closeIdle()
	}
}
