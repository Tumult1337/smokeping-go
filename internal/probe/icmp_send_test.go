package probe

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// Socket deadlines may fire before the context's timer publishes Err. Keeping
// Err nil here makes that ordering deterministic without racing two timers.
type unpublishedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c unpublishedDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestSendOneCanceledBeforeSocketAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rtt, err := sendOne(ctx, nil, nil, false, 1, 1, time.Second)
	if rtt != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("sendOne = %s, %v; want no RTT and cancellation before socket access", rtt, err)
	}
}

func TestSendOneClassifiesRealSocketDeadlines(t *testing.T) {
	requireICMPSocket(t)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		timeout time.Duration
		want    error
	}{
		{"cycle deadline before publication", unpublishedDeadlineContext{context.Background(), time.Now().Add(-time.Second)}, time.Hour, context.DeadlineExceeded},
		{"configured timeout is completed loss", context.Background(), -time.Second, os.ErrDeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := listen(false)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// Both read deadlines are already expired. A real loopback write
			// exercises sendOne's read-error branch without a remote host or sleep.
			rtt, err := sendOne(tc.ctx, conn, &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}, false, 1234, 7, tc.timeout)
			if rtt != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("sendOne = %s, %v; want zero RTT and %v", rtt, err, tc.want)
			}
			if tc.ctx.Err() != nil {
				t.Fatalf("test context published an error: %v", tc.ctx.Err())
			}
		})
	}
}
