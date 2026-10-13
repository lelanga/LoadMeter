package k6

import (
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/model"
)

func TestParityFeaturesNoted(t *testing.T) {
	sc := model.NewScenario("p")
	sc.Defaults.BaseURL = "http://h.example"
	rq := &model.Request{Method: "GET", URL: "/", RequestCheckOptions: model.RequestCheckOptions{IgnoreStatus: true},
		Extract: []model.Extractor{{Name: "ids", Type: model.ExRegex, Expr: `id=(\d+)`, Match: model.MatchAll}},
		Assert: []model.Assertion{{Type: model.AsEquals, Value: "x"},
			{Type: model.AsContains, Value: "y", AssertionOptions: model.AssertionOptions{Not: true}}}}
	sc.Steps = []model.Step{{Type: model.StepRequest, Name: "r", Request: rq}}
	_, rep, err := Export(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, it := range rep.Items {
		notes = append(notes, it.Note)
	}
	all := strings.Join(notes, "\n")
	for _, want := range []string{"ignoreStatus", `match "all"`, "equals assertion not exported", "not/regex/expectNull/source"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing note %q in:\n%s", want, all)
		}
	}
}
