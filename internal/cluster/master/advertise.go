package master

import (
	"fmt"
	"net/netip"
)

// ParseAdvertise validates a slave-reported health address. The value is a
// single IP literal, never a hostname, prefix, or host:port — the master must
// not perform name resolution on slave-controlled input, and a prefix has no
// meaning for a probe destination.
//
// Private and unique-local ranges are deliberately accepted: mesh deployments
// (WireGuard and similar) address slaves entirely within RFC1918 / fc00::/7,
// so rejecting them would break the common case. Addresses that cannot name a
// reachable peer — unspecified, loopback, multicast, link-local — are rejected.
func ParseAdvertise(raw string) (netip.Addr, error) {
	if raw == "" {
		return netip.Addr{}, fmt.Errorf("advertise address is empty")
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("advertise %q: not an IP literal: %w", raw, err)
	}
	// Normalise ::ffff:a.b.c.d to a.b.c.d so two spellings of one address
	// collide in the duplicate check rather than registering as two peers.
	addr = addr.Unmap()
	switch {
	case addr.IsUnspecified():
		return netip.Addr{}, fmt.Errorf("advertise %q: unspecified address", raw)
	case addr.IsLoopback():
		return netip.Addr{}, fmt.Errorf("advertise %q: loopback is not reachable from peers", raw)
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsLinkLocalMulticast():
		return netip.Addr{}, fmt.Errorf("advertise %q: multicast is not a probe destination", raw)
	case addr.IsLinkLocalUnicast():
		return netip.Addr{}, fmt.Errorf("advertise %q: link-local is not routable between peers", raw)
	}
	return addr, nil
}
