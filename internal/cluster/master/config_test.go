package master

import (
	"slices"
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
)

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
	resp := BuildClusterConfig(cfg, "s1")
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
	resp := BuildClusterConfig(cfg, "s1")
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

	eu1 := BuildClusterConfig(cfg, "eu1").Targets
	if len(eu1) != 1 || len(eu1[0].Targets) != 2 {
		t.Fatalf("eu1 expected 2 targets (shared + eu-only), got %+v", eu1)
	}
	names := []string{eu1[0].Targets[0].Name, eu1[0].Targets[1].Name}
	if !(slices.Contains(names, "shared") && slices.Contains(names, "eu-only")) || slices.Contains(names, "us-only") {
		t.Errorf("eu1 target names = %v", names)
	}

	us1 := BuildClusterConfig(cfg, "us1").Targets
	if len(us1) != 1 || len(us1[0].Targets) != 2 {
		t.Fatalf("us1 expected 2 targets (shared + us-only), got %+v", us1)
	}

	stranger := BuildClusterConfig(cfg, "other").Targets
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
	resp := BuildClusterConfig(cfg, "s1")
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
	local := LocalTargets(cfg)
	if len(local.Targets) != 1 || len(local.Targets[0].Targets) != 1 || local.Targets[0].Targets[0].Name != "shared" {
		t.Errorf("master local view = %+v, want only shared", local.Targets)
	}
	// Original must be untouched so UI/ingest keep the full list.
	if len(cfg.Targets[0].Targets) != 2 {
		t.Errorf("LocalTargets mutated input: %+v", cfg.Targets)
	}
}

