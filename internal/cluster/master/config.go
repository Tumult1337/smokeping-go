package master

import (
	"slices"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// BuildClusterConfig returns the scrubbed target+probe set the named slave
// should probe. Targets with a non-empty Slaves list are assigned: only the
// slaves in that list receive them, and the master's own scheduler skips
// them (handled upstream in runNode). Targets with an empty Slaves list are
// unassigned and ship to every slave (plus the master probes them locally).
//
// An empty slaveName ships the unfiltered, unassigned-only view — useful for
// ad-hoc `curl` debugging against /cluster/config without a slave identity.
//
// Alerts are stripped unconditionally: they are evaluated master-side, and a
// stale slave config must never carry alert references. The Slaves list is
// also stripped so slaves cannot see their peers' assignments.
func BuildClusterConfig(cfg *config.Config, slaveName string) cluster.ClusterConfigResp {
	probes := make(map[string]cluster.ProbeDTO, len(cfg.Probes))
	for k, p := range cfg.Probes {
		probes[k] = cluster.ProbeDTO{Type: p.Type, Timeout: p.Timeout, Insecure: p.Insecure}
	}

	groups := make([]config.Group, 0, len(cfg.Targets))
	for _, g := range cfg.Targets {
		targets := make([]config.Target, 0, len(g.Targets))
		for _, t := range g.Targets {
			if !targetVisibleToSlave(t, slaveName) {
				continue
			}
			targets = append(targets, sanitizeTarget(t))
		}
		if len(targets) == 0 {
			continue
		}
		groups = append(groups, config.Group{Group: g.Group, Title: g.Title, Targets: targets})
	}

	return cluster.ClusterConfigResp{
		Interval: cfg.Interval,
		Pings:    cfg.Pings,
		Probes:   probes,
		Targets:  groups,
	}
}

// targetVisibleToSlave returns true if the named slave should probe this
// target. Unassigned targets (empty Slaves list) are visible to everyone;
// assigned targets are visible only to slaves named in the list. An empty
// slaveName acts as "no slave identity" — only unassigned targets are shown,
// so debug `curl` reflects what a freshly-registered slave would see.
func targetVisibleToSlave(t config.Target, slaveName string) bool {
	if len(t.Slaves) == 0 {
		return true
	}
	return slices.Contains(t.Slaves, slaveName)
}

// sanitizeTarget drops fields a slave never needs — Alerts (evaluated
// master-side) and Slaves (peer assignments are none of this slave's
// business). Keeping them out also means a slave with a stale config can't
// mis-dispatch alerts or leak the assignment topology.
func sanitizeTarget(t config.Target) config.Target {
	return config.Target{
		Name:  t.Name,
		Title: t.Title,
		Host:  t.Host,
		URL:   t.URL,
		Probe: t.Probe,
	}
}

// LocalTargets returns a copy of cfg with any target that has a non-empty
// Slaves list removed. This is what the master's own scheduler sees: the
// stored cfg remains authoritative for the UI and /cluster/config, but the
// local probe loop skips anything that's been assigned elsewhere.
func LocalTargets(cfg *config.Config) *config.Config {
	out := *cfg
	groups := make([]config.Group, 0, len(cfg.Targets))
	for _, g := range cfg.Targets {
		targets := make([]config.Target, 0, len(g.Targets))
		for _, t := range g.Targets {
			if len(t.Slaves) == 0 {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			continue
		}
		groups = append(groups, config.Group{Group: g.Group, Title: g.Title, Targets: targets})
	}
	out.Targets = groups
	return &out
}
