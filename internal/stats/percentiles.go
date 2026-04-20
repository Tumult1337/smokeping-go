package stats

import "time"

// PercentileSpec is one entry in the canonical percentile list. Get/Set bind
// the spec to the matching Summary field — adding a new percentile means
// adding an entry here AND a field to Summary; the closures make the binding
// a compile-time check rather than a silent drift between them.
type PercentileSpec struct {
	Name  string // Influx field suffix ("p5", "p10", ... "p95"). No "rtt_" prefix.
	Ratio float64
	Get   func(Summary) time.Duration
	Set   func(*Summary, time.Duration)
}

// PercentileSet is the single source of truth for which percentiles this
// backend tracks, in what order, and under what field name. The writer and
// reader iterate this list, and the Flux rollup is generated from it.
//
// P50 is intentionally absent — use Summary.Median.
var PercentileSet = []PercentileSpec{
	{"p5", 0.05, func(s Summary) time.Duration { return s.P5 }, func(s *Summary, v time.Duration) { s.P5 = v }},
	{"p10", 0.10, func(s Summary) time.Duration { return s.P10 }, func(s *Summary, v time.Duration) { s.P10 = v }},
	{"p15", 0.15, func(s Summary) time.Duration { return s.P15 }, func(s *Summary, v time.Duration) { s.P15 = v }},
	{"p20", 0.20, func(s Summary) time.Duration { return s.P20 }, func(s *Summary, v time.Duration) { s.P20 = v }},
	{"p25", 0.25, func(s Summary) time.Duration { return s.P25 }, func(s *Summary, v time.Duration) { s.P25 = v }},
	{"p30", 0.30, func(s Summary) time.Duration { return s.P30 }, func(s *Summary, v time.Duration) { s.P30 = v }},
	{"p35", 0.35, func(s Summary) time.Duration { return s.P35 }, func(s *Summary, v time.Duration) { s.P35 = v }},
	{"p40", 0.40, func(s Summary) time.Duration { return s.P40 }, func(s *Summary, v time.Duration) { s.P40 = v }},
	{"p45", 0.45, func(s Summary) time.Duration { return s.P45 }, func(s *Summary, v time.Duration) { s.P45 = v }},
	{"p55", 0.55, func(s Summary) time.Duration { return s.P55 }, func(s *Summary, v time.Duration) { s.P55 = v }},
	{"p60", 0.60, func(s Summary) time.Duration { return s.P60 }, func(s *Summary, v time.Duration) { s.P60 = v }},
	{"p65", 0.65, func(s Summary) time.Duration { return s.P65 }, func(s *Summary, v time.Duration) { s.P65 = v }},
	{"p70", 0.70, func(s Summary) time.Duration { return s.P70 }, func(s *Summary, v time.Duration) { s.P70 = v }},
	{"p75", 0.75, func(s Summary) time.Duration { return s.P75 }, func(s *Summary, v time.Duration) { s.P75 = v }},
	{"p80", 0.80, func(s Summary) time.Duration { return s.P80 }, func(s *Summary, v time.Duration) { s.P80 = v }},
	{"p85", 0.85, func(s Summary) time.Duration { return s.P85 }, func(s *Summary, v time.Duration) { s.P85 = v }},
	{"p90", 0.90, func(s Summary) time.Duration { return s.P90 }, func(s *Summary, v time.Duration) { s.P90 = v }},
	{"p95", 0.95, func(s Summary) time.Duration { return s.P95 }, func(s *Summary, v time.Duration) { s.P95 = v }},
}
