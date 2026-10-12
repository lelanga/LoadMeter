package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/service"
	"github.com/lelanga/loadmeter/internal/summary"
)

// Failure triage and grounded run summaries. Auth and CSRF checks come from
// the /api/ wrapper in Handler.
func init() {
	extraRoutes = append(extraRoutes, func(s *Server, m *http.ServeMux) {
		svc := s.Svc
		m.HandleFunc("GET /api/runs/{id}/triage", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !app.ValidRunID(id) {
				writeErr(w, fmt.Errorf("%w: invalid run id", service.ErrInvalid))
				return
			}
			top := 10
			if v := r.URL.Query().Get("top"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 || n > 1000 {
					writeErr(w, fmt.Errorf("%w: top must be 0..1000", service.ErrInvalid))
					return
				}
				top = n
			}
			_, tri, err := summary.ForRun(svc, id, "")
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, tri.Top(top))
		})
		// ?baseline=<runId> adds a comparison; ?ai=1 asks the configured AI to
		// reword the text (still number-verified; falls back to the template).
		m.HandleFunc("GET /api/runs/{id}/summary", func(w http.ResponseWriter, r *http.Request) {
			id, base := r.PathValue("id"), r.URL.Query().Get("baseline")
			if !app.ValidRunID(id) || base != "" && !app.ValidRunID(base) {
				writeErr(w, fmt.Errorf("%w: invalid run id", service.ErrInvalid))
				return
			}
			facts, _, err := summary.ForRun(svc, id, base)
			if err != nil {
				writeErr(w, err)
				return
			}
			var client *ai.Client
			useAI := r.URL.Query().Get("ai") == "1"
			if useAI {
				cfg, err := ai.LoadConfig()
				if err != nil {
					writeErr(w, err)
					return
				}
				client = ai.NewClient(cfg)
			}
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			writeJSON(w, http.StatusOK, summary.Summarize(ctx, client, facts, useAI))
		})
	})
}
