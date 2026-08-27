package master

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// SlaveInfo is one row in the in-memory registry. Kept deliberately small —
// anything that's expensive to track or that should outlive a master restart
// belongs in a persistent store, not here.
type SlaveInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Addr is the observed source address, json:"-" for the reason Advertise is:
	// Snapshot copies it by value into a slice whose own doc invites a debug or
	// UI consumer, and the Probe/Public split exists so no handler can publish a
	// peer's address.
	Addr     string    `json:"-"`
	LastSeen time.Time `json:"last_seen"`

	// Advertise is the validated address peers health-probe this slave at.
	// Zero when the slave opted out, sent an invalid value, lost a duplicate
	// race, or failed its pin. Never serialised to JSON: the registry snapshot
	// feeds debug and UI paths, and the address must not reach either.
	Advertise netip.Addr `json:"-"`

	// WarnedLongField dedups the over-length header warning. It cannot share
	// AdvertiseLogState: resolveAdvertise still runs when only the version is
	// too long and overwrites that slot with its own outcome, so the dedup
	// never matched and one misconfigured slave logged on every request.
	WarnedLongField bool `json:"-"`

	// AdvertiseLogState dedups resolveAdvertise's log lines (outcome kind +
	// raw claimed value): Touch fires on every authenticated request, so a
	// rejected or NAT'd slave would otherwise re-log each heartbeat. Never
	// serialised, like Advertise.
	AdvertiseLogState string `json:"-"`
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
	// pinsFn is the live source of the name→address allowlist, consulted on
	// every resolution rather than cached so a SIGHUP-edited
	// cluster.slave_addrs re-pins without waiting for a restart. It runs
	// under r.mu and must not re-enter the registry.
	pinsFn func() map[string]netip.Addr
	// fullWarned dedups the registry-full warning. A refused name is refused
	// on every retry, and the retry rate is the attacker's to choose.
	fullWarned bool

	onChange func()
	onRemove func(name string)
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

// SetOnRemove registers a callback fired once per name Sweep drops, so state
// keyed on a slave name elsewhere in the master is released with the entry
// that authorised it rather than outliving it. Called with the registry lock
// released, like SetOnChange's.
func (r *Registry) SetOnRemove(fn func(name string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRemove = fn
}

// SetPins installs a fixed name→address allowlist. A pinned slave that
// claims any other address is refused a health entry.
func (r *Registry) SetPins(pins map[string]netip.Addr) {
	r.SetPinsFn(func() map[string]netip.Addr { return pins })
}

// SetPinsFn installs the live source of the pin allowlist; see Registry.pinsFn.
func (r *Registry) SetPinsFn(fn func() map[string]netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pinsFn = fn
}

// currentPins must be called with r.mu held (read or write).
func (r *Registry) currentPins() map[string]netip.Addr {
	if r.pinsFn == nil {
		return nil
	}
	return r.pinsFn()
}

// maxRegisteredSlaves bounds distinct live names per master process: the
// registry is the list of legal source labels, and every minted label is a
// permanent ClickHouse LowCardinality entry. Refusing a *new* name at the
// ceiling never evicts a registered one.
const maxRegisteredSlaves = 512

// maxSlaveFieldLen bounds the header strings the registry retains per entry;
// the longest legal advertise is a 45-byte IPv6 text form and a version is a
// release tag, so 256 is ~5x either.
const maxSlaveFieldLen = config.MaxSlaveFieldLen

// The two refusals stay distinct errors because they need distinct remedies:
// errRegistryFull is capacity (503, retryable once Sweep frees a name),
// errSlaveFieldTooLong is this request's own bytes (400, never succeeds
// resent).
var (
	errEmptySlaveName    = errors.New("empty slave name")
	errSlaveFieldTooLong = fmt.Errorf("version or advertise exceeds %d bytes", maxSlaveFieldLen)
	errRegistryFull      = errors.New("slave registry full")
)

// Touch records that a slave just checked in; a non-nil error names why the
// registry refused the entry. Safe to call on every request that carries a
// valid slave identity, not just /register. advertise is the raw
// slave-reported health address; an empty or rejected value simply leaves the
// slave out of the health mesh without blocking registration.
func (r *Registry) Touch(name, version, addr, advertise string) error {
	if name == "" {
		return errEmptySlaveName
	}
	// Per field, not their union: an over-length X-Slave-Version says nothing
	// about the advertise beside it, and skipping resolveAdvertise for both
	// froze that slave's health-mesh address forever — an operator changing
	// cluster.advertise, or adding a pin to evict a squatter, never took
	// effect, while the branch kept LastSeen fresh so Sweep never reclaimed
	// it either.
	longVersion := len(version) > maxSlaveFieldLen
	longAdvertise := len(advertise) > maxSlaveFieldLen

	r.mu.Lock()
	info, ok := r.slaves[name]
	if !ok {
		// A name this registry has never seen must not be created from a
		// malformed request: the entry is what makes the name a legal ingest
		// label, and every one costs a permanent LowCardinality dictionary
		// entry.
		if longVersion || longAdvertise {
			r.mu.Unlock()
			return errSlaveFieldTooLong
		}
		if len(r.slaves) >= maxRegisteredSlaves {
			warn := !r.fullWarned
			r.fullWarned = true
			r.mu.Unlock()
			if warn {
				r.log.Warn("slave registry full, refusing new names",
					"registered", maxRegisteredSlaves)
			}
			return errRegistryFull
		}
		info = &SlaveInfo{Name: name}
		r.slaves[name] = info
	}
	// LastSeen advances whatever the headers carried: the slave is registered
	// and its cycles are being ingested, so returning before this let Sweep
	// drop it after 24h — taking its dedup window with it — and its next push
	// then 403s. Only the offending value is refused, and neither is retained.
	info.Addr = addr
	info.LastSeen = time.Now()
	if !longVersion {
		info.Version = version
	}

	prev := info.Advertise
	// Fail closed, like every other rejection in resolveAdvertise: an
	// over-length advertise yields no health entry rather than retaining the
	// last good one. Keeping prev meant an operator who fixed cluster.advertise
	// on that node kept a mesh probing the old address and reporting a healthy
	// peer there for the master's whole life, and LastSeen still advanced so
	// Sweep never reclaimed it.
	next := netip.Addr{}
	if !longAdvertise {
		next = r.resolveAdvertise(info, advertise, addr)
	}
	if next != prev {
		// Release only an address this slave still owns, the identity match
		// Sweep already makes. Defence in depth rather than load-bearing:
		// the release above clears the excluded owner's Advertise, so prev
		// is invalid by the time it heartbeats — but an unconditional delete
		// here is one edit away from handing a live claimant's address back
		// to nobody, and only this guard would catch it.
		if prev.IsValid() && r.byAddr[prev] == name {
			delete(r.byAddr, prev)
		}
		if next.IsValid() {
			r.byAddr[next] = name
		}
		info.Advertise = next
	}
	changed := next != prev
	onChange := r.onChange
	warnLong := (longVersion || longAdvertise) && !info.WarnedLongField
	info.WarnedLongField = longVersion || longAdvertise
	r.mu.Unlock()

	if changed && onChange != nil {
		onChange()
	}
	if longVersion || longAdvertise {
		if warnLong {
			r.log.Warn("slave header field past its limit, ignored",
				"slave", name, "field", longFieldName(longVersion, longAdvertise),
				"limit", maxSlaveFieldLen)
		}
		return errSlaveFieldTooLong
	}
	return nil
}

func longFieldName(version, advertise bool) string {
	switch {
	case version && advertise:
		return "version+advertise"
	case version:
		return "version"
	default:
		return "advertise"
	}
}

// Outcome kinds for resolveAdvertise's log dedup key; the key compares
// (kind, raw claim) rather than the resolved Addr because a rejected claim
// resolves to the zero Addr on every call, which would suppress its first log.
const (
	advLogNone    = ""        // advertise empty; nothing to validate or log
	advLogInvalid = "invalid" // failed ParseAdvertise
	advLogPin     = "pin"     // pin configured, claimed address doesn't match
	advLogDup     = "dup"     // address already claimed by another slave
	advLogInfo    = "info"    // accepted, but differs from observed source (NAT or proxy)
	advLogOK      = "ok"      // accepted, matches observed source (or unparseable)
)

// resolveAdvertise validates a claimed address against the live pin list and
// the address ownership map; call with r.mu held. Every rejection returns the
// zero Addr — no health entry, never eviction from the registry. Each log
// line fires once per (kind, claimed value) change rather than per call; the
// three actionable outcomes are Warn, the observed-source mismatch is Debug
// (see its comment).
func (r *Registry) resolveAdvertise(info *SlaveInfo, advertise, remoteAddr string) netip.Addr {
	name := info.Name
	if advertise == "" {
		info.AdvertiseLogState = advLogNone
		return netip.Addr{}
	}
	addr, err := ParseAdvertise(advertise)
	if err != nil {
		if key := advLogInvalid + ":" + advertise; info.AdvertiseLogState != key {
			r.log.Warn("slave advertise address rejected", "slave", name, "err", err)
			info.AdvertiseLogState = key
		}
		return netip.Addr{}
	}
	pins := r.currentPins()
	_, isPinned := pins[name]
	if pin, pinned := pins[name]; pinned && pin != addr {
		if key := advLogPin + ":" + advertise; info.AdvertiseLogState != key {
			r.log.Warn("slave advertise address does not match its pin, excluded from health mesh",
				"slave", name, "claimed", addr, "pinned", pin)
			info.AdvertiseLogState = key
		}
		return netip.Addr{}
	}
	// An owner the live pins now exclude no longer holds this address, and
	// byAddr only ever updates when that owner heartbeats — so a reload that
	// swaps two slaves' pins locked the new rightful owner out until the old
	// one was swept, up to a day later. Clear that owner's Advertise with the
	// map entry: byAddr[X] == name and slaves[name].Advertise == X are one
	// fact, and releasing half of it leaves the stale half to be believed.
	if owner, taken := r.byAddr[addr]; taken && owner != name {
		_, ownerPinned := pins[owner]
		// Two ways an owner loses its claim. Its own pin may now exclude the
		// address — the swap case. Or this slave may be pinned to it while the
		// owner is not: a pin is the operator naming the rightful holder, and
		// it is the documented remedy for a squatter, so it must beat an
		// unpinned claim. Checking only the first arm left an unpinned
		// squatter holding an address a newly added pin assigned elsewhere,
		// permanently — it keeps heartbeating, so Sweep never frees it, and
		// the SIGHUP the operator was told to apply did nothing.
		// isPinned implies pins[name] == addr: the mismatch was refused above.
		if !pinAdmitsIn(pins, owner, addr) || (isPinned && !ownerPinned) {
			delete(r.byAddr, addr)
			if prev := r.slaves[owner]; prev != nil && prev.Advertise == addr {
				prev.Advertise = netip.Addr{}
			}
		}
	}
	if owner, taken := r.byAddr[addr]; taken && owner != name {
		if key := advLogDup + ":" + advertise; info.AdvertiseLogState != key {
			r.log.Warn("slave advertise address already claimed, excluded from health mesh",
				"slave", name, "addr", addr, "owner", owner,
				"hint", "containers on a bridge network all report the same internal address; set cluster.advertise to the host's reachable IP")
			info.AdvertiseLogState = key
		}
		return netip.Addr{}
	}
	// Legitimate under NAT but the signature of a container reporting its
	// internal address; Debug because behind a reverse proxy every slave
	// trips it — remoteAddr is always the immediate peer, since XFF parsing
	// without a trusted-proxy allowlist would let a client spoof its source.
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		if observed, perr := netip.ParseAddr(host); perr == nil && observed.Unmap() != addr {
			if key := advLogInfo + ":" + advertise; info.AdvertiseLogState != key {
				r.log.Debug("slave advertise address differs from its observed source address",
					"slave", name, "advertise", addr, "observed", observed.Unmap(),
					"hint", "expected under NAT; unexpected otherwise — check cluster.advertise")
				info.AdvertiseLogState = key
			}
			return addr
		}
	}
	info.AdvertiseLogState = advLogOK + ":" + advertise
	return addr
}

// pinAdmitsIn reports whether name may still hold addr under the given pins.
// An unpinned slave holds whatever it advertised. Takes the map rather than
// reading it, so one Touch resolves the live pins once and every arm decides
// from the same snapshot.
func pinAdmitsIn(pins map[string]netip.Addr, name string, addr netip.Addr) bool {
	pin, pinned := pins[name]
	return !pinned || pin == addr
}

// Peers returns the slaves eligible for health probing, sorted by name. The
// sort is load-bearing: the scheduler fingerprint is derived from this list,
// and map iteration order would otherwise force a rebuild on every signal.
func (r *Registry) Peers() []slavehealth.Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]slavehealth.Peer, 0, len(r.slaves))
	pins := r.currentPins()
	// config.ParsedSlaveAddrs refuses duplicate values, so this inversion is
	// injective for any config that loaded. Built over sorted names anyway:
	// a stale pinsFn or a future caller that skips validation would otherwise
	// make peer selection depend on map iteration order, which is the one
	// thing this function's own doc comment says the sort exists to prevent.
	// Skipped entirely when unpinned, which is the default and the common
	// case: Peers runs on every unauthenticated /targets and /overview poll.
	var pinnedTo map[netip.Addr]string
	if len(pins) > 0 {
		pinnedTo = make(map[netip.Addr]string, len(pins))
		for _, name := range slices.Sorted(maps.Keys(pins)) {
			if _, taken := pinnedTo[pins[name]]; !taken {
				pinnedTo[pins[name]] = name
			}
		}
	}
	for _, info := range r.slaves {
		if !info.Advertise.IsValid() {
			continue
		}
		// Both halves of the pin rule, re-checked here and not only at Touch
		// time: a pin added by SIGHUP must take effect on the next scheduler
		// signal, not once some slave happens to heartbeat again. Name-to-
		// address alone was not enough — an unpinned squatter holding an
		// address a new pin assigns to someone else passed, and the Touch-side
		// steal only fires when the rightful owner heartbeats, which is
		// exactly what it cannot do when it is the node that is down.
		if pin, pinned := pins[info.Name]; pinned && pin != info.Advertise {
			continue
		}
		if owner, assigned := pinnedTo[info.Advertise]; assigned && owner != info.Name {
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
	var removed []string
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
		removed = append(removed, name)
	}
	if len(r.slaves) < maxRegisteredSlaves {
		r.fullWarned = false
	}
	onChange, onRemove := r.onChange, r.onRemove
	r.mu.Unlock()

	if onRemove != nil {
		for _, name := range removed {
			onRemove(name)
		}
	}
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
