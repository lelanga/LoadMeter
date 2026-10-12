// Package har imports HTTP Archive (HAR 1.2) captures into Phoenix scenarios.
package har

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/correlate"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/redact"
)

type nv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type file struct {
	Log struct {
		Version string `json:"version"`
		Creator struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"creator"`
		Pages []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"pages"`
		Entries []entry `json:"entries"`
	} `json:"log"`
}

type entry struct {
	PageRef         string  `json:"pageref"`
	StartedDateTime string  `json:"startedDateTime"`
	Time            float64 `json:"time"`
	Request         struct {
		Method   string `json:"method"`
		URL      string `json:"url"`
		Headers  []nv   `json:"headers"`
		PostData *struct {
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Params   []struct {
				Name        string `json:"name"`
				Value       string `json:"value"`
				FileName    string `json:"fileName"`
				ContentType string `json:"contentType"`
			} `json:"params"`
		} `json:"postData"`
	} `json:"request"`
	Response struct {
		Status      int    `json:"status"`
		Headers     []nv   `json:"headers"`
		RedirectURL string `json:"redirectURL"`
		Content     struct {
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Encoding string `json:"encoding"`
		} `json:"content"`
	} `json:"response"`
}

type Options struct {
	Name            string
	IncludeStatic   bool
	KeepCredentials bool     // keep Authorization/API-key header values and passwords as recorded
	ExcludeHosts    []string // additional hosts to drop
	ThinkThreshold  time.Duration
}

var staticExt = map[string]bool{".js": true, ".mjs": true, ".css": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true, ".map": true, ".webp": true, ".avif": true,
	".mp4": true, ".webm": true, ".mp3": true, ".wav": true, ".bmp": true}

var analyticsHosts = []string{"google-analytics.com", "googletagmanager.com", "doubleclick.net", "facebook.net", "facebook.com/tr",
	"hotjar.com", "segment.io", "segment.com", "mixpanel.com", "newrelic.com", "nr-data.net", "datadoghq.com", "browser-intake",
	"sentry.io", "clarity.ms", "bing.com/bat", "linkedin.com/px", "fullstory.com", "amplitude.com", "optimizely.com", "intercom.io",
	"gstatic.com", "fonts.googleapis.com", "cdn.jsdelivr.net", "cdnjs.cloudflare.com", "unpkg.com", "googleadservices.com", "adservice.google"}

// dropHeaders are managed by the HTTP client or browser-specific.
var dropHeaders = map[string]bool{"host": true, "content-length": true, "connection": true, "keep-alive": true, "transfer-encoding": true,
	"upgrade": true, "proxy-connection": true, "te": true, "accept-encoding": true, "cookie": true, "if-none-match": true,
	"if-modified-since": true, "upgrade-insecure-requests": true, "priority": true}

var credentialHeaders = map[string]bool{"authorization": true, "proxy-authorization": true, "x-api-key": true, "api-key": true,
	"x-auth-token": true, "x-access-token": true, "apikey": true}

const maxHAR = 512 << 20

// Import parses a HAR and returns a scenario, compatibility report and the
// recording (requests + responses) used for correlation analysis.
func Import(r io.Reader, source string, o Options) (*model.Scenario, *model.CompatReport, *correlate.Recording, error) {
	var h file
	dec := json.NewDecoder(io.LimitReader(r, maxHAR))
	if err := dec.Decode(&h); err != nil {
		return nil, nil, nil, fmt.Errorf("not a valid HAR file: %w", err)
	}
	if len(h.Log.Entries) == 0 {
		return nil, nil, nil, fmt.Errorf("HAR contains no entries")
	}
	if o.ThinkThreshold <= 0 {
		o.ThinkThreshold = 800 * time.Millisecond
	}
	name := o.Name
	if name == "" {
		name = strings.TrimSuffix(path.Base(strings.ReplaceAll(source, `\`, "/")), path.Ext(source))
	}
	rep := &model.CompatReport{Source: source, Format: "har"}
	sc := model.NewScenario(name)
	sc.Description = fmt.Sprintf("Imported from %s (%d entries, created by %s %s).", path.Base(source), len(h.Log.Entries), h.Log.Creator.Name, h.Log.Creator.Version)
	sc.Metadata = map[string]string{"importer": "har", "source": path.Base(source)}

	sort.SliceStable(h.Log.Entries, func(i, j int) bool { return h.Log.Entries[i].StartedDateTime < h.Log.Entries[j].StartedDateTime })

	excluded := map[string][]string{}
	type kept struct {
		e     *entry
		u     *url.URL
		start time.Time
	}
	var keep []kept
	for i := range h.Log.Entries {
		e := &h.Log.Entries[i]
		u, err := url.Parse(e.Request.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			excluded["non-HTTP or invalid URL (data:, blob:, extensions)"] = append(excluded["non-HTTP or invalid URL (data:, blob:, extensions)"], e.Request.URL)
			continue
		}
		reason := ""
		switch {
		case e.Response.Status == 101 || strings.EqualFold(header(e.Request.Headers, "Upgrade"), "websocket"):
			reason = "WebSocket upgrade (WebSocket replay is not supported yet)"
		case e.Request.Method == "OPTIONS":
			reason = "CORS preflight (browser-generated)"
		case isAnalytics(u):
			reason = "analytics/tracking or third-party CDN"
		case hostExcluded(u.Hostname(), o.ExcludeHosts):
			reason = "host excluded by user"
		case !o.IncludeStatic && isStatic(u, e.Response.Content.MimeType):
			reason = "static resource (use includeStatic to keep)"
		case e.Response.Status == 0:
			reason = "request without response (blocked or aborted in the browser)"
		}
		if reason != "" {
			excluded[reason] = append(excluded[reason], e.Request.Method+" "+e.Request.URL)
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, e.StartedDateTime)
		keep = append(keep, kept{e, u, t})
	}
	reasons := make([]string, 0, len(excluded))
	for k := range excluded {
		reasons = append(reasons, k)
	}
	sort.Strings(reasons)
	for _, k := range reasons {
		urls := excluded[k]
		sample := urls
		if len(sample) > 5 {
			sample = sample[:5]
		}
		status := model.Ignored
		if strings.HasPrefix(k, "WebSocket") {
			status = model.Unsupported
		}
		rep.Add("entries", fmt.Sprintf("%d excluded", len(urls)), "", status, k+": "+strings.Join(sample, ", ")+map[bool]string{true: ", …", false: ""}[len(urls) > 5])
	}
	if len(keep) == 0 {
		return nil, rep, nil, fmt.Errorf("all %d entries were filtered out; try includeStatic", len(h.Log.Entries))
	}

	// Base URL: most frequent origin.
	origins := map[string]int{}
	for _, k := range keep {
		origins[k.u.Scheme+"://"+k.u.Host]++
	}
	base, best := "", 0
	for o, n := range origins {
		if n > best || n == best && o < base {
			base, best = o, n
		}
	}
	sc.Variables["baseUrl"] = base
	sc.Defaults.BaseURL = "${baseUrl}"
	sc.Defaults.Timeout = model.Duration(30 * time.Second)

	pageTitle := map[string]string{}
	for _, p := range h.Log.Pages {
		pageTitle[p.ID] = p.Title
	}

	rec := &correlate.Recording{Source: source}
	usedNames := map[string]int{}
	var groups []model.Step
	var cur *model.Step
	var lastEnd time.Time
	credWarned := map[string]bool{}
	skipNext := map[int]bool{}
	txCount := 0
	for i, k := range keep {
		if skipNext[i] {
			continue
		}
		e := k.e
		// Transaction grouping: HAR page, else time gaps.
		groupKey := e.PageRef
		gap := time.Duration(0)
		if !lastEnd.IsZero() && !k.start.IsZero() {
			gap = k.start.Sub(lastEnd)
		}
		newGroup := cur == nil || (groupKey != "" && groupKey != cur.Comment) || (groupKey == "" && gap > o.ThinkThreshold*2)
		if newGroup {
			if cur != nil && gap > o.ThinkThreshold {
				g := min(gap, 30*time.Second).Round(100 * time.Millisecond)
				groups = append(groups, *cur, model.Step{Type: model.StepThink, Think: &model.Think{Min: model.Duration(g)}, Comment: "recorded think time"})
			} else if cur != nil {
				groups = append(groups, *cur)
			}
			title := pageTitle[groupKey]
			txCount++
			tname := fmt.Sprintf("T%02d", txCount)
			if title != "" {
				tname += "_" + sanitize(title)
			} else {
				tname += "_" + sanitize(k.u.Path)
			}
			cur = &model.Step{Type: model.StepTransaction, Name: tname, Comment: groupKey}
		}
		end := k.start.Add(time.Duration(e.Time * float64(time.Millisecond)))
		if end.After(lastEnd) {
			lastEnd = end
		}

		req := &model.Request{Method: strings.ToUpper(e.Request.Method)}
		if k.u.Scheme+"://"+k.u.Host == base {
			req.URL = k.u.RequestURI()
		} else {
			req.URL = k.u.String()
		}
		for _, hd := range e.Request.Headers {
			ln := strings.ToLower(hd.Name)
			if strings.HasPrefix(ln, ":") || dropHeaders[ln] {
				continue
			}
			v := hd.Value
			if credentialHeaders[ln] && !o.KeepCredentials {
				vn := "secret_" + strings.ReplaceAll(ln, "-", "_")
				if _, ok := sc.Variables[vn]; !ok {
					sc.Variables[vn] = ""
				}
				if !credWarned[ln] {
					credWarned[ln] = true
					rep.Manual = append(rep.Manual, fmt.Sprintf("Header %s was recorded with a credential; it was replaced by ${%s}. Apply the correlation suggestion or set the variable (e.g. --set %s=... or ${__env(PHOENIX_...)}).", hd.Name, vn, vn))
				}
				v = "${" + vn + "}"
			}
			req.Headers = append(req.Headers, model.Header{Name: hd.Name, Value: v})
		}
		if pd := e.Request.PostData; pd != nil {
			mt := strings.ToLower(pd.MimeType)
			switch {
			case strings.HasPrefix(mt, "multipart/form-data"):
				for _, p := range pd.Params {
					if p.FileName != "" {
						req.Multipart = append(req.Multipart, model.Part{Name: p.Name, File: "files/" + path.Base(p.FileName), ContentType: p.ContentType})
						rep.Manual = append(rep.Manual, fmt.Sprintf("Upload %q in %s needs the file at files/%s next to the scenario (HAR does not contain file bytes).", p.FileName, req.URL, path.Base(p.FileName)))
					} else {
						req.Multipart = append(req.Multipart, model.Part{Name: p.Name, Value: p.Value})
					}
				}
				req.Headers = removeHeader(req.Headers, "Content-Type") // boundary is regenerated
			case strings.HasPrefix(mt, "application/x-www-form-urlencoded") && pd.Text != "":
				vals, err := url.ParseQuery(pd.Text)
				if err == nil {
					for _, kv := range orderedForm(pd.Text) {
						v := vals.Get(kv)
						if !o.KeepCredentials && isPasswordField(kv) {
							sc.Variables["password"] = ""
							rep.Manual = append(rep.Manual, fmt.Sprintf("Form field %q in %s looked like a password; it now uses ${password}. Provide test credentials via a data file or --set password=...", kv, req.URL))
							v = "${password}"
						}
						req.Form = append(req.Form, model.KV{Name: kv, Value: v})
					}
					req.Headers = removeHeader(req.Headers, "Content-Type")
				} else {
					req.Body = pd.Text
				}
			default:
				req.Body = pd.Text
				if !o.KeepCredentials && strings.Contains(mt, "json") {
					req.Body = maskJSONPasswords(req.Body, sc, rep, req.URL)
				}
			}
		}
		// Redirect chains: if this response redirects to the next kept entry, follow automatically.
		status := e.Response.Status
		if status >= 300 && status < 400 && i+1 < len(keep) {
			loc := e.Response.RedirectURL
			if loc == "" {
				loc = header(e.Response.Headers, "Location")
			}
			if loc != "" {
				target := k.u.ResolveReference(mustParse(loc)).String()
				if keep[i+1].e.Request.URL == target && keep[i+1].e.Request.Method == "GET" {
					skipNext[i+1] = true
					status = keep[i+1].e.Response.Status
					rep.Add("redirect", e.Request.Method+" "+k.u.Path, "", model.Supported, "redirect to "+keep[i+1].u.Path+" is followed automatically")
				}
			}
		}
		if status >= 300 && status < 400 && !skipNext[i+1] {
			f := false
			req.FollowRedirects = &f
		}
		if status > 0 && status < 400 {
			req.Assert = append(req.Assert, model.Assertion{Type: model.AsStatus, Value: fmt.Sprint(status)})
		} else if status >= 400 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s %s was recorded with status %d; no status assertion added.", req.Method, req.URL, status))
		}
		sname := req.Method + " " + k.u.Path
		usedNames[sname]++
		if n := usedNames[sname]; n > 1 {
			sname = fmt.Sprintf("%s #%d", sname, n)
		}
		cur.Steps = append(cur.Steps, model.Step{Type: model.StepRequest, Name: sname, Request: req})
		rep.Add("request", sname, fmt.Sprintf("entry %d", i), model.Supported, "")

		// Recording keeps raw values for correlation (stored locally only).
		ex := correlate.Exchange{Step: sname, Method: req.Method, URL: e.Request.URL, Status: e.Response.Status}
		for _, hd := range e.Request.Headers {
			if !strings.HasPrefix(hd.Name, ":") {
				ex.ReqHeaders = append(ex.ReqHeaders, model.Header{Name: hd.Name, Value: hd.Value})
			}
		}
		if e.Request.PostData != nil {
			ex.ReqBody = e.Request.PostData.Text
		}
		for _, hd := range e.Response.Headers {
			ex.RespHeaders = append(ex.RespHeaders, model.Header{Name: hd.Name, Value: hd.Value})
		}
		body := e.Response.Content.Text
		if e.Response.Content.Encoding == "base64" {
			if b, err := base64.StdEncoding.DecodeString(body); err == nil && isTextual(e.Response.Content.MimeType) {
				body = string(b)
			} else {
				body = ""
			}
		}
		ex.RespBody = body
		rec.Exchanges = append(rec.Exchanges, ex)
	}
	if cur != nil {
		groups = append(groups, *cur)
	}
	for i := range groups {
		groups[i].Comment = ""
	}
	sc.Steps = groups
	sc.Workload = model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(time.Minute)}
	if !o.KeepCredentials {
		rep.Warnings = append(rep.Warnings, "Credentials in headers and password fields were replaced with variables. The local recording file still contains raw values; it is stored with owner-only permissions and is never sent to AI providers without redaction and your consent.")
	}
	rep.Warnings = append(rep.Warnings, "Cookies are not replayed from the capture; each virtual user gets its own cookie jar that is filled by the server's Set-Cookie responses.")
	rep.ComputeConfidence()
	return sc, rep, rec, nil
}

func header(h []nv, name string) string {
	for _, x := range h {
		if strings.EqualFold(x.Name, name) {
			return x.Value
		}
	}
	return ""
}

func removeHeader(h []model.Header, name string) []model.Header {
	out := h[:0]
	for _, x := range h {
		if !strings.EqualFold(x.Name, name) {
			out = append(out, x)
		}
	}
	return out
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return &url.URL{}
	}
	return u
}

func isAnalytics(u *url.URL) bool {
	s := strings.ToLower(u.Host + u.Path)
	for _, a := range analyticsHosts {
		if strings.Contains(s, a) {
			return true
		}
	}
	return false
}

func hostExcluded(h string, ex []string) bool {
	for _, x := range ex {
		if strings.EqualFold(strings.TrimSpace(x), h) {
			return true
		}
	}
	return false
}

func isStatic(u *url.URL, mime string) bool {
	if staticExt[strings.ToLower(path.Ext(u.Path))] {
		return true
	}
	m := strings.ToLower(mime)
	return strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "font/") || strings.Contains(m, "css") ||
		strings.Contains(m, "javascript") || strings.HasPrefix(m, "video/") || strings.HasPrefix(m, "audio/")
}

func isTextual(mime string) bool {
	m := strings.ToLower(mime)
	return strings.HasPrefix(m, "text/") || strings.Contains(m, "json") || strings.Contains(m, "xml") || strings.Contains(m, "html") || strings.Contains(m, "urlencoded")
}

func isPasswordField(n string) bool {
	n = strings.ToLower(n)
	return strings.Contains(n, "password") || n == "pass" || n == "pwd" || n == "passwd"
}

func maskJSONPasswords(body string, sc *model.Scenario, rep *model.CompatReport, where string) string {
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) != nil {
		return body
	}
	changed := false
	for k, val := range v {
		if _, ok := val.(string); ok && isPasswordField(k) {
			v[k] = "${password}"
			changed = true
		}
	}
	if !changed {
		return body
	}
	sc.Variables["password"] = ""
	rep.Manual = append(rep.Manual, fmt.Sprintf("JSON password field in %s now uses ${password}; provide test credentials.", where))
	b, _ := json.Marshal(v)
	return string(b)
}

// orderedForm returns form keys in their original order (url.Values loses order).
func orderedForm(s string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, "&") {
		k, _, _ := strings.Cut(part, "=")
		k, err := url.QueryUnescape(k)
		if err != nil || seen[k] || k == "" {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

func sanitize(s string) string {
	s = strings.Trim(s, "/ ")
	if s == "" {
		s = "root"
	}
	b := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	if len(b) > 40 {
		b = b[:40]
	}
	return b
}

// RedactedSummary is a safe one-line description for logs.
func RedactedSummary(rec *correlate.Recording) string {
	return fmt.Sprintf("%d exchanges from %s", len(rec.Exchanges), redact.Text(path.Base(rec.Source)))
}
