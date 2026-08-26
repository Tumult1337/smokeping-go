package slave

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// newRejectingRunner points a runner at a master that answers every request
// with the given permanent 4xx, the way handleRegister refuses an invalid
// cluster.name.
func newRejectingRunner(t *testing.T, status int) *Runner {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
