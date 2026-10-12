package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lelanga/phoenix/internal/safety"
)

// TokenPath is where the HTTP transport's bearer token lives.
func TokenPath() string { return filepath.Join(safety.ConfigDir(), "mcp-token") }

// EnsureToken returns the bearer token, creating it (0600) on first use.
func EnsureToken() (string, error) {
	p := TokenPath()
	if b, err := os.ReadFile(p); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw)
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	_ = os.Chmod(p, 0o600)
	return t, nil
}

// ValidateLoopbackAddr rejects listen addresses that are not 127.0.0.1/::1/localhost.
func ValidateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if host == "" || !safety.IsLoopback(host) {
		return fmt.Errorf("the MCP HTTP transport only listens on loopback (127.0.0.1), got %q", host)
	}
	return nil
}

func hostOnly(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		return hh
	}
	return strings.Trim(h, "[]")
}

type httpTransport struct {
	s        *Server
	token    string
	mu       sync.Mutex
	sessions map[string]bool
}

// HTTPHandler serves the MCP Streamable HTTP transport on POST /mcp. Callers
// must bind it to a loopback listener only.
func (s *Server) HTTPHandler(token string) http.Handler {
	t := &httpTransport{s: s, token: token, sessions: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", t.serve)
	return mux
}

func (t *httpTransport) serve(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding protection: Host and Origin must be loopback.
	if !safety.IsLoopback(hostOnly(r.Host)) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !safety.IsLoopback(u.Hostname()) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(got), []byte(t.token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="phoenix-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete: // client ends its session
		t.mu.Lock()
		delete(t.sessions, r.Header.Get("Mcp-Session-Id"))
		t.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed (no server-initiated stream)", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if b := strings.TrimSpace(string(body)); strings.HasPrefix(b, "[") {
		writeJSON(w, http.StatusBadRequest, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32600, "JSON-RPC batching is not supported"}})
		return
	}
	var req rpcReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if sid != "" && req.Method != "initialize" {
		t.mu.Lock()
		ok := t.sessions[sid]
		t.mu.Unlock()
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	}
	if len(req.ID) == 0 { // notification or client response
		if req.Method == "notifications/cancelled" {
			t.s.cancelRequest(req.Params)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "initialize" {
		raw := make([]byte, 16)
		_, _ = rand.Read(raw)
		sid = hex.EncodeToString(raw)
		t.mu.Lock()
		t.sessions[sid] = true
		t.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
	}

	accept := r.Header.Get("Accept")
	wantSSE := strings.Contains(accept, "text/event-stream")
	hasJSON := strings.Contains(accept, "application/json")
	var meta struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(req.Params, &meta)
	// SSE when the client only accepts it, or when it accepts both and asked for progress.
	sse := wantSSE && (!hasJSON || len(meta.Meta.ProgressToken) > 0)

	ctx := r.Context() // client disconnect cancels the request
	var notify notifier
	var wmu sync.Mutex
	flusher, _ := w.(http.Flusher)
	writeEvent := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		notify = func(method string, params any) {
			wmu.Lock()
			defer wmu.Unlock()
			writeEvent(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		}
	}
	resp, ok := t.s.process(ctx, req, notify)
	if !ok {
		if !sse {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	if sse {
		wmu.Lock()
		writeEvent(resp)
		wmu.Unlock()
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ServeHTTP listens on addr (loopback only) and serves until ctx ends.
func (s *Server) ServeHTTP(ctx context.Context, addr, token string, ready func(net.Addr)) error {
	if err := ValidateLoopbackAddr(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(ln.Addr())
	}
	hs := &http.Server{Handler: s.HTTPHandler(token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = hs.Shutdown(sc)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
