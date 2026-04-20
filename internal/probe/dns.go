package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// Family pins the record type we ask for: "ip4" → A-only, "ip6" → AAAA-only,
	// "ip" → both. The probe measures lookup latency, so restricting the record
	// type is the semantically correct interpretation of Target.Family here
	// (not the network used to reach the upstream resolver, which is a separate
	// concern we leave to the OS).
	ipNetwork := familyNetwork("ip", t.Family)

	result := &Result{RTTs: make([]time.Duration, 0, count)}
	var lastErr error
	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		lookupCtx, cancel := context.WithTimeout(ctx, p.timeout)
		start := time.Now()
		_, err := resolver.LookupIP(lookupCtx, ipNetwork, t.Host)
		cancel()
		if err != nil {
			result.LossCount++
			lastErr = err
			slog.Debug("dns probe failed", "probe", p.name, "target", t.Name, "host", t.Host, "err", err)
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
	if result.LossCount == result.Sent && lastErr != nil {
		return result, fmt.Errorf("dns: all %d lookups for %s failed: %w", result.Sent, t.Host, lastErr)
	}
	return result, nil
}
