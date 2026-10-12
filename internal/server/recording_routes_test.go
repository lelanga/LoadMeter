package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecordingRoutes(t *testing.T) {
	s, h := newTestServer(t)
	call := func(method, target, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = "127.0.0.1:7878"
		r.Header.Set("X-Phoenix-Token", s.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}
	if code, m := call("GET", "/api/recordings/active", ""); code != 200 || m["active"] != false {
		t.Fatalf("idle active: %d %v", code, m)
	}
	code, m := call("POST", "/api/recordings", `{"browser":false,"includeHosts":["127.0.0.1"],"insecureUpstream":true}`)
	if code != http.StatusCreated {
		t.Fatalf("start: %d %v", code, m)
	}
	id, _ := m["id"].(string)
	if code, _ := call("POST", "/api/recordings", `{}`); code != http.StatusConflict {
		t.Errorf("second start: %d, want 409", code)
	}
	if code, m := call("POST", "/api/recordings/"+id+"/transaction", `{"name":"Login"}`); code != 200 || m["transaction"] != "Login" {
		t.Errorf("transaction: %d %v", code, m)
	}
	if code, m := call("GET", "/api/recordings/active?since=0", ""); code != 200 || m["active"] != true {
		t.Errorf("active: %d %v", code, m)
	}
	if code, _ := call("POST", "/api/recordings/nope/stop", `{}`); code != http.StatusNotFound {
		t.Errorf("stop unknown: %d", code)
	}
	if code, m := call("POST", "/api/recordings/"+id+"/stop", `{"toScenario":"x"}`); code != http.StatusBadRequest || m["result"] == nil {
		t.Errorf("stop with nothing captured: %d %v", code, m)
	}
	if code, m := call("GET", "/api/recordings/active", ""); m["active"] != false {
		t.Errorf("after stop: %d %v", code, m)
	}
}
