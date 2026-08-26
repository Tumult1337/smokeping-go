package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// Result is a per-cycle outcome: every RTT collected plus counters. For MTR
// cycles, Hops contains per-hop stats; the top-level RTTs come from the rows
// the target itself answered and Sent/LossCount count trace rounds, so the
// standard cycle pipeline still sees target stats.
type Result struct {
	RTTs      []time.Duration
	Sent      int
	LossCount int
	Hops      []Hop
	// HTTPSamples is populated by the HTTP probe only: one entry per request,
	// carrying status code + error alongside RTT so the UI can render a status
	// timeline. Empty for every other probe type.
	HTTPSamples []HTTPSample
}

// HTTPSample is a single HTTP request outcome. Status == 0 means the request
// never got a response (DNS, refused, TLS, timeout) and Err holds the reason.
type HTTPSample struct {
	Time   time.Time
	RTT    time.Duration
	Status int
	Err    string
}

// Hop is one entry of an MTR trace: which router responded at a given TTL and
// its latency/loss stats across the round of probes.
type Hop struct {
	Index int
	IP    string
	// TargetReply marks a row whose responder answered as the target itself
	// (an echo reply): under a per-round walk the target's row is not
	// guaranteed to be the deepest, so redaction and the MTR RTT mirror key
	// on this rather than on position.
	TargetReply bool
	// Unreach carries the label of the ICMP unreachable that ended the walk
	// at this hop, from the closed set in unreachLabels; empty for ordinary
	// hops.
	Unreach string
	RTTs    []time.Duration
	Sent    int
	Lost    int
}

// Target is the normalized view of a target passed to a Probe.
type Target struct {
	Name    string
	Group   string
	Host    string
	URL     string
	Timeout time.Duration
	// Family pins address resolution to IPv4 ("v4") or IPv6 ("v6") when set;
	// empty uses the system default. Each probe implementation is responsible
	// for honoring it (ResolveIPAddr network, dialer network, resolver dial).
	Family string
}

// Probe transports round-trip measurements for a given protocol.
type Probe interface {
	Name() string
	Probe(ctx context.Context, t Target, count int) (*Result, error)
}

// Registry maps probe names (from config) to Probe implementations.
type Registry struct {
	probes map[string]Probe
}

func NewRegistry() *Registry {
	return &Registry{probes: map[string]Probe{}}
}

func (r *Registry) Register(p Probe) {
	r.probes[p.Name()] = p
}

func (r *Registry) Get(name string) (Probe, bool) {
	p, ok := r.probes[name]
	return p, ok
}

// Build constructs a Registry holding one probe per entry in probes, whether
// or not a target references it. interval and pings are needed to refuse a
// schedule no per-ping deadline can fit, which is a property of the schedule
// rather than of any one probe.
func Build(probes map[string]config.Probe, interval time.Duration, pings int) (*Registry, error) {
	// Defence in depth: config.Validate refuses this schedule too, so reaching
	// it here means a config that never passed validation — a slave's
	// master-supplied view, or a caller that built a Config in memory.
	if err := config.ValidatePingCount(pings); err != nil {
		return nil, err
	}
	var budget time.Duration
	if config.HasICMPProbe(probes) {
		var err error
		if budget, err = config.ICMPPingBudget(interval, pings); err != nil {
			return nil, fmt.Errorf("icmp schedule: %w", err)
		}
	}
	r := NewRegistry()
	for name, pc := range probes {
		if pc.Type == "icmp" && budget < pc.Timeout {
			slog.Info("icmp per-ping timeout shortened to fit the cycle",
				"probe", name, "configured", pc.Timeout, "full_loss_budget", budget,
				"interval", interval, "pings", pings)
		}
		p, err := build(name, pc)
		if err != nil {
			return nil, fmt.Errorf("probe %q: %w", name, err)
		}
		r.Register(p)
	}
	return r, nil
}

// familyNetwork maps a target Family ("", "v4", "v6") onto a Go net package
// network string. base is the protocol prefix ("ip", "tcp", "udp"). Empty
// family returns base unchanged, which lets the OS pick the family — the
// previous behavior for every probe before Target.Family existed.
func familyNetwork(base, family string) string {
	switch family {
	case "v4":
		return base + "4"
	case "v6":
		return base + "6"
	default:
		return base
	}
}

// interfaceNameByIndex is the injectable seam over the interface table, so a
// test can drive normalizeZone without depending on the host's own links.
var interfaceNameByIndex = func(index int) (string, error) {
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return "", err
	}
	return iface.Name, nil
}

// normalizeZone rewrites a numeric zone to the interface name, because that is
// what the read path will carry: net fills a received address's Zone from
// zoneCache.name(sin6_scope_id), which resolves the index to a name whenever
// it can. Zone is part of netip.Addr equality, so a target configured as
// fe80::1%2 produced a want of zone "2" against replies arriving with zone
// "eth0" — matchEchoReply discarded every one of them and the target read 100%
// loss with nothing logged. The fallback is zoneCache's own: an index that
// resolves to no interface keeps its decimal form.
func normalizeZone(zone string) string {
	if zone == "" {
		return zone
	}
	index, err := strconv.Atoi(zone)
	if err != nil || index <= 0 {
		return zone
	}
	name, err := interfaceNameByIndex(index)
	if err != nil {
		return zone
	}
	return name
}

// lookupIPFn is the injectable seam over the resolver so tests can drive a
// blackholed DNS lookup without one. It takes the network because the
// resolver queries only the pinned family's record: filtering a dual lookup
// instead would make a broken AAAA path fail every v4-pinned cycle.
var lookupIPFn = func(ctx context.Context, network, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, network, host)
}

// resolveIPAddr is net.ResolveIPAddr with the context honored: probes run
// under the cycle deadline and the trace goroutine is joined on every return
// path, so a resolver call that ignores a cancelled cycle blocks shutdown and
// every SIGHUP rebuild behind it for the resolver's own timeout.
func resolveIPAddr(ctx context.Context, network, host string) (*net.IPAddr, error) {
	noSuitable := &net.AddrError{Err: "no suitable address found", Addr: host}
	// A literal never reaches the resolver, which is what carries a
	// link-local zone through to the socket.
	if a, err := netip.ParseAddr(host); err == nil {
		ip := net.IP(a.AsSlice())
		if !matchesFamily(network, ip) {
			return nil, noSuitable
		}
		return &net.IPAddr{IP: ip, Zone: normalizeZone(a.Zone())}, nil
	}
	ips, err := lookupIPFn(ctx, network, host)
	if err != nil {
		return nil, err
	}
	if network == "ip" {
		// net.ResolveIPAddr's own preference for a bare "ip" network: IPv4
		// where the name has one, any family otherwise.
		for i := range ips {
			if ips[i].To4() != nil {
				return &net.IPAddr{IP: ips[i]}, nil
			}
		}
	}
	if len(ips) == 0 {
		return nil, noSuitable
	}
	return &net.IPAddr{IP: ips[0]}, nil
}

func matchesFamily(network string, ip net.IP) bool {
	switch network {
	case "ip4":
		return ip.To4() != nil
	case "ip6":
		return len(ip) == net.IPv6len && ip.To4() == nil
	default:
		return true
	}
}

func build(name string, pc config.Probe) (Probe, error) {
	switch pc.Type {
	case "icmp":
		return NewICMP(name, pc.Timeout, pc.NoTrace), nil
	case "tcp":
		return NewTCP(name, pc.Timeout), nil
	case "http":
		return NewHTTP(name, pc.Timeout, pc.Insecure), nil
	case "dns":
		return NewDNS(name, pc.Timeout), nil
	case "mtr":
		return NewMTR(name, pc.Timeout), nil
	default:
		return nil, fmt.Errorf("unknown type %q", pc.Type)
	}
}
