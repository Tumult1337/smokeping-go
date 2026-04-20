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
	if cfg.Storage.Backend != "influxv2" {
		t.Errorf("storage: %+v", cfg.Storage)
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
		if strings.Contains(n.Detail, "storage.influxv2 is a placeholder") {
			sawStorageNote = true
		}
	}
	if !sawStorageNote {
		t.Errorf("expected storage-placeholder note, got %+v", notes)
	}
}
