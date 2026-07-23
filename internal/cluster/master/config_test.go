package master

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// baseConfig returns a minimal config with one ordinary group and an icmp
// probe — the fixture the health-distribution tests build on.
func baseConfig() *config.Config {
	return &config.Config{
		Interval: 30_000_000_000,
		Pings:    5,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "a", Host: "10.0.0.1", Probe: "icmp"},
			},
		}},
	}
}

func TestBuildClusterConfigShipsAllTargets(t *testing.T) {
	cfg := &config.Config{
		Probes: map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "a", Host: "10.0.0.1", Probe: "icmp"},
				{Name: "b", Host: "10.0.0.2", Probe: "icmp"},
			},
		}},
	}
	resp := BuildClusterConfig(cfg, "s1", nil)
	if len(resp.Targets) != 1 || len(resp.Targets[0].Targets) != 2 {
		t.Fatalf("expected both targets shipped, got %+v", resp.Targets)
	}
}

func TestBuildClusterConfigStripsAlerts(t *testing.T) {
	cfg := &config.Config{
		Probes: map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "t1", Host: "10.0.0.1", Probe: "icmp", Alerts: []string{"pageops"}},
			},
		}},
	}
	resp := BuildClusterConfig(cfg, "s1", nil)
	if got := resp.Targets[0].Targets[0].Alerts; len(got) != 0 {
		t.Errorf("alerts leaked to slave: %v", got)
	}
}

func TestBuildClusterConfigFiltersAssignedTargets(t *testing.T) {
	cfg := &config.Config{
		Probes: map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "shared", Host: "10.0.0.1", Probe: "icmp"},
				{Name: "eu-only", Host: "10.0.0.2", Probe: "icmp", Slaves: []string{"eu1", "eu2"}},
				{Name: "us-only", Host: "10.0.0.3", Probe: "icmp", Slaves: []string{"us1"}},
			},
		}},
	}

	eu1 := BuildClusterConfig(cfg, "eu1", nil).Targets
	if len(eu1) != 1 || len(eu1[0].Targets) != 2 {
		t.Fatalf("eu1 expected 2 targets (shared + eu-only), got %+v", eu1)
	}
	names := []string{eu1[0].Targets[0].Name, eu1[0].Targets[1].Name}
	if !slices.Contains(names, "shared") || !slices.Contains(names, "eu-only") || slices.Contains(names, "us-only") {
		t.Errorf("eu1 target names = %v", names)
	}

	us1 := BuildClusterConfig(cfg, "us1", nil).Targets
	if len(us1) != 1 || len(us1[0].Targets) != 2 {
		t.Fatalf("us1 expected 2 targets (shared + us-only), got %+v", us1)
	}

	stranger := BuildClusterConfig(cfg, "other", nil).Targets
	if len(stranger) != 1 || len(stranger[0].Targets) != 1 || stranger[0].Targets[0].Name != "shared" {
		t.Errorf("unknown slave should see only shared target, got %+v", stranger)
	}
}

func TestBuildClusterConfigStripsSlavesField(t *testing.T) {
	cfg := &config.Config{
		Probes: map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "t1", Host: "10.0.0.1", Probe: "icmp", Slaves: []string{"s1", "s2"}},
			},
		}},
	}
	resp := BuildClusterConfig(cfg, "s1", nil)
	if got := resp.Targets[0].Targets[0].Slaves; len(got) != 0 {
		t.Errorf("slaves list leaked to slave: %v", got)
	}
}

func TestLocalTargetsDropsAssigned(t *testing.T) {
	cfg := &config.Config{
		Interval: 30_000_000_000,
		Pings:    5,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "shared", Host: "10.0.0.1", Probe: "icmp"},
				{Name: "assigned", Host: "10.0.0.2", Probe: "icmp", Slaves: []string{"eu1"}},
			},
		}},
	}
	local := LocalTargets(cfg, nil)
	if len(local.Targets) != 1 || len(local.Targets[0].Targets) != 1 || local.Targets[0].Targets[0].Name != "shared" {
		t.Errorf("master local view = %+v, want only shared", local.Targets)
	}
	// Original must be untouched so UI/ingest keep the full list.
	if len(cfg.Targets[0].Targets) != 2 {
		t.Errorf("LocalTargets mutated input: %+v", cfg.Targets)
	}
}

func healthSet(t *testing.T) *slavehealth.Set {
	t.Helper()
	return slavehealth.NewSet([]slavehealth.Peer{
		{Name: "frankfurt-1", Addr: netip.MustParseAddr("10.44.0.2")},
		{Name: "tokyo-1", Addr: netip.MustParseAddr("10.44.0.7")},
	})
}

func findGroup(groups []config.Group, name string) (config.Group, bool) {
	for _, g := range groups {
		if g.Group == name {
			return g, true
		}
	}
	return config.Group{}, false
}

// Every slave probes every other slave, so the health group ships to slaves —
// unlike alerts and peer assignments, which stay master-side.
func TestBuildClusterConfigShipsHealthGroupMinusSelf(t *testing.T) {
	cfg := baseConfig()
	resp := BuildClusterConfig(cfg, "frankfurt-1", healthSet(t))

	g, ok := findGroup(resp.Targets, slavehealth.Group)
	if !ok {
		t.Fatal("health group missing from the slave's config")
	}
	if len(g.Targets) != 1 || g.Targets[0].Name != "tokyo-1" {
		t.Fatalf("got targets %+v, want only tokyo-1", g.Targets)
	}
	if g.Targets[0].Host == "" {
		t.Fatal("health target shipped to a slave must carry a real host")
	}
}

// The synthesized probe must ship too, or the slave's probe.Build rejects the
// target for referencing an undefined probe.
func TestBuildClusterConfigShipsHealthProbe(t *testing.T) {
	resp := BuildClusterConfig(baseConfig(), "frankfurt-1", healthSet(t))
	p, ok := resp.Probes[slavehealth.ProbeName]
	if !ok {
		t.Fatalf("probe %q missing from the slave's config", slavehealth.ProbeName)
	}
	if p.Type != "icmp" {
		t.Fatalf("got probe type %q, want icmp", p.Type)
	}
}

// A lone slave has nobody to probe; it must not receive an empty group.
func TestBuildClusterConfigOmitsEmptyHealthGroup(t *testing.T) {
	solo := slavehealth.NewSet([]slavehealth.Peer{
		{Name: "frankfurt-1", Addr: netip.MustParseAddr("10.44.0.2")},
	})
	resp := BuildClusterConfig(baseConfig(), "frankfurt-1", solo)
	if _, ok := findGroup(resp.Targets, slavehealth.Group); ok {
		t.Fatal("a slave that is the only peer must not receive an empty health group")
	}
}

func TestBuildClusterConfigNilHealthSet(t *testing.T) {
	resp := BuildClusterConfig(baseConfig(), "frankfurt-1", nil)
	if _, ok := findGroup(resp.Targets, slavehealth.Group); ok {
		t.Fatal("nil health set must produce no health group")
	}
}

// The master is not a peer, so it probes every slave.
func TestLocalTargetsIncludesAllHealthPeers(t *testing.T) {
	local := LocalTargets(baseConfig(), healthSet(t))
	g, ok := findGroup(local.Targets, slavehealth.Group)
	if !ok {
		t.Fatal("master's local view is missing the health group")
	}
	if len(g.Targets) != 2 {
		t.Fatalf("got %d health targets, want 2", len(g.Targets))
	}
	for _, tgt := range g.Targets {
		if tgt.Host == "" {
			t.Fatalf("health target %q has no host in the local view", tgt.Name)
		}
	}
}

// The synthesized probe must exist in the master's local view too, and adding
// it must not mutate the stored config's probe map.
func TestLocalTargetsAddsHealthProbeWithoutMutatingStore(t *testing.T) {
	cfg := baseConfig()
	local := LocalTargets(cfg, healthSet(t))

	if _, ok := local.Probes[slavehealth.ProbeName]; !ok {
		t.Fatalf("probe %q missing from the master's local view", slavehealth.ProbeName)
	}
	if _, leaked := cfg.Probes[slavehealth.ProbeName]; leaked {
		t.Fatal("LocalTargets mutated the stored config's probe map")
	}
}

func TestLocalTargetsNilHealthSetUnchanged(t *testing.T) {
	cfg := baseConfig()
	local := LocalTargets(cfg, nil)
	if _, ok := findGroup(local.Targets, slavehealth.Group); ok {
		t.Fatal("nil health set must add no health group")
	}
	if len(local.Targets) != len(cfg.Targets) {
		t.Fatalf("got %d groups, want %d", len(local.Targets), len(cfg.Targets))
	}
}
