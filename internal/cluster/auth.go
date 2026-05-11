package cluster

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerAuth returns middleware that requires Authorization: Bearer <token>
// on every request. Both sides are SHA-256 hashed before comparison so the
// compare is constant-time regardless of token length (subtle.ConstantTimeCompare
// returns 0 immediately on a length mismatch, leaking token length via timing).
func BearerAuth(token string) func(http.Handler) http.Handler {
	expectedHash := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(h, prefix) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			gotHash := sha256.Sum256([]byte(h[len(prefix):]))
			if subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
