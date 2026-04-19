package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// TCP measures connect-time to host:port. Target.Host must include a port
// (e.g. "example.com:443") — if omitted we default to 80.
type TCP struct {
	name    string
	timeout time.Duration
	spacing time.Duration
}

func NewTCP(name string, timeout time.Duration) *TCP {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &TCP{name: name, timeout: timeout, spacing: 200 * time.Millisecond}
}

func (p *TCP) Name() string { return p.name }

func (p *TCP) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.Host == "" {
		return nil, errors.New("tcp: host required")
	}
	addr := t.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "80")
	}

	result := &Result{RTTs: make([]time.Duration, 0, count)}
	dialer := &net.Dialer{Timeout: p.timeout}
	var lastErr error

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		start := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			result.LossCount++
			lastErr = err
			slog.Debug("tcp probe failed", "probe", p.name, "target", t.Name, "addr", addr, "err", err)
		} else {
			result.RTTs = append(result.RTTs, time.Since(start))
			_ = conn.Close()
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
		return result, fmt.Errorf("tcp: all %d dials to %s failed: %w", result.Sent, addr, lastErr)
	}
	return result, nil
}
