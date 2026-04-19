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
	cycles []storage.CyclePoint
	rtts   []storage.RTTPoint
	http   []storage.HTTPPoint
	hops   []storage.HopPoint
	err    error
	// lastSource captures the source filter passed to the most recent query,
	// so tests can assert the handler threaded ?source=… correctly.
	lastSource string
}

func (s *stubReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res storage.Resolution, source string) ([]storage.CyclePoint, error) {
	s.lastSource = source
	return s.cycles, s.err
}

func (s *stubReader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.RTTPoint, error) {
	s.lastSource = source
	return s.rtts, s.err
}

func (s *stubReader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HTTPPoint, error) {
	s.lastSource = source
	return s.http, s.err
}

func (s *stubReader) QueryLatestHops(ctx context.Context, ref config.TargetRef, source string) ([]storage.HopPoint, error) {
	s.lastSource = source
	return s.hops, s.err
}

func (s *stubReader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, source string) ([]storage.HopPoint, error) {
	s.lastSource = source
	return s.hops, s.err
}

func (s *stubReader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HopPoint, error) {
	s.lastSource = source
	return s.hops, s.err
}

func newTestServer(t *testing.T, reader StorageReader) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		InfluxDB: config.InfluxDB{URL: "http://x", BucketRaw: "raw", Token: "secret"},
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
	sources, _ := body[0]["sources"].([]any)
	if len(sources) != 1 || sources[0] != "master" {
		t.Errorf("sources = %v, want [master]", sources)
	}
}

func TestListTargetsTitlesAndSlaves(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		InfluxDB: config.InfluxDB{URL: "http://x", BucketRaw: "raw"},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Title: "Core Infra",
			Targets: []config.Target{
				{Name: "gw", Title: "Gateway", Host: "1.1.1.1", Probe: "icmp", Slaves: []string{"eu-west"}},
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
	sources, _ := body[0]["sources"].([]any)
	if len(sources) != 1 || sources[0] != "eu-west" {
		t.Errorf("sources = %v, want [eu-west]", sources)
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

func TestListSourcesClusterSourceOverride(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		InfluxDB: config.InfluxDB{URL: "http://x", BucketRaw: "raw"},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "gw", Host: "1.1.1.1", Probe: "icmp"},
				{Name: "eu-gw", Host: "2.2.2.2", Probe: "icmp", Slaves: []string{"eu-west", "eu-central"}},
				{Name: "us-gw", Host: "3.3.3.3", Probe: "icmp", Slaves: []string{"eu-west"}}, // dup slave
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
		Reader: nil,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources", nil)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	var body struct {
		Sources []string `json:"sources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	// Expect master source first (from first locally-probed target), then slave
	// names in encounter order, de-duplicated.
	want := []string{"primary", "eu-west", "eu-central"}
	if len(body.Sources) != len(want) {
		t.Fatalf("sources = %v, want %v", body.Sources, want)
	}
	for i, s := range want {
		if body.Sources[i] != s {
			t.Errorf("sources[%d] = %q, want %q (full: %v)", i, body.Sources[i], s, body.Sources)
		}
	}
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
