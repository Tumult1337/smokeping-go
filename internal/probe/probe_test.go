package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
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
	if err != nil && err != context.DeadlineExceeded {
		t.Fatalf("probe: %v", err)
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

	p := NewHTTP("http", 2*time.Second)
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

	p := NewHTTP("http", time.Second)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{URL: ts.URL}, 2)
	if err != nil {
		t.Fatalf("probe: %v", err)
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
