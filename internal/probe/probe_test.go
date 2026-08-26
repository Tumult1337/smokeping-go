package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	p := NewTCP("tcp", time.Second)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: ln.Addr().String()}, 3)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.LossCount != 0 {
		t.Errorf("LossCount = %d, want 0", res.LossCount)
	}
	if len(res.RTTs) != 3 {
		t.Errorf("got %d rtts, want 3", len(res.RTTs))
	}
}

func TestTCPProbeUnreachable(t *testing.T) {
	p := NewTCP("tcp", 100*time.Millisecond)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 127.0.0.1:1 is reliably refused/unreachable.
	res, err := p.Probe(ctx, Target{Host: "127.0.0.1:1"}, 2)
	if err == nil {
		t.Fatalf("probe: expected error when all dials fail")
	}
	if res.LossCount == 0 {
		t.Errorf("expected some loss, got none")
	}
}

// The RTT slice is sized by count, which is caller-supplied, so a cancelled
// cycle must return before paying that allocation.
func TestTCPProbeCancelledContextAllocatesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := NewTCP("tcp", time.Second).Probe(ctx, Target{Host: "127.0.0.1:9"}, 1<<20)
	if err == nil {
		t.Fatal("a cancelled context must return an error")
	}
	if res.Sent != 0 {
		t.Fatalf("Sent = %d, want 0 attempts on a cancelled cycle", res.Sent)
	}
	if cap(res.RTTs) != 0 {
		t.Fatalf("a cancelled cycle pre-allocated %d rtt slots", cap(res.RTTs))
	}
}

func TestHTTPProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	p := NewHTTP("http", 2*time.Second, false)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pass a count above maxHTTPRequests to verify the cap kicks in: HTTP is
	// deliberately limited to 1-2 requests per cycle regardless of cfg.Pings.
	res, err := p.Probe(ctx, Target{URL: ts.URL}, 5)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.LossCount != 0 {
		t.Errorf("LossCount = %d, want 0", res.LossCount)
	}
	if len(res.RTTs) != maxHTTPRequests {
		t.Errorf("got %d rtts, want %d (capped)", len(res.RTTs), maxHTTPRequests)
	}
	if res.Sent != maxHTTPRequests {
		t.Errorf("Sent = %d, want %d", res.Sent, maxHTTPRequests)
	}
}

func TestHTTPProbe5xxIsLoss(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := NewHTTP("http", time.Second, false)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{URL: ts.URL}, 2)
	if err == nil {
		t.Fatalf("probe: expected error when all requests fail")
	}
	if res.LossCount != 2 {
		t.Errorf("LossCount = %d, want 2", res.LossCount)
	}
}

func TestDNSProbeLocalhost(t *testing.T) {
	p := NewDNS("dns", 2*time.Second)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "localhost"}, 2)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.LossCount != 0 {
		t.Errorf("LossCount = %d, want 0 (got rtts=%d)", res.LossCount, len(res.RTTs))
	}
}

func TestBuildRejectsUnschedulablePingBudget(t *testing.T) {
	icmp := map[string]config.Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}}

	t.Run("deployed config passes unchanged", func(t *testing.T) {
		if _, err := Build(icmp, 20*time.Second, 10); err != nil {
			t.Fatalf("10 pings at 20s derives 1.82s and must build: %v", err)
		}
	})

	t.Run("30 pings at 20s passes on a derived deadline", func(t *testing.T) {
		if _, err := Build(icmp, 20*time.Second, 30); err != nil {
			t.Fatalf("30 pings at 20s derives 473ms, above the floor: %v", err)
		}
	})

	t.Run("spacing alone exceeding the interval is refused", func(t *testing.T) {
		_, err := Build(icmp, 20*time.Second, 200)
		if err == nil {
			t.Fatal("200 pings owes 39.8s of spacing against a 20s interval and must be refused")
		}
		if !strings.Contains(err.Error(), "per-ping budget") {
			t.Fatalf("error must name the per-ping budget, got: %v", err)
		}
	})

	t.Run("budget just below the floor is refused", func(t *testing.T) {
		// Spacing owed at pings=40 is 39*200ms = 7800ms, so interval=9760ms
		// derives exactly 49ms — one below the 50ms floor.
		if _, err := Build(icmp, 9760*time.Millisecond, 40); err == nil {
			t.Fatal("a 49ms derived budget is below the floor and must be refused")
		}
	})

	t.Run("budget exactly at the floor is accepted", func(t *testing.T) {
		// (9800ms - 7800ms)/40 = exactly 50ms.
		if _, err := Build(icmp, 9800*time.Millisecond, 40); err != nil {
			t.Fatalf("a 50ms derived budget is at the floor and must be accepted: %v", err)
		}
	})

	t.Run("non-positive pings is refused rather than dividing by zero", func(t *testing.T) {
		if _, err := Build(icmp, 20*time.Second, 0); err == nil {
			t.Fatal("pings=0 must be refused")
		}
	})

	t.Run("absurd pings is refused rather than overflowing the spacing product", func(t *testing.T) {
		// time.Duration(pings-1) * 200ms overflows int64 here and wraps
		// negative, which without a prior bound derives a passing 168.9ms.
		if _, err := Build(icmp, 20*time.Second, 50_000_000_000); err == nil {
			t.Fatal("pings=50e9 overflows the spacing product and must be refused")
		}
	})

	t.Run("a count past the echo sequence space is refused however long the interval", func(t *testing.T) {
		// The spacing bound scales with interval, so a long enough interval
		// admits a count that leaves echoBaseSeq no room above the TTL walk.
		if _, err := Build(icmp, 24*time.Hour, 100_000); err == nil {
			t.Fatal("100k pings cannot be placed clear of the walk and must be refused")
		}
	})

	t.Run("the floor keys on probe type, not on the map key", func(t *testing.T) {
		named := map[string]config.Probe{"wan": {Type: "icmp", Timeout: 2 * time.Second}}
		_, err := Build(named, 20*time.Second, 200)
		if err == nil {
			t.Fatal(`an icmp probe named "wan" bypassed the budget floor`)
		}
		// Attributing a schedule failure to a probe names whichever one Go's
		// randomized map iteration reached first, so it must name neither.
		if strings.Contains(err.Error(), "wan") {
			t.Fatalf("schedule error is attributed to a probe name: %v", err)
		}
	})

	t.Run("pings past the ingest rtt ceiling is refused without an icmp probe", func(t *testing.T) {
		probes := map[string]config.Probe{"mtr": {Type: "mtr", Timeout: 2 * time.Second}}
		if _, err := Build(probes, 5*time.Second, 100_000); err == nil {
			t.Fatal("a scheduler error path stamps Sent=pings, so 100k pings must be refused for every probe map")
		}
	})

	t.Run("the floor is scoped to icmp probes", func(t *testing.T) {
		for _, typ := range []string{"mtr", "tcp", "http", "dns"} {
			probes := map[string]config.Probe{typ: {Type: typ, Timeout: 2 * time.Second}}
			if _, err := Build(probes, 20*time.Second, 200); err != nil {
				t.Fatalf("%s probe must not be subject to the icmp ping budget: %v", typ, err)
			}
		}
	})
}

func swapLookupIPAddr(t *testing.T, fn func(context.Context, string, string) ([]net.IP, error)) {
	t.Helper()
	orig := lookupIPFn
	lookupIPFn = fn
	t.Cleanup(func() { lookupIPFn = orig })
}

// blackholedLookup behaves like the real resolver against a dead nameserver:
// it honors the context and otherwise takes far longer than any cycle.
func blackholedLookup(ctx context.Context, network, host string) ([]net.IP, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}
}

// The trace goroutine is joined on every return path, so a resolve that does
// not honor the cycle's context blocks shutdown and every SIGHUP rebuild for
// the resolver's own timeout — tens of seconds per hostname-addressed target.
func TestTraceHopsHonorsCancelledContextDuringResolve(t *testing.T) {
	swapLookupIPAddr(t, blackholedLookup)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _, err := traceHops(ctx, "blackhole.example", "", 1, 1, time.Millisecond, 0)
	if err == nil {
		t.Fatal("a cancelled cycle must not resolve successfully")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("resolve outlived the cancelled cycle by %v", elapsed)
	}
}

func TestICMPProbeHonorsCancelledContextDuringResolve(t *testing.T) {
	swapLookupIPAddr(t, blackholedLookup)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := NewICMP("icmp", time.Second, true).Probe(ctx, Target{Host: "blackhole.example"}, 1); err == nil {
		t.Fatal("a cancelled cycle must not resolve successfully")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("resolve outlived the cancelled cycle by %v", elapsed)
	}
}

// resolveIPAddr must keep net.ResolveIPAddr's selection: family networks
// filter, a bare "ip" prefers IPv4 for a hostname and falls back across
// families rather than failing.
func TestResolveIPAddrSelectsLikeNetResolveIPAddr(t *testing.T) {
	v4 := net.IPv4(192, 0, 2, 9)
	v6 := net.ParseIP("2001:db8::1")
	// The real resolver answers per family; a fake that ignores the network
	// cannot tell a family-scoped query from a dual one filtered afterwards.
	byFamily := func(_ context.Context, network, _ string) ([]net.IP, error) {
		switch network {
		case "ip4":
			return []net.IP{v4}, nil
		case "ip6":
			return []net.IP{v6}, nil
		default:
			return []net.IP{v6, v4}, nil
		}
	}
	swapLookupIPAddr(t, byFamily)
	ctx := context.Background()

	if got, err := resolveIPAddr(ctx, "ip4", "dual.example"); err != nil || !got.IP.Equal(v4) {
		t.Fatalf("ip4: got %v, %v", got, err)
	}
	if got, err := resolveIPAddr(ctx, "ip6", "dual.example"); err != nil || !got.IP.Equal(v6) {
		t.Fatalf("ip6: got %v, %v", got, err)
	}
	if got, err := resolveIPAddr(ctx, "ip", "dual.example"); err != nil || !got.IP.Equal(v4) {
		t.Fatalf("ip must prefer IPv4 for a hostname: got %v, %v", got, err)
	}

	swapLookupIPAddr(t, func(_ context.Context, network, _ string) ([]net.IP, error) {
		if network == "ip4" {
			return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
		}
		return []net.IP{v6}, nil
	})
	if got, err := resolveIPAddr(ctx, "ip", "six.example"); err != nil || !got.IP.Equal(v6) {
		t.Fatalf("ip must fall back to the only family: got %v, %v", got, err)
	}
	if _, err := resolveIPAddr(ctx, "ip4", "six.example"); err == nil {
		t.Fatal("ip4 with only AAAA answers must fail like net.ResolveIPAddr")
	}
}

// A pinned family must query only that family's record. Filtering a dual
// lookup instead makes an unrelated blackholed AAAA path fail every v4-pinned
// cycle, which reads as total loss on a target that is answering.
func TestResolveIPAddrQueriesOnlyThePinnedFamily(t *testing.T) {
	v4 := net.IPv4(192, 0, 2, 9)
	var asked []string
	swapLookupIPAddr(t, func(ctx context.Context, network, _ string) ([]net.IP, error) {
		asked = append(asked, network)
		if network == "ip6" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []net.IP{v4}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	got, err := resolveIPAddr(ctx, "ip4", "dual.example")
	if err != nil || !got.IP.Equal(v4) {
		t.Fatalf("v4-pinned resolve got %v, %v — a dead AAAA path must not reach it", got, err)
	}
	for _, network := range asked {
		if network != "ip4" {
			t.Fatalf("resolver asked for %q on a v4-pinned target, want ip4 only", network)
		}
	}
}

// The destination side of the walk's peer check comes from here, so the zone
// check is inert unless resolution keeps the zone the operator wrote.
func TestResolveIPAddrKeepsIPv6Zone(t *testing.T) {
	got, err := resolveIPAddr(context.Background(), "ip6", "fe80::1%eth0")
	if err != nil {
		t.Fatalf("resolve fe80::1%%eth0: %v", err)
	}
	if got.Zone != "eth0" || !got.IP.Equal(net.ParseIP("fe80::1")) {
		t.Fatalf("resolved to %v, want fe80::1%%eth0", got)
	}
}

// The invariant that actually broke: a schedule config.Validate stores is one
// probe.Build must accept, or the store serves every slave a config that fails
// at its next restart, hours after the edit looked green.
func TestValidateAndBuildAgreeOnPingSchedule(t *testing.T) {
	probes := map[string]config.Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}}

	for _, tc := range []struct {
		interval time.Duration
		pings    int
	}{
		{20 * time.Second, 10},
		{20 * time.Second, 30},
		{20 * time.Second, 100},
		{20 * time.Second, 101},
		{20 * time.Second, 102},
		{20 * time.Second, 120},
		{20 * time.Second, 200},
		{9760 * time.Millisecond, 40},
		{9800 * time.Millisecond, 40},
		{time.Second, 1},
		{100 * time.Millisecond, 3},
		{24 * time.Hour, 100_000},
		{20 * time.Second, 0},
		{0, 10},
	} {
		cfg := &config.Config{
			Interval: tc.interval,
			Pings:    tc.pings,
			Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
			Probes:   probes,
		}
		validateErr := cfg.Validate()
		_, buildErr := Build(probes, tc.interval, tc.pings)
		if (validateErr == nil) != (buildErr == nil) {
			t.Errorf("interval=%s pings=%d: Validate err = %v, Build err = %v", tc.interval, tc.pings, validateErr, buildErr)
		}
	}
}
