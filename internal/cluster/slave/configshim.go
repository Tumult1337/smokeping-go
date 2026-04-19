package slave

import (
	"strconv"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
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
		Targets:  resp.Targets,
		Cluster:  local,
	}
}

// targetsFingerprint produces a stable key for the scheduler restart
// decision. When this changes between two /config pulls we rebuild the probe
// registry + scheduler; otherwise we keep running.
//
// Deliberately coarse: group + name + probe + host + url. Changing a
// target's alert list or slave assignments is a master-side concern that
// doesn't affect what the slave probes.
func targetsFingerprint(cfg *config.Config) string {
	var out []byte
	for _, g := range cfg.Targets {
		out = append(out, g.Group...)
		out = append(out, '\x1f')
		for _, t := range g.Targets {
			out = append(out, t.Name...)
			out = append(out, '\x1f')
			out = append(out, t.Probe...)
			out = append(out, '\x1f')
			out = append(out, t.Host...)
			out = append(out, '\x1f')
			out = append(out, t.URL...)
			out = append(out, '\x1e')
		}
		out = append(out, '\x1d')
	}
	// Interval + Pings are part of the "shape" — a slave must restart its
	// scheduler if the cadence changes.
	out = append(out, cfg.Interval.String()...)
	out = append(out, '\x1f')
	out = append(out, strconv.Itoa(cfg.Pings)...)
	return string(out)
}
