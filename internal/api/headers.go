package api

import "net/http"

const (
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	permissionsPolicy     = "accelerometer=(), browsing-topics=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()"

	assetCacheControl    = "public, max-age=31536000, immutable"
	rootFileCacheControl = "no-cache"
	noStoreCacheControl  = "no-store"
	staticFileMethods    = "GET, HEAD"
)

// setSecurityHeaders sends no Strict-Transport-Security, because the binary
// serves plain HTTP and the TLS proxy in front of it owns HSTS.
func setSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		header.Set("Permissions-Policy", permissionsPolicy)
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// successCacheControlWriter decides at the first final status write, because
// http.ServeFileFS selects the status itself and a 412 or a 301 must not be
// cached as the file.
type successCacheControlWriter struct {
	http.ResponseWriter
	cacheControl string
	wroteHeader  bool
}

func (w *successCacheControlWriter) WriteHeader(status int) {
	if !w.wroteHeader && !informationalStatus(status) {
		w.wroteHeader = true
		w.Header().Set("Cache-Control", w.cacheControlForStatus(status))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *successCacheControlWriter) cacheControlForStatus(status int) string {
	if cacheableStatus(status) {
		return w.cacheControl
	}
	return noStoreCacheControl
}

func (w *successCacheControlWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *successCacheControlWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func informationalStatus(status int) bool {
	return status >= 100 && status < 200 && status != http.StatusSwitchingProtocols
}

func cacheableStatus(status int) bool {
	switch status {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
		return true
	}
	return false
}
