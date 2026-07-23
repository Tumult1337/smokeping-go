package slave

import (
	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// buildShim turns a ClusterConfigResp from the master into an in-memory
// config.Config that satisfies the scheduler + probe.Build contracts. It is
// never written to disk — the slave's on-disk config is minimal and untouched.
// The local cluster block (from the slave's own file) is preserved so the
// scheduler stamps cycles with the slave's own name as Source, not whatever
// the master advertises.
func buildShim(resp cluster.ClusterConfigResp, local *config.Cluster) *config.Config {
	probes := make(map[string]config.Probe, len(resp.Probes))
	for name, p := range resp.Probes {
		probes[name] = config.Probe{
			Type:     p.Type,
			Timeout:  p.Timeout,
			Insecure: p.Insecure,
		}
	}
	return &config.Config{
		Interval: resp.Interval,
		Pings:    resp.Pings,
		Probes:   probes,
		Targets:  dropSelfFromHealthGroup(resp.Targets, local.Name),
		Cluster:  local,
	}
}

// dropSelfFromHealthGroup removes this slave's own entry from the health
// group. The master already excludes the recipient, so this is defence in
// depth against a stale or buggy master — a self-probe measures the loopback
// path and would double-count in a quorum.
//
// Filtering happens here rather than in buildScheduler because the shim is
// what the lifecycle fingerprints; filtering later would leave the fingerprint
// reflecting targets the scheduler never runs.
func dropSelfFromHealthGroup(groups []config.Group, self string) []config.Group {
	out := make([]config.Group, 0, len(groups))
	for _, g := range groups {
		if !slavehealth.IsHealthGroup(g.Group) {
			out = append(out, g)
			continue
		}
		targets := make([]config.Target, 0, len(g.Targets))
		for _, t := range g.Targets {
			if t.Name == self {
				continue
			}
			targets = append(targets, t)
		}
		// An empty group must vanish entirely, so the fingerprint matches a
		// master that never sent the group.
		if len(targets) == 0 {
			continue
		}
		out = append(out, config.Group{Group: g.Group, Title: g.Title, Targets: targets})
	}
	return out
}
