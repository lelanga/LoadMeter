package server

import (
	"net/http"
	"strconv"

	"github.com/lelanga/phoenix/internal/service"
)

// Script recording API (see service/recording.go). Authentication and CSRF
// checks are applied by the /api/ wrapper in Handler.
func init() {
	extraRoutes = append(extraRoutes, func(s *Server, m *http.ServeMux) {
		svc := s.Svc
		m.HandleFunc("POST /api/recordings", func(w http.ResponseWriter, r *http.Request) {
			var o service.RecordingOptions
			if err := decode(r, &o); err != nil {
				writeErr(w, err)
				return
			}
			o.InsecureUpstream = false // CLI-only option (audited there)
			info, err := svc.StartRecording(o)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, info)
		})
		m.HandleFunc("GET /api/recordings/active", func(w http.ResponseWriter, r *http.Request) {
			since, _ := strconv.Atoi(r.URL.Query().Get("since"))
			info := svc.ActiveRecording(since)
			if info == nil {
				writeJSON(w, http.StatusOK, map[string]any{"active": false})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"active": true, "recording": info})
		})
		m.HandleFunc("POST /api/recordings/{id}/transaction", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name string `json:"name"`
			}
			if err := decode(r, &body); err != nil {
				writeErr(w, err)
				return
			}
			info, err := svc.MarkTransaction(r.PathValue("id"), body.Name)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, info)
		})
		m.HandleFunc("POST /api/recordings/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ToScenario string `json:"toScenario"`
			}
			if err := decode(r, &body); err != nil {
				writeErr(w, err)
				return
			}
			res, err := svc.StopRecording(r.PathValue("id"), body.ToScenario)
			if err != nil {
				if res != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "result": res})
					return
				}
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, res)
		})
	})
}
