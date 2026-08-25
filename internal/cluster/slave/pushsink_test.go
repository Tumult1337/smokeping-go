package slave

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

func cycleWithHops(group, name string) scheduler.Cycle {
	return scheduler.Cycle{
		Time:      time.Now(),
		Target:    config.TargetRef{Group: group, Target: config.Target{Name: name, Probe: "icmp"}},
		ProbeName: "icmp",
		Sent:      3,
		Hops: []probe.Hop{
			{Index: 1, IP: "198.51.100.1", Sent: 3},
			{Index: 2, IP: "10.44.0.2", Sent: 3, TargetReply: true},
		},
	}
}

func decodeConfigResp(t *testing.T, raw string) cluster.ClusterConfigResp {
	t.Helper()
	var resp cluster.ClusterConfigResp
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// A master predating the marker blanks only the deepest hop row, so a walk
// that echoed at ttl 2 below a silent ttl 30 would have it serve the slave's
// own address on /hops. The advertisement is what the slave keys on, and its
// absence — an old master's response — must withhold health hops without
// touching ordinary targets.
func TestPushSinkWithholdsHealthHopsFromMarkerBlindMaster(t *testing.T) {
	markerBlind := decodeConfigResp(t, `{"interval":60000000000,"pings":20}`)
	markerAware := decodeConfigResp(t, `{"interval":60000000000,"pings":20,"hop_markers":true}`)
	if markerBlind.HopMarkers {
		t.Fatal("a response without hop_markers must read as no support")
	}
	if !markerAware.HopMarkers {
		t.Fatal("the advertised response must read as supported")
	}

	for _, tc := range []struct {
		name       string
		resp       cluster.ClusterConfigResp
		wantHealth int
	}{
		{"marker-blind master", markerBlind, 0},
		{"marker-aware master", markerAware, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := NewPushSink(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
			sink.SetHopMarkers(tc.resp.HopMarkers)
			sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))
			sink.OnCycle(context.Background(), cycleWithHops("core", "gw"))

			batch := sink.Drain(10)
			if len(batch) != 2 {
				t.Fatalf("drained %d payloads, want 2", len(batch))
			}
			if got := len(batch[0].Hops); got != tc.wantHealth {
				t.Fatalf("health target shipped %d hop rows, want %d: %+v", got, tc.wantHealth, batch[0].Hops)
			}
			if got := len(batch[1].Hops); got != 2 {
				t.Fatalf("ordinary target lost its hops: %+v", batch[1].Hops)
			}
		})
	}
}

// The slave holds the fail-closed state until its first /config pull answers.
func TestPushSinkDefaultsToWithholdingHealthHops(t *testing.T) {
	sink := NewPushSink(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))

	batch := sink.Drain(10)
	if len(batch) != 1 || len(batch[0].Hops) != 0 {
		t.Fatalf("health hops shipped before any advertisement: %+v", batch)
	}
}

// The advertisement has to travel from the pulled config to the sink: without
// this the wire field and the guard are both correct and never joined.
func TestRunnerAppliesHopMarkerAdvertisement(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(log, &config.Config{Cluster: &config.Cluster{Name: "s1", MasterURL: "http://master.invalid", Token: "t"}}, "test")

	r.applyConfig(decodeConfigResp(t, `{"interval":60000000000,"pings":20}`))
	r.sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))
	if batch := r.sink.Drain(10); len(batch) != 1 || len(batch[0].Hops) != 0 {
		t.Fatalf("marker-blind advertisement not applied: %+v", batch)
	}

	r.applyConfig(decodeConfigResp(t, `{"interval":60000000000,"pings":20,"hop_markers":true}`))
	r.sink.OnCycle(context.Background(), cycleWithHops(slavehealth.Group, "frankfurt-1"))
	if batch := r.sink.Drain(10); len(batch) != 1 || len(batch[0].Hops) != 2 {
		t.Fatalf("marker-aware advertisement not applied: %+v", batch)
	}
}
