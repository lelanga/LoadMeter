package engine

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
)

// exchangeResult is one sample's HTTP exchange: possibly two legs (a digest
// challenge answered once), recorded as a single sample like JMeter/HC4.
type exchangeResult struct {
	resp      *http.Response
	body      []byte
	err       error
	errKind   string
	start     time.Time
	latency   time.Duration
	timing    timings
	wireIn    int64 // response header + body bytes on the wire (all legs)
	wireBody  int64 // final response body bytes on the wire
	bodyBytes int64 // decoded body bytes of the final response
	extraOut  int64 // request bytes added here (auth headers, second leg)
	retries   int
	authType  string
	notes     []string
	encoding  string
}

// exchange sends req (built by buildRequest) and reads the response body.
func (vu *vuState) exchange(ctx context.Context, req *http.Request, rq *model.Request) *exchangeResult {
	h := vu.r.http
	xr := &exchangeResult{}
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	st := &reqState{host: req.URL.Hostname(), connectLimit: rq.ConnectTimeout.D(), tlsLimit: rq.TLSTimeout.D(),
		ttfbLimit: rq.ResponseHeaderTimeout.D(), failRetries: h.failRetries, cancel: cancel, maxLife: h.maxLife}
	if st.ttfbLimit <= 0 {
		st.ttfbLimit = h.ttfbTimeout
	}
	if st.tlsLimit <= 0 {
		st.tlsLimit = h.tlsTimeout
	}
	defer st.stopTimers()
	cctx = context.WithValue(cctx, reqStateKey{}, st)
	cctx = httptrace.WithClientTrace(cctx, st.trace())
	req = req.WithContext(cctx)

	explicitAuth := req.Header.Get("Authorization") != ""
	if h.compression {
		if note := negotiateEncoding(req); note != "" {
			xr.notes = append(xr.notes, note)
		}
	}
	xr.authType = vu.applyAuth(req, explicitAuth)
	if xr.authType != "" {
		xr.extraOut += int64(len("Authorization") + len(req.Header.Get("Authorization")) + 4)
	}
	vu.curExplicitAuth = explicitAuth

	maxBody := vu.r.sc.Defaults.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	xr.start = time.Now()
	st.start = xr.start
	afterHeaders := false
	for leg := 0; leg < 2; leg++ {
		st.beginLeg()
		resp, err := vu.client.Do(req)
		st.endLeg()
		if err != nil {
			xr.err = err
			break
		}
		xr.resp = resp
		xr.wireIn += headerBytes(resp)
		// A digest challenge is answered once, on the same sample.
		if leg == 0 && !explicitAuth {
			if rule, ch := vu.digestChallengeFor(resp); ch != nil {
				if br, err := readBody(resp, maxBody, false); err == nil {
					xr.wireIn += br.wire
					if next := retryRequest(req, resp); next != nil {
						if hv, err := vu.digestHeader(rule, ch, next); err == nil {
							next.Header.Set("Authorization", hv)
							xr.authType = model.AuthDigest
							xr.extraOut += requestSize(next)
							req = next
							continue
						}
					}
				}
				// fall through: report the 401 as received
				xr.body = nil
				break
			}
		}
		afterHeaders = true
		br, err := readBody(resp, maxBody, h.compression)
		xr.wireIn += br.wire
		xr.wireBody = br.wire
		xr.body, xr.bodyBytes, xr.encoding = br.body, br.decoded, br.encoding
		if br.note != "" {
			xr.notes = append(xr.notes, br.note)
		}
		if err != nil {
			xr.err = err
		}
		break
	}
	xr.latency = time.Since(xr.start)
	st.stopTimers()
	xr.timing = st.timings(xr.latency)
	st.mu.Lock()
	xr.retries = st.retries
	st.mu.Unlock()
	if xr.err != nil {
		xr.errKind = classifyExchange(xr.err, context.Cause(cctx), st, afterHeaders)
	}
	return xr
}

// retryRequest clones req for the authenticated resend, targeting the URL
// that issued the challenge (after any redirects).
func retryRequest(req *http.Request, resp *http.Response) *http.Request {
	final := resp.Request
	next := req.Clone(req.Context())
	next.URL, next.Host = final.URL, ""
	if final.URL.Host == req.URL.Host {
		next.Host = req.Host
	}
	if final.Method != req.Method {
		next.Method, next.Body, next.GetBody, next.ContentLength = final.Method, nil, nil, 0
		return next
	}
	if req.GetBody != nil {
		b, err := req.GetBody()
		if err != nil {
			return nil
		}
		next.Body = b
	} else if req.Body != nil && req.Body != http.NoBody {
		return nil // body cannot be replayed
	}
	return next
}

func headerBytes(resp *http.Response) int64 {
	var n int64
	for k, v := range resp.Header {
		n += int64(len(k) + len(strings.Join(v, ",")) + 4)
	}
	return n
}

func requestSize(req *http.Request) int64 {
	var n int64
	for k, v := range req.Header {
		n += int64(len(k) + len(strings.Join(v, ",")) + 4)
	}
	n += int64(len(req.Method)+len(req.URL.RequestURI())+12) + max(req.ContentLength, 0)
	return n
}

// fill copies the exchange's measurements onto the sample and the inspector
// record, redacting credentials Load Meter added.
func (xr *exchangeResult) fill(s *metrics.Sample, ex *Exchange) {
	s.Start, s.Latency, s.TTFB = xr.start, xr.latency, xr.timing.TTFB
	s.Connect, s.DNS, s.TLS, s.ConnReused = xr.timing.Connect, xr.timing.DNS, xr.timing.TLS, xr.timing.Reused
	s.Retries = xr.retries
	s.BytesIn, s.BodyBytes = xr.wireIn, xr.bodyBytes
	s.BytesOut += xr.extraOut
	if xr.authType != "" && ex.ReqHeaders != nil {
		if _, ok := ex.ReqHeaders["Authorization"]; ok {
			ex.ReqHeaders["Authorization"] = "[REDACTED " + xr.authType + "]"
		}
	}
	t := xr.timing
	ex.Timing = &TimingDetail{DNSMs: ms(t.DNS), ConnectMs: ms(t.Connect), TLSMs: ms(t.TLS), TTFBMs: ms(t.TTFB),
		Reused: t.Reused, Retries: xr.retries}
	ex.WireBytes, ex.BodyBytes, ex.ContentEncoding = xr.wireBody, xr.bodyBytes, xr.encoding
	ex.Notes = append(ex.Notes, xr.notes...)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// TimingDetail is the per-request timing breakdown shown by the inspector.
type TimingDetail struct {
	DNSMs     float64 `json:"dnsMs"`
	ConnectMs float64 `json:"connectMs"`
	TLSMs     float64 `json:"tlsMs"`
	TTFBMs    float64 `json:"ttfbMs"`
	Reused    bool    `json:"reused"`
	Retries   int     `json:"retries,omitempty"`
}

// onRedirect runs for every redirect hop: it points client-certificate
// selection at the new host and re-applies auth by URL prefix, removing
// credentials Load Meter added when the new URL matches no rule.
func (vu *vuState) onRedirect(req *http.Request) {
	st := stateFrom(req.Context())
	if st == nil {
		return
	}
	st.redirected(req.URL.Hostname())
	if vu.curExplicitAuth || len(vu.r.sc.Defaults.Auth) == 0 {
		return
	}
	req.Header.Del("Authorization")
	vu.applyAuth(req, false)
}
