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

// ErrAuth is returned when the master rejects our bearer token. The runner
// treats this as fatal — the operator must rotate + restart.
var ErrAuth = errors.New("cluster: 401 unauthorized")

// ErrNotModified signals that a GET /config returned 304 — caller keeps its
// current config.
var ErrNotModified = errors.New("cluster: 304 not modified")

// ErrNotFound signals the master returned 404 — typically the slave was evicted
// or the master restarted without state. Push callers drop the batch; the next
// /register cadence re-establishes us.
var ErrNotFound = errors.New("cluster: 404 not found")

// Client is the HTTP wrapper the slave uses to talk to the master. Thread-safe;
// the runner holds one instance shared between the config-refresh loop and
// the push loop.
type Client struct {
	masterURL string
	token     string
	name      string
	version   string
	http      *http.Client
}

func NewClient(masterURL, token, name, version string) *Client {
	return &Client{
		masterURL: strings.TrimRight(masterURL, "/"),
		token:     token,
		name:      name,
		version:   version,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// Register posts a heartbeat to the master. Safe to call on boot and on any
// cadence the runner likes — the master only records last-seen + version.
func (c *Client) Register(ctx context.Context) error {
	body := cluster.RegisterReq{Name: c.name, Version: c.version}
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

// PushCycles ships a batch of cycles to the master. Returns the master's
// accepted count or an error. On 5xx/network error the caller should retain
// the batch for retry; on 404 the master has lost us and the caller should
// drop the batch.
func (c *Client) PushCycles(ctx context.Context, batch cluster.CycleBatch) error {
	_, _, err := c.do(ctx, http.MethodPost, "/api/v1/cluster/cycles", nil, batch)
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
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, httpResult{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, httpResult{}, ErrAuth
	}
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, httpResult{}, ErrNotFound
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotModified {
		return resp.StatusCode, httpResult{body: buf}, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return resp.StatusCode, httpResult{body: buf, etag: resp.Header.Get("ETag")}, nil
}
