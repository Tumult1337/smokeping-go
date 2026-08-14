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
//
// current supplies the accepted token per request rather than closing over one,
// so SIGHUP rotates the credential without a restart. It is read once per
// request, and config.Store publishes whole configs through a single atomic
// pointer, so a request sees one coherent token and never a half-applied
// reload. Hashing per request costs microseconds against a handful of slaves
// polling on a multi-second cadence, which buys away any cache-invalidation
// question.
func BearerAuth(current func() string) func(http.Handler) http.Handler {
	// A nil source would deny every request forever, which reads at runtime as
	// "the cluster is broken" rather than "this was wired wrong". Fail at
	// construction instead.
	if current == nil {
		panic("cluster: BearerAuth requires a token source")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			var presented string
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
				presented = h[len(prefix):]
			}
			// Hash unconditionally and branch once at the end. Short-circuiting
			// on a missing or malformed header would be harmless (that framing
			// is public), but keeping one exit keeps the credential compare off
			// every early-return path.
			token := current()
			gotHash := sha256.Sum256([]byte(presented))
			wantHash := sha256.Sum256([]byte(token))
			// An empty configured token must deny everything: sha256("") equals
			// sha256(""), so without this an unset or removed credential would
			// accept the header "Authorization: Bearer " and open ingest to
			// anyone. Revocation has to revoke.
			ok := token != "" && subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
