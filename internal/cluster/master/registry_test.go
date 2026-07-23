package master

import (
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
)

func TestRegistryStoresAdvertise(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("got %d slaves, want 1", len(snap))
	}
	want := netip.MustParseAddr("10.44.0.2")
	if snap[0].Advertise != want {
		t.Fatalf("got advertise %v, want %v", snap[0].Advertise, want)
	}
}

// An invalid advertise value must not register an address, but must not stop
// the slave from registering — it still pushes cycles and appears in /sources.
func TestRegistryRejectsInvalidAdvertise(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.Touch("bad", "v1", "203.0.113.9:5555", "127.0.0.1")

	if !r.Has("bad") {
		t.Fatal("slave with an invalid advertise address must still register")
	}
	if got := r.Peers(); len(got) != 0 {
		t.Fatalf("got %d health peers, want 0", len(got))
	}
}

// The all-containers-are-172.17.0.2 case: the second claimant is excluded from
// the mesh, and must not steal the address from the first.
func TestRegistryRejectsDuplicateAdvertise(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.Touch("first", "v1", "203.0.113.9:5555", "172.17.0.2")
	r.Touch("second", "v1", "203.0.113.10:5555", "172.17.0.2")

	peers := r.Peers()
	if len(peers) != 1 {
		t.Fatalf("got %d health peers, want 1", len(peers))
	}
	if peers[0].Name != "first" {
		t.Fatalf("got peer %q, want the first claimant to keep the address", peers[0].Name)
	}
	if !r.Has("second") {
		t.Fatal("duplicate claimant must still register as a slave")
	}
}

// Re-registering with the address it already holds is not a duplicate.
func TestRegistryReclaimOwnAddress(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5556", "10.44.0.2")

	if got := r.Peers(); len(got) != 1 {
		t.Fatalf("got %d health peers, want 1", len(got))
	}
}

func TestRegistryPinRejectsMismatch(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.SetPins(map[string]netip.Addr{"frankfurt-1": netip.MustParseAddr("10.44.0.2")})

	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.99")
	if got := r.Peers(); len(got) != 0 {
		t.Fatalf("got %d health peers, want 0 (pin mismatch must be rejected)", len(got))
	}

	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	if got := r.Peers(); len(got) != 1 {
		t.Fatalf("got %d health peers, want 1 (pin match must be accepted)", len(got))
	}
}

func TestRegistryPinIgnoresUnpinnedSlaves(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.SetPins(map[string]netip.Addr{"frankfurt-1": netip.MustParseAddr("10.44.0.2")})

	r.Touch("tokyo-1", "v1", "203.0.113.20:5555", "10.44.0.7")
	if got := r.Peers(); len(got) != 1 {
		t.Fatalf("got %d health peers, want 1 (unpinned slaves are accepted)", len(got))
	}
}

// Touch runs every 5s per slave on push. OnChange must fire only when the
// health-relevant tuple changes, or the scheduler would rebuild continuously.
func TestRegistryOnChangeFiresOnlyOnHealthChange(t *testing.T) {
	var fired atomic.Int64
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.SetOnChange(func() { fired.Add(1) })

	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	if got := fired.Load(); got != 1 {
		t.Fatalf("after first register: got %d fires, want 1", got)
	}

	// Heartbeats with an identical tuple.
	for i := 0; i < 5; i++ {
		r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	}
	if got := fired.Load(); got != 1 {
		t.Fatalf("after 5 identical heartbeats: got %d fires, want 1", got)
	}

	// A version bump is not health-relevant.
	r.Touch("frankfurt-1", "v2", "203.0.113.9:5555", "10.44.0.2")
	if got := fired.Load(); got != 1 {
		t.Fatalf("after version bump: got %d fires, want 1", got)
	}

	// A changed address is health-relevant.
	r.Touch("frankfurt-1", "v2", "203.0.113.9:5555", "10.44.0.3")
	if got := fired.Load(); got != 2 {
		t.Fatalf("after address change: got %d fires, want 2", got)
	}

	// A new peer is health-relevant.
	r.Touch("tokyo-1", "v1", "203.0.113.20:5555", "10.44.0.7")
	if got := fired.Load(); got != 3 {
		t.Fatalf("after new peer: got %d fires, want 3", got)
	}
}

func TestRegistrySweepFiresOnChange(t *testing.T) {
	var fired atomic.Int64
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	r.SetOnChange(func() { fired.Add(1) })

	r.Sweep(0) // everything is older than a zero-age cutoff
	if got := fired.Load(); got != 1 {
		t.Fatalf("got %d fires, want 1 (eviction removes a health peer)", got)
	}
	if got := r.Peers(); len(got) != 0 {
		t.Fatalf("got %d health peers after sweep, want 0", len(got))
	}
}

// Peers must be sorted so the scheduler fingerprint is stable across map
// iteration order — otherwise the scheduler rebuilds on every signal.
func TestRegistryPeersSorted(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	r.Touch("tokyo-1", "v1", "203.0.113.20:5555", "10.44.0.7")
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	r.Touch("ashburn-1", "v1", "203.0.113.30:5555", "10.44.0.9")

	peers := r.Peers()
	if len(peers) != 3 {
		t.Fatalf("got %d peers, want 3", len(peers))
	}
	for i := 1; i < len(peers); i++ {
		if peers[i-1].Name >= peers[i].Name {
			t.Fatalf("peers not sorted by name: %v", peers)
		}
	}
}
