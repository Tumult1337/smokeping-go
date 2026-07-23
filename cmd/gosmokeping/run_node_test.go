package main

import (
	"net/netip"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// TestLocalViewRegistryResolvesHealthProbe pins the join between the local
// target view and the probe registry built from it.
//
// LocalTargets injects the synthetic health probe into a clone of the probe
// map, and config.Validate rejects that name in user config, so a registry
// built from the stored cfg.Probes can never resolve it — the scheduler would
// drop every health target with "probe not found for target" and the mesh
// would silently collect nothing. Testing LocalTargets and probe.Build
// separately cannot see this; only the join can.
func TestLocalViewRegistryResolvesHealthProbe(t *testing.T) {
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    5,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}},
		Targets: []config.Group{{
			Group:   "core",
			Targets: []config.Target{{Name: "gw", Host: "192.0.2.1", Probe: "icmp"}},
		}},
		Cluster: &config.Cluster{Token: "t", Source: "master"},
	}
	health := slavehealth.NewSet([]slavehealth.Peer{
		{Name: "tokyo-1", Addr: netip.MustParseAddr("10.44.0.7")},
	}, nil)

	local, registry, err := localView(cfg, health)
	if err != nil {
		t.Fatalf("localView: %v", err)
	}

	var healthTargets int
	for _, g := range local.Targets {
		if slavehealth.IsHealthGroup(g.Group) {
			healthTargets += len(g.Targets)
		}
	}
	if healthTargets != 1 {
		t.Fatalf("built config has %d health targets, want 1", healthTargets)
	}

	if _, ok := registry.Get(slavehealth.ProbeName); !ok {
		t.Fatalf("probe registry cannot resolve %q — every health target would be skipped by the scheduler", slavehealth.ProbeName)
	}
	// The user's own probes must still resolve through the same registry.
	if _, ok := registry.Get("icmp"); !ok {
		t.Fatal("probe registry lost the user-defined icmp probe")
	}
}

// TestLocalViewWithoutHealthLeavesProbesAlone guards the standalone path: a
// nil health set must not conjure a synthetic probe into the registry.
func TestLocalViewWithoutHealthLeavesProbesAlone(t *testing.T) {
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    5,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}},
		Targets: []config.Group{{
			Group:   "core",
			Targets: []config.Target{{Name: "gw", Host: "192.0.2.1", Probe: "icmp"}},
		}},
	}
	_, registry, err := localView(cfg, nil)
	if err != nil {
		t.Fatalf("localView: %v", err)
	}
	if _, ok := registry.Get(slavehealth.ProbeName); ok {
		t.Fatalf("registry resolved %q with no health mesh configured", slavehealth.ProbeName)
	}
}
