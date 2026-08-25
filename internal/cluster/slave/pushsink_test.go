package slave

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

func cycleWithHops(group, name string) scheduler.Cycle {
	return scheduler.Cycle{
		Time:      time.Now(),
		Target:    config.TargetRef{Group: group, Target: config.Target{Name: name, Probe: "icmp"}},
		ProbeName: "icmp",
		Sent:      3,
		Hops: []probe.Hop{
			{Index: 1, IP: "198.51.100.1", Sent: 3},
			{Index: 2, IP: "10.44.0.2", Sent: 3, TargetReply: true},
		},
	}
}

func decodeConfigResp(t *testing.T, raw string) cluster.ClusterConfigResp {
	t.Helper()
	var resp cluster.ClusterConfigResp
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// A master predating the marker blanks only the deepest hop row, so a walk
// that echoed at ttl 2 below a silent ttl 30 would have it serve the slave's
// own address on /hops. The advertisement is what the slave keys on, and its
// absence — an old master's response — must withhold health hops without
// touching ordinary targets.
func TestPushSinkWithholdsHealthHopsFromMarkerBlindMaster(t *testing.T) {
	markerBlind := decodeConfigResp(t, `{"interval":60000000000,"pings":20}`)
	markerAware := decodeConfigResp(t, `{"interval":60000000000,"pings":20,"hop_markers":true}`)
	if markerBlind.HopMarkers {
		t.Fatal("a response without hop_markers must read as no support")
	}
	if !markerAware.HopMarkers {
		t.Fatal("the advertised response must read as supported")
	}

	for _, tc := range []struct {
		name       string
		resp       cluster.ClusterConfigResp
		wantHealth int
	}{
		{"marker-blind master", markerBlind, 0},
		{"marker-aware master", markerAware, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := NewPushSink(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
			sink.SetHopMarkers(tc.resp.HopMarkers)
			sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))
			sink.OnCycle(context.Background(), cycleWithHops("core", "gw"))

			batch := sink.Drain(10)
			if len(batch) != 2 {
				t.Fatalf("drained %d payloads, want 2", len(batch))
			}
			if got := len(batch[0].Hops); got != tc.wantHealth {
				t.Fatalf("health target shipped %d hop rows, want %d: %+v", got, tc.wantHealth, batch[0].Hops)
			}
			if got := len(batch[1].Hops); got != 2 {
				t.Fatalf("ordinary target lost its hops: %+v", batch[1].Hops)
			}
		})
	}
}

// The slave holds the fail-closed state until its first /config pull answers.
func TestPushSinkDefaultsToWithholdingHealthHops(t *testing.T) {
	sink := NewPushSink(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))

	batch := sink.Drain(10)
	if len(batch) != 1 || len(batch[0].Hops) != 0 {
		t.Fatalf("health hops shipped before any advertisement: %+v", batch)
	}
}

// The advertisement has to travel from the pulled config to the sink: without
// this the wire field and the guard are both correct and never joined.
func TestRunnerAppliesHopMarkerAdvertisement(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(log, &config.Config{Cluster: &config.Cluster{Name: "s1", MasterURL: "http://master.invalid", Token: "t"}}, "test")

	r.applyConfig(decodeConfigResp(t, `{"interval":60000000000,"pings":20}`))
	r.sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))
	if batch := r.sink.Drain(10); len(batch) != 1 || len(batch[0].Hops) != 0 {
		t.Fatalf("marker-blind advertisement not applied: %+v", batch)
	}

	r.applyConfig(decodeConfigResp(t, `{"interval":60000000000,"pings":20,"hop_markers":true}`))
	r.sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))
	if batch := r.sink.Drain(10); len(batch) != 1 || len(batch[0].Hops) != 2 {
		t.Fatalf("marker-aware advertisement not applied: %+v", batch)
	}
}

// The master identifies a pushing slave from X-Slave-Name, falling back to
// batch.Source. Sending the header on /cycles makes the two agree instead of
// leaving the master to trust a body field alone.
func TestPushCyclesSendsSlaveIdentityHeaders(t *testing.T) {
	var gotName, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get("X-Slave-Name")
		gotVersion = r.Header.Get("X-Slave-Version")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "tokyo-1", "v9", "")
	if err := c.PushCycles(context.Background(), cluster.CycleBatch{Source: "tokyo-1"}); err != nil {
		t.Fatalf("PushCycles: %v", err)
	}
	if gotName != "tokyo-1" {
		t.Errorf("X-Slave-Name = %q, want %q", gotName, "tokyo-1")
	}
	if gotVersion != "v9" {
		t.Errorf("X-Slave-Version = %q, want %q", gotVersion, "v9")
	}
}

// A master that no longer knows this slave — restarted, or swept — refuses
// the push with 403. Dropping the batch would lose data and, with
// cluster.pull_every "0", the slave would never re-register and would stay
// refused for the life of the process. It re-registers and keeps the batch.
func TestFlushOnceReregistersAndRequeuesWhenUnregistered(t *testing.T) {
	var registers int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			registers++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ack":true}`))
			return
		}
		http.Error(w, "unregistered slave: POST /register first", http.StatusForbidden)
	}))
	defer srv.Close()

	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1"},
	}, "v9")
	r.sink.OnCycle(context.Background(), scheduler.Cycle{
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t"}},
	})

	if err := r.flushOnce(context.Background()); err != nil {
		t.Fatalf("flushOnce: %v", err)
	}
	if registers != 1 {
		t.Errorf("register attempts = %d, want 1", registers)
	}
	if got := r.sink.Len(); got != 1 {
		t.Errorf("buffered cycles = %d, want the batch requeued (1)", got)
	}
}

// pushOnce runs one flush against a master that answers every /cycles with
// status, and reports what the ring holds afterwards.
func pushOnce(t *testing.T, status int, body string) (buffered int, err error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ack":true}`))
			return
		}
		http.Error(w, body, status)
	}))
	defer srv.Close()

	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1"},
	}, "v9")
	r.sink.OnCycle(context.Background(), scheduler.Cycle{
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t"}},
	})
	err = r.flushOnce(context.Background())
	return r.sink.Len(), err
}

// A batch the master will refuse identically forever — oversized, or older
// than MaxCycleAge after a long outage — must be dropped, not requeued.
// Requeueing it head-of-line blocks the ring: every later flush re-sends the
// same doomed batch while drop-oldest discards the live cycles behind it.
func TestFlushOnceDropsPermanentlyRejectedBatch(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity,
	} {
		buffered, err := pushOnce(t, status, "batch outside ingest bounds")
		if err != nil {
			t.Errorf("status %d: flushOnce returned %v, want nil (dropped, not fatal)", status, err)
		}
		if buffered != 0 {
			t.Errorf("status %d: %d cycles requeued, want the batch dropped", status, buffered)
		}
	}
}

// What dropping now makes possible: a master, WAF or proxy answering 4xx to
// everything turns into silent data loss. The 4xx that are genuinely
// transient must still requeue, and 5xx must be untouched.
func TestFlushOnceRequeuesRetryableStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusProxyAuthRequired,
		http.StatusRequestTimeout,
		http.StatusMisdirectedRequest,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		buffered, err := pushOnce(t, status, "try later")
		if err != nil {
			t.Errorf("status %d: flushOnce returned %v, want nil", status, err)
		}
		if buffered != 1 {
			t.Errorf("status %d: %d cycles buffered, want the batch requeued (1)", status, buffered)
		}
	}
}

// 401 stays fatal and 404 stays a drop — the two statuses that already had
// their own meaning must not be swallowed by the new permanent-4xx class.
func TestFlushOncePreservesAuthAndNotFound(t *testing.T) {
	if _, err := pushOnce(t, http.StatusUnauthorized, "nope"); !errors.Is(err, ErrAuth) {
		t.Errorf("401: got %v, want ErrAuth", err)
	}
	buffered, err := pushOnce(t, http.StatusNotFound, "gone")
	if err != nil {
		t.Errorf("404: got %v, want nil", err)
	}
	if buffered != 0 {
		t.Errorf("404: %d cycles buffered, want the batch dropped", buffered)
	}
}

// The retryable set is the statuses whose own RFC says the same bytes may
// succeed later or on another connection, not the three someone remembered.
// Sweeping the whole 4xx range makes adding a status a deliberate act: a new
// one defaults to a drop, which is data loss.
func TestRetryable4xxMatchesTheRFC(t *testing.T) {
	want := map[int]string{
		http.StatusProxyAuthRequired:  "RFC 9110 15.5.8: the proxy demands credentials, not a verdict on the batch",
		http.StatusRequestTimeout:     "RFC 9110 15.5.9: the client MAY repeat the request without modifications",
		http.StatusMisdirectedRequest: "RFC 9110 15.5.20: the client MAY retry the request over a different connection",
		http.StatusTooEarly:           "RFC 8470 5.2: replay the request once it is not early data",
		http.StatusTooManyRequests:    "RFC 6585 4: a transient rate limit",
	}
	for code := 400; code < 500; code++ {
		reason, isRetryable := want[code]
		if got := retryable4xx(code); got != isRetryable {
			if isRetryable {
				t.Errorf("%d is dropped but %s", code, reason)
			} else {
				t.Errorf("%d is retried; a status with no transient reading head-of-line blocks the ring", code)
			}
		}
	}
	// The three statuses classified before retryable4xx is consulted must not
	// also appear in it, or their own handling becomes unreachable.
	for _, code := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
	} {
		if retryable4xx(code) {
			t.Errorf("%d is both specially handled and retryable", code)
		}
	}
}

// RFC 9110 15.5.20's remedy for 421 is retrying over a *different* connection.
// Requeueing through the same pooled connection reproduces the misroute every
// flush, and Requeue puts the batch back on the ring's head — so drop-oldest
// discards live cycles behind a batch that can never succeed.
func TestMisdirectedRequestRetriesOnANewConnection(t *testing.T) {
	var mu sync.Mutex
	var conns int
	var statuses []int

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(statuses)
		code := http.StatusMisdirectedRequest
		if n > 0 {
			code = http.StatusOK
		}
		statuses = append(statuses, code)
		mu.Unlock()
		w.WriteHeader(code)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "tokyo-1", "v9", "")
	batch := cluster.CycleBatch{Source: "tokyo-1", Cycles: []cluster.CyclePayload{{Time: time.Now()}}}

	if err := c.PushCycles(context.Background(), batch); err == nil {
		t.Fatal("421 returned no error")
	} else if errors.Is(err, ErrRejected) {
		t.Fatalf("421 classified as permanently rejected: %v", err)
	}
	if err := c.PushCycles(context.Background(), batch); err != nil {
		t.Fatalf("retry after 421: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(statuses))
	}
	if conns != 2 {
		t.Fatalf("retry reused the pooled connection (%d connections for 2 requests); "+
			"RFC 9110 15.5.20 requires a different one", conns)
	}
}
