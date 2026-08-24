// Package slavehealth synthesizes the per-slave health targets every node in a
// cluster probes.
//
// The package exists to enforce one invariant: a slave's address reaches the
// scheduler and peer slaves, and nothing else. Two accessors over one snapshot
// make that structural rather than a convention —
//
//	Probe()  real addresses; the scheduler and BuildClusterConfig only
//	Public() addresses stripped; the API only
//
// so a handler that renders whatever it is given cannot leak an address, and a
// reviewer checks the wiring rather than auditing every field access.
package slavehealth

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

const (
	// Group is the reserved group health targets live in. The leading
	// underscore keeps it outside the user namespace; config validation
	// rejects a user-defined group of the same name so a real target cannot
	// shadow a health target and inherit its address-stripping behaviour.
	Group = "_cluster"

	// GroupTitle is the sidebar label for the group.
	GroupTitle = "Slaves"

	// ProbeName is the synthesized probe definition health targets use. It
	// is injected into the probe registry at build time rather than required
	// in the operator's config, so the mesh needs no probe setup.
	ProbeName = "_slave_health"

	// defaultTimeout applies when the caller has no interval-derived value.
	defaultTimeout = 2 * time.Second
)

// Peer is one slave eligible for health probing.
type Peer struct {
	Name string
	Addr netip.Addr
}

// Set is an immutable snapshot of the health peers. Build one per scheduler
// rebuild; never mutate one in place.
type Set struct {
	peers []Peer
	// alerts is cluster.health_alerts: the alert names stamped onto every
	// synthesized target. A required NewSet argument rather than an optional
	// builder step, so a caller cannot silently produce alert-less health
	// targets — the exact defect that made quorum unreachable.
	alerts []string
}

// NewSet copies peers so the caller's slice cannot mutate the snapshot, and
// sorts the copy by Name (tie-break Addr) so every derived value —
// Fingerprint(), Probe(), and Public() — is order-invariant regardless of
// what order the caller passes peers in. A correctness property this
// fundamental must not depend on an unenforced contract in a different
// package.
func NewSet(peers []Peer, alerts []string) *Set {
	out := make([]Peer, len(peers))
	copy(out, peers)
	slices.SortFunc(out, func(a, b Peer) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.Addr.String(), b.Addr.String())
	})
	return &Set{peers: out, alerts: slices.Clone(alerts)}
}

// IsHealthGroup reports whether a group name is the reserved health group.
func IsHealthGroup(group string) bool { return group == Group }

// ProbeDef returns the synthesized probe definition. ICMP is deliberate: the
// icmp probe already performs an opportunistic TTL walk concurrently with its
// echo batch, so one probe yields both echo statistics and traceroute hops
// with no second target. hops=false disables that walk. A non-positive
// timeout falls back to the package default.
func ProbeDef(timeout time.Duration, hops bool) config.Probe {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return config.Probe{Type: "icmp", Timeout: timeout, NoTrace: !hops}
}

// Probe returns the health group with real addresses, omitting the named peer.
// Callers pass their own node name so a node never probes itself; the master
// passes "" because it is not a peer.
//
// Returns nil rather than an empty group when nothing remains, so an empty
// group never reaches the sidebar as a stray heading.
func (s *Set) Probe(exclude string) []config.Group {
	if s == nil {
		return nil
	}
	targets := make([]config.Target, 0, len(s.peers))
	for _, p := range s.peers {
		if p.Name == exclude || !p.Addr.IsValid() {
			continue
		}
		targets = append(targets, config.Target{
			Name:  p.Name,
			Title: p.Name,
			Host:  p.Addr.String(),
			Probe: ProbeName,
			// Cloned per target so a consumer that rewrites a target's alert
			// list cannot reach back into the snapshot shared by its peers.
			Alerts: slices.Clone(s.alerts),
			// Family is pinned to the address we already hold, so the probe
			// never re-resolves and never picks the other family.
			Family: familyOf(p.Addr),
		})
	}
	if len(targets) == 0 {
		return nil
	}
	return []config.Group{{Group: Group, Title: GroupTitle, Targets: targets}}
}

// Public returns the health targets with every address-bearing field cleared.
// This is the only view the API is wired to.
func (s *Set) Public() []config.TargetRef {
	if s == nil {
		return nil
	}
	out := make([]config.TargetRef, 0, len(s.peers))
	for _, p := range s.peers {
		// Public() must describe exactly the set Probe() probes — otherwise
		// the API advertises a target nothing ever collects data for, and it
		// shows up in the UI as a permanently-empty row.
		if !p.Addr.IsValid() {
			continue
		}
		out = append(out, config.TargetRef{
			Group: Group,
			Target: config.Target{
				Name:  p.Name,
				Title: p.Name,
				Probe: ProbeName,
				// Alerts are exposed deliberately: they are operator-chosen
				// names with no address content, the API's targetDTO already
				// surfaces them for ordinary targets, and hiding them here
				// would make a health target look unmonitored in the UI while
				// it is in fact alerting.
				Alerts: slices.Clone(s.alerts),
				// Host, URL and Family are left zero. Family would disclose
				// whether a slave is reachable over v4 or v6, which is a weak
				// but unnecessary signal.
			},
		})
	}
	return out
}

// Fingerprint is a stable key over membership *and* the alert list, appended
// to the scheduler's config fingerprint so a mesh change triggers a rebuild.
// Health targets are injected at build time and are absent from the stored
// config, so the config fingerprint alone cannot see them.
//
// The alert list is included because the evaluator reads the names baked into
// the scheduler's targets at Build time, while the API reports Public()'s
// names per request. Without this, editing cluster.health_alerts and sending
// SIGHUP would make the UI claim an alert is attached that can never fire
// until the process restarts.
//
// Field and record separators are escaped: the fingerprint drives a rebuild
// decision, and a peer or alert name containing a raw separator must not be
// able to forge the fingerprint of a different membership set. The two
// sections are additionally split by a group separator so no alert name can
// be read back as a peer record.
//
// Fingerprint tolerates a nil Set so callers wired for standalone mode need
// no nil check on the hot reload path.
func (s *Set) Fingerprint() string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range s.peers {
		b.WriteString(escapeSep(p.Name))
		b.WriteByte(0x1f)
		b.WriteString(p.Addr.String())
		b.WriteByte(0x1e)
	}
	b.WriteByte(0x1d)
	// Order is significant: the alert list is stored as given, so a reorder is
	// treated as a change and rebuilds. That is a cheap false positive, and
	// the alternative (sorting) would hide a genuine edit that only permutes.
	for _, a := range s.alerts {
		b.WriteString(escapeSep(a))
		b.WriteByte(0x1e)
	}
	return b.String()
}

func escapeSep(s string) string {
	if !strings.ContainsAny(s, "\x1d\x1e\x1f\\") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, "\x1f", `\u`, "\x1e", `\r`, "\x1d", `\g`)
	return r.Replace(s)
}

func familyOf(addr netip.Addr) string {
	if addr.Is4() {
		return "v4"
	}
	return "v6"
}
