package master

import (
	"cmp"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// SlaveInfo is one row in the in-memory registry. Kept deliberately small —
// anything that's expensive to track or that should outlive a master restart
// belongs in a persistent store, not here.
type SlaveInfo struct {
	Name     string    `json:"name"`
	Version  string    `json:"version,omitempty"`
	Addr     string    `json:"addr,omitempty"`
	LastSeen time.Time `json:"last_seen"`

	// Advertise is the validated address peers health-probe this slave at.
	// Zero when the slave opted out, sent an invalid value, lost a duplicate
	// race, or failed its pin. Never serialised to JSON: the registry snapshot
	// feeds debug and UI paths, and the address must not reach either.
	Advertise netip.Addr `json:"-"`
}

// Registry tracks slaves that have recently checked in. Lookups and writes
// are concurrent-safe. Cleared on master restart — the slave's next /register
// call re-establishes presence.
type Registry struct {
	log *slog.Logger

	mu     sync.RWMutex
	slaves map[string]*SlaveInfo
	// byAddr enforces one claimant per advertise address. Without it, N
	// bridge-networked containers all claiming 172.17.0.2 would collapse the
	// mesh onto one bogus destination.
	byAddr map[netip.Addr]string
	pins   map[string]netip.Addr

	onChange func()
}

func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{
		log:    log,
		slaves: make(map[string]*SlaveInfo),
		byAddr: make(map[netip.Addr]string),
	}
}

// SetOnChange registers a callback fired whenever the health-relevant set —
// the (name, advertise) tuples — changes. Not fired for version or last-seen
// updates: Touch runs every few seconds per slave, and rebuilding the
// scheduler on each would be continuous churn. Called with the registry lock
// released so the callback may re-enter.
func (r *Registry) SetOnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// SetPins installs the optional name→address allowlist. A pinned slave that
// claims any other address is refused a health entry.
func (r *Registry) SetPins(pins map[string]netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pins = pins
}

// Touch records that a slave just checked in. Safe to call on every request
// that carries a valid slave identity, not just /register. advertise is the
// raw slave-reported health address; an empty or rejected value simply leaves
// the slave out of the health mesh without blocking registration.
func (r *Registry) Touch(name, version, addr, advertise string) {
	if name == "" {
		return
	}

	r.mu.Lock()
	info, ok := r.slaves[name]
	if !ok {
		info = &SlaveInfo{Name: name}
		r.slaves[name] = info
	}
	info.Version = version
	info.Addr = addr
	info.LastSeen = time.Now()

	prev := info.Advertise
	next := r.resolveAdvertise(name, advertise, addr)
	if next != prev {
		if prev.IsValid() {
			delete(r.byAddr, prev)
		}
		if next.IsValid() {
			r.byAddr[next] = name
		}
		info.Advertise = next
	}
	changed := next != prev
	onChange := r.onChange
	r.mu.Unlock()

	if changed && onChange != nil {
		onChange()
	}
}

// resolveAdvertise validates a claimed address against the pin list and the
// current address ownership map. Must be called with r.mu held.
//
// Every rejection path returns the zero Addr, which means "no health entry" —
// the fail-closed outcome. A slave is never dropped from the registry for a
// bad advertise value; it just doesn't join the mesh.
func (r *Registry) resolveAdvertise(name, advertise, remoteAddr string) netip.Addr {
	if advertise == "" {
		return netip.Addr{}
	}
	addr, err := ParseAdvertise(advertise)
	if err != nil {
		r.log.Warn("slave advertise address rejected", "slave", name, "err", err)
		return netip.Addr{}
	}
	if pin, pinned := r.pins[name]; pinned && pin != addr {
		r.log.Warn("slave advertise address does not match its pin, excluded from health mesh",
			"slave", name, "claimed", addr, "pinned", pin)
		return netip.Addr{}
	}
	if owner, taken := r.byAddr[addr]; taken && owner != name {
		r.log.Warn("slave advertise address already claimed, excluded from health mesh",
			"slave", name, "addr", addr, "owner", owner,
			"hint", "containers on a bridge network all report the same internal address; set cluster.advertise to the host's reachable IP")
		return netip.Addr{}
	}
	// A mismatch against the observed source address is legitimate under NAT,
	// so it cannot be an error — but it is the signature of a container
	// reporting its internal address, so it is worth a line in the log.
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		if observed, perr := netip.ParseAddr(host); perr == nil && observed.Unmap() != addr {
			r.log.Info("slave advertise address differs from its observed source address",
				"slave", name, "advertise", addr, "observed", observed.Unmap(),
				"hint", "expected under NAT; unexpected otherwise — check cluster.advertise")
		}
	}
	return addr
}

// Peers returns the slaves eligible for health probing, sorted by name. The
// sort is load-bearing: the scheduler fingerprint is derived from this list,
// and map iteration order would otherwise force a rebuild on every signal.
func (r *Registry) Peers() []slavehealth.Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]slavehealth.Peer, 0, len(r.slaves))
	for _, info := range r.slaves {
		if !info.Advertise.IsValid() {
			continue
		}
		out = append(out, slavehealth.Peer{Name: info.Name, Addr: info.Advertise})
	}
	slices.SortFunc(out, func(a, b slavehealth.Peer) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// Names returns the current set of registered slave names, sorted for stable
// UI rendering.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.slaves))
	for name := range r.slaves {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Has reports whether a slave has registered at least once since startup.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.slaves[name]
	return ok
}

// Sweep removes slaves whose LastSeen is older than `age`. Call periodically
// to prevent unbounded growth from ephemeral or renamed slaves.
func (r *Registry) Sweep(age time.Duration) {
	cutoff := time.Now().Add(-age)
	r.mu.Lock()
	changed := false
	for name, info := range r.slaves {
		if !info.LastSeen.Before(cutoff) {
			continue
		}
		// Release the address only if this slave still owns it, so a racing
		// re-registration under the same address isn't stolen by the eviction.
		if info.Advertise.IsValid() && r.byAddr[info.Advertise] == name {
			delete(r.byAddr, info.Advertise)
			changed = true
		}
		delete(r.slaves, name)
	}
	onChange := r.onChange
	r.mu.Unlock()

	if changed && onChange != nil {
		onChange()
	}
}

// Snapshot returns a copy of the current registry for debugging / UI.
func (r *Registry) Snapshot() []SlaveInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SlaveInfo, 0, len(r.slaves))
	for _, v := range r.slaves {
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b SlaveInfo) int { return cmp.Compare(a.Name, b.Name) })
	return out
}
