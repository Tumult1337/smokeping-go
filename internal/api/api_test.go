package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
	"github.com/tumult/gosmokeping/internal/storage"
)

type stubReader struct {
	cycles   []storage.CyclePoint
	rtts     []storage.RTTPoint
	http     []storage.HTTPPoint
	hops     []storage.HopPoint
	overview []storage.OverviewSourceRow
	err      error
	// lastSource captures the source filter passed to the most recent query,
	// so tests can assert the handler threaded ?source=… correctly.
	lastSource string
	// lastOverviewWindow records the (to - from) span passed to QueryOverview
	// so tests can assert ?window= was honoured.
	lastOverviewWindow time.Duration
	// lastOverviewTargets records how many target refs were passed in, so a
	// test can assert the handler scopes the query to configured targets.
	lastOverviewTargets int
}

func (s *stubReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.CyclePoint, error) {
	s.lastSource = f.Source
	return s.cycles, s.err
}

func (s *stubReader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.RTTPoint, error) {
	s.lastSource = f.Source
	return s.rtts, s.err
}

func (s *stubReader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HTTPPoint, error) {
	s.lastSource = f.Source
	return s.http, s.err
}

func (s *stubReader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f storage.QueryFilter) ([]storage.HopPoint, error) {
	s.lastSource = f.Source
	return s.hops, s.err
}

func (s *stubReader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) ([]storage.HopPoint, error) {
	s.lastSource = f.Source
	return s.hops, s.err
}

func (s *stubReader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HopPoint, error) {
	s.lastSource = f.Source
	return s.hops, s.err
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

func newTestServer(t *testing.T, opts ...testOpt) http.Handler {
	t.Helper()
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
			Group:     "core",
			Name:      "gw",
			Source:    "master",
			LossAvg:   12.4,
			LossMax:   50.0,
			RTTMedian: 18.2,
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
				Group:     "core",
				Name:      "gw",
				Source:    "master",
				LossAvg:   0.0,
				LossMax:   0.0,
				RTTMedian: 8.0,
				RTTP95:    12.0,
				RTTMax:    25.0,
				LastSeen:  now,
			},
			{
				Group:     "core",
				Name:      "gw",
				Source:    "eu-west",
				LossAvg:   18.0,
				LossMax:   50.0,
				RTTMedian: 40.0,
				RTTP95:    80.0,
				RTTMax:    200.0,
				LastSeen:  now,
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
			Group:     "core",
			Name:      "gw",
			Source:    "master",
			LossAvg:   0.0,
			RTTMedian: 8.0,
			LastSeen:  time.Now().Add(-10 * time.Minute),
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
			Host: "198.51.100.10",
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
// apparent terminal one — queryHopsBucketed's GROUP BY makes "terminal" an
// ambiguous notion within a bucket (see redactAllHopAddresses). Both
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
	doJSON(t, srv, "GET", "/api/v1/targets/_cluster/tokyo-1/hops/timeline", &body)

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
	doJSON(t, srv, "GET", "/api/v1/targets/core/gw/hops/timeline", &body)

	if len(body.Hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(body.Hops), body.Hops)
	}
	for _, h := range body.Hops {
		if h.Index == 2 && h.IP != "10.44.0.2" {
			t.Fatalf("ordinary target's terminal hop was redacted: %+v", h)
		}
	}
}

// TestRedactTerminalHopPerTimestamp covers /hops/timeline, where
// QueryHopsTimeline returns one row per (bucket_timestamp, source, ttl,
// hop_addr) spanning a whole window rather than a single pinned timestamp.
// A route change partway through the window shortens or lengthens the path,
// so the terminal index for the same source differs bucket to bucket. Keying
// the max by source alone (the /hops-only-correct behaviour) computes one
// window-wide maximum and misses the terminal hop at every bucket whose path
// length is shorter than the window's longest — leaking the slave's address
// for those buckets straight into hopTimelineDTO.IP.
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
// redactTerminalHops cannot close: queryHopsBucketed groups by (bucket_ts,
// source, ttl, hop_addr), so a single (source, bucket) pair can carry more
// than one row at the *same* ttl with different addresses (a mid-bucket
// route change), and a longer trace than whichever row happens to sit at the
// apparent maximum index. A positional "blank only the max index" pass —
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
