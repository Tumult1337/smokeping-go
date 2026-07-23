package slave

import (
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

func respWithHealthGroup() cluster.ClusterConfigResp {
	return cluster.ClusterConfigResp{
		Interval: time.Minute,
		Pings:    20,
		Probes: map[string]cluster.ProbeDTO{
			"icmp":                {Type: "icmp", Timeout: 2 * time.Second},
			slavehealth.ProbeName: {Type: "icmp", Timeout: 2 * time.Second, NoTrace: true},
		},
		Targets: []config.Group{
			{Group: "core", Targets: []config.Target{{Name: "gw", Probe: "icmp", Host: "192.0.2.1"}}},
			{Group: slavehealth.Group, Title: slavehealth.GroupTitle, Targets: []config.Target{
				{Name: "frankfurt-1", Probe: slavehealth.ProbeName, Host: "10.44.0.2"},
				{Name: "tokyo-1", Probe: slavehealth.ProbeName, Host: "10.44.0.7"},
			}},
		},
	}
}

// Defence in depth: the master already excludes the recipient, but a stale or
// buggy master must not make a slave probe its own loopback path.
func TestBuildShimDropsSelfFromHealthGroup(t *testing.T) {
	shim := buildShim(respWithHealthGroup(), &config.Cluster{Name: "frankfurt-1"})

	for _, g := range shim.Targets {
		if g.Group != slavehealth.Group {
			continue
		}
		for _, tgt := range g.Targets {
			if tgt.Name == "frankfurt-1" {
				t.Fatal("slave must not health-probe itself")
			}
		}
		if len(g.Targets) != 1 {
			t.Fatalf("got %d health targets, want 1", len(g.Targets))
		}
		return
	}
	t.Fatal("health group missing from the shim")
}

// Dropping the last health target must drop the group too, so the slave's
// fingerprint matches a master that never sent the group at all.
func TestBuildShimDropsEmptyHealthGroup(t *testing.T) {
	resp := respWithHealthGroup()
	resp.Targets[1].Targets = resp.Targets[1].Targets[:1] // only frankfurt-1

	shim := buildShim(resp, &config.Cluster{Name: "frankfurt-1"})
	for _, g := range shim.Targets {
		if g.Group == slavehealth.Group {
			t.Fatalf("empty health group retained with %d targets", len(g.Targets))
		}
	}
}

// NoTrace must survive the wire round trip per-probe: the health probe has it
// set (health targets never need hop rows) while an ordinary icmp probe
// doesn't. If buildShim dropped the field, a slave would keep tracing on
// health targets despite the master disabling it, silently reinflating the
// storage cost the option exists to avoid.
func TestBuildShimCopiesNoTrace(t *testing.T) {
	shim := buildShim(respWithHealthGroup(), &config.Cluster{Name: "frankfurt-1"})

	health, ok := shim.Probes[slavehealth.ProbeName]
	if !ok {
		t.Fatal("health probe missing from shim")
	}
	if !health.NoTrace {
		t.Fatal("NoTrace not copied onto health probe: slave would keep tracing health targets")
	}

	icmp, ok := shim.Probes["icmp"]
	if !ok {
		t.Fatal("icmp probe missing from shim")
	}
	if icmp.NoTrace {
		t.Fatal("NoTrace incorrectly set on ordinary icmp probe")
	}
}

func TestBuildShimLeavesOrdinaryGroupsAlone(t *testing.T) {
	shim := buildShim(respWithHealthGroup(), &config.Cluster{Name: "frankfurt-1"})
	for _, g := range shim.Targets {
		if g.Group != "core" {
			continue
		}
		if len(g.Targets) != 1 || g.Targets[0].Name != "gw" {
			t.Fatalf("ordinary group altered: %+v", g)
		}
		return
	}
	t.Fatal("ordinary group missing from the shim")
}

// A slave whose own name collides with an ordinary target name must not have
// that target filtered — the filter applies only inside the health group.
func TestBuildShimFilterIsScopedToHealthGroup(t *testing.T) {
	resp := respWithHealthGroup()
	shim := buildShim(resp, &config.Cluster{Name: "gw"})
	for _, g := range shim.Targets {
		if g.Group != "core" {
			continue
		}
		// If the scoping guard is broken, the "gw" target gets filtered and
		// the empty group is dropped. This assertion catches that regression.
		if len(g.Targets) != 1 || g.Targets[0].Name != "gw" {
			t.Fatalf("filter escaped to ordinary group: %+v", g)
		}
		return
	}
	t.Fatal("core group missing from shim")
}
