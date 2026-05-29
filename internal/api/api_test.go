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

func newTestServer(t *testing.T, reader storage.Reader) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		Storage: config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
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
	s := New(Options{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
		Reader: reader,
	})
	return s.Router()
}

func TestHealth(t *testing.T) {
	h := newTestServer(t, nil)
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
	h := newTestServer(t, nil)
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
		Storage: config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
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
	h := newTestServer(t, nil)
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
		Storage: config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
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
	h := newTestServer(t, nil)
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
		Storage: config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
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
	h := newTestServer(t, &stubReader{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets/doesnotexist/cycles", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestGetCyclesReturnsPoints(t *testing.T) {
	r := &stubReader{cycles: []storage.CyclePoint{{Time: time.Now(), Median: 5.0}}}
	h := newTestServer(t, r)
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
	Window string             `json:"window"`
	From   string             `json:"from"`
	To     string             `json:"to"`
	Rows   []overviewRowBody  `json:"rows"`
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
	h := newTestServer(t, r)
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
