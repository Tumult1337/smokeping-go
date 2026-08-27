package scheduler

import (
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

func fpConfig(probes map[string]config.Probe, targets []config.Group) *config.Config {
	return &config.Config{
		Interval: 10 * time.Second,
		Pings:    5,
		Probes:   probes,
		Targets:  targets,
	}
}

func oneTarget(t config.Target) []config.Group {
	return []config.Group{{Group: "g", Targets: []config.Target{t}}}
}

// NoTrace is baked into the icmp prober at Build time, so only a rebuild can
// change it. Omitted from the fingerprint, cluster.health_hops and every
// probe's no_trace edit passed SIGHUP without one and took effect at the next
// process restart — on the master and on every slave.
func TestFingerprintSeesNoTrace(t *testing.T) {
	on := fpConfig(map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}}, nil)
	off := fpConfig(map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second, NoTrace: true}}, nil)
	if Fingerprint(on) == Fingerprint(off) {
		t.Fatal("no_trace edit produced an identical fingerprint, so RunLifecycle keeps the old scheduler")
	}
}

// Slaves and Alerts used to run together on one field separator, so moving a
// name from one list to the other left the bytes unchanged: the target stayed
// filtered out of local probing and the alert attached to it could never fire.
func TestFingerprintSeparatesSlavesFromAlerts(t *testing.T) {
	assigned := oneTarget(config.Target{Name: "a", Probe: "icmp", Slaves: []string{"fra1"}})
	alerted := oneTarget(config.Target{Name: "a", Probe: "icmp", Alerts: []string{"fra1"}})
	if Fingerprint(fpConfig(nil, assigned)) == Fingerprint(fpConfig(nil, alerted)) {
		t.Fatal("slaves=[fra1] and alerts=[fra1] share a fingerprint")
	}

	twoSlaves := oneTarget(config.Target{Name: "a", Probe: "icmp", Slaves: []string{"x", "y"}})
	split := oneTarget(config.Target{Name: "a", Probe: "icmp", Slaves: []string{"x"}, Alerts: []string{"y"}})
	if Fingerprint(fpConfig(nil, twoSlaves)) == Fingerprint(fpConfig(nil, split)) {
		t.Fatal("slaves=[x,y] and slaves=[x]+alerts=[y] share a fingerprint")
	}
}

// Group and target names are bounded only by MaxLabelLen — no character class —
// so a raw separator inside one otherwise reads back as a level break.
func TestFingerprintEscapesSeparatorsInNames(t *testing.T) {
	a := oneTarget(config.Target{Name: "a\x1fb", Probe: "icmp"})
	b := oneTarget(config.Target{Name: "a", Probe: "b"})
	if Fingerprint(fpConfig(nil, a)) == Fingerprint(fpConfig(nil, b)) {
		t.Fatal("a name carrying a raw field separator forged another config's fingerprint")
	}

	// Spelling every separator the encoding uses, in the order a two-group
	// block emits them: the earlier fixtures here differed by a literal ("icmp"
	// against "b") as well as by the escaping, so they stayed green with
	// EscapeSeparators reduced to the identity function and guarded nothing.
	twoGroups := []config.Group{
		{Group: "x", Targets: []config.Target{{Name: "t", Probe: "icmp"}}},
		{Group: "y", Targets: []config.Target{{Name: "u", Probe: "icmp"}}},
	}
	forged := []config.Group{
		{Group: "x\x1ft\x1ficmp\x1f\x1f\x1f\x1f\x1c\x1e\x1dy", Targets: []config.Target{{Name: "u", Probe: "icmp"}}},
	}
	if Fingerprint(fpConfig(nil, twoGroups)) == Fingerprint(fpConfig(nil, forged)) {
		t.Fatal("a group name spelling a whole group block forged a two-group fingerprint")
	}
}

// The master stamps every locally probed cycle with cfg.Cluster.Source, bound
// once in scheduler.New. Omitted from the fingerprint, a SIGHUP editing it
// alone kept the old stamp while the ingest collision guard and /sources read
// the new value: the guard then refuses the name nothing writes and admits the
// name everything writes.
func TestFingerprintSeesClusterSource(t *testing.T) {
	with := func(source string) *config.Config {
		cfg := fpConfig(nil, nil)
		cfg.Cluster = &config.Cluster{Source: source}
		return cfg
	}
	if Fingerprint(with("master-a")) == Fingerprint(with("fra1")) {
		t.Fatal("a cluster.source edit produced an identical fingerprint, so the scheduler keeps stamping the old label")
	}
}
