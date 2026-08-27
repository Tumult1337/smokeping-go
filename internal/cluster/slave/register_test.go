package slave

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// newRejectingRunner points a runner at a master that answers every request
// with the given permanent 4xx, the way handleRegister refuses an invalid
// cluster.name — carrying the marker that says the verdict is the master's own
// and not an intermediary's.
func newRejectingRunner(t *testing.T, status int) *Runner {
	return newRefusingRunner(t, status, true)
}

func newRefusingRunner(t *testing.T, status int, marked bool) *Runner {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if marked {
			w.Header().Set(cluster.HeaderRefusal, cluster.RefusalPermanent)
		}
		http.Error(w, `name required: <=128 bytes, not "master", no control chars`, status)
	}))
	t.Cleanup(srv.Close)
	return NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "master"},
	}, "v9")
}

// A 400 at /register is a verdict the master repeats forever — an invalid
// cluster.name never becomes valid by waiting — so retrying it leaves a
// "running" slave that has never registered and probes nothing. It must exit
// with ErrRejected instead, like flushOnce already does for pushes.
func TestRegisterForeverExitsOnPermanentRejection(t *testing.T) {
	r := newRejectingRunner(t, http.StatusBadRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := r.registerForever(ctx)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("registerForever = %v, want ErrRejected (a retry loop would return the ctx error instead)", err)
	}
}

// Same classification at the initial /config pull: there is no stale config
// to keep running on, so a permanent 4xx retried forever is a slave that
// never probes.
func TestPullConfigInitialExitsOnPermanentRejection(t *testing.T) {
	r := newRejectingRunner(t, http.StatusBadRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := r.pullConfigInitial(ctx)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("pullConfigInitial = %v, want ErrRejected (a retry loop would return the ctx error instead)", err)
	}
}

// A transient 4xx must keep the boot retry loop: a WAF rate limit at register
// time is the moment, not the slave's config, and exiting would turn every
// CDN flap into a fleet-wide crash loop.
func TestRegisterForeverKeepsRetryingOnRetryable4xx(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ack":true}`))
	}))
	defer srv.Close()
	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1"},
	}, "v9")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.registerForever(ctx); err != nil {
		t.Fatalf("registerForever = %v, want retry through a 429 to success", err)
	}
	if calls != 2 {
		t.Fatalf("register attempts = %d, want 2", calls)
	}
}

// An unmarked 4xx is not the master's verdict, whatever its status. nginx maps
// its internal 494 (header buffer exceeded) and 497 to a plain 400, and the
// slave sends X-Slave-Name/Version/Advertise on every request — so a proxy
// header limit below maxSlaveFieldLen would 400 the whole fleet. Exiting there
// crash-loops every node under systemd with no self-recovery when the proxy is
// fixed, which is the failure client.go already records as the reason 403 is
// not fatal. It must keep retrying with backoff instead.
func TestBareBadRequestFromAnIntermediaryIsNotFatal(t *testing.T) {
	r := newRefusingRunner(t, http.StatusBadRequest, false)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	err := r.registerForever(ctx)
	if errors.Is(err, ErrMasterRefused) {
		t.Fatal("an unmarked 400 exited the process: a proxy header limit now crash-loops the whole fleet")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("registerForever = %v, want the retry loop to still be running at the deadline", err)
	}
}
