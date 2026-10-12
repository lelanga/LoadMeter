package metrics

import (
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkCollectorAdd measures Collector.Add under parallel load (one
// goroutine per GOMAXPROCS, as VUs record samples) with and without the raw CSV.
func BenchmarkCollectorAdd(b *testing.B) {
	for _, raw := range []bool{false, true} {
		name := "noRaw"
		var w io.Writer
		if raw {
			name, w = "raw", io.Discard
		}
		b.Run(name, func(b *testing.B) {
			c := NewCollector(time.Now(), w)
			var vu atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				id := int(vu.Add(1))
				s := Sample{Kind: KindRequest, Name: "GET /x" + strconv.Itoa(id%4), URL: "http://127.0.0.1/x", Status: 200, OK: true,
					Latency: 3 * time.Millisecond, TTFB: time.Millisecond, BytesIn: 512, BytesOut: 100, VU: id}
				for pb.Next() {
					s.Start = time.Now()
					c.Add(s)
				}
			})
		})
	}
}
