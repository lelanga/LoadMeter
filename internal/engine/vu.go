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
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/extract"
	"github.com/lelanga/phoenix/internal/metrics"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/safety"
	"github.com/lelanga/phoenix/internal/vars"
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
}

func (r *Run) newVU(id int) *vuState {
	vu := &vuState{r: r, id: id, vc: vars.NewContext(id, r.opts.Seed), vuRows: map[string]bool{}}
	for k, v := range r.sc.Variables {
		vu.vc.Vars[k] = v
	}
	c := &http.Client{Transport: r.transport}
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
		switch st.Type {
		case model.StepRequest:
			if !vu.doRequest(ctx, st) {
				ok = false
				if vu.onErrorAbort() {
					return false, true
				}
			}
		case model.StepTransaction:
			start := time.Now()
			think0 := vu.thinkAccum
			sub, abort := vu.execSteps(ctx, st.Steps)
			if ctx.Err() != nil && IsCancel(ctx.Err()) {
				return false, true
			}
			dur := time.Since(start) - (vu.thinkAccum - think0)
			vu.r.Col.Add(metrics.Sample{Kind: metrics.KindTransaction, Name: st.Name, Start: start, Latency: dur,
				OK: sub, ErrKind: map[bool]string{true: "", false: "transaction"}[sub], VU: vu.id, Iteration: vu.vc.Iter})
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
			sub, abort := vu.execSteps(ctx, st.Steps)
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

func (vu *vuState) think(ctx context.Context, t *model.Think) {
	if t == nil {
		return
	}
	scale := 1.0
	if s := vu.r.sc.Workload.ThinkTimeScale; s != nil {
		scale = *s
	}
	d := t.Min.D()
	if t.Max > t.Min {
		d += time.Duration(vu.vc.Rand.Int64N(int64(t.Max - t.Min)))
	}
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
		req.Header.Set("User-Agent", "Phoenix-Performance/0.1")
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
		vu.r.Col.Add(sample)
		ex.Error = err.Error()
		vu.inspect(ex)
		return false
	}
	sample.URL = req.URL.String()
	if err := vu.r.limiter.Wait(ctx); err != nil {
		return false
	}
	var ttfb time.Time
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb = time.Now() }}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	start := time.Now()
	sample.Start = start
	resp, err := vu.client.Do(req)
	maxBody := vu.r.sc.Defaults.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	var body []byte
	var bytesIn int64
	if err == nil {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		bytesIn = int64(len(body)) + n
		for k, v := range resp.Header {
			bytesIn += int64(len(k) + len(strings.Join(v, ",")) + 4)
		}
	}
	lat := time.Since(start)
	sample.Latency = lat
	if !ttfb.IsZero() {
		sample.TTFB = ttfb.Sub(start)
	}
	sample.BytesIn = bytesIn
	ex.Method, ex.URL, ex.LatencyMs = req.Method, req.URL.String(), float64(lat.Microseconds())/1000
	ex.ReqHeaders = flatten(req.Header)
	if req.Host != "" {
		ex.ReqHeaders["Host"] = req.Host
	}
	ex.ReqBody = truncate(reqBody, 64<<10)
	ex.VarsUsed, ex.Unresolved = dedupe(used), dedupe(unres)
	if err != nil {
		if ctx.Err() != nil && IsCancel(ctx.Err()) {
			return false // run cancelled: not a target failure
		}
		sample.ErrKind, sample.ErrMsg = classify(err), err.Error()
		vu.r.Col.Add(sample)
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
	if !hasStatusAssert && resp.StatusCode >= 400 {
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
	for _, e := range rq.Extract {
		val, found, xerr := vu.extractOne(e, resp, body, getJSON)
		if xerr != nil || !found {
			if e.Default != "" || !e.Required {
				vu.vc.Vars[e.Name] = e.Default
			}
			ex.Missing = append(ex.Missing, e.Name)
			if e.Required && ok {
				ok = false
				sample.ErrKind = "extraction"
				sample.ErrMsg = fmt.Sprintf("required extraction %q found nothing", e.Name)
				if xerr != nil {
					sample.ErrMsg += ": " + xerr.Error()
				}
			}
			continue
		}
		vu.vc.Vars[e.Name] = val
		ex.Extracted[e.Name] = val
	}
	// Assertions.
	for _, a := range rq.Assert {
		res := vu.assert(a, resp, body, lat, getJSON)
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
	vu.r.Col.Add(sample)
	vu.inspect(ex)
	return ok
}

func (vu *vuState) inspect(ex Exchange) {
	if vu.r.opts.Inspect != nil {
		vu.r.opts.Inspect(ex)
	}
}

func (vu *vuState) extractOne(e model.Extractor, resp *http.Response, body []byte, getJSON func() (any, error)) (string, bool, error) {
	var matches []string
	expr, _ := vu.vc.Expand(e.Expr)
	switch e.Type {
	case model.ExJSONPath:
		doc, err := getJSON()
		if err != nil {
			return "", false, fmt.Errorf("response is not JSON: %w", err)
		}
		vals, err := extract.JSONPath(doc, expr)
		if err != nil {
			return "", false, err
		}
		for _, v := range vals {
			matches = append(matches, extract.Stringify(v))
		}
	case model.ExRegex:
		m, err := extract.Regex(string(body), expr, e.Group)
		if err != nil {
			return "", false, err
		}
		matches = m
	case model.ExBoundary:
		matches = extract.Boundary(string(body), e.Left, e.Right)
	case model.ExXPath:
		root, err := extract.ParseXML(body)
		if err != nil {
			return "", false, fmt.Errorf("response is not XML: %w", err)
		}
		m, err := extract.XPath(root, expr)
		if err != nil {
			return "", false, err
		}
		matches = m
	case model.ExHeader:
		matches = resp.Header.Values(expr)
	case model.ExCookie:
		for _, c := range resp.Cookies() {
			if c.Name == expr {
				matches = append(matches, c.Value)
			}
		}
		if len(matches) == 0 && vu.client.Jar != nil {
			for _, c := range vu.client.Jar.Cookies(resp.Request.URL) {
				if c.Name == expr {
					matches = append(matches, c.Value)
				}
			}
		}
	case model.ExStatus:
		matches = []string{strconv.Itoa(resp.StatusCode)}
	}
	v, ok := extract.Pick(matches, e.Match, vu.vc.Rand.IntN)
	return v, ok, nil
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
		res.Passed = lat <= d
		if !res.Passed {
			res.Message = fmt.Sprintf("latency %s exceeds %s", lat.Round(time.Millisecond), d)
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
