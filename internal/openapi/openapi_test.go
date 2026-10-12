package openapi

import (
	"os"
	"strings"
	"testing"

	"github.com/lelanga/phoenix/internal/model"
)

func load(t *testing.T, o Options) (*model.Scenario, *model.CompatReport) {
	t.Helper()
	f, err := os.Open("../../testdata/openapi/petstore-like.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc, rep, err := Import(f, "petstore-like.json", o)
	if err != nil {
		t.Fatal(err)
	}
	if p := sc.Validate(); len(p) > 0 {
		t.Fatalf("validate: %v", p)
	}
	return sc, rep
}

func find(sc *model.Scenario, name string) *model.Request {
	for _, st := range sc.AllRequests() {
		if st.Name == name {
			return st.Request
		}
	}
	return nil
}

func TestImportDefaults(t *testing.T) {
	sc, rep := load(t, Options{})
	if sc.Variables["baseUrl"] != "http://127.0.0.1:8099/v1" || sc.Defaults.BaseURL != "${baseUrl}" {
		t.Fatalf("base: %v", sc.Variables)
	}
	var tx []string
	for _, s := range sc.Steps {
		tx = append(tx, s.Name)
	}
	if strings.Join(tx, ",") != "pets,store,untagged" {
		t.Fatalf("transactions %v", tx)
	}
	if find(sc, "deletePet") != nil || find(sc, "legacyGetPet") != nil {
		t.Fatal("destructive/deprecated ops must be skipped by default")
	}
	list := find(sc, "listPets")
	if list.URL != "/pets?limit=5&status=available" {
		t.Fatalf("list url %q", list.URL)
	}
	if a := list.Assert[0]; a.Type != model.AsStatus || a.Value != "200" {
		t.Fatalf("assert %+v", a)
	}
	get := find(sc, "getPet")
	if get.URL != "/pets/${petId}" || sc.Variables["petId"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("path param: %q %v", get.URL, sc.Variables)
	}
	if model.HeaderValue(get.Headers, "Authorization") != "Bearer ${secret_bearerauth}" {
		t.Fatalf("headers %+v", get.Headers)
	}
	if v, ok := sc.Variables["secret_bearerauth"]; !ok || v != "" {
		t.Fatal("secret must be seeded empty")
	}
	create := find(sc, "createPet")
	if create.Assert[0].Value != "201" || model.HeaderValue(create.Headers, "Content-Type") != "application/json" {
		t.Fatalf("create %+v", create)
	}
	for _, want := range []string{`"id": "${__uuid()}"`, `"email": "user${__counter()}@example.com"`, `"born": "${__timestamp(`, `"name": "dogs"`, `"name": "string"`} {
		if !strings.Contains(create.Body, want) {
			t.Errorf("body missing %s:\n%s", want, create.Body)
		}
	}
	if strings.Contains(create.Body, "parent") {
		t.Errorf("cyclic property must be dropped:\n%s", create.Body)
	}
	order := find(sc, "placeOrder")
	if model.HeaderValue(order.Headers, "X-API-Key") != "${secret_apikeyauth}" || model.HeaderValue(order.Headers, "Authorization") != "" {
		t.Fatalf("order headers %+v", order.Headers)
	}
	h := find(sc, "health")
	if h.Assert[0].Value != "2xx" || len(h.Headers) != 0 {
		t.Fatalf("health %+v", h)
	}
	// report: one item per operation (8 ops) + servers
	ops := map[string]string{}
	for _, it := range rep.Items {
		if it.Element == "operation" {
			ops[it.Name] = it.Status
		}
	}
	if len(ops) != 9 {
		t.Fatalf("operation items: %v", ops)
	}
	if ops["deletePet"] != model.Ignored || ops["legacyGetPet"] != model.Ignored || ops["createPet"] != model.Partial || ops["getPet"] != model.Supported {
		t.Fatalf("statuses %v", ops)
	}
	if rep.Validated != "passed" || rep.Confidence == "" {
		t.Fatalf("report %+v", rep)
	}
	data, _ := os.ReadFile("../../testdata/openapi/petstore-like.json")
	if strings.Contains(string(data), "secret_") {
		t.Fatal("fixture sanity")
	}
}

func TestFilters(t *testing.T) {
	sc, _ := load(t, Options{Tags: []string{"store"}})
	if len(sc.AllRequests()) != 2 {
		t.Fatalf("tag filter: %d", len(sc.AllRequests()))
	}
	sc, _ = load(t, Options{IncludeDestructive: true, IncludeDeprecated: true, OperationIDs: []string{"deletePet", "legacyGetPet"}})
	if len(sc.AllRequests()) != 2 || find(sc, "legacyGetPet").URL != "/pets/legacy/${petId}" || sc.Variables["petId"] != "42" || find(sc, "deletePet").URL != "/pets/${petId_2}" {
		t.Fatalf("%+v %v", find(sc, "legacyGetPet"), sc.Variables)
	}
	sc, _ = load(t, Options{GroupBy: "path", ExcludeTags: []string{"store"}})
	if sc.Steps[0].Name != "/external" {
		t.Fatalf("group by path: %s", sc.Steps[0].Name)
	}
}

func TestExternalRefAndErrors(t *testing.T) {
	_, rep := load(t, Options{OperationIDs: []string{"externalBody"}})
	for _, it := range rep.Items {
		if it.Name == "externalBody" && (it.Status != model.Partial || !strings.Contains(it.Note, "external $ref")) {
			t.Fatalf("%+v", it)
		}
	}
	for in, want := range map[string]string{
		"openapi: 3.0.0":                     "only JSON",
		`{"swagger":"2.0","paths":{"/":{}}}`: "Swagger 2.0",
		`{"openapi":"3.0.0","paths":{}}`:     "no paths",
		`{"openapi":"4.0"}`:                  "unsupported",
	} {
		if _, _, err := Import(strings.NewReader(in), "x", Options{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", in, err)
		}
	}
	big := strings.NewReader(`{"openapi":"3.0.0","x":"` + strings.Repeat("a", MaxInput) + `"}`)
	if _, _, err := Import(big, "x", Options{}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("size cap: %v", err)
	}
}

func TestSelfRefAndRefLoop(t *testing.T) {
	doc := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/a":{"post":{
	 "requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
	 "parameters":[{"$ref":"#/components/parameters/P"}],
	 "responses":{"200":{"description":"ok"}}}}},
	 "components":{"parameters":{"P":{"$ref":"#/components/parameters/Q"},"Q":{"$ref":"#/components/parameters/P"}},
	 "schemas":{"A":{"$ref":"#/components/schemas/B"},"B":{"$ref":"#/components/schemas/A"}}}}`
	sc, rep, err := Import(strings.NewReader(doc), "loop", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items[len(rep.Items)-1].Status != model.Partial || sc.AllRequests()[0].Request.Body != "{}" {
		t.Fatalf("%+v %q", rep.Items, sc.AllRequests()[0].Request.Body)
	}
}
