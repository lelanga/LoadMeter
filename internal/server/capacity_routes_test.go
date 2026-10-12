package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapacityRoutes(t *testing.T) {
	s, h := newTestServer(t)
	call := func(method, target, body string) (int, string) {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = "127.0.0.1:7878"
		r.Header.Set("X-LoadMeter-Token", s.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	code, body := call("GET", "/api/capacity/active", "")
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	if code != 200 || m["active"] != false {
		t.Fatalf("idle: %d %s", code, body)
	}
	if code, body := call("GET", "/api/capacity", ""); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _ := call("POST", "/api/capacity", `{"file":"missing.json","p95Ms":100}`); code != 404 {
		t.Fatalf("missing scenario: %d", code)
	}
	if code, _ := call("GET", "/api/capacity/..bad", ""); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := call("POST", "/api/capacity/stop", ""); code != 404 {
		t.Fatalf("stop idle: %d", code)
	}
}
