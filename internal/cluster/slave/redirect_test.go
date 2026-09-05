package slave

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// A 3xx the redirect policy refused to follow never reached a master handler,
// so it is not a verdict on the batch. Before do() classified it, the status
// fell through to the success path and PushCycles reported nil: the slave
// recorded an undelivered batch as delivered and dropped it.
func TestFlushRequeuesWhenARedirectIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ack":true}`))
			return
		}
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1"},
	}, "v9")
	r.sink.OnCycle(context.Background(), scheduler.Cycle{
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t"}},
	})

	if _, err := r.flushOnce(context.Background()); err != nil {
		t.Fatalf("flushOnce: %v", err)
	}
	if got := r.sink.Len(); got != 1 {
		t.Fatalf("buffered cycles = %d, want the batch requeued (1)", got)
	}
	if got := elsewhere.Load(); got != 0 {
		t.Fatalf("the redirect target was reached %d times, want 0", got)
	}
}

// A refused redirect requeues, and every condition that produces one is a
// configuration the responder repeats forever. Logged at Warn it is one more
// transient push failure; the ring then fills and drop-oldest discards
// everything with nothing above Warn ever saying why.
func TestARefusedRedirectIsReportedAtError(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ack":true}`))
			return
		}
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	r := NewRunner(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})),
		&config.Config{Cluster: &config.Cluster{
			MasterURL: srv.URL, Token: "tok", Name: "tokyo-1", PullEvery: "0",
		}}, "v9")
	r.sink.OnCycle(context.Background(), scheduler.Cycle{
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t"}},
	})
	if _, err := r.flushOnce(context.Background()); err != nil {
		t.Fatalf("flushOnce: %v", err)
	}

	// The handler only logs at Error, so anything in the buffer was logged
	// above Warn.
	if log := buf.String(); !strings.Contains(log, "redirect") {
		t.Fatalf("log %q does not report the refused redirect at Error", log)
	}
	if got := r.sink.Len(); got != 1 {
		t.Fatalf("buffered cycles = %d, want the batch requeued (1)", got)
	}
}

// 302 makes net/http rewrite POST /cycles into a bodyless GET. The master
// answers that 405 and the slave drops the batch as permanently rejected, so
// following one is silent data loss on a redirect the master never issued.
func TestPushDoesNotFollowARedirectThatRewritesThePost(t *testing.T) {
	var sawGET atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			sawGET.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/api/v1/cluster/cycles-moved", http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "tokyo-1", "v9", "")
	err := c.PushCycles(context.Background(), cluster.CycleBatch{Source: "tokyo-1"})
	if err == nil {
		t.Fatal("PushCycles reported success for a batch whose body was never sent")
	}
	if got := sawGET.Load(); got != 0 {
		t.Fatalf("the master saw %d bodyless GETs, want 0", got)
	}
}

// A challenge in front of the master sets a cookie the next push must carry.
// The jar is per client and not per request here, unlike the http probe's:
// re-solving the challenge on every flush is what trips it again.
func TestMasterClientKeepsTheChallengeCookieAcrossRequests(t *testing.T) {
	var challenges, served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("challenge"); err != nil {
			challenges.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "challenge", Value: "solved", Path: "/"})
			http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		served.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ack":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "tokyo-1", "v9", "")
	for i := range 3 {
		if err := c.Register(context.Background()); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}
	if got := challenges.Load(); got != 1 {
		t.Fatalf("the master issued %d challenges, want 1 — the cookie is not being retained", got)
	}
	if got := served.Load(); got != 3 {
		t.Fatalf("%d requests reached the handler, want 3", got)
	}
}
