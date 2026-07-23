package config

import (
	"encoding/json"
	"testing"
)

func TestQuorumUnmarshal(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		majority bool
		min      int
		wantErr  bool
	}{
		{name: "majority", raw: `"majority"`, majority: true},
		{name: "absolute", raw: `3`, min: 3},
		{name: "one", raw: `1`, min: 1},
		{name: "null is unset", raw: `null`},
		{name: "zero rejected", raw: `0`, wantErr: true},
		{name: "negative rejected", raw: `-1`, wantErr: true},
		{name: "unknown string rejected", raw: `"most"`, wantErr: true},
		{name: "float rejected", raw: `2.5`, wantErr: true},
		{name: "object rejected", raw: `{"min":2}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var q Quorum
			err := json.Unmarshal([]byte(tc.raw), &q)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = nil error, want failure", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.raw, err)
			}
			if q.Majority != tc.majority || q.Min != tc.min {
				t.Fatalf("got {Majority:%v Min:%d}, want {Majority:%v Min:%d}",
					q.Majority, q.Min, tc.majority, tc.min)
			}
		})
	}
}

// Strict majority: more than half. 1-of-2 is not a majority.
func TestQuorumThresholdMajority(t *testing.T) {
	q := Quorum{Majority: true}
	for _, tc := range []struct{ live, want int }{
		{1, 1}, {2, 2}, {3, 2}, {4, 3}, {5, 3}, {6, 4},
	} {
		if got := q.Threshold(tc.live); got != tc.want {
			t.Fatalf("Threshold(%d) = %d, want %d", tc.live, got, tc.want)
		}
	}
}

func TestQuorumThresholdAbsolute(t *testing.T) {
	q := Quorum{Min: 3}
	for _, live := range []int{1, 3, 10} {
		if got := q.Threshold(live); got != 3 {
			t.Fatalf("Threshold(%d) = %d, want 3 regardless of live count", live, got)
		}
	}
}

func TestQuorumEnabled(t *testing.T) {
	if (Quorum{}).Enabled() {
		t.Fatal("zero Quorum must be disabled")
	}
	if !(Quorum{Majority: true}).Enabled() {
		t.Fatal("majority Quorum must be enabled")
	}
	if !(Quorum{Min: 2}).Enabled() {
		t.Fatal("absolute Quorum must be enabled")
	}
}

func TestQuorumRoundTrips(t *testing.T) {
	for _, q := range []Quorum{{}, {Majority: true}, {Min: 4}} {
		b, err := json.Marshal(q)
		if err != nil {
			t.Fatalf("Marshal(%+v): %v", q, err)
		}
		var back Quorum
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if back != q {
			t.Fatalf("round trip: got %+v, want %+v", back, q)
		}
	}
}
