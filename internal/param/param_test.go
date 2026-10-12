package param

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lelanga/phoenix/internal/model"
)

func TestDetectApply(t *testing.T) {
	sc := model.NewScenario("p")
	sc.Steps = []model.Step{
		{Type: model.StepRequest, Name: "login", Request: &model.Request{Method: "POST", URL: "/login",
			Form: []model.KV{{Name: "username", Value: "alice"}, {Name: "password", Value: "${password}"}}}},
		{Type: model.StepRequest, Name: "search", Request: &model.Request{Method: "GET", URL: "/api/items?q=laptop"}},
		{Type: model.StepRequest, Name: "order", Request: &model.Request{Method: "POST", URL: "/api/orders", Body: `{"item":"SKU-001","qty":1}`}},
	}
	s := Detect(sc)
	got := map[string]string{}
	ids := map[string]bool{}
	for _, x := range s {
		got[x.Column] = x.Kind
		ids[x.ID] = true
	}
	if got["username"] != "username" || got["q"] != "search" || got["item"] != "product" {
		t.Fatalf("unexpected suggestions: %v", got)
	}
	if _, ok := got["password"]; ok {
		t.Error("already-parameterized password must be skipped")
	}
	dir := t.TempDir()
	if _, err := Apply(sc, s, ids, dir, "p-params.csv", 10); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "p-params.csv"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 11 || !strings.Contains(lines[1], "perfuser0001") {
		t.Fatalf("bad csv: %q", lines[:2])
	}
	if sc.Steps[0].Request.Form[0].Value != "${username}" || !strings.Contains(sc.Steps[1].Request.URL, "${q}") || !strings.Contains(sc.Steps[2].Request.Body, "${item}") {
		t.Fatalf("not rewritten: %+v %s %s", sc.Steps[0].Request.Form, sc.Steps[1].Request.URL, sc.Steps[2].Request.Body)
	}
	if len(sc.DataSources) != 1 || sc.DataSources[0].Mode != "unique" {
		t.Fatalf("data source: %+v", sc.DataSources)
	}
}
