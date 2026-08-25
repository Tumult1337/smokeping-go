package slave

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster/master"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

type recordingSink struct {
	mu  sync.Mutex
	got []scheduler.Cycle
}

func (s *recordingSink) OnCycle(_ context.Context, c scheduler.Cycle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, c)
}

func (s *recordingSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// masterUnderTest serves a real master.Server whose registry can be replaced,
// which is what a master restart looks like to a slave: the process comes back
// on the same address with an empty registry.
type masterUnderTest struct {
	handler  atomic.Pointer[http.Handler]
	registry atomic.Pointer[master.Registry]
	sink     *recordingSink
	srv      *httptest.Server
	configs  atomic.Int64
}

func newMasterUnderTest(t *testing.T) *masterUnderTest {
	t.Helper()
	m := &masterUnderTest{sink: &recordingSink{}}
	m.restart()
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			m.configs.Add(1)
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1/cluster")
		(*m.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *masterUnderTest) restart() {
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{{Group: "g", Targets: []config.Target{{Name: "t", Probe: "icmp"}}}},
	})
	reg := master.NewRegistry(slog.New(slog.DiscardHandler))
	h := master.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, reg, m.sink, nil).Handler()
	m.registry.Store(reg)
	m.handler.Store(&h)
}

// An upgraded slave must recover on its own after a master restart in the one
// configuration where nothing else would save it: cluster.pull_every "0", which
// starts no refresh goroutine, so there is no /config heartbeat to re-create
// the registry entry the restart dropped. Recovery rides entirely on flushOnce
// re-registering when /cycles answers 403 — and the batch that was in flight
// when the master forgot us must survive it.
func TestSlaveRecoversFromMasterRestartWithoutConfigPull(t *testing.T) {
	m := newMasterUnderTest(t)
	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{
			MasterURL: m.srv.URL, Token: "tok", Name: "tokyo-1", PullEvery: "0",
		},
	}, "v9")
	if r.pullEvery != 0 {
		t.Fatalf("pullEvery = %v, want 0 — the no-heartbeat case is not under test", r.pullEvery)
	}

	ctx := context.Background()
	if err := r.client.Register(ctx); err != nil {
		t.Fatalf("boot register: %v", err)
	}

	m.restart()
	if m.registry.Load().Has("tokyo-1") {
		t.Fatal("restart did not clear the registry")
	}

	r.sink.OnCycle(ctx, scheduler.Cycle{
		// A real timestamp, not the zero value: ingest refuses that as outside
		// [now-MaxCycleAge, now+MaxFutureSkew] and the batch would be dropped
		// as permanently rejected rather than delivered.
		Time:   time.Now(),
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t", Probe: "icmp"}},
		Sent:   5,
	})

	if err := r.flushOnce(ctx); err != nil {
		t.Fatalf("first flush after restart: %v", err)
	}
	if !m.registry.Load().Has("tokyo-1") {
		t.Fatal("the 403 did not drive a re-register")
	}
	if got := r.sink.Len(); got != 1 {
		t.Fatalf("buffered cycles after the refused push = %d, want the batch requeued (1)", got)
	}
	if m.sink.len() != 0 {
		t.Fatalf("master ingested %d cycles from a refused push, want 0", m.sink.len())
	}

	if err := r.flushOnce(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if got := r.sink.Len(); got != 0 {
		t.Fatalf("%d cycles still buffered, want the requeued batch delivered", got)
	}
	if m.sink.len() != 1 {
		t.Fatalf("master ingested %d cycles, want the requeued 1", m.sink.len())
	}
	if got := m.sink.got[0].Source; got != "tokyo-1" {
		t.Fatalf("cycle stamped %q, want tokyo-1", got)
	}
	if n := m.configs.Load(); n != 0 {
		t.Fatalf("%d /config requests — recovery must not depend on a heartbeat this config disables", n)
	}
}
