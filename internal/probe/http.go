package probe

import (
	"context"
	"errors"
	"io"
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

func NewHTTP(name string, timeout time.Duration) *HTTP {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTP{
		name:    name,
		timeout: timeout,
		spacing: 500 * time.Millisecond,
		client: &http.Client{
			Timeout: timeout,
			// Don't follow redirects — we want to measure the target URL itself.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *HTTP) Name() string { return p.name }

func (p *HTTP) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.URL == "" {
		return nil, errors.New("http: url required")
	}
	result := &Result{RTTs: make([]time.Duration, 0, count)}

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		if rtt, ok := p.one(ctx, t.URL); ok {
			result.RTTs = append(result.RTTs, rtt)
		} else {
			result.LossCount++
		}
		if n < count-1 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(p.spacing):
			}
		}
	}
	return result, nil
}

func (p *HTTP) one(ctx context.Context, url string) (time.Duration, bool) {
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	reqCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("User-Agent", "gosmokeping/1.0")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the transport can pool the connection.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return 0, false
	}
	if firstByte.IsZero() {
		return time.Since(start), true
	}
	return firstByte.Sub(start), true
}
