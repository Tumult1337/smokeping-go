package master

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
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
	return NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, "tok", nil)
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

// captureSink records every cycle the master's fanout receives.
type captureSink struct{ got []scheduler.Cycle }

func (c *captureSink) OnCycle(_ context.Context, cy scheduler.Cycle) { c.got = append(c.got, cy) }

// The health mesh is bidirectional: a slave probes its peers and pushes those
// cycles back. Health targets never enter config.Store, so ingest resolution
// must consult the health view or every peer-health cycle is silently dropped
// and the master becomes the only observer — quorum then never sees a second
// source.
func TestIngestResolvesHealthTargets(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{})
	sink := &captureSink{}
	hs := slavehealth.NewSet([]slavehealth.Peer{
		{Name: "frankfurt-1", Addr: netip.MustParseAddr("10.44.0.2")},
		{Name: "tokyo-1", Addr: netip.MustParseAddr("10.44.0.7")},
	}, nil)
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), sink, "tok",
		func() *slavehealth.Set { return hs })

	body := `{"source":"frankfurt-1","cycles":[{"group":"` + slavehealth.Group +
		`","name":"tokyo-1","probe":"` + slavehealth.ProbeName + `","sent":5,"loss_count":0}]}`
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "frankfurt-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /cycles: got %d, want 200", rec.Code)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink saw %d cycles, want 1 — peer-health cycle was dropped", len(sink.got))
	}
	cy := sink.got[0]
	if cy.Target.Group != slavehealth.Group || cy.Target.Target.Name != "tokyo-1" {
		t.Errorf("resolved to %s/%s, want %s/tokyo-1", cy.Target.Group, cy.Target.Target.Name, slavehealth.Group)
	}
	if cy.Source != "frankfurt-1" {
		t.Errorf("source = %q, want frankfurt-1", cy.Source)
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
