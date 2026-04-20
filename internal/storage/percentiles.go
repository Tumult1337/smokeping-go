package storage

// CyclePointPercentileAccessor binds one percentile field on CyclePoint to
// its name. Parallel to stats.PercentileSet — adding a percentile requires
// an entry in both slices. A test in the storage package enforces that every
// stats.PercentileSet entry has a matching accessor here.
type CyclePointPercentileAccessor struct {
	Name string
	Get  func(CyclePoint) float64
	Set  func(*CyclePoint, float64)
}

// CyclePointPercentileAccessors is indexed by the same Name as
// stats.PercentileSet; the influxv2 reader/writer iterate this to read and
// write the per-percentile fields on CyclePoint.
var CyclePointPercentileAccessors = []CyclePointPercentileAccessor{
	{"p5", func(p CyclePoint) float64 { return p.P5 }, func(p *CyclePoint, v float64) { p.P5 = v }},
	{"p10", func(p CyclePoint) float64 { return p.P10 }, func(p *CyclePoint, v float64) { p.P10 = v }},
	{"p15", func(p CyclePoint) float64 { return p.P15 }, func(p *CyclePoint, v float64) { p.P15 = v }},
	{"p20", func(p CyclePoint) float64 { return p.P20 }, func(p *CyclePoint, v float64) { p.P20 = v }},
	{"p25", func(p CyclePoint) float64 { return p.P25 }, func(p *CyclePoint, v float64) { p.P25 = v }},
	{"p30", func(p CyclePoint) float64 { return p.P30 }, func(p *CyclePoint, v float64) { p.P30 = v }},
	{"p35", func(p CyclePoint) float64 { return p.P35 }, func(p *CyclePoint, v float64) { p.P35 = v }},
	{"p40", func(p CyclePoint) float64 { return p.P40 }, func(p *CyclePoint, v float64) { p.P40 = v }},
	{"p45", func(p CyclePoint) float64 { return p.P45 }, func(p *CyclePoint, v float64) { p.P45 = v }},
	{"p55", func(p CyclePoint) float64 { return p.P55 }, func(p *CyclePoint, v float64) { p.P55 = v }},
	{"p60", func(p CyclePoint) float64 { return p.P60 }, func(p *CyclePoint, v float64) { p.P60 = v }},
	{"p65", func(p CyclePoint) float64 { return p.P65 }, func(p *CyclePoint, v float64) { p.P65 = v }},
	{"p70", func(p CyclePoint) float64 { return p.P70 }, func(p *CyclePoint, v float64) { p.P70 = v }},
	{"p75", func(p CyclePoint) float64 { return p.P75 }, func(p *CyclePoint, v float64) { p.P75 = v }},
	{"p80", func(p CyclePoint) float64 { return p.P80 }, func(p *CyclePoint, v float64) { p.P80 = v }},
	{"p85", func(p CyclePoint) float64 { return p.P85 }, func(p *CyclePoint, v float64) { p.P85 = v }},
	{"p90", func(p CyclePoint) float64 { return p.P90 }, func(p *CyclePoint, v float64) { p.P90 = v }},
	{"p95", func(p CyclePoint) float64 { return p.P95 }, func(p *CyclePoint, v float64) { p.P95 = v }},
}
