package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
	"github.com/tumult/gosmokeping/internal/storage"
)

type stubReader struct {
	cycles []storage.CyclePoint
	rtts   []storage.RTTPoint
	http   []storage.HTTPPoint
	hops   []storage.HopPoint
	// cycleCounters is what the hop reads pair with s.hops: target loss comes
	// from the cycle, not from a hop row.
	cycleCounters []storage.CycleCounters
	overview      []storage.OverviewSourceRow
	err           error
	// lastSource captures the source filter passed to the most recent query,
	// so tests can assert the handler threaded ?source=… correctly.
	lastSource string
	// lastOverviewWindow records the (to - from) span passed to QueryOverview
	// so tests can assert ?window= was honoured.
	lastOverviewWindow time.Duration
	// lastOverviewTargets records how many target refs were passed in, so a
	// test can assert the handler scopes the query to configured targets.
	lastOverviewTargets int
	// lastAt/lastWindow capture the pin and the window width passed to the
	// most recent QueryHopsAt, so a test can assert what `at` resolved to.
	lastAt     time.Time
	lastWindow time.Duration
	// lastStep captures the bucket width passed to the most recent cycle
	// query, so a test can tell a raw query from a bucketed one.
	lastStep time.Duration
	// lastFrom/lastTo capture the window of the most recent cycle query, so a
	// test can assert a handler scans only what it serves.
	lastFrom, lastTo time.Time
	// queries counts every reader call. A window-cap test asserts this stays
	// zero on rejection: the guard has to fire before the expensive query,
	// which a status check alone cannot distinguish from a query that ran and
	// then errored.
	queries int
}

func (s *stubReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.CyclePoint, error) {
	s.lastSource = f.Source
	s.lastStep = f.Step
	s.lastFrom, s.lastTo = from, to
	s.queries++
	return s.cycles, s.err
}

func (s *stubReader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.RTTPoint, error) {
	s.lastSource = f.Source
	s.queries++
	return s.rtts, s.err
}

func (s *stubReader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HTTPPoint, error) {
	s.lastSource = f.Source
	s.queries++
	return s.http, s.err
}

func (s *stubReader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f storage.QueryFilter) (storage.HopsResult, error) {
	s.lastSource = f.Source
	return storage.HopsResult{Hops: s.hops, Cycles: s.cycleCounters}, s.err
}

func (s *stubReader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) (storage.HopsResult, error) {
	s.lastSource = f.Source
	s.lastAt, s.lastWindow = at, window
	s.queries++
	return storage.HopsResult{Hops: s.hops, Cycles: s.cycleCounters}, s.err
}

func (s *stubReader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) (storage.HopsResult, error) {
	s.lastSource = f.Source
	s.queries++
	return storage.HopsResult{Hops: s.hops}, s.err
}

func (s *stubReader) QueryOverview(ctx context.Context, from, to time.Time, targets []config.TargetRef) ([]storage.OverviewSourceRow, error) {
	s.lastOverviewWindow = to.Sub(from)
	s.lastOverviewTargets = len(targets)
	return s.overview, s.err
}

// testOpt customises the Options a test server is built with, applied after
// the shared defaults (Log, Store) so a test only needs to name what it cares
// about.
type testOpt func(*Options)

func withReader(r storage.Reader) testOpt {
	return func(o *Options) { o.Reader = r }
}

func withHealth(h HealthLister) testOpt {
	return func(o *Options) { o.Health = h }
}

func withWriterStats(s WriterStats) testOpt {
	return func(o *Options) { o.WriterStats = s }
}

func withReaderStats(s ReaderStats) testOpt {
	return func(o *Options) { o.ReaderStats = s }
}

func withAlertStats(s AlertStats) testOpt {
	return func(o *Options) { o.AlertStats = s }
}

type alertStatStub uint64

func (a alertStatStub) DispatchRefusals() uint64 { return uint64(a) }

type cacheStatStub storage.CacheStats

func (c cacheStatStub) Stats() storage.CacheStats { return storage.CacheStats(c) }

func newTestServer(t *testing.T, opts ...testOpt) http.Handler {
	return newTestServerWithInterval(t, time.Minute, opts...)
}

func newTestServerWithInterval(t *testing.T, interval time.Duration, opts ...testOpt) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Listen:   ":0",
		Interval: interval,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "gw", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	o := Options{
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store: store,
	}
	for _, opt := range opts {
		opt(&o)
	}
	s := New(o)
	return s.Router()
}

// do issues a request against the router and returns the status code and raw
// body, for tests that only care about status or want to unmarshal manually.
func do(t *testing.T, h http.Handler, method, path string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// doJSON issues a request and decodes a 200 JSON body into v, failing the
// test on a non-200 status or a decode error.
func doJSON(t *testing.T, h http.Handler, method, path string, v any) {
	t.Helper()
	code, body := do(t, h, method, path)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
}

// doRaw issues a request and returns the 200 body verbatim, for tests that
// assert on the JSON encoding itself rather than the decoded value.
func doRaw(t *testing.T, h http.Handler, method, path string) string {
	t.Helper()
	code, body := do(t, h, method, path)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	return string(body)
}

func TestHealth(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Errorf("status %v", body["status"])
	}
}

func TestListTargets(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var body []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body) != 1 || body[0]["id"] != "core/gw" {
		t.Errorf("unexpected body: %s", rr.Body.String())
	}
}

func TestListTargetsTitles(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Title: "Core Infra",
			Targets: []config.Target{
				{Name: "gw", Title: "Gateway", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	s := New(Options{
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store: store,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	var body []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1: %s", len(body), rr.Body.String())
	}
	if body[0]["group_title"] != "Core Infra" {
		t.Errorf("group_title = %v, want Core Infra", body[0]["group_title"])
	}
	if body[0]["title"] != "Gateway" {
		t.Errorf("title = %v, want Gateway", body[0]["title"])
	}
}

func TestListSourcesStandalone(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Sources []string `json:"sources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Sources) != 1 || body.Sources[0] != "master" {
		t.Errorf("standalone sources = %v, want [master]", body.Sources)
	}
}

type stubSlaveLister struct{ names []string }

func (s stubSlaveLister) Names() []string { return s.names }

func TestListSourcesMasterWithRegisteredSlaves(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "gw", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
		Cluster: &config.Cluster{Source: "primary"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	s := New(Options{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
		Slaves: stubSlaveLister{names: []string{"eu-central", "eu-west"}},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources", nil)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	var body struct {
		Sources []string `json:"sources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	want := []string{"primary", "eu-central", "eu-west"}
	if len(body.Sources) != len(want) {
		t.Fatalf("sources = %v, want %v", body.Sources, want)
	}
	for i, s := range want {
		if body.Sources[i] != s {
			t.Errorf("sources[%d] = %q, want %q (full: %v)", i, body.Sources[i], s, body.Sources)
		}
	}
}

func TestListTargetsStandaloneSources(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body []struct {
		ID      string   `json:"id"`
		Sources []string `json:"sources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1: %s", len(body), rr.Body.String())
	}
	if len(body[0].Sources) != 1 || body[0].Sources[0] != "master" {
		t.Errorf("standalone target sources = %v, want [master]", body[0].Sources)
	}
}

func TestListTargetsPerTargetSources(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "gw-global", Host: "1.1.1.1", Probe: "icmp"},
				{Name: "gw-eu", Host: "2.2.2.2", Probe: "icmp", Slaves: []string{"eu1"}},
				{Name: "gw-ghost", Host: "3.3.3.3", Probe: "icmp", Slaves: []string{"unregistered"}},
			},
		}},
		Cluster: &config.Cluster{Source: "primary"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	s := New(Options{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
		Slaves: stubSlaveLister{names: []string{"eu1", "us1"}},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	var body []struct {
		ID      string   `json:"id"`
		Sources []string `json:"sources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	got := make(map[string][]string, len(body))
	for _, b := range body {
		got[b.ID] = b.Sources
	}
	cases := map[string][]string{
		"core/gw-global": {"primary", "eu1", "us1"},
		"core/gw-eu":     {"eu1"},
		"core/gw-ghost":  nil, // assigned slave isn't registered → no live sources
	}
	for id, want := range cases {
		if !equalStrings(got[id], want) {
			t.Errorf("%s sources = %v, want %v", id, got[id], want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGetCyclesMissingTarget(t *testing.T) {
	h := newTestServer(t, withReader(&stubReader{}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets/doesnotexist/cycles", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestGetCyclesReturnsPoints(t *testing.T) {
	r := &stubReader{cycles: []storage.CyclePoint{{Time: time.Now(), Median: 5.0}}}
	h := newTestServer(t, withReader(r))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets/core/gw/cycles?from=-1h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Points []storage.CyclePoint `json:"points"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Points) != 1 || body.Points[0].Median != 5.0 {
		t.Errorf("unexpected points: %s", rr.Body.String())
	}
	if r.lastSource != "" {
		t.Errorf("no source query param => reader.lastSource = %q, want empty", r.lastSource)
	}
}

// twoTargetServer builds a test server with two configured targets
// (core/gw and core/dns), used by overview tests.
func twoTargetServer(t *testing.T, reader storage.Reader) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Title: "Core",
			Targets: []config.Target{
				{Name: "gw", Title: "Gateway", Host: "1.1.1.1", Probe: "icmp"},
				{Name: "dns", Host: "9.9.9.9", Probe: "icmp"},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	s := New(Options{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
		Reader: reader,
	})
	return s.Router()
}

// overviewBody is the JSON shape /api/v1/overview returns. Kept as a test-local
// struct to keep the contract tightly pinned.
type overviewBody struct {
	Window string            `json:"window"`
	From   string            `json:"from"`
	To     string            `json:"to"`
	Rows   []overviewRowBody `json:"rows"`
}

type overviewRowBody struct {
	ID          string     `json:"id"`
	Group       string     `json:"group"`
	GroupTitle  string     `json:"group_title,omitempty"`
	Title       string     `json:"title,omitempty"`
	ProbeType   string     `json:"probe_type,omitempty"`
	LossAvg     *float64   `json:"loss_avg"`
	LossMax     *float64   `json:"loss_max"`
	RTTMedian   *float64   `json:"rtt_median"`
	RTTP95      *float64   `json:"rtt_p95"`
	RTTMax      *float64   `json:"rtt_max"`
	WorstSource string     `json:"worst_source,omitempty"`
	LastSeen    *time.Time `json:"last_seen"`
	Silent      bool       `json:"silent"`
	Sparkline   []*float64 `json:"sparkline"`
}

func decodeOverview(t *testing.T, body []byte) overviewBody {
	t.Helper()
	var b overviewBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, string(body))
	}
	return b
}

func TestOverviewReaderNil(t *testing.T) {
	h := twoTargetServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOverviewSilentTarget(t *testing.T) {
	// Reader returns no rows → every configured target should be flagged silent.
	r := &stubReader{}
	h := twoTargetServer(t, r)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b := decodeOverview(t, rr.Body.Bytes())
	if len(b.Rows) != 2 {
		t.Fatalf("rows=%d, want 2: %s", len(b.Rows), rr.Body.String())
	}
	for _, row := range b.Rows {
		if !row.Silent {
			t.Errorf("%s: silent=false, want true", row.ID)
		}
		if row.LossAvg != nil || row.RTTMedian != nil || row.LastSeen != nil {
			t.Errorf("%s: silent row has non-nil metrics: %+v", row.ID, row)
		}
		if len(row.Sparkline) != 0 {
			t.Errorf("%s: silent row sparkline len=%d, want 0", row.ID, len(row.Sparkline))
		}
	}
	if r.lastOverviewTargets != 2 {
		t.Errorf("handler passed %d targets to reader, want 2", r.lastOverviewTargets)
	}
}

func TestOverviewHappyPathSingleSource(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	// loss_pct=12.4, max loss=50%, median RTT 18ms, p95 31ms, max 120ms,
	// last cycle 5s ago. Sparkline of 4 valid points.
	one := func(v float64) *float64 { x := v; return &x }
	r := &stubReader{
		overview: []storage.OverviewSourceRow{{
			Group:   "core",
			Name:    "gw",
			Source:  "master",
			LossAvg: 12.4,
			LossMax: 50.0,
			HasRTT:  true, RTTMedian: 18.2,
			RTTP95:    31.0,
			RTTMax:    120.0,
			LastSeen:  now.Add(-5 * time.Second),
			Sparkline: []*float64{one(12.1), nil, one(13.0), one(18.4)},
		}},
	}
	h := twoTargetServer(t, r)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview?window=-1h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b := decodeOverview(t, rr.Body.Bytes())
	if b.Window != "-1h" {
		t.Errorf("window=%q, want -1h", b.Window)
	}
	if r.lastOverviewWindow != time.Hour {
		t.Errorf("reader called with span=%v, want 1h", r.lastOverviewWindow)
	}
	var gw, dns *overviewRowBody
	for i := range b.Rows {
		switch b.Rows[i].ID {
		case "core/gw":
			gw = &b.Rows[i]
		case "core/dns":
			dns = &b.Rows[i]
		}
	}
	if gw == nil || dns == nil {
		t.Fatalf("missing rows: %s", rr.Body.String())
	}
	if gw.Silent {
		t.Errorf("gw silent=true, want false")
	}
	if gw.LossAvg == nil || *gw.LossAvg != 12.4 {
		t.Errorf("gw loss_avg=%v, want 12.4", gw.LossAvg)
	}
	if gw.WorstSource != "master" {
		t.Errorf("gw worst_source=%q, want master", gw.WorstSource)
	}
	if gw.GroupTitle != "Core" {
		t.Errorf("gw group_title=%q, want Core", gw.GroupTitle)
	}
	if gw.Title != "Gateway" {
		t.Errorf("gw title=%q, want Gateway", gw.Title)
	}
	if !dns.Silent {
		t.Errorf("dns silent=false, want true (no reader rows for it)")
	}
}

func TestOverviewWorstSourceWins(t *testing.T) {
	now := time.Now().UTC()
	// Two sources for core/gw: master is clean, eu-west is lossy.
	// Collapsed row should show eu-west's loss and worst_source=eu-west.
	r := &stubReader{
		overview: []storage.OverviewSourceRow{
			{
				Group:   "core",
				Name:    "gw",
				Source:  "master",
				LossAvg: 0.0,
				LossMax: 0.0,
				HasRTT:  true, RTTMedian: 8.0,
				RTTP95:   12.0,
				RTTMax:   25.0,
				LastSeen: now,
			},
			{
				Group:   "core",
				Name:    "gw",
				Source:  "eu-west",
				LossAvg: 18.0,
				LossMax: 50.0,
				HasRTT:  true, RTTMedian: 40.0,
				RTTP95:   80.0,
				RTTMax:   200.0,
				LastSeen: now,
			},
		},
	}
	h := twoTargetServer(t, r)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b := decodeOverview(t, rr.Body.Bytes())
	var gw *overviewRowBody
	for i := range b.Rows {
		if b.Rows[i].ID == "core/gw" {
			gw = &b.Rows[i]
			break
		}
	}
	if gw == nil {
		t.Fatalf("no core/gw row: %s", rr.Body.String())
	}
	if gw.WorstSource != "eu-west" {
		t.Errorf("worst_source=%q, want eu-west", gw.WorstSource)
	}
	if gw.LossAvg == nil || *gw.LossAvg != 18.0 {
		t.Errorf("loss_avg=%v, want 18.0 (worst source's value)", gw.LossAvg)
	}
	if gw.RTTMax == nil || *gw.RTTMax != 200.0 {
		t.Errorf("rtt_max=%v, want 200.0 (max across sources)", gw.RTTMax)
	}
}

func TestOverviewWindowDefault(t *testing.T) {
	r := &stubReader{}
	h := twoTargetServer(t, r)
	// Garbage window should fall back to -1h.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview?window=garbage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b := decodeOverview(t, rr.Body.Bytes())
	if b.Window != "-1h" {
		t.Errorf("window=%q, want -1h (garbage falls back)", b.Window)
	}
	if r.lastOverviewWindow != time.Hour {
		t.Errorf("span=%v, want 1h", r.lastOverviewWindow)
	}
}

func TestOverviewWindowAccepts6h(t *testing.T) {
	r := &stubReader{}
	h := twoTargetServer(t, r)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview?window=-6h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b := decodeOverview(t, rr.Body.Bytes())
	if b.Window != "-6h" {
		t.Errorf("window=%q, want -6h", b.Window)
	}
	if r.lastOverviewWindow != 6*time.Hour {
		t.Errorf("span=%v, want 6h", r.lastOverviewWindow)
	}
}

func TestOverviewSilentByStaleness(t *testing.T) {
	// Reader returns a row but last_seen is older than 5×interval (1m × 5 = 5m).
	// The handler should still flag it silent.
	r := &stubReader{
		overview: []storage.OverviewSourceRow{{
			Group:   "core",
			Name:    "gw",
			Source:  "master",
			LossAvg: 0.0,
			HasRTT:  true, RTTMedian: 8.0,
			LastSeen: time.Now().Add(-10 * time.Minute),
		}},
	}
	h := twoTargetServer(t, r)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	b := decodeOverview(t, rr.Body.Bytes())
	for _, row := range b.Rows {
		if row.ID == "core/gw" && !row.Silent {
			t.Errorf("stale gw row (last_seen 10m ago, interval 1m) silent=false, want true")
		}
	}
}

func TestGetCyclesThreadsSourceParam(t *testing.T) {
	r := &stubReader{}
	h := newTestServer(t, withReader(r))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets/core/gw/cycles?from=-1h&source=eu-west", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if r.lastSource != "eu-west" {
		t.Errorf("reader.lastSource = %q, want eu-west", r.lastSource)
	}
}

type stubHealth struct{ refs []config.TargetRef }

func (s stubHealth) PublicTargets() []config.TargetRef { return s.refs }

// healthStub returns refs with Target.Host populated, simulating a leaky
// HealthLister implementation. listTargets's health-DTO construction must
// never read Host regardless of what the lister hands it — the stripping
// guarantee has to hold structurally, not because callers happen to leave
// the field zero. TestListTargetsIncludesHealthGroup relies on this to be a
// real assertion instead of a tautology (see its comment).
func healthStub() stubHealth {
	return stubHealth{refs: []config.TargetRef{
		{Group: slavehealth.Group, Target: config.Target{
			Name: "frankfurt-1", Title: "frankfurt-1", Probe: slavehealth.ProbeName,
			Host: "198.51.100.10", Alerts: []string{"slave-down"},
		}},
		{Group: slavehealth.Group, Target: config.Target{
			Name: "tokyo-1", Title: "tokyo-1", Probe: slavehealth.ProbeName,
			Host: "198.51.100.20",
		}},
	}}
}

// healthStub's refs carry a populated Target.Host (a leaky-lister
// simulation) so this test actually exercises the address-stripping in
// listTargets's health-DTO construction. Without that, a stub with Host
// always unset would pass whether or not the DTO reads Host at all — two
// independent reasons for the same green result, neither of which is the
// property under test.
func TestListTargetsIncludesHealthGroup(t *testing.T) {
	srv := newTestServer(t, withHealth(healthStub()))

	var got []map[string]any
	doJSON(t, srv, "GET", "/api/v1/targets", &got)

	var found int
	for _, row := range got {
		if row["group"] != slavehealth.Group {
			continue
		}
		found++
		if host, present := row["host"]; present && host != "" {
			t.Fatalf("health target %v leaked host %q", row["id"], host)
		}
		if row["group_title"] != slavehealth.GroupTitle {
			t.Fatalf("got group_title %v, want %q", row["group_title"], slavehealth.GroupTitle)
		}
	}
	if found != 2 {
		t.Fatalf("got %d health rows, want 2", found)
	}
}

// Ordinary targets must keep their host — the stripping is scoped.
//
// Asserting against "" here is non-discriminating: targetDTO.Host is
// `json:"host,omitempty"`, so a stripped host omits the key entirely and
// row["host"] decodes to nil, not "". nil == "" is false, so the guard never
// fires either way. Assert the actual expected value instead.
func TestListTargetsKeepsOrdinaryHosts(t *testing.T) {
	srv := newTestServer(t, withHealth(healthStub()))

	var got []map[string]any
	doJSON(t, srv, "GET", "/api/v1/targets", &got)

	found := false
	for _, row := range got {
		if row["group"] != "core" {
			continue
		}
		found = true
		if row["host"] != "1.1.1.1" {
			t.Fatalf("ordinary target %v lost its host: got %v, want %q", row["id"], row["host"], "1.1.1.1")
		}
	}
	if !found {
		t.Fatal("no core-group target in response")
	}
}

// Health targets are absent from the stored config, so resolveTarget must
// consult the health view or every chart request 404s.
func TestResolveHealthTarget(t *testing.T) {
	srv := newTestServer(t, withHealth(healthStub()))

	code, _ := do(t, srv, "GET", "/api/v1/targets/_cluster/frankfurt-1/cycles?range=1h")
	if code == http.StatusNotFound {
		t.Fatal("health target did not resolve")
	}
}

func TestResolveUnknownHealthTargetIs404(t *testing.T) {
	srv := newTestServer(t, withHealth(healthStub()))

	code, _ := do(t, srv, "GET", "/api/v1/targets/_cluster/nonexistent/cycles?range=1h")
	if code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", code)
	}
}

// Alert names are operator-chosen labels with no address content. Dropping
// them from the health DTO would render an alerting slave as unmonitored in
// the UI — the exact state Public() exposes them to avoid.
func TestListTargetsSurfacesHealthAlerts(t *testing.T) {
	srv := newTestServer(t, withHealth(healthStub()))

	var got []map[string]any
	doJSON(t, srv, "GET", "/api/v1/targets", &got)

	found := false
	for _, row := range got {
		if row["group"] != slavehealth.Group || row["name"] != "frankfurt-1" {
			continue
		}
		found = true
		alerts, _ := row["alerts"].([]any)
		if len(alerts) != 1 || alerts[0] != "slave-down" {
			t.Fatalf("health target alerts = %v, want [slave-down]", row["alerts"])
		}
	}
	if !found {
		t.Fatal("frankfurt-1 health row missing")
	}
}

func TestHealthTargetsAbsentWithoutHealthLister(t *testing.T) {
	srv := newTestServer(t) // no health option

	var got []map[string]any
	doJSON(t, srv, "GET", "/api/v1/targets", &got)
	for _, row := range got {
		if row["group"] == slavehealth.Group {
			t.Fatal("health rows present without a HealthLister")
		}
	}
}

func TestRedactTerminalHop(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "master", Index: 1, IP: "198.51.100.1"},
		{Source: "master", Index: 2, IP: "198.51.100.9"},
		{Source: "master", Index: 3, IP: "10.44.0.2"},
		{Source: "tokyo-1", Index: 1, IP: "203.0.113.1"},
		{Source: "tokyo-1", Index: 2, IP: "10.44.0.2"},
	}
	got := redactTerminalHops(hops)

	for _, h := range got {
		if h.IP == "10.44.0.2" {
			t.Fatalf("terminal hop not redacted for source %q index %d", h.Source, h.Index)
		}
	}
	// Intermediates survive.
	if got[0].IP != "198.51.100.1" {
		t.Fatalf("intermediate hop altered: %q", got[0].IP)
	}
	if got[3].IP != "203.0.113.1" {
		t.Fatalf("intermediate hop altered for second source: %q", got[3].IP)
	}
}

// Redaction is per-source: each observer's trace has its own terminal hop, and
// redacting only the global maximum index would leak the shorter paths.
func TestRedactTerminalHopIsPerSource(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 1, IP: "10.0.0.1"},
		{Source: "a", Index: 9, IP: "10.44.0.2"},
		{Source: "b", Index: 1, IP: "10.44.0.2"},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.IP == "10.44.0.2" {
			t.Fatalf("terminal hop for source %q not redacted", h.Source)
		}
	}
}

func TestRedactTerminalHopEmpty(t *testing.T) {
	if got := redactTerminalHops(nil); len(got) != 0 {
		t.Fatalf("got %d hops, want 0", len(got))
	}
}

// TestGetHopsRedactsHealthTarget exercises /hops end-to-end for a health
// target, catching wiring bugs the isolated redactTerminalHops unit tests
// can't: deleting the `if slavehealth.IsHealthGroup(ref.Group)` guard in
// getHops leaves those unit tests untouched but would ship the slave's
// address to the browser.
func TestGetHopsRedactsHealthTarget(t *testing.T) {
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "tokyo-1", Index: 1, IP: "203.0.113.1"},
		{Source: "tokyo-1", Index: 2, IP: "10.44.0.2"}, // terminal: the slave itself
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []storage.HopPoint `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops", &body)

	if len(body.Hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(body.Hops), body.Hops)
	}
	for _, h := range body.Hops {
		if h.Index == 2 && h.IP != "" {
			t.Fatalf("terminal hop IP not redacted: %+v", h)
		}
		// The intermediate hop must survive: /hops feeds the MTR path table,
		// and this assertion is what distinguishes the endpoint's contract
		// from /hops/timeline's blanket redaction. Without it the test would
		// pass against a blanket-redact implementation.
		if h.Index == 1 && h.IP != "203.0.113.1" {
			t.Fatalf("intermediate hop redacted on /hops: %+v", h)
		}
	}
}

// TestGetHopsKeepsOrdinaryTargetIntact guards against an inverted or
// over-broad IsHealthGroup check: an ordinary (non-health) target's terminal
// hop must reach the client unredacted.
func TestGetHopsKeepsOrdinaryTargetIntact(t *testing.T) {
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "master", Index: 1, IP: "203.0.113.1"},
		{Source: "master", Index: 2, IP: "10.44.0.2"},
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []storage.HopPoint `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/core/gw/hops", &body)

	if len(body.Hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(body.Hops), body.Hops)
	}
	for _, h := range body.Hops {
		if h.Index == 2 && h.IP != "10.44.0.2" {
			t.Fatalf("ordinary target's terminal hop was redacted: %+v", h)
		}
	}
}

// TestGetHopsTimelineRedactsHealthTarget is the /hops/timeline counterpart
// to TestGetHopsRedactsHealthTarget — the two endpoints have separate guard
// call sites in getHops and getHopsTimeline, so each needs its own coverage.
//
// Unlike /hops, /hops/timeline redacts every non-empty address, not just the
// apparent terminal one — a grid row's address is the worst-loss cycle's
// responder, so "terminal" names nothing (see redactAllHopAddresses). Both
// addresses here must come back as the sentinel, never blank and never the
// real value.
func TestGetHopsTimelineRedactsHealthTarget(t *testing.T) {
	now := time.Now()
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "tokyo-1", Time: now, Index: 1, IP: "203.0.113.1"},
		{Source: "tokyo-1", Time: now, Index: 2, IP: "10.44.0.2"}, // the slave
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []hopTimelineDTO `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops/timeline?source=tokyo-1", &body)

	if len(body.Hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(body.Hops), body.Hops)
	}
	for _, h := range body.Hops {
		if h.IP == "203.0.113.1" || h.IP == "10.44.0.2" {
			t.Fatalf("real address leaked on hops/timeline: %+v", h)
		}
		if h.IP != hopAddrSentinel {
			t.Fatalf("non-empty hop address not sentinelized: %+v", h)
		}
	}
}

// TestGetHopsTimelineKeepsOrdinaryTargetIntact is the /hops/timeline
// counterpart to TestGetHopsKeepsOrdinaryTargetIntact.
func TestGetHopsTimelineKeepsOrdinaryTargetIntact(t *testing.T) {
	now := time.Now()
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "master", Time: now, Index: 1, IP: "203.0.113.1"},
		{Source: "master", Time: now, Index: 2, IP: "10.44.0.2"},
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []hopTimelineDTO `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/core/gw/hops/timeline?source=master", &body)

	if len(body.Hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(body.Hops), body.Hops)
	}
	for _, h := range body.Hops {
		if h.Index == 2 && h.IP != "10.44.0.2" {
			t.Fatalf("ordinary target's terminal hop was redacted: %+v", h)
		}
	}
}

// TestRedactTerminalHopPerTimestamp pins redactTerminalHops' (source,
// timestamp) keying against a row set spanning two traces of different
// lengths. No current caller produces such a set — /hops pins one timestamp
// per source, and /hops/timeline uses redactAllHopAddresses instead — so this
// guards the property, not a live code path: keying the max by source alone
// computes one set-wide maximum and misses the terminal hop of every shorter
// trace, leaking the slave's address for those rows. Any future caller handing
// multi-timestamp rows to this function inherits the correct behaviour.
func TestRedactTerminalHopPerTimestamp(t *testing.T) {
	t1 := time.Unix(1_700_000_000, 0)
	t2 := time.Unix(1_700_003_600, 0) // 1h later: route change adds a hop

	hops := []storage.HopPoint{
		// t1: tokyo-1 reaches the slave in 8 hops.
		{Source: "tokyo-1", Time: t1, Index: 1, IP: "203.0.113.1"},
		{Source: "tokyo-1", Time: t1, Index: 8, IP: "10.44.0.2"}, // terminal @ t1
		// t2: route change — tokyo-1 now takes 9 hops.
		{Source: "tokyo-1", Time: t2, Index: 1, IP: "203.0.113.1"},
		{Source: "tokyo-1", Time: t2, Index: 8, IP: "198.51.100.9"}, // now intermediate @ t2
		{Source: "tokyo-1", Time: t2, Index: 9, IP: "10.44.0.2"},    // terminal @ t2
	}
	got := redactTerminalHops(hops)

	byKey := make(map[[2]int64]storage.HopPoint, len(got))
	for _, h := range got {
		byKey[[2]int64{h.Time.Unix(), h.Index}] = h
	}

	if h := byKey[[2]int64{t1.Unix(), 8}]; h.IP != "" {
		t.Fatalf("t1 terminal hop (index 8) not redacted: got IP %q", h.IP)
	}
	if h := byKey[[2]int64{t2.Unix(), 9}]; h.IP != "" {
		t.Fatalf("t2 terminal hop (index 9) not redacted: got IP %q", h.IP)
	}
	// Index 8 at t2 is now an intermediate hop and must survive.
	if h := byKey[[2]int64{t2.Unix(), 8}]; h.IP != "198.51.100.9" {
		t.Fatalf("t2 intermediate hop (index 8) altered: got IP %q, want 198.51.100.9", h.IP)
	}
	// Index 1 at both timestamps is an intermediate hop and must survive.
	if h := byKey[[2]int64{t1.Unix(), 1}]; h.IP != "203.0.113.1" {
		t.Fatalf("t1 intermediate hop (index 1) altered: got IP %q", h.IP)
	}
	if h := byKey[[2]int64{t2.Unix(), 1}]; h.IP != "203.0.113.1" {
		t.Fatalf("t2 intermediate hop (index 1) altered: got IP %q", h.IP)
	}
}

// TestRedactAllHopAddressesClosesIntraBucketLeak proves the intra-bucket case
// redactTerminalHops cannot close. It guards the property rather than a live
// row shape: queryHopsGrid now emits one row per (slot, ttl), so the two rows
// at the same ttl below are no longer producible, but the redaction may not
// depend on that — the shapes it still produces carry a longer trace than
// whichever row sits at the apparent maximum index all the same. A positional
// "blank only the max index" pass —
// exactly what redactTerminalHops does — leaves every other row's address
// intact, including one carrying the slave's real address at the same ttl
// as a still-shorter trace. redactAllHopAddresses must not have this gap:
// no real address may survive regardless of how the rows are shaped.
func TestRedactAllHopAddressesClosesIntraBucketLeak(t *testing.T) {
	tBucket := time.Unix(1_700_000_000, 0)
	hops := []storage.HopPoint{
		// Same (source, time, ttl=8), two distinct addresses: mid-bucket route
		// change. One is the slave, reached via the old (shorter) route.
		{Source: "tokyo-1", Time: tBucket, Index: 8, IP: "10.44.0.2"},
		{Source: "tokyo-1", Time: tBucket, Index: 8, IP: "198.51.100.9"},
		// Same (source, time), one row longer: the new route reaches the slave
		// at ttl 9 instead.
		{Source: "tokyo-1", Time: tBucket, Index: 9, IP: "10.44.0.2"},
	}
	got := redactAllHopAddresses(hops)
	for _, h := range got {
		if h.IP == "10.44.0.2" || h.IP == "198.51.100.9" {
			t.Fatalf("real address survived redaction: %+v", h)
		}
	}
}

// TestRedactAllHopAddressesKeepsNoReplyEmpty guards the other half of the
// contract: a hop that genuinely never replied must stay "", not become the
// sentinel. ui/src/MtrHeatmap.tsx reads IP=="" as "no reply" and colors the
// cell accordingly — sentinelizing empty addresses would flip every
// no-reply cell to look like a reply.
func TestRedactAllHopAddressesKeepsNoReplyEmpty(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "tokyo-1", Index: 3, IP: ""},
	}
	got := redactAllHopAddresses(hops)
	if got[0].IP != "" {
		t.Fatalf("no-reply hop was sentinelized: got IP %q, want empty", got[0].IP)
	}
}

// Window caps are the only thing bounding an unauthenticated request's
// ClickHouse scan, so every case asserts the reader was never reached: a 400
// produced after the query ran would leave the amplification intact.
func TestReadEndpointWindowCaps(t *testing.T) {
	cases := []struct {
		name string
		path string
		want int
	}{
		{"rtts at the cap", "/api/v1/targets/core/gw/rtts?from=-24h", http.StatusOK},
		{"rtts past the cap", "/api/v1/targets/core/gw/rtts?from=-25h", http.StatusBadRequest},
		{"rtts far past the cap", "/api/v1/targets/core/gw/rtts?from=-3000d", http.StatusBadRequest},
		{"http at the cap", "/api/v1/targets/core/gw/http?from=-7d", http.StatusOK},
		{"http past the cap", "/api/v1/targets/core/gw/http?from=-8d", http.StatusBadRequest},
		{"hops timeline at the cap", "/api/v1/targets/core/gw/hops/timeline?source=master&from=-7d", http.StatusOK},
		{"hops timeline past the cap", "/api/v1/targets/core/gw/hops/timeline?source=master&from=-8d", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &stubReader{}
			code, body := do(t, newTestServer(t, withReader(r)), http.MethodGet, tc.path)
			if code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", code, tc.want, body)
			}
			wantQueries := 1
			if tc.want != http.StatusOK {
				wantQueries = 0
			}
			if r.queries != wantQueries {
				t.Fatalf("reader calls = %d, want %d — the guard must reject before querying", r.queries, wantQueries)
			}
		})
	}
}

// The ?step= overrides bypass the tier ladder, so each is bounded by the
// ladder's own tier rather than a second copy of its threshold: raw outside
// the raw tier and 1h past the 1h tier are refused, never widened. Asserting
// the step the reader received keeps a guard that silently downgrades an
// override to bucketed from passing as a fix.
func TestGetCyclesRawStepCap(t *testing.T) {
	rawTier := 2 * time.Hour
	if storage.PickCycleStep(rawTier) != 0 {
		t.Fatalf("ladder no longer serves raw at %s; update this test's boundary", rawTier)
	}
	cases := []struct {
		name     string
		path     string
		want     int
		wantStep time.Duration
	}{
		{"raw inside the ladder's raw tier", "/api/v1/targets/core/gw/cycles?from=-2h&step=raw", http.StatusOK, 0},
		{"raw past the raw tier", "/api/v1/targets/core/gw/cycles?from=-3h&step=raw", http.StatusBadRequest, 0},
		{"raw far past the raw tier", "/api/v1/targets/core/gw/cycles?from=-3000d&step=raw", http.StatusBadRequest, 0},
		{"bucketed wide window unaffected", "/api/v1/targets/core/gw/cycles?from=-30d", http.StatusOK, time.Hour},
		{"1h override at its ladder tier", "/api/v1/targets/core/gw/cycles?from=-30d&step=1h", http.StatusOK, time.Hour},
		{"1h finer than the 6h tier", "/api/v1/targets/core/gw/cycles?from=-180d&step=1h", http.StatusBadRequest, 0},
		{"1h finer than the 1d tier", "/api/v1/targets/core/gw/cycles?from=-365d&step=1h", http.StatusBadRequest, 0},
		{"1d override on a wide window unaffected", "/api/v1/targets/core/gw/cycles?from=-3000d&step=1d", http.StatusOK, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &stubReader{}
			code, body := do(t, newTestServer(t, withReader(r)), http.MethodGet, tc.path)
			if code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", code, tc.want, body)
			}
			if tc.want != http.StatusOK {
				if r.queries != 0 {
					t.Fatalf("reader calls = %d, want 0 — the guard must reject before querying", r.queries)
				}
				return
			}
			if r.queries != 1 {
				t.Fatalf("reader calls = %d, want 1", r.queries)
			}
			if r.lastStep != tc.wantStep {
				t.Fatalf("step = %s, want %s", r.lastStep, tc.wantStep)
			}
		})
	}
}

// /status keeps statusRecentCycles points, so that count times the probe
// interval is the only window it may ask storage for: the previous fixed 24h
// raw scan decoded ~690x what the handler kept, and the full slice is what
// CachingReader stored.
func TestStatusQueriesOnlyTheWindowItServes(t *testing.T) {
	r := &stubReader{}
	code, body := do(t, newTestServer(t, withReader(r)), http.MethodGet, "/api/v1/targets/core/gw/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, body)
	}
	// newTestServer configures a 1m interval.
	want := time.Duration(statusRecentCycles) * time.Minute
	if got := r.lastTo.Sub(r.lastFrom); got != want {
		t.Fatalf("query window = %s, want %s (statusRecentCycles x interval)", got, want)
	}
	if r.lastStep != 0 {
		t.Fatalf("step = %s, want raw", r.lastStep)
	}
}

// Overload is backpressure, not an upstream fault: a client that retries a 502
// is doing the wrong thing, and a cache admission refusal is exactly the case
// where retrying shortly is correct.
func TestQueryOverloadIsRetryable503(t *testing.T) {
	paths := []string{
		"/api/v1/targets/core/gw/cycles?from=-1h",
		"/api/v1/targets/core/gw/hops",
		"/api/v1/overview",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			h := newTestServer(t, withReader(&stubReader{err: storage.ErrOverloaded}))
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", rr.Code, rr.Body)
			}
			if got := rr.Header().Get("Retry-After"); got != "5" {
				t.Fatalf("Retry-After = %q, want %q", got, "5")
			}
		})
	}
}

// A non-overload reader failure must stay 502: mapping every error to a
// retryable 503 would tell clients to hammer a genuinely broken backend.
func TestQueryFailureStays502(t *testing.T) {
	h := newTestServer(t, withReader(&stubReader{err: errors.New("clickhouse: connection refused")}))
	code, body := do(t, h, http.MethodGet, "/api/v1/targets/core/gw/cycles?from=-1h")
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", code, body)
	}
}

// The heatmap draws one column per bucket and cannot recover the bucket width
// from the rows: a window holding a single bucket has no row gap to measure.
// Sizing a column by row count instead painted that one bucket across the
// whole window, showing hours of history that had not been collected. The
// server resolves the step, so it has to say what it picked.
func TestHopsTimelineEchoesResolvedStep(t *testing.T) {
	for _, span := range []time.Duration{time.Hour, 6 * time.Hour, 48 * time.Hour, 7 * 24 * time.Hour} {
		t.Run(span.String(), func(t *testing.T) {
			h := newTestServer(t, withReader(&stubReader{}))
			var body struct {
				StepSec int64 `json:"step_sec"`
			}
			doJSON(t, h, http.MethodGet,
				"/api/v1/targets/core/gw/hops/timeline?source=master&from=-"+span.String(), &body)

			want := int64(storage.PickHopStep(span, time.Minute) / time.Second)
			if body.StepSec != want {
				t.Fatalf("step_sec = %d, want %d for a %s window", body.StepSec, want, span)
			}
			if body.StepSec == 0 {
				t.Fatalf("a %s window resolved to a raw grid, whose row count is the producer's cycle rate", span)
			}
		})
	}
}

// The grid is never finer than the probe interval: an empty slot renders as a
// gap in the heatmap, which reads as a probe that stopped rather than as a
// column the cadence could not fill. newTestServer configures a 1m interval,
// well above the ladder's own floor for a 1h window.
func TestHopsTimelineStepIsNeverFinerThanTheInterval(t *testing.T) {
	h := newTestServer(t, withReader(&stubReader{}))
	var body struct {
		StepSec int64 `json:"step_sec"`
	}
	doJSON(t, h, http.MethodGet, "/api/v1/targets/core/gw/hops/timeline?source=master&from=-1h", &body)
	if body.StepSec != int64(time.Minute/time.Second) {
		t.Fatalf("step_sec = %d, want the 60s probe interval", body.StepSec)
	}
}

// The walk-output shape that breaks positional redaction: the target echoed
// at index 2 in one round, later rounds stayed silent through maxTTL, so the
// positional terminal is a silent row and the slave's address sits mid-path.
// Only the TargetReply marker identifies it.
func TestRedactTerminalHopKeysOnTargetReply(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 1, IP: "198.51.100.1"},
		{Source: "a", Index: 2, IP: "10.44.0.2", TargetReply: true, Unreach: ""},
		{Source: "a", Index: 3, IP: ""},
		{Source: "a", Index: 4, IP: ""},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.IP == "10.44.0.2" {
			t.Fatalf("marked target row survived at index %d", h.Index)
		}
		if h.Index == 2 && !h.TargetReply {
			t.Fatalf("TargetReply cleared on a blanked row: %+v", h)
		}
	}
	if got[0].IP != "198.51.100.1" {
		t.Fatalf("intermediate hop altered: %+v", got[0])
	}
}

// Address-mates of a marked row are blanked too: a TimeExceeded quoting the
// target's own address must not survive because it lacked the marker.
func TestRedactTerminalHopCoversTargetAddressMates(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 4, IP: "198.51.100.1"},
		{Source: "a", Index: 5, IP: "10.44.0.2"},
		{Source: "a", Index: 6, IP: "10.44.0.2", TargetReply: true},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.IP == "10.44.0.2" {
			t.Fatalf("target address mate survived at index %d", h.Index)
		}
	}
	if got[0].IP != "198.51.100.1" {
		t.Fatalf("intermediate hop altered: %+v", got[0])
	}
}

// Blanked implies annotation cleared on every arm: a mate row blanked via the
// address match must lose its Unreach too, or the annotation becomes a side
// channel for the blanked row.
func TestRedactTerminalHopClearsAnnotationOnMates(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 3, IP: "10.44.0.2", Unreach: "no-route"},
		{Source: "a", Index: 6, IP: "10.44.0.2", TargetReply: true},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.Index == 3 && (h.IP != "" || h.Unreach != "") {
			t.Fatalf("mate row kept address or annotation: %+v", h)
		}
	}
}

// The marker and address match are scoped per (source, timestamp): source b's
// index-1 row deliberately carries source a's target address so a key-less
// (global) IP set goes RED here.
func TestRedactTerminalHopAddressMatchIsPerSource(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 1, IP: "198.51.100.1"},
		{Source: "a", Index: 2, IP: "10.44.0.2", TargetReply: true},
		{Source: "b", Index: 1, IP: "10.44.0.2"},
		{Source: "b", Index: 2, IP: "10.44.0.9", TargetReply: true},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.Source == "b" && h.Index == 1 && h.IP != "10.44.0.2" {
			t.Fatalf("source b's intermediate judged against source a's target: %+v", h)
		}
		if h.Index == 2 && h.IP != "" {
			t.Fatalf("target row not blanked for source %q", h.Source)
		}
	}
}

// A silent terminal (IP "") with no marker anywhere names no address, so the
// intermediate below it may itself be the slave and is blanked with it.
func TestRedactTerminalHopSilentTerminalBlanksIntermediates(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 1, IP: "198.51.100.1"},
		{Source: "a", Index: 2, IP: ""},
	}
	got := redactTerminalHops(hops)
	if got[0].IP != "" {
		t.Fatalf("silent terminal left an address unblanked: %+v", got)
	}
}

// The annotation follows its address on both endpoints: cleared wherever the
// address is blanked, kept everywhere for ordinary targets.
func TestGetHopsRedactsAnnotationsWithAddress(t *testing.T) {
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "tokyo-1", Index: 1, IP: "203.0.113.1", Unreach: "no-route"},
		{Source: "tokyo-1", Index: 2, IP: "10.44.0.2", TargetReply: true, Unreach: "admin-prohibited"},
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []storage.HopPoint `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops", &body)
	for _, h := range body.Hops {
		if h.Index == 2 && (h.IP != "" || h.Unreach != "") {
			t.Fatalf("target row leaked: %+v", h)
		}
		if h.Index == 1 && h.Unreach != "no-route" {
			t.Fatalf("intermediate annotation must survive on /hops: %+v", h)
		}
	}
}

func TestGetHopsTimelineClearsAnnotationsForHealthTarget(t *testing.T) {
	now := time.Now()
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "tokyo-1", Time: now, Index: 1, IP: "203.0.113.1", Unreach: "no-route"},
		{Source: "tokyo-1", Time: now, Index: 2, IP: "10.44.0.2", TargetReply: true},
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []hopTimelineDTO `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops/timeline?source=tokyo-1", &body)
	for _, h := range body.Hops {
		if h.IP != hopAddrSentinel {
			t.Fatalf("address not sentinelized: %+v", h)
		}
		if h.Unreach != "" {
			t.Fatalf("annotation leaked on timeline: %+v", h)
		}
	}
}

// Positive counterparts: an ordinary target's fields must reach the client at
// all — without these, the clearing tests pass against DTOs that simply never
// carry the fields.
func TestGetHopsCarriesAnnotationsForOrdinaryTarget(t *testing.T) {
	now := time.Now()
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "master", Time: now, Index: 2, IP: "10.0.0.2", Unreach: "admin-prohibited", TargetReply: true},
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	raw := doRaw(t, srv, "GET", "/api/v1/targets/core/gw/hops")
	if !strings.Contains(raw, `"Unreach":"admin-prohibited"`) || !strings.Contains(raw, `"TargetReply":true`) {
		t.Fatalf("/hops lost an annotation field:\n%s", raw)
	}

	rawTL := doRaw(t, srv, "GET", "/api/v1/targets/core/gw/hops/timeline?source=master")
	if !strings.Contains(rawTL, `"Unreach":"admin-prohibited"`) {
		t.Fatalf("/hops/timeline lost Unreach:\n%s", rawTL)
	}
	// The timeline DTO must not carry TargetReply: it has no consumer there,
	// and this pair is what makes the absence assertion falsifiable.
	if strings.Contains(rawTL, "TargetReply") {
		t.Fatalf("/hops/timeline serves TargetReply, which its DTO must omit:\n%s", rawTL)
	}
}

type dropStub map[string]uint64

func (d dropStub) Dropped() map[string]uint64 { return d }

// Dropped() had no read site outside tests: the counters existed and were
// unreachable by an operator. /health is the read site.
func TestHealthReportsWriterDrops(t *testing.T) {
	srv := newTestServer(t, withWriterStats(dropStub{"probe_hop": 7, "probe_cycle": 0}))
	var body struct {
		WriterDrops map[string]uint64 `json:"writer_drops"`
	}
	doJSON(t, srv, "GET", "/api/v1/health", &body)
	if body.WriterDrops["probe_hop"] != 7 {
		t.Fatalf("writer_drops = %+v, want probe_hop=7", body.WriterDrops)
	}
}

func TestHealthOmitsWriterDropsWithoutWriter(t *testing.T) {
	srv := newTestServer(t)
	var body map[string]any
	doJSON(t, srv, "GET", "/api/v1/health", &body)
	if _, ok := body["writer_drops"]; ok {
		t.Fatal("writer_drops present with no writer wired")
	}
}

func TestHealthReportsCacheStats(t *testing.T) {
	srv := newTestServer(t, withReaderStats(cacheStatStub{CyclesHits: 9, CyclesMisses: 2, HopsHits: 4, HopsMisses: 1}))
	var body struct {
		Cache storage.CacheStats `json:"cache"`
	}
	doJSON(t, srv, "GET", "/api/v1/health", &body)
	if body.Cache.CyclesHits != 9 || body.Cache.HopsMisses != 1 {
		t.Fatalf("cache = %+v, want cycles_hits=9 hops_misses=1", body.Cache)
	}
}

func TestHealthReportsAlertDispatchRefusals(t *testing.T) {
	srv := newTestServer(t, withAlertStats(alertStatStub(3)))
	var body struct {
		Refusals uint64 `json:"alert_dispatch_refusals"`
	}
	doJSON(t, srv, "GET", "/api/v1/health", &body)
	if body.Refusals != 3 {
		t.Fatalf("alert_dispatch_refusals = %d, want 3", body.Refusals)
	}
}

// A slave runs no evaluator, so the field must be absent rather than a zero
// reading as a delivery queue that has never been under pressure.
func TestHealthOmitsAlertRefusalsWithoutAnEvaluator(t *testing.T) {
	srv := newTestServer(t)
	var body map[string]any
	doJSON(t, srv, "GET", "/api/v1/health", &body)
	if _, ok := body["alert_dispatch_refusals"]; ok {
		t.Fatal("alert_dispatch_refusals present with no evaluator wired")
	}
}

// A slave and a storage-disabled master hold no caching reader, so the field
// must be absent rather than four zeroes reading as a cache that never hits.
func TestHealthOmitsCacheStatsWithoutACachingReader(t *testing.T) {
	srv := newTestServer(t)
	var body map[string]any
	doJSON(t, srv, "GET", "/api/v1/health", &body)
	if _, ok := body["cache"]; ok {
		t.Fatal("cache present with no caching reader wired")
	}
}

// earlyEchoHealthRows is the walk output TestWalkRoundsMarksEarlyEchoRow
// produces: the target answered at ttl 2 and later rounds stayed silent to
// maxTTL. The two endpoints treat the marker on these rows differently, and
// this pair is what pins each choice.
func earlyEchoHealthRows() []storage.HopPoint {
	now := time.Unix(1_700_000_000, 0)
	return []storage.HopPoint{
		{Source: "tokyo-1", Time: now, Index: 1, IP: "198.51.100.1", Mean: 3, Sent: 3},
		{Source: "tokyo-1", Time: now, Index: 2, IP: "10.44.0.2", TargetReply: true, Mean: 5, Sent: 3, LossCount: 1, LossPct: 33},
		{Source: "tokyo-1", Time: now, Index: 30, IP: "", Sent: 2, LossCount: 2, LossPct: 100},
	}
}

// /hops keeps the marker on the rows it blanks: once the address is gone it
// is the only thing naming which row the trace ended at, and a per-round walk
// does not put that row at the deepest ttl. It discloses nothing the same
// response does not already carry — the intermediates keep their real
// addresses, and a blanked row that answered keeps its RTTs and sub-100% loss,
// so the answering ttl is readable either way.
func TestRedactTerminalHopKeepsTargetReplyMarker(t *testing.T) {
	got := redactTerminalHops(earlyEchoHealthRows())
	for _, h := range got {
		if h.IP == "10.44.0.2" {
			t.Fatalf("target address survived at index %d", h.Index)
		}
	}
	if marked := got[1]; !marked.TargetReply {
		t.Fatalf("marker cleared on the blanked target row: %+v", marked)
	}
	if got[0].IP != "198.51.100.1" || got[0].TargetReply {
		t.Fatalf("intermediate row altered: %+v", got[0])
	}
}

// /hops/timeline is the asymmetric case: every address becomes the sentinel,
// so no row's stats set it apart, and the marker would be the one thing
// naming the ttl the slave answered at. It is cleared there — and the DTO
// carries no counterpart either, per
// TestGetHopsCarriesAnnotationsForOrdinaryTarget.
func TestRedactAllHopAddressesClearsTargetReplyMarker(t *testing.T) {
	got := redactAllHopAddresses(earlyEchoHealthRows())
	for _, h := range got {
		if h.TargetReply {
			t.Fatalf("marker survived bucketed redaction at index %d: %+v", h.Index, h)
		}
	}
}

// The endpoint counterpart: a health target's /hops response must reach the
// client with the marker on it, or the redaction unit test above is asserting
// against a field the handler never serves.
func TestGetHopsServesMarkerForRedactedHealthTarget(t *testing.T) {
	r := &stubReader{hops: earlyEchoHealthRows()}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []storage.HopPoint `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops", &body)
	if len(body.Hops) != 3 {
		t.Fatalf("got %d hops, want 3", len(body.Hops))
	}
	if !body.Hops[1].TargetReply || body.Hops[1].IP != "" {
		t.Fatalf("marked row must be served blanked but still marked: %+v", body.Hops[1])
	}
}

// A hostile slave can write a health-target trace whose deepest row is silent
// and whose only address sits on an unmarked intermediate: neither redaction
// arm names a terminal, so a set-membership implementation serves the slave's
// address. The group must be blanked wholesale instead.
func TestRedactTerminalHopBlanksGroupWithNoTerminalAddress(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "tokyo-1", Index: 2, IP: "10.44.0.2"},
		{Source: "tokyo-1", Index: 30, IP: ""},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.IP != "" {
			t.Fatalf("address survived a group with no confident terminal: %+v", h)
		}
	}
}

// The fail-closed blanking is scoped to the group that lacks a terminal: a
// sibling source whose trace does name one keeps its intermediates.
func TestRedactTerminalHopFailClosedIsPerGroup(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "a", Index: 2, IP: "10.44.0.2"},
		{Source: "a", Index: 30, IP: ""},
		{Source: "b", Index: 1, IP: "198.51.100.1"},
		{Source: "b", Index: 2, IP: "10.44.0.3", TargetReply: true},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.Source == "a" && h.IP != "" {
			t.Fatalf("source a not blanked: %+v", h)
		}
		if h.Source == "b" && h.Index == 1 && h.IP != "198.51.100.1" {
			t.Fatalf("source b's intermediate blanked by source a's ambiguity: %+v", h)
		}
		if h.Source == "b" && h.Index == 2 && h.IP != "" {
			t.Fatalf("source b's target row not blanked: %+v", h)
		}
	}
}

// The address-mates arm compares addresses, not their spelling: a slave that
// writes the same address in two equivalent textual forms must not defeat it.
func TestRedactTerminalHopMatesNormalizeSpelling(t *testing.T) {
	hops := []storage.HopPoint{
		{Source: "v6", Index: 1, IP: "2001:db8:ffff::1"},
		{Source: "v6", Index: 2, IP: "2001:db8::1"},
		{Source: "v6", Index: 3, IP: "2001:0db8:0000:0000:0000:0000:0000:0001", TargetReply: true},
		{Source: "v4", Index: 1, IP: "198.51.100.1"},
		{Source: "v4", Index: 2, IP: "::ffff:10.44.0.2"},
		{Source: "v4", Index: 3, IP: "10.44.0.2", TargetReply: true},
		{Source: "zone", Index: 1, IP: "2001:db8:ffff::1"},
		{Source: "zone", Index: 2, IP: "fe80::1%eth0"},
		{Source: "zone", Index: 3, IP: "fe80::1", TargetReply: true},
	}
	got := redactTerminalHops(hops)
	for _, h := range got {
		if h.Index == 2 && h.IP != "" {
			t.Fatalf("target address mate survived under a different spelling: %+v", h)
		}
	}
	if got[0].IP != "2001:db8:ffff::1" {
		t.Fatalf("unrelated v6 intermediate altered: %+v", got[0])
	}
	if got[3].IP != "198.51.100.1" {
		t.Fatalf("unrelated v4 intermediate altered: %+v", got[3])
	}
}

// End-to-end counterpart: the hostile row shape must not reach the browser
// through /hops either.
func TestGetHopsFailsClosedOnAmbiguousHealthPath(t *testing.T) {
	r := &stubReader{hops: []storage.HopPoint{
		{Source: "tokyo-1", Index: 2, IP: "10.44.0.2"},
		{Source: "tokyo-1", Index: 30, IP: ""},
	}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	var body struct {
		Hops []storage.HopPoint `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops", &body)

	if len(body.Hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(body.Hops), body.Hops)
	}
	for _, h := range body.Hops {
		if h.IP != "" {
			t.Fatalf("address served for an ambiguous health path: %+v", h)
		}
	}
}

// A hop read that reached its row cap is refused by storage rather than
// truncated, and the handler must say so: mapping it to the generic 502 tells
// the operator the backend is broken, and a 503 tells them to retry a query
// that will fail identically every time.
func TestTruncatedHopReadIsARequestError(t *testing.T) {
	paths := []string{
		"/api/v1/targets/core/gw/hops",
		"/api/v1/targets/core/gw/hops?at=1774000000",
		"/api/v1/targets/core/gw/hops/timeline?source=master&from=-24h",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			h := newTestServer(t, withReader(&stubReader{err: storage.ErrHopsTruncated}))
			code, body := do(t, h, http.MethodGet, path)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, body)
			}
			if !strings.Contains(string(body), "narrow the range") {
				t.Fatalf("body does not tell the operator what to do: %s", body)
			}
		})
	}
}

// Target loss cannot be read off hop rows: a per-round walk marks the target
// at every TTL it answered at, so the three marked rows below sum to 6 sent
// and 3 lost — 50% — for a cycle that reached the target on all three of its
// rounds. /hops therefore ships the cycle's own counters under a key that is
// a sibling of hops, not a field on one.
func TestGetHopsCarriesTargetLoss(t *testing.T) {
	cycleTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	r := &stubReader{
		hops: []storage.HopPoint{
			{Source: "master", Time: cycleTime, Index: 1, IP: "10.0.0.1", Sent: 3},
			{Source: "master", Time: cycleTime, Index: 2, IP: "192.0.2.9", Sent: 3, LossCount: 2, TargetReply: true},
			{Source: "master", Time: cycleTime, Index: 3, IP: "192.0.2.9", Sent: 2, LossCount: 1, TargetReply: true},
			{Source: "master", Time: cycleTime, Index: 4, IP: "192.0.2.9", Sent: 1, TargetReply: true},
		},
		cycleCounters: []storage.CycleCounters{
			{Source: "master", Time: cycleTime, Sent: 3, LossCount: 0, LossPct: 0},
		},
	}
	srv := newTestServer(t, withReader(r))

	var body struct {
		Hops       []storage.HopPoint `json:"hops"`
		TargetLoss []struct {
			Source    string  `json:"Source"`
			Sent      int64   `json:"Sent"`
			LossCount int64   `json:"LossCount"`
			LossPct   float64 `json:"LossPct"`
		} `json:"target_loss"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/core/gw/hops", &body)

	var markedSent, markedLost int64
	for _, h := range body.Hops {
		if h.TargetReply {
			markedSent += h.Sent
			markedLost += h.LossCount
		}
	}
	if markedSent != 6 || markedLost != 3 {
		t.Fatalf("fixture no longer reproduces the row-summed lie: sent=%d lost=%d", markedSent, markedLost)
	}
	if len(body.TargetLoss) != 1 {
		t.Fatalf("got %d target_loss entries, want one per source: %+v", len(body.TargetLoss), body.TargetLoss)
	}
	got := body.TargetLoss[0]
	if got.Source != "master" || got.Sent != 3 || got.LossCount != 0 || got.LossPct != 0 {
		t.Fatalf("target_loss = %+v, want master 3 sent 0 lost", got)
	}
}

// A source whose cycle sent nothing wrote no probe_cycle row, so it has no
// counters. The key must then be an empty array rather than a zeroed entry:
// 0 sent / 0 lost renders as a healthy 0%, which is the fabricated point the
// writer refuses to store in the first place.
func TestGetHopsOmitsTargetLossWithoutAMeasurement(t *testing.T) {
	r := &stubReader{hops: []storage.HopPoint{{Source: "master", Index: 1, IP: "10.0.0.1", Sent: 1, LossCount: 1}}}
	srv := newTestServer(t, withReader(r))

	code, raw := do(t, srv, http.MethodGet, "/api/v1/targets/core/gw/hops")
	if code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", code, raw)
	}
	if !strings.Contains(string(raw), `"target_loss":[]`) {
		t.Fatalf("target_loss is not an empty array: %s", raw)
	}
}

// Cycle counters carry no address, but they are served from the same handler
// that redacts one — so the health path has to be exercised end to end rather
// than reasoned about.
func TestGetHopsTargetLossLeaksNoAddressForHealthTarget(t *testing.T) {
	cycleTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	r := &stubReader{
		hops: []storage.HopPoint{
			{Source: "tokyo-1", Time: cycleTime, Index: 1, IP: "203.0.113.1"},
			{Source: "tokyo-1", Time: cycleTime, Index: 2, IP: "10.44.0.2", TargetReply: true},
		},
		cycleCounters: []storage.CycleCounters{{Source: "tokyo-1", Time: cycleTime, Sent: 10, LossCount: 1, LossPct: 10}},
	}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	code, raw := do(t, srv, http.MethodGet, "/api/v1/targets/_cluster/tokyo-1/hops")
	if code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", code, raw)
	}
	if strings.Contains(string(raw), "10.44.0.2") {
		t.Fatalf("health target response carries the slave's address: %s", raw)
	}
	if !strings.Contains(string(raw), `"Sent":10`) || !strings.Contains(string(raw), `"LossCount":1`) {
		t.Fatalf("health target lost its cycle counters, which carry no address: %s", raw)
	}
}

// /hops/timeline serves one probe origin per request. Without that admission
// the row count carries a factor — the number of sources with rows in the
// window — that nothing in the config or the schema bounds, and no ceiling
// derived from the grid can hold. The heatmap already draws one canvas per
// source and fetches each separately, so the cost is a refusal on a hand-made
// request, not on the UI.
func TestGetHopsTimelineRequiresASource(t *testing.T) {
	now := time.Now()
	r := &stubReader{hops: []storage.HopPoint{{Source: "master", Time: now, Index: 1, IP: "203.0.113.1"}}}
	srv := newTestServer(t, withReader(r), withHealth(healthStub()))

	code, body := do(t, srv, "GET", "/api/v1/targets/core/gw/hops/timeline")
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 for a source-less timeline", code, body)
	}
	if r.queries != 0 {
		t.Fatalf("the refused request still ran %d queries", r.queries)
	}

	// Present but empty names the untagged pre-cluster origin, which is a
	// source like any other — the reader matches it exactly.
	var served struct {
		Hops []hopTimelineDTO `json:"hops"`
	}
	doJSON(t, srv, "GET", "/api/v1/targets/core/gw/hops/timeline?source=", &served)
	if r.lastSource != "" {
		t.Fatalf("lastSource = %q, want the untagged origin", r.lastSource)
	}

	doJSON(t, srv, "GET", "/api/v1/targets/core/gw/hops/timeline?source=master", &served)
	if r.lastSource != "master" {
		t.Fatalf("lastSource = %q, want master", r.lastSource)
	}
}

// A finite unix second between JavaScript's date limit and MaxInt64 parsed
// fine and then wrapped: time.Unix(1e16, 0).UnixMilli() is negative, so the
// centre ClickHouse received sat in the opposite epoch direction from the one
// asked for. Reject an instant no probe row can carry, before any conversion.
func TestTimestampOutsideStorableRangeIsRejected(t *testing.T) {
	h := newTestServer(t, withReader(&stubReader{}))

	rejected := []string{
		"/api/v1/targets/core/gw/hops?at=10000000000000000",
		"/api/v1/targets/core/gw/hops?at=9999-12-31T23:59:59Z",
		"/api/v1/targets/core/gw/hops?at=1899-12-31T23:59:59.999Z",
		"/api/v1/targets/core/gw/cycles?from=-1h&to=10000000000000000",
		// from and to both out of range and correctly ordered, so the
		// to-after-from check cannot stand in for the range guard.
		"/api/v1/targets/core/gw/cycles?from=10000000000000000&to=10000000000000001",
		"/api/v1/targets/core/gw/rtts?from=10000000000000000&to=10000000000000001",
		"/api/v1/targets/core/gw/http?from=10000000000000000&to=10000000000000001",
		"/api/v1/targets/core/gw/hops/timeline?source=&from=10000000000000000&to=10000000000000001",
	}
	for _, path := range rejected {
		t.Run(path, func(t *testing.T) {
			reader := &stubReader{}
			code, body := do(t, newTestServer(t, withReader(reader)), http.MethodGet, path)
			if code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", code, body)
			}
			if reader.queries != 0 {
				t.Fatalf("reader ran %d queries; the guard must fire before the read", reader.queries)
			}
		})
	}

	// The inclusive edges of the DateTime64(3) domain, and the pre-epoch
	// instant `at=-1` names, are ordinary requests. The last is the literal a
	// caller sends: the RFC3339 equivalent it used to carry is a different
	// input, and it passed for a year while `at=-1` itself returned 400.
	for _, path := range []string{
		"/api/v1/targets/core/gw/hops?at=1900-01-01T00:00:00Z",
		"/api/v1/targets/core/gw/hops?at=2299-12-31T23:59:59.999Z",
		"/api/v1/targets/core/gw/hops?at=-1",
	} {
		t.Run(path, func(t *testing.T) {
			code, body := do(t, h, http.MethodGet, path)
			if code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", code, body)
			}
		})
	}
}

// What the README promises about `at`: RFC3339 carries the sub-second
// precision that names one cycle at a sub-2s cadence, the unix form is
// integer-only, and the 30-minute window handed to the reader is centred — so
// the documented reach is ±15m, not ±30m.
func TestHopsAtFormsAndWindow(t *testing.T) {
	reader := &stubReader{}
	h := newTestServer(t, withReader(reader))

	code, body := do(t, h, http.MethodGet, "/api/v1/targets/core/gw/hops?at=2026-04-01T00:00:00.900Z")
	if code != http.StatusOK {
		t.Fatalf("rfc3339 with milliseconds: status=%d body=%s", code, body)
	}
	if got := reader.lastAt.UTC(); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 900_000_000, time.UTC)) {
		t.Errorf("reader saw at=%s, want the milliseconds it was given", got)
	}
	if reader.lastWindow != 30*time.Minute {
		t.Errorf("window = %s, want 30m (±15m around at)", reader.lastWindow)
	}

	reader.queries = 0
	code, _ = do(t, h, http.MethodGet, "/api/v1/targets/core/gw/hops?at=1775001600.9")
	if code != http.StatusBadRequest {
		t.Errorf("fractional unix: status=%d, want 400 — the unix branch is integer-only", code)
	}
	if reader.queries != 0 {
		t.Errorf("reader ran %d queries for a rejected at", reader.queries)
	}
}

// A leading sign starts two different grammars: `-1h` is an offset from now,
// `-1` is a unix instant one second before the epoch, and the README documents
// both forms on `from`, `to` and `at`. Routing every signed value into the
// duration parser made every signed unix second a 400. Each case below is the
// literal string a caller puts on the wire — the RFC3339 equivalent is a
// different input and cannot fail first for this bug.
func TestSignedUnixSecondsResolveAsInstants(t *testing.T) {
	instants := map[string]time.Time{
		"-1":            time.Unix(-1, 0),
		"-86400":        time.Unix(-86400, 0),
		"%2B1":          time.Unix(1, 0),
		"-0":            time.Unix(0, 0),
		"%2B0":          time.Unix(0, 0),
		"%2B1775001600": time.Unix(1775001600, 0),
	}
	for raw, want := range instants {
		t.Run("at="+raw, func(t *testing.T) {
			reader := &stubReader{}
			h := newTestServer(t, withReader(reader))
			code, body := do(t, h, http.MethodGet, "/api/v1/targets/core/gw/hops?at="+raw)
			if code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", code, body)
			}
			if got := reader.lastAt.UTC(); !got.Equal(want.UTC()) {
				t.Fatalf("at=%s resolved to %s, want %s", raw, got, want.UTC())
			}
		})
	}

	// from/to on a range endpoint, asserted through the window the handler
	// echoes back rather than through a duplicate of its own parse.
	for _, tc := range []struct{ from, to string }{
		{"-86400", "-3600"},
		{"%2B1775001600", "%2B1775005200"},
		{"-3600", "%2B60"},
	} {
		t.Run("cycles?from="+tc.from+"&to="+tc.to, func(t *testing.T) {
			h := newTestServer(t, withReader(&stubReader{}))
			var got struct {
				From time.Time `json:"from"`
				To   time.Time `json:"to"`
			}
			doJSON(t, h, "GET", "/api/v1/targets/core/gw/cycles?from="+tc.from+"&to="+tc.to, &got)
			wantFrom, _ := strconv.ParseInt(strings.TrimPrefix(tc.from, "%2B"), 10, 64)
			wantTo, _ := strconv.ParseInt(strings.TrimPrefix(tc.to, "%2B"), 10, 64)
			if !got.From.Equal(time.Unix(wantFrom, 0)) || !got.To.Equal(time.Unix(wantTo, 0)) {
				t.Fatalf("echoed %s..%s, want %s..%s", got.From.UTC(), got.To.UTC(),
					time.Unix(wantFrom, 0).UTC(), time.Unix(wantTo, 0).UTC())
			}
		})
	}
}

// The other half of the same precedence rule: a signed value carrying a unit
// is still an offset from now, and must not be read as a unix second.
func TestSignedDurationsStayRelative(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"-1h":    -time.Hour,
		"%2B30m": 30 * time.Minute,
		"-7d":    -7 * 24 * time.Hour,
		"-1w":    -7 * 24 * time.Hour,
		"-90s":   -90 * time.Second,
	} {
		t.Run("at="+raw, func(t *testing.T) {
			reader := &stubReader{}
			h := newTestServer(t, withReader(reader))
			before := time.Now()
			code, body := do(t, h, http.MethodGet, "/api/v1/targets/core/gw/hops?at="+raw)
			if code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", code, body)
			}
			// A relative `at` is coalesced onto the pinned read's cache
			// quantum, so it floors backwards by up to one quantum — while a
			// unix-second misreading would land decades away.
			if got := reader.lastAt; !got.Equal(storage.CoalesceHopsAt(got)) {
				t.Fatalf("at=%s resolved to %s, which is not on the cache quantum", raw, got.UTC())
			}
			if drift := reader.lastAt.Sub(before.Add(want)); drift < -time.Minute || drift > 5*time.Second {
				t.Fatalf("at=%s resolved to %s, want ~%s (drift %s)", raw, reader.lastAt.UTC(), before.Add(want).UTC(), drift)
			}
		})
	}

	h := newTestServer(t, withReader(&stubReader{}))
	var got struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	before := time.Now()
	doJSON(t, h, "GET", "/api/v1/targets/core/gw/cycles?from=-1h&to=%2B30m", &got)
	if drift := got.From.Sub(before.Add(-time.Hour)); drift < 0 || drift > 5*time.Second {
		t.Fatalf("from=-1h echoed %s, want ~%s", got.From.UTC(), before.Add(-time.Hour).UTC())
	}
	if drift := got.To.Sub(before.Add(30 * time.Minute)); drift < 0 || drift > 5*time.Second {
		t.Fatalf("to=+30m echoed %s, want ~%s", got.To.UTC(), before.Add(30*time.Minute).UTC())
	}
}

// Reading a signed value as an instant hands ValidQueryTime a whole new way to
// reach outside the storable range, so the guard has to bind the signed form
// exactly as it binds RFC3339: both inclusive edges are ordinary requests and
// one second past either is a 400 that never reaches the reader.
func TestSignedUnixSecondsStayInsideTheStorableRange(t *testing.T) {
	minSec, maxSec := storage.MinQueryTime.Unix(), storage.MaxQueryTime.Unix()
	for _, tc := range []struct {
		at   string
		want int
	}{
		{strconv.FormatInt(minSec, 10), http.StatusOK},
		{strconv.FormatInt(minSec-1, 10), http.StatusBadRequest},
		{strconv.FormatInt(maxSec, 10), http.StatusOK},
		{strconv.FormatInt(maxSec+1, 10), http.StatusBadRequest},
		{"-99999999999", http.StatusBadRequest},
	} {
		t.Run("at="+tc.at, func(t *testing.T) {
			reader := &stubReader{}
			h := newTestServer(t, withReader(reader))
			code, body := do(t, h, http.MethodGet, "/api/v1/targets/core/gw/hops?at="+tc.at)
			if code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", code, body, tc.want)
			}
			if tc.want == http.StatusBadRequest && reader.queries != 0 {
				t.Fatalf("reader ran %d queries for a refused instant", reader.queries)
			}
		})
	}
}

// A bare `+` in a query value is a space to net/http's own parser, so the
// plus-prefixed forms above only reach the handler percent-encoded. Pinned so
// the 400 is understood as the encoding rule it is, not as a parse bug.
func TestBarePlusInAQueryValueIsRefused(t *testing.T) {
	code, _ := do(t, newTestServer(t, withReader(&stubReader{})), http.MethodGet,
		"/api/v1/targets/core/gw/hops?at=+1775001600")
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 — an unencoded + arrives as a leading space", code)
	}
}

// Reading a signed value as an instant is a new way to name a window edge, so
// the window caps have to bind it exactly as they bind RFC3339 — a 400 years
// wide span expressed in signed unix seconds is refused before the read, not
// waved through as a relative offset the parser never resolved.
func TestSignedUnixSecondsStillObeyTheWindowCaps(t *testing.T) {
	span := "from=" + strconv.FormatInt(storage.MinQueryTime.Unix(), 10) +
		"&to=" + strconv.FormatInt(storage.MaxQueryTime.Unix(), 10)
	// The refusal must name the cap. A parse error is also a 400, so a status
	// check alone would pass on the very code that never resolved the window.
	for path, want := range map[string]string{
		"/api/v1/targets/core/gw/rtts?" + span:                        "rtts window limited to 24h",
		"/api/v1/targets/core/gw/http?" + span:                        "http window limited to 7d",
		"/api/v1/targets/core/gw/hops/timeline?source=master&" + span: "hops/timeline window limited to 7d",
		"/api/v1/targets/core/gw/cycles?step=raw&" + span:             "requested step is finer than the bucket ladder serves",
	} {
		t.Run(path, func(t *testing.T) {
			reader := &stubReader{}
			code, body := do(t, newTestServer(t, withReader(reader)), http.MethodGet, path)
			if code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", code, body)
			}
			if !strings.Contains(string(body), want) {
				t.Fatalf("body=%s, want the cap refusal %q", body, want)
			}
			if reader.queries != 0 {
				t.Fatalf("reader ran %d queries for a refused window", reader.queries)
			}
		})
	}
}

// The window scans statusRecentCycles intervals, which is that many cycles per
// source — so trimming to that many across every source made the window and
// the trim describe different quantities: on a six-source install the endpoint
// scanned six times what it returned and showed eight cycles per source. It
// also echoes the window, because a target silent longer than it comes back
// empty and a caller cannot otherwise tell that apart from a target that never
// existed.
func TestStatusTrimsPerSourceAndEchoesItsWindow(t *testing.T) {
	const sources = 6
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	r := &stubReader{}
	for i := range statusRecentCycles + 10 {
		for s := range sources {
			r.cycles = append(r.cycles, storage.CyclePoint{
				Time:   base.Add(time.Duration(i) * time.Minute),
				Source: fmt.Sprintf("edge-%d", s),
			})
		}
	}

	code, body := do(t, newTestServer(t, withReader(r)), http.MethodGet, "/api/v1/targets/core/gw/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, body)
	}
	var got struct {
		Recent []storage.CyclePoint `json:"recent"`
		From   time.Time            `json:"from"`
		To     time.Time            `json:"to"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.From.IsZero() || got.To.IsZero() {
		t.Fatal("/status echoes no window, so an empty recent is indistinguishable from a target that never existed")
	}
	per := map[string]int{}
	for _, p := range got.Recent {
		per[p.Source]++
	}
	if len(per) != sources {
		t.Fatalf("%d sources survived the trim, want %d — the trim is across sources, not per source", len(per), sources)
	}
	for src, n := range per {
		if n != statusRecentCycles {
			t.Errorf("source %s kept %d cycles, want %d", src, n, statusRecentCycles)
		}
	}
	// The newest must survive: an operator opens /status during an incident.
	newest := base.Add(time.Duration(statusRecentCycles+9) * time.Minute).Unix()
	for _, p := range got.Recent[len(got.Recent)-sources:] {
		if p.Time.Unix() != newest {
			t.Fatalf("trim dropped the newest cycles, keeping up to %s", p.Time)
		}
	}
}

// cycleCounterDTOs serves storage.CycleCounters as-is, so a field added to
// that struct publishes itself on an unauthenticated endpoint without anyone
// deciding to. This is the decision point: adding a field means updating this
// list, which is where the question gets asked.
func TestCycleCountersPublishOnlyTheFieldsWeChose(t *testing.T) {
	want := []string{"Source", "Time", "Sent", "LossCount", "LossPct"}
	rt := reflect.TypeOf(storage.CycleCounters{})
	var got []string
	for i := range rt.NumField() {
		got = append(got, rt.Field(i).Name)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("storage.CycleCounters is %v, want %v — /hops serves it verbatim to anonymous callers", got, want)
	}
}

// The `?step=` override is bounded by what it costs, not by how it compares to
// the derived tier's width. Comparing widths put a cliff between 30d (720
// buckets at step=1h) and 31d (744) — both inside the 500-1000 the ladder
// itself targets — while the real protection is against 365d at step=1h, which
// is 8760. The ceiling also rises with the ladder: past ~1000d the coarsest
// tier already exceeds it, so a flat bound would refuse `?step=1d` on a window
// about to be served at exactly that step.
func TestStepOverrideIsBoundedByCostNotByTierWidth(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/v1/targets/core/gw/cycles?from=-30d&step=1h", http.StatusOK},
		{"/api/v1/targets/core/gw/cycles?from=-31d&step=1h", http.StatusOK},
		{"/api/v1/targets/core/gw/cycles?from=-41d&step=1h", http.StatusOK},
		{"/api/v1/targets/core/gw/cycles?from=-180d&step=1h", http.StatusBadRequest},
		{"/api/v1/targets/core/gw/cycles?from=-365d&step=1h", http.StatusBadRequest},
		{"/api/v1/targets/core/gw/cycles?from=-3000d&step=1d", http.StatusOK},
	} {
		t.Run(tc.path, func(t *testing.T) {
			code, body := do(t, newTestServer(t, withReader(&stubReader{})), http.MethodGet, tc.path)
			if code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", code, tc.want, body)
			}
		})
	}
}

// /status trims per source, which bounds no total: the window is
// statusRecentCycles cycles *per source* and a target's distinct origins are
// bounded only by the registry's 512, so one anonymous GET could buffer and
// marshal 25,600 points. And the derived window is not monotonically small —
// config admits an interval up to config.MaxProbeInterval, where
// statusRecentCycles x interval is ~59.7h, wider than the fixed 24h it
// replaced.
func TestStatusBoundsItsResponseAndItsWindow(t *testing.T) {
	t.Run("response", func(t *testing.T) {
		base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		r := &stubReader{}
		for s := range statusMaxSources * 4 {
			for i := range statusRecentCycles + 5 {
				r.cycles = append(r.cycles, storage.CyclePoint{
					Time:   base.Add(time.Duration(i) * time.Minute),
					Source: fmt.Sprintf("edge-%03d", s),
				})
			}
		}
		_, body := do(t, newTestServer(t, withReader(r)), http.MethodGet, "/api/v1/targets/core/gw/status")
		var got struct {
			Recent []storage.CyclePoint `json:"recent"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		sources := map[string]int{}
		for _, p := range got.Recent {
			sources[p.Source]++
		}
		if len(sources) > statusMaxSources {
			t.Fatalf("served %d sources, past the %d ceiling — one anonymous GET marshals the product of that and the per-source trim", len(sources), statusMaxSources)
		}
		for src, n := range sources {
			if n != statusRecentCycles {
				t.Fatalf("source %s served %d cycles, want a complete %d — sources are dropped whole, not shortened", src, n, statusRecentCycles)
			}
		}
	})

	t.Run("window", func(t *testing.T) {
		r := &stubReader{}
		h := newTestServerWithInterval(t, config.MaxProbeInterval, withReader(r))
		if code, body := do(t, h, http.MethodGet, "/api/v1/targets/core/gw/status"); code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, body)
		}
		if got := r.lastTo.Sub(r.lastFrom); got > statusWindowCap {
			t.Fatalf("scan window = %s at the widest configurable interval, past the %s ceiling the fixed window carried", got, statusWindowCap)
		}
	})
}

// A cap that trims in silence is the defect the hop caps exist not to have:
// a caller cannot tell a dropped origin from one that never probed the target.
func TestStatusReportsTheSourcesItOmitted(t *testing.T) {
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	r := &stubReader{}
	const extra = 5
	for s := range statusMaxSources + extra {
		// Distinct newest timestamps, so the selection is by recency and not
		// by the alphabetical tiebreak.
		r.cycles = append(r.cycles, storage.CyclePoint{
			Time:   base.Add(time.Duration(s) * time.Minute),
			Source: fmt.Sprintf("edge-%03d", s),
		})
	}
	_, body := do(t, newTestServer(t, withReader(r)), http.MethodGet, "/api/v1/targets/core/gw/status")
	var got struct {
		Recent  []storage.CyclePoint `json:"recent"`
		Omitted int                  `json:"sources_omitted"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Omitted != extra {
		t.Fatalf("sources_omitted = %d, want %d — a silently dropped origin is indistinguishable from one that never reported", got.Omitted, extra)
	}
	// The freshest must survive, not the alphabetically first.
	kept := map[string]bool{}
	for _, p := range got.Recent {
		kept[p.Source] = true
	}
	if !kept[fmt.Sprintf("edge-%03d", statusMaxSources+extra-1)] {
		t.Fatal("the most recently active source was dropped — the selection is not by recency")
	}
	if kept["edge-000"] {
		t.Fatal("the least recently active source survived — selection fell back to alphabetical order")
	}
}

// The documented boundary itself: 41d is served, 42d is the first refusal.
// Without a row at the refusing side the constant is pinned from one direction
// only, and README quotes the figure.
func TestStepOverrideRefusesAtItsDocumentedBoundary(t *testing.T) {
	for _, tc := range []struct {
		days int
		want int
	}{
		{41, http.StatusOK},
		{42, http.StatusBadRequest},
	} {
		path := fmt.Sprintf("/api/v1/targets/core/gw/cycles?from=-%dd&step=1h", tc.days)
		code, body := do(t, newTestServer(t, withReader(&stubReader{})), http.MethodGet, path)
		if code != tc.want {
			t.Fatalf("%dd at step=1h: status %d, want %d (body %s)", tc.days, code, tc.want, body)
		}
	}
}
