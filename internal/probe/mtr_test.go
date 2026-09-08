package probe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMTRDirectEchoReservesFullTraceWindow(t *testing.T) {
	echo := newMTRDirectICMP("mtr", time.Second)
	// Every sequence the ten-round, thirty-TTL walk can emit must be below
	// the direct batch, including when raw socket identifiers collide.
	for round := range 10 {
		for ttl := 1; ttl <= 30; ttl++ {
			seq := round*31 + ttl
			if seq >= echo.traceSeqCeil() {
				t.Fatalf("trace sequence %d overlaps direct window starting at %d", seq, echo.traceSeqCeil())
			}
		}
	}
	if !echo.noTrace {
		t.Fatal("direct echo enabled a second TTL walk")
	}
	if echo.traceSeqCeil()+10 > 1<<16 {
		t.Fatal("direct batch cannot fit above the full trace window")
	}
	// Fill the remaining sequence space so allocation has exactly one legal
	// base. This checks the lower boundary without relying on random draws.
	base := echo.echoBaseSeq(65536 - 310)
	if base != 310 {
		t.Fatalf("direct sequence base = %d, want 310", base)
	}
}

func TestMTRSharesCancellationAndCapsBothOperations(t *testing.T) {
	for _, tc := range []struct{ requested, want int }{{3, 3}, {27, 10}} {
		t.Run(fmt.Sprintf("count_%d", tc.requested), func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			abort := make(chan struct{})
			type observation struct {
				operation string
				ctx       context.Context
				count     int
			}
			started := make(chan observation, 2)
			finished := make(chan string, 2)
			wait := func(operation string, ctx context.Context, count int) error {
				started <- observation{operation, ctx, count}
				select {
				case <-ctx.Done():
				case <-abort:
				}
				finished <- operation
				return ctx.Err()
			}
			m := NewMTR("mtr", time.Second)
			m.echo = func(ctx context.Context, _ Target, count int) (*Result, error) {
				return &Result{}, wait("echo", ctx, count)
			}
			m.trace = func(ctx context.Context, _, _ string, count, _ int, _, _ time.Duration) ([]Hop, roundStats, error) {
				return nil, roundStats{}, wait("trace", ctx, count)
			}
			returned := make(chan error, 1)
			probeDone := make(chan struct{})
			go func() {
				defer close(probeDone)
				_, err := m.Probe(parent, Target{Host: "example.invalid"}, tc.requested)
				returned <- err
			}()
			defer func() {
				close(abort)
				select {
				case <-probeDone:
				case <-time.After(time.Second):
					t.Error("probe did not join after test cleanup released both operations")
				}
			}()
			for range 2 {
				select {
				case observed := <-started:
					if observed.ctx != parent {
						t.Errorf("%s received a different cycle context", observed.operation)
					}
					if observed.count != tc.want {
						t.Errorf("%s count = %d, want %d", observed.operation, observed.count, tc.want)
					}
				case <-parent.Done():
					t.Fatal("both operations did not reach the start barrier")
				}
			}
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) || len(finished) != 2 {
					t.Fatalf("Probe returned %v with %d operations finished; want parent cancellation and both joined", err, len(finished))
				}
			case <-time.After(time.Second):
				t.Fatal("parent cancellation did not release and join both operations")
			}
		})
	}
}

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

// A direct-echo panic is recovered by the scheduler, but only after MTR has
// joined the trace it started. Otherwise the raw socket and goroutine can
// outlive the cycle whose stack is unwinding.
func TestMTRJoinsTraceBeforePropagatingDirectPanic(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	traceStarted := make(chan struct{})
	releaseTrace := make(chan struct{})
	traceFinished := make(chan struct{})
	m.trace = func(context.Context, string, string, int, int, time.Duration, time.Duration) ([]Hop, roundStats, error) {
		close(traceStarted)
		<-releaseTrace
		close(traceFinished)
		return nil, roundStats{}, nil
	}

	echoStarted := make(chan struct{})
	panicValue := &struct{ name string }{name: "direct echo panic"}
	m.echo = func(context.Context, Target, int) (*Result, error) {
		<-traceStarted
		close(echoStarted)
		panic(panicValue)
	}

	propagated := make(chan any, 1)
	go func() {
		defer func() { propagated <- recover() }()
		_, _ = m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	}()

	<-echoStarted
	select {
	case got := <-propagated:
		close(releaseTrace)
		<-traceFinished
		t.Fatalf("panic %v propagated before the trace operation was released and joined", got)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseTrace)
	select {
	case got := <-propagated:
		if got != panicValue {
			t.Fatalf("propagated panic = %v, want original panic %v", got, panicValue)
		}
	case <-time.After(time.Second):
		t.Fatal("direct-echo panic did not propagate after the trace finished")
	}

	select {
	case <-traceFinished:
	default:
		t.Fatal("direct-echo panic propagated before the trace operation finished")
	}
}
