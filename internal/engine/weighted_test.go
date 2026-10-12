package engine

import (
	"math"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

func weighted(children ...model.Step) model.Step {
	return model.Step{Type: model.StepGroup, Name: "mix", Select: model.SelectWeighted, Steps: children}
}

func withWeight(st model.Step, w float64) model.Step { st.Weight = &w; return st }

// A weighted group runs exactly one child per execution, in proportion to the
// weights; with a fixed seed the outcome is reproducible.
func TestWeightedGroupFollowsWeights(t *testing.T) {
	srv, _ := newMock(t)
	mk := func() *model.Scenario {
		return newSc(srv.URL, perVU(4, 750), weighted(
			withWeight(get("health", "/health"), 70),
			withWeight(get("items", "/api/items"), 20),
			withWeight(get("delay", "/delay?ms=0"), 10),
			withWeight(get("never", "/status/500"), 0),
		))
	}
	run := func() map[string]uint64 {
		rec := &recorder{}
		_, snap := runSc(t, mk(), opts(rec))
		out := map[string]uint64{}
		for _, n := range snap.ByRequest {
			out[n.Name] = n.Count
		}
		return out
	}
	got := run()
	var total uint64
	for _, n := range got {
		total += n
	}
	if total != 3000 {
		t.Fatalf("one request per iteration expected, got %d (%v)", total, got)
	}
	if got["never"] != 0 {
		t.Errorf("zero-weight step ran %d times", got["never"])
	}
	for name, want := range map[string]float64{"health": 0.70, "items": 0.20, "delay": 0.10} {
		if share := float64(got[name]) / float64(total); math.Abs(share-want) > 0.03 {
			t.Errorf("%s share %.3f, want %.2f±0.03", name, share, want)
		}
	}
	again := run()
	for k, v := range got {
		if again[k] != v {
			t.Errorf("seeded run not reproducible: %v vs %v", got, again)
			break
		}
	}
}

func TestWeightedGroupSkipsDisabledAndDefaultsWeight(t *testing.T) {
	srv, _ := newMock(t)
	off := get("off", "/status/500")
	off.Disabled = true
	sc := newSc(srv.URL, perVU(1, 200), weighted(get("a", "/health"), get("b", "/api/items"), withWeight(off, 1000)))
	_, snap := runSc(t, sc, opts(nil))
	a, _ := byReq(snap, "a")
	b, _ := byReq(snap, "b")
	if _, ok := byReq(snap, "off"); ok || a.Count+b.Count != 200 {
		t.Fatalf("a=%d b=%d", a.Count, b.Count)
	}
	if a.Count < 70 || b.Count < 70 {
		t.Errorf("default weights should split about evenly: a=%d b=%d", a.Count, b.Count)
	}
}

func TestThinkDistributionIsSampledAndClamped(t *testing.T) {
	srv, _ := newMock(t)
	th := model.Step{Type: model.StepThink, Think: &model.Think{
		Min: model.Duration(5 * time.Millisecond), Max: model.Duration(8 * time.Millisecond),
		Distribution: &model.Distribution{Type: model.DistLogNormal, Mu: 3, Sigma: 1}}} // median 20 s, clamped to 8 ms
	sc := newSc(srv.URL, perVU(1, 10), get("a", "/health"), th)
	start := time.Now()
	runSc(t, sc, opts(nil))
	if el := time.Since(start); el < 50*time.Millisecond || el > 2*time.Second {
		t.Errorf("10 iterations with a 5..8 ms clamp took %v", el)
	}
}
