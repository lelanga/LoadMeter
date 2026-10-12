// Package server is the local web UI + JSON API (loopback only, token-protected).
package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/assistant"
	"github.com/lelanga/loadmeter/internal/har"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/safety"
	"github.com/lelanga/loadmeter/internal/service"
	"github.com/lelanga/loadmeter/web"
)

type Server struct {
	Svc   *service.Service
	Token string
}

func New(svc *service.Service) *Server {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return &Server{Svc: svc, Token: hex.EncodeToString(b)}
}

const maxUpload = 256 << 20

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	body := map[string]any{"error": err.Error()}
	var hna *safety.ErrHostNotAllowed
	var nc *app.ErrNeedsConfirmation
	switch {
	case errors.Is(err, service.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, service.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, service.ErrBusy):
		code = http.StatusConflict
	case errors.As(err, &hna):
		code = http.StatusForbidden
		body["blockedHost"] = hna.Host
	case errors.As(err, &nc):
		code = http.StatusConflict
		body["needsConfirmation"] = true
	case errors.Is(err, ai.ErrDisabled):
		code = http.StatusPreconditionFailed
	}
	writeJSON(w, code, body)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return fmt.Errorf("%w: invalid JSON body: %v", service.ErrInvalid, err)
	}
	return nil
}

// hostOK blocks DNS-rebinding: only loopback Host headers are served.
func hostOK(r *http.Request) bool {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return safety.IsLoopback(h)
}

func (s *Server) authed(r *http.Request) bool {
	tok := r.Header.Get("X-LoadMeter-Token")
	if tok == "" {
		if c, err := r.Cookie("loadmeter_session"); err == nil {
			tok = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(web.Static, "static")
	files := http.FileServer(http.FS(static))
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if t := r.URL.Query().Get("token"); t != "" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) != 1 {
				http.Error(w, "invalid token: use the URL printed by 'loadmeter serve'", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "loadmeter_session", Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if r.URL.Path == "/" && !s.authed(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Load Meter</title><body style="font-family:system-ui;padding:40px"><h1>Load Meter</h1><p>Open the link printed in the terminal by <code>loadmeter serve</code> (it contains a one-time access token).</p></body>`)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	api := http.NewServeMux()
	s.routes(api)
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid session token"})
			return
		}
		// CSRF: state-changing requests need the header token (cookies alone are not enough).
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-LoadMeter-Token")), []byte(s.Token)) != 1 {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "X-LoadMeter-Token header required"})
				return
			}
		}
		api.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostOK(r) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/api/runs/") || !strings.HasSuffix(r.URL.Path, "/report") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		} else {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'self'")
		}
		mux.ServeHTTP(w, r)
	})
}

// extraRoutes lets other files in this package add API routes (call from init).
var extraRoutes []func(s *Server, m *http.ServeMux)

func (s *Server) routes(m *http.ServeMux) {
	svc := s.Svc
	for _, add := range extraRoutes {
		add(s, m)
	}
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		abs, _ := filepath.Abs(svc.WS.Dir)
		writeJSON(w, 200, map[string]string{"token": s.Token, "version": results.Version, "workspace": abs})
	})

	// Scenarios
	m.HandleFunc("GET /api/scenarios", func(w http.ResponseWriter, r *http.Request) {
		l, err := svc.ListScenarios()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, l)
	})
	m.HandleFunc("GET /api/scenarios/{file}", func(w http.ResponseWriter, r *http.Request) {
		sc, err := svc.GetScenario(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, sc)
	})
	m.HandleFunc("PUT /api/scenarios/{file}", func(w http.ResponseWriter, r *http.Request) {
		var sc = struct{ *json.RawMessage }{}
		_ = sc
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			writeErr(w, err)
			return
		}
		tmp := filepath.Join(os.TempDir(), "loadmeter-put-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".json")
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			writeErr(w, err)
			return
		}
		defer os.Remove(tmp)
		parsed, err := app.LoadScenario(tmp)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: %v", service.ErrInvalid, strings.ReplaceAll(err.Error(), tmp+": ", "")))
			return
		}
		probs, err := svc.SaveScenario(r.PathValue("file"), parsed, "edit")
		if err != nil {
			writeErr(w, err)
			return
		}
		if probs == nil {
			probs = []string{}
		}
		writeJSON(w, 200, map[string]any{"file": r.PathValue("file"), "problems": probs})
	})
	m.HandleFunc("DELETE /api/scenarios/{file}", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.DeleteScenario(r.PathValue("file")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	})
	m.HandleFunc("POST /api/scenarios/{file}/validate", func(w http.ResponseWriter, r *http.Request) {
		sc, err := svc.GetScenario(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		pol := svc.Policy()
		hosts := app.TargetHosts(sc)
		blocked := []string{}
		for _, h := range hosts {
			if !pol.HostAllowed(h) {
				blocked = append(blocked, h)
			}
		}
		probs := sc.Validate()
		if probs == nil {
			probs = []string{}
		}
		writeJSON(w, 200, map[string]any{"problems": probs, "targets": hosts, "blocked": blocked})
	})
	m.HandleFunc("POST /api/scenarios/{file}/debug", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Vars map[string]string `json:"vars"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		exs, err := svc.Debug(r.Context(), r.PathValue("file"), body.Vars)
		if err != nil && exs == nil {
			writeErr(w, err)
			return
		}
		ok := err == nil
		for _, e := range exs {
			ok = ok && e.OK
		}
		writeJSON(w, 200, map[string]any{"exchanges": exs, "ok": ok})
	})
	m.HandleFunc("GET /api/scenarios/{file}/history", func(w http.ResponseWriter, r *http.Request) {
		h, err := svc.History(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, h)
	})
	m.HandleFunc("POST /api/scenarios/{file}/history/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.Restore(r.PathValue("file"), r.PathValue("id")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"restored": true})
	})
	m.HandleFunc("GET /api/scenarios/{file}/diff/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := svc.Diff(r.PathValue("file"), r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		if d == nil {
			d = []service.Change{}
		}
		writeJSON(w, 200, map[string]any{"changes": d})
	})

	// Correlation / parameters
	m.HandleFunc("POST /api/scenarios/{file}/correlate", func(w http.ResponseWriter, r *http.Request) {
		res, err := svc.Correlate(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("POST /api/scenarios/{file}/correlate/apply", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			IDs      []string `json:"ids"`
			Validate *bool    `json:"validate"`
			Policy   string   `json:"policy"`
		}
		if err := decode(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		if b.Policy == "" {
			b.Policy = "validated"
			if b.Validate != nil && !*b.Validate {
				b.Policy = "manual"
			}
		}
		res, err := svc.ApplyCorrelations(r.Context(), r.PathValue("file"), b.IDs, b.Policy)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("POST /api/scenarios/{file}/parameterize", func(w http.ResponseWriter, r *http.Request) {
		s, err := svc.Parameterize(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"suggestions": s})
	})
	m.HandleFunc("POST /api/scenarios/{file}/parameterize/apply", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			IDs  []string `json:"ids"`
			Rows int      `json:"rows"`
		}
		if err := decode(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		ids, data, err := svc.ApplyParameters(r.PathValue("file"), b.IDs, b.Rows)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"applied": ids, "dataFile": data})
	})

	// Imports / exports
	readUpload := func(r *http.Request) (map[string][]byte, map[string]string, error) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, nil, fmt.Errorf("%w: multipart upload expected: %v", service.ErrInvalid, err)
		}
		files := map[string][]byte{}
		var total int64
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				total += fh.Size
				if total > maxUpload {
					return nil, nil, fmt.Errorf("%w: upload too large", service.ErrInvalid)
				}
				f, err := fh.Open()
				if err != nil {
					return nil, nil, err
				}
				b, err := io.ReadAll(io.LimitReader(f, maxUpload))
				f.Close()
				if err != nil {
					return nil, nil, err
				}
				files[fh.Filename] = b
			}
		}
		vals := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				vals[k] = v[0]
			}
		}
		if len(files) == 0 {
			return nil, nil, fmt.Errorf("%w: no file uploaded", service.ErrInvalid)
		}
		return files, vals, nil
	}
	first := func(files map[string][]byte) (string, []byte) {
		for k, v := range files {
			return k, v
		}
		return "", nil
	}
	nameOr := func(v map[string]string, fn string) string {
		if n := strings.TrimSpace(v["name"]); n != "" {
			return n
		}
		return strings.TrimSuffix(filepath.Base(fn), filepath.Ext(fn))
	}
	m.HandleFunc("POST /api/import/har", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		files, vals, err := readUpload(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		fn, data := first(files)
		res, err := svc.ImportHAR(nameOr(vals, fn), data, har.Options{IncludeStatic: vals["includeStatic"] == "true", KeepCredentials: vals["keepCredentials"] == "true"})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("POST /api/import/jmx", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		files, vals, err := readUpload(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		fn, data := first(files)
		res, err := svc.ImportJMX(nameOr(vals, fn), data)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("POST /api/import/loadrunner", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		files, vals, err := readUpload(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := vals["name"]
		if name == "" {
			name = "loadrunner-import"
			for k := range files {
				if strings.HasSuffix(strings.ToLower(k), ".usr") {
					name = strings.TrimSuffix(filepath.Base(k), filepath.Ext(k))
				}
			}
		}
		res, err := svc.ImportLoadRunner(name, files)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("GET /api/export/jmx/{file}", func(w http.ResponseWriter, r *http.Request) {
		b, rep, err := svc.ExportJMX(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		c := rep.Counts()
		w.Header().Set("X-Load Meter-Compat", fmt.Sprintf("supported=%d partial=%d unsupported=%d confidence=%s", c["supported"], c["partial"], c["unsupported"], rep.Confidence))
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.TrimSuffix(r.PathValue("file"), ".json")+`.jmx"`)
		w.Write(b)
	})
	m.HandleFunc("GET /api/export/jmx/{file}/report", func(w http.ResponseWriter, r *http.Request) {
		_, rep, err := svc.ExportJMX(r.PathValue("file"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, rep)
	})

	// Runs
	m.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var req service.RunRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		id, err := svc.StartRun(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"runId": id})
	})
	m.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		l, err := svc.ListRuns()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, l)
	})
	m.HandleFunc("GET /api/runs/active", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, svc.ActiveRun()) })
	m.HandleFunc("POST /api/runs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.StopRun(r.PathValue("id")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"stopping": true})
	})
	m.HandleFunc("GET /api/runs/{id}/live", s.live)
	m.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if a := svc.Active(id); a != nil {
			select {
			case <-a.Run.Done():
			default:
				writeJSON(w, 200, map[string]any{"runId": id, "status": "running", "metrics": a.Run.Col.Snapshot(time.Now(), 0)})
				return
			}
		}
		sum, err := svc.Summary(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, sum)
	})
	serveRunFile := func(name, ctype string, download bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			d, err := svc.RunDir(r.PathValue("id"))
			if err != nil {
				writeErr(w, err)
				return
			}
			p := filepath.Join(d, name)
			if _, err := os.Stat(p); err != nil {
				writeErr(w, fmt.Errorf("%w: %s not available (run still in progress?)", service.ErrNotFound, name))
				return
			}
			w.Header().Set("Content-Type", ctype)
			if download {
				w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("id")+"-"+name+`"`)
			}
			http.ServeFile(w, r, p)
		}
	}
	m.HandleFunc("GET /api/runs/{id}/report", serveRunFile("report.html", "text/html; charset=utf-8", false))
	m.HandleFunc("GET /api/runs/{id}/raw.csv", serveRunFile("raw.csv", "text/csv", true))
	m.HandleFunc("GET /api/runs/{id}/summary.csv", serveRunFile("summary.csv", "text/csv", true))
	m.HandleFunc("GET /api/runs/{id}/summary.json", serveRunFile("summary.json", "application/json", true))
	m.HandleFunc("GET /api/compare", func(w http.ResponseWriter, r *http.Request) {
		c, err := svc.Compare(r.URL.Query().Get("base"), r.URL.Query().Get("run"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	m.HandleFunc("POST /api/demo", func(w http.ResponseWriter, r *http.Request) {
		f, u, err := svc.Demo()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"file": f, "mockUrl": u})
	})

	// Settings
	m.HandleFunc("GET /api/settings/policy", func(w http.ResponseWriter, r *http.Request) {
		p, err := safety.LoadPolicy()
		if err != nil {
			writeErr(w, err)
			return
		}
		if p.AllowedHosts == nil {
			p.AllowedHosts = []string{}
		}
		writeJSON(w, 200, p)
	})
	m.HandleFunc("PUT /api/settings/policy", func(w http.ResponseWriter, r *http.Request) {
		var p safety.Policy
		if err := decode(r, &p); err != nil {
			writeErr(w, err)
			return
		}
		if p.MaxVUs < 1 || p.MaxVUs > 100000 || p.MaxRPS < 0 {
			writeErr(w, fmt.Errorf("%w: maxVUs must be 1..100000 and maxRPS >= 0", service.ErrInvalid))
			return
		}
		for _, h := range p.AllowedHosts {
			if h == "*" || strings.ContainsAny(h, "/ ") {
				writeErr(w, fmt.Errorf("%w: invalid allowlist entry %q (use a host name or *.domain)", service.ErrInvalid, h))
				return
			}
		}
		if err := safety.SavePolicy(p); err != nil {
			writeErr(w, err)
			return
		}
		safety.Audit("policy.changed", map[string]any{"allowedHosts": p.AllowedHosts, "maxVUs": p.MaxVUs, "maxRPS": p.MaxRPS})
		writeJSON(w, 200, p)
	})
	m.HandleFunc("GET /api/settings/ai", func(w http.ResponseWriter, r *http.Request) {
		c, err := ai.LoadConfig()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, aiView(c))
	})
	m.HandleFunc("PUT /api/settings/ai", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ai.Config
			Providers []struct {
				ai.ProviderConfig
				APIKey *string `json:"apiKey"`
			} `json:"providers"`
		}
		if err := decode(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		c := in.Config
		c.Providers = nil
		for _, p := range in.Providers {
			switch p.Kind {
			case "anthropic", "openai", "openrouter", "gemini", "ollama":
			default:
				writeErr(w, fmt.Errorf("%w: unknown provider kind %q", service.ErrInvalid, p.Kind))
				return
			}
			if p.ID == "" || p.Model == "" {
				writeErr(w, fmt.Errorf("%w: each provider needs an id and a model", service.ErrInvalid))
				return
			}
			if p.APIKey != nil {
				if err := ai.SetKey(p.ID, strings.TrimSpace(*p.APIKey)); err != nil {
					writeErr(w, fmt.Errorf("storing key for %s: %w", p.ID, err))
					return
				}
			}
			c.Providers = append(c.Providers, p.ProviderConfig)
		}
		if err := ai.SaveConfig(c); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, aiView(c))
	})
	m.HandleFunc("POST /api/ai/test", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Provider string }
		if err := decode(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		c, err := ai.LoadConfig()
		if err != nil {
			writeErr(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		resp, err := ai.NewClient(c).Test(ctx, b.Provider)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "model": resp.Model, "latencyMs": resp.Latency.Milliseconds(), "reply": resp.Text})
	})
	m.HandleFunc("POST /api/ai/ask", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Prompt  string `json:"prompt"`
			File    string `json:"file"`
			Confirm bool   `json:"confirm"`
		}
		if err := decode(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		if strings.TrimSpace(b.Prompt) == "" || len(b.Prompt) > 8000 {
			writeErr(w, fmt.Errorf("%w: prompt must be 1..8000 characters", service.ErrInvalid))
			return
		}
		c, _ := ai.LoadConfig()
		res, err := assistant.Ask(r.Context(), svc, ai.NewClient(c), b.Prompt, b.File, b.Confirm)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("POST /api/ai/explain", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			RunID string `json:"runId"`
		}
		if err := decode(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		c, _ := ai.LoadConfig()
		resp, err := assistant.Explain(r.Context(), svc, ai.NewClient(c), b.RunID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"text": resp.Text, "provider": resp.Provider, "model": resp.Model, "sentBytes": resp.SentBytes})
	})
	m.HandleFunc("GET /api/ai/activity", func(w http.ResponseWriter, r *http.Request) {
		a := ai.RecentActivity(100)
		if a == nil {
			a = []ai.Activity{}
		}
		writeJSON(w, 200, a)
	})
	m.HandleFunc("GET /api/audit", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(filepath.Join(safety.ConfigDir(), "audit.log"))
		out := []json.RawMessage{}
		if err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				out = append(out, json.RawMessage(append([]byte(nil), sc.Bytes()...)))
			}
		}
		if len(out) > 200 {
			out = out[len(out)-200:]
		}
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		writeJSON(w, 200, out)
	})
}

type providerView struct {
	ai.ProviderConfig
	HasKey bool `json:"hasKey"`
	Local  bool `json:"local"`
}

func aiView(c ai.Config) map[string]any {
	ps := []providerView{}
	for _, p := range c.Providers {
		ps = append(ps, providerView{p, p.HasKey(), p.IsLocal()})
	}
	return map[string]any{"enabled": c.Enabled, "cloudConsent": c.CloudConsent, "localOnly": c.LocalOnly, "providers": ps,
		"routing": c.Routing, "limits": c.Limits, "keyStorage": ai.KeyBackend()}
}

// live streams Server-Sent Events: a snapshot each second, then "done".
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a := s.Svc.Active(id)
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	if a == nil {
		sum, err := s.Svc.Summary(id)
		if err != nil {
			send("error", map[string]string{"error": err.Error()})
			return
		}
		send("done", sum)
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.Run.Done():
			sum, err := a.Wait()
			if err != nil {
				send("error", map[string]string{"error": err.Error()})
				return
			}
			send("done", sum)
			return
		case now := <-t.C:
			snap := a.Run.Col.Snapshot(now, -1)
			if n := len(snap.Timeline); n > 60 {
				snap.Timeline = snap.Timeline[n-60:]
			}
			send("snapshot", map[string]any{"runId": id, "state": a.Run.State(), "plannedSec": a.Scenario.Workload.PlannedDuration().Seconds(), "metrics": snap})
		}
	}
}
