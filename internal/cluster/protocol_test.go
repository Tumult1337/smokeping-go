package cluster_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// TestCycleRoundTrip guards the wire protocol: FromCycle → JSON → ToCycle must
// preserve every populated field on scheduler.Cycle. Adding a new field to
// scheduler.Cycle (or probe.Hop / probe.HTTPSample / stats.Summary) without
// mirroring it in the DTOs will regress this test as soon as the author
// populates it here.
func TestCycleRoundTrip(t *testing.T) {
	target := config.Target{
		Name:  "web1",
		Host:  "1.2.3.4",
		URL:   "https://example.com",
		Probe: "icmp",
	}

	summary := stats.Summary{
		Min:    1 * time.Microsecond,
		Max:    2 * time.Microsecond,
		Mean:   3 * time.Microsecond,
		Median: 4 * time.Microsecond,
		StdDev: 5 * time.Microsecond,
	}
	for i, spec := range stats.PercentileSet {
		spec.Set(&summary, time.Duration(100+i)*time.Microsecond)
	}

	original := scheduler.Cycle{
		Time:      time.Unix(1700000000, 123456789).UTC(),
		Target:    config.TargetRef{Group: "prod", Target: target},
		ProbeName: "icmp",
		Source:    "slave-a",
		RTTs:      []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
		Sent:      5,
		LossCount: 2,
		Summary:   summary,
		Hops: []probe.Hop{
			{
				Index: 1,
				IP:    "10.0.0.1",
				RTTs:  []time.Duration{500 * time.Microsecond, 600 * time.Microsecond},
				Sent:  3,
				Lost:  1,
			},
			{
				Index: 2,
				IP:    "10.0.0.2",
				RTTs:  []time.Duration{900 * time.Microsecond},
				Sent:  3,
				Lost:  2,
			},
		},
		HTTPSamples: []probe.HTTPSample{
			{
				Time:   time.Unix(1700000001, 0).UTC(),
				RTT:    45 * time.Millisecond,
				Status: 200,
				Err:    "",
			},
			{
				Time:   time.Unix(1700000002, 0).UTC(),
				RTT:    0,
				Status: 0,
				Err:    "dial tcp: i/o timeout",
			},
		},
	}

	payload := cluster.FromCycle(original)
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded cluster.CyclePayload
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := decoded.ToCycle(target)

	if !reflect.DeepEqual(got, original) {
		t.Errorf("round-trip mismatch:\n got:  %#v\n want: %#v", got, original)
	}

	// Sanity: every percentile landed where expected so the reflect.DeepEqual
	// above is actually exercising them, not silently accepting zero==zero.
	for i, spec := range stats.PercentileSet {
		want := time.Duration(100+i) * time.Microsecond
		if gotVal := spec.Get(got.Summary); gotVal != want {
			t.Errorf("percentile %s: got %v, want %v", spec.Name, gotVal, want)
		}
	}
}

// TestCycleRoundTripEmptySlices checks the nil-hops / nil-http-samples path
// used by non-MTR, non-HTTP cycles. JSON omitempty + absent-key decoding
// round-trips nil → nil, which matters because callers check len() after.
func TestCycleRoundTripEmptySlices(t *testing.T) {
	target := config.Target{Name: "a", Host: "1.1.1.1", Probe: "icmp"}
	original := scheduler.Cycle{
		Time:      time.Unix(1700000000, 0).UTC(),
		Target:    config.TargetRef{Group: "g", Target: target},
		ProbeName: "icmp",
		Source:    "master",
		RTTs:      []time.Duration{time.Millisecond},
		Sent:      1,
	}

	payload := cluster.FromCycle(original)
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded cluster.CyclePayload
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.ToCycle(target)

	if len(got.Hops) != 0 {
		t.Errorf("hops should round-trip empty, got %d", len(got.Hops))
	}
	if len(got.HTTPSamples) != 0 {
		t.Errorf("http samples should round-trip empty, got %d", len(got.HTTPSamples))
	}
}
