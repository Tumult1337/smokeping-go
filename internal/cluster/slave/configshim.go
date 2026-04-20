package slave

import (
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
