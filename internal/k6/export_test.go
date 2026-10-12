package k6

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/openapi"
)

func nodeCheck(t *testing.T, name string, src []byte) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	f := filepath.Join(t.TempDir(), name+".mjs")
	if err := os.WriteFile(f, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("node --check %s failed: %v\n%s", name, err, out)
	}
}

func TestExportShopLogin(t *testing.T) {
	sc, err := app.LoadScenario("../../examples/shop-login.json")
	if err != nil {
		t.Fatal(err)
	}
	b, rep, err := Export(sc, Options{DataPrefix: "examples"})
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"import http from 'k6/http';", "import { SharedArray } from 'k6/data';",
		`"executor": "ramping-vus"`, `"http_req_duration{group:::03_order}"`, `"p(95)<500"`,
		`group("01_login"`, `open("examples/users.csv")`, "_rx(res.body,", "_jp(res.body, \"$.token\")", "_hdr(res, \"X-CSRF-Token\")",
		"_think(200, 500, 1)", "_stat(r.status, \"201\")",
	} {
		// json.Marshal escapes < > as <; accept either spelling
		if !strings.Contains(src, want) && !strings.Contains(src, strings.ReplaceAll(want, `<`, "<")) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(src, "https://") {
		t.Error("output must not contain remote imports")
	}
	if len(rep.Items) < 12 || rep.Confidence == "" {
		t.Fatalf("report too small: %d items", len(rep.Items))
	}
	kinds := map[string]int{}
	for _, it := range rep.Items {
		kinds[it.Element]++
	}
	if kinds["request"] != 6 || kinds["transaction"] != 3 || kinds["sla"] != 3 || kinds["dataSource"] != 1 {
		t.Fatalf("items %v", kinds)
	}
	nodeCheck(t, "shop-login", b)
}

func TestExportOpenAPIResult(t *testing.T) {
	f, err := os.Open("../../testdata/openapi/petstore-like.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc, _, err := openapi.Import(f, "petstore-like.json", openapi.Options{IncludeDestructive: true})
	if err != nil {
		t.Fatal(err)
	}
	b, rep, err := Export(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "${__uuid()}") || !strings.Contains(string(b), "secret_bearerauth") {
		t.Error("expected dynamic body and secret placeholder in output")
	}
	if len(rep.Items) == 0 {
		t.Fatal("empty report")
	}
	nodeCheck(t, "petstore", b)
}

func TestUnsupportedAndPartial(t *testing.T) {
	sc := model.NewScenario("edge")
	sc.Defaults.BaseURL = "http://127.0.0.1:1"
	off := false
	sc.Steps = []model.Step{
		{Type: model.StepTransaction, Name: "t{1}", Steps: []model.Step{{Type: model.StepRequest, Name: "x", Request: &model.Request{
			Method: "GET", URL: "/a?${__groovy(1)}", FollowRedirects: &off,
			Extract: []model.Extractor{{Name: "v", Type: model.ExXPath, Expr: "//a", Required: true}, {Name: "w", Type: model.ExJSONPath, Expr: "$..a[?(@.b)]"}},
			Assert:  []model.Assertion{{Type: model.AsMaxLatency, Value: "250ms"}},
		}}}},
		{Type: model.StepLoop, Count: 2, Steps: []model.Step{{Type: model.StepThink, Think: &model.Think{Min: model.Duration(1e6)}}}},
	}
	sc.Workload = model.Workload{Executor: model.ExecArrivalRate, Rate: 0.5, Duration: model.Duration(1e10), MaxVUs: 3}
	sc.SLAs = []model.SLA{{Metric: "p95", Scope: "nope", Op: "<", Threshold: 1}, {Metric: "p77", Op: "<", Threshold: 1}}
	b, rep, err := Export(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Counts()
	if c[model.Unsupported] != 2 || c[model.Partial] < 2 {
		t.Fatalf("counts %v\n%+v", c, rep.Items)
	}
	for _, w := range []string{`"timeUnit": "1m"`, `"rate": 30`, "// TODO: xpath"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("missing %s", w)
		}
	}
	nodeCheck(t, "edge", b)
	if _, _, err := Export(&model.Scenario{}, Options{}); err == nil {
		t.Error("invalid scenario must fail")
	}
}
