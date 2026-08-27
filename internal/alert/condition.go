package alert

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tumult/gosmokeping/internal/scheduler"
)

// Op is a comparison operator.
type Op string

const (
	OpGT Op = ">"
	OpGE Op = ">="
	OpLT Op = "<"
	OpLE Op = "<="
	OpEQ Op = "=="
	OpNE Op = "!="
)

// Condition is a parsed threshold expression of the form "<field> <op> <value>".
// Valid fields: loss_pct, rtt_min, rtt_max, rtt_mean, rtt_median, rtt_p5,
// rtt_p95, rtt_stddev. Values for rtt_* fields may include a duration suffix
// (e.g. "50ms"); values without a unit are interpreted as milliseconds.
type Condition struct {
	Field string
	Op    Op
	Value float64 // for rtt_* fields this is milliseconds; for loss_pct it's a percentage
	Raw   string
	// get is the field's accessor, resolved once by ParseCondition. Eval used to
	// re-run the lookup per cycle behind an `ok` branch that only a Condition
	// built outside ParseCondition could take — and that branch returned false,
	// which reads as "never triggered" rather than as an error.
	get func(scheduler.Cycle) float64
}

// ParseCondition parses strings like "loss_pct > 5" or "rtt_median > 50ms".
func ParseCondition(s string) (Condition, error) {
	raw := s
	s = strings.TrimSpace(s)
	// Order matters — check two-char operators before one-char ones.
	ops := []Op{OpGE, OpLE, OpEQ, OpNE, OpGT, OpLT}
	var op Op
	var idx int
	for _, o := range ops {
		if i := strings.Index(s, string(o)); i >= 0 {
			op = o
			idx = i
			break
		}
	}
	if op == "" {
		return Condition{}, fmt.Errorf("no operator in %q", raw)
	}
	field := strings.TrimSpace(s[:idx])
	rhs := strings.TrimSpace(s[idx+len(op):])
	if field == "" || rhs == "" {
		return Condition{}, fmt.Errorf("malformed condition %q", raw)
	}

	get, ok := fieldGetter(field)
	if !ok {
		return Condition{}, fmt.Errorf("unknown field %q", field)
	}

	val, err := parseValue(field, rhs)
	if err != nil {
		return Condition{}, fmt.Errorf("invalid value %q: %w", rhs, err)
	}
	return Condition{Field: field, Op: op, Value: val, Raw: raw, get: get}, nil
}

func parseValue(field, s string) (float64, error) {
	if strings.HasPrefix(field, "rtt_") {
		// Accept "50ms" style durations or a bare number (ms).
		if d, err := time.ParseDuration(s); err == nil {
			return float64(d) / float64(time.Millisecond), nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	// ParseFloat accepts "inf" and "nan". A threshold of +Inf is an alert no
	// finite measurement can trip, and IEEE-754 makes `actual != NaN` true for
	// every cycle — so both validate green and then either never fire or page
	// the fleet on the first cycle from every source.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("threshold must be a finite number, got %q", s)
	}
	return v, nil
}

// Eval returns true if the cycle satisfies the condition.
func (c Condition) Eval(cy scheduler.Cycle) bool {
	if c.get == nil {
		return false
	}
	actual := c.get(cy)
	switch c.Op {
	case OpGT:
		return actual > c.Value
	case OpGE:
		return actual >= c.Value
	case OpLT:
		return actual < c.Value
	case OpLE:
		return actual <= c.Value
	case OpEQ:
		return actual == c.Value
	case OpNE:
		return actual != c.Value
	}
	return false
}

func fieldGetter(f string) (func(scheduler.Cycle) float64, bool) {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	switch f {
	case "loss_pct":
		// loss_pct is target-only loss across all probe types: ICMP/TCP/HTTP/DNS
		// each populate Sent/LossCount from attempts to the target itself, and
		// MTR from the rounds it attempted against the rounds the target
		// echoed in. Intermediate-hop drops never feed this metric — those are
		// visible in the per-hop stats but ignored by the alert evaluator.
		return func(c scheduler.Cycle) float64 {
			// Unreachable: Evaluator.OnCycle drops a no-measurement cycle
			// before any condition runs. Kept as a divide-by-zero guard only.
			if c.Sent == 0 {
				return 0
			}
			return 100 * float64(c.LossCount) / float64(c.Sent)
		}, true
	case "rtt_min":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.Min) }, true
	case "rtt_max":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.Max) }, true
	case "rtt_mean":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.Mean) }, true
	case "rtt_median":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.Median) }, true
	case "rtt_p5":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.P5) }, true
	case "rtt_p95":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.P95) }, true
	case "rtt_stddev":
		return func(c scheduler.Cycle) float64 { return ms(c.Summary.StdDev) }, true
	}
	return nil, false
}
