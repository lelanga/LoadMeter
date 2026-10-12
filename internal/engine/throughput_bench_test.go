package engine

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestThroughputMock is an opt-in throughput benchmark (LOADMETER_BENCH=1):
// 50 VUs against the in-process mock for LOADMETER_BENCH_SECS (default 10) s.
// It prints requests/s so before/after numbers can be compared on one machine.
// LOADMETER_BENCH_CONNECTIONS=shared|per-vu picks the connection model.
//
//	LOADMETER_BENCH=1 go test ./internal/engine -run TestThroughputMock -v
func TestThroughputMock(t *testing.T) {
	if os.Getenv("LOADMETER_BENCH") == "" {
		t.Skip("set LOADMETER_BENCH=1 to run the throughput benchmark")
	}
	secs := 10
	if v, err := strconv.Atoi(os.Getenv("LOADMETER_BENCH_SECS")); err == nil && v > 0 {
		secs = v
	}
	srv, _ := newMock(t)
	sc := newSc(srv.URL, constVUs(50, time.Duration(secs)*time.Second), get("health", "/health"))
	sc.Defaults.Connections = os.Getenv("LOADMETER_BENCH_CONNECTIONS") // "", per-vu or shared
	o := opts(nil)
	o.Policy.MaxVUs = 100
	_, snap := runSc(t, sc, o)
	t.Logf("50 VUs, %ds: requests=%d errors=%d rps=%.0f p50=%.2fms p99=%.2fms",
		secs, snap.Requests.Count, snap.Requests.Errors, float64(snap.Requests.Count)/snap.ElapsedSec, snap.Requests.P50Ms, snap.Requests.P99Ms)
}
