package metrics

import (
	"bytes"
	"encoding/csv"
	"sync"
	"testing"
	"time"
)

// Rows are formatted outside the collector lock; concurrent Adds must still
// produce one complete, well-formed CSV row per sample.
func TestCollectorRawConcurrent(t *testing.T) {
	t0 := time.Now()
	var buf bytes.Buffer
	c := NewCollector(t0, &buf)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				c.Add(Sample{Kind: KindRequest, Name: "a,\"b\"", Start: t0, Latency: time.Millisecond, Status: 200, OK: true, VU: g,
					Connect: 2 * time.Millisecond, DNS: time.Millisecond, Retries: 1, BodyBytes: 42})
			}
		}(g)
	}
	wg.Wait()
	c.Flush()
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4001 {
		t.Fatalf("rows=%d", len(rows))
	}
	for _, r := range rows[1:] {
		if len(r) != len(rows[0]) || r[2] != `a,"b"` || r[13] != "2" || r[14] != "1" || r[16] != "false" || r[17] != "1" || r[18] != "42" {
			t.Fatalf("bad row %v", r)
		}
	}
	if c.Retries != 4000 || c.NewConns != 4000 {
		t.Errorf("retries=%d newConns=%d", c.Retries, c.NewConns)
	}
}
