package api

import (
	"context"
	"fmt"
	"maps"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReverseDNSCachesPositiveAndNegativeResults(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	cache := newReverseDNSCache(func(_ context.Context, addr string) ([]string, error) {
		calls.Add(1)
		if addr == "192.0.2.1" {
			return []string{"edge.example.net."}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	})
	cache.now = func() time.Time { return now }

	want := map[string]string{"192.0.2.1": "edge.example.net"}
	for range 2 {
		got := cache.hostnames(context.Background(), []string{"192.0.2.1", "192.0.2.2"})
		if !maps.Equal(got, want) {
			t.Fatalf("hostnames = %#v, want %#v", got, want)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("lookup calls = %d, want one positive and one negative", got)
	}
}

func TestReverseDNSPositiveEntryExpiresAfter24Hours(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	cache := newReverseDNSCache(func(context.Context, string) ([]string, error) {
		call := calls.Add(1)
		return []string{fmt.Sprintf("edge-%d.example.net.", call)}, nil
	})
	cache.now = func() time.Time { return now }

	if got := cache.hostnames(context.Background(), []string{"192.0.2.1"}); got["192.0.2.1"] != "edge-1.example.net" {
		t.Fatalf("initial result = %#v", got)
	}
	now = now.Add(reverseDNSPositiveTTL - time.Nanosecond)
	if got := cache.hostnames(context.Background(), []string{"192.0.2.1"}); got["192.0.2.1"] != "edge-1.example.net" {
		t.Fatalf("live result = %#v", got)
	}
	now = now.Add(2 * time.Nanosecond)
	if got := cache.hostnames(context.Background(), []string{"192.0.2.1"}); got["192.0.2.1"] != "edge-2.example.net" {
		t.Fatalf("refreshed result = %#v", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("lookup calls = %d, want 2", got)
	}
}

func TestReverseDNSNegativeEntryExpiresAfter5Minutes(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	cache := newReverseDNSCache(func(_ context.Context, addr string) ([]string, error) {
		if calls.Add(1) == 1 {
			return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
		}
		return []string{"appeared.example.net."}, nil
	})
	cache.now = func() time.Time { return now }

	if got := cache.hostnames(context.Background(), []string{"192.0.2.1"}); len(got) != 0 {
		t.Fatalf("negative result = %#v, want empty", got)
	}
	now = now.Add(reverseDNSNegativeTTL - time.Nanosecond)
	if got := cache.hostnames(context.Background(), []string{"192.0.2.1"}); len(got) != 0 {
		t.Fatalf("live negative result = %#v, want empty", got)
	}
	now = now.Add(2 * time.Nanosecond)
	if got := cache.hostnames(context.Background(), []string{"192.0.2.1"}); got["192.0.2.1"] != "appeared.example.net" {
		t.Fatalf("refreshed result = %#v", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("lookup calls = %d, want 2", got)
	}
}

func TestReverseDNSCanonicalizesMappedIPv4AndDropsZones(t *testing.T) {
	var mu sync.Mutex
	var lookedUp []string
	cache := newReverseDNSCache(func(_ context.Context, addr string) ([]string, error) {
		mu.Lock()
		lookedUp = append(lookedUp, addr)
		mu.Unlock()
		return []string{"router.example.net."}, nil
	})

	got := cache.hostnames(context.Background(), []string{
		"::ffff:192.0.2.1",
		"192.0.2.1",
		"fe80::1%eth0",
		"fe80::1%2",
		"",
		"redacted",
	})
	want := map[string]string{
		"::ffff:192.0.2.1": "router.example.net",
		"192.0.2.1":        "router.example.net",
		"fe80::1%eth0":     "router.example.net",
		"fe80::1%2":        "router.example.net",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("hostnames = %#v, want %#v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lookedUp) != 2 {
		t.Fatalf("lookups = %#v, want two canonical addresses", lookedUp)
	}
	seen := map[string]bool{}
	for _, addr := range lookedUp {
		seen[addr] = true
	}
	if !seen["192.0.2.1"] || !seen["fe80::1"] {
		t.Fatalf("lookups = %#v, want canonical 192.0.2.1 and fe80::1", lookedUp)
	}
}

func TestReverseDNSEvictsTheLeastRecentlyUsedCompletedEntry(t *testing.T) {
	var calls atomic.Int64
	cache := newReverseDNSCache(func(_ context.Context, addr string) ([]string, error) {
		calls.Add(1)
		return []string{"name-" + addr}, nil
	})
	cache.maxEntries = 2

	cache.hostnames(context.Background(), []string{"192.0.2.1", "192.0.2.2"})
	cache.hostnames(context.Background(), []string{"192.0.2.1"})
	cache.hostnames(context.Background(), []string{"192.0.2.3"})
	cache.hostnames(context.Background(), []string{"192.0.2.2"})

	if got := calls.Load(); got != 4 {
		t.Fatalf("lookup calls = %d, want 4 after LRU eviction", got)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if got := len(cache.entries); got != 2 {
		t.Fatalf("cache entries = %d, want 2", got)
	}
}

func TestReverseDNSRejectsInvalidPTRNames(t *testing.T) {
	tests := []struct {
		name string
		bad  string
	}{
		{name: "empty", bad: ""},
		{name: "invalid UTF-8", bad: string([]byte{0xff})},
		{name: "newline", bad: "bad\nname."},
		{name: "DEL", bad: "bad\x7fname."},
		{name: "too long", bad: string(make([]byte, 254))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newReverseDNSCache(func(context.Context, string) ([]string, error) {
				return []string{tt.bad, "usable.example.net."}, nil
			})
			got := cache.hostnames(context.Background(), []string{"192.0.2.1"})
			if got["192.0.2.1"] != "usable.example.net" {
				t.Fatalf("hostnames = %#v, want later usable name", got)
			}
		})
	}
}

func TestReverseDNSCoalescesOneAddress(t *testing.T) {
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var calls atomic.Int64
	cache := newReverseDNSCache(func(ctx context.Context, _ string) ([]string, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return []string{"edge.example.net."}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	addr, ok := canonicalHopAddr("192.0.2.1")
	if !ok {
		t.Fatal("test address did not parse")
	}

	const callers = 16
	results := make(chan string, callers)
	go func() { results <- cache.resolve(context.Background(), addr) }()
	<-entered
	for range callers - 1 {
		go func() { results <- cache.resolve(context.Background(), addr) }()
	}

	duplicate := false
	select {
	case <-entered:
		duplicate = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range callers {
		if got := <-results; got != "edge.example.net" {
			t.Fatalf("joined result = %q, want edge.example.net", got)
		}
	}
	if duplicate || calls.Load() != 1 {
		t.Fatalf("lookup calls = %d, want one coalesced call", calls.Load())
	}
}

func TestReverseDNSRunsAtMostEightLookupsGlobally(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, reverseDNSMaxConcurrent+1)
	var active atomic.Int64
	var peak atomic.Int64
	cache := newReverseDNSCache(func(ctx context.Context, addr string) ([]string, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			return []string{"name-" + addr}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	ips := make([]string, reverseDNSMaxConcurrent+1)
	for i := range ips {
		ips[i] = fmt.Sprintf("192.0.2.%d", i+1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		cache.hostnames(ctx, ips)
		close(done)
	}()

	for i := 0; i < reverseDNSMaxConcurrent; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			<-done
			t.Fatalf("only %d lookups entered concurrently, want %d", peak.Load(), reverseDNSMaxConcurrent)
		}
	}
	select {
	case <-entered:
		close(release)
		<-done
		t.Fatal("ninth resolver call entered before a global slot was released")
	default:
	}
	close(release)
	<-done
	if got := peak.Load(); got != reverseDNSMaxConcurrent {
		t.Fatalf("peak resolver calls = %d, want %d", got, reverseDNSMaxConcurrent)
	}
}

func TestReverseDNSSchedulesAtMost128MissesButReturnsAllCacheHits(t *testing.T) {
	var calls atomic.Int64
	cache := newReverseDNSCache(func(_ context.Context, addr string) ([]string, error) {
		calls.Add(1)
		return []string{"name-" + addr}, nil
	})
	const cachedIP = "203.0.113.1"
	if got := cache.hostnames(context.Background(), []string{cachedIP}); got[cachedIP] == "" {
		t.Fatalf("prewarm result = %#v", got)
	}
	calls.Store(0)

	ips := make([]string, 0, reverseDNSMaxMissesPerResponse+2)
	for i := 1; i <= reverseDNSMaxMissesPerResponse+1; i++ {
		ips = append(ips, fmt.Sprintf("198.51.100.%d", i))
	}
	ips = append(ips, cachedIP)
	got := cache.hostnames(context.Background(), ips)

	if got[cachedIP] == "" {
		t.Fatalf("cached address after miss cap was omitted: %#v", got)
	}
	if calls.Load() != reverseDNSMaxMissesPerResponse {
		t.Fatalf("new lookups = %d, want %d", calls.Load(), reverseDNSMaxMissesPerResponse)
	}
	if _, ok := got["198.51.100.129"]; ok {
		t.Fatalf("address past miss cap was resolved: %#v", got)
	}
}

func TestReverseDNSReturnsCompletedNamesAtResponseDeadline(t *testing.T) {
	cache := newReverseDNSCache(func(ctx context.Context, addr string) ([]string, error) {
		if addr == "192.0.2.2" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []string{"name-" + addr}, nil
	})
	cache.responseTimeout = 50 * time.Millisecond

	got := cache.hostnames(context.Background(), []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"})
	want := map[string]string{
		"192.0.2.1": "name-192.0.2.1",
		"192.0.2.3": "name-192.0.2.3",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("partial hostnames = %#v, want %#v", got, want)
	}
}

func TestReverseDNSCancellationStopsActiveLookups(t *testing.T) {
	entered := make(chan struct{})
	stopped := make(chan struct{})
	cache := newReverseDNSCache(func(ctx context.Context, _ string) ([]string, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan map[string]string)
	go func() {
		done <- cache.hostnames(ctx, []string{"192.0.2.1"})
	}()
	<-entered
	cancel()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("active lookup did not observe cancellation")
	}
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("cancelled result = %#v, want empty", got)
		}
	case <-time.After(time.Second):
		t.Fatal("hostnames did not return after cancellation")
	}
}
