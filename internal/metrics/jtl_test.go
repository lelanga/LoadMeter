package metrics

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"
)

func jtlRows(t *testing.T, buf *bytes.Buffer) (map[string]int, [][]string) {
	t.Helper()
	rows, err := csv.NewReader(buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]int{}
	for i, h := range rows[0] {
		idx[h] = i
	}
	return idx, rows[1:]
}

func TestJTLThreadsAndTransactionRows(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	var buf bytes.Buffer
	c := NewCollector(t0, &buf)
	active := 7
	c.SetThreads(func() int { return active })
	c.Add(Sample{Kind: KindRequest, Name: "r", Start: t0, Latency: time.Millisecond, Status: 200, OK: true, VU: 1})
	c.Add(Sample{Kind: KindTransaction, Name: "tx ok", Start: t0, Latency: time.Millisecond, OK: true, VU: 1, SubSamples: 3})
	c.Add(Sample{Kind: KindTransaction, Name: "tx bad", Start: t0, Latency: time.Millisecond, OK: false, ErrKind: "transaction",
		VU: 1, SubSamples: 2, SubFailures: 1})
	c.SetThreads(nil)
	c.SetGauge(t0, 4, 0, 0)
	c.Add(Sample{Kind: KindRequest, Name: "r2", Start: t0, Latency: time.Millisecond, Status: 500, ErrKind: "http_5xx", VU: 2})
	c.Flush()
	idx, rows := jtlRows(t, &buf)
	for _, col := range []string{"grpThreads", "allThreads", "responseMessage", "responseCode"} {
		if _, ok := idx[col]; !ok {
			t.Fatalf("missing column %s", col)
		}
	}
	get := func(r int, col string) string { return rows[r][idx[col]] }
	if get(0, "grpThreads") != "7" || get(0, "allThreads") != "7" || get(3, "grpThreads") != "4" {
		t.Errorf("threads: %v / %v", rows[0], rows[3])
	}
	if get(0, "responseMessage") != "" || get(3, "responseMessage") != "http_5xx" {
		t.Errorf("request messages changed: %q %q", get(0, "responseMessage"), get(3, "responseMessage"))
	}
	if m := get(1, "responseMessage"); m != "Number of samples in transaction : 3, number of failing samples : 0" || get(1, "responseCode") != "200" {
		t.Errorf("tx ok row: %q %q", get(1, "responseCode"), m)
	}
	if m := get(2, "responseMessage"); m != "Number of samples in transaction : 2, number of failing samples : 1" || get(2, "responseCode") != "" {
		t.Errorf("tx bad row: %q %q", get(2, "responseCode"), m)
	}
}
