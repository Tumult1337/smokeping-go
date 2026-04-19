package cluster

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerAuth returns middleware that requires Authorization: Bearer <token>
// on every request. 401 with an empty body on mismatch — no token echo, no
// timing-sensitive comparison.
func BearerAuth(token string) func(http.Handler) http.Handler {
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(h, prefix) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			got := []byte(h[len(prefix):])
			if len(got) == 0 || subtle.ConstantTimeCompare(got, expected) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
