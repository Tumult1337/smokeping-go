package main

import (
	"log/slog"
	"net/netip"
	"os"
	"strings"
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

// currentSlavePins is the live closure the registry reads pins through; it
// must parse from whatever config it is handed (the caller passes
// store.Current()) and treat an absent cluster block as unpinned.
func TestCurrentSlavePins(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if got := currentSlavePins(log, &config.Config{}); got != nil {
		t.Fatalf("nil cluster block: pins = %v, want nil", got)
	}
	pins := currentSlavePins(log, &config.Config{
		Cluster: &config.Cluster{SlaveAddrs: map[string]string{"tokyo-1": "10.44.0.7"}},
	})
	if want := netip.MustParseAddr("10.44.0.7"); pins["tokyo-1"] != want {
		t.Fatalf("pins = %v, want tokyo-1 -> %s", pins, want)
	}
}

// Link three of the path to Evaluator.PruneDeparted lives in a Supervisor
// literal built inside runNode, which no unit test can reach without standing
// up storage and a listener. Severing it leaves the sweep unreachable in the
// shipped binary with the whole suite green, so the wiring is pinned by
// reading the source — the same shape internal/config/tracebounds_test.go uses
// for the trace bounds it cannot import.
func TestRunNodeWiresOnRebuiltToPruneDeparted(t *testing.T) {
	raw, err := os.ReadFile("run_node.go")
	if err != nil {
		t.Fatalf("read run_node.go: %v", err)
	}
	src := string(raw)
	sup := strings.Index(src, "sup := &scheduler.Supervisor{")
	if sup < 0 {
		t.Fatal("run_node.go no longer builds a scheduler.Supervisor; update this guard")
	}
	end := strings.Index(src[sup:], "\n\t}")
	if end < 0 {
		t.Fatal("could not delimit the Supervisor literal")
	}
	lit := src[sup : sup+end]
	if !strings.Contains(lit, "OnRebuilt:") {
		t.Fatal("the Supervisor sets no OnRebuilt: Evaluator.PruneDeparted is unreachable, so state for a removed or renamed (target, alert) pair is stranded for the process's life — and a stranded StateFiring resumes firing if the target comes back, making the recovery a non-transition so the page never closes")
	}
	if !strings.Contains(lit, "PruneDeparted") {
		t.Fatal("OnRebuilt is set but does not call PruneDeparted")
	}
}
