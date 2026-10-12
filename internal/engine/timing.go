package engine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/loadmeter/internal/safety"
)

// Causes attached to the request context when one of Load Meter's own
// per-phase timers fires (the overall timeout is a plain deadline).
var (
	errTTFBTimeout  = errors.New("response header timeout: no response byte within the limit after the request was sent")
	errTLSTimeout   = errors.New("TLS handshake timeout")
	errHiddenRetry  = errors.New("connection closed before a response; Go would resend this request transparently (defaults.hiddenRetries=fail)")
	errDecompressed = errors.New("response body decompression failed")
)

type reqStateKey struct{}

// reqState is the per-sample state shared by trace hooks, the dialler, TLS
// client-certificate selection and redirect handling. Hooks run on transport
// goroutines, so every field is guarded by mu.
type reqState struct {
	mu sync.Mutex

	start        time.Time
	host         string // current hop's host (client-certificate selection)
	connectLimit time.Duration
	tlsLimit     time.Duration
	ttfbLimit    time.Duration
	failRetries  bool
	cancel       context.CancelCauseFunc

	dnsStart, dnsDone   time.Time
	connStart, connDone time.Time
	tlsStart, tlsDone   time.Time
	gotConn             time.Time
	firstByte           time.Time
	reused, haveConn    bool
	conn                *lifeConn // underlying connection of the last GotConn

	getConns, hops int // GetConn calls and redirect hops in the current leg
	retries        int
	curReq         *http.Request // the request the transport is sending (see vuRoundTripper)
	maxLife        time.Duration

	ttfbTimer, tlsTimer *time.Timer
}

func stateFrom(ctx context.Context) *reqState {
	st, _ := ctx.Value(reqStateKey{}).(*reqState)
	return st
}

func (st *reqState) setHost(h string) {
	st.mu.Lock()
	st.host = h
	st.mu.Unlock()
}

func (st *reqState) currentHost() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.host
}

// beginLeg resets the per-leg retry accounting (a digest-auth resend is a new leg).
func (st *reqState) beginLeg() {
	st.mu.Lock()
	st.getConns, st.hops = 0, 0
	st.mu.Unlock()
}

// endLeg folds the leg's extra GetConn calls into the retry count.
func (st *reqState) endLeg() {
	st.mu.Lock()
	if extra := st.getConns - st.hops - 1; extra > 0 {
		st.retries += extra
	}
	st.mu.Unlock()
}

func (st *reqState) redirected(host string) {
	st.mu.Lock()
	st.hops++
	st.host = host
	st.mu.Unlock()
}

func (st *reqState) stopTimers() {
	st.mu.Lock()
	if st.ttfbTimer != nil {
		st.ttfbTimer.Stop()
	}
	if st.tlsTimer != nil {
		st.tlsTimer.Stop()
	}
	st.mu.Unlock()
}

func (st *reqState) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			st.mu.Lock()
			st.getConns++
			retry := st.getConns > st.hops+1
			fail := retry && st.failRetries
			st.mu.Unlock()
			if fail {
				st.cancel(errHiddenRetry)
			}
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			st.mu.Lock()
			if st.dnsStart.IsZero() {
				st.dnsStart = time.Now()
			}
			st.mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			st.mu.Lock()
			if st.dnsDone.IsZero() {
				st.dnsDone = time.Now()
			}
			st.mu.Unlock()
		},
		ConnectStart: func(string, string) {
			st.mu.Lock()
			if st.connStart.IsZero() {
				st.connStart = time.Now()
			}
			st.mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			st.mu.Lock()
			if err == nil && st.connDone.IsZero() {
				st.connDone = time.Now()
			}
			st.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			st.mu.Lock()
			if st.tlsStart.IsZero() {
				st.tlsStart = time.Now()
			}
			if st.tlsLimit > 0 && st.tlsTimer == nil {
				st.tlsTimer = time.AfterFunc(st.tlsLimit, func() { st.cancel(errTLSTimeout) })
			}
			st.mu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			st.mu.Lock()
			if st.tlsDone.IsZero() {
				st.tlsDone = time.Now()
			}
			if st.tlsTimer != nil {
				st.tlsTimer.Stop()
			}
			st.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			now := time.Now()
			lc := unwrapConn(info.Conn)
			st.mu.Lock()
			if !st.haveConn {
				st.haveConn, st.gotConn, st.reused = true, now, info.Reused
			}
			st.conn = lc
			// Max connection lifetime: mark this (last) exchange on an expired
			// connection as closing. HTTP/1.x sends Connection: close and drops
			// the connection after the response; HTTP/2 stops opening new
			// streams on it and closes it once idle. Nothing in flight breaks.
			if lc != nil && info.Reused && st.maxLife > 0 && st.curReq != nil && now.Sub(lc.born) > st.maxLife {
				st.curReq.Close = true
			}
			st.mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			st.mu.Lock()
			if st.ttfbLimit > 0 {
				if st.ttfbTimer != nil {
					st.ttfbTimer.Stop()
				}
				st.ttfbTimer = time.AfterFunc(st.ttfbLimit, func() { st.cancel(errTTFBTimeout) })
			}
			st.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			st.mu.Lock()
			st.firstByte = time.Now()
			if st.ttfbTimer != nil {
				st.ttfbTimer.Stop()
			}
			st.mu.Unlock()
		},
	}
}

// timings is a consistent copy of the phase durations for one sample.
type timings struct {
	DNS, Connect, TLS, TTFB, Total time.Duration
	Reused                         bool
	NewConn                        bool
}

func (st *reqState) timings(total time.Duration) timings {
	st.mu.Lock()
	defer st.mu.Unlock()
	t := timings{Total: total, Reused: st.reused}
	if !st.dnsStart.IsZero() && !st.dnsDone.IsZero() {
		t.DNS = st.dnsDone.Sub(st.dnsStart)
	}
	if !st.tlsStart.IsZero() && !st.tlsDone.IsZero() {
		t.TLS = st.tlsDone.Sub(st.tlsStart)
	}
	if st.haveConn && !st.reused {
		t.NewConn = true
		t.Connect = max(st.gotConn.Sub(st.start), time.Microsecond) // JMeter: start -> connection ready
	}
	if !st.firstByte.IsZero() {
		t.TTFB = st.firstByte.Sub(st.start)
	}
	return t
}

// get returns the named timing for extractors and max_latency assertions.
func (t timings) get(name string) time.Duration {
	switch name {
	case "ttfb":
		return t.TTFB
	case "connect":
		return t.Connect
	case "dns":
		return t.DNS
	case "tls":
		return t.TLS
	default:
		return t.Total
	}
}

func fmtMs(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 3, 64)
}

// phase reports where a failed request was when it failed.
func (st *reqState) phase() (connecting, handshaking, waitingHeaders bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.haveConn {
		return false, false, st.firstByte.IsZero()
	}
	if !st.tlsStart.IsZero() && st.tlsDone.IsZero() {
		return false, true, false
	}
	return true, false, false
}

// classifyExchange maps an error to an error kind, using the cause set by
// Load Meter's timers and the phase the request had reached.
// afterHeaders marks errors raised while reading the response body.
func classifyExchange(err error, cause error, st *reqState, afterHeaders bool) string {
	var hna *safety.ErrHostNotAllowed
	var dno *safety.ErrDNSOverrideInvalid
	switch {
	case errors.As(err, &hna), errors.As(err, &dno):
		return "blocked_by_policy"
	case errors.Is(cause, errTTFBTimeout):
		return "ttfb_timeout"
	case errors.Is(cause, errTLSTimeout):
		return "tls"
	case errors.Is(cause, errHiddenRetry):
		return "connection_closed"
	case errors.Is(err, errDecompressed):
		return "decompression"
	case isTLSError(err):
		return "tls"
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" && oe.Timeout() {
		return "connect_timeout"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		if afterHeaders {
			return "read_timeout"
		}
		if st != nil {
			switch connecting, handshaking, _ := st.phase(); {
			case handshaking:
				return "tls"
			case connecting:
				return "connect_timeout"
			}
		}
		return "timeout"
	}
	return classify(err)
}

func isTLSError(err error) bool {
	var rh tls.RecordHeaderError
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	var ae tls.AlertError
	var cv *tls.CertificateVerificationError
	if errors.As(err, &rh) || errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &he) || errors.As(err, &ae) || errors.As(err, &cv) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "TLS handshake timeout") || strings.Contains(msg, "tls: ")
}
