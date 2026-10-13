package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/extract"
	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/vars"
)

type ctxKey int

const noRedirectKey ctxKey = 1

const defaultMaxBody = 1 << 20

type vuState struct {
	r          *Run
	id         int
	client     *http.Client
	vc         *vars.Context
	vuRows     map[string]bool // per-VU-scoped data already assigned
	thinkAccum time.Duration
	setupDone  bool
	iterOK     bool
	// HTTP layer (transport.go, auth.go, exchange.go).
	ownTransport    *http.Transport // per-vu connection mode only
	auth            authCache
	curExplicitAuth bool     // the request in flight set Authorization itself (redirect hook)
	cur             *timings // last request's timings (timing extractor/assertion); not the body
	// Control-flow step state (control.go).
	ctl vuControl
	txc txCount // request samples recorded, for transaction rows (checks.go)
}

func (r *Run) newVU(id int) *vuState {
	vu := &vuState{r: r, id: id, vc: vars.NewContext(id, r.opts.Seed), vuRows: map[string]bool{}, auth: authCache{}}
	vu.vc.Counter = &r.counter // ${__counter()} is per run, not per process
	for k, v := range r.sc.Variables {
		vu.vc.Vars[k] = v
	}
	rt, own := r.clientTransport()
	vu.ownTransport = own
	c := &http.Client{Transport: rt}
	if !r.sc.Defaults.DisableCookies {
		jar, _ := cookiejar.New(nil)
		c.Jar = jar
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if v, _ := req.Context().Value(noRedirectKey).(bool); v {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		vu.onRedirect(req)
		return nil
	}
	vu.client = c
	return vu
}

// runPhase executes setup/teardown steps once; returns false when the VU should stop.
func (vu *vuState) runPhase(ctx context.Context, steps []model.Step, phase string) bool {
	if len(steps) == 0 {
		return true
	}
	ok, abort := vu.execSteps(ctx, steps)
	if !ok && phase == "setup" {
		vu.r.log.Warn("setup failed", "vu", vu.id)
		return vu.r.sc.Workload.OnError != "stop-vu" && !abort
	}
	return true
}

// iteration runs the main steps once; returns false when the VU must stop.
func (vu *vuState) iteration(ctx context.Context) bool {
	vu.vc.Iter++
	vu.freshUser() // workload.newUserEachIteration (control.go)
	for name, f := range vu.r.feeds {
		if f.ds.Scope == "vu" && vu.vuRows[name] {
			continue
		}
		row, err := f.next(vu.id, vu.vc.Rand)
		if err != nil {
			if f.ds.OnExhaust == "error" {
				vu.r.Col.IterationDone(false)
				return true
			}
			vu.r.log.Info("data exhausted; VU stopping", "vu", vu.id, "source", name)
			return false
		}
		f.assign(vu.vc.Vars, row)
		vu.vuRows[name] = true
	}
	vu.iterOK = true
	ok, abort := vu.execSteps(ctx, vu.r.sc.Steps)
	if ctx.Err() != nil {
		return false
	}
	vu.r.Col.IterationDone(ok)
	if !ok && vu.r.sc.Workload.OnError == "stop-vu" {
		return false
	}
	_ = abort
	return true
}

func (vu *vuState) onErrorAbort() bool {
	oe := vu.r.sc.Workload.OnError
	return oe == "abort-iteration" || oe == "stop-vu"
}

// execSteps returns (allOK, aborted).
func (vu *vuState) execSteps(ctx context.Context, steps []model.Step) (bool, bool) {
	ok := true
	for i := range steps {
		st := &steps[i]
		if st.Disabled {
			continue
		}
		if ctx.Err() != nil {
			return false, true
		}
		if sub, abort, handled := vu.execControl(ctx, st); handled { // control.go
			ok = ok && sub
			if abort {
				return false, true
			}
			continue
		}
		switch st.Type {
		case model.StepRequest:
			if !vu.ctl.note(vu.doRequest(ctx, st)) {
				ok = false
				if vu.onErrorAbort() {
					return false, true
				}
			}
		case model.StepTransaction:
			start := time.Now()
			think0, tx0 := vu.thinkAccum, vu.txc
			sub, abort := vu.execSteps(ctx, st.Steps)
			if ctx.Err() != nil && IsCancel(ctx.Err()) {
				return false, true
			}
			dur := time.Since(start) - (vu.thinkAccum - think0)
			txs := metrics.Sample{Kind: metrics.KindTransaction, Name: st.Name, Start: start, Latency: dur,
				OK: sub, ErrKind: map[bool]string{true: "", false: "transaction"}[sub], VU: vu.id, Iteration: vu.vc.Iter}
			vu.txSince(tx0, &txs)
			vu.r.Col.Add(txs)
			if !sub {
				ok = false
			}
			if abort {
				return false, true
			}
		case model.StepThink:
			vu.think(ctx, st.Think)
		case model.StepLoop:
			for n := 0; n < st.Count; n++ {
				sub, abort := vu.execSteps(ctx, st.Steps)
				ok = ok && sub
				if abort {
					return false, true
				}
			}
		case model.StepIf:
			if vu.vc.EvalCondition(st.Cond) {
				sub, abort := vu.execSteps(ctx, st.Steps)
				ok = ok && sub
				if abort {
					return false, true
				}
			}
		case model.StepGroup:
			inner := st.Steps
			if st.Select == model.SelectWeighted {
				inner = vu.pickWeighted(st.Steps)
			}
			sub, abort := vu.execSteps(ctx, inner)
			ok = ok && sub
			if abort {
				return false, true
			}
		case model.StepSetVar:
			for _, kv := range st.Set {
				v, _ := vu.vc.Expand(kv.Value)
				vu.vc.Vars[kv.Name] = v
			}
		}
	}
	return ok, false
}

// pickWeighted returns a one-element slice holding the step chosen at random in
// proportion to its weight (disabled and zero-weight steps are never chosen).
func (vu *vuState) pickWeighted(steps []model.Step) []model.Step {
	total := 0.0
	for i := range steps {
		if !steps[i].Disabled {
			total += steps[i].StepWeight()
		}
	}
	if total <= 0 {
		return nil
	}
	x := vu.vc.Rand.Float64() * total
	last := -1
	for i := range steps {
		w := steps[i].StepWeight()
		if steps[i].Disabled || w <= 0 {
			continue
		}
		last = i
		if x < w {
			return steps[i : i+1]
		}
		x -= w
	}
	return steps[last : last+1]
}

func (vu *vuState) think(ctx context.Context, t *model.Think) {
	if t == nil {
		return
	}
	scale := 1.0
	if s := vu.r.sc.Workload.ThinkTimeScale; s != nil {
		scale = *s
	}
	d := t.Sample(vu.vc.Rand)
	d = time.Duration(float64(d) * scale)
	if d <= 0 {
		return
	}
	start := time.Now()
	select {
	case <-time.After(d):
	case <-ctx.Done():
	case <-vu.r.stopIters:
		// graceful stop: finish the iteration without waiting out think time
	}
	vu.thinkAccum += time.Since(start)
}

func (vu *vuState) expand(s string, used, unresolved *[]string) string {
	out, res := vu.vc.Expand(s)
	*used = append(*used, res.Used...)
	*unresolved = append(*unresolved, res.Unresolved...)
	return out
}

func (vu *vuState) buildRequest(ctx context.Context, rq *model.Request, used, unres *[]string) (*http.Request, int64, string, error) {
	d := vu.r.sc.Defaults
	u := vu.expand(rq.URL, used, unres)
	if !strings.Contains(u, "://") {
		base := strings.TrimRight(vu.expand(d.BaseURL, used, unres), "/")
		if base == "" {
			return nil, 0, "", fmt.Errorf("relative URL %q needs defaults.baseUrl", u)
		}
		if !strings.HasPrefix(u, "/") {
			u = "/" + u
		}
		u = base + u
	}
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
		return nil, 0, "", fmt.Errorf("invalid URL %q", u)
	}
	method := strings.ToUpper(rq.Method)
	var body io.Reader
	var bodyStr string
	var size int64
	contentType := ""
	switch {
	case len(rq.Multipart) > 0:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, p := range rq.Multipart {
			name := vu.expand(p.Name, used, unres)
			if p.File != "" {
				path, err := SafePath(vu.r.opts.BaseDir, vu.expand(p.File, used, unres))
				if err != nil {
					return nil, 0, "", err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return nil, 0, "", err
				}
				fw, err := mw.CreateFormFile(name, baseName(path))
				if err != nil {
					return nil, 0, "", err
				}
				_, _ = fw.Write(data)
			} else {
				_ = mw.WriteField(name, vu.expand(p.Value, used, unres))
			}
		}
		_ = mw.Close()
		contentType = mw.FormDataContentType()
		bodyStr = fmt.Sprintf("[multipart %d bytes]", buf.Len())
		size = int64(buf.Len())
		body = &buf
	case len(rq.Form) > 0:
		f := url.Values{}
		for _, kv := range rq.Form {
			f.Add(vu.expand(kv.Name, used, unres), vu.expand(kv.Value, used, unres))
		}
		bodyStr = f.Encode()
		contentType = "application/x-www-form-urlencoded"
	case rq.Body != "":
		bodyStr = vu.expand(rq.Body, used, unres)
	}
	if body == nil && bodyStr != "" {
		body = strings.NewReader(bodyStr)
		size = int64(len(bodyStr))
	}
	req, err := http.NewRequestWithContext(ctx, method, pu.String(), body)
	if err != nil {
		return nil, 0, "", err
	}
	for _, h := range d.Headers {
		req.Header.Set(h.Name, vu.expand(h.Value, used, unres))
	}
	for _, h := range rq.Headers {
		v := vu.expand(h.Value, used, unres)
		if strings.EqualFold(h.Name, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(h.Name, v)
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Load Meter-Performance/0.1")
	}
	for k, v := range req.Header {
		size += int64(len(k) + len(strings.Join(v, ",")) + 4)
	}
	size += int64(len(method) + len(pu.RequestURI()) + 12)
	return req, size, bodyStr, nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func classify(err error) string {
	var ne net.Error
	var hna *safety.ErrHostNotAllowed
	switch {
	case errors.As(err, &hna):
		return "blocked_by_policy"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return "connect"
	}
	if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") {
		return "connect"
	}
	return "other"
}

func (vu *vuState) doRequest(ctx context.Context, st *model.Step) bool {
	rq := st.Request
	name := st.Name
	if name == "" {
		name = rq.Method + " " + rq.URL
	}
	var used, unres []string
	timeout := rq.Timeout.D()
	if timeout <= 0 {
		timeout = vu.r.sc.Defaults.Timeout.D()
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	follow := true
	if vu.r.sc.Defaults.FollowRedirects != nil {
		follow = *vu.r.sc.Defaults.FollowRedirects
	}
	if rq.FollowRedirects != nil {
		follow = *rq.FollowRedirects
	}
	if !follow {
		rctx = context.WithValue(rctx, noRedirectKey, true)
	}
	ex := Exchange{VU: vu.id, Iteration: vu.vc.Iter, Step: name}
	req, sent, reqBody, err := vu.buildRequest(rctx, rq, &used, &unres)
	sample := metrics.Sample{Kind: metrics.KindRequest, Name: name, VU: vu.id, Iteration: vu.vc.Iter, Start: time.Now(), BytesOut: sent}
	if err != nil {
		sample.ErrKind, sample.ErrMsg = "invalid_request", err.Error()
		vu.record(sample)
		ex.Error = err.Error()
		vu.inspect(ex)
		return false
	}
	sample.URL = req.URL.String()
	if err := vu.r.limiter.Wait(ctx); err != nil {
		return false
	}
	// exchange.go: timings, split timeouts, auth, compression, wire bytes.
	xr := vu.exchange(rctx, req, rq)
	tm := xr.timing
	vu.cur = &tm
	resp, body, err, lat := xr.resp, xr.body, xr.err, xr.latency
	ex.Method, ex.URL, ex.LatencyMs = req.Method, req.URL.String(), float64(lat.Microseconds())/1000
	ex.ReqHeaders = flatten(req.Header)
	if req.Host != "" {
		ex.ReqHeaders["Host"] = req.Host
	}
	ex.ReqBody = truncate(reqBody, 64<<10)
	ex.VarsUsed, ex.Unresolved = dedupe(used), dedupe(unres)
	xr.fill(&sample, &ex)
	if err != nil {
		if ctx.Err() != nil && IsCancel(ctx.Err()) {
			return false // run cancelled: not a target failure
		}
		sample.ErrKind, sample.ErrMsg = xr.errKind, err.Error()
		vu.record(sample)
		ex.Error = err.Error()
		vu.inspect(ex)
		return false
	}
	sample.Status = resp.StatusCode
	ex.Status = resp.StatusCode
	ex.RespHeaders = flatten(resp.Header)
	ex.RespBody = truncate(string(body), 64<<10)

	ok := true
	hasStatusAssert := false
	for _, a := range rq.Assert {
		if a.Type == model.AsStatus {
			hasStatusAssert = true
		}
	}
	if !hasStatusAssert && !rq.IgnoreStatus && resp.StatusCode >= 400 {
		ok = false
		if resp.StatusCode >= 500 {
			sample.ErrKind = "http_5xx"
		} else {
			sample.ErrKind = "http_4xx"
		}
		sample.ErrMsg = resp.Status
	}
	// Extraction.
	ex.Extracted = map[string]string{}
	var jsonDoc any
	var jsonErr error
	jsonParsed := false
	getJSON := func() (any, error) {
		if !jsonParsed {
			jsonParsed = true
			jsonDoc, jsonErr = extract.ParseJSON(body)
		}
		return jsonDoc, jsonErr
	}
	vu.runExtract(rq.Extract, resp, body, getJSON, &ex, &sample, &ok) // checks.go
	// Assertions.
	for _, a := range rq.Assert {
		res := vu.check(a, resp, body, lat, getJSON, &sample) // checks.go
		ex.Assertions = append(ex.Assertions, res)
		if !res.Passed && (ok || sample.ErrKind == "http_4xx" || sample.ErrKind == "http_5xx") {
			ok = false
			sample.ErrKind = "assertion"
			sample.ErrMsg = res.Message
		}
	}
	if ok && hasStatusAssert {
		sample.ErrKind, sample.ErrMsg = "", ""
	}
	sample.OK = ok
	ex.OK = ok
	if !ok && ex.Error == "" {
		ex.Error = sample.ErrMsg
	}
	vu.record(sample)
	vu.inspect(ex)
	return ok
}

func (vu *vuState) inspect(ex Exchange) {
	if vu.r.opts.Inspect != nil {
		vu.r.opts.Inspect(ex)
	}
}

func (vu *vuState) assert(a model.Assertion, resp *http.Response, body []byte, lat time.Duration, getJSON func() (any, error)) AssertResult {
	res := AssertResult{Type: a.Type, Expr: a.Expr, Value: a.Value}
	val, _ := vu.vc.Expand(a.Value)
	expr, _ := vu.vc.Expand(a.Expr)
	switch a.Type {
	case model.AsStatus:
		res.Passed = statusMatches(resp.StatusCode, val)
		if !res.Passed {
			res.Message = fmt.Sprintf("status %d does not match %s", resp.StatusCode, val)
		}
	case model.AsContains:
		res.Passed = bytes.Contains(body, []byte(val))
		if !res.Passed {
			res.Message = fmt.Sprintf("body does not contain %q", truncate(val, 80))
		}
	case model.AsNotContains:
		res.Passed = !bytes.Contains(body, []byte(val))
		if !res.Passed {
			res.Message = fmt.Sprintf("body contains forbidden text %q", truncate(val, 80))
		}
	case model.AsRegex:
		m, err := extract.Match(string(body), expr)
		res.Passed = err == nil && m
		if !res.Passed {
			res.Message = fmt.Sprintf("body does not match /%s/", expr)
		}
	case model.AsJSONPath:
		doc, err := getJSON()
		if err != nil {
			res.Message = "response is not JSON"
			break
		}
		vals, err := extract.JSONPath(doc, expr)
		if err != nil || len(vals) == 0 {
			res.Message = fmt.Sprintf("jsonpath %s not found", expr)
			break
		}
		if a.Value == "" {
			res.Passed = true
			break
		}
		for _, v := range vals {
			if extract.Stringify(v) == val {
				res.Passed = true
			}
		}
		if !res.Passed {
			res.Message = fmt.Sprintf("jsonpath %s = %q, expected %q", expr, extract.Stringify(vals[0]), val)
		}
	case model.AsMaxLatency:
		d, err := time.ParseDuration(val)
		if err != nil {
			res.Message = "invalid max_latency value " + val
			break
		}
		what := "latency"
		if expr != "" && expr != "total" && vu.cur != nil {
			lat, what = vu.cur.get(expr), expr+" time"
		}
		res.Passed = lat <= d
		if !res.Passed {
			res.Message = fmt.Sprintf("%s %s exceeds %s", what, lat.Round(time.Millisecond), d)
		}
	case model.AsHeader:
		h := resp.Header.Get(expr)
		res.Passed = h != "" && (val == "" || strings.Contains(h, val))
		if !res.Passed {
			res.Message = fmt.Sprintf("header %s = %q, expected %q", expr, h, val)
		}
	}
	return res
}

// statusMatches supports "200", "2xx", "200,201,3xx".
func statusMatches(code int, spec string) bool {
	if spec == "" {
		return code < 400
	}
	s := strconv.Itoa(code)
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == s {
			return true
		}
		if len(p) == 3 && strings.HasSuffix(p, "xx") && p[0] == s[0] {
			return true
		}
	}
	return false
}

func flatten(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

func dedupe(a []string) []string {
	if len(a) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
