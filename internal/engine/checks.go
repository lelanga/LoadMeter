package engine

// JMeter-parity extraction and assertion (G02, G07): every-match extraction
// with name_1..N / name_matchNr / name_gK variables, regex templates and
// alternative sources (headers, request headers, URL, status, message), and
// NOT / custom message / equals / size / hash / JSON regex+null assertions.

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/extract"
	"github.com/lelanga/loadmeter/internal/metrics"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/vars"
)

// sourceText returns the text an extractor or assertion with source src reads.
func sourceText(src string, resp *http.Response, body []byte) string {
	switch src {
	case model.SrcHeaders:
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s\n", resp.Proto, resp.Status)
		writeHeaderLines(&b, resp.Header)
		return b.String()
	case model.SrcRequestHeaders:
		var b strings.Builder
		if rq := resp.Request; rq != nil {
			if h := rq.Host; h != "" {
				fmt.Fprintf(&b, "Host: %s\n", h)
			} else if rq.URL != nil {
				fmt.Fprintf(&b, "Host: %s\n", rq.URL.Host)
			}
			writeHeaderLines(&b, rq.Header)
		}
		return b.String()
	case model.SrcURL:
		if resp.Request != nil && resp.Request.URL != nil {
			return resp.Request.URL.String()
		}
		return ""
	case model.SrcStatus:
		return strconv.Itoa(resp.StatusCode)
	case model.SrcMessage:
		return reason(resp)
	}
	return string(body)
}

// reason is the reason phrase of the status line ("Not Found").
func reason(resp *http.Response) string {
	return strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
}

func writeHeaderLines(b *strings.Builder, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
}

func maxMatches(e model.Extractor) int {
	if e.MaxMatches > 0 {
		return e.MaxMatches
	}
	return model.DefaultMaxMatches
}

// collectMatches returns every candidate value of e, in document order, and
// for regex extractors the capture groups of each match.
func (vu *vuState) collectMatches(e model.Extractor, resp *http.Response, body []byte, getJSON func() (any, error)) ([]string, [][]string, error) {
	var matches []string
	expr, _ := vu.vc.Expand(e.Expr)
	limit := maxMatches(e)
	if e.Match > 0 && int(e.Match) < limit {
		limit = int(e.Match)
	}
	switch e.Type {
	case model.ExJSONPath:
		doc, err := getJSON()
		if err != nil {
			return nil, nil, fmt.Errorf("response is not JSON: %w", err)
		}
		vals, err := extract.JSONPath(doc, expr)
		if err != nil {
			return nil, nil, err
		}
		for _, v := range vals {
			matches = append(matches, extract.Stringify(v))
		}
	case model.ExRegex:
		ms, err := extract.RegexAll(sourceText(e.Source, resp, body), expr, limit)
		if err != nil {
			return nil, nil, err
		}
		tpl, _ := vu.vc.Expand(e.Template)
		g := e.Group
		groups := make([][]string, 0, len(ms))
		for _, m := range ms {
			if tpl != "" {
				matches = append(matches, m.Template(tpl))
			} else {
				ng := len(m.Groups) - 1
				gg := g
				if gg == 0 && ng > 0 {
					gg = 1
				}
				if gg > ng {
					return nil, nil, fmt.Errorf("regex has %d groups, requested %d", ng, gg)
				}
				matches = append(matches, m.Groups[gg])
			}
			groups = append(groups, m.Groups)
		}
		return capMatches(matches, limit), groups, nil
	case model.ExBoundary:
		left, _ := vu.vc.Expand(e.Left)
		right, _ := vu.vc.Expand(e.Right)
		matches = extract.Boundary(sourceText(e.Source, resp, body), left, right)
	case model.ExXPath:
		root, err := extract.ParseXML(body)
		if err != nil {
			return nil, nil, fmt.Errorf("response is not XML: %w", err)
		}
		m, err := extract.XPath(root, expr)
		if err != nil {
			return nil, nil, err
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
		if len(matches) == 0 && vu.client.Jar != nil && resp.Request != nil {
			for _, c := range vu.client.Jar.Cookies(resp.Request.URL) {
				if c.Name == expr {
					matches = append(matches, c.Value)
				}
			}
		}
	case model.ExStatus:
		matches = []string{strconv.Itoa(resp.StatusCode)}
	case model.ExTiming:
		if vu.cur != nil {
			matches = []string{fmtMs(vu.cur.get(expr))}
		}
	}
	return capMatches(matches, maxMatches(e)), nil, nil
}

func capMatches(m []string, n int) []string {
	if n > 0 && len(m) > n {
		return m[:n]
	}
	return m
}

// pickIndex chooses a match: 0 = first, n > 0 = nth, -1 = random; -1 when none.
func pickIndex(n int, m model.MatchIndex, rnd func(int) int) int {
	switch {
	case n == 0:
		return -1
	case m == model.MatchFirst:
		return 0
	case m < 0:
		return rnd(n)
	case int(m) <= n:
		return int(m) - 1
	}
	return -1
}

// saveGroups writes name_g (group count) and name_g0..name_gN (JMeter regex extractor).
func (vu *vuState) saveGroups(name string, groups []string) {
	vu.removeGroups(name)
	vu.vc.SetRaw(name+"_g", strconv.Itoa(len(groups)-1))
	for i, g := range groups {
		vu.vc.SetRaw(name+"_g"+strconv.Itoa(i), g)
	}
}

func (vu *vuState) removeGroups(name string) {
	n, err := strconv.Atoi(vu.vc.Vars[name+"_g"])
	if err != nil {
		return
	}
	vu.vc.Delete(name + "_g")
	for i := 0; i <= n; i++ {
		vu.vc.Delete(name + "_g" + strconv.Itoa(i))
	}
}

// runExtract applies a request's extractors. It replaces the previous
// single-value loop and keeps its semantics for the default match modes.
func (vu *vuState) runExtract(list []model.Extractor, resp *http.Response, body []byte, getJSON func() (any, error),
	ex *Exchange, sample *metrics.Sample, ok *bool) {
	for _, e := range list {
		vals, groups, xerr := vu.collectMatches(e, resp, body, getJSON)
		if xerr != nil {
			vals, groups = nil, nil
		}
		// Leftovers of an earlier "all" extraction: name_matchNr, name_1..N.
		prev, _ := strconv.Atoi(vu.vc.Vars[e.Name+"_matchNr"])
		vu.vc.Delete(e.Name + "_matchNr")
		keep := 0
		val, found := "", false
		if e.Match == model.MatchAll {
			keep = len(vals)
			vu.vc.SetRaw(e.Name+"_matchNr", strconv.Itoa(keep))
			for i, v := range vals {
				n := e.Name + "_" + strconv.Itoa(i+1)
				vu.vc.SetRaw(n, v)
				if groups != nil {
					vu.saveGroups(n, groups[i])
				}
			}
			if keep > 0 {
				vu.vc.SetRaw(e.Name+"_ALL", strings.Join(vals, ","))
				val, found = vals[0], true
				ex.Extracted[e.Name+"_matchNr"] = strconv.Itoa(keep)
			} else {
				vu.vc.Delete(e.Name + "_ALL")
			}
		} else if i := pickIndex(len(vals), e.Match, vu.vc.Rand.IntN); i >= 0 {
			val, found = vals[i], true
			if groups != nil {
				vu.saveGroups(e.Name, groups[i])
			}
		} else if e.Type == model.ExRegex {
			vu.removeGroups(e.Name)
		}
		for i := keep + 1; i <= prev; i++ {
			n := e.Name + "_" + strconv.Itoa(i)
			vu.vc.Delete(n)
			vu.removeGroups(n)
		}
		if !found {
			if e.Default != "" || !e.Required {
				vu.vc.Set(e.Name, e.Default)
			}
			ex.Missing = append(ex.Missing, e.Name)
			if e.Required && *ok {
				*ok = false
				sample.ErrKind = "extraction"
				sample.ErrMsg = fmt.Sprintf("required extraction %q found nothing", e.Name)
				if xerr != nil {
					sample.ErrMsg += ": " + xerr.Error()
				}
			}
			continue
		}
		vu.vc.SetRaw(e.Name, val)
		ex.Extracted[e.Name] = val
	}
}

// check evaluates one assertion, including the parity extensions, then
// applies Not and the custom failure message.
func (vu *vuState) check(a model.Assertion, resp *http.Response, body []byte, lat time.Duration,
	getJSON func() (any, error), sample *metrics.Sample) AssertResult {
	var res AssertResult
	switch a.Type {
	case model.AsEquals, model.AsSize, model.AsHash:
		res = vu.assertExtra(a, resp, body, sample)
	case model.AsContains, model.AsNotContains, model.AsRegex:
		text := body
		if a.Source != "" && a.Source != model.SrcBody {
			text = []byte(sourceText(a.Source, resp, body))
		}
		res = vu.assert(a, resp, text, lat, getJSON)
		if a.Source != "" && a.Source != model.SrcBody && !res.Passed {
			res.Message = strings.Replace(res.Message, "body", a.Source, 1)
		}
	case model.AsJSONPath:
		if a.Regex || a.ExpectNull {
			res = vu.assertJSONExtra(a, getJSON)
		} else {
			res = vu.assert(a, resp, body, lat, getJSON)
		}
	default:
		res = vu.assert(a, resp, body, lat, getJSON)
	}
	if a.Not {
		res.Passed = !res.Passed
		if res.Passed {
			res.Message = ""
		} else {
			res.Message = "NOT " + describeAssertion(a) + ": the check passed but was expected to fail"
		}
	}
	if !res.Passed && a.Message != "" {
		res.Message, _ = vu.vc.Expand(a.Message)
	}
	return res
}

func describeAssertion(a model.Assertion) string {
	parts := []string{a.Type}
	if a.Expr != "" {
		parts = append(parts, a.Expr)
	}
	if a.Op != "" {
		parts = append(parts, a.Op)
	}
	if a.Value != "" {
		parts = append(parts, strconv.Quote(truncate(a.Value, 80)))
	}
	return strings.Join(parts, " ")
}

func (vu *vuState) assertExtra(a model.Assertion, resp *http.Response, body []byte, sample *metrics.Sample) AssertResult {
	res := AssertResult{Type: a.Type, Expr: a.Expr, Value: a.Value}
	val, _ := vu.vc.Expand(a.Value)
	switch a.Type {
	case model.AsEquals:
		src := a.Source
		if src == "" {
			src = model.SrcBody
		}
		text := sourceText(src, resp, body)
		res.Passed = text == val
		if !res.Passed {
			res.Message = fmt.Sprintf("%s %q does not equal %q", src, truncate(text, 80), truncate(val, 80))
		}
	case model.AsSize:
		want, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			res.Message = "invalid size value " + val
			break
		}
		what, got := "body", sample.BodyBytes
		if got == 0 {
			got = int64(len(body))
		}
		if a.Expr == "full" {
			what, got = "response", sample.BytesIn
		}
		op := a.Op
		if op == "" {
			op = "=="
		}
		res.Passed = compareInt(got, op, want)
		if !res.Passed {
			res.Message = fmt.Sprintf("%s size %d bytes is not %s %d", what, got, op, want)
		}
	case model.AsHash:
		alg := strings.ToLower(a.Expr)
		if alg == "" {
			alg = "md5"
		}
		h, err := vars.NewHash(alg)
		if err != nil {
			res.Message = err.Error()
			break
		}
		h.Write(body)
		got := hex.EncodeToString(h.Sum(nil))
		res.Passed = strings.EqualFold(got, strings.TrimSpace(val))
		if !res.Passed {
			res.Message = fmt.Sprintf("body %s %s, expected %s", alg, got, strings.ToLower(strings.TrimSpace(val)))
		}
	}
	return res
}

func compareInt(got int64, op string, want int64) bool {
	switch op {
	case "==":
		return got == want
	case "!=":
		return got != want
	case "<":
		return got < want
	case "<=":
		return got <= want
	case ">":
		return got > want
	case ">=":
		return got >= want
	}
	return false
}

func (vu *vuState) assertJSONExtra(a model.Assertion, getJSON func() (any, error)) AssertResult {
	res := AssertResult{Type: a.Type, Expr: a.Expr, Value: a.Value}
	expr, _ := vu.vc.Expand(a.Expr)
	doc, err := getJSON()
	if err != nil {
		res.Message = "response is not JSON"
		return res
	}
	vals, err := extract.JSONPath(doc, expr)
	if err != nil || len(vals) == 0 {
		res.Message = fmt.Sprintf("jsonpath %s not found", expr)
		return res
	}
	if a.ExpectNull {
		for _, v := range vals {
			if v == nil {
				res.Passed = true
			}
		}
		if !res.Passed {
			res.Message = fmt.Sprintf("jsonpath %s = %q, expected null", expr, extract.Stringify(vals[0]))
		}
		return res
	}
	val, _ := vu.vc.Expand(a.Value)
	re, err := regexp.Compile("^(?:" + val + ")$")
	if err != nil {
		res.Message = fmt.Sprintf("invalid regex %q: %v", val, err)
		return res
	}
	for _, v := range vals {
		if re.MatchString(extract.Stringify(v)) {
			res.Passed = true
		}
	}
	if !res.Passed {
		res.Message = fmt.Sprintf("jsonpath %s = %q does not match /%s/", expr, extract.Stringify(vals[0]), val)
	}
	return res
}

// ---------- transaction sample counts (JTL transaction rows) ----------

// txCount counts the request samples a VU recorded, so a transaction can
// report "Number of samples in transaction : N, number of failing samples : M"
// like JMeter's Transaction Controller (JMeter's report generator relies on it).
type txCount struct{ n, failed int }

// record adds a request sample to the collector and counts it.
func (vu *vuState) record(s metrics.Sample) {
	vu.txc.n++
	if !s.OK {
		vu.txc.failed++
	}
	vu.r.Col.Add(s)
}

// txSince fills a transaction sample's sub-sample counts since start.
func (vu *vuState) txSince(start txCount, s *metrics.Sample) {
	s.SubSamples, s.SubFailures = vu.txc.n-start.n, vu.txc.failed-start.failed
}
