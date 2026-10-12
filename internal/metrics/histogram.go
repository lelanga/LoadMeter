// Package metrics records samples into log-linear histograms so that
// percentiles are computed from the full distribution (never by averaging
// per-window percentiles). Relative error is below 0.8% per value.
package metrics

import (
	"math"
	"math/bits"
	"slices"
)

const (
	subBits  = 7
	subCount = 1 << subBits // 128
	nBuckets = 64 * subCount
)

func bucketIndex(v uint64) int {
	if v < 2*subCount {
		return int(v)
	}
	shift := bits.Len64(v) - (subBits + 1)
	return (shift+1)*subCount + int(v>>uint(shift)) - subCount
}

// bucketMid returns a representative value for a bucket (midpoint).
func bucketMid(i int) uint64 {
	if i < 2*subCount {
		return uint64(i)
	}
	shift := i/subCount - 1
	mant := uint64(i%subCount + subCount)
	low := mant << uint(shift)
	return low + (uint64(1)<<uint(shift))/2
}

// Histogram of values in microseconds.
type Histogram struct {
	counts []uint64
	Total  uint64
	Sum    float64
	Min    uint64
	Max    uint64
}

func NewHistogram() *Histogram {
	return &Histogram{counts: make([]uint64, nBuckets), Min: math.MaxUint64}
}

func (h *Histogram) Record(us uint64) {
	h.counts[bucketIndex(us)]++
	h.Total++
	h.Sum += float64(us)
	if us < h.Min {
		h.Min = us
	}
	if us > h.Max {
		h.Max = us
	}
}

func (h *Histogram) Merge(o *Histogram) {
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.Total += o.Total
	h.Sum += o.Sum
	if o.Total > 0 {
		h.Min = min(h.Min, o.Min)
		h.Max = max(h.Max, o.Max)
	}
}

// Quantile returns the q-quantile (0..1) using nearest-rank, clamped to [Min,Max].
func (h *Histogram) Quantile(q float64) uint64 {
	if h.Total == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(h.Total)))
	if rank < 1 {
		rank = 1
	}
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= rank {
			v := bucketMid(i)
			return max(h.Min, min(h.Max, v))
		}
	}
	return h.Max
}

func (h *Histogram) Mean() float64 {
	if h.Total == 0 {
		return 0
	}
	return h.Sum / float64(h.Total)
}

// Sparse is a compact histogram for per-second windows.
type Sparse map[uint16]uint32

func (s Sparse) Record(us uint64) { s[uint16(bucketIndex(us))]++ }

func (s Sparse) Quantile(q float64) uint64 {
	var total uint64
	for _, c := range s {
		total += uint64(c)
	}
	if total == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(total)))
	if rank < 1 {
		rank = 1
	}
	// iterate in bucket order
	keys := make([]int, 0, len(s))
	for k := range s {
		keys = append(keys, int(k))
	}
	slices.Sort(keys)
	var cum uint64
	for _, k := range keys {
		cum += uint64(s[uint16(k)])
		if cum >= rank {
			return bucketMid(k)
		}
	}
	return 0
}
