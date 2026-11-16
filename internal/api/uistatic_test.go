package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/tumult/gosmokeping/internal/cluster/master"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	embeddedui "github.com/tumult/gosmokeping/internal/ui"
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
// listed route per name drifts and serveStaticOutsideAPI consults the embedded FS.
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

func assertNoStore(t *testing.T, target string, res *http.Response) {
	t.Helper()
	if got := res.Header.Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
		t.Errorf("%s: Cache-Control = %q on status %d, want exactly no-store, so no shared cache keeps it under the key of a real file", target, got, res.StatusCode)
	}
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
	assertNoStore(t, target, res)
}

func assertJSONNotFound(t *testing.T, target string, res *http.Response, body string) {
	t.Helper()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("%s: status = %d, want 404", target, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type = %q, want application/json", target, ct)
	}
	assertNoStore(t, target, res)
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
		"/api/v1/%74argets",
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
		noStore   = "no-store"
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
		{target: fixtureAssetJS, header: staleETag, wantStatus: http.StatusPreconditionFailed, wantCache: noStore},
		{target: "/theme.js", header: staleETag, wantStatus: http.StatusPreconditionFailed, wantCache: noStore},
		{target: fixtureAssetJS, header: anyETag, wantStatus: http.StatusNotModified, wantCache: immutable},
		{target: "/theme.js", header: anyETag, wantStatus: http.StatusNotModified, wantCache: noCache},
		{target: fixtureAssetJS, header: http.Header{"Range": {"bytes=0-3"}}, wantStatus: http.StatusPartialContent, wantCache: immutable},
		{target: fixtureAssetJS, header: http.Header{"Range": {"bytes=9999-"}}, wantStatus: http.StatusRequestedRangeNotSatisfiable, wantCache: noStore},
		{target: "/index.html", wantStatus: http.StatusMovedPermanently, wantCache: noStore},
		{target: "/assets/does-not-exist.js", wantStatus: http.StatusNotFound, wantCache: noStore},
		{target: "/does-not-exist.js", wantStatus: http.StatusNotFound, wantCache: noStore},
		{target: "/api/v1/health", wantStatus: http.StatusOK},
		{target: "/api/v1/does-not-exist", wantStatus: http.StatusNotFound, wantCache: noStore},
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

var staticFileTargets = []struct {
	target string
	fsName string
}{
	{target: "/", fsName: "index.html"},
	{target: fixtureAssetJS, fsName: strings.TrimPrefix(fixtureAssetJS, "/")},
	{target: fixtureAssetCSS, fsName: strings.TrimPrefix(fixtureAssetCSS, "/")},
	{target: "/theme.js", fsName: "theme.js"},
	{target: "/favicon.svg", fsName: "favicon.svg"},
	{target: "/icon.svg", fsName: "icon.svg"},
}

func assertHeadMatchesGet(t *testing.T, h http.Handler, target string) {
	t.Helper()
	getRes := get(t, h, target, nil)
	readBody(t, getRes)
	headRes := send(t, h, http.MethodHead, target, nil)
	headBody := readBody(t, headRes)
	if headRes.StatusCode != getRes.StatusCode {
		t.Errorf("HEAD %s: status = %d, want %d as for GET", target, headRes.StatusCode, getRes.StatusCode)
	}
	if !maps.EqualFunc(headRes.Header, getRes.Header, slices.Equal) {
		t.Errorf("HEAD %s: headers = %v, want the headers of GET %v", target, headRes.Header, getRes.Header)
	}
	if headBody != "" {
		t.Errorf("HEAD %s: body = %q, want none", target, headBody)
	}
	assertSecurityHeaders(t, headRes.Header)
}

func TestStaticFilesAnswerHeadWithTheStatusAndHeadersOfGet(t *testing.T) {
	uiFiles := testUIFS()
	h := newTestServer(t, withUIFS(uiFiles))
	for _, file := range staticFileTargets {
		t.Run(file.target, func(t *testing.T) {
			res := get(t, h, file.target, nil)
			if body := readBody(t, res); res.StatusCode != http.StatusOK || body != string(uiFiles[file.fsName].Data) {
				t.Fatalf("precondition: GET %s = %d %q, want 200 with the embedded file", file.target, res.StatusCode, body)
			}
			assertHeadMatchesGet(t, h, file.target)
		})
	}
}

func TestOtherMethodsOnAStaticFileGet405WithAllowGetHead(t *testing.T) {
	uiFiles := testUIFS()
	h := newTestServer(t, withUIFS(uiFiles))
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace, "PROPFIND"}
	for _, file := range staticFileTargets {
		for _, method := range methods {
			row := method + " " + file.target
			t.Run(row, func(t *testing.T) {
				res := send(t, h, method, file.target, nil)
				body := readBody(t, res)
				if res.StatusCode != http.StatusMethodNotAllowed {
					t.Errorf("%s: status = %d, want 405", row, res.StatusCode)
				}
				if got := res.Header.Values("Allow"); len(got) != 1 || got[0] != "GET, HEAD" {
					t.Errorf("%s: Allow = %q, want exactly \"GET, HEAD\"", row, got)
				}
				if strings.Contains(body, string(uiFiles[file.fsName].Data)) {
					t.Errorf("%s: body holds the file: %q", row, body)
				}
				assertNoStore(t, row, res)
				assertSecurityHeaders(t, res.Header)
			})
		}
	}
}

func TestUnknownPathsGet404(t *testing.T) {
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions}
	textMethods := append(slices.Clone(methods), "PROPFIND")
	routers := []struct {
		name      string
		router    http.Handler
		textPaths []string
	}{
		{
			name:      "with UI",
			router:    newTestServer(t, withUIFS(testUIFS())),
			textPaths: []string{"/does-not-exist", "/favicon.ico", "/assets/does-not-exist.js", "/assets", "/index.html/"},
		},
		{
			name:      "headless",
			router:    newTestServer(t),
			textPaths: []string{"/", "/theme.js", fixtureAssetJS, "/favicon.ico"},
		},
	}
	apiPaths := []string{"/api/v1/does-not-exist", "/api/v1/%74argets", "/api"}
	for _, rt := range routers {
		for _, method := range textMethods {
			for _, target := range rt.textPaths {
				row := rt.name + " " + method + " " + target
				t.Run(row, func(t *testing.T) {
					res := send(t, rt.router, method, target, nil)
					assertTextNotFound(t, row, res, readBody(t, res))
					if got := res.Header.Values("Allow"); len(got) != 0 {
						t.Errorf("%s: Allow = %q, want none on a 404", row, got)
					}
					assertSecurityHeaders(t, res.Header)
				})
			}
		}
		for _, method := range methods {
			for _, target := range apiPaths {
				row := rt.name + " " + method + " " + target
				t.Run(row, func(t *testing.T) {
					res := send(t, rt.router, method, target, nil)
					assertJSONNotFound(t, row, res, readBody(t, res))
					assertSecurityHeaders(t, res.Header)
				})
			}
		}
	}
}

func TestStaticHandlerRefusesANameWhoseLastElementStartsWithADot(t *testing.T) {
	uiFiles := testUIFS()
	uiFiles[".gitkeep"] = &fstest.MapFile{}
	uiFiles["assets/.cache-AbCd12_-.js"] = &fstest.MapFile{Data: []byte("hidden")}
	uiFiles[".well-known/security.txt"] = &fstest.MapFile{Data: []byte("Contact: mailto:security@example.com\n")}
	h := newTestServer(t, withUIFS(uiFiles))
	for _, target := range []string{"/.gitkeep", "/assets/.cache-AbCd12_-.js"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			row := method + " " + target
			res := send(t, h, method, target, nil)
			assertTextNotFound(t, row, res, readBody(t, res))
		}
	}
	const wellKnown = "/.well-known/security.txt"
	res := get(t, h, wellKnown, nil)
	if body := readBody(t, res); res.StatusCode != http.StatusOK || body != string(uiFiles[".well-known/security.txt"].Data) {
		t.Errorf("%s: status = %d, body = %q, want 200 with the file: only the last element is checked for a dot", wellKnown, res.StatusCode, body)
	}
}

type anyNameFS struct {
	opened []string
}

func (f *anyNameFS) Open(name string) (fs.File, error) {
	f.opened = append(f.opened, name)
	return fstest.MapFS{"file.js": {Data: []byte("served for any name")}}.Open("file.js")
}

func TestStaticHandlerHandsTheUIFSOnlyValidNames(t *testing.T) {
	anyName := &anyNameFS{}
	h := newTestServer(t, withUIFS(anyName))
	if res := get(t, h, "/valid.js", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("precondition: the fixture answers /valid.js with status %d, want 200 for each name", res.StatusCode)
	}
	for _, target := range []string{"/%FF", "/assets/%FF.js"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if req.URL.RawPath != "" || req.URL.Path != path.Clean(req.URL.Path) || utf8.ValidString(req.URL.Path) {
			t.Fatalf("precondition: %s parses to Path %q and RawPath %q, so the row does not reach the name check", target, req.URL.Path, req.URL.RawPath)
		}
		anyName.opened = nil
		res := get(t, h, target, nil)
		assertTextNotFound(t, target, res, readBody(t, res))
		if len(anyName.opened) != 0 {
			t.Errorf("%s: the UI FS got %q, want no call for a name that fs.ValidPath refuses", target, anyName.opened)
		}
	}
}

func TestEmbeddedBuildFollowsTheStaticFileRules(t *testing.T) {
	const immutable = "public, max-age=31536000, immutable"
	requireUI := os.Getenv("GOSMOKEPING_REQUIRE_UI") == "1"
	dist := embeddedui.FS()
	if dist == nil {
		if requireUI {
			t.Fatal("dist not built and GOSMOKEPING_REQUIRE_UI=1: run `make ui`")
		}
		t.Skip("dist not built; run `make ui`")
	}
	h := newTestServer(t, withUIFS(dist))
	var dotFiles, assetFiles, rootFiles int
	err := fs.WalkDir(dist, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		target := "/" + name
		if name == "index.html" {
			target = "/"
		}
		if strings.HasPrefix(path.Base(name), ".") {
			dotFiles++
			res := get(t, h, target, nil)
			assertTextNotFound(t, target, res, readBody(t, res))
			return nil
		}
		wantCache := "no-cache"
		if strings.HasPrefix(name, "assets/") {
			wantCache = immutable
			assetFiles++
		} else {
			rootFiles++
		}
		res := get(t, h, target, nil)
		readBody(t, res)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", target, res.StatusCode)
		}
		if got := res.Header.Values("Cache-Control"); len(got) != 1 || got[0] != wantCache {
			t.Errorf("GET %s: Cache-Control = %q, want exactly %q", target, got, wantCache)
		}
		assertHeadMatchesGet(t, h, target)
		post := send(t, h, http.MethodPost, target, nil)
		readBody(t, post)
		if post.StatusCode != http.StatusMethodNotAllowed || post.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("POST %s: status = %d, Allow = %q, want 405 with GET, HEAD", target, post.StatusCode, post.Header.Get("Allow"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the embedded build: %v", err)
	}
	if assetFiles == 0 || rootFiles == 0 {
		t.Errorf("the build holds %d asset files and %d root files, want at least one of each", assetFiles, rootFiles)
	}
	if requireUI && dotFiles == 0 {
		t.Error("precondition: the build holds no dot file, so the dot rule did not run on it; npm run build writes .gitkeep")
	}
}

type seenRequest struct {
	method string
	path   string
}

type requestRecorder struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (rec *requestRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, seenRequest{method: r.Method, path: r.URL.Path})
		rec.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (rec *requestRecorder) take() []seenRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	seen := rec.seen
	rec.seen = nil
	return seen
}

func sendRequestLine(t *testing.T, addr, requestLine, method string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\r\nHost: example.com\r\nConnection: close\r\n\r\n", requestLine); err != nil {
		t.Fatalf("write %q: %v", requestLine, err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read the response to %q: %v", requestLine, err)
	}
	return res, readBody(t, res)
}

func headerWithoutDate(header http.Header) http.Header {
	clone := header.Clone()
	clone.Del("Date")
	return clone
}

func startRecordedServer(t *testing.T, opts ...testOpt) (*requestRecorder, string) {
	t.Helper()
	rec := &requestRecorder{}
	srv := httptest.NewUnstartedServer(rec.wrap(newTestServer(t, opts...)))
	srv.Start()
	t.Cleanup(srv.Close)
	return rec, srv.Listener.Addr().String()
}

func TestEmptyRequestPathGetsTheAnswerOfSlash(t *testing.T) {
	uiFiles := testUIFS()
	rec, addr := startRecordedServer(t, withUIFS(uiFiles))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		want, wantBody := sendRequestLine(t, addr, method+" / HTTP/1.1", method)
		rec.take()
		requestLine := method + " http://example.com HTTP/1.1"
		got, gotBody := sendRequestLine(t, addr, requestLine, method)
		if seen := rec.take(); len(seen) != 1 || seen[0].path != "" {
			t.Fatalf("precondition: %s reached the handler as %+v, want one request with an empty URL.Path", requestLine, seen)
		}
		if want.StatusCode != http.StatusOK || got.StatusCode != want.StatusCode {
			t.Errorf("%s: status = %d, want %d as for %s /, which must be 200", requestLine, got.StatusCode, want.StatusCode, method)
		}
		if !maps.EqualFunc(headerWithoutDate(got.Header), headerWithoutDate(want.Header), slices.Equal) {
			t.Errorf("%s: headers = %v, want the headers of %s / %v", requestLine, got.Header, method, want.Header)
		}
		if gotBody != wantBody {
			t.Errorf("%s: body = %q, want the body of %s / %q", requestLine, gotBody, method, wantBody)
		}
		if method == http.MethodGet && gotBody != string(uiFiles["index.html"].Data) {
			t.Errorf("%s: body = %q, want the index page", requestLine, gotBody)
		}
		assertSecurityHeaders(t, got.Header)
	}

	const connect = "CONNECT example.com:443 HTTP/1.1"
	res, body := sendRequestLine(t, addr, connect, http.MethodConnect)
	if seen := rec.take(); len(seen) != 1 || seen[0].method != http.MethodConnect || seen[0].path != "" {
		t.Fatalf("precondition: %s reached the handler as %+v, want one CONNECT with an empty URL.Path", connect, seen)
	}
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("%s: status = %d, want 405 as for CONNECT /", connect, res.StatusCode)
	}
	if got := res.Header.Values("Allow"); len(got) != 1 || got[0] != "GET, HEAD" {
		t.Errorf("%s: Allow = %q, want exactly \"GET, HEAD\"", connect, got)
	}
	if strings.Contains(body, indexMarker) {
		t.Errorf("%s: body holds the index page: %q", connect, body)
	}
	assertNoStore(t, connect, res)
	assertSecurityHeaders(t, res.Header)
}

func TestAsteriskRequestTargetGetsNoFile(t *testing.T) {
	rec, addr := startRecordedServer(t, withUIFS(testUIFS()))

	const getAsterisk = "GET * HTTP/1.1"
	res, body := sendRequestLine(t, addr, getAsterisk, http.MethodGet)
	if seen := rec.take(); len(seen) != 1 || seen[0].path != "*" {
		t.Fatalf("precondition: %s reached the handler as %+v, want one request with URL.Path *", getAsterisk, seen)
	}
	assertTextNotFound(t, getAsterisk, res, body)
	assertSecurityHeaders(t, res.Header)

	const optionsAsterisk = "OPTIONS * HTTP/1.1"
	res, body = sendRequestLine(t, addr, optionsAsterisk, http.MethodOptions)
	if seen := rec.take(); len(seen) != 0 {
		t.Errorf("%s reached the handler as %+v, want net/http to answer it before the handler", optionsAsterisk, seen)
	}
	if res.StatusCode != http.StatusOK || body != "" {
		t.Errorf("%s: status = %d, body = %q, want the empty 200 of net/http", optionsAsterisk, res.StatusCode, body)
	}
}
