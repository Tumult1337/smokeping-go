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

	t.Run("the floor keys on probe type, not on the map key", func(t *testing.T) {
		named := map[string]config.Probe{"wan": {Type: "icmp", Timeout: 2 * time.Second}}
		if _, err := Build(named, 20*time.Second, 200); err == nil {
			t.Fatal(`an icmp probe named "wan" bypassed the budget floor`)
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
