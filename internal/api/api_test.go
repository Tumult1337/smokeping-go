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
	hops   []storage.HopPoint
	err    error
}

func (s *stubReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res storage.Resolution) ([]storage.CyclePoint, error) {
	return s.cycles, s.err
}

func (s *stubReader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time) ([]storage.RTTPoint, error) {
	return s.rtts, s.err
}

func (s *stubReader) QueryLatestHops(ctx context.Context, ref config.TargetRef) ([]storage.HopPoint, error) {
	return s.hops, s.err
}

func (s *stubReader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration) ([]storage.HopPoint, error) {
	return s.hops, s.err
}

func (s *stubReader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time) ([]storage.HopPoint, error) {
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
}
