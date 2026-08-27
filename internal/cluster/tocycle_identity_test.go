package cluster

import (
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
)

// Neither group nor name has a character class, so {group:"eu", name:"lon/1"}
// and {group:"eu/lon", name:"1"} flatten to the same id: taking the payload's
// spelling wrote rows under a (target_group, target_id) pair no config holds,
// unaddressable by resolveTarget and a permanent LowCardinality entry per
// spelling. Both halves must come from the master's own resolved TargetRef.
//
// Uncovered until now: reverting ToCycle to Group: p.Group left the whole
// suite green, because every other call site passes a payload group equal to
// the resolved one.
func TestToCycleTakesBothHalvesOfTheIdentityFromTheConfig(t *testing.T) {
	ref := config.TargetRef{Group: "eu", Target: config.Target{Name: "lon/1", Probe: "icmp"}}
	p := CyclePayload{
		// What a slave could claim: the same flattened id, split elsewhere.
		Group: "eu/lon",
		Name:  "1",
		Sent:  5,
	}

	cy := p.ToCycle(ref)
	if cy.Target.Group != ref.Group {
		t.Errorf("Cycle.Target.Group = %q, want %q — the wire spelling reached storage", cy.Target.Group, ref.Group)
	}
	if cy.Target.Target.Name != ref.Target.Name {
		t.Errorf("Cycle.Target.Target.Name = %q, want %q", cy.Target.Target.Name, ref.Target.Name)
	}
	if cy.Target.ID() != "eu/lon/1" {
		t.Fatalf("ID() = %q, want %q", cy.Target.ID(), "eu/lon/1")
	}
}
