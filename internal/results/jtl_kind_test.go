package results

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lelanga/loadmeter/internal/metrics"
)

// A JMeter JTL has no "kind" column; Transaction Controller rows are told
// apart by their responseMessage, as JMeter's report generator does.
func TestReadRawWindowsJMeterTransactionRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "raw.csv")
	data := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success\n" +
		"1000,10,login,200,OK,t-1,true\n" +
		"1000,30,Checkout,200,\"Number of samples in transaction : 2, number of failing samples : 0\",t-1,true\n"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRawWindows(p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got[metrics.KindRequest+"\x00login"] == nil || got[metrics.KindTransaction+"\x00Checkout"] == nil {
		keys := []string{}
		for k := range got {
			keys = append(keys, k)
		}
		t.Errorf("series keys %q", keys)
	}
}
