package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tumult/gosmokeping/internal/cluster/master"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

const indexMarker = "<!doctype html"

func get(t *testing.T, h http.Handler, target string, header http.Header) *http.Response {
	t.Helper()
	return send(t, h, http.MethodGet, target, header)
}

func send(t *testing.T, h http.Handler, method, target string, header http.Header) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for name, values := range header {
		req.Header[name] = values
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// `npm run icon` and Vite's public/ copy decide the root build files, so a
// listed route per name drifts and the fallback consults the embedded FS.
func TestRootBuildFilesAreServedAndUnknownPathsGet404(t *testing.T) {
	ui := testUIFS()
	h := newTestServer(t, withUIFS(ui))

	served := []struct {
		target   string
		fsName   string
		wantType string
	}{
		{target: "/favicon.svg", fsName: "favicon.svg", wantType: "svg"},
		{target: "/icon.svg", fsName: "icon.svg", wantType: "svg"},
		{target: "/theme.js", fsName: "theme.js", wantType: "javascript"},
		{target: fixtureAssetJS, fsName: strings.TrimPrefix(fixtureAssetJS, "/"), wantType: "javascript"},
		{target: fixtureAssetCSS, fsName: strings.TrimPrefix(fixtureAssetCSS, "/"), wantType: "text/css"},
	}
	for _, tc := range served {
		res := get(t, h, tc.target, nil)
		body := readBody(t, res)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tc.target, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
			t.Errorf("%s: Content-Type = %q, want a %s type", tc.target, ct, tc.wantType)
		}
		if want := string(ui[tc.fsName].Data); body != want {
			t.Errorf("%s: body = %q, want the embedded file %q", tc.target, body, want)
		}
	}

	res := get(t, h, "/some/app/route", nil)
	assertTextNotFound(t, "/some/app/route", res, readBody(t, res))
}

func assertTextNotFound(t *testing.T, target string, res *http.Response, body string) {
	t.Helper()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("%s: status = %d, want 404", target, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("%s: Content-Type = %q, want the text 404 of net/http", target, ct)
	}
	if strings.Contains(body, indexMarker) {
		t.Errorf("%s: served the index page: %q", target, body)
	}
}

func assertJSONNotFound(t *testing.T, target string, res *http.Response, body string) {
	t.Helper()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("%s: status = %d, want 404", target, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type = %q, want application/json", target, ct)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Errorf("%s: body %q is not the API error JSON: %v", target, body, err)
		return
	}
	if payload["error"] == "" || len(payload) != 1 {
		t.Errorf("%s: body = %q, want one non-empty \"error\" field", target, body)
	}
}

func TestUnknownPathsGet404WithJSONBelowAPI(t *testing.T) {
	routers := []struct {
		name   string
		router http.Handler
	}{
		{name: "with UI", router: newTestServer(t, withUIFS(testUIFS()))},
		{name: "headless", router: newTestServer(t)},
	}
	apiPaths := []string{
		"/api",
		"/api/",
		"/api/does-not-exist",
		"/api/v1/does-not-exist",
		"/api/v1/targets/core/gw/does-not-exist",
		"/api/v2/health",
		"//api/v1/does-not-exist",
		"/foo/../api/x",
		"/api%2Fv1%2Fdoes-not-exist",
	}
	otherPaths := []string{"/does-not-exist", "/some/app/route", "/apix", "/favicon.ico", "/api/../favicon.svg"}

	for _, rt := range routers {
		t.Run(rt.name, func(t *testing.T) {
			for _, target := range apiPaths {
				res := get(t, rt.router, target, nil)
				assertJSONNotFound(t, target, res, readBody(t, res))
			}
			for _, target := range otherPaths {
				res := get(t, rt.router, target, nil)
				assertTextNotFound(t, target, res, readBody(t, res))
			}
		})
	}
}

type discardSink struct{}

func (discardSink) OnCycle(context.Context, scheduler.Cycle) {}

func TestUnknownClusterPathAnswersAfterTheBearerCheck(t *testing.T) {
	const token = "cluster-test-token"
	log := slog.New(slog.DiscardHandler)
	masterStore := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: token}})
	clusterHandler := master.NewServer(log, masterStore, master.NewRegistry(log), discardSink{}, nil).Handler()
	h := newTestServer(t, withUIFS(testUIFS()), withClusterHandler(clusterHandler))
	const target = "/api/v1/cluster/does-not-exist"

	res := get(t, h, target, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("%s without a token: status = %d, want 401 from the bearer check", target, res.StatusCode)
	}
	assertSecurityHeaders(t, res.Header)

	res = get(t, h, target, http.Header{"Authorization": {"Bearer " + token}})
	assertJSONNotFound(t, target, res, readBody(t, res))
	assertSecurityHeaders(t, res.Header)
}

func TestStaticHandlerRefusesPercentEncodedAliases(t *testing.T) {
	h := newTestServer(t, withUIFS(testUIFS()))
	assetFile := strings.TrimPrefix(fixtureAssetJS, "/assets/")
	aliases := []struct {
		method string
		target string
	}{
		{method: http.MethodGet, target: "/assets%2F" + assetFile},
		{method: http.MethodPost, target: "/assets%2F" + assetFile},
		{method: http.MethodGet, target: "/%61ssets/" + assetFile},
		{method: http.MethodGet, target: "/theme%2Ejs"},
	}
	for _, alias := range aliases {
		row := alias.method + " " + alias.target
		if req := httptest.NewRequest(alias.method, alias.target, nil); req.URL.RawPath == "" {
			t.Fatalf("precondition: %s parses with an empty RawPath, so the row cannot reach the rule", row)
		}
		res := send(t, h, alias.method, alias.target, nil)
		assertTextNotFound(t, row, res, readBody(t, res))
		if got := res.Header.Values("Cache-Control"); len(got) != 0 {
			t.Errorf("%s: Cache-Control = %q, want none", row, got)
		}
	}
	for _, target := range []string{fixtureAssetJS, "/theme.js", "/favicon.svg"} {
		if res := get(t, h, target, nil); res.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 at the default encoding", target, res.StatusCode)
		}
	}
}

func TestStaticHandlerRefusesNonCanonicalPathsDirectoriesAndFilesOutsideTheUI(t *testing.T) {
	h := newTestServer(t, withUIFS(testUIFS()))
	assetName := strings.TrimPrefix(fixtureAssetJS, "/assets/")
	for _, target := range []string{
		"/assets/../favicon.svg",
		"/api/../favicon.svg",
		"/assets/../index.html",
		"/%2e%2e/api.go",
		"/assets/%2e%2e/favicon.svg",
		"/index.html/",
		"/favicon.svg/",
		fixtureAssetJS + "/",
		"/" + fixtureAssetJS,
		"/./favicon.svg",
		"/api.go",
		"/uistatic_test.go",
		"/assets",
		"/assets/",
	} {
		res := get(t, h, target, nil)
		body := readBody(t, res)
		assertTextNotFound(t, target, res, body)
		if strings.Contains(body, assetName) {
			t.Errorf("%s: body lists the directory: %q", target, body)
		}
		if strings.Contains(body, "package api") {
			t.Errorf("%s: body is a source file outside the UI", target)
		}
	}
}

func TestCacheControlFollowsTheFileKind(t *testing.T) {
	const (
		immutable = "public, max-age=31536000, immutable"
		noCache   = "no-cache"
	)
	h := newTestServer(t, withUIFS(testUIFS()))
	staleETag := http.Header{"If-Match": {`"stale"`}}
	anyETag := http.Header{"If-None-Match": {"*"}}
	cases := []struct {
		target     string
		header     http.Header
		wantStatus int
		wantCache  string
	}{
		{target: "/", wantStatus: http.StatusOK, wantCache: noCache},
		{target: "/theme.js", wantStatus: http.StatusOK, wantCache: noCache},
		{target: "/favicon.svg", wantStatus: http.StatusOK, wantCache: noCache},
		{target: "/icon.svg", wantStatus: http.StatusOK, wantCache: noCache},
		{target: fixtureAssetJS, wantStatus: http.StatusOK, wantCache: immutable},
		{target: fixtureAssetCSS, wantStatus: http.StatusOK, wantCache: immutable},
		{target: fixtureAssetJS, header: staleETag, wantStatus: http.StatusPreconditionFailed},
		{target: "/theme.js", header: staleETag, wantStatus: http.StatusPreconditionFailed},
		{target: fixtureAssetJS, header: anyETag, wantStatus: http.StatusNotModified, wantCache: immutable},
		{target: "/theme.js", header: anyETag, wantStatus: http.StatusNotModified, wantCache: noCache},
		{target: fixtureAssetJS, header: http.Header{"Range": {"bytes=0-3"}}, wantStatus: http.StatusPartialContent, wantCache: immutable},
		{target: fixtureAssetJS, header: http.Header{"Range": {"bytes=9999-"}}, wantStatus: http.StatusRequestedRangeNotSatisfiable},
		{target: "/index.html", wantStatus: http.StatusMovedPermanently},
		{target: "/assets/does-not-exist.js", wantStatus: http.StatusNotFound},
		{target: "/does-not-exist.js", wantStatus: http.StatusNotFound},
		{target: "/api/v1/health", wantStatus: http.StatusOK},
		{target: "/api/v1/does-not-exist", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		res := get(t, h, tc.target, tc.header)
		row := fmt.Sprintf("%s %v", tc.target, tc.header)
		if res.StatusCode != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", row, res.StatusCode, tc.wantStatus)
		}
		got := res.Header.Values("Cache-Control")
		if tc.wantCache == "" {
			if len(got) != 0 {
				t.Errorf("%s: Cache-Control = %q on status %d, want none", row, got, res.StatusCode)
			}
			continue
		}
		if len(got) != 1 || got[0] != tc.wantCache {
			t.Errorf("%s: Cache-Control = %q, want exactly %q", row, got, tc.wantCache)
		}
	}
}
