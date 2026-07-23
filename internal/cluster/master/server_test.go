package master

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

func TestValidSlaveName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"s1", true},
		{"edge-pop-01", true},
		{"", false},
		{"master", false},                  // reserved: collides with local-probe source label
		{"bad\nname", false},               // control char
		{"tab\tname", false},               // control char
		{string(make([]byte, 129)), false}, // oversized
		{string(make([]byte, 128)), false}, // 128 NUL bytes — control chars, rejected
	}
	for _, c := range cases {
		if got := validSlaveName(c.name); got != c.want {
			t.Errorf("validSlaveName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

type nopSink struct{}

func (nopSink) OnCycle(context.Context, scheduler.Cycle) {}

func newTestServer() *Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{})
	return NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, "tok")
}

func postCycles(t *testing.T, srv *Server, slaveName, bodySource string) int {
	t.Helper()
	body := `{"source":"` + bodySource + `","cycles":[]}`
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	if slaveName != "" {
		req.Header.Set("X-Slave-Name", slaveName)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// A compromised-but-token-holding slave must not be able to claim source
// "master" (which would corrupt the master's own data/alert series) via the
// X-Slave-Name header, nor via the legacy batch.Source fallback.
func TestHandleCyclesRejectsReservedName(t *testing.T) {
	srv := newTestServer()

	if code := postCycles(t, srv, "master", ""); code != http.StatusBadRequest {
		t.Errorf("X-Slave-Name=master: got %d, want 400", code)
	}
	if code := postCycles(t, srv, "", "master"); code != http.StatusBadRequest {
		t.Errorf("batch.Source=master fallback: got %d, want 400", code)
	}
	if srv.registry.Has("master") {
		t.Error("reserved name leaked into registry")
	}
}

func TestHandleCyclesAcceptsValidName(t *testing.T) {
	srv := newTestServer()
	if code := postCycles(t, srv, "edge-1", ""); code != http.StatusOK {
		t.Fatalf("valid slave: got %d, want 200", code)
	}
	if !srv.registry.Has("edge-1") {
		t.Error("valid slave not registered")
	}
}

func TestHandleConfigRejectsInvalidName(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "bad\x00name")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("control-char name on /config: got %d, want 400", rec.Code)
	}
}
