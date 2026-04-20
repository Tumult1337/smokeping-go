package scheduler

import (
	"sort"
	"strconv"

	"github.com/tumult/gosmokeping/internal/config"
)

// Fingerprint produces a stable key that changes iff a config edit requires
// the scheduler to be rebuilt. Used by Supervisor to decide whether a SIGHUP
// reload needs a goroutine restart, and by the slave runner for the same
// decision after a /cluster/config pull.
//
// Included: interval, pings, target shape (group + name + probe + host + url),
// probe shape (name + type + timeout + insecure). Deliberately excluded:
// alert definitions (re-read per cycle by the evaluator), action URLs
// (re-read per dispatch), listen/cluster/storage blocks (not scheduler-
// visible), and the slave assignment list (applied by master.LocalTargets
// before Fingerprint is called, so the filtered view already reflects any
// reassignment).
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
	sort.Strings(names)
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
			out = append(out, '\x1e')
		}
		out = append(out, '\x1d')
	}
	return string(out)
}
