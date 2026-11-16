package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/fstest"
)

var wantSecurityHeaders = []struct {
	name  string
	value string
}{
	{"Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"},
	{"X-Content-Type-Options", "nosniff"},
	{"X-Frame-Options", "DENY"},
	{"Referrer-Policy", "strict-origin-when-cross-origin"},
	{"Permissions-Policy", "accelerometer=(), browsing-topics=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
}

const (
	fixtureAssetJS  = "/assets/index-AbCd12_-.js"
	fixtureAssetCSS = "/assets/index-Xy-9zZ0a.css"
)

func testUIFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                {Data: []byte("<!doctype html><title>gosmokeping</title>")},
		"favicon.svg":               {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"icon.svg":                  {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"theme.js":                  {Data: []byte("document.documentElement.dataset.theme = \"dark\";\n")},
		"assets/index-AbCd12_-.js":  {Data: []byte("console.log(\"app\");\n")},
		"assets/index-Xy-9zZ0a.css": {Data: []byte("body { margin: 0; }\n")},
	}
}

func withUIFS(ui fs.FS) testOpt {
	return func(o *Options) { o.UIFS = ui }
}

func withClusterHandler(h http.Handler) testOpt {
	return func(o *Options) { o.ClusterHandler = h }
}

func assertSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, want := range wantSecurityHeaders {
		got := header.Values(want.name)
		if len(got) != 1 || got[0] != want.value {
			t.Errorf("%s = %q, want exactly one value %q", want.name, got, want.value)
		}
	}
	if hsts := header.Values("Strict-Transport-Security"); len(hsts) != 0 {
		t.Errorf("Strict-Transport-Security = %q, want absent: the binary serves plain HTTP and the TLS proxy owns HSTS", hsts)
	}
}

type statusLog struct {
	header   http.Header
	statuses []int
	body     []byte
}

func (s *statusLog) Header() http.Header { return s.header }

func (s *statusLog) WriteHeader(status int) { s.statuses = append(s.statuses, status) }

func (s *statusLog) Write(body []byte) (int, error) {
	s.body = append(s.body, body...)
	return len(body), nil
}

func TestCacheControlWriterTreatsABareWriteAsStatus200(t *testing.T) {
	inner := &statusLog{header: http.Header{}}
	w := &successCacheControlWriter{ResponseWriter: inner, cacheControl: "no-cache"}
	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(inner.statuses) != 1 || inner.statuses[0] != http.StatusOK {
		t.Errorf("statuses forwarded = %v, want [200]", inner.statuses)
	}
	if got := inner.header.Values("Cache-Control"); len(got) != 1 || got[0] != "no-cache" {
		t.Errorf("Cache-Control = %q, want exactly no-cache on the implicit 200", got)
	}
	if string(inner.body) != "body" {
		t.Errorf("body = %q, want body", inner.body)
	}
}

func TestCacheControlWriterDecidesOnTheFirstStatusOnly(t *testing.T) {
	inner := &statusLog{header: http.Header{}}
	w := &successCacheControlWriter{ResponseWriter: inner, cacheControl: "no-cache"}
	w.WriteHeader(http.StatusPreconditionFailed)
	if _, err := w.Write([]byte("refused")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.WriteHeader(http.StatusOK)
	if want := []int{http.StatusPreconditionFailed, http.StatusOK}; !slices.Equal(inner.statuses, want) {
		t.Errorf("statuses forwarded = %v, want %v: a Write after a status adds none", inner.statuses, want)
	}
	if got := inner.header.Values("Cache-Control"); len(got) != 0 {
		t.Errorf("Cache-Control = %q after a 412 and a late 200, want none", got)
	}
}

func TestCacheControlWriterUnwrapsForTheResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &successCacheControlWriter{ResponseWriter: rec, cacheControl: "no-cache"}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("Flush through the wrapper: %v", err)
	}
	if !rec.Flushed {
		t.Error("the wrapped recorder was not flushed")
	}
}

func TestEveryResponseCarriesTheSecurityHeadersOnce(t *testing.T) {
	unauthorizedCluster := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	withUI := newTestServer(t, withUIFS(testUIFS()), withClusterHandler(unauthorizedCluster))
	headless := newTestServer(t)

	cases := []struct {
		name       string
		router     http.Handler
		method     string
		target     string
		acceptGzip bool
		wantStatus int
	}{
		{name: "index page", router: withUI, method: http.MethodGet, target: "/", wantStatus: http.StatusOK},
		{name: "asset", router: withUI, method: http.MethodGet, target: fixtureAssetJS, wantStatus: http.StatusOK},
		{name: "favicon", router: withUI, method: http.MethodGet, target: "/favicon.svg", wantStatus: http.StatusOK},
		{name: "theme script", router: withUI, method: http.MethodGet, target: "/theme.js", wantStatus: http.StatusOK},
		{name: "index.html redirect", router: withUI, method: http.MethodGet, target: "/index.html", wantStatus: http.StatusMovedPermanently},
		{name: "health", router: withUI, method: http.MethodGet, target: "/api/v1/health", wantStatus: http.StatusOK},
		{name: "gzip targets", router: withUI, method: http.MethodGet, target: "/api/v1/targets", acceptGzip: true, wantStatus: http.StatusOK},
		{name: "unknown root path", router: withUI, method: http.MethodGet, target: "/does-not-exist", wantStatus: http.StatusNotFound},
		{name: "unknown api v1 path", router: withUI, method: http.MethodGet, target: "/api/v1/does-not-exist", wantStatus: http.StatusNotFound},
		{name: "unknown api path", router: withUI, method: http.MethodGet, target: "/api/does-not-exist", wantStatus: http.StatusNotFound},
		{name: "cluster handler 401", router: withUI, method: http.MethodGet, target: "/api/v1/cluster/config", wantStatus: http.StatusUnauthorized},
		{name: "method not allowed", router: withUI, method: http.MethodPost, target: "/api/v1/health", wantStatus: http.StatusMethodNotAllowed},
		{name: "headless health", router: headless, method: http.MethodGet, target: "/api/v1/health", wantStatus: http.StatusOK},
		{name: "headless index page", router: headless, method: http.MethodGet, target: "/", wantStatus: http.StatusNotFound},
		{name: "headless unknown root path", router: headless, method: http.MethodGet, target: "/does-not-exist", wantStatus: http.StatusNotFound},
		{name: "headless unknown api path", router: headless, method: http.MethodGet, target: "/api/v1/does-not-exist", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			if tc.acceptGzip {
				req.Header.Set("Accept-Encoding", "gzip")
			}
			rr := httptest.NewRecorder()
			tc.router.ServeHTTP(rr, req)
			res := rr.Result()

			if res.StatusCode != tc.wantStatus {
				t.Errorf("%s %s: status = %d, want %d", tc.method, tc.target, res.StatusCode, tc.wantStatus)
			}
			if tc.acceptGzip && res.Header.Get("Content-Encoding") != "gzip" {
				t.Errorf("precondition: Content-Encoding = %q, want gzip, so the row runs through the compress writer", res.Header.Get("Content-Encoding"))
			}
			assertSecurityHeaders(t, res.Header)
		})
	}
}
