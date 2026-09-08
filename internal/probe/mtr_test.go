package probe

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMTRUsesDirectTargetLoss(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	wantRTTs := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
		6 * time.Millisecond,
		7 * time.Millisecond,
		8 * time.Millisecond,
	}
	wantHops := []Hop{
		{Index: 1, IP: "10.0.0.1", Sent: 10},
		{Index: 2, IP: "192.0.2.9", Sent: 10, TargetReply: true},
	}
	m.echo = func(_ context.Context, target Target, count int) (*Result, error) {
		if target.Host != "example.invalid" || count != 10 {
			t.Fatalf("echo called with target=%+v count=%d", target, count)
		}
		return &Result{Sent: 10, LossCount: 2, RTTs: wantRTTs}, nil
	}
	m.trace = func(_ context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		return wantHops, roundStats{attempted: 10, reached: 10}, nil
	}

	result, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sent != 10 || result.LossCount != 2 {
		t.Fatalf("Sent=%d LossCount=%d, want direct result 10/2", result.Sent, result.LossCount)
	}
	if !reflect.DeepEqual(result.RTTs, wantRTTs) {
		t.Fatalf("RTTs=%v, want direct RTTs %v", result.RTTs, wantRTTs)
	}
	if !reflect.DeepEqual(result.Hops, wantHops) {
		t.Fatalf("Hops=%v, want trace hops %v", result.Hops, wantHops)
	}
}

func TestMTRPreservesDirectResultOnTraceError(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	traceErr := errors.New("trace failed")
	wantRTTs := []time.Duration{3 * time.Millisecond, 5 * time.Millisecond}
	wantHops := []Hop{{Index: 1, IP: "10.0.0.1", Sent: 3}}
	var echoFinished atomic.Bool
	var traceFinished atomic.Bool
	m.echo = func(context.Context, Target, int) (*Result, error) {
		echoFinished.Store(true)
		return &Result{Sent: 3, LossCount: 1, RTTs: wantRTTs}, nil
	}
	m.trace = func(context.Context, string, string, int, int, time.Duration, time.Duration) ([]Hop, roundStats, error) {
		traceFinished.Store(true)
		return wantHops, roundStats{attempted: 3, reached: 2}, traceErr
	}

	result, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if !errors.Is(err, traceErr) {
		t.Fatalf("err=%v, want wrapped trace error", err)
	}
	if !strings.Contains(err.Error(), "mtr trace") {
		t.Fatalf("err=%q, want trace context", err)
	}
	if result.Sent != 3 || result.LossCount != 1 || !reflect.DeepEqual(result.RTTs, wantRTTs) {
		t.Fatalf("direct result not preserved: %+v", result)
	}
	if !reflect.DeepEqual(result.Hops, wantHops) {
		t.Fatalf("partial trace hops not preserved: %v", result.Hops)
	}
	if !echoFinished.Load() || !traceFinished.Load() {
		t.Fatalf("Probe returned before both operations finished: echo=%v trace=%v", echoFinished.Load(), traceFinished.Load())
	}
}

func TestMTRPreservesHopsOnDirectError(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	directErr := errors.New("direct echo failed")
	wantHops := []Hop{
		{Index: 1, IP: "10.0.0.1", Sent: 3},
		{Index: 2, IP: "192.0.2.9", Sent: 3, TargetReply: true},
	}
	var echoFinished atomic.Bool
	var traceFinished atomic.Bool
	m.echo = func(context.Context, Target, int) (*Result, error) {
		echoFinished.Store(true)
		return &Result{Sent: 1, LossCount: 1}, directErr
	}
	m.trace = func(context.Context, string, string, int, int, time.Duration, time.Duration) ([]Hop, roundStats, error) {
		traceFinished.Store(true)
		return wantHops, roundStats{attempted: 3, reached: 3}, nil
	}

	result, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if !errors.Is(err, directErr) {
		t.Fatalf("err=%v, want wrapped direct error", err)
	}
	if !strings.Contains(err.Error(), "mtr direct echo") {
		t.Fatalf("err=%q, want direct-echo context", err)
	}
	if result.Sent != 1 || result.LossCount != 1 || len(result.RTTs) != 0 {
		t.Fatalf("partial direct result not preserved: %+v", result)
	}
	if !reflect.DeepEqual(result.Hops, wantHops) {
		t.Fatalf("trace hops not preserved: %v", result.Hops)
	}
	if !echoFinished.Load() || !traceFinished.Load() {
		t.Fatalf("Probe returned before both operations finished: echo=%v trace=%v", echoFinished.Load(), traceFinished.Load())
	}
}
