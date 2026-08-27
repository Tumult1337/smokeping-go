package config

import (
	"strings"
	"testing"
)

// probe.build passes NoTrace only to NewICMP and Insecure only to NewHTTP,
// while scheduler.Fingerprint hashes both — so `no_trace: true` on an mtr probe
// validated, changed the fingerprint, genuinely rebuilt the scheduler, shipped
// to every slave, and left the walk filling probe_hop exactly as before. A knob
// with no read site is worse than a missing one: the operator sets it and
// believes the surface is bounded.
func TestProbeKnobsAreRefusedWhereNothingReadsThem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		probe  Probe
		reject string
	}{
		{"no_trace on mtr", Probe{Type: "mtr", NoTrace: true}, "no_trace"},
		{"no_trace on tcp", Probe{Type: "tcp", NoTrace: true}, "no_trace"},
		{"no_trace on http", Probe{Type: "http", NoTrace: true}, "no_trace"},
		{"insecure on dns", Probe{Type: "dns", Insecure: true}, "insecure"},
		{"insecure on icmp", Probe{Type: "icmp", Insecure: true}, "insecure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeTmp(t, minimalConfig))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Probes["extra"] = tc.probe
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s accepted: it is hashed into the rebuild and shipped to every slave, then dropped", tc.name)
			} else if !strings.Contains(err.Error(), tc.reject) {
				t.Errorf("error %q does not name the field the operator has to remove", err)
			}
		})
	}
}

// The refusal must not reach the type that reads the knob — config.example.json
// ships both, and slavehealth.ProbeDef injects an icmp probe with NoTrace set
// from cluster.health_hops.
func TestProbeKnobsStayAcceptedWhereTheyAreRead(t *testing.T) {
	cfg, err := Load(writeTmp(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Probes["icmp-no-trace"] = Probe{Type: "icmp", NoTrace: true}
	cfg.Probes["http-insecure"] = Probe{Type: "http", Insecure: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("refused a knob on the probe type that reads it: %v", err)
	}
}
