package influxv3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// bootstrapHTTPTimeout caps the database-create call. The endpoint should
// respond near-instantly on a healthy server; a stuck install shouldn't be
// able to block startup forever.
const bootstrapHTTPTimeout = 30 * time.Second

// Bootstrap ensures the configured database exists on the v3 server. Idempotent:
// if the database is already present the server returns 409 / "already exists"
// and we treat that as success. Bootstrap intentionally does NOT install any
// rollup or downsampling plugins — v3's Processing Engine plugins are Python,
// operator-managed, and out of scope for this binary; the reader handles
// resolution tiering at query time via date_bin() instead.
func Bootstrap(ctx context.Context, log *slog.Logger, cfg config.InfluxV3) error {
	if cfg.Database == "" {
		return fmt.Errorf("influxv3: database is required")
	}

	body := map[string]any{"db": cfg.Database}
	if cfg.RetentionPeriod != "" {
		body["retention_period"] = cfg.RetentionPeriod
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("influxv3 bootstrap: marshal: %w", err)
	}

	endpoint, err := url.JoinPath(strings.TrimRight(cfg.URL, "/"), "/api/v3/configure/database")
	if err != nil {
		return fmt.Errorf("influxv3 bootstrap: build url: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, bootstrapHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("influxv3 bootstrap: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("influxv3 bootstrap: do: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		log.Info("influxv3 database created", "database", cfg.Database)
		return nil
	case resp.StatusCode == http.StatusConflict, isAlreadyExists(respBody):
		// Already-exists responses come back as 409 on current Core/Enterprise
		// builds; older builds returned 400 with a textual marker, so the body
		// fallback covers both. Either way: idempotent success — the database
		// is there, we keep going.
		log.Info("influxv3 database exists", "database", cfg.Database)
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("influxv3 bootstrap: %s — token must have admin scope to create databases (status %d)",
			strings.TrimSpace(string(respBody)), resp.StatusCode)
	default:
		return fmt.Errorf("influxv3 bootstrap: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// isAlreadyExists checks the response body for the textual marker some older
// v3 builds use instead of HTTP 409 when a database collision happens. Kept
// case-insensitive because the exact phrasing has shifted between releases.
func isAlreadyExists(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "already exists") || strings.Contains(lower, "duplicate")
}
