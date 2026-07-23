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
	"github.com/tumult/gosmokeping/internal/slavehealth"
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

// overviewHealthServer mirrors overviewTestServer but wires a health mesh:
// two slaves ("berlin", "nyc"), one health target apiece, and reader rows for
// the berlin health target only — so a source filter and the silent flag are
// both observable.
func overviewHealthServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Listen:   ":0",
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group:   "core",
			Targets: []config.Target{{Name: "gw", Host: "1.1.1.1", Probe: "icmp"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	now := time.Now().UTC()
	reader := &stubReader{overview: []storage.OverviewSourceRow{
		{Group: "core", Name: "gw", Source: "master", LossAvg: 1, LastSeen: now},
		// The mesh saw berlin at 100% loss from the master — a slave that is
		// down, which is precisely what the fleet dashboard has to surface.
		{Group: slavehealth.Group, Name: "berlin", Source: "master", LossAvg: 100, LossMax: 100, LastSeen: now},
	}}
	// Health refs carry a Host here on purpose: overview rows have no
	// address-bearing field, so a leak would show up as a failed decode or a
	// stray value rather than passing silently.
	health := stubHealth{refs: []config.TargetRef{
		{Group: slavehealth.Group, Target: config.Target{
			Name: "berlin", Title: "berlin", Probe: slavehealth.ProbeName, Host: "198.51.100.10",
		}},
		{Group: slavehealth.Group, Target: config.Target{
			Name: "nyc", Title: "nyc", Probe: slavehealth.ProbeName, Host: "198.51.100.20",
		}},
	}}
	s := New(Options{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  config.NewStore("/dev/null", cfg),
		Reader: reader,
		Slaves: stubSlaveLister{names: []string{"berlin", "nyc"}},
		Health: health,
	})
	return s.Router()
}

func overviewRows(t *testing.T, h http.Handler, query string) []overviewRowDTO {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/overview"+query, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rows []overviewRowDTO `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Rows
}

// The fleet dashboard iterated cfg.AllTargets() only, so a slave going down
// could never appear on it — health targets live outside the stored config.
func TestOverviewIncludesHealthTargets(t *testing.T) {
	rows := overviewRows(t, overviewHealthServer(t), "?window=-1h")

	byID := make(map[string]overviewRowDTO, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	berlin, ok := byID[slavehealth.Group+"/berlin"]
	if !ok {
		t.Fatalf("health target missing from overview: %v", byID)
	}
	if _, ok := byID[slavehealth.Group+"/nyc"]; !ok {
		t.Fatalf("second health target missing from overview: %v", byID)
	}
	if berlin.GroupTitle != slavehealth.GroupTitle {
		t.Errorf("group title = %q, want %q", berlin.GroupTitle, slavehealth.GroupTitle)
	}
	if berlin.ProbeType != "icmp" {
		t.Errorf("probe type = %q, want icmp", berlin.ProbeType)
	}
	if berlin.LossAvg == nil || *berlin.LossAvg != 100 {
		t.Errorf("loss_avg = %v, want 100 (the slave is down)", berlin.LossAvg)
	}
	// The ordinary target must still be there.
	if _, ok := byID["core/gw"]; !ok {
		t.Errorf("ordinary target dropped from overview: %v", byID)
	}
}

// The "By slave" view filters by source. A slave never probes its own health
// target, so filtering by berlin must show nyc's row and not berlin's.
func TestOverviewHealthTargetsHonourSourceFilter(t *testing.T) {
	rows := overviewRows(t, overviewHealthServer(t), "?window=-1h&source=berlin")

	ids := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		ids[r.ID] = struct{}{}
	}
	if _, ok := ids[slavehealth.Group+"/nyc"]; !ok {
		t.Errorf("berlin's view is missing peer nyc: %v", ids)
	}
	if _, ok := ids[slavehealth.Group+"/berlin"]; ok {
		t.Errorf("berlin's view includes its own health target, which it never probes: %v", ids)
	}
}
