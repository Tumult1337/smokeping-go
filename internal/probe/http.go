package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"time"
)

// HTTP issues a GET to Target.URL and measures time-to-first-byte. Non-2xx
// responses count as loss. Body is drained (up to 4KB) to keep the connection
// pool healthy but we don't measure download time.
type HTTP struct {
	name    string
	timeout time.Duration
	spacing time.Duration
	client  *http.Client
}

// NewHTTP builds an HTTP probe. If insecure is true, TLS verification is
// skipped — intended for targets with self-signed or expired certs where
// reachability is the point, not cert hygiene.
func NewHTTP(name string, timeout time.Duration, insecure bool) *HTTP {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// Clone DefaultTransport so we keep its sane connection-pool defaults and
	// only override TLS config when asked.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &HTTP{
		name:    name,
		timeout: timeout,
		spacing: 500 * time.Millisecond,
		client: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// Don't follow redirects — we want to measure the target URL itself.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *HTTP) Name() string { return p.name }

// maxHTTPRequests caps requests per cycle. HTTP is far more expensive than a
// ping (TLS handshake, server log entries, possible rate limits / WAF flags),
// so we deliberately do at most a couple per interval regardless of cfg.Pings.
const maxHTTPRequests = 2

func (p *HTTP) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.URL == "" {
		return nil, errors.New("http: url required")
	}
	if count > maxHTTPRequests {
		count = maxHTTPRequests
	}
	if count < 1 {
		count = 1
	}
	result := &Result{RTTs: make([]time.Duration, 0, count)}
	var lastErr error

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		rtt, err := p.one(ctx, t.URL)
		if err != nil {
			result.LossCount++
			lastErr = err
			slog.Debug("http probe failed", "probe", p.name, "target", t.Name, "url", t.URL, "err", err)
		} else {
			result.RTTs = append(result.RTTs, rtt)
		}
		if n < count-1 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(p.spacing):
			}
		}
	}
	if result.LossCount == result.Sent && lastErr != nil {
		return result, fmt.Errorf("http: all %d requests failed: %w", result.Sent, lastErr)
	}
	return result, nil
}

func (p *HTTP) one(ctx context.Context, url string) (time.Duration, error) {
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	reqCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", "gosmokeping/1.0")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the transport can pool the connection.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	if firstByte.IsZero() {
		return time.Since(start), nil
	}
	return firstByte.Sub(start), nil
}
