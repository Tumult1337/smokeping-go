package stats

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestEmpty(t *testing.T) {
	s := Compute(nil)
	if s != (Summary{}) {
		t.Errorf("empty summary = %+v", s)
	}
}

func TestSingle(t *testing.T) {
	s := Compute([]time.Duration{ms(42)})
	if s.Min != ms(42) || s.Max != ms(42) || s.Median != ms(42) ||
		s.P5 != ms(42) || s.P95 != ms(42) || s.Mean != ms(42) {
		t.Errorf("single summary = %+v", s)
	}
	if s.StdDev != 0 {
		t.Errorf("stddev = %v, want 0", s.StdDev)
	}
}

func TestBasic(t *testing.T) {
	in := []time.Duration{ms(10), ms(20), ms(30), ms(40), ms(50)}
	s := Compute(in)
	if s.Min != ms(10) {
		t.Errorf("min = %v", s.Min)
	}
	if s.Max != ms(50) {
		t.Errorf("max = %v", s.Max)
	}
	if s.Median != ms(30) {
		t.Errorf("median = %v", s.Median)
	}
	if s.Mean != ms(30) {
		t.Errorf("mean = %v", s.Mean)
	}
}

func TestPercentiles(t *testing.T) {
	// 0..99 ms
	in := make([]time.Duration, 100)
	for i := range in {
		in[i] = ms(i)
	}
	s := Compute(in)
	// With n-1 = 99, p=0.05 → pos=4.95 → ~4.95ms
	if s.P5 < ms(4) || s.P5 > ms(6) {
		t.Errorf("p5 = %v, want ~5ms", s.P5)
	}
	if s.P95 < ms(93) || s.P95 > ms(95) {
		t.Errorf("p95 = %v, want ~94ms", s.P95)
	}
}

func TestDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{ms(30), ms(10), ms(20)}
	orig := append([]time.Duration(nil), in...)
	_ = Compute(in)
	for i, v := range in {
		if v != orig[i] {
			t.Errorf("input mutated at %d: %v != %v", i, v, orig[i])
		}
	}
}

func TestStdDev(t *testing.T) {
	// All equal → stddev 0
	in := []time.Duration{ms(10), ms(10), ms(10)}
	if s := Compute(in); s.StdDev != 0 {
		t.Errorf("stddev = %v, want 0", s.StdDev)
	}
	// Population stddev of [10,20,30]ms mean=20, var=(100+0+100)/3≈66.67, sd≈8.165ms
	in = []time.Duration{ms(10), ms(20), ms(30)}
	s := Compute(in)
	want := 8165000 * time.Nanosecond
	if diff := s.StdDev - want; diff < -100000 || diff > 100000 {
		t.Errorf("stddev = %v, want ~%v", s.StdDev, want)
	}
}
