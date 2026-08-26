// Package slave implements the slave-side runner: a standalone binary mode
// (enabled by --slave) that registers with a master, pulls its target list
// over HTTP, probes locally, and pushes cycle batches back. Slaves never
// touch storage or alerts — those stay master-side concerns.
package slave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
)

// ErrAuth is returned when the master rejects our bearer token with 401.
// The runner treats this as fatal — the operator must rotate + restart.
//
// 403 is intentionally NOT treated as ErrAuth. A 403 from a WAF / CDN
// (Cloudflare bot management, JS challenges, IUAM, geo-blocks, rate limits)
// is transient and recoverable — fast-failing the process on a CDN flap
// would tear down the whole slave fleet on every challenge. Real "token
// lacks scope" 403s from the master itself just become indefinite retries,
// which the operator notices via metrics rather than a crash loop.
var ErrAuth = errors.New("cluster: 401 unauthorized")

// ErrUnregistered signals a 403: the master will not accept cycles under a
// name its registry does not hold. Recoverable by re-registering, so callers
// keep the batch. A WAF 403 lands here too and costs one extra /register
// attempt, which is the same retry the previous generic-error path made.
var ErrUnregistered = errors.New("cluster: 403 slave not registered")

// ErrNotModified signals that a GET /config returned 304 — caller keeps its
// current config.
var ErrNotModified = errors.New("cluster: 304 not modified")

// ErrNotFound signals the master returned 404 — typically the slave was evicted
// or the master restarted without state. Push callers drop the batch; the next
// /register cadence re-establishes us.
var ErrNotFound = errors.New("cluster: 404 not found")

// ErrRejected signals a 4xx the master will answer identically however often
// the batch is resent — a batch outside the ingest bounds, or one whose oldest
// cycle aged past config.MaxCycleAge during an outage. Push callers drop it:
// requeueing head-of-line blocks the ring, so every later flush re-sends the
// same doomed batch while drop-oldest discards the live cycles behind it.
var ErrRejected = errors.New("cluster: batch permanently rejected")

// ErrMasterRefused marks the subset of ErrRejected that the master itself
// emits for a request it will answer identically forever: an invalid
// cluster.name, a header past maxSlaveFieldLen. Only this is fatal at boot and
// on re-registration. ErrRejected alone is every 4xx bar 401/403/404 and the
// retryable set, which any intermediary on the path can produce — a
// client_max_body_size, a header-buffer limit or a routing change answering
// 413/431/405 would otherwise crash-loop the whole fleet under systemd, the
// failure client.go already records for 403.
var ErrMasterRefused = errors.New("cluster: master refused this request")

// retryable4xx are the client-error statuses whose own specification says the
// same bytes may succeed later or on another connection, so they describe the
// moment rather than the batch. Anything else in 4xx is ErrRejected; 401, 403
// and 404 are classified before this is consulted. Entries are the RFC's
// reading, not a guess at the master's behaviour, because the answer can come
// from any intermediary on the path.
func retryable4xx(code int) bool {
	switch code {
	// RFC 9110 15.5.8: the proxy is demanding credentials, which says nothing
	// about the batch. The master never emits it, so only an intermediary can,
	// and dropping there loses data for a reason unrelated to the payload.
	case http.StatusProxyAuthRequired:
		return true
	// RFC 9110 15.5.9: "the client MAY repeat the request without
	// modifications at any later time".
	case http.StatusRequestTimeout:
		return true
	// RFC 9110 15.5.20: "the client MAY retry the request over a different
	// connection" — a reverse proxy's routing condition, not a verdict.
	case http.StatusMisdirectedRequest:
		return true
	// RFC 8470 5.2: the server refused early data; the same request succeeds
	// once it is not replayable.
	case http.StatusTooEarly:
		return true
	// RFC 6585 4: a transient rate limit.
	case http.StatusTooManyRequests:
		return true
	}
	return false
}

// Client is the HTTP wrapper the slave uses to talk to the master. Thread-safe;
// the runner holds one instance shared between the config-refresh loop and
// the push loop.
type Client struct {
	masterURL string
	token     string
	name      string
	version   string
	advertise string
	http      *http.Client
}

func NewClient(masterURL, token, name, version, advertise string) *Client {
	return &Client{
		masterURL: strings.TrimRight(masterURL, "/"),
		token:     token,
		name:      name,
		version:   version,
		advertise: advertise,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// Register posts a heartbeat to the master. Safe to call on boot and on any
// cadence the runner likes — the master only records last-seen + version.
func (c *Client) Register(ctx context.Context) error {
	body := cluster.RegisterReq{Name: c.name, Version: c.version, Advertise: c.advertise}
	_, _, err := c.do(ctx, http.MethodPost, "/api/v1/cluster/register", nil, body)
	return err
}

// PullConfig fetches the scrubbed cluster config for this slave. Pass the
// previous ETag (or "") — a 304 returns ErrNotModified so the caller can keep
// its cached config and skip the re-parse.
func (c *Client) PullConfig(ctx context.Context, etag string) (cluster.ClusterConfigResp, string, error) {
	headers := map[string]string{
		"X-Slave-Name":    c.name,
		"X-Slave-Version": c.version,
	}
	if etag != "" {
		headers["If-None-Match"] = etag
	}
	status, respBody, err := c.do(ctx, http.MethodGet, "/api/v1/cluster/config", headers, nil)
	if err != nil {
		return cluster.ClusterConfigResp{}, "", err
	}
	if status == http.StatusNotModified {
		return cluster.ClusterConfigResp{}, etag, ErrNotModified
	}
	var resp cluster.ClusterConfigResp
	if err := json.Unmarshal(respBody.body, &resp); err != nil {
		return cluster.ClusterConfigResp{}, "", fmt.Errorf("decode config: %w", err)
	}
	return resp, respBody.etag, nil
}

// PushCycles ships a batch of cycles to the master. On a 5xx, a network error
// or a transient 4xx the caller should retain the batch for retry; on 404 or
// ErrRejected the master will never accept these bytes and the caller should
// drop them.
func (c *Client) PushCycles(ctx context.Context, batch cluster.CycleBatch) error {
	headers := map[string]string{
		"X-Slave-Name":    c.name,
		"X-Slave-Version": c.version,
	}
	_, _, err := c.do(ctx, http.MethodPost, "/api/v1/cluster/cycles", headers, batch)
	return err
}

type httpResult struct {
	body []byte
	etag string
}

func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, body any) (int, httpResult, error) {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, httpResult{}, fmt.Errorf("encode body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.masterURL+path, reqBody)
	if err != nil {
		return 0, httpResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.advertise != "" {
		req.Header.Set(cluster.HeaderAdvertise, c.advertise)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, httpResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	const maxResponseBody = 64 << 20 // 64 MiB — generous cap against a rogue/MitM master
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return resp.StatusCode, httpResult{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, httpResult{}, ErrAuth
	}
	if resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, httpResult{}, ErrUnregistered
	}
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, httpResult{}, ErrNotFound
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotModified {
		if resp.StatusCode == http.StatusMisdirectedRequest {
			// RFC 9110 15.5.20's remedy is a *different* connection; retrying
			// through the same pool reproduces the misroute every flush.
			c.http.CloseIdleConnections()
		}
		err := fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(buf)))
		if resp.StatusCode < 500 && !retryable4xx(resp.StatusCode) {
			err = fmt.Errorf("%w: %w", ErrRejected, err)
			// 400 is the only status the master's own handlers emit for a
			// permanent refusal; every other 4xx here reached us from
			// somewhere else and must stay retryable at the process level.
			if resp.StatusCode == http.StatusBadRequest {
				err = fmt.Errorf("%w: %w", ErrMasterRefused, err)
			}
		}
		return resp.StatusCode, httpResult{body: buf}, err
	}
	return resp.StatusCode, httpResult{body: buf, etag: resp.Header.Get("ETag")}, nil
}
