// Package mockserver is a deterministic HTTP target for tests, demos and
// benchmarks. Tokens are dynamic per session but derived from a seeded counter,
// so two runs with the same seed see the same sequence of values.
package mockserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type session struct {
	user     string
	token    string
	csrf     string
	requests int
}

type Server struct {
	seed      string
	counter   atomic.Uint64
	mu        sync.Mutex
	sessions  map[string]*session // by session cookie
	byToken   map[string]*session
	orders    map[string]string // orderId -> user
	rateMu    sync.Mutex
	rateSec   int64
	rateCount int
	flaky     atomic.Uint64
	// SessionLimit expires a session after this many authenticated requests (0 = never).
	SessionLimit int
	// RateLimit is the per-second request budget for /api/limited.
	RateLimit int
	Hits      atomic.Uint64
}

func New(seed string) *Server {
	return &Server{seed: seed, sessions: map[string]*session{}, byToken: map[string]*session{},
		orders: map[string]string{}, RateLimit: 50}
}

func (s *Server) next(prefix string) string {
	n := s.counter.Add(1)
	m := hmac.New(sha256.New, []byte(s.seed))
	fmt.Fprintf(m, "%s:%d", prefix, n)
	return hex.EncodeToString(m.Sum(nil))[:24]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) sessionFrom(r *http.Request) *session {
	c, err := r.Cookie("SESSIONID")
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[c.Value]
	if ss != nil {
		ss.requests++
		if s.SessionLimit > 0 && ss.requests > s.SessionLimit {
			delete(s.sessions, c.Value)
			return nil
		}
	}
	return ss
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	count := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { s.Hits.Add(1); h(w, r) }
	}
	mux.HandleFunc("GET /health", count(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("GET /", count(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Load Meter Mock Shop</title><script src="/static/app.js"></script></head><body><a href="/login-page">Login</a></body></html>`)
	}))
	mux.HandleFunc("GET /static/", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		fmt.Fprint(w, "/* static asset */")
	}))
	// HTML login form with hidden CSRF token (classic web correlation).
	mux.HandleFunc("GET /login-page", count(func(w http.ResponseWriter, r *http.Request) {
		tok := s.next("form")
		s.mu.Lock()
		s.byToken["form:"+tok] = &session{}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><form method="post" action="/login"><input type="hidden" name="__RequestVerificationToken" value="%s"/><input name="username"/><input name="password" type="password"/></form></body></html>`, tok)
	}))
	// Login: form (needs __RequestVerificationToken from /login-page) or JSON.
	mux.HandleFunc("POST /login", count(func(w http.ResponseWriter, r *http.Request) {
		var user, pass string
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var body struct{ Username, Password string }
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "malformed json"})
				return
			}
			user, pass = body.Username, body.Password
		} else {
			if err := r.ParseForm(); err != nil {
				writeJSON(w, 400, map[string]string{"error": "bad form"})
				return
			}
			user, pass = r.PostForm.Get("username"), r.PostForm.Get("password")
			ft := r.PostForm.Get("__RequestVerificationToken")
			s.mu.Lock()
			_, ok := s.byToken["form:"+ft]
			delete(s.byToken, "form:"+ft)
			s.mu.Unlock()
			if !ok {
				writeJSON(w, 403, map[string]string{"error": "invalid request verification token"})
				return
			}
		}
		if user == "" || pass != "secret" {
			writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
			return
		}
		ss := &session{user: user, token: s.next("token"), csrf: s.next("csrf")}
		sid := s.next("sid")
		s.mu.Lock()
		s.sessions[sid] = ss
		s.byToken[ss.token] = ss
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "SESSIONID", Value: sid, Path: "/", HttpOnly: true})
		w.Header().Set("X-CSRF-Token", ss.csrf)
		writeJSON(w, 200, map[string]any{"token": ss.token, "csrf": ss.csrf, "user": map[string]string{"name": user}})
	}))
	mux.HandleFunc("GET /dashboard", count(func(w http.ResponseWriter, r *http.Request) {
		ss := s.sessionFrom(r)
		if ss == nil {
			http.Redirect(w, r, "/login-page", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><h1>Welcome %s</h1><meta name="csrf-token" content="%s"></body></html>`, ss.user, ss.csrf)
	}))
	mux.HandleFunc("GET /api/profile", count(func(w http.ResponseWriter, r *http.Request) {
		ss := s.bearer(r)
		if ss == nil {
			writeJSON(w, 401, map[string]string{"error": "missing or invalid bearer token"})
			return
		}
		writeJSON(w, 200, map[string]any{"name": ss.user, "tier": "gold"})
	}))
	mux.HandleFunc("GET /api/items", count(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]any{}
		for i := 1; i <= 5; i++ {
			items = append(items, map[string]any{"id": fmt.Sprintf("SKU-%03d", i), "price": 10 * i})
		}
		writeJSON(w, 200, map[string]any{"items": items, "q": r.URL.Query().Get("q")})
	}))
	mux.HandleFunc("POST /api/orders", count(func(w http.ResponseWriter, r *http.Request) {
		ss := s.sessionFrom(r)
		if ss == nil {
			writeJSON(w, 401, map[string]string{"error": "session expired"})
			return
		}
		if r.Header.Get("X-CSRF-Token") != ss.csrf {
			writeJSON(w, 403, map[string]string{"error": "csrf token mismatch"})
			return
		}
		var body struct {
			Item string `json:"item"`
			Qty  int    `json:"qty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Item == "" {
			writeJSON(w, 400, map[string]string{"error": "item required"})
			return
		}
		id := "ORD-" + s.next("order")[:12]
		s.mu.Lock()
		s.orders[id] = ss.user
		s.mu.Unlock()
		writeJSON(w, 201, map[string]any{"orderId": id, "status": "created", "item": body.Item})
	}))
	mux.HandleFunc("GET /api/orders/{id}", count(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		u, ok := s.orders[r.PathValue("id")]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "order not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"orderId": r.PathValue("id"), "user": u, "status": "created"})
	}))
	mux.HandleFunc("GET /delay", count(func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		if ms < 0 || ms > 60000 {
			ms = 0
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		writeJSON(w, 200, map[string]int{"delayed": ms})
	}))
	mux.HandleFunc("GET /status/{code}", count(func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.PathValue("code"))
		if err != nil || code < 100 || code > 599 {
			code = 400
		}
		w.WriteHeader(code)
	}))
	mux.HandleFunc("GET /redirect", count(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			writeJSON(w, 200, map[string]string{"redirects": "done"})
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/redirect?n=%d", n-1), http.StatusFound)
	}))
	mux.HandleFunc("GET /hang", count(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(60 * time.Second):
		case <-r.Context().Done():
		}
	}))
	mux.HandleFunc("GET /malformed", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items": [1, 2,`)
	}))
	mux.HandleFunc("GET /api/limited", count(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		s.rateMu.Lock()
		if now != s.rateSec {
			s.rateSec, s.rateCount = now, 0
		}
		s.rateCount++
		over := s.rateCount > s.RateLimit
		s.rateMu.Unlock()
		if over {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, map[string]string{"error": "rate limited"})
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))
	// Every 10th call fails with 500 (deterministic).
	mux.HandleFunc("GET /flaky", count(func(w http.ResponseWriter, r *http.Request) {
		if s.flaky.Add(1)%10 == 0 {
			writeJSON(w, 500, map[string]string{"error": "internal server error"})
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))
	mux.HandleFunc("POST /upload", count(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, 400, map[string]string{"error": "multipart required"})
			return
		}
		files := map[string]int64{}
		for name, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				files[name] += fh.Size
			}
		}
		writeJSON(w, 200, map[string]any{"fields": r.MultipartForm.Value, "files": files})
	}))
	mux.HandleFunc("POST /soap", count(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if !strings.Contains(string(b), "Envelope") {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><EchoResponse><RequestId>%s</RequestId><Length>%d</Length></EchoResponse></soap:Body></soap:Envelope>`, s.next("soap")[:10], len(b))
	}))
	mux.HandleFunc("POST /echo", count(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		hdr := map[string]string{}
		for k := range r.Header {
			hdr[k] = r.Header.Get(k)
		}
		writeJSON(w, 200, map[string]any{"method": r.Method, "query": r.URL.RawQuery, "headers": hdr, "body": string(b)})
	}))
	return mux
}

func (s *Server) bearer(r *http.Request) *session {
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken[strings.TrimPrefix(a, "Bearer ")]
}
