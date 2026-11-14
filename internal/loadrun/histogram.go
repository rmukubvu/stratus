package loadrun

import (
	"math"
	"time"
)

// Latency buckets grow by a quarter from 0.25 ms, so a percentile is never
// more than 25% above the true value, and 64 buckets reach past ten
// minutes. A histogram is a fixed array, so recording costs no allocation
// and memory stays flat however long a run lasts.
const (
	histogramBuckets = 64
	histogramBase    = 0.25 // ms
	histogramGrowth  = 1.25
)

var bucketBounds = func() [histogramBuckets]float64 {
	var b [histogramBuckets]float64
	bound := histogramBase
	for i := range b {
		b[i] = bound
		bound *= histogramGrowth
	}
	return b
}()

type histogram struct {
	counts [histogramBuckets]uint32
	total  uint32
	max    float64
}

func (h *histogram) add(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	i := 0
	if ms > histogramBase {
		i = int(math.Ceil(math.Log(ms/histogramBase) / math.Log(histogramGrowth)))
	}
	i = min(max(i, 0), histogramBuckets-1)
	// Guard rounding at bucket edges.
	for i > 0 && bucketBounds[i-1] >= ms {
		i--
	}
	h.counts[i]++
	h.total++
	h.max = math.Max(h.max, ms)
}

// percentile returns the upper bound of the bucket holding the p-th
// percentile (0 < p <= 1), capped at the largest value seen, in ms.
func (h *histogram) percentile(p float64) float64 {
	if h.total == 0 {
		return 0
	}
	rank := uint32(math.Ceil(p * float64(h.total)))
	var seen uint32
	for i, c := range h.counts {
		seen += c
		if seen >= rank {
			return round(math.Min(bucketBounds[i], h.max))
		}
	}
	return round(h.max)
}

func round(ms float64) float64 { return math.Round(ms*100) / 100 }
