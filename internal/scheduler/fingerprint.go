package scheduler

import (
	"slices"
	"strconv"

	"github.com/tumult/gosmokeping/internal/config"
)

// Fingerprint produces a stable key that changes iff a config edit requires
// the scheduler to be rebuilt. Used by Supervisor to decide whether a SIGHUP
// reload needs a goroutine restart, and by the slave runner for the same
// decision after a /cluster/config pull.
//
// Included: interval, pings, target shape (group + name + probe + host + url +
// family + slave assignments), probe shape (name + type + timeout + insecure).
// Deliberately excluded: alert definitions (re-read per cycle by the evaluator),
// action URLs (re-read per dispatch), listen/cluster/storage blocks (not
// scheduler-visible).
//
// Family is included because it changes how a host is resolved — a family
// change must trigger a scheduler rebuild. Slaves is included because
// master.LocalTargets filters based on it; a change to the assignment list
// changes which targets the local scheduler probes.
func Fingerprint(cfg *config.Config) string {
	var out []byte
	out = append(out, cfg.Interval.String()...)
	out = append(out, '\x1f')
	out = append(out, strconv.Itoa(cfg.Pings)...)
	out = append(out, '\x1d')

	names := make([]string, 0, len(cfg.Probes))
	for name := range cfg.Probes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		p := cfg.Probes[name]
		out = append(out, name...)
		out = append(out, '\x1f')
		out = append(out, p.Type...)
		out = append(out, '\x1f')
		out = append(out, p.Timeout.String()...)
		out = append(out, '\x1f')
		out = append(out, strconv.FormatBool(p.Insecure)...)
		out = append(out, '\x1e')
	}
	out = append(out, '\x1d')

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
			out = append(out, '\x1f')
			out = append(out, t.Family...)
			out = append(out, '\x1f')
			for _, s := range t.Slaves {
				out = append(out, s...)
				out = append(out, '\x1f')
			}
			out = append(out, '\x1e')
		}
		out = append(out, '\x1d')
	}
	return string(out)
}
