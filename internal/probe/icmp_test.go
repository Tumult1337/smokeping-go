package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

// requireICMPSocket probes the capability directly rather than inferring it
// from Probe's return: a nil *Result also means Probe bailed before its echo
// loop, which would silently turn every regression test below into a green
// SKIP.
func requireICMPSocket(t *testing.T) {
	t.Helper()
	c, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		if c, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err != nil {
			t.Skipf("no unprivileged ping socket and no CAP_NET_RAW: %v", err)
		}
	}
	_ = c.Close()
}

// NoTrace must suppress the opportunistic path walk without touching echo
// statistics. This exercises the real echo path, which needs either an
// unprivileged ping socket (a permissive net.ipv4.ping_group_range) or
// CAP_NET_RAW — neither guaranteed on a CI runner, so we skip rather than fail
// when the environment cannot send ICMP at all. The gating logic itself is
// verified environment-independently by TestICMPProbeNoTraceGatesTraceCall;
// this test adds the integration check that a real probe with NoTrace set
// produces no hops and leaves echo stats intact.
func TestICMPProbeNoTraceSkipsHops(t *testing.T) {
	p := NewICMP("icmp", time.Second, true)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 1)
	if err != nil {
		t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
	}

	// The NoTrace contract holds whenever a probe ran, regardless of whether
	// the loopback echo happened to land: no hops, ever.
	if len(res.Hops) != 0 {
		t.Fatalf("got %d hops, want 0 with NoTrace set", len(res.Hops))
	}
	// The echo-unaffected assertion needs a completed echo. A socket that
	// opened but dropped the loopback reply (constrained sandbox) can't speak
	// to whether NoTrace touched echo stats, so skip that half rather than
	// flake on it.
	if len(res.RTTs) == 0 {
		t.Skip("loopback echo did not complete; skipping echo-unaffected assertion")
	}
	if len(res.RTTs) != 1 || res.LossCount != 0 {
		t.Fatalf("echo stats affected by NoTrace: rtts=%d lossCount=%d", len(res.RTTs), res.LossCount)
	}
}

// TestICMPProbeNoTraceGatesTraceCall replaces the trace seam with a spy so the
// gating logic is exercised directly — this is what discriminates a correct
// guard from a deleted or inverted one, which TestICMPProbeNoTraceSkipsHops
// cannot (without raw-socket privilege traceHops fails regardless of the flag).
//
// The spy isolates the trace call but NOT the echo: Probe opens an ICMP echo
// socket before it ever reaches the trace step, so this still needs an
// unprivileged ping socket or CAP_NET_RAW and skips when neither is available.
// The gating is thus verified wherever ICMP works (local dev, production); a
// locked-down CI runner skips rather than fails.
func TestICMPProbeNoTraceGatesTraceCall(t *testing.T) {
	t.Run("NoTrace set: trace is not called", func(t *testing.T) {
		p := NewICMP("icmp", time.Second, true)
		p.spacing = time.Millisecond
		called := false
		p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
			called = true
			return []Hop{{Index: 1, IP: "10.0.0.1"}}, true, nil
		}

		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 1)
		if err != nil {
			t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
		}
		if called {
			t.Fatal("trace func called despite NoTrace set")
		}
		if len(res.Hops) != 0 {
			t.Fatalf("got %d hops, want 0 with NoTrace set", len(res.Hops))
		}
	})

	t.Run("NoTrace clear: trace is called and its hops flow through", func(t *testing.T) {
		p := NewICMP("icmp", time.Second, false)
		p.spacing = time.Millisecond
		called := false
		want := []Hop{{Index: 1, IP: "10.0.0.1"}, {Index: 2, IP: "10.0.0.2"}}
		p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
			called = true
			return want, true, nil
		}

		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 1)
		if err != nil {
			t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
		}
		if !called {
			t.Fatal("trace func not called with NoTrace clear")
		}
		if len(res.Hops) != len(want) {
			t.Fatalf("got %d hops, want %d from trace func", len(res.Hops), len(want))
		}
	})
}

// The TTL walk must not be starved by the echo batch it shares a cycle
// deadline with. Run after the batch, a loss-saturated batch returned at
// Probe's early ctx.Err() check and the walk never ran at all, so hop rows
// vanished on exactly the lossy cycles a traceroute exists for.
func TestICMPProbeTracesDespiteExhaustedEchoBudget(t *testing.T) {
	requireICMPSocket(t)
	p := NewICMP("icmp", time.Second, false)
	// Spacing alone outruns the cycle context below, so the echo loop is
	// guaranteed to exit on cancellation rather than completing.
	p.spacing = 50 * time.Millisecond

	var entered bool
	var entryErr error
	want := []Hop{{Index: 1, IP: "10.0.0.1"}, {Index: 2, IP: "10.0.0.2"}}
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		entered, entryErr = true, ctx.Err()
		return want, true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 20)
	if res == nil {
		t.Fatalf("Probe returned no result: %v", err)
	}
	if !entered {
		t.Fatal("trace never ran: the echo batch consumed the whole cycle")
	}
	if entryErr != nil {
		t.Fatalf("trace entered with an already-expired context: %v", entryErr)
	}
	if len(res.Hops) != len(want) {
		t.Fatalf("got %d hops, want %d", len(res.Hops), len(want))
	}
}

// Concurrency is the point, not merely ordering: a trace moved *before* the
// echo loop but still called synchronously would satisfy every "the trace ran"
// assertion while restoring the additive echo+trace cycle cost this change
// exists to remove. Assert the cycle costs max(echo, trace), not their sum.
func TestICMPProbeRunsTraceConcurrentlyWithEchoBatch(t *testing.T) {
	requireICMPSocket(t)
	p := NewICMP("icmp", time.Second, false)
	// 20 loopback pings at 20ms spacing is ~380ms of echo batch.
	p.spacing = 20 * time.Millisecond

	const traceDur = 200 * time.Millisecond
	want := []Hop{{Index: 1, IP: "10.0.0.1"}}
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		time.Sleep(traceDur)
		return want, true, nil
	}

	start := time.Now()
	res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 20)
	if res == nil {
		t.Fatalf("Probe returned no result: %v", err)
	}
	// Concurrent: ~380ms (the echo batch dominates). Sequential in either
	// order: ~580ms. 500ms separates them with ~120ms of margin each way.
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("Probe took %v: the trace and the echo batch ran sequentially, not concurrently", elapsed)
	}
	if len(res.Hops) != len(want) {
		t.Fatalf("got %d hops, want %d", len(res.Hops), len(want))
	}
}

// Probe owns the trace goroutine and must join it on EVERY exit path. The
// early return in the echo loop is the one that matters: an implementation
// that polls a ready result there but blocks only on the normal return passes
// a happy-path join test while leaking a slow trace and its raw socket.
func TestICMPProbeJoinsSlowTraceOnEarlyReturn(t *testing.T) {
	requireICMPSocket(t)
	p := NewICMP("icmp", time.Second, false)
	p.spacing = 50 * time.Millisecond

	const traceDur = 250 * time.Millisecond
	want := []Hop{{Index: 1, IP: "10.0.0.1"}}
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		time.Sleep(traceDur)
		return want, true, nil
	}

	// The echo loop is cancelled at ~60ms, long before the trace finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 20)
	if res == nil {
		t.Fatalf("Probe returned no result: %v", err)
	}
	if elapsed := time.Since(start); elapsed < traceDur {
		t.Fatalf("Probe returned in %v on the early-return path without joining the trace", elapsed)
	}
	if len(res.Hops) != len(want) {
		t.Fatalf("got %d hops, want %d — trace result dropped on the early-return path", len(res.Hops), len(want))
	}
}

type capturedRecord struct {
	level slog.Level
	text  string
}

type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, capturedRecord{level: r.Level, text: b.String()})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func (h *captureHandler) at(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.level == level {
			out = append(out, r.text)
		}
	}
	return out
}

func captureLogs(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// The walk used to run on the target's goroutine, under the scheduler's
// per-cycle recover; on its own goroutine Go's per-goroutine recovery leaves a
// panic there uncaught and it takes the process down, and the deferred join
// blocks forever because nothing is ever sent. Asserting on the Error record
// rather than on recover() is what discriminates: a test that only checks
// recover() passes either way, since without the fix the binary dies before it
// runs, and it cannot tell an Error-level report from a swallowed Debug one.
func TestICMPProbeContainsTracePanic(t *testing.T) {
	requireICMPSocket(t)
	logs := captureLogs(t)

	p := NewICMP("icmp", time.Second, false)
	p.spacing = time.Millisecond
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		panic("boom in the TTL walk")
	}

	res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 1)
	if res == nil {
		t.Fatalf("Probe returned no result: %v", err)
	}
	if len(res.Hops) != 0 {
		t.Fatalf("got %d hops from a panicking walk, want 0", len(res.Hops))
	}

	errRecords := logs.at(slog.LevelError)
	if len(errRecords) != 1 {
		t.Fatalf("got %d error-level records, want exactly 1: %q", len(errRecords), errRecords)
	}
	for _, want := range []string{"boom in the TTL walk", "probe=icmp", "host=127.0.0.1"} {
		if !strings.Contains(errRecords[0], want) {
			t.Fatalf("error record %q does not name %q", errRecords[0], want)
		}
	}
}

// Both sockets are open at once and a raw ICMP socket receives every echo
// reply on the host, so a sequence number shared with the walk makes sendTTL
// report the target reached at that TTL and truncates the hop list. The
// deeper-walk case is the mutation guard: a bound hardcoded at the default
// ceiling instead of derived from traceRounds/traceMaxTTL fails only there.
func TestICMPEchoBaseSeqDisjointFromTraceWindow(t *testing.T) {
	const draws = 20000

	for _, tc := range []struct {
		name           string
		rounds, maxTTL int
		count          int
	}{
		{"defaults", 3, 30, 20},
		{"deeper walk", 3, 60, 20},
		{"more rounds", 8, 30, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewICMP("icmp", time.Second, false)
			p.traceRounds, p.traceMaxTTL = tc.rounds, tc.maxTTL
			walkMax := (tc.rounds-1)*(tc.maxTTL+1) + tc.maxTTL

			for range draws {
				base := p.echoBaseSeq(tc.count)
				for n := range tc.count {
					seq := (base + n) & 0xffff
					if seq != base+n {
						t.Fatalf("echo seq wrapped: base %d + %d masked to %d", base, n, seq)
					}
					if seq <= walkMax {
						t.Fatalf("echo seq %d (base %d + %d) lands in the walk's range [1,%d]", seq, base, n, walkMax)
					}
				}
			}
		})
	}
}

// pingBudget must honor the configured timeout whenever the schedule can
// afford it and shrink only when it provably cannot — a flat clamp would
// shorten every ping even on cycles with budget to spare.
func TestPingBudget(t *testing.T) {
	const spacing = 200 * time.Millisecond
	tests := []struct {
		name           string
		remainingCycle time.Duration
		remainingPings int
		spacingLeft    time.Duration
		configured     time.Duration
		want           time.Duration
	}{
		{"budget to spare keeps the configured timeout", 18140 * time.Millisecond, 5, 4 * spacing, 2 * time.Second, 2 * time.Second},
		{"tight cycle derives a shorter deadline", 20 * time.Second, 10, 9 * spacing, 2 * time.Second, 1820 * time.Millisecond},
		{"many pings shrink the deadline further", 20 * time.Second, 30, 29 * spacing, 2 * time.Second, 473333333 * time.Nanosecond},
		{"spacing alone exhausts the cycle", 1500 * time.Millisecond, 5, 4 * time.Second, 2 * time.Second, 0},
		{"cycle already overrun", -5 * time.Millisecond, 3, 0, 2 * time.Second, 0},
		{"no pings left", 10 * time.Second, 0, 0, 2 * time.Second, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pingBudget(tc.remainingCycle, tc.remainingPings, tc.spacingLeft, tc.configured)
			if got != tc.want {
				t.Fatalf("pingBudget(%v, %d, %v, %v) = %v, want %v",
					tc.remainingCycle, tc.remainingPings, tc.spacingLeft, tc.configured, got, tc.want)
			}
		})
	}
}

// The budget must be recomputed per ping, not once before the loop. Computing
// it once yields a correct-looking flat 1.82s that still fits the interval and
// still attempts every ping — but a ping that answers fast never returns its
// unused share, so ping 6 below would keep 1.82s instead of recovering the
// full configured 2s. Walks the loop's own arithmetic with no socket.
func TestICMPEchoTimeoutSelfLevels(t *testing.T) {
	p := NewICMP("icmp", 2*time.Second, true)
	p.spacing = 200 * time.Millisecond

	const count = 10
	const interval = 20 * time.Second
	const fastRTT = 12 * time.Millisecond

	var elapsed time.Duration
	got := make([]time.Duration, 0, count)
	for n := range count {
		to := p.echoTimeout(interval-elapsed, count, n)
		got = append(got, to)
		if n < 5 { // first five answer fast, the rest burn their whole budget
			elapsed += fastRTT
		} else {
			elapsed += to
		}
		if n < count-1 {
			elapsed += p.spacing
		}
	}

	if got[0] != 1820*time.Millisecond {
		t.Fatalf("ping 1 budget = %v, want 1.82s ((20s - 9*200ms)/10)", got[0])
	}
	if got[5] != 2*time.Second {
		t.Fatalf("ping 6 budget = %v, want the full configured 2s — fast pings did not return their unused share", got[5])
	}
	if elapsed > interval {
		t.Fatalf("nominal batch total %v overran the %v interval", elapsed, interval)
	}
}

// All-loss is the case that must exactly fill the interval rather than overrun
// it: every ping burns its whole derived budget and the spacing still has to
// fit.
func TestICMPEchoTimeoutFillsIntervalUnderTotalLoss(t *testing.T) {
	p := NewICMP("icmp", 2*time.Second, true)
	p.spacing = 200 * time.Millisecond

	const count = 10
	const interval = 20 * time.Second

	var elapsed time.Duration
	attempts := 0
	for n := range count {
		to := p.echoTimeout(interval-elapsed, count, n)
		if to <= 0 {
			break
		}
		attempts++
		elapsed += to
		if n < count-1 {
			elapsed += p.spacing
		}
	}
	if attempts != count {
		t.Fatalf("attempted %d of %d pings: the batch was truncated instead of fitting the cycle", attempts, count)
	}
	if elapsed != interval {
		t.Fatalf("nominal batch total = %v, want exactly %v", elapsed, interval)
	}
}

// The loop must pass the derived deadline to the send, not the configured
// timeout. The send seam captures what it was handed.
func TestICMPProbePassesDerivedTimeoutToSend(t *testing.T) {
	requireICMPSocket(t)
	p := NewICMP("icmp", 2*time.Second, true) // NoTrace: this is about the echo batch
	p.spacing = 10 * time.Millisecond

	var timeouts []time.Duration
	p.send = func(ctx context.Context, conn *icmp.PacketConn, dst *net.IPAddr, isV6 bool, id, seq int, timeout time.Duration) (time.Duration, error) {
		timeouts = append(timeouts, timeout)
		return 0, errors.New("simulated loss")
	}

	const count = 10
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, count)
	if res == nil {
		t.Fatalf("Probe returned no result: %v", err)
	}
	if len(timeouts) != count {
		t.Fatalf("send called %d times, want %d", len(timeouts), count)
	}
	if res.Sent != count || res.LossCount != count {
		t.Fatalf("Sent=%d LossCount=%d, want %d/%d", res.Sent, res.LossCount, count, count)
	}
	// Derived budget is (1s - 9*10ms)/10 = 91ms, well under the configured 2s.
	if timeouts[0] > 100*time.Millisecond {
		t.Fatalf("first send got %v, want the derived ~91ms rather than the configured 2s", timeouts[0])
	}
}
