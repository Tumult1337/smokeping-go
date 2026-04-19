package master

import (
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
)

func TestBuildClusterConfigFiltersBySlave(t *testing.T) {
	cfg := &config.Config{
		Probes: map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "all", Host: "10.0.0.1", Probe: "icmp", Slaves: []string{"s1", "s2"}},
				{Name: "s1-only", Host: "10.0.0.2", Probe: "icmp", Slaves: []string{"s1"}},
				{Name: "master-only", Host: "10.0.0.3", Probe: "icmp"},
			},
		}},
	}

	tests := []struct {
		name  string
		slave string
		want  []string
	}{
		{"s1 sees its own + all-slave targets", "s1", []string{"all", "s1-only"}},
		{"s2 sees only all-slave target", "s2", []string{"all"}},
		{"unknown slave sees nothing", "s3", nil},
		{"empty slaveName bypasses filter", "", []string{"all", "s1-only", "master-only"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := BuildClusterConfig(cfg, tc.slave)
			var got []string
			for _, g := range resp.Targets {
				for _, tt := range g.Targets {
					got = append(got, tt.Name)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i, n := range tc.want {
				if got[i] != n {
					t.Errorf("got[%d] = %q, want %q", i, got[i], n)
				}
			}
		})
	}
}

func TestBuildClusterConfigStripsAlerts(t *testing.T) {
	cfg := &config.Config{
		Probes: map[string]config.Probe{"icmp": {Type: "icmp"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "t1", Host: "10.0.0.1", Probe: "icmp", Slaves: []string{"s1"}, Alerts: []string{"pageops"}},
			},
		}},
	}
	resp := BuildClusterConfig(cfg, "s1")
	if got := resp.Targets[0].Targets[0].Alerts; len(got) != 0 {
		t.Errorf("alerts leaked to slave: %v", got)
	}
}
