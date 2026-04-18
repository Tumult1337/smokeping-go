package stats

import (
	"math"
	"slices"
	"time"
)

type Summary struct {
	Min    time.Duration
	Max    time.Duration
	Mean   time.Duration
	Median time.Duration
	StdDev time.Duration
	// Percentiles every 5% for dense SmokePing-style rendering. P50 is not
	// stored separately — use Median. The drawing code walks symmetric pairs
	// (P5/P95, P10/P90, ..., P45/P55) as stacked translucent bands.
	P5  time.Duration
	P10 time.Duration
	P15 time.Duration
	P20 time.Duration
	P25 time.Duration
	P30 time.Duration
	P35 time.Duration
	P40 time.Duration
	P45 time.Duration
	P55 time.Duration
	P60 time.Duration
	P65 time.Duration
	P70 time.Duration
	P75 time.Duration
	P80 time.Duration
	P85 time.Duration
	P90 time.Duration
	P95 time.Duration
}

// Compute returns a Summary of rtts. If rtts is empty all fields are zero.
// Input is not modified.
func Compute(rtts []time.Duration) Summary {
	n := len(rtts)
	if n == 0 {
		return Summary{}
	}

	sorted := slices.Clone(rtts)
	slices.Sort(sorted)

	var sumNs int64
	for _, v := range sorted {
		sumNs += int64(v)
	}
	meanNs := float64(sumNs) / float64(n)

	var sqSum float64
	for _, v := range sorted {
		d := float64(v) - meanNs
		sqSum += d * d
	}
	stddevNs := math.Sqrt(sqSum / float64(n))

	return Summary{
		Min:    sorted[0],
		Max:    sorted[n-1],
		Mean:   time.Duration(meanNs),
		Median: percentile(sorted, 0.50),
		StdDev: time.Duration(stddevNs),
		P5:     percentile(sorted, 0.05),
		P10:    percentile(sorted, 0.10),
		P15:    percentile(sorted, 0.15),
		P20:    percentile(sorted, 0.20),
		P25:    percentile(sorted, 0.25),
		P30:    percentile(sorted, 0.30),
		P35:    percentile(sorted, 0.35),
		P40:    percentile(sorted, 0.40),
		P45:    percentile(sorted, 0.45),
		P55:    percentile(sorted, 0.55),
		P60:    percentile(sorted, 0.60),
		P65:    percentile(sorted, 0.65),
		P70:    percentile(sorted, 0.70),
		P75:    percentile(sorted, 0.75),
		P80:    percentile(sorted, 0.80),
		P85:    percentile(sorted, 0.85),
		P90:    percentile(sorted, 0.90),
		P95:    percentile(sorted, 0.95),
	}
}

// percentile uses linear interpolation between closest ranks.
// sorted must be non-empty and ascending.
func percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	pos := p * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + time.Duration(frac*float64(sorted[hi]-sorted[lo]))
}
