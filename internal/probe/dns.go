package probe

import (
	"context"
	"errors"
	"net"
	"time"
)

// DNS measures the time to resolve Target.Host. If Target.URL is non-empty and
// parses as "host:port", the server at host:port is used as the resolver via
// net.Resolver with a custom Dial; otherwise the system resolver is used.
type DNS struct {
	name    string
	timeout time.Duration
	spacing time.Duration
}

func NewDNS(name string, timeout time.Duration) *DNS {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &DNS{name: name, timeout: timeout, spacing: 200 * time.Millisecond}
}

func (p *DNS) Name() string { return p.name }

func (p *DNS) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.Host == "" {
		return nil, errors.New("dns: host required")
	}

	resolver := net.DefaultResolver
	if t.URL != "" {
		server := t.URL
		if _, _, err := net.SplitHostPort(server); err != nil {
			server = net.JoinHostPort(server, "53")
		}
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: p.timeout}
				return d.DialContext(ctx, network, server)
			},
		}
	}

	result := &Result{RTTs: make([]time.Duration, 0, count)}
	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		lookupCtx, cancel := context.WithTimeout(ctx, p.timeout)
		start := time.Now()
		_, err := resolver.LookupHost(lookupCtx, t.Host)
		cancel()
		if err != nil {
			result.LossCount++
		} else {
			result.RTTs = append(result.RTTs, time.Since(start))
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
