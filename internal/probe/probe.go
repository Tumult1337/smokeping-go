package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
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

// lookupIPAddrFn is the injectable seam over the resolver so tests can drive
// a blackholed DNS lookup without one.
var lookupIPAddrFn = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// resolveIPAddr is net.ResolveIPAddr with the context honored: probes run
// under the cycle deadline and the trace goroutine is joined on every return
// path, so a resolver call that ignores a cancelled cycle blocks shutdown and
// every SIGHUP rebuild behind it for the resolver's own timeout.
func resolveIPAddr(ctx context.Context, network, host string) (*net.IPAddr, error) {
	addrs, err := lookupIPAddrFn(ctx, host)
	if err != nil {
		return nil, err
	}
	var match func(net.IPAddr) bool
	switch network {
	case "ip4":
		match = func(a net.IPAddr) bool { return a.IP.To4() != nil }
	case "ip6":
		match = func(a net.IPAddr) bool { return len(a.IP) == net.IPv6len && a.IP.To4() == nil }
	default:
		// net.ResolveIPAddr's own preference for a bare "ip" network: the
		// family a literal spells, IPv4 otherwise, any family as a fallback.
		prefer6 := strings.ContainsRune(host, ':')
		match = func(a net.IPAddr) bool { return (a.IP.To4() == nil) == prefer6 }
	}
	for i := range addrs {
		if match(addrs[i]) {
			return &addrs[i], nil
		}
	}
	if network == "ip" && len(addrs) > 0 {
		return &addrs[0], nil
	}
	return nil, &net.AddrError{Err: "no suitable address found", Addr: host}
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
