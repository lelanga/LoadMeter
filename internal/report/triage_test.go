package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/results"
)

// A report written to a file next to raw.csv gets the failure triage section.
func TestReportIncludesTriageWhenRawCSVPresent(t *testing.T) {
	dir := t.TempDir()
	start := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	raw := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage,bytes,sentBytes,Latency,URL,kind\n" +
		strings.Join([]string{"" + itoa(start.UnixMilli()+2000), "5", "GET /flaky", "500", "http_5xx", "vu-1", "false", "500 Internal Server Error", "0", "0", "0", "http://127.0.0.1:1/flaky", "request"}, ",") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "raw.csv"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &results.Summary{RunID: "r", Scenario: "x", StartedAt: start, DurationS: 10}
	s.Snapshot.Requests.Count, s.Snapshot.Requests.Errors = 5, 1
	f, err := os.Create(filepath.Join(dir, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Render(f, s, ""); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(b), "Failure triage") || !strings.Contains(string(b), "GET /flaky") {
		t.Error("triage section missing from the report")
	}
	// no errors: no section
	var sb strings.Builder
	s.Snapshot.Requests.Errors = 0
	if err := Render(&sb, s, ""); err != nil || strings.Contains(sb.String(), "Failure triage") {
		t.Errorf("unexpected triage section: %v", err)
	}
}

func itoa(n int64) string {
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
