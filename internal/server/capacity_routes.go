package server

import (
	"net/http"

	"github.com/lelanga/loadmeter/internal/service"
)

// Capacity finder API (see service/capacity.go). Authentication and CSRF
// checks are applied by the /api/ wrapper in Handler.
func init() {
	extraRoutes = append(extraRoutes, func(s *Server, m *http.ServeMux) {
		svc := s.Svc
		m.HandleFunc("POST /api/capacity", func(w http.ResponseWriter, r *http.Request) {
			var req service.CapacityRequest
			if err := decode(r, &req); err != nil {
				writeErr(w, err)
				return
			}
			info, err := svc.StartCapacity(req)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, info)
		})
		m.HandleFunc("GET /api/capacity/active", func(w http.ResponseWriter, r *http.Request) {
			info := svc.ActiveCapacity()
			if info == nil {
				writeJSON(w, http.StatusOK, map[string]any{"active": false})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"active": info.State == "running", "capacity": info})
		})
		m.HandleFunc("POST /api/capacity/stop", func(w http.ResponseWriter, r *http.Request) {
			if err := svc.StopCapacity(); err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
		})
		m.HandleFunc("GET /api/capacity", func(w http.ResponseWriter, r *http.Request) {
			ids, err := svc.ListCapacity()
			if err != nil {
				writeErr(w, err)
				return
			}
			if ids == nil {
				ids = []string{}
			}
			writeJSON(w, http.StatusOK, ids)
		})
		m.HandleFunc("GET /api/capacity/{id}", func(w http.ResponseWriter, r *http.Request) {
			res, err := svc.CapacityResult(r.PathValue("id"))
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, res)
		})
	})
}
