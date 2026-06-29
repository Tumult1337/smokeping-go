package api

import (
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

// overviewTestServer builds a server with two targets (one unassigned, one
// pinned to slave "berlin"), two registered slaves, and a stub reader that
// returns one overview row per (target, source) so the source filter is
// observable.
func overviewTestServer(t *testing.T) http.Handler {
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
				{Name: "edge", Host: "2.2.2.2", Probe: "icmp", Slaves: []string{"berlin"}},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	now := time.Now().UTC()
	mk := func(name, source string, loss float64) storage.OverviewSourceRow {
		return storage.OverviewSourceRow{
			Group: "core", Name: name, Source: source,
			LossAvg: loss, LossMax: loss, RTTMedian: 10, RTTP95: 20, RTTMax: 30,
			LastSeen: now,
		}
	}
	reader := &stubReader{overview: []storage.OverviewSourceRow{
		mk("gw", "master", 1), mk("gw", "berlin", 2), mk("gw", "nyc", 3),
		mk("edge", "berlin", 9),
	}}
	store := config.NewStore("/dev/null", cfg)
	s := New(Options{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
		Reader: reader,
		Slaves: stubSlaveLister{names: []string{"berlin", "nyc"}},
	})
	return s.Router()
}

func overviewIDs(t *testing.T, h http.Handler, query string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview"+query, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rows []struct {
			ID      string   `json:"id"`
			LossAvg *float64 `json:"loss_avg"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make([]string, len(body.Rows))
	for i, r := range body.Rows {
		ids[i] = r.ID
	}
	return ids
}

func TestOverviewSourceSlaveSeesAssignedAndUnassigned(t *testing.T) {
	h := overviewTestServer(t)
	ids := overviewIDs(t, h, "?window=-1h&source=berlin")
	// berlin probes gw (unassigned → all sources) and edge (pinned to berlin).
	if len(ids) != 2 {
		t.Fatalf("want 2 targets, got %v", ids)
	}
}

func TestOverviewSourceExcludesUnassignedTarget(t *testing.T) {
	h := overviewTestServer(t)
	ids := overviewIDs(t, h, "?window=-1h&source=nyc")
	// nyc is not assigned to edge → only gw.
	if len(ids) != 1 || ids[0] != "core/gw" {
		t.Fatalf("want [core/gw], got %v", ids)
	}
}

func TestOverviewSourceMasterSkipsPinnedTarget(t *testing.T) {
	h := overviewTestServer(t)
	ids := overviewIDs(t, h, "?window=-1h&source=master")
	// edge is pinned to berlin → master skips it locally.
	if len(ids) != 1 || ids[0] != "core/gw" {
		t.Fatalf("want [core/gw], got %v", ids)
	}
}

func TestOverviewSourceUsesThatSourcesNumbers(t *testing.T) {
	h := overviewTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview?window=-1h&source=berlin", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body struct {
		Rows []struct {
			ID      string   `json:"id"`
			LossAvg *float64 `json:"loss_avg"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range body.Rows {
		if r.ID == "core/gw" {
			// berlin's gw row has loss 2 — not the worst-source (nyc=3) collapse.
			if r.LossAvg == nil || *r.LossAvg != 2 {
				t.Fatalf("want gw loss_avg=2 (berlin), got %v", r.LossAvg)
			}
		}
	}
}

func TestOverviewUnknownSourceIs400(t *testing.T) {
	h := overviewTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview?window=-1h&source=bogus", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOverviewNoSourceUnchanged(t *testing.T) {
	h := overviewTestServer(t)
	ids := overviewIDs(t, h, "?window=-1h")
	if len(ids) != 2 {
		t.Fatalf("want both targets without source filter, got %v", ids)
	}
}
