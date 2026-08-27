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
// family + slave assignments + attached alert names), probe shape (name +
// type + timeout + insecure). A target's alert *attachment* list is baked
// into config.Target at Build time — same as its probe/host/url — so it must
// be included or attaching an alert and sending SIGHUP would leave the
// scheduler running the pre-attachment shape while /api/v1/targets already
// reports it attached.
//
// Deliberately excluded: alert *definitions* (cfg.Alerts — condition,
// sustained, actions, quorum), which the evaluator re-reads from the live
// config store per cycle rather than from anything baked in at Build time,
// so editing one doesn't need a rebuild. Also excluded: action definitions and
// listen/cluster/storage blocks (not scheduler-visible). Note that actions are
// no longer re-read per dispatch — alert.resolveActions snapshots them when the
// transition is committed — so an already-queued event still carries the old
// URL after a SIGHUP. Rotating a leaked webhook URL therefore does not reach
// events already in flight; the queue drains within its own budget.
//
// Family is included because it changes how a host is resolved — a family
// change must trigger a scheduler rebuild. Slaves is included because
// master.LocalTargets filters based on it; a change to the assignment list
// changes which targets the local scheduler probes.
func Fingerprint(cfg *config.Config) string {
	var out []byte
	field := func(s string) {
		out = append(out, config.EscapeSeparators(s)...)
		out = append(out, config.SepField)
	}
	out = append(out, cfg.Interval.String()...)
	out = append(out, config.SepField)
	out = append(out, strconv.Itoa(cfg.Pings)...)
	out = append(out, config.SepBlock)

	// scheduler.New binds cfg.Cluster.Source once, so only a rebuild changes the
	// label every locally probed cycle is stamped with. Omitted, a SIGHUP editing
	// it alone left the stamp on the old value while the ingest collision guard
	// and /api/v1/sources read the new one — refusing the name nothing writes and
	// admitting the name everything writes.
	if cfg.Cluster != nil {
		field(cfg.Cluster.Source)
	}
	out = append(out, config.SepBlock)

	names := make([]string, 0, len(cfg.Probes))
	for name := range cfg.Probes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		p := cfg.Probes[name]
		field(name)
		field(p.Type)
		field(p.Timeout.String())
		field(strconv.FormatBool(p.Insecure))
		// NoTrace is baked into the icmp prober at Build time, so only a rebuild
		// can change it — omitting it made cluster.health_hops and every probe's
		// no_trace edit take effect at the next process restart and not at SIGHUP.
		field(strconv.FormatBool(p.NoTrace))
		out = append(out, config.SepEntry)
	}
	out = append(out, config.SepBlock)

	for _, g := range cfg.Targets {
		field(g.Group)
		for _, t := range g.Targets {
			field(t.Name)
			field(t.Probe)
			field(t.Host)
			field(t.URL)
			field(t.Family)
			for _, s := range t.Slaves {
				field(s)
			}
			// The two lists are delimited: run together on one separator, moving
			// a name from slaves to alerts left the bytes unchanged, so the
			// rebuild that reassigns the target and attaches the alert never ran.
			out = append(out, config.SepList)
			for _, a := range t.Alerts {
				field(a)
			}
			out = append(out, config.SepEntry)
		}
		out = append(out, config.SepBlock)
	}
	return string(out)
}
