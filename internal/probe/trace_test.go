package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

// swapListenRaw routes traceHops' socket open through a stub for the duration
// of one test. Not parallel-safe by design; none of these tests call
// t.Parallel.
func swapListenRaw(t *testing.T, fn func(bool) (*icmp.PacketConn, error)) {
	t.Helper()
	orig := listenRawFn
	listenRawFn = fn
	t.Cleanup(func() { listenRawFn = orig })
}

// Only permission errors may claim errRawUnavailable: that class is logged
// once per process and then silenced, so classifying a transient EMFILE into
// it makes every later raw-socket failure invisible.
func TestTraceHopsClassifiesListenErrors(t *testing.T) {
	tests := []struct {
		name        string
		listenErr   error
		wantRawUnav bool
	}{
		{"EPERM is permission", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EPERM)}, true},
		{"EACCES is permission", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EACCES)}, true},
		{"wrapped fs.ErrPermission is permission", fmt.Errorf("denied: %w", fs.ErrPermission), true},
		{"EMFILE is transient", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EMFILE)}, false},
		{"ENOBUFS is transient", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.ENOBUFS)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swapListenRaw(t, func(bool) (*icmp.PacketConn, error) { return nil, tc.listenErr })
			_, _, err := traceHops(context.Background(), "127.0.0.1", "", 1, 1, time.Millisecond, 0)
			if err == nil {
				t.Fatal("expected an error from a failed listen")
			}
			if got := errors.Is(err, errRawUnavailable); got != tc.wantRawUnav {
				t.Fatalf("errors.Is(err, errRawUnavailable) = %v, want %v (err: %v)", got, tc.wantRawUnav, err)
			}
			if !errors.Is(err, tc.listenErr) {
				t.Fatalf("underlying error lost from chain: %v", err)
			}
		})
	}
}

// The throttle is time-based and counts what it suppresses: a throttle
// without a count is a blind window — up to ~366 failures/minute fleet-wide
// would collapse into one line, quietly recreating a milder version of the
// invisibility this task exists to fix.
func TestTraceErrLogThrottle(t *testing.T) {
	resetTraceErrThrottle()
	// Elapsed values are strictly positive: time.Since never returns 0, and
	// the throttle uses 0 as its never-logged sentinel.
	if ok, n := traceErrLogAllowed(time.Second); !ok || n != 0 {
		t.Fatalf("first warn: ok=%v n=%d, want allowed with 0 suppressed", ok, n)
	}
	if ok, _ := traceErrLogAllowed(time.Second + traceErrLogEvery/3); ok {
		t.Fatal("second warn inside the window must be suppressed")
	}
	if ok, _ := traceErrLogAllowed(time.Second + traceErrLogEvery/2); ok {
		t.Fatal("third warn inside the window must be suppressed")
	}
	if ok, n := traceErrLogAllowed(2*time.Second + traceErrLogEvery); !ok || n != 2 {
		t.Fatalf("post-window warn: ok=%v n=%d, want allowed with 2 suppressed", ok, n)
	}
	if ok, n := traceErrLogAllowed(3*time.Second + 2*traceErrLogEvery); !ok || n != 0 {
		t.Fatalf("suppressed count must reset once reported: ok=%v n=%d", ok, n)
	}
}
