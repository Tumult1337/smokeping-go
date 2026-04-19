package master

import (
	"slices"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// BuildClusterConfig returns the subset of cfg a slave with the given name
// needs to probe. The slave filter keeps targets where `slaveName` is listed
// in Target.Slaves, plus targets that restrict to specific slaves at all
// (empty Slaves = master-only, never shipped to a slave). When slaveName is
// empty the filter is a no-op — useful for debugging and the future
// heartbeat-without-name case.
func BuildClusterConfig(cfg *config.Config, slaveName string) cluster.ClusterConfigResp {
	probes := make(map[string]cluster.ProbeDTO, len(cfg.Probes))
	for k, p := range cfg.Probes {
		probes[k] = cluster.ProbeDTO{Type: p.Type, Timeout: p.Timeout, Insecure: p.Insecure}
	}

	groups := make([]config.Group, 0, len(cfg.Targets))
	for _, g := range cfg.Targets {
		filtered := make([]config.Target, 0, len(g.Targets))
		for _, t := range g.Targets {
			if !targetAssignedToSlave(t, slaveName) {
				continue
			}
			filtered = append(filtered, sanitizeTarget(t))
		}
		if len(filtered) == 0 {
			continue
		}
		groups = append(groups, config.Group{Group: g.Group, Title: g.Title, Targets: filtered})
	}

	return cluster.ClusterConfigResp{
		Interval: cfg.Interval,
		Pings:    cfg.Pings,
		Probes:   probes,
		Targets:  groups,
	}
}

// targetAssignedToSlave returns true when `slaveName` should probe this
// target. Empty Target.Slaves = master-only. Empty slaveName = no filter
// (pass everything) so tests / curl-debug see the whole picture.
func targetAssignedToSlave(t config.Target, slaveName string) bool {
	if slaveName == "" {
		return true
	}
	return slices.Contains(t.Slaves, slaveName)
}

// sanitizeTarget drops fields a slave never needs — notably Alerts, which
// are evaluated master-side. Keeping the alerts array out of the slave
// config also means a slave with a stale config can't mis-dispatch alerts.
func sanitizeTarget(t config.Target) config.Target {
	return config.Target{
		Name:   t.Name,
		Title:  t.Title,
		Host:   t.Host,
		URL:    t.URL,
		Probe:  t.Probe,
		Slaves: t.Slaves,
	}
}
