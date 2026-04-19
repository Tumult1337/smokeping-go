package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ETag returns a stable hash of any JSON-marshalable value. Used by
// GET /cluster/config so a slave can skip re-parsing when the master config
// hasn't changed since its last pull.
func ETag(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Unmarshalable value shouldn't happen with our DTOs, but return the
		// empty etag rather than panicking — caller will just always serve 200.
		return ""
	}
	sum := sha256.Sum256(b)
	// 16 hex chars = 8 bytes of the digest. Collision risk at this scale is
	// negligible and shorter etags read better in logs.
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}
