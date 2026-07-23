package slavehealth

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

func peers() []Peer {
	return []Peer{
		{Name: "ashburn-1", Addr: netip.MustParseAddr("10.44.0.9")},
		{Name: "frankfurt-1", Addr: netip.MustParseAddr("10.44.0.2")},
		{Name: "tokyo-1", Addr: netip.MustParseAddr("2001:db8::7")},
	}
}

func TestProbeCarriesRealHosts(t *testing.T) {
	groups := NewSet(peers(), nil).Probe("")
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Group != Group {
		t.Fatalf("got group %q, want %q", groups[0].Group, Group)
	}
	if groups[0].Title != GroupTitle {
		t.Fatalf("got title %q, want %q", groups[0].Title, GroupTitle)
	}
	if len(groups[0].Targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(groups[0].Targets))
	}
	for _, tgt := range groups[0].Targets {
		if tgt.Host == "" {
			t.Fatalf("target %q has no host; Probe must carry real addresses", tgt.Name)
		}
		if tgt.Probe != ProbeName {
			t.Fatalf("target %q uses probe %q, want %q", tgt.Name, tgt.Probe, ProbeName)
		}
	}
}

// A node must never health-probe itself: the result is meaningless and it
// would double-count in a quorum.
func TestProbeExcludesSelf(t *testing.T) {
	groups := NewSet(peers(), nil).Probe("frankfurt-1")
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	for _, tgt := range groups[0].Targets {
		if tgt.Name == "frankfurt-1" {
			t.Fatal("Probe(self) must exclude the caller")
		}
	}
	if len(groups[0].Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(groups[0].Targets))
	}
}

// Excluding the only peer must yield no group at all, not an empty group —
// an empty group would render as a stray sidebar heading.
func TestProbeExcludingOnlyPeerYieldsNoGroup(t *testing.T) {
	one := []Peer{{Name: "solo", Addr: netip.MustParseAddr("10.44.0.2")}}
	if groups := NewSet(one, nil).Probe("solo"); len(groups) != 0 {
		t.Fatalf("got %d groups, want 0", len(groups))
	}
}

func TestEmptySetYieldsNothing(t *testing.T) {
	s := NewSet(nil, nil)
	if groups := s.Probe(""); len(groups) != 0 {
		t.Fatalf("got %d groups, want 0", len(groups))
	}
	if refs := s.Public(); len(refs) != 0 {
		t.Fatalf("got %d refs, want 0", len(refs))
	}
}

// The core guarantee: nothing reachable from Public() carries an address.
func TestPublicNeverCarriesAHost(t *testing.T) {
	s := NewSet(peers(), nil)
	for _, ref := range s.Public() {
		if ref.Target.Host != "" {
			t.Fatalf("Public() leaked host %q for %q", ref.Target.Host, ref.ID())
		}
		if ref.Target.URL != "" {
			t.Fatalf("Public() leaked url %q for %q", ref.Target.URL, ref.ID())
		}
		if ref.Group != Group {
			t.Fatalf("got group %q, want %q", ref.Group, Group)
		}
	}
}

// Aliasing guard: if Public() shared a backing array with Probe(), mutating
// one would corrupt the other and an address could surface through the API.
func TestPublicDoesNotAliasProbe(t *testing.T) {
	s := NewSet(peers(), nil)

	groups := s.Probe("")
	refs := s.Public()

	// Mutate the Probe view; Public must be unaffected.
	groups[0].Targets[0].Host = "mutated.example"
	for _, ref := range s.Public() {
		if ref.Target.Host != "" {
			t.Fatalf("mutating Probe() leaked into Public(): %q", ref.Target.Host)
		}
	}

	// And the reverse: mutating Public must not reach Probe.
	refs[0].Target.Name = "mutated"
	for _, g := range s.Probe("") {
		for _, tgt := range g.Targets {
			if tgt.Name == "mutated" {
				t.Fatal("mutating Public() leaked into Probe()")
			}
		}
	}
}

// Calling Probe twice must yield independent slices — Task 5 hands one copy to
// the local scheduler and another to each slave's config.
func TestProbeReturnsIndependentCopies(t *testing.T) {
	s := NewSet(peers(), nil)
	a := s.Probe("")
	b := s.Probe("")
	a[0].Targets[0].Host = "mutated.example"
	if b[0].Targets[0].Host == "mutated.example" {
		t.Fatal("two Probe() calls share a backing array")
	}
}

func TestFingerprintChangesWithMembership(t *testing.T) {
	base := NewSet(peers(), nil).Fingerprint()

	if same := NewSet(peers(), nil).Fingerprint(); same != base {
		t.Fatal("fingerprint must be stable for an identical peer set")
	}

	fewer := NewSet(peers()[:2], nil).Fingerprint()
	if fewer == base {
		t.Fatal("removing a peer must change the fingerprint")
	}

	moved := peers()
	moved[0].Addr = netip.MustParseAddr("10.44.0.99")
	if NewSet(moved, nil).Fingerprint() == base {
		t.Fatal("changing a peer address must change the fingerprint")
	}
}

// The fingerprint feeds a rebuild decision, so a name containing the field
// separator must not be able to forge an identical fingerprint for a
// different membership set.
func TestFingerprintResistsSeparatorInjection(t *testing.T) {
	a := NewSet([]Peer{
		{Name: "a", Addr: netip.MustParseAddr("10.0.0.1")},
		{Name: "b", Addr: netip.MustParseAddr("10.0.0.2")},
	}, nil).Fingerprint()
	b := NewSet([]Peer{
		{Name: "a\x1f10.0.0.1\x1eb", Addr: netip.MustParseAddr("10.0.0.2")},
	}, nil).Fingerprint()
	if a == b {
		t.Fatal("separator injection produced a colliding fingerprint")
	}
}

// Membership is what matters, not the order the caller happened to build the
// slice in — a reorder must not trigger a spurious scheduler rebuild.
func TestFingerprintIsOrderInvariant(t *testing.T) {
	forward := peers()
	reversed := []Peer{forward[2], forward[0], forward[1]}

	a := NewSet(forward, nil).Fingerprint()
	b := NewSet(reversed, nil).Fingerprint()
	if a != b {
		t.Fatalf("fingerprint depends on peer order: forward %q, reversed %q", a, b)
	}
}

// Probe() and Public() must agree on membership: a peer Probe() can't reach
// (invalid address) must not appear in Public() either, or the API
// advertises a target nothing ever collects data for.
func TestInvalidAddrPeerExcludedFromBothViews(t *testing.T) {
	set := NewSet([]Peer{
		{Name: "good", Addr: netip.MustParseAddr("10.44.0.2")},
		{Name: "bad", Addr: netip.Addr{}},
	}, nil)

	groups := set.Probe("")
	if len(groups) != 1 || len(groups[0].Targets) != 1 {
		t.Fatalf("got groups %+v, want exactly the valid peer", groups)
	}
	for _, tgt := range groups[0].Targets {
		if tgt.Name == "bad" {
			t.Fatal("Probe() must exclude a peer with an invalid address")
		}
	}

	refs := set.Public()
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	for _, ref := range refs {
		if ref.Target.Name == "bad" {
			t.Fatal("Public() must exclude a peer with an invalid address")
		}
	}
}

func TestIsHealthGroup(t *testing.T) {
	if !IsHealthGroup(Group) {
		t.Fatalf("IsHealthGroup(%q) = false, want true", Group)
	}
	if IsHealthGroup("core") {
		t.Fatal(`IsHealthGroup("core") = true, want false`)
	}
}

func TestProbeDefIsICMP(t *testing.T) {
	p := ProbeDef(0, true)
	if p.Type != "icmp" {
		t.Fatalf("got probe type %q, want icmp", p.Type)
	}
	if p.Timeout <= 0 {
		t.Fatalf("got timeout %v, want a positive default", p.Timeout)
	}
}

func TestProbeDefHopsEnabled(t *testing.T) {
	if p := ProbeDef(0, true); p.NoTrace {
		t.Fatal("ProbeDef(_, true) must leave tracing on")
	}
}

func TestProbeDefHopsDisabled(t *testing.T) {
	if p := ProbeDef(0, false); !p.NoTrace {
		t.Fatal("ProbeDef(_, false) must disable tracing")
	}
}

func TestGroupNameIsReserved(t *testing.T) {
	if !strings.HasPrefix(Group, "_") {
		t.Fatalf("group %q must start with _ to stay outside the user namespace", Group)
	}
}

// The config package duplicates these names to avoid an import cycle. If they
// drift, config would stop reserving the namespace the mesh actually uses.
func TestReservedNamesMatchConfigValidation(t *testing.T) {
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    20,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "127.0.0.1:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group:   Group,
			Targets: []config.Target{{Name: "x", Probe: "icmp", Host: "192.0.2.1"}},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("config.Validate() accepted group %q; the reserved name has drifted", Group)
	}

	cfg2 := &config.Config{
		Interval: time.Minute,
		Pings:    20,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "127.0.0.1:9000"}},
		Probes:   map[string]config.Probe{ProbeName: {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group:   "core",
			Targets: []config.Target{{Name: "x", Probe: ProbeName, Host: "192.0.2.1"}},
		}},
	}
	if err := cfg2.Validate(); err == nil {
		t.Fatalf("config.Validate() accepted probe %q; the reserved name has drifted", ProbeName)
	}
}

func TestNilSetIsSafe(t *testing.T) {
	var s *Set
	if got := s.Fingerprint(); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := s.Probe(""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if got := s.Public(); len(got) != 0 {
		t.Fatalf("got %d refs, want 0", len(got))
	}
}

// Health targets are synthesized, so cluster.health_alerts is the only way an
// alert can ever be attached to one. Without the stamp, alert.Evaluator's
// `len(alerts) == 0` early return drops every health cycle and a slave going
// down can never fire — which also makes quorum's headline use case dead.
func TestProbeStampsHealthAlerts(t *testing.T) {
	s := NewSet(peers(), []string{"slave-unreachable"})
	groups := s.Probe("")
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	for _, tgt := range groups[0].Targets {
		if len(tgt.Alerts) != 1 || tgt.Alerts[0] != "slave-unreachable" {
			t.Fatalf("target %q alerts = %v, want [slave-unreachable]", tgt.Name, tgt.Alerts)
		}
	}
}

// Public exposes alert names deliberately (see Public's comment): they are
// operator-chosen labels with no address content, and targetDTO surfaces them
// for ordinary targets.
func TestPublicExposesHealthAlerts(t *testing.T) {
	s := NewSet(peers(), []string{"slave-unreachable"})
	refs := s.Public()
	if len(refs) == 0 {
		t.Fatal("no public refs")
	}
	for _, ref := range refs {
		if len(ref.Target.Alerts) != 1 || ref.Target.Alerts[0] != "slave-unreachable" {
			t.Fatalf("ref %q alerts = %v, want [slave-unreachable]", ref.ID(), ref.Target.Alerts)
		}
	}
}

// Per-target clones: a consumer rewriting one target's alert list must not
// reach into the snapshot shared with its peers.
func TestAlertsAreNotSharedAcrossTargets(t *testing.T) {
	s := NewSet(peers(), []string{"slave-unreachable"})
	groups := s.Probe("")
	groups[0].Targets[0].Alerts[0] = "mutated"
	for i, tgt := range groups[0].Targets[1:] {
		if tgt.Alerts[0] != "slave-unreachable" {
			t.Fatalf("target %d alerts corrupted: %v", i+1, tgt.Alerts)
		}
	}
	for _, ref := range s.Public() {
		if ref.Target.Alerts[0] != "slave-unreachable" {
			t.Fatalf("Public() saw a mutated alert list: %v", ref.Target.Alerts)
		}
	}
}
