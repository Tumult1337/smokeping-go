package slave

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
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

	if _, err := r.flushOnce(ctx); err != nil {
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

	if _, err := r.flushOnce(ctx); err != nil {
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

// A re-registration the master refuses permanently must exit, not loop. The
// same range made handleRegister answer 400 for every Touch refusal except
// registry-full, and registerForever and pullConfigInitial both exit on
// ErrRejected — this third caller of Register did not, so an oversized
// advertise (or any 4xx an intermediary injects) requeued the batch and
// retried forever: the ring head-of-line blocks and drop-oldest eats the live
// cycles behind it while the process reports healthy.
func TestPermanentlyRejectedReRegisterExitsRatherThanLooping(t *testing.T) {
	m := newMasterUnderTest(t)
	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{
			MasterURL: m.srv.URL, Token: "tok", Name: "tokyo-1", PullEvery: "0",
			Advertise: strings.Repeat("a", 300),
		},
	}, "v9")

	ctx := context.Background()
	r.sink.OnCycle(ctx, scheduler.Cycle{
		Time:   time.Now(),
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t", Probe: "icmp"}},
		Sent:   5,
	})

	// The master has never seen this slave, so /cycles answers 403 and the
	// re-register that follows is refused for the oversized advertise.
	_, err := r.flushOnce(ctx)
	if err == nil {
		t.Fatal("flushOnce returned nil on a permanently refused re-registration — the runner keeps going and pushes nothing forever")
	}
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("flushOnce returned %v, want ErrRejected so the process exits with the master's own message", err)
	}
	if got := r.sink.Len(); got != 1 {
		t.Fatalf("buffered cycles = %d, want the batch requeued (1) — the process is exiting, not discarding data", got)
	}
}

// ErrRejected is every 4xx bar 401/403/404 and the retryable set, and any
// intermediary on the path can produce one — a client_max_body_size, a
// header-buffer limit or a routing change answering 413/431/405, and nginx
// maps its internal 494 and 497 to a plain 400. Making the whole class fatal
// crash-looped the fleet on a proxy misconfiguration, which is the failure
// client.go already records for 403 — and keying on 400 alone reproduced it,
// because that is exactly the status those proxies rewrite to. Only a refusal
// carrying the master's own marker is fatal.
func TestOnlyTheMastersOwnMarkedRefusalIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		marked bool
		fatal  bool
	}{
		{"master refuses the request", http.StatusBadRequest, true, true},
		{"proxy rewrites 494 to 400", http.StatusBadRequest, false, false},
		{"proxy method not allowed", http.StatusMethodNotAllowed, false, false},
		{"proxy body too large", http.StatusRequestEntityTooLarge, false, false},
		{"proxy headers too large", http.StatusRequestHeaderFieldsTooLarge, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.marked {
					w.Header().Set(cluster.HeaderRefusal, cluster.RefusalPermanent)
				}
				http.Error(w, "nope", tc.code)
			}))
			t.Cleanup(srv.Close)
			r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
				Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1", PullEvery: "0"},
			}, "v9")

			err := r.client.Register(context.Background())
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("%d: %v, want ErrRejected", tc.code, err)
			}
			if got := errors.Is(err, ErrMasterRefused); got != tc.fatal {
				t.Fatalf("%d marked=%v: fatal=%v, want %v — only the master's own verdict may exit the process",
					tc.code, tc.marked, got, tc.fatal)
			}
		})
	}
}

// The batch is dropped either way — requeueing head-of-line blocks the ring
// and drop-oldest eats the live cycles behind it — but the message must not
// assert a verdict the master never gave. An unmarked 4xx is equally an nginx
// large_client_header_buffers or client_max_body_size below what this push
// needs, and naming the master sends the operator to the wrong component.
func TestUnmarkedRefusalDoesNotBlameTheMaster(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marked  bool
		wantHas string
		wantNot string
	}{
		{"master's own verdict", true, "master permanently rejected", "proxy"},
		{"unmarked, could be a proxy", false, "proxy", "master permanently rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.marked {
					w.Header().Set(cluster.HeaderRefusal, cluster.RefusalPermanent)
				}
				http.Error(w, "nope", http.StatusBadRequest)
			}))
			t.Cleanup(srv.Close)

			var buf bytes.Buffer
			r := NewRunner(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})),
				&config.Config{Cluster: &config.Cluster{
					MasterURL: srv.URL, Token: "tok", Name: "tokyo-1", PullEvery: "0",
				}}, "v9")

			ctx := context.Background()
			r.sink.OnCycle(ctx, scheduler.Cycle{
				Time:   time.Now(),
				Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t", Probe: "icmp"}},
				Sent:   5,
			})
			_, _ = r.flushOnce(ctx)

			log := buf.String()
			if !strings.Contains(log, tc.wantHas) {
				t.Fatalf("log %q does not contain %q", log, tc.wantHas)
			}
			if strings.Contains(log, tc.wantNot) {
				t.Fatalf("log %q contains %q — it names a component that may not be at fault", log, tc.wantNot)
			}
			if got := r.sink.Len(); got != 0 {
				t.Fatalf("buffered = %d, want the batch dropped either way", got)
			}
		})
	}
}
