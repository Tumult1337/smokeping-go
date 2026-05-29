package smokepingconv

import (
	"strings"
	"testing"
)

func TestConvert_MinimalEndToEnd(t *testing.T) {
	src := `*** Database ***
step = 30
pings = 5

*** Probes ***
+ FPing
timeout = 3

*** Targets ***
probe = FPing

+ europe
++ berlin
host = berlin.example.com
`
	cfg, notes, err := Convert(strings.NewReader(src), "/tmp", "/tmp/x.conf")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if cfg.Pings != 5 {
		t.Errorf("pings: %d", cfg.Pings)
	}
	if cfg.Interval.Seconds() != 30 {
		t.Errorf("interval: %v", cfg.Interval)
	}
	if cfg.Storage.ClickHouse.Addr == "" {
		t.Errorf("expected ClickHouse.Addr to be set, got empty")
	}
	if _, ok := cfg.Actions["log"]; !ok {
		t.Error("log action missing")
	}
	if len(cfg.Targets) == 0 || len(cfg.Targets[0].Targets) == 0 {
		t.Fatalf("targets: %+v", cfg.Targets)
	}
	if cfg.Targets[0].Targets[0].Probe != "fping" {
		t.Errorf("target probe: %q", cfg.Targets[0].Targets[0].Probe)
	}
	var sawStorageNote bool
	for _, n := range notes {
		if strings.Contains(n.Detail, "storage.clickhouse.addr is set to") {
			sawStorageNote = true
		}
	}
	if !sawStorageNote {
		t.Errorf("expected storage-placeholder note, got %+v", notes)
	}
}

// TestConvert_AlertRefsSlugged guards the bug where an alert defined with an
// uppercase/spaced SmokePing name was stored slugged in the alert map but
// referenced verbatim from targets — producing a config that fails
// config.Validate (and was written anyway). The emitted target's Alerts must
// match the alert map keys.
func TestConvert_AlertRefsSlugged(t *testing.T) {
	src := `*** Probes ***
+ FPing

*** Alerts ***
+ LossAlert
type = loss
pattern = >50%,>50%

*** Targets ***
probe = FPing
alerts = LossAlert

+ berlin
host = berlin.example.com
`
	cfg, _, err := Convert(strings.NewReader(src), "/tmp", "/tmp/x.conf")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if _, ok := cfg.Alerts["lossalert"]; !ok {
		t.Fatalf("alert map missing slugged key %q: %+v", "lossalert", cfg.Alerts)
	}
	if len(cfg.Targets) == 0 || len(cfg.Targets[0].Targets) == 0 {
		t.Fatalf("targets: %+v", cfg.Targets)
	}
	got := cfg.Targets[0].Targets[0].Alerts
	if len(got) != 1 || got[0] != "lossalert" {
		t.Errorf("target alerts = %v, want [lossalert]", got)
	}
	// The whole point: the emitted config must validate.
	if err := cfg.Validate(); err != nil {
		t.Errorf("emitted config fails validation: %v", err)
	}
}
