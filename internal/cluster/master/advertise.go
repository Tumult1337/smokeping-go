package master

import (
	"fmt"
	"net/netip"

	"github.com/tumult/gosmokeping/internal/config"
)

// ParseAdvertise validates a slave-reported health address. The value is a
// single IP literal, never a hostname, prefix, or host:port — the master must
// not perform name resolution on slave-controlled input, and a prefix has no
// meaning for a probe destination.
//
// The reachability predicate itself lives in config.ParseReachableAddr —
// config.ParsedSlaveAddrs (an operator-written pin) validates the same class
// of value, and duplicating the checks would let the two drift. This wrapper
// only supplies the "advertise" call-site context on error.
func ParseAdvertise(raw string) (netip.Addr, error) {
	addr, err := config.ParseReachableAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("advertise %w", err)
	}
	return addr, nil
}
