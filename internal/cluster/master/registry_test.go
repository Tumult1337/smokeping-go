package master

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// countLogLines returns the number of non-empty lines written to buf — one
// per slog record with the standard text handler.
func countLogLines(buf *bytes.Buffer) int {
	n := 0
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			n++
		}
	}
	return n
}

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
	for range 5 {
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

// Touch runs on every authenticated request — roughly every 5s per slave,
// forever. A repeated identical rejection must log once, not once per Touch.
func TestRegistryRepeatedRejectionLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	r := NewRegistry(slog.New(slog.NewTextHandler(&buf, nil)))

	// First call: a rejection must still log (this is the one that matters).
	r.Touch("second", "v1", "203.0.113.10:5555", "not-an-ip")
	if got := countLogLines(&buf); got != 1 {
		t.Fatalf("after first rejection: got %d log lines, want 1", got)
	}

	// Repeated identical rejection must stay quiet.
	for range 5 {
		r.Touch("second", "v1", "203.0.113.10:5555", "not-an-ip")
	}
	if got := countLogLines(&buf); got != 1 {
		t.Fatalf("after 5 identical repeated rejections: got %d log lines, want 1 (still)", got)
	}
}

// A rejection changing to a *different* bad value must log again. This is
// the case the naive fix (gating on info.Advertise != prev) gets wrong: the
// resolved Addr is the zero value for every rejection, so next == prev holds
// across different bad claims too — that would wrongly stay silent here.
func TestRegistryChangedRejectionLogsAgain(t *testing.T) {
	var buf bytes.Buffer
	r := NewRegistry(slog.New(slog.NewTextHandler(&buf, nil)))

	r.Touch("second", "v1", "203.0.113.10:5555", "not-an-ip")
	if got := countLogLines(&buf); got != 1 {
		t.Fatalf("after first rejection: got %d log lines, want 1", got)
	}

	// Same bad value again: still quiet.
	r.Touch("second", "v1", "203.0.113.10:5555", "not-an-ip")
	if got := countLogLines(&buf); got != 1 {
		t.Fatalf("after repeat of same bad value: got %d log lines, want 1", got)
	}

	// A different bad value is a new condition and must log again.
	r.Touch("second", "v1", "203.0.113.10:5555", "also-not-an-ip")
	if got := countLogLines(&buf); got != 2 {
		t.Fatalf("after a different bad value: got %d log lines, want 2", got)
	}
}

// A slave transitioning from rejected to accepted (a change in outcome, not
// just in claimed value) must log again — proven here via the observed-
// source-mismatch line, which only fires once the address is actually
// accepted. That line is Debug (see resolveAdvertise), so the handler must
// opt into Debug to observe it here; the gating behavior under test is
// unaffected by level.
func TestRegistryRejectedToAcceptedLogsAgain(t *testing.T) {
	var buf bytes.Buffer
	r := NewRegistry(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Rejected: invalid value.
	r.Touch("nat-slave", "v1", "203.0.113.9:5555", "not-an-ip")
	if got := countLogLines(&buf); got != 1 {
		t.Fatalf("after rejection: got %d log lines, want 1", got)
	}

	// Accepted, but the claimed address differs from the observed source —
	// expected under NAT, and worth its own Debug line on first occurrence.
	r.Touch("nat-slave", "v1", "203.0.113.9:5555", "10.44.0.2")
	if got := countLogLines(&buf); got != 2 {
		t.Fatalf("after transition to accepted (observed-address mismatch): got %d log lines, want 2", got)
	}
	if got := r.Peers(); len(got) != 1 {
		t.Fatalf("got %d health peers after acceptance, want 1", len(got))
	}

	// Repeated identical mismatch must stay quiet.
	for range 5 {
		r.Touch("nat-slave", "v1", "203.0.113.9:5555", "10.44.0.2")
	}
	if got := countLogLines(&buf); got != 2 {
		t.Fatalf("after 5 identical mismatch heartbeats: got %d log lines, want 2 (still)", got)
	}
}

// The observed-address mismatch line is Debug, not Info: behind a reverse
// proxy or CDN, remoteAddr is the proxy's address for every slave, so at the
// default level this line must not appear even on first occurrence — a
// regression back to Info would spam one line per slave per proxied fleet.
func TestRegistryObservedMismatchIsQuietAtDefaultLevel(t *testing.T) {
	var buf bytes.Buffer
	r := NewRegistry(slog.New(slog.NewTextHandler(&buf, nil)))

	r.Touch("nat-slave", "v1", "203.0.113.9:5555", "10.44.0.2")
	if got := countLogLines(&buf); got != 0 {
		t.Fatalf("got %d log lines at default level, want 0 (mismatch line must be Debug)", got)
	}
	if got := r.Peers(); len(got) != 1 {
		t.Fatalf("got %d health peers, want 1 (mismatch is not a rejection)", len(got))
	}
}

// The motivating scenario: several bridge-networked containers all claiming
// the same address. The losing claimant's rejection must log once, not on
// every push cycle for the lifetime of the process. The handler runs at
// Debug so the first claimant's observed-address-mismatch line is visible
// too, keeping the line count exercising both log lines' gating.
func TestRegistryDuplicateRejectionLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	r := NewRegistry(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// The claimed address differs from the observed source (both slaves are
	// bridge-networked containers reporting their internal IP), so the first
	// claimant's own registration logs the expected mismatch Debug line.
	r.Touch("first", "v1", "203.0.113.9:5555", "172.17.0.2")
	if got := countLogLines(&buf); got != 1 {
		t.Fatalf("after first claimant: got %d log lines, want 1 (observed-address mismatch)", got)
	}

	r.Touch("second", "v1", "203.0.113.10:5555", "172.17.0.2")
	if got := countLogLines(&buf); got != 2 {
		t.Fatalf("after second (losing) claimant: got %d log lines, want 2", got)
	}

	for range 5 {
		r.Touch("second", "v1", "203.0.113.10:5555", "172.17.0.2")
	}
	if got := countLogLines(&buf); got != 2 {
		t.Fatalf("after 5 repeated duplicate heartbeats: got %d log lines, want 2 (still)", got)
	}
}

// Version and advertise are free strings off the wire — the header form is
// bounded only by net/http's 1 MiB header cap — and the registry keeps both
// per slave (advertise inside the log-dedup key even when ParseAdvertise
// rejects it). Refusing the entry keeps the registry's footprint a function
// of the fleet size, not of what a token holder chooses to send.
func TestRegistryRefusesOversizedFields(t *testing.T) {
	reg := NewRegistry(slog.New(slog.DiscardHandler))
	big := strings.Repeat("v", maxSlaveFieldLen+1)

	if err := reg.Touch("edge-1", big, "10.0.0.5:5000", ""); !errors.Is(err, errSlaveFieldTooLong) {
		t.Errorf("oversized version: err = %v, want errSlaveFieldTooLong", err)
	}
	if err := reg.Touch("edge-2", "", "10.0.0.5:5000", big); !errors.Is(err, errSlaveFieldTooLong) {
		t.Errorf("oversized advertise: err = %v, want errSlaveFieldTooLong", err)
	}
	if reg.Has("edge-1") || reg.Has("edge-2") {
		t.Error("refused slave stored anyway")
	}
	if err := reg.Touch("edge-3", strings.Repeat("v", maxSlaveFieldLen), "10.0.0.5:5000", "10.44.0.2"); err != nil {
		t.Errorf("a version at the limit was refused: %v", err)
	}
}

// The pin list follows the config hot-reload contract: consulted live on
// every read, never cached at wiring time. A pin added by SIGHUP must leave
// the mesh on the next Peers() read — the scheduler rebuild path — not once
// the pinned slave happens to heartbeat again.
func TestPinsFollowTheLiveConfigNotACachedCopy(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	pins := map[string]netip.Addr{}
	r.SetPinsFn(func() map[string]netip.Addr { return pins })

	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	if got := len(r.Peers()); got != 1 {
		t.Fatalf("unpinned slave: peers = %d, want 1", got)
	}

	pins = map[string]netip.Addr{"frankfurt-1": netip.MustParseAddr("10.44.0.99")}
	if got := len(r.Peers()); got != 0 {
		t.Fatalf("reloaded pin not applied until the slave's next heartbeat: peers = %d, want 0", got)
	}

	pins = map[string]netip.Addr{}
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	if got := len(r.Peers()); got != 1 {
		t.Fatalf("removed pin: peers = %d, want the slave re-admitted (1)", got)
	}
}

// The same handover with the old owner silent. byAddr only ever updates when
// a slave heartbeats, so a reload swapping two pins left the address owned by
// a slave the live pins no longer admit, and the new rightful owner was
// refused a health entry until a Sweep removed the old one — up to a day.
func TestReloadedPinFreesTheAddressFromASilentOwner(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	pins := map[string]netip.Addr{}
	r.SetPinsFn(func() map[string]netip.Addr { return pins })

	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")

	// The operator re-pins frankfurt-1 elsewhere and gives its old address to
	// tokyo-1. frankfurt-1 never heartbeats again.
	pins = map[string]netip.Addr{
		"frankfurt-1": netip.MustParseAddr("10.44.0.3"),
		"tokyo-1":     netip.MustParseAddr("10.44.0.2"),
	}
	r.Touch("tokyo-1", "v1", "203.0.113.20:5555", "10.44.0.2")

	peers := r.Peers()
	if len(peers) != 1 || peers[0].Name != "tokyo-1" {
		t.Fatalf("peers = %+v, want tokyo-1 holding the address its pin names", peers)
	}
	if peers[0].Addr != netip.MustParseAddr("10.44.0.2") {
		t.Fatalf("tokyo-1 addr = %s, want 10.44.0.2", peers[0].Addr)
	}
}

// Touch-time resolution reads the same live pins, and a heartbeat under a
// reloaded mismatching pin releases the address ownership so another slave
// can claim it — a cached pin map would leave the old owner holding it.
func TestReloadedPinFreesTheAddressAtNextHeartbeat(t *testing.T) {
	r := NewRegistry(slog.New(slog.DiscardHandler))
	pins := map[string]netip.Addr{}
	r.SetPinsFn(func() map[string]netip.Addr { return pins })

	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	pins = map[string]netip.Addr{"frankfurt-1": netip.MustParseAddr("10.44.0.99")}
	r.Touch("frankfurt-1", "v1", "203.0.113.9:5555", "10.44.0.2")
	r.Touch("tokyo-1", "v1", "203.0.113.20:5555", "10.44.0.2")

	peers := r.Peers()
	if len(peers) != 1 || peers[0].Name != "tokyo-1" {
		t.Fatalf("peers = %v, want only tokyo-1 holding the freed address", peers)
	}
}
