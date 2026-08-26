package master

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	store := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: "tok"}})
	return NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, nil)
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

func TestHandleCyclesAcceptsRegisteredName(t *testing.T) {
	srv := newTestServer()
	srv.registry.Touch("edge-1", "", "", "")
	if code := postCycles(t, srv, "edge-1", ""); code != http.StatusOK {
		t.Fatalf("registered slave: got %d, want 200", code)
	}
	if !srv.registry.Has("edge-1") {
		t.Error("registered slave dropped from the registry")
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
	store := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: "tok"}})
	sink := &captureSink{}
	hs := slavehealth.NewSet([]slavehealth.Peer{
		{Name: "frankfurt-1", Addr: netip.MustParseAddr("10.44.0.2")},
		{Name: "tokyo-1", Addr: netip.MustParseAddr("10.44.0.7")},
	}, nil)
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), sink,
		func() *slavehealth.Set { return hs })
	srv.registry.Touch("frankfurt-1", "", "", "")

	now := time.Now().UTC().Format(time.RFC3339)
	body := `{"source":"frankfurt-1","cycles":[{"group":"` + slavehealth.Group +
		`","name":"tokyo-1","probe":"` + slavehealth.ProbeName + `","time":"` + now + `","sent":5,"loss_count":0}]}`
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

// A slave holding a valid token must not be able to forge the per-cycle
// Source field to impersonate "master" or another slave — that would let it
// manufacture phantom quorum votes (mask a real outage or trigger a false
// page). handleCycles pins batch.Source to the authenticated X-Slave-Name
// header; ingestBatch must apply that pinned value to every cycle
// unconditionally, not just when the wire-provided per-cycle Source is empty.
func TestIngestBatchOverridesForgedPerCycleSource(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{
			{Group: "g", Targets: []config.Target{{Name: "t", Probe: "icmp"}}},
		},
	})
	sink := &captureSink{}
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), sink, nil)
	srv.registry.Touch("frankfurt-1", "", "", "")

	// Distinct timestamps because (source, group, name, timestamp) is one
	// measurement: same-stamped cycles are one identity and the ingest dedup
	// collapses them, which would hide the second forged source from this test.
	now := time.Now().UTC()
	first, second := now.Format(time.RFC3339Nano), now.Add(-time.Second).Format(time.RFC3339Nano)
	body := `{"source":"frankfurt-1","cycles":[` +
		`{"group":"g","name":"t","probe":"icmp","source":"master","time":"` + first + `","sent":5,"loss_count":0},` +
		`{"group":"g","name":"t","probe":"icmp","source":"tokyo-1","time":"` + second + `","sent":5,"loss_count":0}` +
		`]}`
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "frankfurt-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /cycles: got %d, want 200", rec.Code)
	}
	if len(sink.got) != 2 {
		t.Fatalf("sink saw %d cycles, want 2", len(sink.got))
	}
	for i, cy := range sink.got {
		if cy.Source != "frankfurt-1" {
			t.Errorf("cycle %d: source = %q, want authenticated %q (forged wire source not overridden)", i, cy.Source, "frankfurt-1")
		}
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

// configWithToken writes a valid config carrying tok and returns its path.
func configWithToken(t *testing.T, dir, tok string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	body := `{
      "listen": ":0",
      "interval": "60s",
      "pings": 5,
      "storage": {"clickhouse": {"addr": "ch:9000"}},
      "probes": {"icmp": {"type": "icmp", "timeout": "1s"}},
      "targets": [{"group": "g", "targets": [{"name": "t", "probe": "icmp", "host": "1.1.1.1"}]}],
      "cluster": {"token": ` + strconv.Quote(tok) + `}
    }`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Rotating cluster.token over SIGHUP has to take effect without a restart:
// a token that cannot be revoked without downtime is one an operator will not
// revoke during an incident. Driven through a real file + Reload rather than a
// synthetic setter so it exercises the path SIGHUP actually takes.
func TestClusterTokenRotatesOnReload(t *testing.T) {
	dir := t.TempDir()
	path := configWithToken(t, dir, "old-secret")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := config.NewStore(path, cfg)
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store,
		NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, nil)

	// Built once and reused, because that is how run_node.go mounts it: a
	// handler rebuilt per request would re-read the token on its own and hide
	// a credential frozen at construction, which is the bug under test.
	handler := srv.Handler()
	get := func(tok string) int {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get("old-secret"); code != http.StatusOK {
		t.Fatalf("before rotation, old token: got %d, want 200", code)
	}
	if code := get("new-secret"); code != http.StatusUnauthorized {
		t.Fatalf("before rotation, new token: got %d, want 401", code)
	}

	configWithToken(t, dir, "new-secret")
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if code := get("new-secret"); code != http.StatusOK {
		t.Fatalf("after rotation, new token: got %d, want 200", code)
	}
	if code := get("old-secret"); code != http.StatusUnauthorized {
		t.Fatalf("after rotation, old token still accepted: got %d, want 401 — rotation did not revoke", code)
	}
}

// A removed token must revoke access rather than fall back to the last good
// one. sha256("") == sha256(""), so an empty accepted token that skipped the
// emptiness guard would accept the header "Authorization: Bearer " from anyone.
func TestClusterEmptyTokenDeniesEveryone(t *testing.T) {
	store := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: ""}})
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store,
		NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, nil)

	for _, header := range []string{"", "Bearer ", "Bearer anything", "Bearer tok"} {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: got %d, want 401", header, rec.Code)
		}
	}
}

// A config with no cluster block at all is the same deny-all case, reached by
// a different route: currentToken must not panic dereferencing cfg.Cluster.
func TestClusterNilBlockDeniesEveryone(t *testing.T) {
	store := config.NewStore("", &config.Config{})
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store,
		NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil cluster block: got %d, want 401", rec.Code)
	}
}

// A slave holding the shared token can put any string in batch.Source, and
// the master stamped it onto every cycle: it became a ClickHouse
// LowCardinality dictionary entry, a permanent row in QueryLatestHops, and a
// source label on the unauthenticated API naming no real node. Only names the
// registry already knows are accepted.
func TestHandleCyclesRejectsUnregisteredSource(t *testing.T) {
	srv := newTestServer()

	if code := postCycles(t, srv, "", "10.44.0.2"); code != http.StatusForbidden {
		t.Errorf("unregistered batch.Source: got %d, want 403", code)
	}
	if code := postCycles(t, srv, "evil-1", ""); code != http.StatusForbidden {
		t.Errorf("unregistered X-Slave-Name: got %d, want 403", code)
	}
	if srv.registry.Has("10.44.0.2") || srv.registry.Has("evil-1") {
		t.Error("rejected name leaked into the registry")
	}
}

// An unidentified batch is refused rather than ingested under the empty
// source label.
func TestHandleCyclesRejectsEmptySource(t *testing.T) {
	srv := newTestServer()
	if code := postCycles(t, srv, "", ""); code != http.StatusBadRequest {
		t.Errorf("no name anywhere: got %d, want 400", code)
	}
}

// Rolling upgrade: a slave that predates the X-Slave-Name header on /cycles
// registered at boot, so its batch.Source names a registered slave and its
// cycles keep flowing.
func TestHandleCyclesAcceptsLegacySlaveWithoutHeader(t *testing.T) {
	srv := newTestServer()
	srv.registry.Touch("edge-1", "v0", "10.0.0.5:5000", "")

	if code := postCycles(t, srv, "", "edge-1"); code != http.StatusOK {
		t.Fatalf("registered legacy slave: got %d, want 200", code)
	}
}

// The header wins over the body when both are present and both are
// registered, so the identity the master stamps is the one it also used for
// the registry heartbeat.
func TestHandleCyclesPrefersHeaderOverBatchSource(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{{Group: "core", Targets: []config.Target{{Name: "gw", Host: "example.test"}}}}})
	sink := &captureSink{}
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), sink, nil)
	srv.registry.Touch("a", "", "", "")
	srv.registry.Touch("b", "", "", "")

	now := time.Now().UTC().Format(time.RFC3339)
	body := `{"source":"b","cycles":[{"group":"core","name":"gw","probe":"icmp","source":"b","time":"` + now + `","sent":5}]}`
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "a")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(sink.got) != 1 {
		t.Fatalf("got %d cycles, want 1", len(sink.got))
	}
	if sink.got[0].Source != "a" {
		t.Fatalf("cycle stamped %q, want the header identity %q", sink.got[0].Source, "a")
	}
}

// The registry is the master's only list of legitimate source labels, so its
// size is the bound on how many distinct labels a token holder can mint. New
// names past the cap are refused; names already in it keep heartbeating.
func TestRegistryCapsDistinctNames(t *testing.T) {
	reg := NewRegistry(slog.New(slog.DiscardHandler))
	for i := range maxRegisteredSlaves {
		if err := reg.Touch("slave-"+strconv.Itoa(i), "", "", ""); err != nil {
			t.Fatalf("slave-%d refused below the cap: %v", i, err)
		}
	}
	if err := reg.Touch("one-too-many", "", "", ""); !errors.Is(err, errRegistryFull) {
		t.Fatalf("past the cap: err = %v, want errRegistryFull", err)
	}
	if reg.Has("one-too-many") {
		t.Fatal("refused name stored anyway")
	}
	if err := reg.Touch("slave-0", "v2", "", ""); err != nil {
		t.Fatalf("an already-registered slave was refused at the cap: %v", err)
	}
}

// A full registry must not turn into an ingest outage for the slaves already
// in it, and must not admit the new name by the back door of /register.
func TestRegisterRefusedAtCap(t *testing.T) {
	srv := newTestServer()
	for i := range maxRegisteredSlaves {
		srv.registry.Touch("slave-"+strconv.Itoa(i), "", "", "")
	}
	req := httptest.NewRequest(http.MethodPost, "/register",
		bytes.NewReader([]byte(`{"name":"overflow"}`)))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("register past cap: got %d, want 503", rec.Code)
	}
	if code := postCycles(t, srv, "", "slave-0"); code != http.StatusOK {
		t.Fatalf("registered slave refused while the registry is full: got %d, want 200", code)
	}
}

// An oversized advertise is this request's own bytes, not a capacity
// condition: answering it "slave registry full" (503) sent an operator
// chasing a capacity problem that does not exist, on the only endpoint that
// says why a slave will not join.
func TestRegisterOversizedFieldIs400NotRegistryFull(t *testing.T) {
	srv := newTestServer()
	body := `{"name":"edge-1","advertise":"` + strings.Repeat("a", maxSlaveFieldLen+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized advertise: got %d, want 400", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, "registry full") {
		t.Fatalf("oversized advertise answered as a capacity problem: %q", got)
	}
	if srv.registry.Has("edge-1") {
		t.Error("refused slave stored anyway")
	}
}

// The ingest bounds have to fire at the HTTP boundary, not only in the DTO
// unit test: without the call site a hostile batch reaches the sink.
func TestHandleCyclesRejectsUnboundedBatch(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{{Group: "g", Targets: []config.Target{{Name: "t", Probe: "icmp"}}}},
	})
	sink := &captureSink{}
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), sink, nil)
	srv.registry.Touch("edge-1", "", "", "")

	future := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"source":"edge-1","cycles":[{"group":"g","name":"t","probe":"icmp","time":"` +
		future + `","sent":5}]}`
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "edge-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("future-dated cycle: got %d, want 400", rec.Code)
	}
	if len(sink.got) != 0 {
		t.Fatalf("sink saw %d cycles from a rejected batch, want 0", len(sink.got))
	}
}

// probe_type is a ClickHouse LowCardinality column, so a slave free to name it
// mints a permanent dictionary entry per push. The master already resolved the
// target from its own config, and that target names its probe, so the wire
// value has nothing to add.
func TestIngestBatchOverridesForgedProbeName(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{
			{Group: "g", Targets: []config.Target{{Name: "t", Probe: "icmp"}}},
		},
	})
	sink := &captureSink{}
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), sink, nil)
	srv.registry.Touch("edge-1", "", "", "")

	now := time.Now().UTC().Format(time.RFC3339)
	body := `{"source":"edge-1","cycles":[{"group":"g","name":"t","probe":"` +
		strings.Repeat("z", 200) + `","time":"` + now + `","sent":5,"loss_count":0}]}`
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "edge-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink saw %d cycles, want 1", len(sink.got))
	}
	if got := sink.got[0].ProbeName; got != "icmp" {
		t.Fatalf("cycle stamped probe %q, want the configured %q", got, "icmp")
	}
}
