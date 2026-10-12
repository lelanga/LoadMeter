package har

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Recorder is an http.RoundTripper that captures traffic as HAR 1.2. It backs
// the recording proxy and is used to produce realistic test fixtures.
type Recorder struct {
	Next    http.RoundTripper
	MaxBody int64
	mu      sync.Mutex
	entries []map[string]any
	page    string
	pages   []map[string]any
}

func NewRecorder(next http.RoundTripper) *Recorder {
	if next == nil {
		next = http.DefaultTransport
	}
	return &Recorder{Next: next, MaxBody: 2 << 20}
}

// Page starts a new HAR page (used as a transaction boundary on import).
func (r *Recorder) Page(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.page = "page_" + itoa(len(r.pages)+1)
	r.pages = append(r.pages, map[string]any{"id": r.page, "title": title, "startedDateTime": time.Now().UTC().Format(time.RFC3339Nano), "pageTimings": map[string]any{}})
}

func itoa(n int) string { return strconv.Itoa(n) }

func nvs(h http.Header) []map[string]string {
	out := []map[string]string{}
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, map[string]string{"name": k, "value": v})
		}
	}
	return out
}

func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(io.LimitReader(req.Body, r.MaxBody))
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := r.Next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, r.MaxBody))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	qs := []map[string]string{}
	for k, vs := range req.URL.Query() {
		for _, v := range vs {
			qs = append(qs, map[string]string{"name": k, "value": v})
		}
	}
	hdr := req.Header.Clone()
	if req.Host != "" {
		hdr.Set("Host", req.Host)
	}
	e := map[string]any{
		"startedDateTime": start.UTC().Format(time.RFC3339Nano), "time": elapsed,
		"request": map[string]any{"method": req.Method, "url": req.URL.String(), "httpVersion": "HTTP/1.1", "headers": nvs(hdr),
			"queryString": qs, "cookies": []any{}, "headersSize": -1, "bodySize": len(reqBody)},
		"response": map[string]any{"status": resp.StatusCode, "statusText": http.StatusText(resp.StatusCode), "httpVersion": "HTTP/1.1",
			"headers": nvs(resp.Header), "cookies": []any{}, "redirectURL": resp.Header.Get("Location"), "headersSize": -1, "bodySize": len(body),
			"content": map[string]any{"size": len(body), "mimeType": resp.Header.Get("Content-Type"), "text": string(body)}},
		"cache": map[string]any{}, "timings": map[string]any{"send": 0, "wait": elapsed, "receive": 0},
	}
	if len(reqBody) > 0 {
		e["request"].(map[string]any)["postData"] = map[string]any{"mimeType": req.Header.Get("Content-Type"), "text": string(reqBody)}
	}
	r.mu.Lock()
	if r.page != "" {
		e["pageref"] = r.page
	}
	r.entries = append(r.entries, e)
	r.mu.Unlock()
	return resp, nil
}

// HAR returns the captured traffic as a HAR document.
func (r *Recorder) HAR() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	doc := map[string]any{"log": map[string]any{"version": "1.2", "creator": map[string]string{"name": "Phoenix Recorder", "version": "0.1"},
		"pages": r.pages, "entries": r.entries}}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return b
}

func (r *Recorder) Save(path string) error { return os.WriteFile(path, r.HAR(), 0o600) }

// Proxy returns an HTTP forward-proxy handler that records plain-HTTP traffic.
// HTTPS (CONNECT) is refused: intercepting TLS needs a trusted local CA, which
// Phoenix does not install. Use the browser's "Save all as HAR" for HTTPS.
func (r *Recorder) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect {
			http.Error(w, "HTTPS recording through the proxy is not supported; export a HAR from your browser's developer tools instead", http.StatusNotImplemented)
			return
		}
		if !req.URL.IsAbs() {
			http.Error(w, "configure this address as your HTTP proxy (absolute request URIs expected)", http.StatusBadRequest)
			return
		}
		out := req.Clone(req.Context())
		out.RequestURI = ""
		for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
			out.Header.Del(h)
		}
		resp, err := r.RoundTrip(out)
		if err != nil {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}
