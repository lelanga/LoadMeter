// Package accesslog turns web-server access logs (nginx/Apache combined and
// common, AWS ALB, CloudFront standard logs) into a traffic profile and a
// Load Meter scenario that replays the observed endpoint mix and request-rate
// curve. Parsing is streaming, tolerant (malformed lines are counted, never
// fatal) and never keeps client addresses: they are hashed into session keys
// that live only in memory.
package accesslog

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Log formats.
const (
	FormatAuto       = "auto"
	FormatNginx      = "nginx"  // combined or common (same parser)
	FormatApache     = "apache" // combined or common (same parser)
	FormatALB        = "alb"
	FormatCloudFront = "cloudfront"
)

// Record is one parsed request line. Client is only used to build session keys.
type Record struct {
	Time   time.Time
	Method string
	Target string // path plus optional ?query, as logged
	Host   string // only ALB and CloudFront carry it
	Status int
	Client string
	UA     string
	Cookie string // CloudFront cs(Cookie) when logged
}

var errMalformed = errors.New("malformed line")

// parser parses one format; it may be stateful (CloudFront #Fields).
type parser interface {
	// parse returns skip=true for comment/header lines that are not records.
	parse(line string) (rec Record, skip bool, err error)
}

func newParser(format string) (parser, error) {
	switch format {
	case FormatNginx, FormatApache, "combined", "common":
		return &combinedParser{}, nil
	case FormatALB:
		return &albParser{}, nil
	case FormatCloudFront:
		return &cfParser{}, nil
	}
	return nil, fmt.Errorf("unknown log format %q (expected auto, nginx, apache, alb or cloudfront)", format)
}

// splitFields splits on spaces, keeping "quoted" and [bracketed] fields as one
// token without their delimiters. Inside quotes, \" and \\ are unescaped.
func splitFields(line string) ([]string, bool) {
	var out []string
	i, n := 0, len(line)
	for i < n {
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		switch line[i] {
		case '"':
			i++
			var b strings.Builder
			closed := false
			for i < n {
				c := line[i]
				if c == '\\' && i+1 < n && (line[i+1] == '"' || line[i+1] == '\\') {
					b.WriteByte(line[i+1])
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return out, false
			}
			out = append(out, b.String())
		case '[':
			j := strings.IndexByte(line[i:], ']')
			if j < 0 {
				return out, false
			}
			out = append(out, line[i+1:i+j])
			i += j + 1
		default:
			j := i
			for j < n && line[j] != ' ' && line[j] != '\t' {
				j++
			}
			out = append(out, line[i:j])
			i = j
		}
	}
	return out, true
}

func dash(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

// ---------- nginx / Apache combined and common ----------

type combinedParser struct{}

const combinedTime = "02/Jan/2006:15:04:05 -0700"

func (combinedParser) parse(line string) (Record, bool, error) {
	f, ok := splitFields(line)
	// common: 7 fields; combined: 9+ (nginx may append request time etc.)
	if !ok || len(f) < 7 {
		return Record{}, false, errMalformed
	}
	t, err := time.Parse(combinedTime, f[3])
	if err != nil {
		return Record{}, false, errMalformed
	}
	parts := strings.Fields(f[4])
	if len(parts) < 2 || !isMethod(parts[0]) {
		return Record{}, false, errMalformed
	}
	st, err := strconv.Atoi(f[5])
	if err != nil || st < 100 || st > 599 {
		return Record{}, false, errMalformed
	}
	r := Record{Time: t, Method: strings.ToUpper(parts[0]), Target: parts[1], Status: st, Client: f[0]}
	if len(f) >= 9 {
		r.UA = dash(f[8])
	}
	return r, false, nil
}

func isMethod(m string) bool {
	if m == "" || len(m) > 16 {
		return false
	}
	for _, c := range m {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// ---------- AWS ALB ----------

type albParser struct{}

func (albParser) parse(line string) (Record, bool, error) {
	f, ok := splitFields(line)
	if !ok || len(f) < 13 {
		return Record{}, false, errMalformed
	}
	switch f[0] {
	case "http", "https", "h2", "grpcs", "ws", "wss":
	default:
		return Record{}, false, errMalformed
	}
	t, err := time.Parse(time.RFC3339Nano, f[1])
	if err != nil {
		return Record{}, false, errMalformed
	}
	st, err := strconv.Atoi(f[8])
	if err != nil || st < 100 || st > 599 {
		return Record{}, false, errMalformed
	}
	parts := strings.Fields(f[12])
	if len(parts) < 2 || !isMethod(parts[0]) {
		return Record{}, false, errMalformed
	}
	r := Record{Time: t, Method: strings.ToUpper(parts[0]), Status: st, Client: f[3]}
	if i := strings.LastIndexByte(f[3], ':'); i > 0 {
		r.Client = f[3][:i]
	}
	// ALB logs the absolute URL: https://host:443/path?query
	if u, err := url.Parse(parts[1]); err == nil && u.Host != "" {
		r.Host = strings.ToLower(u.Hostname())
		r.Target = u.RequestURI()
	} else {
		r.Target = parts[1]
	}
	if len(f) > 13 {
		r.UA = dash(f[13])
	}
	return r, false, nil
}

// ---------- CloudFront standard logs (tab-separated W3C) ----------

type cfParser struct{ idx map[string]int }

var cfDefault = []string{"date", "time", "x-edge-location", "sc-bytes", "c-ip", "cs-method", "cs(Host)", "cs-uri-stem",
	"sc-status", "cs(Referer)", "cs(User-Agent)", "cs-uri-query", "cs(Cookie)"}

func (p *cfParser) parse(line string) (Record, bool, error) {
	if strings.HasPrefix(line, "#") {
		if rest, ok := strings.CutPrefix(line, "#Fields:"); ok {
			p.idx = map[string]int{}
			for i, name := range strings.Fields(rest) {
				p.idx[name] = i
			}
		}
		return Record{}, true, nil
	}
	if p.idx == nil {
		p.idx = map[string]int{}
		for i, name := range cfDefault {
			p.idx[name] = i
		}
	}
	f := strings.Split(line, "\t")
	get := func(name string) string {
		if i, ok := p.idx[name]; ok && i < len(f) {
			return dash(f[i])
		}
		return ""
	}
	if len(f) < 8 {
		return Record{}, false, errMalformed
	}
	t, err := time.Parse("2006-01-02 15:04:05", get("date")+" "+get("time"))
	if err != nil {
		return Record{}, false, errMalformed
	}
	st, err := strconv.Atoi(get("sc-status"))
	if err != nil || st < 100 || st > 599 {
		return Record{}, false, errMalformed
	}
	m := get("cs-method")
	stem := get("cs-uri-stem")
	if !isMethod(m) || !strings.HasPrefix(stem, "/") {
		return Record{}, false, errMalformed
	}
	r := Record{Time: t, Method: m, Target: stem, Status: st, Client: get("c-ip"),
		Host: strings.ToLower(get("cs(Host)")), Cookie: get("cs(Cookie)")}
	if c, err := url.PathUnescape(r.Cookie); err == nil {
		r.Cookie = c
	}
	if q := get("cs-uri-query"); q != "" {
		r.Target += "?" + q
	}
	if ua, err := url.QueryUnescape(get("cs(User-Agent)")); err == nil {
		r.UA = ua
	} else {
		r.UA = get("cs(User-Agent)")
	}
	return r, false, nil
}

// detect returns the format whose parser accepts the line, or "".
func detect(line string) string {
	if strings.HasPrefix(line, "#Version:") || strings.HasPrefix(line, "#Fields:") {
		return FormatCloudFront
	}
	if strings.Count(line, "\t") >= 7 {
		if _, _, err := (&cfParser{}).parse(line); err == nil {
			return FormatCloudFront
		}
	}
	if _, _, err := (albParser{}).parse(line); err == nil {
		return FormatALB
	}
	if _, _, err := (combinedParser{}).parse(line); err == nil {
		return FormatNginx
	}
	return ""
}
