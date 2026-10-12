package har

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/correlate"
	"github.com/lelanga/loadmeter/internal/mockserver"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

// RecordShopFlow drives the mock shop like a browser and returns a HAR.
func RecordShopFlow(t testing.TB, base string) []byte {
	t.Helper()
	rec := NewRecorder(http.DefaultTransport)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: rec, Jar: jar}
	get := func(path string, hdr map[string]string) []byte {
		req, _ := http.NewRequest("GET", base+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	rec.Page("Home")
	get("/", nil)
	get("/static/app.js", nil)
	rec.Page("Login")
	page := string(get("/login-page", nil))
	i := strings.Index(page, `value="`) + 7
	tok := page[i : i+strings.Index(page[i:], `"`)]
	form := "__RequestVerificationToken=" + tok + "&username=alice&password=secret"
	req, _ := http.NewRequest("POST", base+"/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var login struct{ Token, Csrf string }
	json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()
	rec.Page("Dashboard")
	get("/dashboard", nil)
	get("/api/items?q=SKU-001", nil)
	get("/api/profile", map[string]string{"Authorization": "Bearer " + login.Token})
	rec.Page("Order")
	req, _ = http.NewRequest("POST", base+"/api/orders", strings.NewReader(`{"item":"SKU-001","qty":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", login.Csrf)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var order struct{ OrderID string }
	json.NewDecoder(resp.Body).Decode(&order)
	resp.Body.Close()
	get("/api/orders/"+order.OrderID, nil)
	return rec.HAR()
}

func TestImportCorrelateValidate(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	srv := httptest.NewServer(mockserver.New("rec-1").Handler())
	defer srv.Close()
	harBytes := RecordShopFlow(t, srv.URL)
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		os.WriteFile(filepath.Join("..", "..", "testdata", "har", "shop.har"), harBytes, 0o644)
	}
	sc, rep, rec, err := Import(bytes.NewReader(harBytes), "shop.har", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if probs := sc.Validate(); len(probs) > 0 {
		t.Fatalf("invalid scenario: %v", probs)
	}
	// static asset excluded and reported (never silent)
	foundStatic := false
	for _, it := range rep.Items {
		if strings.Contains(it.Note, "static resource") {
			foundStatic = true
		}
	}
	if !foundStatic {
		t.Errorf("static exclusion not reported: %+v", rep.Items)
	}
	if n := len(sc.AllRequests()); n != 8 {
		t.Errorf("want 8 requests, got %d", n)
	}
	if sc.Variables["password"] != "" {
		t.Errorf("password should be blanked")
	}
	res := correlate.Detect(rec, nil)
	vars := map[string]correlate.Candidate{}
	for _, c := range res.Candidates {
		vars[c.Source.Location] = c
		t.Logf("candidate %s %.2f %s -> %d dests (%s)", c.Variable, c.Confidence, c.Source.Location, len(c.Destinations), c.Security)
	}
	for _, want := range []string{"html __RequestVerificationToken", "json $.token", "json $.orderId"} {
		if _, ok := vars[want]; !ok {
			t.Errorf("missing candidate from %s", want)
		}
	}
	csrfFound := false
	for loc := range vars {
		if loc == "json $.csrf" || loc == "header X-CSRF-Token" {
			csrfFound = true
		}
	}
	if !csrfFound {
		t.Error("missing csrf candidate")
	}

	// Replay against a fresh server instance: recorded tokens are now stale.
	srv2 := httptest.NewServer(mockserver.New("rec-2").Handler())
	defer srv2.Close()
	sc.Variables["baseUrl"] = srv2.URL
	sc.Variables["password"] = "secret"
	original := sc.Clone()
	ids := map[string]bool{}
	for _, c := range res.Candidates {
		ids[c.ID] = true
	}
	applied, notes := correlate.Apply(sc, res.Candidates, ids)
	if len(applied) != len(res.Candidates) {
		t.Errorf("applied %d of %d: %v", len(applied), len(res.Candidates), notes)
	}
	v := correlate.Validate(context.Background(), original, sc, res.Candidates, t.TempDir(), safety.DefaultPolicy())
	t.Log(v.Summary)
	if v.Original.Failures == 0 {
		t.Error("expected the uncorrelated replay to fail")
	}
	if !v.OK {
		b, _ := json.MarshalIndent(v, "", " ")
		t.Fatalf("validation failed: %s", b)
	}
	_ = model.SchemaVersion
}
