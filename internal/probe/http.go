package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// HTTP issues a GET to Target.URL and measures time-to-first-byte. Non-2xx
// responses count as loss. Body is drained (up to 4KB) to keep the connection
// pool healthy but we don't measure download time.
type HTTP struct {
	name     string
	timeout  time.Duration
	spacing  time.Duration
	insecure bool
	client   *http.Client

	// famClients caches family-pinned http.Client instances ("v4" / "v6")
	// so each cycle reuses an existing connection pool instead of building
	// a fresh http.Transport. Without this cache, every probe call for a
	// family-pinned target allocates a new transport whose idle TCP
	// connections survive until GC + finalizer reclaim them — a slow leak
	// over weeks of uptime. Bounded to two entries in practice.
	famMu      sync.RWMutex
	famClients map[string]*http.Client
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
		name:     name,
		timeout:  timeout,
		spacing:  500 * time.Millisecond,
		insecure: insecure,
		client: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// Don't follow redirects — we want to measure the target URL itself.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		famClients: make(map[string]*http.Client),
	}
}

// clientFor returns a client pinned to the given address family, or the shared
// client when family is empty. The family-pinned client clones the default
// transport and overrides its DialContext with a net.Dialer tied to tcp4/tcp6,
// so connection reuse stays per-family rather than global; with maxHTTPRequests
// == 2 that's fine.
//
// Results are cached per family for the life of the probe — building a fresh
// http.Transport on every cycle would leak its connection pool until GC.
func (p *HTTP) clientFor(family string) *http.Client {
	if family == "" {
		return p.client
	}
	p.famMu.RLock()
	c, ok := p.famClients[family]
	p.famMu.RUnlock()
	if ok {
		return c
	}

	p.famMu.Lock()
	defer p.famMu.Unlock()
	// Re-check under the write lock so two readers racing past the fast
	// path don't both construct a client and then disagree on which one
	// the cache holds.
	if c, ok := p.famClients[family]; ok {
		return c
	}
	c = p.buildFamilyClient(family)
	p.famClients[family] = c
	return c
}

func (p *HTTP) buildFamilyClient(family string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if p.insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	network := familyNetwork("tcp", family)
	dialer := &net.Dialer{Timeout: p.timeout, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Timeout:   p.timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
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
	result := &Result{
		RTTs:        make([]time.Duration, 0, count),
		HTTPSamples: make([]HTTPSample, 0, count),
	}
	client := p.clientFor(t.Family)
	var lastErr error

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		sampleTime := time.Now()
		rtt, status, err := p.one(ctx, client, t.URL)
		sample := HTTPSample{Time: sampleTime, RTT: rtt, Status: status}
		if err != nil {
			result.LossCount++
			lastErr = err
			sample.Err = err.Error()
			slog.Debug("http probe failed", "probe", p.name, "target", t.Name, "url", t.URL, "status", status, "err", err)
		} else {
			result.RTTs = append(result.RTTs, rtt)
		}
		result.HTTPSamples = append(result.HTTPSamples, sample)
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

// one issues a single request. Returns RTT, status code (0 if no response was
// received), and any error. A non-2xx/3xx response returns a non-nil error but
// the status code is still reported so the UI can distinguish 404 from TCP
// refused.
func (p *HTTP) one(ctx context.Context, client *http.Client, url string) (time.Duration, int, error) {
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	reqCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", "gosmokeping/1.0")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the transport can pool the connection.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)

	rtt := time.Since(start)
	if !firstByte.IsZero() {
		rtt = firstByte.Sub(start)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return rtt, resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	return rtt, resp.StatusCode, nil
}
