package probe

import (
	"context"
	"fmt"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// Result is a per-cycle outcome: every RTT collected plus counters. For MTR
// cycles, Hops contains per-hop stats; the top-level RTTs/Sent/LossCount
// mirror the final hop so the standard cycle pipeline still sees target stats.
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
	RTTs  []time.Duration
	Sent  int
	Lost  int
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

// Build constructs a Registry from config, returning the set of probes
// referenced by the config's targets.
func Build(probes map[string]config.Probe) (*Registry, error) {
	r := NewRegistry()
	for name, pc := range probes {
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

func build(name string, pc config.Probe) (Probe, error) {
	switch pc.Type {
	case "icmp":
		return NewICMP(name, pc.Timeout), nil
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
