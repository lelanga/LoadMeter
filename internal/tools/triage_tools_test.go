package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/results"
	"github.com/lelanga/loadmeter/internal/service"
	"github.com/lelanga/loadmeter/internal/summary"
	"github.com/lelanga/loadmeter/internal/triage"
)

func TestTriageAndSummarizeTools(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	svc, err := service.New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	id := "20261012-101010-tool-demo"
	dir := filepath.Join(svc.WS.RunsDir(), id)
	start := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	sum := &results.Summary{RunID: id, Scenario: "demo", Status: "finished", StartedAt: start, DurationS: 30}
	sum.Snapshot.Requests.Count, sum.Snapshot.Requests.Errors, sum.Snapshot.Requests.ErrorRate = 10, 2, 20
	if err := results.Save(dir, sum); err != nil {
		t.Fatal(err)
	}
	ts := start.UnixMilli()
	csv := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind\n"
	for i := 0; i < 2; i++ {
		csv += strings.Join([]string{itoa(ts + int64(i*1000)), "5", "GET /x", "503", "http_5xx", "vu-1", "false", "503 Service Unavailable", "0", "0", "0", "http://127.0.0.1:1/x", "request"}, ",") + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "raw.csv"), []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"triage_run", "summarize_run"} {
		tl, ok := Find(n)
		if !ok || !tl.ReadOnly || tl.Confirm {
			t.Fatalf("%s must be a registered read-only tool", n)
		}
	}
	out, err := Call(context.Background(), svc, "triage_run", map[string]any{"runId": id}, "test")
	if err != nil {
		t.Fatal(err)
	}
	tr := out.(*triage.Result)
	if len(tr.Clusters) != 1 || tr.Clusters[0].Side != triage.SideTarget || tr.Clusters[0].Count != 2 {
		t.Errorf("%+v", tr)
	}
	out, err = Call(context.Background(), svc, "summarize_run", map[string]any{"runId": id}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(summary.Result); r.Source != "template" || !strings.Contains(r.Text, "GET /x") {
		t.Errorf("%+v", r)
	}
	if _, err := Call(context.Background(), svc, "triage_run", map[string]any{"runId": "../etc"}, "test"); err == nil {
		t.Error("path traversal run id must be refused")
	}
	if _, err := Call(context.Background(), svc, "summarize_run", map[string]any{"runId": id, "ai": true}, "test"); err == nil {
		t.Error("summarize_run must not accept an ai argument")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
