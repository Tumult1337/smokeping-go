package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tumult/gosmokeping/internal/config"
)

// The build emits favicon.svg and icon.svg at the root of dist and index.html
// references both, but only /assets/* and /favicon.ico were routed — so each
// matched NotFound and was answered with index.html at text/html, which a
// browser rejects as an icon. `npm run icon` regenerates those files, so a
// listed route per name drifts; the fallback consults the embedded FS instead.
func TestRootBuildAssetsAreServedNotSwallowedBySPAFallback(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":  {Data: []byte("<!doctype html><title>gosmokeping</title>")},
		"favicon.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"icon.svg":    {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
	cfg := &config.Config{
		Listen: ":8080", Interval: 30 * 1e9, Pings: 5,
		Storage: config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:  map[string]config.Probe{"icmp": {Type: "icmp", Timeout: 1e9}},
		Targets: []config.Group{{Group: "core", Targets: []config.Target{
			{Name: "gw", Host: "1.1.1.1", Probe: "icmp"},
		}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	h := New(Options{
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store: config.NewStore("/dev/null", cfg),
		UIFS:  ui,
	}).Router()

	for _, name := range []string{"/favicon.svg", "/icon.svg"} {
		req := httptest.NewRequest(http.MethodGet, name, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "svg") {
			t.Errorf("%s: Content-Type = %q, want an svg type — text/html is the SPA fallback, which browsers reject as an icon", name, ct)
		}
		if strings.Contains(rr.Body.String(), "<!doctype html") {
			t.Errorf("%s: served index.html instead of the asset", name)
		}
	}

	// The fallback must still answer an app route with the SPA, or deep links
	// stop working.
	req := httptest.NewRequest(http.MethodGet, "/some/app/route", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "<!doctype html") {
		t.Errorf("app route did not fall back to index.html: %q", rr.Body.String())
	}
}
